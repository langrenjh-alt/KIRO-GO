package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCodeExecutionGeneratorRequestUsesClientToolOnly(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-opus-4.8",
		Stream:   true,
		Messages: []ClaudeMessage{{Role: "user", Content: "calculate a digest"}},
		Tools:    []ClaudeTool{{Type: codeExecutionToolType, Name: codeExecutionToolName}},
	}
	generated := codeExecutionGeneratorRequest(req)
	if generated.Stream || generated.Thinking != nil || generated.OutputConfig != nil {
		t.Fatalf("generator request retained response-only options: %#v", generated)
	}
	if len(generated.Tools) != 1 || generated.Tools[0].Type != "custom" || generated.Tools[0].Name != codeExecutionToolName {
		t.Fatalf("generator tool = %#v", generated.Tools)
	}
	choice, err := resolveToolChoice(generated.ToolChoice)
	if err != nil || choice.Mode != "specific" || choice.Name != codeExecutionToolName {
		t.Fatalf("generator tool choice = %#v, err=%v", choice, err)
	}
}

func TestNativeCodeExecutionVersions(t *testing.T) {
	for _, toolType := range []string{codeExecutionToolType, codeExecutionToolTypeLatest} {
		if !isNativeCodeExecutionTool(ClaudeTool{Type: toolType, Name: codeExecutionToolName}) {
			t.Fatalf("supported code execution type %q was rejected", toolType)
		}
	}
	for _, tool := range []ClaudeTool{
		{Type: "code_execution_20990101", Name: codeExecutionToolName},
		{Type: codeExecutionToolType, Name: "other"},
	} {
		if isNativeCodeExecutionTool(tool) {
			t.Fatalf("unsupported code execution declaration was accepted: %#v", tool)
		}
	}
}

func TestCodeExecutionResponseContracts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h := newResponseContractHandler(t,
				awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
				awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
					"toolUseId": "kiro-code", "name": codeExecutionToolName,
					"input": `{"code":"print('contract')"}`, "stop": true,
				}),
			)
			body := fmt.Sprintf(`{
				"model":"claude-opus-4-8",
				"max_tokens":512,
				"stream":%v,
				"messages":[{"role":"user","content":"Run a calculation."}],
				"tools":[{"type":"code_execution_20250522","name":"code_execution"}],
				"tool_choice":{"type":"auto"}
			}`, stream)
			var clientRequest ClaudeRequest
			if err := json.Unmarshal([]byte(body), &clientRequest); err != nil {
				t.Fatalf("decode client request: %v", err)
			}
			wantInputTokens := estimateClaudeRequestInputTokens(&clientRequest)
			rec := httptest.NewRecorder()
			h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}

			if !stream {
				var response ClaudeResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				assertCodeExecutionBlocks(t, response.Content)
				if response.Container == nil || !strings.HasPrefix(response.Container.ID, "container_"+claudeMessageIDVersion) || response.Container.ExpiresAt == "" {
					t.Fatalf("container=%#v", response.Container)
				}
				if response.StopReason != "end_turn" || response.Usage.ServerToolUse == nil {
					t.Fatalf("stop=%q usage=%#v", response.StopReason, response.Usage)
				}
				if total := response.Usage.InputTokens + response.Usage.CacheCreationInputTokens + response.Usage.CacheReadInputTokens; total != wantInputTokens {
					t.Fatalf("code execution public input=%d, want client estimate %d", total, wantInputTokens)
				}
				assertKiroContextUsageValue(t, response.Usage.KiroContextUsage, 125_000, 1_000_000, 12.5, false)
				return
			}

			events := decodeResponseContractSSE(t, rec.Body.String())
			wantOrder := []string{
				"message_start", "content_block_start", "ping", "content_block_delta", "content_block_delta",
				"content_block_stop", "content_block_start", "content_block_stop", "content_block_start",
				"content_block_delta", "content_block_stop", "message_delta", "message_stop",
			}
			gotOrder := make([]string, len(events))
			for i := range events {
				gotOrder[i] = events[i].Name
			}
			if !reflect.DeepEqual(gotOrder, wantOrder) {
				t.Fatalf("event order:\n got=%v\nwant=%v\n%s", gotOrder, wantOrder, rec.Body.String())
			}
			serverUse := responseContractMap(t, events[1].Data["content_block"], "server tool block")
			serverID := fmt.Sprint(serverUse["id"])
			if serverUse["type"] != "server_tool_use" || serverUse["name"] != codeExecutionToolName || !validClaudeOpaqueID(serverID, "srvtoolu_") {
				t.Fatalf("server tool=%#v", serverUse)
			}
			result := responseContractMap(t, events[6].Data["content_block"], "code result block")
			if result["type"] != "code_execution_tool_result" || result["tool_use_id"] != serverID {
				t.Fatalf("result=%#v", result)
			}
			terminalDelta := responseContractMap(t, events[11].Data["delta"], "message delta")
			container := responseContractMap(t, terminalDelta["container"], "container")
			if !strings.HasPrefix(fmt.Sprint(container["id"]), "container_"+claudeMessageIDVersion) || terminalDelta["stop_reason"] != "end_turn" {
				t.Fatalf("terminal delta=%#v", terminalDelta)
			}
			usage := responseContractMap(t, events[11].Data["usage"], "terminal usage")
			if _, ok := usage["server_tool_use"].(map[string]interface{}); !ok {
				t.Fatalf("terminal usage=%#v", usage)
			}
			totalInput := int(usage["input_tokens"].(float64) + usage["cache_creation_input_tokens"].(float64) + usage["cache_read_input_tokens"].(float64))
			if totalInput != wantInputTokens {
				t.Fatalf("code execution terminal input=%d, want client estimate %d", totalInput, wantInputTokens)
			}
			assertKiroContextUsageJSON(t, usage["kiro_context_usage"], 125_000, 1_000_000, 12.5, false)
		})
	}
}

func assertCodeExecutionBlocks(t *testing.T, blocks []ClaudeContentBlock) {
	t.Helper()
	if len(blocks) != 3 {
		t.Fatalf("blocks=%#v", blocks)
	}
	serverUse := blocks[0]
	if serverUse.Type != "server_tool_use" || serverUse.Name != codeExecutionToolName || !validClaudeOpaqueID(serverUse.ID, "srvtoolu_") {
		t.Fatalf("server use=%#v", serverUse)
	}
	input, ok := serverUse.Input.(map[string]interface{})
	if !ok || input["code"] != "print('contract')" {
		t.Fatalf("server input=%#v", serverUse.Input)
	}
	result := blocks[1]
	if result.Type != "code_execution_tool_result" || result.ToolUseID != serverUse.ID {
		t.Fatalf("result=%#v", result)
	}
	content, ok := result.Content.(map[string]interface{})
	if !ok || content["type"] != "code_execution_tool_result_error" || content["error_code"] != "too_many_requests" {
		t.Fatalf("result content=%#v", result.Content)
	}
	if blocks[2].Type != "text" || blocks[2].Text != codeExecutionUnavailableText {
		t.Fatalf("text block=%#v", blocks[2])
	}
}
