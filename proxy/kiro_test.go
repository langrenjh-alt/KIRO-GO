package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsRetryableKiroStreamError(t *testing.T) {
	transient := []error{
		context.DeadlineExceeded,
		&url.Error{Op: "read", URL: "https://q.example", Err: context.DeadlineExceeded},
		fmt.Errorf("read Kiro event stream frame: %w", io.ErrUnexpectedEOF),
		errors.New("Kiro stream exception InternalServerException: Encountered an unexpected error when processing the request, please try again."),
		errors.New("read Kiro event stream prelude: stream error: stream ID 11241; INTERNAL_ERROR; received from peer"),
		&KiroStreamProtocolError{Code: "upstream_tool_json_error", Err: errors.New("unexpected EOF")},
	}
	for _, err := range transient {
		if !isRetryableKiroStreamError(err) {
			t.Fatalf("expected transient Kiro stream error to be retryable: %v", err)
		}
	}
	if isRetryableKiroStreamError(errors.New("Kiro stream exception ValidationException: invalid tool")) {
		t.Fatal("permanent validation exception must not be classified as transient")
	}
	if isRetryableKiroStreamError(&KiroHTTPError{StatusCode: http.StatusBadRequest, Endpoint: "test", Body: "invalid request"}) {
		t.Fatal("permanent HTTP 400 must not be classified as transient")
	}
}

