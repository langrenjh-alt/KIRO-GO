package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-go/config"
)

const (
	retryBillingEstimatedInput = 5
	retryBillingUpstreamInput  = 37
	retryBillingUpstreamOutput = 11
	retryBillingCachedTokens   = retryBillingUpstreamInput + retryBillingUpstreamOutput
)

func newRetryBillingContractHandler(t *testing.T) *Handler {
	t.Helper()
	attempts := 0
	return newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		attempts++
		if attempts == 1 {
			callback.OnComplete(retryBillingUpstreamInput, retryBillingUpstreamOutput)
			return newUpstreamAgentMarkerMismatchError()
		}
		callback.OnText(payload.AgentCompletionMarker+"done", false)
		callback.OnStopReason("END_TURN")
		callback.OnComplete(13, 2)
		return nil
	})
}

func retryBillingPayload() *KiroPayload {
	return completionProtocolTestPayload()
}

func assertClaudeRetryCacheUsage(t *testing.T, usage map[string]interface{}) {
	t.Helper()
	if got := int(usage["cache_read_input_tokens"].(float64)); got != retryBillingCachedTokens {
		t.Fatalf("cache_read_input_tokens=%d, want discarded retry total %d; usage=%#v", got, retryBillingCachedTokens, usage)
	}
	if got := int(usage["cache_creation_input_tokens"].(float64)); got != 0 {
		t.Fatalf("cache_creation_input_tokens=%d, want 0; usage=%#v", got, usage)
	}
	if got := int(usage["input_tokens"].(float64)); got != retryBillingEstimatedInput {
		t.Fatalf("uncached input_tokens=%d, want final request input %d; usage=%#v", got, retryBillingEstimatedInput, usage)
	}
}

func assertOpenAIRetryCacheUsage(t *testing.T, usage map[string]interface{}, inputField string) {
	t.Helper()
	if got := int(usage[inputField].(float64)); got != retryBillingEstimatedInput+retryBillingCachedTokens {
		t.Fatalf("%s=%d, want final input plus discarded retry %d; usage=%#v", inputField, got, retryBillingEstimatedInput+retryBillingCachedTokens, usage)
	}
	details := responseContractMap(t, usage[map[string]string{
		"prompt_tokens": "prompt_tokens_details",
		"input_tokens":  "input_tokens_details",
	}[inputField]], inputField+" details")
	if got := int(details["cached_tokens"].(float64)); got != retryBillingCachedTokens {
		t.Fatalf("cached_tokens=%d, want discarded retry total %d; usage=%#v", got, retryBillingCachedTokens, usage)
	}
}

func TestDiscardedRetryUsageIsCacheReadAcrossProtocols(t *testing.T) {
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "false")
	model := "claude-opus-5"

	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "claude_non_stream", true: "claude_stream"}[stream], func(t *testing.T) {
			h := newRetryBillingContractHandler(t)
			rec := httptest.NewRecorder()
			if stream {
				h.handleClaudeStream(rec, retryBillingPayload(), model, model, false, claudeThinkingResponseOptions{}, retryBillingEstimatedInput, nil, "")
			} else {
				h.handleClaudeNonStream(rec, retryBillingPayload(), model, model, false, claudeThinkingResponseOptions{}, retryBillingEstimatedInput, nil, "")
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !stream {
				var envelope map[string]interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				assertClaudeRetryCacheUsage(t, responseContractMap(t, envelope["usage"], "usage"))
				return
			}
			var terminal map[string]interface{}
			for _, event := range decodeResponseContractSSE(t, rec.Body.String()) {
				if event.Name == "message_delta" {
					terminal = responseContractMap(t, event.Data["usage"], "message_delta.usage")
				}
			}
			if terminal == nil {
				t.Fatalf("missing terminal usage: %s", rec.Body.String())
			}
			assertClaudeRetryCacheUsage(t, terminal)
		})

		t.Run(map[bool]string{false: "chat_non_stream", true: "chat_stream"}[stream], func(t *testing.T) {
			h := newRetryBillingContractHandler(t)
			rec := httptest.NewRecorder()
			if stream {
				h.handleOpenAIStream(rec, retryBillingPayload(), model, model, false, true, retryBillingEstimatedInput, "")
			} else {
				h.handleOpenAINonStream(rec, retryBillingPayload(), model, model, false, retryBillingEstimatedInput, "")
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !stream {
				var envelope map[string]interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				assertOpenAIRetryCacheUsage(t, responseContractMap(t, envelope["usage"], "usage"), "prompt_tokens")
				return
			}
			chunks, done := decodeOpenAIDataSSE(t, rec.Body.String())
			if !done {
				t.Fatalf("stream is missing [DONE]: %s", rec.Body.String())
			}
			var terminal map[string]interface{}
			for _, chunk := range chunks {
				if raw, ok := chunk["usage"].(map[string]interface{}); ok {
					terminal = raw
				}
			}
			if terminal == nil {
				t.Fatalf("missing terminal usage: %s", rec.Body.String())
			}
			assertOpenAIRetryCacheUsage(t, terminal, "prompt_tokens")
		})

		t.Run(map[bool]string{false: "responses_non_stream", true: "responses_stream"}[stream], func(t *testing.T) {
			h := newRetryBillingContractHandler(t)
			rec := httptest.NewRecorder()
			req := &ResponsesRequest{Model: model, Stream: stream, Input: json.RawMessage(`"test"`)}
			if stream {
				h.handleResponsesStream(rec, retryBillingPayload(), model, model, false, retryBillingEstimatedInput, "", "resp_retry", req, nil, false)
			} else {
				h.handleResponsesNonStream(rec, retryBillingPayload(), model, model, false, retryBillingEstimatedInput, "", "resp_retry", req, nil, false)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !stream {
				var envelope map[string]interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				assertOpenAIRetryCacheUsage(t, responseContractMap(t, envelope["usage"], "usage"), "input_tokens")
				return
			}
			var terminal map[string]interface{}
			for _, event := range decodeResponseContractSSE(t, rec.Body.String()) {
				if event.Name == "response.completed" {
					response := responseContractMap(t, event.Data["response"], "response.completed.response")
					terminal = responseContractMap(t, response["usage"], "response.completed.response.usage")
				}
			}
			if terminal == nil {
				t.Fatalf("missing response.completed usage: %s", rec.Body.String())
			}
			assertOpenAIRetryCacheUsage(t, terminal, "input_tokens")
		})
	}
}

