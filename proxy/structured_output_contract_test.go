package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-go/config"
)

func structuredContractRequest(t *testing.T, stream bool, schema, field string) string {
	t.Helper()
	body := map[string]interface{}{
		"model": "claude-sonnet-4-6", "max_tokens": 2048, "stream": stream,
		"messages": []map[string]interface{}{{"role": "user", "content": "Answer the original question."}},
	}
	format := map[string]interface{}{"type": "json_schema", "schema": json.RawMessage(schema)}
	if field == "output_format" {
		body[field] = format
	} else {
		body["output_config"] = map[string]interface{}{"format": format}
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func structuredContractResponse(t *testing.T, rec *httptest.ResponseRecorder, stream bool) (text, stop string) {
	t.Helper()
	if !stream {
		var response ClaudeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		for _, block := range response.Content {
			if block.Type == "text" {
				text += block.Text
			}
		}
		return text, response.StopReason
	}
	events := decodeResponseContractSSE(t, rec.Body.String())
	if len(events) == 0 || events[0].Name != "message_start" || events[len(events)-1].Name != "message_stop" {
		t.Fatalf("invalid SSE lifecycle: %s", rec.Body.String())
	}
	for _, event := range events {
		if event.Name == "content_block_delta" {
			delta, _ := event.Data["delta"].(map[string]interface{})
			if delta["type"] == "text_delta" {
				text += delta["text"].(string)
			}
		}
		if event.Name == "message_delta" {
			delta, _ := event.Data["delta"].(map[string]interface{})
			stop, _ = delta["stop_reason"].(string)
		}
	}
	return text, stop
}

func TestClaudeStructuredOutputLegacyFormatAlias(t *testing.T) {
	for _, body := range []string{
		`{"output_format":{"type":"json_schema","schema":{"type":"integer"}}}`,
		`{"output_config":{"effort":"high"},"output_format":{"type":"json_schema","schema":{"type":"integer"}}}`,
		`{"output_config":{"effort":"high","format":null},"output_format":{"type":"json_schema","schema":{"type":"integer"}}}`,
	} {
		var request ClaudeRequest
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		if request.OutputConfig == nil || request.OutputConfig.Format == nil || string(request.OutputConfig.Format.Schema) != `{"type":"integer"}` {
			t.Fatalf("legacy alias was ignored: %#v", request.OutputConfig)
		}
		if strings.Contains(body, "effort") && request.OutputConfig.Effort != "high" {
			t.Fatalf("legacy format discarded effort: %#v", request.OutputConfig)
		}
	}
	var request ClaudeRequest
	if err := json.Unmarshal([]byte(`{"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"integer"}}},"output_format":{"type":"yaml","schema":[]}}`), &request); err != nil {
		t.Fatal(err)
	}
	if request.OutputConfig.Format.Type != "json_schema" || request.OutputConfig.Effort != "high" {
		t.Fatalf("canonical format must win: %#v", request.OutputConfig)
	}
	if err := json.Unmarshal([]byte(`{"output_config":{"format":{"type":"json_schema","schema":{"type":"integer"}}},"output_format":"ignored-legacy-value"}`), &request); err != nil || request.OutputConfig.Format.Type != "json_schema" {
		t.Fatalf("canonical format must also supersede an obsolete alias value: format=%#v err=%v", request.OutputConfig, err)
	}
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		h := &Handler{}
		rec := httptest.NewRecorder()
		body := `{"model":"claude-sonnet-4-6","max_tokens":2048,"messages":[{"role":"user","content":"answer"}],"output_format":{"type":"json_schema","schema":[]}}`
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if strings.HasSuffix(path, "count_tokens") {
			h.handleCountTokens(rec, req)
		} else {
			h.handleClaudeMessagesInternal(rec, req)
		}
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "format.schema") {
			t.Fatalf("%s ignored invalid alias: status=%d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestClaudeStructuredOutputValidTopLevelValues(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, test := range []struct{ name, schema, content, want string }{
			{"object", `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`, "Here is the result:\n```json\n{\"answer\":42}\n```", `{"answer":42}`},
			{"array", `{"type":"array","items":{"type":"integer"}}`, "```json\n[1,2,3]\n```", `[1,2,3]`},
			{"string", `{"type":"string","enum":["a { literal"]}`, `"a { literal"`, `"a { literal"`},
			{"integer", `{"type":"integer"}`, `9007199254740993`, `9007199254740993`},
			{"boolean", `{"type":"boolean"}`, `true`, `true`},
			{"null", `{"type":"null"}`, `null`, `null`},
			{"literal thinking tags", `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`, `{"text":"literal <thinking>not reasoning</thinking> value"}`, `{"text":"literal <thinking>not reasoning</thinking> value"}`},
		} {
			t.Run(test.name+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				h := newResponseContractHandler(t)
				calls := 0
				h.kiroCaller = func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
					calls++
					if !payload.StructuredOutput || string(payload.StructuredOutputSchema) != test.schema {
						t.Fatalf("legacy schema did not reach payload: %#v", payload)
					}
					callback.OnText(test.content, false)
					callback.OnComplete(4, 7)
					return nil
				}
				rec := httptest.NewRecorder()
				body := structuredContractRequest(t, stream, test.schema, "output_format")
				h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
				if rec.Code != http.StatusOK || calls != 1 {
					t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
				}
				text, stop := structuredContractResponse(t, rec, stream)
				if text != test.want || stop != "end_turn" {
					t.Fatalf("text=%q stop=%q want=%q", text, stop, test.want)
				}
			})
		}
	}
}

