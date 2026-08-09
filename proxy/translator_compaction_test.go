package proxy

import (
	"strings"
	"testing"
)

// Completed tool cycles survive compaction in Kiro's native causal structure.
func TestClaudeToKiroPreservesHistoryToolCyclesForCompaction(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run the build"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "running build"},
				map[string]interface{}{"type": "tool_use", "id": "t1", "name": "exec_command", "input": map[string]interface{}{"cmd": "make"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": "build ok"},
			}},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "t2", "name": "exec_command", "input": map[string]interface{}{"cmd": "test"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t2", "content": "tests pass"},
			}},
			// Final plain-text instruction (the compaction request).
			{Role: "user", Content: "Summarize everything that happened above."},
		},
	}

	payload := ClaudeToKiro(req, false)

	// Current message is plain text, so it must not carry structured tool results.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) > 0 {
		t.Fatalf("current message should not carry structured toolResults for a plain instruction")
	}
	if !strings.Contains(cur.Content, "Summarize everything") {
		t.Fatalf("expected current content to be the compaction instruction, got %q", cur.Content)
	}

	rounds := collectNativeKiroToolRounds(t, payload)
	if len(rounds) != 2 {
		t.Fatalf("structured tool rounds=%d, want 2", len(rounds))
	}
	for i, want := range []struct{ id, command, output string }{
		{"t1", "make", "build ok"},
		{"t2", "test", "tests pass"},
	} {
		if rounds[i].Uses[0].ToolUseID != want.id || rounds[i].Uses[0].Name != "execCommand" ||
			rounds[i].Uses[0].Input["cmd"] != want.command ||
			kiroToolResultText(rounds[i].Results[0]) != want.output {
			t.Fatalf("round %d changed: %+v", i, rounds[i])
		}
	}

	// Regression guard: assistant turns must NOT contain tool-invocation-looking
	// text. Such text trains the model to emit it instead of real tool calls.
	for i, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			if strings.Contains(a.Content, "[Called tool") {
				t.Fatalf("history[%d] assistant content contains mimicable tool-invocation text: %q", i, a.Content)
			}
		}
	}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.Tools) != 1 || context.Tools[0].ToolSpecification.Name != "execCommand" {
		t.Fatalf("historical tool definition was not retained in current tools: %+v", context)
	}
}

// TestClaudeToKiroKeepsActiveToolTurnStructured verifies the in-progress tool
// case still works: the last assistant turn issues a tool_use and the final user
// message delivers the matching tool_result. That single active turn must remain
// structured (last history assistant keeps toolUses, current keeps toolResults).
func TestClaudeToKiroKeepsActiveToolTurnStructured(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Tools: []ClaudeTool{{Name: "exec_command", Description: "run", InputSchema: map[string]interface{}{"type": "object"}}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run ls"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "t9", "name": "exec_command", "input": map[string]interface{}{"cmd": "ls"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t9", "content": "file1 file2"},
			}},
		},
	}

	payload := ClaudeToKiro(req, false)

	hist := payload.ConversationState.History
	if len(hist) == 0 {
		t.Fatalf("expected non-empty history")
	}
	last := hist[len(hist)-1].AssistantResponseMessage
	if last == nil || len(last.ToolUses) != 1 || last.ToolUses[0].ToolUseID != "t9" {
		t.Fatalf("expected last history assistant to keep the active structured tool use t9")
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected current message to keep the matching structured tool result")
	}
	if cur.UserInputMessageContext.ToolResults[0].ToolUseID != "t9" {
		t.Fatalf("expected current tool result to answer t9, got %q", cur.UserInputMessageContext.ToolResults[0].ToolUseID)
	}
}

// TestClaudeToKiroFlattensActiveToolResultWithoutToolConfig covers clients
// that omit `tools` on the follow-up tool-result request. Bedrock rejects a
// structured toolResult without the matching toolConfig, so the result must
// become ordinary user text while preserving its output.
func TestClaudeToKiroFlattensActiveToolResultWithoutToolConfig(t *testing.T) {
	t.Setenv("KIRO_NATIVE_STRUCTURED_HISTORY", "off")
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run ls"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "t-no-schema", "name": "exec_command", "input": map[string]interface{}{"cmd": "ls"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t-no-schema", "content": "file1 file2"},
			}},
		},
	}

	payload := ClaudeToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) > 0 {
		t.Fatalf("tool result must not be sent without toolConfig: %#v", cur.UserInputMessageContext.ToolResults)
	}
	if !strings.Contains(cur.Content, "file1 file2") {
		t.Fatalf("flattened tool output missing from current content: %q", cur.Content)
	}
	for i, item := range payload.ConversationState.History {
		if item.AssistantResponseMessage != nil && len(item.AssistantResponseMessage.ToolUses) > 0 {
			t.Fatalf("history[%d] retained toolUse without toolConfig: %#v", i, item.AssistantResponseMessage.ToolUses)
		}
	}
}

func TestOpenAIToKiroFlattensActiveToolResultWithoutToolConfig(t *testing.T) {
	t.Setenv("KIRO_NATIVE_STRUCTURED_HISTORY", "off")
	req := &OpenAIRequest{
		Model: "claude-opus-4.8",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "run ls"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "call-no-schema", Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "exec_command", Arguments: `{"cmd":"ls"}`},
			}}},
			{Role: "tool", ToolCallID: "call-no-schema", Content: "file1 file2"},
		},
	}

	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) > 0 {
		t.Fatalf("tool result must not be sent without toolConfig: %#v", cur.UserInputMessageContext.ToolResults)
	}
	if !strings.Contains(cur.Content, "file1 file2") {
		t.Fatalf("flattened tool output missing from current content: %q", cur.Content)
	}
}
