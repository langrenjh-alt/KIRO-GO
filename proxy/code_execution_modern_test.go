package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestCodeExecutionModernGeneratorAndPublicResult(t *testing.T) {
	req := &ClaudeRequest{Tools: []ClaudeTool{{Type: codeExecutionToolTypeLatest, Name: codeExecutionToolName}}, ToolChoice: map[string]interface{}{"type": "tool", "name": codeExecutionToolName}}
	generated := codeExecutionGeneratorRequest(req)
	if len(generated.Tools) != 1 || generated.Tools[0].Name != "bash_code_execution" {
		t.Fatalf("20250825 must expose Bash commands, tools=%#v", generated.Tools)
	}
	choice, err := resolveToolChoice(generated.ToolChoice)
	if err != nil || choice.Mode != "specific" || choice.Name != "bash_code_execution" {
		t.Fatalf("modern specific choice=%#v err=%v", choice, err)
	}
	if original, _ := resolveToolChoice(req.ToolChoice); original.Name != codeExecutionToolName {
		t.Fatal("original request choice was mutated")
	}
	blocks := codeExecutionResultBlocks(KiroToolUse{Name: "bash_code_execution", Input: map[string]interface{}{"command": "printf 'computed\\n'"}}, CodeExecutionResult{Stdout: "computed\n", ReturnCode: 0})
	if len(blocks) != 2 || blocks[0].Name != "bash_code_execution" || blocks[1].Type != "bash_code_execution_tool_result" || blocks[1].ToolUseID != blocks[0].ID {
		t.Fatalf("modern blocks=%#v", blocks)
	}
	raw, _ := json.Marshal(blocks[1].Content)
	var result map[string]interface{}
	_ = json.Unmarshal(raw, &result)
	if result["type"] != "bash_code_execution_result" || result["stdout"] != "computed\n" || result["return_code"] != float64(0) {
		t.Fatalf("modern result=%s", raw)
	}
}

