package proxy

import (
	"strings"
	"testing"
)

func TestKiroRSClaudeMetadataSessionIDBecomesConversationID(t *testing.T) {
	want := "8bb5523b-ec7c-4540-a9ca-beb6d79f1552"
	for _, userID := range []string{
		"user_account__session_" + want,
		`{"device_id":"device","session_id":"` + want + `"}`,
	} {
		payload := ClaudeToKiro(&ClaudeRequest{
			Model:    "claude-sonnet-4.5",
			Metadata: &ClaudeRequestMetadata{UserID: userID},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
		}, false)
		if got := payload.ConversationState.ConversationID; got != want {
			t.Fatalf("metadata user_id %q produced conversationId %q, want %q", userID, got, want)
		}
	}
}

func TestKiroRSClaudeDropsTrailingAssistantPrefill(t *testing.T) {
	payload := ClaudeToKiro(&ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "real request"},
			{Role: "assistant", Content: "prefill that Kiro cannot continue"},
		},
	}, false)
	current := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if current != "real request" {
		t.Fatalf("current content=%q, want the final user message", current)
	}
	if len(payload.ConversationState.History) != 0 {
		t.Fatalf("assistant prefill leaked into history: %#v", payload.ConversationState.History)
	}
}

func TestKiroRSClaudeAddsWriteEditChunkingContract(t *testing.T) {
	payload := ClaudeToKiro(&ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "system",
		Tools: []ClaudeTool{
			{Name: "Write", Description: "write files", InputSchema: map[string]interface{}{"type": "object"}},
			{Name: "Edit", Description: "edit files", InputSchema: map[string]interface{}{"type": "object"}},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "update the file"}},
	}, false)
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected system priming history, got %#v", payload.ConversationState.History)
	}
	if !strings.Contains(payload.ConversationState.History[0].UserInputMessage.Content, chunkedWriteSystemPolicy) {
		t.Fatalf("system chunking policy missing: %q", payload.ConversationState.History[0].UserInputMessage.Content)
	}
	tools := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	if len(tools) != 2 || !strings.Contains(tools[0].ToolSpecification.Description, writeToolDescriptionSuffix) ||
		!strings.Contains(tools[1].ToolSpecification.Description, editToolDescriptionSuffix) {
		t.Fatalf("Write/Edit description suffixes missing: %#v", tools)
	}
}

func TestKiroRSClaudeAddsChunkingPolicyToEveryNonEmptySystem(t *testing.T) {
	payload := ClaudeToKiro(&ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   "system without tools",
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}, false)
	if len(payload.ConversationState.History) < 2 || payload.ConversationState.History[0].UserInputMessage == nil ||
		!strings.Contains(payload.ConversationState.History[0].UserInputMessage.Content, chunkedWriteSystemPolicy) {
		t.Fatalf("system chunking policy missing without Write/Edit tools: %#v", payload.ConversationState.History)
	}

	thinkingOnly := ClaudeToKiro(&ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		Thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 1024},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}, true)
	if len(thinkingOnly.ConversationState.History) > 0 && thinkingOnly.ConversationState.History[0].UserInputMessage != nil &&
		strings.Contains(thinkingOnly.ConversationState.History[0].UserInputMessage.Content, chunkedWriteSystemPolicy) {
		t.Fatalf("chunking policy leaked into thinking-only priming: %#v", thinkingOnly.ConversationState.History)
	}
}

func TestKiroRSMergesConsecutiveAssistantHistory(t *testing.T) {
	payload := ClaudeToKiro(&ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "part one"},
			{Role: "assistant", Content: "part two"},
			{Role: "user", Content: "continue"},
		},
	}, false)
	if len(payload.ConversationState.History) != 2 {
		t.Fatalf("expected one user and one merged assistant history entry, got %#v", payload.ConversationState.History)
	}
	assistant := payload.ConversationState.History[1].AssistantResponseMessage
	if assistant == nil || !strings.Contains(assistant.Content, "part one") || !strings.Contains(assistant.Content, "part two") {
		t.Fatalf("consecutive assistant messages were not merged: %#v", payload.ConversationState.History)
	}
}

func TestKiroRSClosesBufferedUserHistoryBeforeCurrentTurn(t *testing.T) {
	claude := ClaudeToKiro(&ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "first user turn"},
			{Role: "user", Content: "current user turn"},
		},
	}, false)
	if len(claude.ConversationState.History) != 2 ||
		claude.ConversationState.History[0].UserInputMessage == nil ||
		claude.ConversationState.History[1].AssistantResponseMessage == nil ||
		claude.ConversationState.History[1].AssistantResponseMessage.Content != "OK" {
		t.Fatalf("Claude history did not close buffered user turn: %#v", claude.ConversationState.History)
	}

	openai := OpenAIToKiro(&OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "first user turn"},
			{Role: "user", Content: "current user turn"},
		},
	}, false)
	if len(openai.ConversationState.History) != 2 ||
		openai.ConversationState.History[0].UserInputMessage == nil ||
		openai.ConversationState.History[1].AssistantResponseMessage == nil ||
		openai.ConversationState.History[1].AssistantResponseMessage.Content != "OK" {
		t.Fatalf("OpenAI history did not close buffered user turn: %#v", openai.ConversationState.History)
	}
}

func TestKiroRSClaudeCodeAliasesUseGenerationAndClaudeAuth(t *testing.T) {
	for _, path := range []string{"/cc/v1/messages", "/cc/v1/messages/count_tokens"} {
		if !isGenerationAPIPath(path) || !isClaudeCredentialPath(path) {
			t.Fatalf("Claude Code path %q is not classified as an authenticated generation route", path)
		}
	}
}
