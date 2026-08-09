package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func nativeHistoryClaudeRequest(toolUses []interface{}, toolResults []interface{}, tools []ClaudeTool) *ClaudeRequest {
	return &ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "inspect the repository"},
			{Role: "assistant", Content: append([]interface{}{
				map[string]interface{}{"type": "redacted_thinking", "data": "opaque-active-reasoning"},
			}, toolUses...)},
			{Role: "user", Content: toolResults},
		},
		Tools: tools,
	}
}

func nativeHistoryToolUse(id, name string) interface{} {
	return map[string]interface{}{
		"type":  "tool_use",
		"id":    id,
		"name":  name,
		"input": map[string]interface{}{"path": "proxy/translator.go"},
	}
}

func nativeHistoryToolResult(id, output string) interface{} {
	return map[string]interface{}{
		"type":        "tool_result",
		"tool_use_id": id,
		"content":     output,
	}
}

func TestKiroNativeStructuredHistoryDefaultsOn(t *testing.T) {
	t.Setenv("KIRO_NATIVE_STRUCTURED_HISTORY", "")
	if got := configuredKiroHistoryMode(); got != kiroHistoryModeNativeActive {
		t.Fatalf("history mode=%q, want native_active", got)
	}
	req := nativeHistoryClaudeRequest(
		[]interface{}{nativeHistoryToolUse("toolu_1", "read_file")},
		[]interface{}{nativeHistoryToolResult("toolu_1", "package proxy")},
		[]ClaudeTool{{Name: "read_file", InputSchema: map[string]interface{}{"type": "object"}}},
	)
	payload := ClaudeToKiro(req, false)
	if payload.HistoryMode != kiroHistoryModeNativeActive {
		t.Fatalf("payload history mode=%q, want native_active", payload.HistoryMode)
	}
	active := payload.ConversationState.History[len(payload.ConversationState.History)-1].AssistantResponseMessage
	if active == nil || active.MessageID == "" || active.ReasoningContent == nil {
		t.Fatalf("default native fields are missing: %+v", active)
	}
}

func TestKiroNativeStructuredHistoryCanBeExplicitlyDisabled(t *testing.T) {
	for _, value := range []string{"0", "false", "no", "off", "legacy"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("KIRO_NATIVE_STRUCTURED_HISTORY", value)
			if got := configuredKiroHistoryMode(); got != kiroHistoryModeLegacy {
				t.Fatalf("history mode=%q, want legacy", got)
			}
			payload := ClaudeToKiro(nativeHistoryClaudeRequest(
				[]interface{}{nativeHistoryToolUse("toolu_1", "read_file")},
				[]interface{}{nativeHistoryToolResult("toolu_1", "package proxy")},
				[]ClaudeTool{{Name: "read_file", InputSchema: map[string]interface{}{"type": "object"}}},
			), false)
			if payload.HistoryMode != kiroHistoryModeLegacy {
				t.Fatalf("payload history mode=%q, want legacy", payload.HistoryMode)
			}
			for _, message := range payload.ConversationState.History {
				if assistant := message.AssistantResponseMessage; assistant != nil &&
					(assistant.MessageID != "" || assistant.ReasoningContent != nil) {
					t.Fatalf("native fields leaked while disabled: %+v", assistant)
				}
			}
		})
	}
}

func TestKiroNativeHistoryValidationErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "improperly formed request",
			err:  &KiroHTTPError{StatusCode: http.StatusBadRequest, Body: `{"message":"Improperly formed request","reason":"REQUEST_BODY_INVALID"}`},
			want: true,
		},
		{
			name: "tool result field validation",
			err:  &KiroHTTPError{StatusCode: http.StatusUnprocessableEntity, Body: `conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults is invalid`},
			want: true,
		},
		{
			name: "input too long is not a history shape error",
			err:  &KiroHTTPError{StatusCode: http.StatusBadRequest, Body: `{"message":"Input is too long","reason":"REQUEST_BODY_INVALID"}`},
		},
		{
			name: "unrelated additional model fields error",
			err:  &KiroHTTPError{StatusCode: http.StatusBadRequest, Body: `additionalModelRequestFields is not supported for this model`},
		},
		{
			name: "server failure",
			err:  &KiroHTTPError{StatusCode: http.StatusInternalServerError, Body: `Improperly formed request`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isKiroNativeHistoryValidationError(test.err); got != test.want {
				t.Fatalf("classified=%v, want %v for %v", got, test.want, test.err)
			}
		})
	}
}