func TestCodeExecutionModernHTTPCommandAndStreamingResult(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			const command = "python3 - <<'PY'\nvalues = [7, 13]\nprint(sum(values))\nPY\n"
			const stdout = "20\n"
			workerCalls, modelCalls := 0, 0
			executor, _ := configureTestCodeExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				workerCalls++
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["mode"] != "bash" || body["code"] != command {
					t.Errorf("modern command/mode changed: %#v err=%v", body, err)
				}
				_ = json.NewEncoder(w).Encode(CodeExecutionResult{Stdout: stdout, ReturnCode: 0})
			})
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				modelCalls++
				if modelCalls == 1 {
					if payload.ToolChoiceMode != "specific" || payload.ToolChoiceName != bashCodeExecutionToolName {
						t.Errorf("initial forced tool choice=%s/%s", payload.ToolChoiceMode, payload.ToolChoiceName)
					}
					callback.OnToolUse(KiroToolUse{ToolUseID: "modern-call", Name: bashCodeExecutionToolName, Input: map[string]interface{}{"command": command}})
					callback.OnStopReason("TOOL_USE")
				} else {
					encoded, _ := json.Marshal(payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext)
					if !strings.Contains(string(encoded), `20`) || !strings.Contains(string(encoded), "return_code") {
						t.Errorf("actual result absent from final model context: %s", encoded)
					}
					callback.OnText("The actual command returned 20.", false)
					callback.OnStopReason("END_TURN")
				}
				callback.OnCredits(0.2)
				callback.OnComplete(10, 5)
				return nil
			})
			h.codeExecutor = executor
			req := modernCodeExecutionRequest(stream)
			req.ToolChoice = map[string]interface{}{"type": "tool", "name": codeExecutionToolName}
			rec := httptest.NewRecorder()
			h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 10, "", nil)
			if rec.Code != 200 || modelCalls != 2 || workerCalls != 1 {
				t.Fatalf("status=%d model=%d worker=%d response=%s", rec.Code, modelCalls, workerCalls, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), `"type":"code_execution_result"`) || strings.Contains(rec.Body.String(), `"name":"code_execution"`) {
				t.Fatalf("modern request leaked legacy wire protocol: %s", rec.Body.String())
			}
			if !stream {
				var response ClaudeResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Content) != 3 || response.StopReason != "end_turn" || response.Content[0].Name != bashCodeExecutionToolName || response.Content[1].Type != "bash_code_execution_tool_result" || response.Content[1].ToolUseID != response.Content[0].ID {
					t.Fatalf("response=%#v", response)
				}
				return
			}
			events := decodeResponseContractSSE(t, rec.Body.String())
			var commandJSON strings.Builder
			serverID, resultID, resultType, resultOutput := "", "", "", ""
			for _, event := range events {
				if event.Name == "content_block_start" {
					block := responseContractMap(t, event.Data["content_block"], "block")
					if block["type"] == "server_tool_use" {
						if block["name"] != bashCodeExecutionToolName {
							t.Errorf("modern server name=%v", block["name"])
						}
						serverID = fmt.Sprint(block["id"])
					}
					if block["type"] == "bash_code_execution_tool_result" {
						resultID = fmt.Sprint(block["tool_use_id"])
						content := responseContractMap(t, block["content"], "result")
						resultType, resultOutput = fmt.Sprint(content["type"]), fmt.Sprint(content["stdout"])
					}
				}
				if event.Name == "content_block_delta" {
					delta := responseContractMap(t, event.Data["delta"], "delta")
					if delta["type"] == "input_json_delta" {
						commandJSON.WriteString(fmt.Sprint(delta["partial_json"]))
					}
				}
			}
			var input map[string]string
			if err := json.Unmarshal([]byte(commandJSON.String()), &input); err != nil || input["command"] != command || serverID == "" || resultID != serverID || resultType != "bash_code_execution_result" || resultOutput != stdout {
				t.Fatalf("modern SSE mismatch: input=%#v err=%v ids=%s/%s type=%s stdout=%q", input, err, serverID, resultID, resultType, resultOutput)
			}
		})
	}
}

func TestCodeExecutionModernErrorContractsAndLegacyIsolation(t *testing.T) {
	for _, failure := range []struct{ worker, public string }{
		{"executor_busy", "too_many_requests"}, {"execution_timeout", "execution_time_exceeded"},
		{"output_limit_exceeded", "output_file_too_large"}, {"code_too_large", "invalid_tool_input"}, {"runtime_error", "unavailable"},
	} {
		blocks := codeExecutionResultBlocks(KiroToolUse{Name: bashCodeExecutionToolName}, CodeExecutionResult{ReturnCode: -1, ErrorCode: failure.worker})
		content := blocks[1].Content.(map[string]interface{})
		if content["type"] != "bash_code_execution_tool_result_error" || content["error_code"] != failure.public {
			t.Errorf("worker error %q => %#v", failure.worker, content)
		}
	}
	legacy := codeExecutionResultBlocks(KiroToolUse{Name: codeExecutionToolName}, CodeExecutionResult{ReturnCode: -1, ErrorCode: "output_limit_exceeded"})
	if content := legacy[1].Content.(map[string]interface{}); content["type"] != "code_execution_tool_result_error" || content["error_code"] != "unavailable" {
		t.Fatalf("legacy error enum changed: %#v", content)
	}
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			callback.OnToolUse(KiroToolUse{ToolUseID: "modern", Name: bashCodeExecutionToolName, Input: map[string]interface{}{"command": "printf x"}})
			callback.OnStopReason("TOOL_USE")
		} else {
			callback.OnText("The Bash executor is unavailable.", false)
			callback.OnStopReason("END_TURN")
		}
		callback.OnComplete(5, 5)
		return nil
	})
	h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
		t.Error("Bash command reached a Python-only executor")
		return CodeExecutionResult{}, nil
	})
	rec := httptest.NewRecorder()
	req := modernCodeExecutionRequest(false)
	h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 10, "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"error_code":"unavailable"`) || !strings.Contains(rec.Body.String(), `bash_code_execution_tool_result_error`) {
		t.Fatalf("unavailable result=%s", rec.Body.String())
	}
}