func TestClaudeStreamPreludeCarriesBufferedRetryAsCacheRead(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnText("done", false)
		callback.OnComplete(13, 2)
		return nil
	})
	preludeRetry := retryBillingUsage{}
	preludeRetry.add(newDiscardedAttemptUsage(retryBillingUpstreamInput, retryBillingUpstreamOutput, 1.25))
	rec := httptest.NewRecorder()
	h.handleClaudeStreamWithPrelude(
		rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false,
		claudeThinkingResponseOptions{}, retryBillingEstimatedInput, nil, "",
		&claudeStreamPrelude{InputTokens: 7, RetryUsage: preludeRetry},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var terminal map[string]interface{}
	for _, event := range decodeResponseContractSSE(t, rec.Body.String()) {
		if event.Name == "message_delta" {
			terminal = responseContractMap(t, event.Data["usage"], "message_delta.usage")
		}
	}
	if terminal == nil {
		t.Fatalf("missing terminal usage: %s", rec.Body.String())
	}
	if got := int(terminal["cache_read_input_tokens"].(float64)); got != retryBillingCachedTokens {
		t.Fatalf("cache_read_input_tokens=%d, want advisor retry total %d; usage=%#v", got, retryBillingCachedTokens, terminal)
	}
	if got := int(terminal["input_tokens"].(float64)); got != retryBillingEstimatedInput+7 {
		t.Fatalf("uncached input_tokens=%d, want executor plus advisor input 12; usage=%#v", got, terminal)
	}
}

const structuredRetryRequestBody = `{
	"model":"claude-sonnet-4-6",
	"max_tokens":64,
	"messages":[{"role":"user","content":"answer"}],
	"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"integer"}}}}}
}`

// A discarded structured-output attempt must settle inside its own iteration.
// Deferring it to the successful attempt made the client-visible usage report
// tokens that turn never generated, and dropped the usage entirely whenever the
// retry chain ended in an error response instead of a success.
func TestStructuredOutputRetrySettlesEachAttemptSeparately(t *testing.T) {
	h := newResponseContractHandler(t)

	attempts := 0
	h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		attempts++
		if attempts == 1 {
			// Schema-invalid, and deliberately long so a carried-over total would
			// be obvious in the final response.
			callback.OnText(strings.Repeat("this attempt violates the schema. ", 40), false)
		} else {
			callback.OnText(`{"answer":7}`, false)
		}
		callback.OnComplete(0, 0)
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(structuredRetryRequestBody))
	rec := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("expected one validation retry, got %d upstream attempts", attempts)
	}

	var response ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	finalOutput := estimateApproxTokens(`{"answer":7}`)
	if response.Usage.OutputTokens != finalOutput {
		t.Fatalf("client-visible output_tokens=%d, want only this attempt's %d",
			response.Usage.OutputTokens, finalOutput)
	}

	// Internal accounting still sees both attempts: the discarded one settled in
	// its own iteration rather than vanishing.
	if got := atomic.LoadInt64(&h.totalRequests); got != 2 {
		t.Fatalf("internal requests=%d, want both attempts accounted", got)
	}
	if got := atomic.LoadInt64(&h.totalTokens); int(got) <= finalOutput {
		t.Fatalf("internal tokens=%d did not include the discarded attempt", got)
	}
}