func TestCallKiroAPIRetriesNativeHistoryValidationOnceAsLegacy(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Setenv("KIRO_NATIVE_STRUCTURED_HISTORY", "")
	payload := ClaudeToKiro(nativeHistoryClaudeRequest(
		[]interface{}{nativeHistoryToolUse("toolu_fallback", "read_file")},
		[]interface{}{nativeHistoryToolResult("toolu_fallback", "fallback-output-sentinel")},
		[]ClaudeTool{{Name: "read_file", InputSchema: map[string]interface{}{"type": "object"}}},
	), false)
	if payload.HistoryMode != kiroHistoryModeNativeActive {
		t.Fatalf("fixture history mode=%q, want native_active", payload.HistoryMode)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var got KiroPayload
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request %d: %v", requests, err)
		}
		context := got.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
		if requests == 1 {
			if context == nil || len(context.ToolResults) != 1 {
				t.Fatalf("first request lost native tool result: %+v", context)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Improperly formed request","reason":"REQUEST_BODY_INVALID"}`))
			return
		}
		if context != nil && len(context.ToolResults) != 0 {
			t.Fatalf("legacy retry retained structured tool results: %+v", context.ToolResults)
		}
		if !strings.Contains(got.ConversationState.CurrentMessage.UserInputMessage.Content, "fallback-output-sentinel") {
			t.Fatalf("legacy retry lost tool output: %q", got.ConversationState.CurrentMessage.UserInputMessage.Content)
		}
		for i, message := range got.ConversationState.History {
			if assistant := message.AssistantResponseMessage; assistant != nil &&
				(len(assistant.ToolUses) != 0 || assistant.MessageID != "" || assistant.ReasoningContent != nil) {
				t.Fatalf("legacy retry history[%d] retained native fields: %+v", i, assistant)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "fallback accepted"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	err := CallKiroAPI(&config.Account{
		ID:          "native-history-fallback",
		AccessToken: "token",
		ProfileArn:  "arn:aws:codewhisperer:profile/native-history-fallback",
	}, payload, nil)
	if err != nil {
		t.Fatalf("CallKiroAPI: %v", err)
	}
	if requests != 2 {
		t.Fatalf("upstream requests=%d, want one native attempt and one legacy retry", requests)
	}
	if payload.HistoryMode != kiroHistoryModeLegacy {
		t.Fatalf("payload history mode=%q after fallback, want legacy", payload.HistoryMode)
	}
}

func TestKiroNativeStructuredHistoryKeepsOnlyCompleteActiveRound(t *testing.T) {
	t.Setenv("KIRO_NATIVE_STRUCTURED_HISTORY", "true")
	req := nativeHistoryClaudeRequest(
		[]interface{}{nativeHistoryToolUse("toolu_1", "read_file")},
		[]interface{}{nativeHistoryToolResult("toolu_1", "package proxy")},
		[]ClaudeTool{{Name: "read_file", InputSchema: map[string]interface{}{"type": "object"}}},
	)
	first := ClaudeToKiro(req, false)
	second := ClaudeToKiro(req, false)
	if first.HistoryMode != kiroHistoryModeNativeActive {
		t.Fatalf("history mode=%q, want native_active", first.HistoryMode)
	}
	if len(first.ConversationState.History) == 0 {
		t.Fatal("active assistant history is missing")
	}
	active := first.ConversationState.History[len(first.ConversationState.History)-1].AssistantResponseMessage
	activeAgain := second.ConversationState.History[len(second.ConversationState.History)-1].AssistantResponseMessage
	if active == nil || active.MessageID == "" || activeAgain == nil || activeAgain.MessageID != active.MessageID {
		t.Fatalf("message ID is not stable: first=%+v second=%+v", active, activeAgain)
	}
	if active.ReasoningContent == nil || active.ReasoningContent.RedactedContent != "opaque-active-reasoning" {
		t.Fatalf("active redacted reasoning was not preserved: %+v", active.ReasoningContent)
	}
	context := first.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.ToolResults) != 1 || len(context.Tools) != 1 || len(active.ToolUses) != 1 {
		t.Fatalf("active tool round was not preserved: assistant=%+v context=%+v", active, context)
	}
	if _, err := json.Marshal(first); err != nil {
		t.Fatalf("native payload does not marshal: %v", err)
	}
}

func TestKiroNativeStructuredHistoryPreservesEveryCompletedToolRound(t *testing.T) {
	t.Setenv("KIRO_NATIVE_STRUCTURED_HISTORY", "true")
	req := &ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "inspect the repository"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "redacted_thinking", "data": "opaque-old-reasoning"},
				nativeHistoryToolUse("toolu_read", "read_file"),
			}},
			{Role: "user", Content: []interface{}{nativeHistoryToolResult("toolu_read", "package proxy")}},
			{Role: "assistant", Content: "I need the test file next."},
			{Role: "user", Content: "inspect the tests"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "redacted_thinking", "data": "opaque-active-reasoning"},
				nativeHistoryToolUse("toolu_search", "search_files"),
			}},
			{Role: "user", Content: []interface{}{nativeHistoryToolResult("toolu_search", "test match")}},
		},
		// Deliberately omit search_files so the native history path must add a
		// broad placeholder schema for a tool that occurred in history.
		Tools: []ClaudeTool{{Name: "read_file", InputSchema: map[string]interface{}{"type": "object"}}},
	}

	payload := ClaudeToKiro(req, false)
	if payload.HistoryMode != kiroHistoryModeNativeActive {
		t.Fatalf("history mode=%q, want native", payload.HistoryMode)
	}
	if payload.ConversationState.AgentTaskType != "vibe" {
		t.Fatalf("agentTaskType=%q, want vibe", payload.ConversationState.AgentTaskType)
	}
	if payload.ConversationState.CurrentMessage.UserInputMessage.Content != "" {
		t.Fatalf("tool-result continuation content=%q, want empty", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	}

	toolRounds := 0
	resultRounds := 0
	for i, message := range payload.ConversationState.History {
		if assistant := message.AssistantResponseMessage; assistant != nil {
			if assistant.MessageID == "" {
				t.Fatalf("assistant history[%d] has no stable messageId", i)
			}
			if len(assistant.ToolUses) > 0 {
				toolRounds++
				if assistant.ToolUses[0].ToolUseID == "toolu_read" && assistant.ReasoningContent != nil {
					t.Fatalf("old reasoning was replayed: %+v", assistant.ReasoningContent)
				}
				if assistant.ToolUses[0].ToolUseID == "toolu_search" &&
					(assistant.ReasoningContent == nil || assistant.ReasoningContent.RedactedContent != "opaque-active-reasoning") {
					t.Fatalf("active reasoning was not replayed: %+v", assistant.ReasoningContent)
				}
			}
		}
		if user := message.UserInputMessage; user != nil && user.UserInputMessageContext != nil &&
			len(user.UserInputMessageContext.ToolResults) > 0 {
			resultRounds++
		}
	}
	if toolRounds != 2 || resultRounds != 1 {
		// The second result is currentMessage; only the first result belongs to history.
		t.Fatalf("structured rounds in history: tool=%d result=%d", toolRounds, resultRounds)
	}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.ToolResults) != 1 || len(context.Tools) != 2 {
		t.Fatalf("current native context=%+v, want one result and two tool definitions", context)
	}
	toolNames := map[string]bool{}
	for _, tool := range context.Tools {
		toolNames[tool.ToolSpecification.Name] = true
	}
	if !toolNames["readFile"] || !toolNames["searchFiles"] {
		t.Fatalf("historical tool definitions=%v", toolNames)
	}
}

