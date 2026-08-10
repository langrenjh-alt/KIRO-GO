package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEarlyStreamPreludeEnabled(t *testing.T) {
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "")
	if !earlyStreamPreludeEnabled() {
		t.Fatal("empty KIRO_EARLY_STREAM_PRELUDE did not keep the default enabled")
	}
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "false")
	if earlyStreamPreludeEnabled() {
		t.Fatal("false KIRO_EARLY_STREAM_PRELUDE did not disable the feature")
	}
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "true")
	if !earlyStreamPreludeEnabled() {
		t.Fatal("true KIRO_EARLY_STREAM_PRELUDE did not enable the feature")
	}
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "invalid")
	if earlyStreamPreludeEnabled() {
		t.Fatal("invalid KIRO_EARLY_STREAM_PRELUDE unexpectedly enabled the feature")
	}
}

func TestCallKiroAPIReportsStreamReadyBeforeFirstEvent(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	headerFlushed := make(chan struct{})
	releaseBody := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBody) }) }
	defer release()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(headerFlushed)
		<-releaseBody
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content":      "ready",
			"inputTokens":  4,
			"outputTokens": 1,
		}))
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	ready := make(chan struct{})
	firstEvent := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- CallKiroAPI(
			&config.Account{
				ID:          "stream-ready",
				AccessToken: "token",
				ProfileArn:  "arn:aws:codewhisperer:profile/stream-ready",
			},
			emptyVisibleOutputPayload(),
			&KiroStreamCallback{
				OnStreamReady: func() { close(ready) },
				OnFirstEvent:  func() { close(firstEvent) },
			},
		)
	}()

	select {
	case <-headerFlushed:
	case <-time.After(time.Second):
		t.Fatal("Kiro response headers were not flushed")
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("OnStreamReady did not fire after the Kiro 2xx response")
	}
	select {
	case <-firstEvent:
		t.Fatal("OnFirstEvent fired before the upstream body was released")
	default:
	}

	release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("CallKiroAPI: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CallKiroAPI did not finish after the first event")
	}
}

func TestCallKiroAPIDoesNotReportStreamReadyForHTTPError(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	ready := false
	err := CallKiroAPI(
		&config.Account{
			ID:          "stream-not-ready",
			AccessToken: "token",
			ProfileArn:  "arn:aws:codewhisperer:profile/stream-not-ready",
		},
		emptyVisibleOutputPayload(),
		&KiroStreamCallback{OnStreamReady: func() { ready = true }},
	)
	if err == nil || !isRateLimitError(err) {
		t.Fatalf("CallKiroAPI error=%v, want HTTP 429", err)
	}
	if ready {
		t.Fatal("OnStreamReady fired for an upstream HTTP 429")
	}
}

func TestEarlyStreamPreludeFlushesBeforeUpstreamOutput(t *testing.T) {
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "true")
	for _, protocol := range []string{"claude", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			ready := make(chan struct{}, 1)
			releaseUpstream := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseUpstream) }) }
			defer release()

			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				if callback.OnStreamReady == nil {
					return errors.New("early stream prelude callback is missing")
				}
				callback.OnStreamReady()
				ready <- struct{}{}
				<-releaseUpstream
				callback.OnText("generated after prelude", false)
				callback.OnComplete(8, 3)
				return nil
			})

			downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payload := bindKiroPayloadContext(emptyVisibleOutputPayload(), r.Context())
				invokeEarlyPreludeProtocol(protocol, h, w, payload)
			}))
			defer downstream.Close()

			type responseResult struct {
				response *http.Response
				err      error
			}
			responseCh := make(chan responseResult, 1)
			go func() {
				resp, err := http.Get(downstream.URL)
				responseCh <- responseResult{response: resp, err: err}
			}()

			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("upstream attempt did not reach stream-ready")
			}

			var result responseResult
			select {
			case result = <-responseCh:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("client received no response while upstream output was blocked")
			}
			if result.err != nil {
				t.Fatalf("downstream request: %v", result.err)
			}
			defer result.response.Body.Close()
			if result.response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", result.response.StatusCode)
			}

			release()
			bodyBytes, err := io.ReadAll(result.response.Body)
			if err != nil {
				t.Fatalf("read stream: %v", err)
			}
			assertEarlyPreludeOrder(t, protocol, string(bodyBytes), "generated after prelude")
		})
	}
}

