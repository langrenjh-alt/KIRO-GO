package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

// These are public HTTP contracts with an injected upstream transport, not
// CCTest fixtures, platform-identity rewrites, or live account requests.
func nonStreamContractObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content-type=%q body=%s", rec.Header().Get("Content-Type"), rec.Body.String())
	}
	var obj map[string]interface{}
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	if err := dec.Decode(&obj); err != nil {
		t.Fatal(err)
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("response has a trailing JSON value: %v", err)
	}
	return obj
}

func TestNonStreamContractStopReasonPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, upstream, want string
		tools                int
		context              bool
	}{
		{name: "natural", upstream: "END_TURN", want: "end_turn"},
		{name: "refusal", upstream: "refusal", want: "refusal"},
		{name: "refusal case and whitespace", upstream: " REFUSAL ", want: "refusal"},
		{name: "paused", upstream: "pause_turn", want: "pause_turn"},
		{name: "paused case and whitespace", upstream: " PAUSE_TURN ", want: "pause_turn"},
		{name: "max tokens", upstream: "MAX_TOKENS", want: "max_tokens"},
		{name: "context alias", upstream: "CONTEXT_WINDOW_EXCEEDED", want: "model_context_window_exceeded"},
		{name: "explicit context", upstream: "model_context_window_exceeded", want: "model_context_window_exceeded"},
		{name: "unknown stays natural", upstream: "future_unknown_value", want: "end_turn"},
		{name: "pending client tool", upstream: "END_TURN", tools: 1, want: "tool_use"},
		{name: "pending tool before pause", upstream: "pause_turn", tools: 1, want: "tool_use"},
		{name: "pending tool before refusal", upstream: "refusal", tools: 1, want: "tool_use"},
		{name: "length before tool", upstream: "MAX_TOKENS", tools: 1, want: "max_tokens"},
		{name: "context before tool and pause", upstream: "pause_turn", tools: 1, context: true, want: "model_context_window_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveClaudeStopReason(tc.tools, tc.context, tc.upstream); got != tc.want {
				t.Fatalf("stop_reason=%q want=%q (upstream=%q tools=%d context=%v)", got, tc.want, tc.upstream, tc.tools, tc.context)
			}
		})
	}
}

func TestNonStreamContractClaudeHTTPStopReasons(t *testing.T) {
	for _, reason := range []string{"end_turn", "refusal", "pause_turn", " REFUSAL ", " PAUSE_TURN ", "MODEL_CONTEXT_WINDOW_EXCEEDED", "unrecognized_future_stop"} {
		t.Run(reason, func(t *testing.T) {
			var calls int
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, c *KiroStreamCallback) error {
				calls++
				c.OnText("An actual upstream answer.", false)
				c.OnStopReason(reason)
				c.OnComplete(20, 6)
				return nil
			})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":256,"stream":false,"messages":[{"role":"user","content":"Answer this request."}]}`)))
			obj := nonStreamContractObject(t, rec)
			want := strings.ToLower(strings.TrimSpace(reason))
			if reason == "unrecognized_future_stop" {
				want = "end_turn"
			}
			if rec.Code != http.StatusOK || calls != 1 || obj["stop_reason"] != want {
				t.Fatalf("HTTP=%d calls=%d stop_reason=%v want=%s body=%s", rec.Code, calls, obj["stop_reason"], want, rec.Body.String())
			}
			if obj["model"] != "claude-sonnet-4-6" || obj["type"] != "message" || obj["role"] != "assistant" {
				t.Fatalf("message envelope=%v", obj)
			}
			if seq, exists := obj["stop_sequence"]; !exists || seq != nil {
				t.Fatalf("stop_sequence=%v exists=%v", seq, exists)
			}
		})
	}
}

func TestNonStreamContractMatchedStopOverridesToolAndLength(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, c *KiroStreamCallback) error {
		c.OnToolUse(KiroToolUse{ToolUseID: "contract-tool", Name: "lookup", Input: map[string]interface{}{"city": "Paris"}})
		c.OnText("before<STOP>hidden", false)
		c.OnStopReason("MAX_TOKENS")
		c.OnComplete(20, 6)
		return nil
	})
	payload := emptyVisibleOutputPayload()
	payload.StopSequences = []string{"<STOP>"}
	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, payload, "claude-sonnet-4.6", "claude-sonnet-4-6", false, claudeThinkingResponseOptions{}, 20, nil, "")
	obj := nonStreamContractObject(t, rec)
	if rec.Code != http.StatusOK || obj["stop_reason"] != "stop_sequence" || obj["stop_sequence"] != "<STOP>" || strings.Contains(rec.Body.String(), "hidden") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	content, _ := obj["content"].([]interface{})
	if len(content) != 2 || content[0].(map[string]interface{})["text"] != "before" || content[1].(map[string]interface{})["type"] != "tool_use" {
		t.Fatalf("matched stop lost retained text/tool: %v", content)
	}
}

