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

func TestStructuredOutputIntegrityRejectsInvalidOuterAndMultipleValues(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`)
	for _, text := range []string{
		`{"broken":{"answer":7},"unfinished":}`,
		`{"broken":{"answer":7}`,
		`[{"answer":7}, broken]`,
		`{"answer":7} {"answer":8}`,
		"```json\n{\"answer\":7}\n```\n```json\n{\"answer\":8}\n```",
		`{"answer":7} true`,
		`{"answer":7} "unterminated`,
		`{"answer":7} 1e`,
		`{"answer":7} tru`,
		`{"answer":7} fals`,
		`{"answer":7} nul`,
		`1e {"answer":7}`,
		`- {"answer":7}`,
		`tru {"answer":7}`,
		"{\"answer\":7}\n```json\ntrue",
	} {
		t.Run(text, func(t *testing.T) {
			got := normalizeStructuredOutputContent(text)
			if err := validateStructuredOutputContent(got, "json_schema", schema); err == nil {
				t.Fatalf("invalid whole answer became schema-valid: original=%q normalized=%q", text, got)
			}
		})
	}
}

func TestStructuredOutputIntegrityPreservesOneExactJSONValue(t *testing.T) {
	for _, test := range []struct{ text, want string }{
		{`9007199254740993`, `9007199254740993`},
		{`true`, `true`},
		{`null`, `null`},
		{`"escaped \" and { [] }"`, `"escaped \" and { [] }"`},
		{`{"answer":{"nested":[1,2]},"literal":"{} []"}`, `{"answer":{"nested":[1,2]},"literal":"{} []"}`},
		{"Here is the result:\n```json\n{\"answer\":9007199254740993}\n```", `{"answer":9007199254740993}`},
		{"Result follows:\n{\"answer\":42}\nEverything is complete.", `{"answer":42}`},
		{`prefix [1,2,3] trailing prose`, `[1,2,3]`},
		{`{"text":"literal <thinking>not reasoning</thinking> {} []"}`, `{"text":"literal <thinking>not reasoning</thinking> {} []"}`},
	} {
		if got := normalizeStructuredOutputContent(test.text); got != test.want {
			t.Fatalf("JSON data changed: got=%q want=%q", got, test.want)
		}
	}
}

func TestStructuredOutputIntegrityRejectsInvalidSchemaAtHTTPBeforeGeneration(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(route+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				h := newResponseContractHandler(t)
				calls := 0
				h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
					calls++
					callback.OnText(`{"answer":42}`, false)
					callback.OnComplete(10, 8)
					return nil
				}
				body := map[string]interface{}{"model": "claude-sonnet-4-6", "stream": stream}
				format := map[string]interface{}{"name": "result", "schema": map[string]interface{}{"type": "not-a-json-type"}}
				field := "json_schema.schema"
				if route == "/v1/chat/completions" {
					body["messages"] = []map[string]string{{"role": "user", "content": "Return the answer."}}
					body["response_format"] = map[string]interface{}{"type": "json_schema", "json_schema": format}
				} else {
					body["input"] = "Return the answer."
					format["type"] = "json_schema"
					body["text"] = map[string]interface{}{"format": format}
					field = "format.schema"
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, route, strings.NewReader(string(encoded)))
				if route == "/v1/chat/completions" {
					h.handleOpenAIChat(rec, request)
				} else {
					h.handleOpenAIResponses(rec, request)
				}
				if rec.Code != http.StatusBadRequest || calls != 0 || !strings.Contains(rec.Body.String(), field) {
					t.Fatalf("schema validation did not reject before upstream: calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestStructuredOutputIntegrityRetriesInvalidWholeAnswerBeforePublishing(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, invalid := range []string{`{"broken":{"answer":7},"unfinished":}`, `{"answer":7} {"answer":8}`, `{"answer":7} "unterminated`, `{"answer":7} 1e`} {
			t.Run(map[bool]string{false: "nonstream/", true: "stream/"}[stream]+invalid, func(t *testing.T) {
				h := newResponseContractHandler(t)
				calls := 0
				rec := httptest.NewRecorder()
				h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
					calls++
					if calls == 1 {
						callback.OnText(invalid, false)
					} else {
						callback.OnText(`{"answer":42}`, false)
					}
					if rec.Body.Len() != 0 {
						t.Fatal("invalid draft published before format validation")
					}
					callback.OnComplete(10, 8)
					return nil
				}
				body := structuredContractRequest(t, stream, `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`, "output_config")
				h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
				if calls != 2 || rec.Code != http.StatusOK {
					t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
				}
				text, stop := structuredContractResponse(t, rec, stream)
				if text != `{"answer":42}` || stop != "end_turn" {
					t.Fatalf("text=%q stop=%q", text, stop)
				}
				if atomic.LoadInt64(&h.totalRequests) != 2 {
					t.Fatal("discarded invalid draft usage was lost")
				}
			})
		}
	}
}

func TestStructuredOutputIntegrityPersistentInvalidWholeAnswerReturnsError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			h := newResponseContractHandler(t)
			calls := 0
			h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				callback.OnText(`{"broken":{"answer":7},"unfinished":}`, false)
				callback.OnComplete(10, 8)
				return nil
			}
			rec := httptest.NewRecorder()
			body := structuredContractRequest(t, stream, `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`, "output_config")
			h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if calls != 2 || rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_structured_output_error") {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "event: message_start") {
				t.Fatal("invalid schema answer published as a successful SSE message")
			}
		})
	}
}

func TestStructuredOutputIntegrityRejectsInvalidSchemasAcrossInputFormats(t *testing.T) {
	for _, schema := range []string{`{"type":"not-a-json-type"}`, `{"required":true}`, `{"$ref":"https://invalid.example/schema"}`, `{"$ref":"#/$defs/missing"}`} {
		t.Run(schema, func(t *testing.T) {
			format := &OpenAIResponseFormat{Type: "json_schema", JSONSchema: &OpenAIJSONSchema{Name: "result", Schema: json.RawMessage(schema)}}
			if msg := validateOpenAIResponseFormat(format); !strings.Contains(msg, "json_schema.schema") {
				t.Fatalf("invalid Chat Completions schema accepted: %q", msg)
			}
			text := &ResponsesTextConfig{Format: &ResponsesTextFormat{Type: "json_schema", Name: "result", Schema: json.RawMessage(schema)}}
			if msg := validateResponsesTextConfig(text); !strings.Contains(msg, "format.schema") {
				t.Fatalf("invalid Responses schema accepted: %q", msg)
			}
		})
	}
}
