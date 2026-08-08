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