func TestBuildKiroTransportBoundsPreResponsePhases(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("KIRO_RESPONSE_HEADER_TIMEOUT_SECONDS", "")
	transport := buildKiroTransport("")
	if transport.DialContext == nil {
		t.Fatal("stream transport has no bounded dialer")
	}
	if transport.TLSHandshakeTimeout != kiroTLSHandshakeTimeout {
		t.Fatalf("TLS handshake timeout=%s, want %s", transport.TLSHandshakeTimeout, kiroTLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != defaultKiroResponseHeaderTimeout {
		t.Fatalf("response header timeout=%s, want %s", transport.ResponseHeaderTimeout, defaultKiroResponseHeaderTimeout)
	}
	if transport.MaxConnsPerHost != 0 || transport.MaxIdleConnsPerHost != 32 {
		t.Fatalf("direct pool settings = active:%d idle:%d", transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("direct transport should attempt HTTP/2")
	}
}

func TestBuildKiroTransportAllowsExplicitResponseHeaderTimeout(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("KIRO_RESPONSE_HEADER_TIMEOUT_SECONDS", "45")

	transport := buildKiroTransport("")
	if transport.ResponseHeaderTimeout != 45*time.Second {
		t.Fatalf("response header timeout=%s, want 45s", transport.ResponseHeaderTimeout)
	}
}

func TestKiroTimeoutPolicyDefaults(t *testing.T) {
	t.Setenv("KIRO_RESPONSE_HEADER_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_STREAM_BODY_IDLE_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_GENERATION_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_RECOVERY_CHAIN_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_AGENT_CONTINUATION_TIMEOUT_SECONDS", "")

	policy := kiroTimeoutPolicyFromEnv()
	if policy.responseHeaderTimeout != defaultKiroResponseHeaderTimeout ||
		policy.bodyIdleTimeout != defaultKiroStreamBodyIdleTimeout ||
		policy.generationTimeout != defaultKiroGenerationTimeout ||
		policy.recoveryChainTimeout != defaultKiroRecoveryChainTimeout ||
		policy.agentContinueTimeout != defaultKiroAgentContinueTimeout {
		t.Fatalf("unexpected default timeout policy: %#v", policy)
	}
	if policy.agentContinueTimeout != 0 {
		t.Fatalf("agent continuation timeout=%s, want disabled", policy.agentContinueTimeout)
	}
	if policy.bodyIdleTimeout != 0 || policy.generationTimeout != 0 {
		t.Fatalf("streaming timeouts remain enabled: body_idle=%s generation=%s",
			policy.bodyIdleTimeout, policy.generationTimeout)
	}
	if policy.recoveryChainTimeout != 0 {
		t.Fatalf("recovery chain timeout=%s, want disabled", policy.recoveryChainTimeout)
	}
}

func TestKiroRecoveryChainDeadlineIsDisabledByDefault(t *testing.T) {
	t.Setenv("KIRO_RESPONSE_HEADER_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_STREAM_BODY_IDLE_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_GENERATION_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_RECOVERY_CHAIN_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_AGENT_CONTINUATION_TIMEOUT_SECONDS", "")
	payload := &KiroPayload{requestContext: context.Background()}

	recoveryContext, policy := kiroRecoveryContext(payload, time.Now())

	if policy.recoveryChainTimeout != 0 || !payload.recoveryDeadline.IsZero() {
		t.Fatalf("recovery chain remained bounded: timeout=%s deadline=%s",
			policy.recoveryChainTimeout, payload.recoveryDeadline)
	}
	if _, bounded := recoveryContext.Deadline(); bounded {
		t.Fatal("disabled recovery chain unexpectedly received a deadline")
	}
}

func TestKiroGenerationDeadlineIsDisabledByDefault(t *testing.T) {
	t.Setenv("KIRO_RESPONSE_HEADER_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_STREAM_BODY_IDLE_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_GENERATION_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_RECOVERY_CHAIN_TIMEOUT_SECONDS", "")
	t.Setenv("KIRO_AGENT_CONTINUATION_TIMEOUT_SECONDS", "")
	payload := &KiroPayload{requestContext: context.Background()}

	generationContext, cancel, policy := kiroGenerationContext(payload, time.Now())
	defer cancel()

	if policy.bodyIdleTimeout != 0 || policy.generationTimeout != 0 ||
		policy.recoveryChainTimeout != 0 || policy.agentContinueTimeout != 0 {
		t.Fatalf("generation timeout policy was not disabled: %#v", policy)
	}
	if _, bounded := generationContext.Deadline(); bounded {
		t.Fatal("default generation unexpectedly received a deadline")
	}
}

func TestAgentContinuationUsesNormalGenerationDeadlineByDefault(t *testing.T) {
	payload := &KiroPayload{
		requestContext: context.Background(),
		timeoutPolicy: kiroTimeoutPolicy{
			responseHeaderTimeout: time.Second,
			bodyIdleTimeout:       time.Second,
			generationTimeout:     20 * time.Millisecond,
			recoveryChainTimeout:  200 * time.Millisecond,
			agentContinueTimeout:  0,
		},
		agentActionContinuationTimeout: true,
	}
	ctx, cancel, _ := kiroGenerationContext(payload, time.Now())
	defer cancel()
	<-ctx.Done()
	if code, _ := kiroStreamProtocolErrorCode(context.Cause(ctx)); code != upstreamGenerationTimeoutCode {
		t.Fatalf("agent continuation deadline cause=%v code=%q", context.Cause(ctx), code)
	}
}

func TestAgentContinuationAllowsExplicitShortDeadline(t *testing.T) {
	payload := &KiroPayload{
		requestContext: context.Background(),
		timeoutPolicy: kiroTimeoutPolicy{
			responseHeaderTimeout: time.Second,
			bodyIdleTimeout:       time.Second,
			generationTimeout:     time.Second,
			recoveryChainTimeout:  2 * time.Second,
			agentContinueTimeout:  20 * time.Millisecond,
		},
		agentActionContinuationTimeout: true,
	}
	ctx, cancel, _ := kiroGenerationContext(payload, time.Now())
	defer cancel()
	<-ctx.Done()
	if code, _ := kiroStreamProtocolErrorCode(context.Cause(ctx)); code != upstreamAgentContinuationTimeoutCode {
		t.Fatalf("agent continuation deadline cause=%v code=%q", context.Cause(ctx), code)
	}
}

func TestGetClientForProxyReusesOneClientDuringFirstBurst(t *testing.T) {
	proxyURL := "http://127.0.0.1:47891"
	proxyClientCache.Delete(proxyURL)
	proxyClientCache.Delete("rest:" + proxyURL)
	t.Cleanup(func() {
		if client, ok := proxyClientCache.LoadAndDelete(proxyURL); ok {
			client.(*http.Client).CloseIdleConnections()
		}
		if client, ok := proxyClientCache.LoadAndDelete("rest:" + proxyURL); ok {
			client.(*http.Client).CloseIdleConnections()
		}
	})

	const workers = 64
	assertOneClient := func(name string, get func() *http.Client) {
		t.Helper()
		start := make(chan struct{})
		clients := make(chan *http.Client, workers)
		var wg sync.WaitGroup
		wg.Add(workers)
		for i := 0; i < workers; i++ {
			go func() {
				defer wg.Done()
				<-start
				clients <- get()
			}()
		}
		close(start)
		wg.Wait()
		close(clients)
		var first *http.Client
		for client := range clients {
			if first == nil {
				first = client
				continue
			}
			if client != first {
				t.Fatalf("%s first burst returned multiple clients", name)
			}
		}
	}

	assertOneClient("stream", func() *http.Client { return GetClientForProxy(proxyURL) })
	assertOneClient("rest", func() *http.Client { return GetRestClientForProxy(proxyURL) })
}

func TestCallKiroAPITrimsHistoryBeforeReturningInputTooLongWithoutRetry(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	t.Setenv("KIRO_INPUT_TOKEN_RESERVE", "8192")
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if len(body) > defaultKiroMaxRawBodyBytes {
			t.Fatalf("preflight body bytes=%d, limit=%d", len(body), defaultKiroMaxRawBodyBytes)
		}
		if tokens := max(estimateApproxTokens(string(body)), (len(body)+agentRecoveryBytesPerToken-1)/agentRecoveryBytesPerToken); tokens > 200_000-defaultKiroInputTokenReserve {
			t.Fatalf("preflight estimated tokens=%d, budget=%d", tokens, 200_000-defaultKiroInputTokenReserve)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Input is too long.","reason":"REQUEST_BODY_INVALID"}`))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	defer func() { kiroEndpoints = oldEndpoints }()
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "final question",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}
	for i := 0; i < 100; i++ {
		payload.ConversationState.History = append(payload.ConversationState.History, KiroHistoryMessage{
			UserInputMessage: &KiroUserInputMessage{
				Content: strings.Repeat("history ", 1100),
				ModelID: "claude-sonnet-4.5",
				Origin:  "AI_EDITOR",
			},
		})
	}
	err := CallKiroAPI(&config.Account{ID: "input-too-long-test", AccessToken: "token", ProfileArn: "arn:aws:codewhisperer:profile/input-too-long-test"}, payload, nil)
	if err == nil || !isKiroInputTooLongError(err) {
		t.Fatalf("CallKiroAPI error=%v, want input-too-long error", err)
	}
	if requests != 1 {
		t.Fatalf("requests=%d, want exactly one upstream attempt", requests)
	}
	if len(payload.ConversationState.History) >= 100 {
		t.Fatalf("history entries=%d, want old history trimmed", len(payload.ConversationState.History))
	}
	if payload.ConversationState.CurrentMessage.UserInputMessage.Content != "final question" {
		t.Fatalf("current request changed: %q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
	if !kiroHistoryContainsText(payload.ConversationState.History, kiroHistoryTruncationNote) {
		t.Fatal("trimmed payload did not carry the history truncation note")
	}
}

func TestCallKiroAPIPreOutputTimeoutStaysTransportError(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-releaseServer
	}))
	defer func() {
		close(releaseServer)
		server.Close()
	}()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	defer func() { kiroEndpoints = oldEndpoints }()
	oldClient := kiroHttpStore.Load()
	transport := &http.Transport{ResponseHeaderTimeout: 20 * time.Millisecond}
	kiroHttpStore.Store(&http.Client{Transport: transport})
	defer func() {
		transport.CloseIdleConnections()
		kiroHttpStore.Store(oldClient)
	}()

	payload := completionProtocolTestPayload()
	originalPrompt := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	account := &config.Account{
		ID:          "pre-output-timeout",
		AccessToken: "token",
		ProfileArn:  "arn:aws:codewhisperer:profile/pre-output-timeout",
	}

	err := CallKiroAPI(account, payload, &KiroStreamCallback{})
	if err == nil || isUpstreamIncompleteResponseError(err) {
		t.Fatalf("pre-output timeout became a semantic completion error: %v", err)
	}
	if payload.ConversationState.CurrentMessage.UserInputMessage.Content != originalPrompt {
		t.Fatal("pre-output timeout mutated the model prompt")
	}
	if payload.AgentCompletionRetries != 0 || agentCompletionRetryAllowed(payload, err) {
		t.Fatalf("transport timeout consumed semantic retry budget: retries=%d err=%v", payload.AgentCompletionRetries, err)
	}
}

func TestParseEventStreamCompletesValidPendingToolUseOnEOF(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "mcpIdaProMcpStatus",
		"input":     `{"server":"ida-pro-mcp"}`,
	}))

	var toolUses []KiroToolUse
	var completed bool
	var callbackErr error
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
		OnComplete: func(_, _ int) {
			completed = true
		},
		OnError: func(err error) {
			callbackErr = err
		},
	})
	if err != nil {
		t.Fatalf("expected valid pending tool JSON to complete at EOF, got %v", err)
	}
	if callbackErr != nil {
		t.Fatalf("unexpected callback error: %v", callbackErr)
	}
	if !completed {
		t.Fatal("completion callback did not run")
	}
	if len(toolUses) != 1 || toolUses[0].ToolUseID != "toolu_1" || toolUses[0].Name != "mcpIdaProMcpStatus" || toolUses[0].Input["server"] != "ida-pro-mcp" {
		t.Fatalf("completed tool use = %#v", toolUses)
	}
}

