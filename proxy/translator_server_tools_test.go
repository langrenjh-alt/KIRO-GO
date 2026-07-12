package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeToolDecodesServerToolMetadata(t *testing.T) {
	var tool ClaudeTool
	err := json.Unmarshal([]byte(`{
		"type":"web_search_20260209",
		"name":"web_search",
		"max_uses":4,
		"allowed_domains":["example.com"],
		"blocked_domains":["blocked.example"]
	}`), &tool)
	if err != nil {
		t.Fatalf("unmarshal Claude tool: %v", err)
	}

	if tool.Type != "web_search_20260209" || tool.MaxUses != 4 {
		t.Fatalf("server tool metadata was not retained: %#v", tool)
	}
	if len(tool.AllowedDomains) != 1 || tool.AllowedDomains[0] != "example.com" {
		t.Fatalf("allowed_domains was not retained: %#v", tool.AllowedDomains)
	}
	if len(tool.BlockedDomains) != 1 || tool.BlockedDomains[0] != "blocked.example" {
		t.Fatalf("blocked_domains was not retained: %#v", tool.BlockedDomains)
	}
}

func TestConvertClaudeToolsSkipsTypedServerToolsAndEmptyNames(t *testing.T) {
	tools, _ := convertClaudeTools([]ClaudeTool{
		{Name: "read_file", Description: "Read a file", InputSchema: map[string]interface{}{"type": "object"}},
		{Type: "custom", Name: "write_file", Description: "Write a file", InputSchema: map[string]interface{}{"type": "object"}},
		{Type: "advisor_20260301", Name: "advisor"},
		{Type: "web_search_20250305", Name: "web_search"},
		{Type: "web_fetch_20260209", Name: "web_fetch"},
		{Name: "   ", Description: "must not become the fallback tool name"},
	})

	if len(tools) != 2 {
		t.Fatalf("expected only two custom tools, got %#v", tools)
	}
	if tools[0].ToolSpecification.Name != "readFile" || tools[1].ToolSpecification.Name != "writeFile" {
		t.Fatalf("unexpected converted tools: %#v", tools)
	}
}

func TestClaudeServerToolBlocksRemainReadableWithoutToolUses(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		Tools: []ClaudeTool{
			{Type: "advisor_20260301", Name: "advisor"},
			{Type: "web_search_20250305", Name: "web_search"},
		},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Research the release"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{
					"type":  "server_tool_use",
					"id":    "srvtoolu_1",
					"name":  "web_search",
					"input": map[string]interface{}{"query": "Kiro release notes"},
				},
				map[string]interface{}{
					"type":        "advisor_tool_result",
					"tool_use_id": "srvtoolu_2",
					"content": map[string]interface{}{
						"type": "advisor_result",
						"text": "Check the primary source before answering.",
					},
				},
				map[string]interface{}{
					"type":        "web_search_tool_result",
					"tool_use_id": "srvtoolu_1",
					"content": []interface{}{
						map[string]interface{}{
							"type":              "web_search_result",
							"title":             "Kiro release notes",
							"url":               "https://example.com/releases",
							"encrypted_content": "opaque-secret-payload",
						},
					},
				},
			}},
			{Role: "user", Content: "Summarize what you found"},
		},
	}

	payload := ClaudeToKiro(req, false)
	if ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext; ctx != nil && len(ctx.Tools) != 0 {
		t.Fatalf("typed server tools must not be sent as ordinary Kiro tools: %#v", ctx.Tools)
	}

	var assistant *KiroAssistantResponseMessage
	for i := range payload.ConversationState.History {
		if payload.ConversationState.History[i].AssistantResponseMessage != nil {
			assistant = payload.ConversationState.History[i].AssistantResponseMessage
		}
	}
	if assistant == nil {
		t.Fatal("expected server tool context to remain in assistant history")
	}
	if len(assistant.ToolUses) != 0 {
		t.Fatalf("server_tool_use must not become an ordinary tool use: %#v", assistant.ToolUses)
	}
	for _, want := range []string{
		"Kiro release notes",
		"Check the primary source before answering.",
		"https://example.com/releases",
	} {
		if !strings.Contains(assistant.Content, want) {
			t.Fatalf("expected history to retain %q, got %q", want, assistant.Content)
		}
	}
	if strings.Contains(assistant.Content, "opaque-secret-payload") {
		t.Fatalf("encrypted server payload should not be copied into readable history: %q", assistant.Content)
	}
}

func TestClaudeCustomReadPagesSchemaAndInputPassThrough(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		Tools: []ClaudeTool{{
			Type:        "custom",
			Name:        "Read",
			Description: "Read a file",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string"},
					"pages":     map[string]interface{}{"type": "string"},
				},
			},
		}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Read page two"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{
					"type": "tool_use",
					"id":   "toolu_read",
					"name": "Read",
					"input": map[string]interface{}{
						"file_path": "manual.pdf",
						"pages":     "2",
					},
				},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "toolu_read",
					"content":     "page two text",
				},
			}},
		},
	}

	payload := ClaudeToKiro(req, false)
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.Tools) != 1 {
		t.Fatalf("expected Read to remain a custom tool, got %#v", ctx)
	}
	schema, ok := ctx.Tools[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected Read schema type: %#v", ctx.Tools[0].ToolSpecification.InputSchema.JSON)
	}
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok || properties["pages"] == nil {
		t.Fatalf("Read.pages schema was removed or remapped: %#v", schema)
	}

	history := payload.ConversationState.History
	if len(history) == 0 || history[len(history)-1].AssistantResponseMessage == nil {
		t.Fatalf("expected active Read tool use in history: %#v", history)
	}
	toolUses := history[len(history)-1].AssistantResponseMessage.ToolUses
	if len(toolUses) != 1 || toolUses[0].Input["pages"] != "2" {
		t.Fatalf("Read.pages input was removed or rejected: %#v", toolUses)
	}
}