func TestNormalizeNativeKiroHistoryDoesNotMergeOrdinaryUserTurns(t *testing.T) {
	history := []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "first user turn"}},
		{UserInputMessage: &KiroUserInputMessage{Content: "second user turn"}},
	}

	normalized := normalizeNativeKiroHistory(history)
	if len(normalized) != 2 || normalized[0].UserInputMessage == nil || normalized[1].UserInputMessage == nil {
		t.Fatalf("ordinary user turns were merged: %+v", normalized)
	}
	if normalized[0].UserInputMessage.Content != "first user turn" ||
		normalized[1].UserInputMessage.Content != "second user turn" {
		t.Fatalf("ordinary user content changed: %+v", normalized)
	}

	toolResultTurn := []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{
			UserInputMessageContext: &UserInputMessageContext{
				ToolResults: []KiroToolResult{{ToolUseID: "toolu_1"}},
			},
		}},
		{UserInputMessage: &KiroUserInputMessage{Content: "continue after the tool"}},
	}
	coalesced := normalizeNativeKiroHistory(toolResultTurn)
	if len(coalesced) != 1 || coalesced[0].UserInputMessage == nil ||
		coalesced[0].UserInputMessage.Content != "continue after the tool" ||
		coalesced[0].UserInputMessage.UserInputMessageContext == nil ||
		len(coalesced[0].UserInputMessage.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("tool-result continuation was not coalesced: %+v", coalesced)
	}
}