func TestParseEventStreamRejectsTruncatedPendingToolUseOnEOF(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_truncated",
		"name":      "write",
		"input":     `{"path":"file.txt","content":`,
	}))
	var callbackErr error
	var completed bool
	var toolUses []KiroToolUse
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse:  func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
		OnComplete: func(_, _ int) { completed = true },
		OnError:    func(err error) { callbackErr = err },
	})
	if err == nil || !strings.Contains(err.Error(), "stream ended before complete JSON") {
		t.Fatalf("expected truncated tool JSON error, got %v", err)
	}
	if callbackErr == nil || callbackErr.Error() != err.Error() || completed || len(toolUses) != 0 {
		t.Fatalf("callbackErr=%v completed=%v toolUses=%#v", callbackErr, completed, toolUses)
	}
}

func TestParseEventStreamRejectsInvalidCRC(t *testing.T) {
	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"})
	frame[len(frame)-5] ^= 0xff

	var callbackErr error
	err := parseEventStream(bytes.NewReader(frame), &KiroStreamCallback{
		OnError: func(err error) { callbackErr = err },
	})
	if err == nil || !strings.Contains(err.Error(), "message CRC") {
		t.Fatalf("expected message CRC error, got %v", err)
	}
	if callbackErr == nil || callbackErr.Error() != err.Error() {
		t.Fatalf("expected OnError to receive CRC error, got %v", callbackErr)
	}
}

func TestParseEventStreamRejectsOversizedFrameBeforeAllocation(t *testing.T) {
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[:4], maxKiroEventStreamFrameBytes+1)
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[:8]))

	err := parseEventStream(bytes.NewReader(prelude), nil)
	if err == nil || !strings.Contains(err.Error(), "frame length") {
		t.Fatalf("expected oversized frame error, got %v", err)
	}
}

func TestParseEventStreamPassesRepeatedDeltaTextThrough(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "test"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "test"}),
	}, nil))
	var content strings.Builder
	if err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, _ bool) { content.WriteString(text) },
	}); err != nil {
		t.Fatal(err)
	}
	if got := content.String(); got != "testtest" {
		t.Fatalf("delta content = %q, want repeated text intact", got)
	}
}

func TestParseEventStreamKeepsAliveDuringUpstreamSilenceThenContinues(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "continued"})
	go func() {
		time.Sleep(35 * time.Millisecond)
		_, _ = writer.Write(frame)
		_ = writer.Close()
	}()

	var events []string
	err := parseEventStreamWithKeepAlive(reader, &KiroStreamCallback{
		OnKeepAlive: func() { events = append(events, "ping") },
		OnText:      func(text string, _ bool) { events = append(events, text) },
	}, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0] != "ping" || events[len(events)-1] != "continued" {
		t.Fatalf("events=%#v, want keepalive(s) followed by upstream content", events)
	}
}

func TestParseEventStreamBodyIdleTimeoutIgnoresKeepAlive(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	keepAlives := 0
	started := time.Now()
	err := parseEventStreamWithKeepAlive(reader, &KiroStreamCallback{
		BodyIdleTimeout: 20 * time.Millisecond,
		OnKeepAlive:     func() { keepAlives++ },
	}, 5*time.Millisecond)
	elapsed := time.Since(started)

	code, ok := kiroStreamProtocolErrorCode(err)
	if !ok || code != upstreamBodyIdleTimeoutCode {
		t.Fatalf("body idle error=%v code=%q", err, code)
	}
	if keepAlives == 0 {
		t.Fatal("expected downstream keepalives before the body idle timeout")
	}
	if elapsed < 15*time.Millisecond || elapsed >= 100*time.Millisecond {
		t.Fatalf("body idle elapsed=%s, want an independent 20ms budget", elapsed)
	}
}

func TestKiroGenerationAndRecoveryDeadlinePriority(t *testing.T) {
	basePolicy := kiroTimeoutPolicy{
		responseHeaderTimeout: time.Second,
		bodyIdleTimeout:       time.Second,
		generationTimeout:     20 * time.Millisecond,
		recoveryChainTimeout:  200 * time.Millisecond,
		agentContinueTimeout:  30 * time.Millisecond,
	}

	generationPayload := &KiroPayload{
		requestContext: context.Background(),
		timeoutPolicy:  basePolicy,
	}
	generationContext, cancelGeneration, _ := kiroGenerationContext(generationPayload, time.Now())
	defer cancelGeneration()
	<-generationContext.Done()
	if code, _ := kiroStreamProtocolErrorCode(context.Cause(generationContext)); code != upstreamGenerationTimeoutCode {
		t.Fatalf("generation deadline cause=%v code=%q", context.Cause(generationContext), code)
	}

	recoveryPolicy := basePolicy
	recoveryPolicy.generationTimeout = 200 * time.Millisecond
	recoveryPolicy.recoveryChainTimeout = 50 * time.Millisecond
	recoveryPayload := &KiroPayload{
		requestContext:    context.Background(),
		timeoutPolicy:     recoveryPolicy,
		recoveryStartedAt: time.Now().Add(-40 * time.Millisecond),
	}
	recoveryContext, cancelRecoveryAttempt, _ := kiroGenerationContext(recoveryPayload, time.Now())
	defer cancelRecoveryAttempt()
	<-recoveryContext.Done()
	if code, _ := kiroStreamProtocolErrorCode(context.Cause(recoveryContext)); code != upstreamRecoveryTimeoutCode {
		t.Fatalf("recovery deadline cause=%v code=%q", context.Cause(recoveryContext), code)
	}
}

