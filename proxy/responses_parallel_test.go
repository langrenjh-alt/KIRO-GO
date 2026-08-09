package proxy

import (
	"encoding/json"
	"testing"
)

// TestResponsesParallelToolCallsMerge verifies that consecutive function_call
// items in the Responses API input are merged into a single assistant message
// so parallel tool calls stay grouped. Previously each parallel call became its
// own assistant turn, which split the tool_use/tool_result pairing and caused
// Kiro to reject the request with HTTP 400 "Improperly formed request".
func TestResponsesParallelToolCallsMerge(t *testing.T) {
	items := []json.RawMessage{
		mustRaw(`{"type":"message","role":"user","content":[{"type":"input_text","text":"run two commands"}]}`),
		mustRaw(`{"type":"function_call","call_id":"call_a","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}`),
		mustRaw(`{"type":"function_call","call_id":"call_b","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}`),
		mustRaw(`{"type":"function_call_output","call_id":"call_a","output":"file1"}`),
		mustRaw(`{"type":"function_call_output","call_id":"call_b","output":"/home"}`),
	}

	msgs, err := convertResponsesInputItems(items)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	// Find the assistant message; it must contain BOTH tool calls.
	var asstToolCalls int
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			asstToolCalls = len(m.ToolCalls)
		}
	}
	if asstToolCalls != 2 {
		t.Fatalf("expected 2 merged tool calls in one assistant message, got %d", asstToolCalls)
	}

	// Count assistant messages carrying tool calls; must be exactly 1 (merged).
	asstWithCalls := 0
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			asstWithCalls++
		}
	}
	if asstWithCalls != 1 {
		t.Fatalf("expected parallel calls merged into 1 assistant message, got %d", asstWithCalls)
	}
}

func TestResponsesParallelToolResultStatusesReachKiro(t *testing.T) {
	items := []json.RawMessage{
		mustRaw(`{"type":"message","role":"user","content":[{"type":"input_text","text":"run checks"}]}`),
		mustRaw(`{"type":"function_call","call_id":"call_ok","name":"exec_command","arguments":"{}"}`),
		mustRaw(`{"type":"function_call","call_id":"call_error","name":"exec_command","arguments":"{}"}`),
		mustRaw(`{"type":"function_call","call_id":"call_incomplete","name":"exec_command","arguments":"{}"}`),
		mustRaw(`{"type":"function_call_output","call_id":"call_ok","output":"ok","status":"completed"}`),
		mustRaw(`{"type":"function_call_output","call_id":"call_error","output":"permission denied","is_error":true}`),
		mustRaw(`{"type":"function_call_output","call_id":"call_incomplete","output":"partial","status":"incomplete"}`),
	}

	messages, err := convertResponsesInputItems(items)
	if err != nil {
		t.Fatalf("convert Responses input: %v", err)
	}
	req := &OpenAIRequest{
		Model:    "claude-opus-4.8",
		Messages: messages,
		Tools:    []OpenAITool{openAITestTool("exec_command")},
	}
	current := OpenAIToKiro(req, false).ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 3 {
		t.Fatalf("expected three active Responses tool results: %#v", current.UserInputMessageContext)
	}
	results := make(map[string]KiroToolResult, 3)
	for _, result := range current.UserInputMessageContext.ToolResults {
		results[result.ToolUseID] = result
	}
	if results["call_ok"].Status != "success" {
		t.Fatalf("completed result status=%q", results["call_ok"].Status)
	}
	for _, id := range []string{"call_error", "call_incomplete"} {
		if results[id].Status != "error" {
			t.Fatalf("Responses failure %s was not preserved: %#v", id, results[id])
		}
	}
}

func TestResponsesCompletedToolRoundsStayNativeAndKeepToolDefinitions(t *testing.T) {
	items := []json.RawMessage{
		mustRaw(`{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect the project"}]}`),
		mustRaw(`{"type":"function_call","call_id":"call_read","name":"read_file","arguments":"{\"path\":\"main.go\"}"}`),
		mustRaw(`{"type":"function_call_output","call_id":"call_read","output":"package main"}`),
		mustRaw(`{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect the tests"}]}`),
		mustRaw(`{"type":"function_call","call_id":"call_search","name":"search_files","arguments":"{\"query\":\"TestMain\"}"}`),
		mustRaw(`{"type":"function_call_output","call_id":"call_search","output":"main_test.go:10"}`),
		mustRaw(`{"type":"message","role":"user","content":[{"type":"input_text","text":"summarize"}]}`),
	}

	messages, err := convertResponsesInputItems(items)
	if err != nil {
		t.Fatalf("convert Responses input: %v", err)
	}
	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: messages}, false)
	if payload.HistoryMode != kiroHistoryModeNativeActive || payload.ConversationState.AgentTaskType != "vibe" {
		t.Fatalf("Responses protocol mode=%q task=%q", payload.HistoryMode, payload.ConversationState.AgentTaskType)
	}
	rounds := collectNativeKiroToolRounds(t, payload)
	if len(rounds) != 2 || rounds[0].Uses[0].ToolUseID != "call_read" ||
		kiroToolResultText(rounds[0].Results[0]) != "package main" ||
		rounds[1].Uses[0].ToolUseID != "call_search" ||
		kiroToolResultText(rounds[1].Results[0]) != "main_test.go:10" {
		t.Fatalf("Responses native history changed: %+v", rounds)
	}

	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.Tools) != 2 {
		t.Fatalf("historical Responses tools were not restored: %+v", context)
	}
	names := map[string]bool{}
	for _, tool := range context.Tools {
		names[tool.ToolSpecification.Name] = true
	}
	if !names["readFile"] || !names["searchFiles"] {
		t.Fatalf("restored Responses tools=%v", names)
	}
}

func mustRaw(s string) json.RawMessage {
	return json.RawMessage(s)
}
