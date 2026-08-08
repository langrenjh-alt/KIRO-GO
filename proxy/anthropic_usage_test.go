package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClaudeUsagePublishesKiroContextInStandardResponse(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			h := newResponseContractHandler(t,
				awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
				awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}),
			)

			body := `{
				"model":"claude-opus-5",
				"max_tokens":32,
				"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,
				"messages":[{"role":"user","content":"hello"}]
			}`
			var clientRequest ClaudeRequest
			if err := json.Unmarshal([]byte(body), &clientRequest); err != nil {
				t.Fatalf("decode request fixture: %v", err)
			}
			wantInputTokens := estimateClaudeRequestInputTokens(&clientRequest)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			rec := httptest.NewRecorder()
			h.handleClaudeMessagesInternal(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("request-id"); !strings.HasPrefix(got, "req_") {
				t.Fatalf("request-id=%q, want Anthropic req_ prefix", got)
			}
			if got := rec.Header().Get("x-request-id"); got != "" {
				t.Fatalf("x-request-id=%q must be left to the outer gateway", got)
			}
			if got := atomic.LoadInt64(&h.totalTokens); got >= 100 {
				t.Fatalf("internal accounted tokens=%d unexpectedly include hidden Kiro context", got)
			}

			if !stream {
				var response ClaudeResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
				}
				totalInput := response.Usage.InputTokens + response.Usage.CacheCreationInputTokens + response.Usage.CacheReadInputTokens
				if totalInput != wantInputTokens {
					t.Fatalf("usage input total=%d, want client-visible estimate %d: %+v", totalInput, wantInputTokens, response.Usage)
				}
				assertKiroContextUsageValue(t, response.Usage.KiroContextUsage, 125_000, 1_000_000, 12.5, false)
				if response.Usage.CacheCreationInputTokens != 0 || response.Usage.CacheReadInputTokens != 0 {
					t.Fatalf("unexpected cache usage: %+v", response.Usage)
				}
				if response.Usage.OutputTokensDetails == nil || response.Usage.OutputTokensDetails.ThinkingTokens != 0 {
					t.Fatalf("plain response thinking usage = %#v", response.Usage.OutputTokensDetails)
				}
				return
			}

			var startUsage, terminalUsage map[string]interface{}
			for _, event := range decodeResponseContractSSE(t, rec.Body.String()) {
				if event.Name == "message_start" {
					message := responseContractMap(t, event.Data["message"], "message_start.message")
					startUsage = responseContractMap(t, message["usage"], "message_start.message.usage")
				}
				if event.Name == "message_delta" {
					terminalUsage = responseContractMap(t, event.Data["usage"], "message_delta.usage")
				}
			}
			if startUsage == nil || terminalUsage == nil {
				t.Fatalf("missing stream usage: start=%#v terminal=%#v body=%s", startUsage, terminalUsage, rec.Body.String())
			}
			if startUsage["input_tokens"] != float64(wantInputTokens) {
				t.Fatalf("message_start input_tokens=%v, want client estimate %d", startUsage["input_tokens"], wantInputTokens)
			}
			terminalTotal := int(terminalUsage["input_tokens"].(float64) + terminalUsage["cache_creation_input_tokens"].(float64) + terminalUsage["cache_read_input_tokens"].(float64))
			if terminalTotal != wantInputTokens {
				t.Fatalf("terminal input total=%d, want client-visible estimate %d: %#v", terminalTotal, wantInputTokens, terminalUsage)
			}
			for _, field := range []string{"cache_creation_input_tokens", "cache_read_input_tokens"} {
				if startUsage[field] != terminalUsage[field] {
					t.Fatalf("stream %s drifted: start=%#v terminal=%#v", field, startUsage, terminalUsage)
				}
			}
			assertKiroContextUsageJSON(t, terminalUsage["kiro_context_usage"], 125_000, 1_000_000, 12.5, false)
		})
	}
}

func TestClaudeUsageStreamsBeforeContextUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			recorder := &flushSnapshotResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
			streamedBeforeContext := false
			h := newResponseContractHandler(t)
			h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				callback.OnText("visible immediately", false)
				streamedBeforeContext = strings.Contains(recorder.Body.String(), "visible immediately")
				callback.OnContextUsage(12.5)
				callback.OnComplete(999_999, 4)
				return nil
			}

			body := `{
				"model":"claude-opus-5",
				"max_tokens":32,
				"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,
				"messages":[{"role":"user","content":"hello"}]
			}`
			var clientRequest ClaudeRequest
			if err := json.Unmarshal([]byte(body), &clientRequest); err != nil {
				t.Fatalf("decode request fixture: %v", err)
			}
			wantEstimatedInput := estimateClaudeRequestInputTokens(&clientRequest)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			h.handleClaudeMessagesInternal(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if got := atomic.LoadInt64(&h.totalTokens); got >= 100 {
				t.Fatalf("internal accounted tokens=%d unexpectedly include Kiro context occupancy", got)
			}

			if !stream {
				var response ClaudeResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode response: %v body=%s", err, recorder.Body.String())
				}
				totalInput := response.Usage.InputTokens + response.Usage.CacheCreationInputTokens + response.Usage.CacheReadInputTokens
				if totalInput != wantEstimatedInput {
					t.Fatalf("standard input usage=%d, want client-visible estimate %d: %+v", totalInput, wantEstimatedInput, response.Usage)
				}
				assertKiroContextUsageValue(t, response.Usage.KiroContextUsage, 125_000, 1_000_000, 12.5, false)
				return
			}

			if !streamedBeforeContext {
				t.Fatal("visible content was buffered until contextUsageEvent")
			}
			var startUsage, terminalUsage map[string]interface{}
			for _, event := range decodeResponseContractSSE(t, recorder.Body.String()) {
				switch event.Name {
				case "message_start":
					message := responseContractMap(t, event.Data["message"], "message_start.message")
					startUsage = responseContractMap(t, message["usage"], "message_start.message.usage")
				case "message_delta":
					terminalUsage = responseContractMap(t, event.Data["usage"], "message_delta.usage")
				}
			}
			if startUsage == nil || terminalUsage == nil {
				t.Fatalf("missing stream usage: start=%#v terminal=%#v body=%s", startUsage, terminalUsage, recorder.Body.String())
			}
			if got := int(startUsage["input_tokens"].(float64)); got != wantEstimatedInput {
				t.Fatalf("message_start input_tokens=%d, want immediate client estimate %d", got, wantEstimatedInput)
			}
			terminalTotal := int(terminalUsage["input_tokens"].(float64) + terminalUsage["cache_creation_input_tokens"].(float64) + terminalUsage["cache_read_input_tokens"].(float64))
			if terminalTotal != wantEstimatedInput {
				t.Fatalf("terminal input usage=%d, want client-visible estimate %d: %#v", terminalTotal, wantEstimatedInput, terminalUsage)
			}
			assertKiroContextUsageJSON(t, terminalUsage["kiro_context_usage"], 125_000, 1_000_000, 12.5, false)
		})
	}
}

func TestClaudeCountTokensExcludesProxyInjectedThinkingPrompt(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{}
	count := func(body string) int {
		t.Helper()
		recorder := httptest.NewRecorder()
		h.handleCountTokens(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("count_tokens status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode count_tokens response: %v", err)
		}
		return response.InputTokens
	}

	plain := `{"model":"claude-opus-5","max_tokens":2048,"messages":[{"role":"user","content":"hello"}]}`
	thinking := `{"model":"claude-opus-5","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hello"}]}`
	if plainTokens, thinkingTokens := count(plain), count(thinking); thinkingTokens != plainTokens {
		t.Fatalf("proxy thinking prompt changed public count_tokens: plain=%d thinking=%d", plainTokens, thinkingTokens)
	}

	var request ClaudeRequest
	if err := json.Unmarshal([]byte(thinking), &request); err != nil {
		t.Fatalf("decode thinking fixture: %v", err)
	}
	if public, upstream := estimateClaudeRequestInputTokens(&request), estimateClaudeRequestInputTokens(cloneClaudeRequestForThinking(&request, true)); upstream <= public {
		t.Fatalf("fixture did not inject an upstream-only thinking prompt: public=%d upstream=%d", public, upstream)
	}
}