func TestEarlyStreamPreludeSurvivesHTTP429Failover(t *testing.T) {
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "true")
	for _, protocol := range []string{"claude", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			var calls int
			h := newEmptyVisibleOutputTestHandler(t, 2, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if calls == 1 {
					return &UpstreamRateLimitError{Endpoint: "test", Body: "busy"}
				}
				callback.OnStreamReady()
				callback.OnText("recovered after 429", false)
				callback.OnComplete(8, 3)
				return nil
			})
			rateLimit := config.RateLimitConfig{Mode: config.RateLimitModeSwitchAccount, SwitchAccountRounds: 1}
			if err := config.UpdateDispatchPolicies(&rateLimit, nil, nil); err != nil {
				t.Fatalf("configure rate-limit failover: %v", err)
			}

			recorder := httptest.NewRecorder()
			invokeEarlyPreludeProtocol(protocol, h, recorder, emptyVisibleOutputPayload())
			if calls != 2 {
				t.Fatalf("upstream calls=%d, want 2", calls)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			assertSingleEarlyPrelude(t, protocol, recorder.Body.String())
			if !strings.Contains(recorder.Body.String(), "recovered after 429") {
				t.Fatalf("recovered output missing: %s", recorder.Body.String())
			}
		})
	}
}

func TestEarlyStreamPreludeIsNotRepeatedAfterPreOutputDisconnect(t *testing.T) {
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "true")
	for _, protocol := range []string{"claude", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			var calls int
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				callback.OnStreamReady()
				if calls == 1 {
					return io.ErrUnexpectedEOF
				}
				callback.OnText("recovered after disconnect", false)
				callback.OnComplete(8, 3)
				return nil
			})

			recorder := httptest.NewRecorder()
			invokeEarlyPreludeProtocol(protocol, h, recorder, emptyVisibleOutputPayload())
			if calls != 2 {
				t.Fatalf("upstream calls=%d, want 2", calls)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			assertSingleEarlyPrelude(t, protocol, recorder.Body.String())
			if strings.Contains(recorder.Body.String(), `"error"`) || !strings.Contains(recorder.Body.String(), "recovered after disconnect") {
				t.Fatalf("stream did not recover cleanly: %s", recorder.Body.String())
			}
			if protocol == "claude" {
				assertClaudeContentBlocksSerialized(t, recorder.Body.String())
				assertClaudeTextBlocksContainDelta(t, recorder.Body.String())
			}
		})
	}
}

func TestClaudeEarlyPreludeDoesNotLeakEmptyBlockAcrossSemanticRetry(t *testing.T) {
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "true")
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnStreamReady()
		if calls == 1 {
			callback.OnComplete(8, 1)
			return nil
		}
		callback.OnText("visible after empty retry", false)
		callback.OnComplete(8, 3)
		return nil
	})

	recorder := httptest.NewRecorder()
	invokeEarlyPreludeProtocol("claude", h, recorder, emptyVisibleOutputPayload())
	if calls != 2 || recorder.Code != http.StatusOK ||
		!strings.Contains(recorder.Body.String(), "visible after empty retry") {
		t.Fatalf("calls=%d status=%d body=%s", calls, recorder.Code, recorder.Body.String())
	}
	assertSingleEarlyPrelude(t, "claude", recorder.Body.String())
	assertClaudeContentBlocksSerialized(t, recorder.Body.String())
	assertClaudeTextBlocksContainDelta(t, recorder.Body.String())
}

