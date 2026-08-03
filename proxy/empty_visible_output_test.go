package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"

	"github.com/google/uuid"
)

func TestClaudeStreamRetriesMalformedToolJSONBeforeOutput(t *testing.T) {
	var calls int
	var accountIDs []string
	h := newEmptyVisibleOutputTestHandler(t, 2, func(account *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		accountIDs = append(accountIDs, account.ID)
		if calls == 1 {
			return &KiroStreamProtocolError{
				Code: "upstream_tool_json_error",
				Err:  errors.New(`incomplete tool use: unexpected EOF`),
			}
		}
		callback.OnText("recovered", false)
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 2 || len(accountIDs) != 2 || accountIDs[0] == accountIDs[1] {
		t.Fatalf("calls=%d accounts=%v, want two distinct attempts", calls, accountIDs)
	}
	if !strings.Contains(rec.Body.String(), "recovered") || strings.Contains(rec.Body.String(), "upstream_tool_json_error") {
		t.Fatalf("stream did not recover cleanly: %s", rec.Body.String())
	}
}

func TestClaudeStreamRetriesTransientPreludeErrorBeforeOutput(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls < 3 {
			return errors.New("read Kiro event stream prelude: stream error: stream ID 7; INTERNAL_ERROR; received from peer")
		}
		callback.OnText("recovered", false)
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if rec.Code != http.StatusOK || calls != 3 || !strings.Contains(rec.Body.String(), "recovered") {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
}

func TestClaudeStreamAutoToolsFlushesTextBeforeUpstreamCompletes(t *testing.T) {
	rec := httptest.NewRecorder()
	var observedDuringCall string
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnText("first token", false)
		// The upstream call has not returned yet. Seeing the delta here proves
		// that auto+tools is genuinely streamed rather than flushed at EOF.
		observedDuringCall = rec.Body.String()
		callback.OnComplete(10, 2)
		return nil
	})
	payload := emptyVisibleOutputPayload()
	payload.ToolChoiceMode = "auto"
	tool := KiroToolWrapper{}
	tool.ToolSpecification.Name = "lookup"
	tool.ToolSpecification.Description = "lookup"
	tool.ToolSpecification.InputSchema.JSON = map[string]interface{}{"type": "object"}
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		Tools: []KiroToolWrapper{tool},
	}

	h.handleClaudeStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if !strings.Contains(observedDuringCall, `"type":"text_delta"`) || !strings.Contains(observedDuringCall, "first token") {
		t.Fatalf("auto tool text was not flushed before upstream completion: %s", observedDuringCall)
	}
}

func newEmptyVisibleOutputTestHandler(t *testing.T, accountCount int, caller kiroAPICaller) *Handler {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for i := 0; i < accountCount; i++ {
		id := "empty-output-" + uuid.NewString() + "-" + string(rune('a'+i))
		if err := config.AddAccount(config.Account{
			ID:          id,
			Email:       id + "@example.test",
			Enabled:     true,
			AccessToken: "token-" + id,
			ProfileArn:  "arn:aws:codewhisperer:profile/" + id,
		}); err != nil {
			t.Fatalf("add account %s: %v", id, err)
		}
	}
	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingStats)
	return &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		kiroCaller:  caller,
	}
}

func TestClaudeNonStreamReturnsInputTooLongWithoutAccountRotation(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 2, func(_ *config.Account, _ *KiroPayload, _ *KiroStreamCallback) error {
		calls++
		return errors.New(`HTTP 400 from test: {"message":"Input is too long.","reason":"REQUEST_BODY_INVALID"}`)
	})

	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 1, nil, "")

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1 without retry or account rotation", calls)
	}
	for _, account := range config.GetAccounts() {
		if !account.Enabled {
			t.Fatalf("input-too-long error disabled account %q", account.ID)
		}
	}
}

func emptyVisibleOutputPayload() *KiroPayload {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "test",
		ModelID: "claude-opus-5",
		Origin:  "AI_EDITOR",
	}
	return payload
}

func TestClaudeStreamTreatsReasoningEventAsTextWhenThinkingIsDisabled(t *testing.T) {
	var accountIDs []string
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 2, func(account *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		accountIDs = append(accountIDs, account.ID)
		callback.OnReasoning(KiroReasoningContentEvent{Text: "visible answer"})
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 || len(accountIDs) != 1 {
		t.Fatalf("calls=%d accounts=%v, want one attempt", calls, accountIDs)
	}

	events := decodeResponseContractSSE(t, rec.Body.String())
	var visibleText string
	var outputTokens float64
	var messageStops int
	for _, event := range events {
		switch event.Name {
		case "content_block_delta":
			delta, _ := event.Data["delta"].(map[string]interface{})
			if delta["type"] == "text_delta" {
				visibleText += delta["text"].(string)
			}
		case "message_delta":
			usage, _ := event.Data["usage"].(map[string]interface{})
			outputTokens, _ = usage["output_tokens"].(float64)
		case "message_stop":
			messageStops++
		}
	}
	if visibleText != "visible answer" || outputTokens <= 0 || messageStops != 1 {
		t.Fatalf("visible=%q output_tokens=%v message_stops=%d\n%s", visibleText, outputTokens, messageStops, rec.Body.String())
	}
}

func TestClaudeStreamReturnsSuccessfulEmptyMessageWithoutRetry(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if calls != 1 {
		t.Fatalf("calls=%d, want one attempt", calls)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "empty_visible_output") {
		t.Fatalf("empty success was converted to an error: %s", rec.Body.String())
	}
	logs := h.getRequestLogs()
	if len(logs) != 1 || logs[0].Status != "success" || logs[0].Error != "" {
		t.Fatalf("request logs=%#v, want one successful empty response", logs)
	}
}