func TestClaudeTerminalCacheUsageExcludesKiroHiddenContext(t *testing.T) {
	payload := &KiroPayload{}
	if observed, exceeded := observeKiroContextUsage(payload, "claude-opus-5", 80); observed != 800_000 || exceeded {
		t.Fatalf("Kiro context fixture=%d exceeded=%v, want 800000/false", observed, exceeded)
	}

	profile := &promptCacheProfile{TotalInputTokens: 100, Model: "claude-opus-5"}
	cacheUsage := promptCacheUsage{
		CacheCreationInputTokens:   16,
		CacheReadInputTokens:       24,
		CacheCreation5mInputTokens: 16,
	}
	// Past the blend threshold the public total carries a bounded lift so Claude
	// Code compacts before upstream fills, but it must stay proportional to the
	// client estimate rather than restating Kiro's whole-conversation occupancy.
	const clientInputTokens = 80
	publicInputTokens := claudePublicInputTokens(payload, "claude-opus-5", clientInputTokens)
	terminalCache := claudeTerminalCacheUsage(profile, payload, "claude-opus-5", clientInputTokens, cacheUsage)
	// This fixture's client estimate is a tiny fraction of the reported occupancy,
	// so the two numbers describe different conversations and must not be blended.
	if publicInputTokens != clientInputTokens {
		t.Fatalf("public input tokens=%d, want the unblended client estimate %d", publicInputTokens, clientInputTokens)
	}
	if terminalCache.CacheReadInputTokens < cacheUsage.CacheReadInputTokens ||
		terminalCache.CacheCreationInputTokens < cacheUsage.CacheCreationInputTokens {
		t.Fatalf("terminal cache=%+v, want the split scaled with the lift, not reset to a miss", terminalCache)
	}
	if terminalCache.CacheCreation5mInputTokens+terminalCache.CacheCreation1hInputTokens != terminalCache.CacheCreationInputTokens {
		t.Fatalf("terminal cache TTL buckets do not sum to the creation total: %+v", terminalCache)
	}
	if total := billedClaudeInputTokens(publicInputTokens, terminalCache) + terminalCache.CacheCreationInputTokens + terminalCache.CacheReadInputTokens; total != publicInputTokens {
		t.Fatalf("terminal public input total=%d, want %d", total, publicInputTokens)
	}
	assertKiroContextUsageValue(t, kiroContextUsage(payload, "claude-opus-5"), 800_000, 1_000_000, 80, false)
}

func TestBufferedClaudeUsageSeparatesUpstreamCountersFromKiroContext(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnContextUsage(80)
		callback.OnText("ok", false)
		callback.OnComplete(800_000, 1)
		return nil
	})

	result, err := h.callClaudeBuffered("claude-opus-5", emptyVisibleOutputPayload(), false, 37)
	if err != nil {
		t.Fatal(err)
	}
	if result.InputTokens != 37 {
		t.Fatalf("buffered public input tokens=%d, want client estimate 37", result.InputTokens)
	}
	if result.ContextInputTokens != 800_000 {
		t.Fatalf("buffered Kiro context tokens=%d, want raw occupancy 800000", result.ContextInputTokens)
	}
}