func TestNormalizeKiroCallErrorClassifiesResponseHeaderTimeout(t *testing.T) {
	policy := kiroTimeoutPolicy{responseHeaderTimeout: 45 * time.Second}
	err := normalizeKiroCallError(context.Background(), errors.New("net/http: timeout awaiting response headers"), policy)
	if code, _ := kiroStreamProtocolErrorCode(err); code != upstreamResponseHeaderTimeoutCode {
		t.Fatalf("response header error=%v code=%q", err, code)
	}
}

func TestParseEventStreamKeepsIndependentHeartbeatWhileFramesAreActive(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	go func() {
		for _, frame := range [][]byte{
			awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 1}),
			awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 2}),
			awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 3}),
			awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 4}),
			awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 5}),
			awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 6}),
		} {
			time.Sleep(5 * time.Millisecond)
			_, _ = writer.Write(frame)
		}
		time.Sleep(10 * time.Millisecond)
		_, _ = writer.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "abc"}))
		_ = writer.Close()
	}()

	var content strings.Builder
	keepAlives := 0
	err := parseEventStreamWithKeepAlive(reader, &KiroStreamCallback{
		OnKeepAlive: func() { keepAlives++ },
		OnText:      func(text string, _ bool) { content.WriteString(text) },
	}, 15*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if keepAlives < 2 || content.String() != "abc" {
		t.Fatalf("keepalives=%d content=%q, want independent heartbeats and intact active stream", keepAlives, content.String())
	}
}

func TestParseEventStreamIgnoresUnknownNonJSONEvent(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrameWithRawPayload(t, map[string]string{
			":message-type": "event",
			":event-type":   "futureKiroEvent",
		}, []byte("not-json")),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "continued"}),
	}, nil))
	var content strings.Builder
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, _ bool) { content.WriteString(text) },
	})
	if err != nil {
		t.Fatalf("unknown event terminated stream: %v", err)
	}
	if content.String() != "continued" {
		t.Fatalf("content=%q, want continued output after unknown event", content.String())
	}
}

func TestParseEventStreamStillRejectsMalformedKnownEvent(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrameWithRawPayload(t, map[string]string{
		":message-type": "event",
		":event-type":   "assistantResponseEvent",
	}, []byte("not-json")))
	err := parseEventStream(stream, &KiroStreamCallback{})
	if err == nil || !strings.Contains(err.Error(), "invalid assistantResponseEvent payload") {
		t.Fatalf("malformed known event was accepted: %v", err)
	}
}

func TestCompletionProtocolWaitsThroughLongReasoningUntilToolCall(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	go func() {
		_, _ = writer.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
			"text": strings.Repeat("still thinking; ", 2000),
		}))
		time.Sleep(80 * time.Millisecond)
		_, _ = writer.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "I will inspect the file.",
		}))
		_, _ = writer.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_action", "name": "Read", "input": `{"path":"test.txt"}`, "stop": true,
		}))
		_ = writer.Close()
	}()

	var events []string
	keepAlives := 0
	err := parseEventStreamWithKeepAlive(reader, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		OnKeepAlive:           func() { keepAlives++ },
		OnReasoning:           func(KiroReasoningContentEvent) { events = append(events, "reasoning") },
		OnText:                func(string, bool) { events = append(events, "text") },
		OnToolUse:             func(KiroToolUse) { events = append(events, "tool") },
	}, 10*time.Millisecond)
	if err != nil || keepAlives == 0 {
		t.Fatalf("long active generation was interrupted: err=%v keepalives=%d", err, keepAlives)
	}
	if strings.Join(events, ",") != "reasoning,text,tool" {
		t.Fatalf("upstream event order=%v, want reasoning,text,tool", events)
	}
}

func TestInitialAgentThinkingTimeoutTriggersSemanticRecovery(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	go func() {
		_, _ = writer.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
			"text": "I will inspect the repository after more planning.",
		}))
		time.Sleep(100 * time.Millisecond)
		_ = writer.Close()
	}()

	var callbackErr error
	reasoningObserved := false
	started := time.Now()
	err := parseEventStreamWithKeepAlive(reader, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		AgentThinkingTimeout:  20 * time.Millisecond,
		OnReasoning: func(KiroReasoningContentEvent) {
			reasoningObserved = true
		},
		OnError: func(err error) { callbackErr = err },
	}, 0)
	elapsed := time.Since(started)

	if !reasoningObserved || !isUpstreamAgentMarkerMismatchError(err) || !isUpstreamIncompleteResponseError(err) {
		t.Fatalf("reasoning=%v err=%v", reasoningObserved, err)
	}
	if callbackErr == nil || callbackErr.Error() != err.Error() || !strings.Contains(err.Error(), "initial reasoning exceeded") {
		t.Fatalf("callbackErr=%v err=%v", callbackErr, err)
	}
	if elapsed < 15*time.Millisecond || elapsed >= 100*time.Millisecond {
		t.Fatalf("thinking timeout elapsed=%s, want prompt semantic recovery", elapsed)
	}
}

func TestInitialAgentThinkingTimeoutStopsAfterToolUse(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	go func() {
		_, _ = writer.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
			"text": "The next action is a repository search.",
		}))
		_, _ = writer.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_search", "name": "Grep", "input": `{"pattern":"Finder"}`, "stop": true,
		}))
		time.Sleep(60 * time.Millisecond)
		_ = writer.Close()
	}()

	var toolUses []KiroToolUse
	err := parseEventStreamWithKeepAlive(reader, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		AgentThinkingTimeout:  20 * time.Millisecond,
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	}, 0)
	if err != nil || len(toolUses) != 1 || toolUses[0].Name != "Grep" {
		t.Fatalf("tool call did not cancel thinking timeout: err=%v tools=%#v", err, toolUses)
	}
}

func TestAgentThinkingTimeoutIsDisabledByDefault(t *testing.T) {
	payload := completionProtocolTestPayload()
	if got := initialAgentThinkingTimeout(payload); got != 0 {
		t.Fatalf("initial thinking timeout=%s, want disabled", got)
	}
	payload.AgentCompletionRetries = 1
	if got := initialAgentThinkingTimeout(payload); got != 0 {
		t.Fatalf("recovery thinking timeout=%s, want disabled", got)
	}
	payload.AgentCompletionRetries = 0
	payload.AgentCompletionMarker = ""
	if got := initialAgentThinkingTimeout(payload); got != 0 {
		t.Fatalf("ordinary request thinking timeout=%s, want disabled", got)
	}
}

