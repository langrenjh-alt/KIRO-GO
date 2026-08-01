package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"kiro-go/config"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseEventStreamRejectsPendingToolUseOnEOF(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "stream ended before stop=true") {
		t.Fatalf("expected incomplete tool use error, got %v", err)
	}
	if callbackErr == nil || callbackErr.Error() != err.Error() {
		t.Fatalf("expected OnError to receive parse error, got %v", callbackErr)
	}
	if completed {
		t.Fatalf("completion callback must not run after an incomplete tool use")
	}
	if len(toolUses) != 0 {
		t.Fatalf("incomplete tool use must not be emitted, got %#v", toolUses)
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
	if err != nil {
		t.Fatalf("content length termination returned error: %v", err)
	}
	if content != "partial answer" || stopReason != "max_tokens" || !completed {
		t.Fatalf("content=%q stop=%q completed=%v", content, stopReason, completed)
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

func TestInitKiroHttpClientKeepsShortRestTimeout(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	streamClient := kiroHttpStore.Load()
	restClient := kiroRestHttpStore.Load()

	if streamClient.Timeout != 5*time.Minute {
		t.Fatalf("expected streaming timeout to be 5m, got %s", streamClient.Timeout)
	}
	if restClient.Timeout != 30*time.Second {
		t.Fatalf("expected REST timeout to stay 30s, got %s", restClient.Timeout)
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

func TestKiroStreamingUsesOnlyRegionalQEndpoint(t *testing.T) {
	if len(kiroEndpoints) != 1 {
		t.Fatalf("streaming endpoint count: want 1, got %d", len(kiroEndpoints))
	}

	ep := kiroEndpoints[0]
	if ep.URL != "https://q.us-east-1.amazonaws.com/generateAssistantResponse" {
		t.Fatalf("unexpected streaming endpoint: %q", ep.URL)
	}
	if ep.AmzTarget != "" {
		t.Fatalf("regional Q endpoint must not use a fallback X-Amz-Target: %q", ep.AmzTarget)
	}

	got := regionalizeURLForRegion(ep.URL, "eu-central-1")
	want := "https://q.eu-central-1.amazonaws.com/generateAssistantResponse"
	if got != want {
		t.Fatalf("regional endpoint: want %q, got %q", want, got)
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