// The discarded attempt is billed even when every later attempt fails, so the
// tokens Kiro already produced are never silently written off.
func TestStructuredOutputRetryAccountsDiscardedAttemptOnFailure(t *testing.T) {
	h := newResponseContractHandler(t)

	attempts := 0
	h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		attempts++
		if attempts == 1 {
			callback.OnText(strings.Repeat("still not valid json. ", 30), false)
			callback.OnComplete(0, 0)
			return nil
		}
		return newAuthError(500, "api_error", "upstream exploded")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(structuredRetryRequestBody))
	rec := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected the retry chain to surface an error, got 200: %s", rec.Body.String())
	}
	if got := atomic.LoadInt64(&h.totalTokens); got <= 0 {
		t.Fatalf("discarded attempt tokens were lost when the retry failed: total=%d", got)
	}
}

// A reasoning-only agent attempt is discarded before reaching the client, but
// Kiro already produced those thinking tokens upstream. The streaming paths used
// to skip accounting entirely on that branch, which hid a reasoning loop from
// both the usage counters and the quota gate.
func TestReasoningOnlyDiscardedAttemptIsBilledOnStreamingPaths(t *testing.T) {
	const loopedThinking = "考虑这个问题的另一种可能性，再回到原点重新判断。"

	for _, tc := range []struct {
		name string
		path string
		body string
		call func(*Handler, *httptest.ResponseRecorder, *http.Request)
	}{
		{
			name: "claude stream",
			path: "/v1/messages",
			body: `{"model":"claude-sonnet-4-6","max_tokens":2048,"stream":true,
				"thinking":{"type":"enabled","budget_tokens":1024},
				"system":"You are a coding agent.",
				"messages":[{"role":"user","content":"fix the bug"}],
				"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`,
			call: func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request) {
				h.handleClaudeMessagesInternal(rec, req)
			},
		},
		{
			name: "openai stream",
			path: "/v1/chat/completions",
			body: `{"model":"claude-sonnet-4-6","stream":true,"reasoning_effort":"high",
				"messages":[{"role":"system","content":"You are a coding agent."},
					{"role":"user","content":"fix the bug"}],
				"tools":[{"type":"function","function":{"name":"Bash","parameters":{"type":"object"}}}]}`,
			call: func(h *Handler, rec *httptest.ResponseRecorder, req *http.Request) {
				h.handleOpenAIChat(rec, req)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponseContractHandler(t)

			attempts := 0
			h.kiroCaller = func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				attempts++
				if payload.AgentCompletionMarker == "" {
					t.Fatal("coding-agent completion marker was not enabled")
				}
				if attempts == 1 {
					// Neither a marker nor a tool call: the attempt is discarded.
					for i := 0; i < 12; i++ {
						callback.OnText(loopedThinking, true)
					}
					callback.OnStopReason("END_TURN")
					callback.OnComplete(0, 0)
					return nil
				}
				callback.OnToolUse(KiroToolUse{
					ToolUseID: "call_1",
					Name:      "Bash",
					Input:     map[string]interface{}{"command": "go test ./..."},
				})
				callback.OnStopReason("TOOL_USE")
				callback.OnComplete(0, 0)
				return nil
			}

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			tc.call(h, rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if attempts < 2 {
				t.Fatalf("expected the discarded attempt to trigger recovery, got %d attempts", attempts)
			}

			// The recovered attempt only emitted a short tool call, so anything
			// beyond that must come from the discarded reasoning.
			minThinking := estimateApproxTokens(strings.Repeat(loopedThinking, 12))
			if got := atomic.LoadInt64(&h.totalTokens); int(got) < minThinking {
				t.Fatalf("internal tokens=%d did not include the discarded reasoning (>=%d)", got, minThinking)
			}
			if got := atomic.LoadInt64(&h.totalRequests); int(got) < attempts {
				t.Fatalf("internal requests=%d, want every attempt accounted (%d)", got, attempts)
			}
		})
	}
}