func TestAgentMarkerPrefixCanSpanFrames(t *testing.T) {
	const marker = "<agent_final_0123456789abcdef>"
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": " \n" + marker[:13]}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": marker[13:] + "done"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"}),
	}, nil))

	err := parseEventStreamWithKeepAlive(stream, &KiroStreamCallback{
		AgentCompletionMarker: marker,
	}, 0)
	if err != nil {
		t.Fatalf("split completion marker was rejected: %v", err)
	}
}

func TestAgentMarkerDoesNotCompleteWithoutNativeTerminal(t *testing.T) {
	const marker = "<agent_final_0123456789abcdef>"
	stream := bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": marker + "done",
	}))

	err := parseEventStreamWithKeepAlive(stream, &KiroStreamCallback{
		AgentCompletionMarker: marker,
	}, 0)
	if !isUpstreamIncompleteResponseError(err) || !isUpstreamAgentMarkerMismatchError(err) {
		t.Fatalf("marker without native terminal was accepted: %v", err)
	}
}

func TestAgentMarkerMismatchWaitsForFollowingToolCall(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	go func() {
		_, _ = writer.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "Let me inspect it.",
		}))
		time.Sleep(80 * time.Millisecond)
		_, _ = writer.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_read", "name": "Read", "input": `{"file_path":"test.cs"}`, "stop": true,
		}))
		_ = writer.Close()
	}()

	var toolUses []KiroToolUse
	err := parseEventStreamWithKeepAlive(reader, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		OnKeepAlive:           func() {},
		OnToolUse:             func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	}, 0)
	if err != nil || len(toolUses) != 1 || toolUses[0].Name != "Read" {
		t.Fatalf("delayed tool call after unmarked text was rejected: err=%v tools=%#v", err, toolUses)
	}
}

func TestAgentMarkerMismatchIsCheckedOnlyAtEOF(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	go func() {
		_, _ = writer.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "Let me inspect it.",
		}))
		time.Sleep(80 * time.Millisecond)
		_ = writer.Close()
	}()

	started := time.Now()
	err := parseEventStreamWithKeepAlive(reader, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		OnKeepAlive:           func() {},
	}, 0)
	elapsed := time.Since(started)
	if !isUpstreamAgentMarkerMismatchError(err) || !isUpstreamIncompleteResponseError(err) {
		t.Fatalf("marker mismatch was not classified: %v", err)
	}
	if elapsed < 70*time.Millisecond {
		t.Fatalf("marker mismatch was reported before EOF: elapsed=%s", elapsed)
	}
}

func TestParseEventStreamTreatsContentLengthExceededAsMaxTokens(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial answer"}),
		awsEventStreamFrameWithHeaders(t, map[string]string{
			":message-type":   "exception",
			":exception-type": "ContentLengthExceededException",
		}, map[string]interface{}{"message": "maximum output reached"}),
	}, nil))
	var content, stopReason string
	var completed bool
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText:       func(text string, _ bool) { content += text },
		OnStopReason: func(reason string) { stopReason = reason },
		OnComplete:   func(_, _ int) { completed = true },
	})
	if !isUpstreamOutputTruncatedError(err) {
		t.Fatalf("content length termination was not classified for continuation: %v", err)
	}
	if content != "partial answer" || stopReason != "max_tokens" || !completed {
		t.Fatalf("content=%q stop=%q completed=%v", content, stopReason, completed)
	}
}

func TestParseEventStreamReadsNativeEndTurn(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "complete native answer"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "END_TURN",
			"tokenUsage": map[string]interface{}{"inputTokens": 10, "outputTokens": 3},
		}),
	}, nil))

	var stopReason string
	err := parseEventStream(stream, &KiroStreamCallback{
		OnStopReason: func(reason string) { stopReason = reason },
	})
	if err != nil || stopReason != "end_turn" {
		t.Fatalf("native END_TURN was not parsed: stop=%q err=%v", stopReason, err)
	}
}

func TestParseEventStreamUsesMeteringAsEnterpriseTerminalFallback(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "enterprise answer"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 0.25}),
	}, nil))

	var stopReason string
	err := parseEventStream(stream, &KiroStreamCallback{
		OnStopReason: func(reason string) { stopReason = reason },
	})
	if err != nil || stopReason != "end_turn" {
		t.Fatalf("metering fallback was not accepted: stop=%q err=%v", stopReason, err)
	}
}

func TestParseEventStreamPublishesMeteringBeforeAgentMarkerFailure(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "END_TURN",
			"tokenUsage": map[string]interface{}{"inputTokens": 37, "outputTokens": 11},
		}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.25}),
	}, nil))

	var credits float64
	var inputTokens, outputTokens int
	err := parseEventStream(stream, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		OnCredits:             func(value float64) { credits = value },
		OnComplete: func(input, output int) {
			inputTokens = input
			outputTokens = output
		},
	})
	if !isUpstreamIncompleteResponseError(err) || !isUpstreamAgentMarkerMismatchError(err) {
		t.Fatalf("expected marker mismatch, got %v", err)
	}
	if credits != 1.25 || inputTokens != 37 || outputTokens != 11 {
		t.Fatalf("terminal metering was lost: credits=%v input=%d output=%d", credits, inputTokens, outputTokens)
	}
}

func TestParseEventStreamNativeEndTurnAcceptsVisibleAssistantText(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "planning only"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"}),
	}, nil))

	var content, stopReason string
	err := parseEventStream(stream, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		OnText:                func(text string, _ bool) { content += text },
		OnStopReason:          func(reason string) { stopReason = reason },
	})
	if err != nil || stopReason != "end_turn" || content != "planning only" {
		t.Fatalf("visible END_TURN was not accepted: content=%q stop=%q err=%v", content, stopReason, err)
	}
}