func TestClaudeStructuredOutputValidatesBeforePublishingAndRetriesOnce(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			h := newResponseContractHandler(t)
			rec := httptest.NewRecorder()
			var accountIDs []string
			h.kiroCaller = func(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				accountIDs = append(accountIDs, account.ID)
				if len(accountIDs) == 1 {
					callback.OnText(`{"answer":"wrong"}`, false)
				} else {
					if !strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "<structured_output_retry>") {
						t.Fatal("missing bounded correction instruction")
					}
					callback.OnText(`{"answer":42}`, false)
				}
				if rec.Body.Len() != 0 {
					t.Fatalf("assistant data leaked before validation: %s", rec.Body.String())
				}
				callback.OnComplete(10, 8)
				return nil
			}
			body := structuredContractRequest(t, stream, `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`, "output_config")
			h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if rec.Code != http.StatusOK || len(accountIDs) != 2 || accountIDs[0] != accountIDs[1] {
				t.Fatalf("accounts=%v status=%d body=%s", accountIDs, rec.Code, rec.Body.String())
			}
			text, stop := structuredContractResponse(t, rec, stream)
			if text != `{"answer":42}` || stop != "end_turn" || strings.Contains(rec.Body.String(), "wrong") {
				t.Fatalf("discarded invalid output leaked: text=%q stop=%q body=%s", text, stop, rec.Body.String())
			}
			if got := atomic.LoadInt64(&h.totalRequests); got != 2 {
				t.Fatalf("usage lost an attempt: totalRequests=%d", got)
			}
		})
	}
}

func TestClaudeStructuredOutputPersistentMismatchReturnsError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			h := newResponseContractHandler(t)
			calls := 0
			h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				callback.OnText(`{"answer":"wrong"}`, false)
				callback.OnComplete(10, 8)
				return nil
			}
			rec := httptest.NewRecorder()
			body := structuredContractRequest(t, stream, `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`, "output_config")
			h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if calls != 2 || rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_structured_output_error") || strings.Contains(rec.Body.String(), "event: message_start") {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
			}
			if got := atomic.LoadInt64(&h.totalTokens); got <= 0 {
				t.Fatal("discarded output was not accounted")
			}
		})
	}
}

func TestClaudeStructuredOutputPreservesExceptionalStops(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, reason := range []string{"max_tokens", "model_context_window_exceeded", "refusal", "pause_turn"} {
			t.Run(reason+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				h := newResponseContractHandler(t)
				calls := 0
				const partial = `{"answer":`
				h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
					calls++
					callback.OnText(partial, false)
					callback.OnStopReason(reason)
					callback.OnComplete(10, 8)
					return nil
				}
				rec := httptest.NewRecorder()
				body := structuredContractRequest(t, stream, `{"type":"object"}`, "output_config")
				h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
				if rec.Code != http.StatusOK || calls != 1 {
					t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
				}
				text, stop := structuredContractResponse(t, rec, stream)
				if text != partial || stop != reason {
					t.Fatalf("text=%q stop=%q want stop %q", text, stop, reason)
				}
			})
		}
	}
}