func TestEarlyStreamPreludeContinuesAfterUpstreamOutput(t *testing.T) {
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "true")
	for _, protocol := range []string{"claude", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			var calls int
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				callback.OnStreamReady()
				if protocol == "claude" && calls == 2 {
					callback.OnText("continued suffix", false)
					callback.OnComplete(8, 3)
					return nil
				}
				callback.OnText("already delivered", false)
				return io.ErrUnexpectedEOF
			})

			recorder := httptest.NewRecorder()
			invokeEarlyPreludeProtocol(protocol, h, recorder, emptyVisibleOutputPayload())
			wantCalls := 1
			if protocol == "claude" {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("upstream calls=%d, want %d", calls, wantCalls)
			}
			assertSingleEarlyPrelude(t, protocol, recorder.Body.String())
			if strings.Count(recorder.Body.String(), "already delivered") != 1 {
				t.Fatalf("stream duplicated the visible prefix: %s", recorder.Body.String())
			}
			if protocol == "claude" {
				if !strings.Contains(recorder.Body.String(), "continued suffix") || strings.Contains(recorder.Body.String(), `"error"`) {
					t.Fatalf("Claude continuation did not complete cleanly: %s", recorder.Body.String())
				}
			} else if !strings.Contains(recorder.Body.String(), `"error"`) {
				t.Fatalf("chat stream lost its terminal error: %s", recorder.Body.String())
			}
		})
	}
}

func TestEarlyStreamPreludeDoesNotCommitWhenNoAccountIsAvailable(t *testing.T) {
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "true")
	for _, protocol := range []string{"claude", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			var calls int
			h := newEmptyVisibleOutputTestHandler(t, 0, func(_ *config.Account, _ *KiroPayload, _ *KiroStreamCallback) error {
				calls++
				return nil
			})
			recorder := httptest.NewRecorder()
			invokeEarlyPreludeProtocol(protocol, h, recorder, emptyVisibleOutputPayload())
			if calls != 0 {
				t.Fatalf("upstream calls=%d, want 0", calls)
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if hasEarlyPrelude(protocol, recorder.Body.String()) {
				t.Fatalf("prelude was sent without an account: %s", recorder.Body.String())
			}
		})
	}
}

func TestFailureAfterEarlyStreamPreludeUsesSSEError(t *testing.T) {
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "true")
	for _, protocol := range []string{"claude", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				callback.OnStreamReady()
				callback.OnText("partial output", false)
				return errors.New("fatal stream failure")
			})
			recorder := httptest.NewRecorder()
			invokeEarlyPreludeProtocol(protocol, h, recorder, emptyVisibleOutputPayload())
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			assertSingleEarlyPrelude(t, protocol, recorder.Body.String())
			if !strings.Contains(recorder.Body.String(), `"error"`) || !strings.Contains(recorder.Body.String(), "fatal stream failure") {
				t.Fatalf("committed stream is missing its SSE error: %s", recorder.Body.String())
			}
			if protocol == "chat" && !strings.HasSuffix(strings.TrimSpace(recorder.Body.String()), "data: [DONE]") {
				t.Fatalf("chat stream error is missing [DONE]: %s", recorder.Body.String())
			}
		})
	}
}

func TestDownstreamPreludeLatencyIsIndependentFromVisibleTokenLatency(t *testing.T) {
	tracker := &latencyTracker{}
	_, timing := withRequestTiming(context.Background(), tracker)
	timing.markDownstreamPreludeFlushed(time.Now())
	timing.markDownstreamPreludeFlushed(time.Now())

	snapshot := tracker.Snapshot()
	if got := snapshot["downstream_prelude_flush"].Samples; got != 1 {
		t.Fatalf("downstream prelude samples=%d, want 1", got)
	}
	if _, exists := snapshot["first_visible_token_flush"]; exists {
		t.Fatal("prelude flush was counted as a visible token flush")
	}

	timing.markVisibleCandidate()
	timing.markVisibleFlushed(time.Now())
	if got := tracker.Snapshot()["first_visible_token_flush"].Samples; got != 1 {
		t.Fatalf("visible token samples=%d, want 1", got)
	}
}

