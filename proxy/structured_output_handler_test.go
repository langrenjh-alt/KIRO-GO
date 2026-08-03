package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestClaudeStructuredOutputNormalizesNonStreamResponse(t *testing.T) {
	h := newResponseContractHandler(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "Here is the result:\n```json\n{\"answer\":42}\n```",
	}))
	body := `{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"answer"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"integer"}}}}}}`
	rec := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "text" || response.Content[0].Text != `{"answer":42}` {
		t.Fatalf("structured content=%#v", response.Content)
	}
}

func TestClaudeStructuredOutputNormalizesStreamResponse(t *testing.T) {
	h := newResponseContractHandler(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "Here is the result:\n```json\n{\"answer\":42}\n```",
	}))
	body := `{"model":"claude-sonnet-4-6","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"answer"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"integer"}}}}}}`
	rec := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var text string
	for _, event := range decodeResponseContractSSE(t, rec.Body.String()) {
		if event.Name != "content_block_delta" {
			continue
		}
		delta, _ := event.Data["delta"].(map[string]interface{})
		if delta["type"] == "text_delta" {
			text += delta["text"].(string)
		}
	}
	if text != `{"answer":42}` {
		t.Fatalf("stream structured text=%q", text)
	}
}

func TestClaudeStructuredOutputStreamsJSONIncrementally(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		for _, chunk := range []string{
			"Here is the result:\n```json\n{\"ans",
			"wer\":42,\"nested\":{\"ok\":",
			"true}}\n```\ntrailing prose",
		} {
			callback.OnText(chunk, false)
		}
		callback.OnComplete(10, 8)
		return nil
	})
	payload := emptyVisibleOutputPayload()
	payload.StructuredOutput = true
	payload.StructuredOutputType = "json_schema"
	payload.StructuredOutputSchema = json.RawMessage(`{
		"type":"object",
		"properties":{"answer":{"type":"integer"},"nested":{"type":"object"}},
		"required":["answer","nested"]
	}`)
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if calls != 1 || rec.Code != http.StatusOK {
		t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
	}
	var text string
	var textDeltas int
	for _, event := range decodeResponseContractSSE(t, rec.Body.String()) {
		if event.Name != "content_block_delta" {
			continue
		}
		delta, _ := event.Data["delta"].(map[string]interface{})
		if delta["type"] == "text_delta" {
			text += delta["text"].(string)
			textDeltas++
		}
	}
	if text != `{"answer":42,"nested":{"ok":true}}` || textDeltas != 3 {
		t.Fatalf("text=%q deltas=%d\n%s", text, textDeltas, rec.Body.String())
	}
}

func TestClaudeStructuredOutputNonStreamRetriesSchemaMismatchOnce(t *testing.T) {
	var calls int
	var accountIDs []string
	h := newEmptyVisibleOutputTestHandler(t, 1, func(account *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		accountIDs = append(accountIDs, account.ID)
		if calls == 1 {
			callback.OnText(`{"answer":"wrong type"}`, false)
		} else {
			callback.OnText(`{"answer":42}`, false)
		}
		callback.OnComplete(10, 8)
		return nil
	})
	payload := emptyVisibleOutputPayload()
	payload.StructuredOutput = true
	payload.StructuredOutputType = "json_schema"
	payload.StructuredOutputSchema = json.RawMessage(`{
		"type":"object",
		"properties":{"answer":{"type":"integer"}},
		"required":["answer"],
		"additionalProperties":false
	}`)
	rec := httptest.NewRecorder()

	h.handleClaudeNonStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if calls != 2 || len(accountIDs) != 2 || accountIDs[0] != accountIDs[1] {
		t.Fatalf("calls=%d accounts=%v, want one same-account retry", calls, accountIDs)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].Text != `{"answer":42}` {
		t.Fatalf("content=%#v", response.Content)
	}
}

func TestClaudeNonStreamToolUseSuppressesNarration(t *testing.T) {
	h := newResponseContractHandler(t,
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "I will call the tool now."}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "call_1", "name": "lookup", "input": `{"key":"value"}`, "stop": true,
		}),
	)
	body := `{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"lookup"}],"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object"}}]}`
	rec := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "tool_use" || response.StopReason != "tool_use" {
		t.Fatalf("tool response=%#v stop_reason=%q", response.Content, response.StopReason)
	}
}

func TestClaudeStreamDeclaredToolDirectAnswerClosesLateTextBlock(t *testing.T) {
	h := newResponseContractHandler(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "PONG",
	}))
	body := `{"model":"claude-sonnet-4-6","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Reply with PONG without using a tool."}],"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"}}`
	rec := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	events := decodeResponseContractSSE(t, rec.Body.String())
	got := make([]string, 0, len(events))
	for _, event := range events {
		got = append(got, event.Name)
	}
	want := []string{
		"message_start",
		"content_block_start",
		"ping",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(got) != len(want) {
		t.Fatalf("event order=%v, want %v\n%s", got, want, rec.Body.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event order=%v, want %v\n%s", got, want, rec.Body.String())
		}
	}
	delta := events[len(events)-2].Data
	contextManagement, _ := delta["context_management"].(map[string]interface{})
	if contextManagement == nil {
		t.Fatalf("message_delta context_management missing: %#v", delta)
	}
	if _, ok := contextManagement["applied_edits"].([]interface{}); !ok {
		t.Fatalf("message_delta applied_edits missing: %#v", delta)
	}
}
