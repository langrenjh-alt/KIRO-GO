package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

func TestClaudeToolDecodesServerToolMetadata(t *testing.T) {
	var tool ClaudeTool
	err := json.Unmarshal([]byte(`{
		"type":"web_search_20260209",
		"name":"web_search",
		"model":"claude-opus-4-8",
		"max_uses":4,
		"max_tokens":2048,
		"caching":{"type":"ephemeral","ttl":"5m"},
		"allowed_domains":["example.com"],
		"blocked_domains":["blocked.example"]
	}`), &tool)
	if err != nil {
		t.Fatalf("unmarshal Claude tool: %v", err)
	}

	if tool.Type != "web_search_20260209" || tool.MaxUses != 4 {
		t.Fatalf("server tool metadata was not retained: %#v", tool)
	}
	if tool.Model != "claude-opus-4-8" || tool.MaxTokens != 2048 || tool.Caching == nil {
		t.Fatalf("server tool model settings were not retained: %#v", tool)
	}
	if len(tool.AllowedDomains) != 1 || tool.AllowedDomains[0] != "example.com" {
		t.Fatalf("allowed_domains was not retained: %#v", tool.AllowedDomains)
	}
	if len(tool.BlockedDomains) != 1 || tool.BlockedDomains[0] != "blocked.example" {
		t.Fatalf("blocked_domains was not retained: %#v", tool.BlockedDomains)
	}
}

func TestHandleAdvisorRequestExecutesAdvisorAndExecutorWithMixedTools(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "advisor-account",
		Enabled:     true,
		AccessToken: "advisor-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:1:profile/advisor",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	pool := accountpool.GetPool()
	pool.Reload()
	t.Cleanup(pool.WaitForPendingStats)

	var calls int
	h := &Handler{
		pool:        pool,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		kiroCaller: func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
			calls++
			model := payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
			raw, _ := json.Marshal(payload)
			switch calls {
			case 1:
				if model != "claude-opus-4.8" {
					t.Fatalf("advisor model = %q", model)
				}
				ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
				if ctx != nil && len(ctx.Tools) != 0 {
					t.Fatalf("advisor sub-call received client tools: %#v", ctx.Tools)
				}
				callback.OnText("Check the state transition before changing the implementation.", false)
				callback.OnComplete(11, 7)
			case 2:
				if model != "claude-sonnet-4.6" {
					t.Fatalf("executor model = %q", model)
				}
				if !strings.Contains(string(raw), "Check the state transition") {
					t.Fatalf("executor payload is missing advisor guidance: %s", raw)
				}
				ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
				if ctx == nil || len(ctx.Tools) != 1 || ctx.Tools[0].ToolSpecification.Name != "read" {
					t.Fatalf("executor did not retain the ordinary tool: %#v", ctx)
				}
				callback.OnText("Implemented after checking the transition.", false)
				callback.OnComplete(17, 9)
			default:
				t.Fatalf("unexpected upstream call %d", calls)
			}
			return nil
		},
	}

	body := `{
		"model":"claude-sonnet-4-6",
		"max_tokens":4096,
		"messages":[{"role":"user","content":"Implement the change"}],
		"tools":[
			{"name":"Read","description":"Read a file","input_schema":{"type":"object"}},
			{"type":"advisor_20260301","name":"advisor","model":"claude-opus-4-8"}
		]
	}`
	recorder := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)),
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want advisor + executor", calls)
	}
	var response ClaudeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Content) < 3 {
		t.Fatalf("content = %#v", response.Content)
	}
	serverUse := response.Content[0]
	advisorResult := response.Content[1]
	if serverUse.Type != "server_tool_use" || serverUse.Name != advisorToolName || serverUse.ID == "" {
		t.Fatalf("server tool block = %#v", serverUse)
	}
	if advisorResult.Type != "advisor_tool_result" || advisorResult.ToolUseID != serverUse.ID {
		t.Fatalf("advisor result block = %#v", advisorResult)
	}
	resultContent, ok := advisorResult.Content.(map[string]interface{})
	if !ok || resultContent["type"] != "advisor_result" || !strings.Contains(fmt.Sprint(resultContent["text"]), "state transition") {
		t.Fatalf("advisor result content = %#v", advisorResult.Content)
	}
	if response.Content[2].Type != "text" || !strings.Contains(response.Content[2].Text, "Implemented") {
		t.Fatalf("executor response block = %#v", response.Content[2])
	}
}

func TestWriteBufferedClaudeSSEPreservesAdvisorBlocks(t *testing.T) {
	response := KiroToClaudeResponse("final", "", false, nil, 12, 8, "claude-sonnet-4.6")
	response.Content = append([]ClaudeContentBlock{
		{Type: "server_tool_use", ID: "srvtoolu_advisor", Name: advisorToolName, Input: map[string]interface{}{}},
		{
			Type:      "advisor_tool_result",
			ToolUseID: "srvtoolu_advisor",
			Content:   map[string]interface{}{"type": "advisor_result", "text": "review"},
		},
	}, response.Content...)
	recorder := httptest.NewRecorder()
	if err := writeBufferedClaudeSSE(recorder, response); err != nil {
		t.Fatalf("writeBufferedClaudeSSE: %v", err)
	}

	events := decodeWebSearchSSE(t, recorder.Body.String())
	var serverID, resultID string
	for _, event := range events {
		if event.Name != "content_block_start" {
			continue
		}
		block, _ := event.Data["content_block"].(map[string]interface{})
		switch block["type"] {
		case "server_tool_use":
			serverID, _ = block["id"].(string)
		case "advisor_tool_result":
			resultID, _ = block["tool_use_id"].(string)
		}
	}
	if serverID != "srvtoolu_advisor" || resultID != serverID {
		t.Fatalf("advisor stream pairing: server=%q result=%q", serverID, resultID)
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