func TestKiroNativeStructuredHistoryFallsBackOnPartialRound(t *testing.T) {
	t.Setenv("KIRO_NATIVE_STRUCTURED_HISTORY", "on")
	req := nativeHistoryClaudeRequest(
		[]interface{}{
			nativeHistoryToolUse("toolu_1", "read_file"),
			nativeHistoryToolUse("toolu_2", "list_files"),
		},
		[]interface{}{nativeHistoryToolResult("toolu_1", "partial-result-sentinel")},
		[]ClaudeTool{
			{Name: "read_file", InputSchema: map[string]interface{}{"type": "object"}},
			{Name: "list_files", InputSchema: map[string]interface{}{"type": "object"}},
		},
	)
	payload := ClaudeToKiro(req, false)
	if payload.HistoryMode != kiroHistoryModeLegacy {
		t.Fatalf("partial round stayed native: %q", payload.HistoryMode)
	}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context != nil && len(context.ToolResults) != 0 {
		t.Fatalf("partial results stayed structured: %+v", context.ToolResults)
	}
	if !strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "partial-result-sentinel") {
		t.Fatalf("fallback lost tool output: %q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
	for _, cause := range []string{"toolu_1", "toolu_2", "read_file", "list_files", "proxy/translator.go", "invalid_or_partial_active_round"} {
		if !strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, cause) {
			t.Fatalf("fallback lost tool cause %q: %q", cause, payload.ConversationState.CurrentMessage.UserInputMessage.Content)
		}
	}
	for _, message := range payload.ConversationState.History {
		if assistant := message.AssistantResponseMessage; assistant != nil && assistant.ReasoningContent != nil {
			t.Fatalf("fallback retained native reasoning: %+v", assistant.ReasoningContent)
		}
	}
}

func TestKiroNativeStructuredHistoryInfersPlaceholderToolSchema(t *testing.T) {
	t.Setenv("KIRO_NATIVE_STRUCTURED_HISTORY", "1")
	req := nativeHistoryClaudeRequest(
		[]interface{}{nativeHistoryToolUse("toolu_1", "mcp__workspace__read_file")},
		[]interface{}{nativeHistoryToolResult("toolu_1", "ok")},
		nil,
	)
	payload := ClaudeToKiro(req, false)
	if payload.HistoryMode != kiroHistoryModeNativeActive {
		t.Fatalf("inferred active round fell back: %q", payload.HistoryMode)
	}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.Tools) != 1 {
		t.Fatalf("placeholder schema missing: %+v", context)
	}
	schema, ok := context.Tools[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if !ok || schema["type"] != "object" {
		t.Fatalf("placeholder schema=%#v", context.Tools[0].ToolSpecification.InputSchema.JSON)
	}
	if _, hasRequired := schema["required"]; hasRequired {
		t.Fatalf("placeholder schema inferred required fields: %#v", schema)
	}
	active := payload.ConversationState.History[len(payload.ConversationState.History)-1].AssistantResponseMessage
	if active == nil || active.ToolUses[0].Name != context.Tools[0].ToolSpecification.Name {
		t.Fatalf("inferred tool names diverged: assistant=%+v tool=%+v", active, context.Tools[0])
	}
}

func TestKiroNativeStructuredHistoryDoesNotReplayPlaintextThinking(t *testing.T) {
	t.Setenv("KIRO_NATIVE_STRUCTURED_HISTORY", "true")
	req := &ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "inspect"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "thinking", "thinking": "decision-cause", "signature": "foreign-signature"},
				nativeHistoryToolUse("toolu_1", "read_file"),
			}},
			{Role: "user", Content: []interface{}{nativeHistoryToolResult("toolu_1", "ok")}},
		},
		Tools: []ClaudeTool{{Name: "read_file", InputSchema: map[string]interface{}{"type": "object"}}},
	}
	payload := ClaudeToKiro(req, false)
	active := payload.ConversationState.History[len(payload.ConversationState.History)-1].AssistantResponseMessage
	if active == nil || active.ReasoningContent != nil || !strings.Contains(active.Content, "decision-cause") {
		t.Fatalf("plaintext thinking was lost or replayed natively: %+v", active)
	}
}