func TestClaudeRecoveryUsageKeepsClientInputAndPeakKiroOccupancy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if calls == 1 && callback.OnContextUsage != nil {
					callback.OnContextUsage(80)
				}
				if calls == 1 {
					callback.OnText("I will inspect the repository now.", false)
					callback.OnStopReason("END_TURN")
					callback.OnComplete(10, 4)
					return nil
				}
				callback.OnToolUse(KiroToolUse{
					ToolUseID: "toolu_context_recovery",
					Name:      "shell_command",
					Input:     map[string]interface{}{"command": "rg -n TODO ."},
				})
				callback.OnStopReason("TOOL_USE")
				callback.OnComplete(10, 4)
				return nil
			})
			payload := completionProtocolTestPayload()
			rec := httptest.NewRecorder()

			if stream {
				h.handleClaudeStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			} else {
				h.handleClaudeNonStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			}

			if calls != 2 || rec.Code != http.StatusOK {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
			}
			if !stream {
				var response ClaudeResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
				}
				if totalInput := response.Usage.InputTokens + response.Usage.CacheCreationInputTokens + response.Usage.CacheReadInputTokens; totalInput != 5 {
					t.Fatalf("terminal input total=%d, want client-visible estimate 5", totalInput)
				}
				assertKiroContextUsageValue(t, response.Usage.KiroContextUsage, 800_000, 1_000_000, 80, false)
				return
			}

			var terminalUsage map[string]interface{}
			for _, event := range decodeResponseContractSSE(t, rec.Body.String()) {
				if event.Name == "message_delta" {
					terminalUsage = responseContractMap(t, event.Data["usage"], "message_delta.usage")
				}
			}
			terminalTotal := int(terminalUsage["input_tokens"].(float64) + terminalUsage["cache_creation_input_tokens"].(float64) + terminalUsage["cache_read_input_tokens"].(float64))
			if terminalTotal != 5 {
				t.Fatalf("terminal input total=%d, want client-visible estimate 5; usage=%#v", terminalTotal, terminalUsage)
			}
			assertKiroContextUsageJSON(t, terminalUsage["kiro_context_usage"], 800_000, 1_000_000, 80, false)
		})
	}
}

func assertKiroContextUsageValue(t *testing.T, got *KiroContextUsage, inputTokens, contextWindow int, percentage float64, exceeded bool) {
	t.Helper()
	if got == nil || got.InputTokens != inputTokens || got.ContextWindow != contextWindow || got.Percentage != percentage || got.Exceeded != exceeded {
		t.Fatalf("kiro_context_usage=%+v, want input=%d window=%d percentage=%v exceeded=%v", got, inputTokens, contextWindow, percentage, exceeded)
	}
}

func assertKiroContextUsageJSON(t *testing.T, value interface{}, inputTokens, contextWindow int, percentage float64, exceeded bool) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode kiro_context_usage: %v", err)
	}
	var got KiroContextUsage
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("decode kiro_context_usage: %v value=%#v", err, value)
	}
	assertKiroContextUsageValue(t, &got, inputTokens, contextWindow, percentage, exceeded)
}

func TestContextUsagePeakSurvivesFullPayloadRestore(t *testing.T) {
	payload := completionProtocolTestPayload()
	if !captureAgentRecoveryOriginalPayload(payload) {
		t.Fatal("failed to snapshot recovery payload")
	}
	if got, exceeded := observeKiroContextUsage(payload, "claude-opus-5", 87.5); got != 875_000 || exceeded {
		t.Fatalf("initial occupancy tokens=%d exceeded=%v", got, exceeded)
	}
	if !restoreAgentRecoveryOriginalPayload(payload) {
		t.Fatal("failed to restore recovery payload")
	}
	if payload.maxContextUsagePercentage != 87.5 {
		t.Fatalf("restore changed request-chain peak to %v", payload.maxContextUsagePercentage)
	}
	if got, _ := observeKiroContextUsage(payload, "claude-opus-5", 1); got != 875_000 {
		t.Fatalf("post-restore occupancy tokens=%d, want preserved 87.5%% peak", got)
	}
}