type modernCodeExecutorFunc func(context.Context, string, string) (CodeExecutionResult, error)

func (f modernCodeExecutorFunc) Execute(context.Context, string) (CodeExecutionResult, error) {
	return CodeExecutionResult{ReturnCode: -1, ErrorCode: "unavailable"}, fmt.Errorf("Python path must not be used for Bash")
}

func (f modernCodeExecutorFunc) ExecuteMode(ctx context.Context, mode, code string) (CodeExecutionResult, error) {
	return f(ctx, mode, code)
}

func TestCodeExecutionModernLimitsSchemaCorrectionAndBilling(t *testing.T) {
	modelCalls, workerCalls := 0, 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		modelCalls++
		if !payload.StructuredOutput {
			t.Error("modern execution discarded the schema")
		}
		if modelCalls <= maxCodeExecutionRounds {
			callback.OnToolUse(KiroToolUse{ToolUseID: fmt.Sprintf("bash-%d", modelCalls), Name: bashCodeExecutionToolName, Input: map[string]interface{}{"command": "python3 -c 'print(5 + 8)'"}})
			callback.OnStopReason("TOOL_USE")
		} else {
			if payload.ToolChoiceMode != "none" {
				t.Error("execution limit or schema retry re-enabled tools")
			}
			if modelCalls == maxCodeExecutionRounds+1 {
				callback.OnText(`{"answer":"invalid"}`, false)
			} else {
				callback.OnText(`{"answer":13}`, false)
			}
			callback.OnStopReason("END_TURN")
		}
		callback.OnCredits(.25)
		callback.OnComplete(7, 3)
		return nil
	})
	h.codeExecutor = modernCodeExecutorFunc(func(_ context.Context, mode, command string) (CodeExecutionResult, error) {
		workerCalls++
		if mode != "bash" || command != "python3 -c 'print(5 + 8)'" {
			t.Errorf("mode=%s command=%s", mode, command)
		}
		return CodeExecutionResult{Stdout: "13\n"}, nil
	})
	key, err := config.AddApiKey(config.ApiKeyEntry{Name: "modern billing fixture", Key: "sk-modern-billing-fixture", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	req := modernCodeExecutionRequest(true)
	req.OutputConfig = &ClaudeOutputConfig{Format: &ClaudeOutputFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer","const":13}},"required":["answer"],"additionalProperties":false}`)}}
	rec := httptest.NewRecorder()
	h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 10, key.ID, nil)
	text, stop := structuredContractResponse(t, rec, true)
	if rec.Code != 200 || modelCalls != maxCodeExecutionRounds+2 || workerCalls != maxCodeExecutionRounds || text != `{"answer":13}` || stop != "end_turn" {
		t.Fatalf("status=%d model=%d worker=%d text=%s stop=%s body=%s", rec.Code, modelCalls, workerCalls, text, stop, rec.Body.String())
	}
	if h.totalCredits != 1.25 || config.GetApiKeyEntry(key.ID).CreditsUsed != 1.25 {
		t.Fatalf("modern usage missing/doubled: global=%v key=%+v", h.totalCredits, config.GetApiKeyEntry(key.ID))
	}
	accounts := h.pool.GetAllAccounts()
	if len(accounts) != 1 || accounts[0].RequestCount != 5 || accounts[0].TotalCredits != 1.25 {
		t.Fatalf("modern pool usage missing/doubled: %+v", accounts)
	}
}

func TestCodeExecutionModernCancellationSettlesConsumedModelRound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnToolUse(KiroToolUse{ToolUseID: "cancel-bash", Name: bashCodeExecutionToolName, Input: map[string]interface{}{"command": "sleep 10"}})
		callback.OnCredits(.25)
		callback.OnComplete(7, 3)
		callback.OnStopReason("TOOL_USE")
		return nil
	})
	h.codeExecutor = modernCodeExecutorFunc(func(_ context.Context, mode, _ string) (CodeExecutionResult, error) {
		if mode != "bash" {
			t.Errorf("cancel mode=%s", mode)
		}
		cancel()
		return CodeExecutionResult{ReturnCode: -1, ErrorCode: "execution_cancelled"}, context.Canceled
	})
	key, err := config.AddApiKey(config.ApiKeyEntry{Name: "modern cancel fixture", Key: "sk-modern-cancel-fixture", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	req := modernCodeExecutionRequest(true)
	rec := httptest.NewRecorder()
	h.handleCodeExecutionRequest(rec, ctx, req, req.Model, 10, key.ID, nil)
	keyUsage := config.GetApiKeyEntry(key.ID)
	if rec.Body.Len() != 0 || h.totalCredits != .25 || keyUsage.CreditsUsed != .25 || keyUsage.RequestsCount != 1 || keyUsage.TokensUsed <= 0 {
		t.Fatalf("modern cancelled output/billing: body=%s global=%v key=%+v", rec.Body.String(), h.totalCredits, keyUsage)
	}
}

func TestCodeExecutionModernNoneAndClientToolCoexistence(t *testing.T) {
	for _, none := range []bool{false, true} {
		t.Run(fmt.Sprintf("none=%v", none), func(t *testing.T) {
			workerCalls := 0
			executor, _ := configureTestCodeExecutor(t, func(w http.ResponseWriter, _ *http.Request) {
				workerCalls++
				_ = json.NewEncoder(w).Encode(CodeExecutionResult{Stdout: "shell\n"})
			})
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				if none {
					if payload.ToolChoiceMode != "none" {
						t.Errorf("none mode=%s", payload.ToolChoiceMode)
					}
					callback.OnText("No tools used.", false)
					callback.OnStopReason("END_TURN")
				} else {
					callback.OnToolUse(KiroToolUse{ToolUseID: "bash", Name: bashCodeExecutionToolName, Input: map[string]interface{}{"command": "printf shell"}})
					callback.OnToolUse(KiroToolUse{ToolUseID: "client", Name: "lookup", Input: map[string]interface{}{"query": "next"}})
					callback.OnStopReason("TOOL_USE")
				}
				callback.OnComplete(5, 5)
				return nil
			})
			h.codeExecutor = executor
			req := modernCodeExecutionRequest(false)
			req.Tools = append(req.Tools, ClaudeTool{Name: "lookup", InputSchema: map[string]interface{}{"type": "object"}})
			if none {
				req.ToolChoice = map[string]interface{}{"type": "none"}
			}
			rec := httptest.NewRecorder()
			h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 10, "", nil)
			var response ClaudeResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != 200 {
				t.Fatalf("response status=%d err=%v body=%s", rec.Code, err, rec.Body.String())
			}
			if none {
				if workerCalls != 0 || len(response.Content) != 1 || response.Content[0].Type != "text" {
					t.Fatalf("none executed tools: calls=%d response=%#v", workerCalls, response)
				}
			} else if workerCalls != 1 || response.StopReason != "tool_use" || len(response.Content) != 3 || response.Content[2].Type != "tool_use" || response.Content[2].Name != "lookup" {
				t.Fatalf("client coexistence calls=%d response=%#v", workerCalls, response)
			}
		})
	}
}

func modernCodeExecutionRequest(stream bool) *ClaudeRequest {
	return &ClaudeRequest{Model: "claude-opus-5.5", Stream: stream, MaxTokens: 512, Messages: []ClaudeMessage{{Role: "user", Content: "Run the supplied computation and report its actual result."}}, Tools: []ClaudeTool{{Type: codeExecutionToolTypeLatest, Name: codeExecutionToolName}}}
}