func TestParseEventStreamRejectsEmptyAgentEndTurn(t *testing.T) {
	const marker = "<agent_final_0123456789abcdef>"
	tests := []struct {
		name   string
		frames [][]byte
	}{
		{
			name: "empty",
		},
		{
			name: "reasoning only",
			frames: [][]byte{
				awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking"}),
			},
		},
		{
			name: "marker only",
			frames: [][]byte{
				awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": marker}),
			},
		},
		{
			name: "whitespace only",
			frames: [][]byte{
				awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": " \r\n\t"}),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frames := append([][]byte(nil), test.frames...)
			frames = append(frames, awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"}))
			err := parseEventStream(bytes.NewReader(bytes.Join(frames, nil)), &KiroStreamCallback{
				AgentCompletionMarker: marker,
			})
			if !isUpstreamIncompleteResponseError(err) || !isUpstreamAgentMarkerMismatchError(err) {
				t.Fatalf("empty agent END_TURN was accepted: %v", err)
			}
		})
	}
}

func TestParseEventStreamDoesNotTreatContextUsageAsTerminal(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial answer"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
	}, nil))

	err := parseEventStream(stream, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
	})
	if !isUpstreamIncompleteResponseError(err) || !isUpstreamAgentMarkerMismatchError(err) {
		t.Fatalf("context usage incorrectly completed a truncated turn: %v", err)
	}
}

func TestParseEventStreamMaxTokensStillRequiresAgentRecovery(t *testing.T) {
	const marker = "<agent_final_0123456789abcdef>"
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": marker + "partial answer"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "MAX_TOKENS"}),
	}, nil))

	var stopReason string
	err := parseEventStream(stream, &KiroStreamCallback{
		AgentCompletionMarker: marker,
		OnStopReason:          func(reason string) { stopReason = reason },
	})
	if stopReason != "max_tokens" || !isUpstreamOutputTruncatedError(err) {
		t.Fatalf("max-token truncation was not routed to recovery: stop=%q err=%v", stopReason, err)
	}
}

func TestParseEventStreamInterruptedAgentOutputUsesSemanticRecovery(t *testing.T) {
	streamBytes := awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
		"text": "I have identified the next action.",
	})
	streamBytes = append(streamBytes, 0, 0, 0)

	var callbackErr error
	err := parseEventStream(bytes.NewReader(streamBytes), &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		OnError:               func(err error) { callbackErr = err },
	})
	if !isUpstreamIncompleteResponseError(err) || !isUpstreamAgentMarkerMismatchError(err) {
		t.Fatalf("interrupted agent output was not recoverable: %v", err)
	}
	if callbackErr == nil || callbackErr.Error() != err.Error() || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("callbackErr=%v err=%v", callbackErr, err)
	}
}

func TestParseEventStreamPreservesStructuredReasoning(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
			"text":            "thinking",
			"signature":       "signed-thinking",
			"redactedContent": "encrypted-thinking",
		}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
			"text": "thinking further",
		}),
	}, nil))

	var reasoning []KiroReasoningContentEvent
	var legacyThinking []string
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				legacyThinking = append(legacyThinking, text)
			}
		},
		OnReasoning: func(event KiroReasoningContentEvent) {
			reasoning = append(reasoning, event)
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(reasoning) != 2 {
		t.Fatalf("expected two reasoning callbacks, got %#v", reasoning)
	}
	if reasoning[0].Text != "thinking" || reasoning[0].Signature != "signed-thinking" || reasoning[0].RedactedContent != "encrypted-thinking" {
		t.Fatalf("reasoning fields were not preserved: %#v", reasoning[0])
	}
	if reasoning[1].Text != "thinking further" {
		t.Fatalf("expected reasoning delta to pass through unchanged, got %q", reasoning[1].Text)
	}
	if len(legacyThinking) != 0 {
		t.Fatalf("OnReasoning must replace the legacy thinking callback, got %#v", legacyThinking)
	}
}

func TestParseEventStreamPreservesSignatureAndRedactedOnlyReasoning(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"signature": "signature-only"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"redacted_content": "redacted-only"}),
	}, nil))

	var reasoning []KiroReasoningContentEvent
	err := parseEventStream(stream, &KiroStreamCallback{
		OnReasoning: func(event KiroReasoningContentEvent) {
			reasoning = append(reasoning, event)
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(reasoning) != 2 || reasoning[0].Signature != "signature-only" || reasoning[1].RedactedContent != "redacted-only" {
		t.Fatalf("signature/redacted-only events were lost: %#v", reasoning)
	}
}

func TestParseEventStreamKeepsLegacyReasoningTextCallback(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
		"text":      "legacy thinking",
		"signature": "not-exposed-by-legacy-callback",
	}))

	var text string
	var thinking bool
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(chunk string, isThinking bool) {
			text += chunk
			thinking = isThinking
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if text != "legacy thinking" || !thinking {
		t.Fatalf("legacy reasoning callback changed: text=%q thinking=%v", text, thinking)
	}
}

func TestParseEventStreamAccumulatesInterleavedToolUsesByID(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_a", "name": "alpha", "input": `{"value":`,
		}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_b", "name": "beta", "input": `{"value":`,
		}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_a", "name": "alpha", "input": `1}`, "stop": true,
		}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_b", "name": "beta", "input": `2}`, "stop": true,
		}),
	}, nil))

	var toolUses []KiroToolUse
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(toolUses) != 2 {
		t.Fatalf("expected two completed tool uses, got %#v", toolUses)
	}
	if toolUses[0].ToolUseID != "toolu_a" || toolUses[0].Name != "alpha" || toolUses[0].Input["value"] != json.Number("1") {
		t.Fatalf("first tool fragments were mixed: %#v", toolUses[0])
	}
	if toolUses[1].ToolUseID != "toolu_b" || toolUses[1].Name != "beta" || toolUses[1].Input["value"] != json.Number("2") {
		t.Fatalf("second tool fragments were mixed: %#v", toolUses[1])
	}
}

func TestParseEventStreamRejectsInvalidCompletedToolJSON(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_bad",
		"name":      "broken",
		"input":     `{"unterminated":`,
		"stop":      true,
	}))

	var callbackErr error
	var completed bool
	var toolUses []KiroToolUse
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse:  func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
		OnComplete: func(_, _ int) { completed = true },
		OnError:    func(err error) { callbackErr = err },
	})
	if err == nil || !strings.Contains(err.Error(), "invalid JSON for tool use") {
		t.Fatalf("expected invalid tool JSON error, got %v", err)
	}
	if callbackErr == nil || callbackErr.Error() != err.Error() {
		t.Fatalf("expected OnError to receive invalid JSON error, got %v", callbackErr)
	}
	if completed || len(toolUses) != 0 {
		t.Fatalf("invalid tool JSON must not complete or emit: completed=%v tools=%#v", completed, toolUses)
	}
}