func TestNonStreamContractPublicEnvelopesToolsAndCacheUsage(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		for _, tool := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tool=%v", path, tool), func(t *testing.T) {
				h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, c *KiroStreamCallback) error {
					if tool {
						c.OnToolUse(KiroToolUse{ToolUseID: "contract-tool", Name: "lookup", Input: map[string]interface{}{"city": "Paris"}})
						c.OnStopReason("TOOL_USE")
					} else {
						c.OnText("Confirmed: Unicode works.", false)
						c.OnStopReason("END_TURN")
					}
					c.OnComplete(200, 8)
					return nil
				})
				setBillingInflation(t, 50)
				mode := config.CacheModeFixedRatio
				ratio := config.FixedRatioCacheConfig{CacheReadPercent: 50, CacheCreationPercent: 30}
				if err := config.UpdateCachePolicy(&mode, nil, &ratio); err != nil {
					t.Fatal(err)
				}
				body := `{"model":"claude-sonnet-4-6","max_tokens":256,"stream":false,"messages":[{"role":"user","content":"Provide a detailed answer for the request."}]}`
				if tool {
					if path == "/v1/messages" {
						body = strings.TrimSuffix(body, "}") + `,"tools":[{"name":"lookup","description":"Find a city.","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`
					} else {
						body = strings.TrimSuffix(body, "}") + `,"tools":[{"type":"function","function":{"name":"lookup","description":"Find a city.","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`
					}
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
				obj := nonStreamContractObject(t, rec)
				if rec.Code != http.StatusOK || obj["model"] != "claude-sonnet-4-6" {
					t.Fatalf("HTTP=%d body=%s", rec.Code, rec.Body.String())
				}
				usage, ok := obj["usage"].(map[string]interface{})
				if !ok {
					t.Fatalf("missing usage: %v", obj)
				}
				if path == "/v1/messages" {
					if !strings.HasPrefix(obj["id"].(string), "msg_") || obj["type"] != "message" || obj["role"] != "assistant" {
						t.Fatalf("bad Claude envelope: %v", obj)
					}
					wantStop := "end_turn"
					if tool {
						wantStop = "tool_use"
					}
					if obj["stop_reason"] != wantStop {
						t.Fatalf("stop_reason=%v want=%s", obj["stop_reason"], wantStop)
					}
					for _, key := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
						if number, ok := usage[key].(float64); !ok || number < 0 || number != float64(int(number)) {
							t.Fatalf("invalid integral usage %s=%v", key, usage[key])
						}
					}
					cache := usage["cache_creation"].(map[string]interface{})
					if usage["cache_creation_input_tokens"] != cache["ephemeral_5m_input_tokens"].(float64)+cache["ephemeral_1h_input_tokens"].(float64) {
						t.Fatalf("cache TTL split not conserved: %v", usage)
					}
					if tool {
						blocks := obj["content"].([]interface{})
						last := blocks[len(blocks)-1].(map[string]interface{})
						if last["type"] != "tool_use" || last["name"] != "lookup" || last["input"].(map[string]interface{})["city"] != "Paris" {
							t.Fatalf("bad tool block: %v", last)
						}
					}
				} else {
					if !strings.HasPrefix(obj["id"].(string), "chatcmpl-") || obj["object"] != "chat.completion" || obj["created"].(float64) <= 0 {
						t.Fatalf("bad OpenAI envelope: %v", obj)
					}
					if usage["total_tokens"] != usage["prompt_tokens"].(float64)+usage["completion_tokens"].(float64) {
						t.Fatalf("OpenAI usage not conserved: %v", usage)
					}
					cached := usage["prompt_tokens_details"].(map[string]interface{})["cached_tokens"].(float64)
					if cached < 0 || cached > usage["prompt_tokens"].(float64) {
						t.Fatalf("bad cached prompt split: %v", usage)
					}
					choice := obj["choices"].([]interface{})[0].(map[string]interface{})
					message := choice["message"].(map[string]interface{})
					if choice["index"] != float64(0) || message["role"] != "assistant" {
						t.Fatalf("bad choice: %v", choice)
					}
					if tool {
						if choice["finish_reason"] != "tool_calls" {
							t.Fatalf("tool finish_reason=%v", choice["finish_reason"])
						}
						call := message["tool_calls"].([]interface{})[0].(map[string]interface{})
						function := call["function"].(map[string]interface{})
						var input map[string]interface{}
						if call["type"] != "function" || function["name"] != "lookup" || json.Unmarshal([]byte(function["arguments"].(string)), &input) != nil || input["city"] != "Paris" {
							t.Fatalf("bad function call: %v", call)
						}
					} else if choice["finish_reason"] != "stop" {
						t.Fatalf("text finish_reason=%v", choice["finish_reason"])
					}
				}
			})
		}
	}
}

func TestNonStreamContractPureErrorsDoNotBecomeSuccessOrQuota(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, _ *KiroStreamCallback) error {
				return &KiroHTTPError{StatusCode: http.StatusBadRequest, Endpoint: "local-mock", Body: `{"reason":"REQUEST_BODY_INVALID","message":"invalid request input"}`}
			})
			attachQuotaTestLedger(t, h)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":256,"stream":false,"messages":[{"role":"user","content":"Test invalid request handling."}]}`)))
			obj := nonStreamContractObject(t, rec)
			if rec.Code != http.StatusBadRequest || obj["error"].(map[string]interface{})["type"] != "invalid_request_error" {
				t.Fatalf("HTTP=%d body=%s", rec.Code, rec.Body.String())
			}
			if _, exists := obj["usage"]; exists {
				t.Fatal("pure error response includes success usage")
			}
			usd, unknown, incomplete := quotaLedgerTotal(h)
			if usd != 0 || unknown != 0 || incomplete != 0 {
				t.Fatalf("pure error polluted quota: USD=%v unknown=%d incomplete=%d", usd, unknown, incomplete)
			}
		})
	}
}