func invokeEarlyPreludeProtocol(protocol string, h *Handler, w http.ResponseWriter, payload *KiroPayload) {
	switch protocol {
	case "claude":
		h.handleClaudeStream(w, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
	case "chat":
		h.handleOpenAIStream(w, payload, "claude-opus-5", "claude-opus-5", false, false, 5, "")
	default:
		panic("unknown protocol: " + protocol)
	}
}

func assertEarlyPreludeOrder(t *testing.T, protocol, body, output string) {
	t.Helper()
	assertSingleEarlyPrelude(t, protocol, body)
	preludeAt := -1
	switch protocol {
	case "claude":
		preludeAt = strings.Index(body, "event: message_start")
		pingAt := strings.Index(body, "event: ping")
		outputAt := strings.Index(body, output)
		if preludeAt < 0 || pingAt < preludeAt || outputAt < pingAt {
			t.Fatalf("invalid Claude prelude/output order: %s", body)
		}
		if !strings.Contains(body, "event: message_stop") {
			t.Fatalf("Claude stream is missing message_stop: %s", body)
		}
		assertClaudePreludeUsageConsistent(t, body)
	case "chat":
		preludeAt = strings.Index(body, `"role":"assistant"`)
		outputAt := strings.Index(body, output)
		if preludeAt < 0 || outputAt < preludeAt {
			t.Fatalf("invalid Chat prelude/output order: %s", body)
		}
		if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
			t.Fatalf("Chat stream is missing [DONE]: %s", body)
		}
	}
}

func assertClaudePreludeUsageConsistent(t *testing.T, body string) {
	t.Helper()
	events := decodeResponseContractSSE(t, body)
	if len(events) == 0 || events[0].Name != "message_start" {
		t.Fatalf("Claude stream does not start with message_start: %s", body)
	}
	message := responseContractMap(t, events[0].Data["message"], "message_start.message")
	startUsage := responseContractMap(t, message["usage"], "message_start.usage")
	var terminalUsage map[string]interface{}
	for _, event := range events {
		if event.Name == "message_delta" {
			terminalUsage = responseContractMap(t, event.Data["usage"], "message_delta.usage")
		}
	}
	if terminalUsage == nil {
		t.Fatalf("Claude stream is missing terminal usage: %s", body)
	}
	for _, key := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		if startUsage[key] != terminalUsage[key] {
			t.Fatalf("Claude %s changed between message_start and message_delta: start=%v terminal=%v", key, startUsage[key], terminalUsage[key])
		}
	}
}

func assertSingleEarlyPrelude(t *testing.T, protocol, body string) {
	t.Helper()
	var count int
	switch protocol {
	case "claude":
		count = strings.Count(body, "event: message_start")
	case "chat":
		count = strings.Count(body, `"role":"assistant"`)
	}
	if count != 1 {
		t.Fatalf("%s prelude count=%d, want 1; body=%s", protocol, count, body)
	}
}

func assertClaudeTextBlocksContainDelta(t *testing.T, body string) {
	t.Helper()
	textBlocks := make(map[int]bool)
	for _, event := range decodeResponseContractSSE(t, body) {
		indexValue, hasIndex := event.Data["index"].(float64)
		if !hasIndex {
			continue
		}
		index := int(indexValue)
		switch event.Name {
		case "content_block_start":
			block, ok := event.Data["content_block"].(map[string]interface{})
			if ok && block["type"] == "text" {
				textBlocks[index] = false
			}
		case "content_block_delta":
			delta, ok := event.Data["delta"].(map[string]interface{})
			if ok && delta["type"] == "text_delta" && strings.TrimSpace(fmt.Sprint(delta["text"])) != "" {
				if _, tracked := textBlocks[index]; tracked {
					textBlocks[index] = true
				}
			}
		case "content_block_stop":
			if sawDelta, tracked := textBlocks[index]; tracked && !sawDelta {
				t.Fatalf("Claude emitted an empty text block at index %d: %s", index, body)
			}
		}
	}
}

func hasEarlyPrelude(protocol, body string) bool {
	switch protocol {
	case "claude":
		return strings.Contains(body, "event: message_start")
	case "chat":
		return strings.Contains(body, `"role":"assistant"`)
	default:
		return false
	}
}