func TestParseEventStreamPreservesLargeToolIntegers(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_large",
		"name":      "lookup",
		"input":     `{"id":9007199254740993}`,
		"stop":      true,
	}))
	var toolUse KiroToolUse
	if err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(value KiroToolUse) { toolUse = value },
	}); err != nil {
		t.Fatal(err)
	}
	if got := toolUse.Input["id"]; got != json.Number("9007199254740993") {
		t.Fatalf("large integer = %#v", got)
	}
	raw, err := json.Marshal(toolUse.Input)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"id":9007199254740993}` {
		t.Fatalf("large integer JSON = %s", raw)
	}
}

func TestToolUseAccumulatorRejectsOversizedInput(t *testing.T) {
	accumulator := newToolUseAccumulator()
	err := accumulator.push(map[string]interface{}{
		"toolUseId": "toolu_large",
		"name":      "lookup",
		"input":     strings.Repeat("x", maxKiroToolInputBytes+1),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected oversized tool input error, got %v", err)
	}
}

func TestToolUseAccumulatorLimitsActiveTools(t *testing.T) {
	accumulator := newToolUseAccumulator()
	for i := 0; i < maxKiroActiveToolUses; i++ {
		if err := accumulator.push(map[string]interface{}{
			"toolUseId": fmt.Sprintf("toolu_%d", i),
			"name":      "lookup",
		}, nil); err != nil {
			t.Fatalf("tool %d: %v", i, err)
		}
	}
	err := accumulator.push(map[string]interface{}{
		"toolUseId": "toolu_over_limit",
		"name":      "lookup",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "too many active") {
		t.Fatalf("expected active tool limit error, got %v", err)
	}
}

func TestParseEventStreamPropagatesErrorAndExceptionFrames(t *testing.T) {
	tests := []struct {
		name        string
		headers     map[string]string
		payload     map[string]interface{}
		wantMessage string
	}{
		{
			name: "error",
			headers: map[string]string{
				":message-type": "error",
				":error-code":   "BadRequest",
			},
			payload:     map[string]interface{}{"message": "request rejected"},
			wantMessage: "Kiro stream error BadRequest: request rejected",
		},
		{
			name: "exception",
			headers: map[string]string{
				":message-type":   "exception",
				":exception-type": "ValidationException",
			},
			payload:     map[string]interface{}{"errorMessage": "invalid tool"},
			wantMessage: "Kiro stream exception ValidationException: invalid tool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := bytes.NewReader(awsEventStreamFrameWithHeaders(t, tt.headers, tt.payload))
			var callbackErr error
			var completed bool
			err := parseEventStream(stream, &KiroStreamCallback{
				OnError:    func(err error) { callbackErr = err },
				OnComplete: func(_, _ int) { completed = true },
			})
			if err == nil || err.Error() != tt.wantMessage {
				t.Fatalf("expected %q, got %v", tt.wantMessage, err)
			}
			if callbackErr == nil || callbackErr.Error() != err.Error() {
				t.Fatalf("expected OnError to receive stream error, got %v", callbackErr)
			}
			if completed {
				t.Fatalf("completion callback must not run after a stream error")
			}
		})
	}
}

func TestParseEventStreamNilCallbackIsNoOp(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.25}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"name":  "mcpIdaProMcpStatus",
			"input": `{"server":"ida-pro-mcp"}`,
			"stop":  true,
		}),
	}, nil))

	if err := parseEventStream(stream, nil); err != nil {
		t.Fatalf("expected nil callback to be a no-op, got %v", err)
	}
}

func TestParseEventStreamNilCallbackFieldsAreNoOp(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "hello",
	}))

	if err := parseEventStream(stream, &KiroStreamCallback{}); err != nil {
		t.Fatalf("expected empty callback to be a no-op, got %v", err)
	}
}

func TestHandleToolUseEventGeneratesMissingToolUseID(t *testing.T) {
	var toolUses []KiroToolUse
	current := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":"ida-pro-mcp"}`,
		"stop":  true,
	}, nil, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})

	if current != nil {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID == "" {
		t.Fatalf("expected generated tool use id")
	}
	if toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool name: %q", toolUses[0].Name)
	}
}

func TestHandleToolUseEventReplacesGeneratedIDWhenRealIDArrives(t *testing.T) {
	var toolUses []KiroToolUse
	callback := &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	}

	current := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":`,
	}, nil, callback)
	current = handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_real",
		"name":      "mcpIdaProMcpStatus",
		"input":     `"ida-pro-mcp"}`,
		"stop":      true,
	}, current, callback)

	if current != nil {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one completed tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_real" {
		t.Fatalf("expected real tool id to replace generated id, got %q", toolUses[0].ToolUseID)
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected joined tool input, got %#v", toolUses[0].Input)
	}
}

func TestBuildKiroTransportUsesExplicitProxyURL(t *testing.T) {
	transport := buildKiroTransport("http://proxy.local:8080")
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
	if transport.MaxConnsPerHost != 0 {
		t.Fatalf("proxy MaxConnsPerHost=%d, want unlimited", transport.MaxConnsPerHost)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("proxied transport should use HTTP/1 connections")
	}
}

func TestBuildKiroTransportFallsBackToEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-proxy.local:2323")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	transport := buildKiroTransport("")
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://env-proxy.local:2323")
}

func TestInitKiroHttpClientDoesNotLimitStreamLifetime(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	streamClient := kiroHttpStore.Load()
	restClient := kiroRestHttpStore.Load()

	if streamClient.Timeout != 0 {
		t.Fatalf("expected streaming timeout to follow request context, got %s", streamClient.Timeout)
	}
	if restClient.Timeout != 30*time.Second {
		t.Fatalf("expected REST timeout to stay 30s, got %s", restClient.Timeout)
	}
}

func TestPerAccountProxyClientDoesNotLimitStreamLifetime(t *testing.T) {
	proxyURL := "http://proxy-timeout.example.test:8080"
	proxyClientCache.Delete(proxyURL)
	t.Cleanup(func() { proxyClientCache.Delete(proxyURL) })

	streamClient := GetClientForProxy(proxyURL)
	if streamClient.Timeout != 0 {
		t.Fatalf("expected proxied streaming timeout to follow request context, got %s", streamClient.Timeout)
	}
}

func TestSetPayloadProfileArnForAccountUsesAccountArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:profile/stale"}

	setPayloadProfileArnForAccount(payload, &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/current "})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/current" {
		t.Fatalf("expected current account profile ARN, got %q", payload.ProfileArn)
	}
}