func TestClaudeStructuredOutputAllowsRealToolTurn(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			h := newResponseContractHandler(t)
			calls := 0
			h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				callback.OnText("I will use the real tool.", false)
				callback.OnToolUse(KiroToolUse{ToolUseID: "call_lookup", Name: "lookup", Input: map[string]interface{}{"city": "Paris"}})
				callback.OnComplete(10, 8)
				return nil
			}
			rec := httptest.NewRecorder()
			body := structuredContractRequest(t, stream, `{"type":"object"}`, "output_config")
			body = strings.TrimSuffix(body, "}") + `,"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
			h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if rec.Code != http.StatusOK || calls != 1 || !strings.Contains(rec.Body.String(), "lookup") || strings.Contains(rec.Body.String(), "I will use") {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
			}
			_, stop := structuredContractResponse(t, rec, stream)
			if stop != "tool_use" {
				t.Fatalf("stop=%q body=%s", stop, rec.Body.String())
			}
		})
	}
}

func TestClaudeStructuredOutputCancellationDoesNotRetryOrPublish(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			h := newResponseContractHandler(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				callback.OnText(`{"answer":"wrong"}`, false)
				callback.OnComplete(10, 8)
				cancel()
				return nil
			}
			rec := httptest.NewRecorder()
			body := structuredContractRequest(t, stream, `{"type":"object"}`, "output_config")
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
			h.handleClaudeMessagesInternal(rec, req)
			if calls != 1 || rec.Body.Len() != 0 {
				t.Fatalf("calls=%d body=%s", calls, rec.Body.String())
			}
		})
	}
}

func TestClaudeStructuredOutputKeepsReasoningSeparateFromJSON(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, omit := range []bool{false, true} {
			t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream]+map[bool]string{false: "/shown", true: "/omitted"}[omit], func(t *testing.T) {
				h := newResponseContractHandler(t)
				h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
					callback.OnReasoning(KiroReasoningContentEvent{Text: "private reasoning", Signature: "native-continuation-signature"})
					callback.OnText(`{"answer":42}`, false)
					callback.OnComplete(10, 8)
					return nil
				}
				payload := emptyVisibleOutputPayload()
				payload.StructuredOutput = true
				payload.StructuredOutputType = "json_schema"
				payload.StructuredOutputSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`)
				rec := httptest.NewRecorder()
				opts := claudeThinkingResponseOptions{Format: "think", OmitDisplay: omit}
				if stream {
					h.handleClaudeStream(rec, payload, "claude-sonnet-4.6", "claude-sonnet-4.6", true, opts, 10, nil, "")
				} else {
					h.handleClaudeNonStream(rec, payload, "claude-sonnet-4.6", "claude-sonnet-4.6", true, opts, 10, nil, "")
				}
				text, stop := structuredContractResponse(t, rec, stream)
				if text != `{"answer":42}` || stop != "end_turn" || !strings.Contains(rec.Body.String(), "native-continuation-signature") || strings.Contains(rec.Body.String(), "private reasoning") == omit {
					t.Fatalf("reasoning corrupted JSON or signature: text=%q body=%s", text, rec.Body.String())
				}
			})
		}
	}
}

func TestClaudeStructuredOutputCCBillingInflatesOnce(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/cc/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			h := newResponseContractHandler(t)
			setBillingInflation(t, 50)
			h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				callback.OnText(`{"answer":42}`, false)
				callback.OnComplete(10, 8)
				return nil
			}
			rec := httptest.NewRecorder()
			body := structuredContractRequest(t, true, `{"type":"object"}`, "output_format")
			h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			want := inflateBillingTokensWithPercent(estimateClaudeOutputTokensWithRedacted(`{"answer":42}`, "", nil, nil), 50)
			found := false
			for _, event := range decodeResponseContractSSE(t, rec.Body.String()) {
				if event.Name == "message_delta" {
					found = true
					usage, _ := event.Data["usage"].(map[string]interface{})
					if int(usage["output_tokens"].(float64)) != want {
						t.Fatalf("output_tokens=%v want=%d body=%s", usage["output_tokens"], want, rec.Body.String())
					}
				}
			}
			if !found {
				t.Fatal("missing terminal usage")
			}
		})
	}
}

func TestClaudeStructuredOutputInvalidSchemaRejectedBeforeGeneration(t *testing.T) {
	h := newResponseContractHandler(t)
	calls := 0
	h.kiroCaller = func(_ *config.Account, _ *KiroPayload, _ *KiroStreamCallback) error {
		calls++
		return nil
	}
	for _, schema := range []string{`{"type":"not-a-json-schema-type"}`, `{"$ref":"https://invalid.example/schema"}`, `{"required":true}`} {
		rec := httptest.NewRecorder()
		body := structuredContractRequest(t, false, schema, "output_format")
		h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || calls != 0 || !strings.Contains(rec.Body.String(), "format.schema") {
			t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
		}
	}
}