func TestSetPayloadProfileArnForAccountPreservesExplicitPayloadArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: " arn:aws:codewhisperer:profile/explicit "}

	setPayloadProfileArnForAccount(payload, &config.Account{})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/explicit" {
		t.Fatalf("expected explicit payload profile ARN to be preserved, got %q", payload.ProfileArn)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid test URL: %v", err)
	}
	return parsed
}

func assertProxyURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected proxy URL %q, got nil", want)
	}
	if got.String() != want {
		t.Fatalf("expected proxy URL %q, got %q", want, got.String())
	}
}

func awsEventStreamFrame(t *testing.T, eventType string, payload map[string]interface{}) []byte {
	t.Helper()
	return awsEventStreamFrameWithHeaders(t, map[string]string{
		":message-type": "event",
		":event-type":   eventType,
	}, payload)
}

func awsEventStreamFrameWithHeaders(t *testing.T, stringHeaders map[string]string, payload map[string]interface{}) []byte {
	t.Helper()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return awsEventStreamFrameWithRawPayload(t, stringHeaders, payloadBytes)
}

func awsEventStreamFrameWithRawPayload(t *testing.T, stringHeaders map[string]string, payloadBytes []byte) []byte {
	t.Helper()

	var headers []byte
	for name, value := range stringHeaders {
		headerValue := []byte(value)
		headers = append(headers, byte(len(name)))
		headers = append(headers, []byte(name)...)
		headers = append(headers, byte(7))
		headers = append(headers, byte(len(headerValue)>>8), byte(len(headerValue)))
		headers = append(headers, headerValue...)
	}

	totalLength := 12 + len(headers) + len(payloadBytes) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headers...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}

// TestCallKiroAPIRebuildsRegionForApiKeyAccount asserts an api_key account's
// endpoint host is rebuilt from EffectiveApiRegion (q.<region> for non-us-east-1
// endpoints), since it has no profile ARN to derive the region from.
func TestCallKiroAPIRebuildsRegionForApiKeyAccount(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	var gotHost string
	// CallKiroAPI uses the streaming store (kiroHttpStore), not the REST store.
	kiroHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotHost = req.URL.Host
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	apiKeyAcct := &config.Account{
		AuthMethod:  "api_key",
		KiroApiKey:  "k",
		AccessToken: "k",
		ApiRegion:   "eu-central-1",
	}
	payload := &KiroPayload{}
	_ = CallKiroAPI(apiKeyAcct, payload, nil)
	if gotHost != "q.eu-central-1.amazonaws.com" {
		t.Fatalf("api_key host: want q.eu-central-1.amazonaws.com, got %q", gotHost)
	}
}

func TestKiroUsesOnlyRegionalQEndpoint(t *testing.T) {
	if len(kiroEndpoints) != 1 {
		t.Fatalf("streaming endpoint count: want 1, got %d", len(kiroEndpoints))
	}

	ep := kiroEndpoints[0]
	if ep.URL != "https://q.us-east-1.amazonaws.com/generateAssistantResponse" {
		t.Fatalf("unexpected streaming endpoint: %q", ep.URL)
	}
	if got := regionalizeURLForRegion(ep.URL, "eu-central-1"); got != "https://q.eu-central-1.amazonaws.com/generateAssistantResponse" {
		t.Fatalf("regional endpoint: got %q", got)
	}
}

func TestCallKiroAPIApiKeyUsesSingleRegionalQRequest(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	type capturedRequest struct {
		URL           string
		Host          string
		Authorization string
		TokenType     string
		AmzTarget     string
		Connection    string
		UserAgent     string
		Close         bool
	}
	var requests []capturedRequest
	kiroHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests = append(requests, capturedRequest{
				URL:           req.URL.String(),
				Host:          req.Host,
				Authorization: req.Header.Get("Authorization"),
				TokenType:     req.Header.Get("tokentype"),
				AmzTarget:     req.Header.Get("X-Amz-Target"),
				Connection:    req.Header.Get("Connection"),
				UserAgent:     req.Header.Get("User-Agent"),
				Close:         req.Close,
			})
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader("unavailable")),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{
		ID:          "api-key-regional-q",
		AuthMethod:  "api_key",
		KiroApiKey:  "kiro-key-123",
		AccessToken: "must-not-be-used",
		ApiRegion:   "eu-central-1",
		MachineId:   "machine-123",
	}
	payload := &KiroPayload{}
	payload.ConversationState.ConversationID = "conversation-123"
	err := CallKiroAPI(account, payload, nil)
	var httpErr *KiroHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("CallKiroAPI error=%v, want HTTP 503", err)
	}

	if len(requests) != 1 {
		t.Fatalf("request count: want 1, got %d", len(requests))
	}
	req := requests[0]
	if req.URL != "https://q.eu-central-1.amazonaws.com/generateAssistantResponse" || req.Host != "q.eu-central-1.amazonaws.com" {
		t.Errorf("request route: URL=%q Host=%q", req.URL, req.Host)
	}
	if req.Authorization != "Bearer kiro-key-123" || req.TokenType != "API_KEY" {
		t.Errorf("credential headers: authorization=%q tokentype=%q", req.Authorization, req.TokenType)
	}
	if req.AmzTarget != "" {
		t.Errorf("regional Q request must not set X-Amz-Target: %q", req.AmzTarget)
	}
	if req.Connection != "" || req.Close {
		t.Errorf("regional Q request disabled connection reuse: header=%q close=%v", req.Connection, req.Close)
	}
	if strings.Contains(req.UserAgent, "api/codewhispererruntime#1.0.0") {
		t.Errorf("regional Q request used runtime fingerprint: %q", req.UserAgent)
	}
}

// TestAccountEmailForLogMasksApiKey ensures api_key accounts (which have no email
// until their first successful refresh) are not blind in logs: fall back to the masked
// key. When an email is present it wins.
func TestAccountEmailForLogMasksApiKey(t *testing.T) {
	acct := &config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_abcdefghijklmnop"}
	if got := accountEmailForLog(acct); got != "ksk_ab…mnop" {
		t.Fatalf("api_key no-email: want masked key %q, got %q", "ksk_ab…mnop", got)
	}
	acct.Email = "user@example.com"
	if got := accountEmailForLog(acct); got != "user@example.com" {
		t.Fatalf("email present: want %q, got %q", "user@example.com", got)
	}
	if got := accountEmailForLog(nil); got != "<nil>" {
		t.Fatalf("nil account: want %q, got %q", "<nil>", got)
	}
}
