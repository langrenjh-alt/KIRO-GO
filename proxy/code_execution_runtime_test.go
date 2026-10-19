package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

type codeExecutorFunc func(context.Context, string) (CodeExecutionResult, error)

func (f codeExecutorFunc) Execute(ctx context.Context, code string) (CodeExecutionResult, error) {
	return f(ctx, code)
}

func configureTestCodeExecutor(t *testing.T, handler http.HandlerFunc) (CodeExecutor, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	tokenPath := filepath.Join(t.TempDir(), "executor-token")
	if err := os.WriteFile(tokenPath, []byte("test-worker-credential\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIRO_CODE_EXECUTOR_URL", server.URL)
	t.Setenv("KIRO_CODE_EXECUTOR_TOKEN_FILE", tokenPath)
	executor, err := configuredCodeExecutor()
	if err != nil {
		t.Fatal(err)
	}
	return executor, server
}

func TestCodeExecutorHTTPProtocolAndRealNonzeroExit(t *testing.T) {
	var called atomic.Int32
	executor, _ := configureTestCodeExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if r.URL.Path != "/execute" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-worker-credential" {
			t.Errorf("unexpected authenticated request: method=%s path=%s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["code"] != "raise ValueError('original')" {
			t.Errorf("code changed: %#v err=%v", body, err)
		}
		_, _ = fmt.Fprint(w, `{"stdout":"before failure\n","stderr":"ValueError: original\n","return_code":1}`)
	})
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	result, err := executor.Execute(context.Background(), "raise ValueError('original')")
	if err != nil || result.ReturnCode != 1 || result.Stdout != "before failure\n" || result.Stderr != "ValueError: original\n" || result.ErrorCode != "" || called.Load() != 1 {
		t.Fatalf("actual failed execution was not preserved: result=%#v err=%v calls=%d", result, err, called.Load())
	}
}

func TestCodeExecutorRejectsInvalidEndpointsAndCredentials(t *testing.T) {
	for _, endpoint := range []string{"https://example.com", "http://localhost:12346", "file:///execute", "http://127.0.0.1:12@evil.example/execute", "http://127.0.0.1:12346/other", "http://127.0.0.1:12346/execute?q=x", "http://127.0.0.1:12346/#x"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Setenv("KIRO_CODE_EXECUTOR_URL", endpoint)
			t.Setenv("KIRO_CODE_EXECUTOR_TOKEN_FILE", "")
			if executor, err := configuredCodeExecutor(); err == nil || executor != nil {
				t.Fatalf("endpoint unexpectedly accepted: %#v %v", executor, err)
			}
		})
	}
	t.Setenv("KIRO_CODE_EXECUTOR_URL", "")
	if executor, err := configuredCodeExecutor(); executor != nil || err != nil {
		t.Fatalf("unconfigured client = %#v, %v", executor, err)
	}
}

func TestCodeExecutorResponseAndRequestBounds(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		wantCode   string
	}{
		{"missing status", `{"stdout":"","stderr":""}`, 200, "unavailable"},
		{"trailing JSON", `{"stdout":"","stderr":"","return_code":0}{}`, 200, "unavailable"},
		{"oversized", strings.Repeat(" ", maxCodeExecutionResponseBytes+1), 200, "unavailable"},
		{"busy", `{"stdout":"","stderr":"busy","return_code":-1,"error_code":"too_many_requests"}`, 429, "too_many_requests"},
		{"timeout", `{"stdout":"partial","stderr":"timed out","return_code":-9,"error_code":"execution_timeout"}`, 504, "execution_timeout"},
		{"transport failure", `{"stdout":"","stderr":"runtime unavailable","return_code":-1}`, 502, "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, _ := configureTestCodeExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = fmt.Fprint(w, test.body)
			})
			result, err := executor.Execute(context.Background(), "print(1)")
			if err == nil || result.ErrorCode != test.wantCode {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
	var calls atomic.Int32
	executor, _ := configureTestCodeExecutor(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, code := range []string{"", strings.Repeat("x", maxCodeExecutionCodeBytes+1)} {
		result, err := executor.Execute(context.Background(), code)
		if err == nil || result.ErrorCode != "invalid_tool_input" || calls.Load() != 0 {
			t.Fatalf("oversized/empty input reached worker: %#v %v calls=%d", result, err, calls.Load())
		}
	}
}

func TestCodeExecutorDoesNotFollowRedirectsAndHonorsCancellation(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	executor, _ := configureTestCodeExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	if _, err := executor.Execute(context.Background(), "print(1)"); err == nil || targetCalls.Load() != 0 {
		t.Fatalf("redirect followed or accepted: err=%v target_calls=%d", err, targetCalls.Load())
	}
	executor, _ = configureTestCodeExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := executor.Execute(ctx, "print(1)")
	if err == nil || result.ErrorCode != "execution_timeout" {
		t.Fatalf("deadline not preserved: %#v %v", result, err)
	}
}

func TestCodeExecutionActualResultFeedsFinalModelAnswer(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			calls, executions := 0, 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if calls == 1 {
					callback.OnToolUse(KiroToolUse{ToolUseID: "call-native", Name: codeExecutionToolName, Input: map[string]interface{}{"code": "print(7 * 13)"}})
					callback.OnStopReason("TOOL_USE")
				} else {
					encoded, _ := json.Marshal(payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext)
					if !strings.Contains(string(encoded), `91`) || !strings.Contains(string(encoded), "return_code") {
						t.Errorf("actual result missing from model feedback: %s", encoded)
					}
					callback.OnText("The executed calculation returned 91.", false)
					callback.OnStopReason("END_TURN")
				}
				callback.OnComplete(10, 5)
				return nil
			})
			h.codeExecutor = codeExecutorFunc(func(_ context.Context, code string) (CodeExecutionResult, error) {
				executions++
				if code != "print(7 * 13)" {
					t.Errorf("code=%q", code)
				}
				return CodeExecutionResult{Stdout: "91\n", ReturnCode: 0}, nil
			})
			req := &ClaudeRequest{Model: "claude-opus-4.8", Stream: stream, MaxTokens: 512, Messages: []ClaudeMessage{{Role: "user", Content: "Run a calculation."}}, Tools: []ClaudeTool{{Type: codeExecutionToolType, Name: codeExecutionToolName}}}
			rec := httptest.NewRecorder()
			h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 10, "", nil)
			if rec.Code != 200 || calls != 2 || executions != 1 {
				t.Fatalf("status=%d model_calls=%d executions=%d body=%s", rec.Code, calls, executions, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `code_execution_result`) || !strings.Contains(rec.Body.String(), "91") || strings.Contains(rec.Body.String(), "too_many_requests") || strings.Contains(rec.Body.String(), codeExecutionUnavailableText) {
				t.Fatalf("actual execution result missing: %s", rec.Body.String())
			}
			if !stream {
				var response ClaudeResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Content) != 3 || response.Content[2].Text != "The executed calculation returned 91." || response.StopReason != "end_turn" {
					t.Fatalf("response=%#v", response)
				}
			}
		})
	}
}

func TestCodeExecutionFailureFeedsRealErrorAndStopsFurtherExecutions(t *testing.T) {
	for _, runtimeFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtime=%v", runtimeFailure), func(t *testing.T) {
			calls, executions := 0, 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if calls == 1 {
					callback.OnToolUse(KiroToolUse{ToolUseID: "call-fail", Name: codeExecutionToolName, Input: map[string]interface{}{"code": "print(1/0)"}})
					callback.OnStopReason("TOOL_USE")
				} else {
					if payload.ToolChoiceMode != "none" {
						t.Errorf("failure allowed another execution: %s", payload.ToolChoiceMode)
					}
					encoded, _ := json.Marshal(payload.ConversationState.CurrentMessage.UserInputMessage)
					if !strings.Contains(string(encoded), "ZeroDivisionError") {
						t.Errorf("real failure missing: %s", encoded)
					}
					callback.OnText("Execution failed with ZeroDivisionError.", false)
					callback.OnStopReason("END_TURN")
				}
				callback.OnComplete(10, 5)
				return nil
			})
			h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
				executions++
				result := CodeExecutionResult{Stderr: "ZeroDivisionError", ReturnCode: 1}
				if runtimeFailure {
					result.ErrorCode = "execution_timeout"
					return result, errors.New("isolated runtime failed")
				}
				return result, nil
			})
			req := &ClaudeRequest{Model: "claude-opus-4.8", Messages: []ClaudeMessage{{Role: "user", Content: "Run."}}, Tools: []ClaudeTool{{Type: codeExecutionToolType, Name: codeExecutionToolName}}}
			rec := httptest.NewRecorder()
			h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 10, "", nil)
			if rec.Code != 200 || calls != 2 || executions != 1 || !strings.Contains(rec.Body.String(), "ZeroDivisionError") {
				t.Fatalf("status=%d calls=%d executions=%d body=%s", rec.Code, calls, executions, rec.Body.String())
			}
			if runtimeFailure && !strings.Contains(rec.Body.String(), "execution_time_exceeded") {
				t.Fatalf("runtime error lost: %s", rec.Body.String())
			}
			if !runtimeFailure && !strings.Contains(rec.Body.String(), `"return_code":1`) {
				t.Fatalf("nonzero exit misreported: %s", rec.Body.String())
			}
		})
	}
}

func TestCodeExecutionAutoCanAnswerWithoutRunning(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		if payload.ToolChoiceMode != "auto" {
			t.Errorf("choice=%s", payload.ToolChoiceMode)
		}
		callback.OnText("No execution is needed.", false)
		callback.OnStopReason("END_TURN")
		callback.OnComplete(5, 5)
		return nil
	})
	h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
		t.Error("auto text answer unexpectedly executed code")
		return CodeExecutionResult{}, nil
	})
	req := &ClaudeRequest{Model: "claude-opus-4.8", Messages: []ClaudeMessage{{Role: "user", Content: "What does Python mean?"}}, Tools: []ClaudeTool{{Type: codeExecutionToolTypeLatest, Name: codeExecutionToolName}}}
	rec := httptest.NewRecorder()
	h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 5, "", nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"type":"server_tool_use"`) || !strings.Contains(rec.Body.String(), "No execution is needed.") {
		t.Fatalf("response=%s", rec.Body.String())
	}
}

func TestCodeExecutionRoundLimit(t *testing.T) {
	calls, executions := 0, 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls <= maxCodeExecutionRounds {
			callback.OnToolUse(KiroToolUse{ToolUseID: fmt.Sprintf("call-%d", calls), Name: codeExecutionToolName, Input: map[string]interface{}{"code": "print(1)"}})
			callback.OnStopReason("TOOL_USE")
		} else {
			if payload.ToolChoiceMode != "none" {
				t.Errorf("round limit did not disable tools: %s", payload.ToolChoiceMode)
			}
			callback.OnText("The execution round limit was reached.", false)
			callback.OnStopReason("END_TURN")
		}
		callback.OnComplete(5, 5)
		return nil
	})
	h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
		executions++
		return CodeExecutionResult{Stdout: "1\n"}, nil
	})
	req := &ClaudeRequest{Model: "claude-opus-4.8", Messages: []ClaudeMessage{{Role: "user", Content: "Run."}}, Tools: []ClaudeTool{{Type: codeExecutionToolType, Name: codeExecutionToolName}}}
	rec := httptest.NewRecorder()
	h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 5, "", nil)
	if rec.Code != 200 || calls != 4 || executions != 3 || !strings.Contains(rec.Body.String(), "round limit") {
		t.Fatalf("status=%d calls=%d executions=%d body=%s", rec.Code, calls, executions, rec.Body.String())
	}
}

func TestCodeExecutionNoneNeverInvokesWorker(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		toolContext := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
		if payload.ToolChoiceMode != "none" || (toolContext != nil && len(toolContext.Tools) != 0) {
			t.Errorf("none retained executable tools: mode=%s context=%#v", payload.ToolChoiceMode, toolContext)
		}
		callback.OnText("Execution is disabled for this request.", false)
		callback.OnStopReason("END_TURN")
		callback.OnComplete(5, 5)
		return nil
	})
	h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
		t.Error("tool_choice none invoked worker")
		return CodeExecutionResult{}, nil
	})
	body := `{"model":"claude-opus-4.8","max_tokens":512,"messages":[{"role":"user","content":"Do not execute code."}],"tools":[{"type":"code_execution_20250522","name":"code_execution"}],"tool_choice":{"type":"none"}}`
	rec := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"type":"server_tool_use"`) || !strings.Contains(rec.Body.String(), "Execution is disabled") {
		t.Fatalf("none response=%s", rec.Body.String())
	}
}

func TestCodeExecutionWorkerOutputIsNotCountedAsModelTokens(t *testing.T) {
	tool := KiroToolUse{Name: codeExecutionToolName, Input: map[string]interface{}{"code": "print('x')"}}
	short := codeExecutionResultBlocks(tool, CodeExecutionResult{Stdout: "x"})
	long := codeExecutionResultBlocks(tool, CodeExecutionResult{Stdout: strings.Repeat("x", 32000)})
	if estimateCodeExecutionOutputTokens(short) != estimateCodeExecutionOutputTokens(long) {
		t.Fatal("worker output inflated model output token accounting")
	}
}

func TestCodeExecutorEscapedOutputBoundary(t *testing.T) {
	output := strings.Repeat("\x00", maxCodeExecutionOutputBytes)
	executor, _ := configureTestCodeExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(CodeExecutionResult{Stdout: output, Stderr: output})
	})
	result, err := executor.Execute(context.Background(), "print('boundary')")
	if err != nil || result.Stdout != output || result.Stderr != output {
		t.Fatalf("legal escaped output was rejected: stdout=%d stderr=%d err=%v", len(result.Stdout), len(result.Stderr), err)
	}
	executor, _ = configureTestCodeExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(CodeExecutionResult{Stdout: strings.Repeat("x", maxCodeExecutionOutputBytes+1)})
	})
	if _, err := executor.Execute(context.Background(), "print('oversized')"); err == nil {
		t.Fatal("decoded output bound was not enforced")
	}
}

func TestCodeExecutionWorkerErrorAliases(t *testing.T) {
	for _, test := range []struct{ internal, public string }{
		{"executor_busy", "too_many_requests"}, {"code_too_large", "invalid_tool_input"}, {"request_too_large", "invalid_tool_input"}, {"execution_timeout", "execution_time_exceeded"}, {"runtime_error", "unavailable"},
	} {
		if got := publicCodeExecutionError(test.internal); got != test.public {
			t.Errorf("%s => %s want %s", test.internal, got, test.public)
		}
	}
}

func TestCodeExecutionConsumedRoundsSettleOnAbort(t *testing.T) {
	for _, mode := range []string{"worker cancellation", "second model failure", "second preflight failure", "retry then cancellation"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if mode == "retry then cancellation" && calls == 1 {
					callback.OnComplete(7, 3)
					callback.OnCredits(.75)
					return newUpstreamEmptyResponseError()
				}
				if mode == "second model failure" && calls == 2 {
					return &KiroHTTPError{StatusCode: 400, Endpoint: "fixture", Body: "invalid request"}
				}
				callback.OnToolUse(KiroToolUse{ToolUseID: "bill-native", Name: codeExecutionToolName, Input: map[string]interface{}{"code": "print(1)"}})
				callback.OnCredits(1.25)
				callback.OnComplete(7, 3)
				callback.OnStopReason("TOOL_USE")
				return nil
			})
			key, err := config.AddApiKey(config.ApiKeyEntry{Name: "execution accounting", Key: "sk-execution-accounting-fixture", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
				if mode == "worker cancellation" || mode == "retry then cancellation" {
					cancel()
					return CodeExecutionResult{ReturnCode: -1, ErrorCode: "execution_cancelled"}, context.Canceled
				}
				if mode == "second preflight failure" {
					t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "10")
				}
				return CodeExecutionResult{Stdout: "1\n"}, nil
			})
			req := &ClaudeRequest{Model: "claude-opus-4.8", Messages: []ClaudeMessage{{Role: "user", Content: "Run."}}, Tools: []ClaudeTool{{Type: codeExecutionToolType, Name: codeExecutionToolName}}}
			rec := httptest.NewRecorder()
			h.handleCodeExecutionRequest(rec, ctx, req, req.Model, 5, key.ID, nil)
			wantCredits, wantRequests := 1.25, int64(1)
			if mode == "retry then cancellation" {
				wantCredits, wantRequests = 2, int64(2)
			}
			gotKey := config.GetApiKeyEntry(key.ID)
			if h.totalCredits != wantCredits || gotKey.CreditsUsed != wantCredits || gotKey.RequestsCount != wantRequests || gotKey.TokensUsed <= 0 || atomic.LoadInt64(&h.totalTokens) <= 0 {
				t.Fatalf("consumed usage lost/doubled: credits=%v requests=%d tokens=%d key=%+v", h.totalCredits, h.totalRequests, h.totalTokens, gotKey)
			}
			accounts := h.pool.GetAllAccounts()
			if len(accounts) != 1 || accounts[0].TotalCredits != wantCredits || int64(accounts[0].RequestCount) != wantRequests {
				t.Fatalf("pool settlement duplicated: %+v", accounts)
			}
			if mode == "worker cancellation" || mode == "retry then cancellation" {
				if rec.Body.Len() != 0 {
					t.Fatalf("cancelled output published: %s", rec.Body.String())
				}
			} else if rec.Code == 200 {
				t.Fatalf("expected failure: %s", rec.Body.String())
			}
		})
	}
}

func TestCodeExecutionSuccessDoesNotDoubleSettle(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			callback.OnToolUse(KiroToolUse{ToolUseID: "bill-success", Name: codeExecutionToolName, Input: map[string]interface{}{"code": "print(1)"}})
			callback.OnCredits(1.25)
			callback.OnStopReason("TOOL_USE")
		} else {
			callback.OnText("Actual result: 1.", false)
			callback.OnCredits(.5)
			callback.OnStopReason("END_TURN")
		}
		callback.OnComplete(7, 3)
		return nil
	})
	key, err := config.AddApiKey(config.ApiKeyEntry{Name: "success accounting", Key: "sk-success-accounting-fixture", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
		return CodeExecutionResult{Stdout: "1\n"}, nil
	})
	req := &ClaudeRequest{Model: "claude-opus-4.8", Messages: []ClaudeMessage{{Role: "user", Content: "Run."}}, Tools: []ClaudeTool{{Type: codeExecutionToolType, Name: codeExecutionToolName}}}
	rec := httptest.NewRecorder()
	h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 5, key.ID, nil)
	gotKey := config.GetApiKeyEntry(key.ID)
	if rec.Code != 200 || calls != 2 || h.totalCredits != 1.75 || h.successRequests != 1 || gotKey.CreditsUsed != 1.75 || gotKey.RequestsCount != 1 {
		t.Fatalf("successful chain double-settled: credits=%v success=%d key=%+v body=%s", h.totalCredits, h.successRequests, gotKey, rec.Body.String())
	}
	accounts := h.pool.GetAllAccounts()
	if len(accounts) != 1 || accounts[0].RequestCount != 2 || accounts[0].TotalCredits != 1.75 {
		t.Fatalf("pool double-settled: %+v", accounts)
	}
}

func TestCodeExecutionPreservesFinalStructuredSchema(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, persistent := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/persistent=%v", stream, persistent), func(t *testing.T) {
				calls, executions := 0, 0
				h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
					calls++
					if !payload.StructuredOutput {
						t.Error("schema contract discarded")
					}
					if calls == 1 {
						callback.OnToolUse(KiroToolUse{ToolUseID: "schema-exec", Name: codeExecutionToolName, Input: map[string]interface{}{"code": "print(91)"}})
						callback.OnStopReason("TOOL_USE")
					} else if calls == 2 || persistent {
						callback.OnText(`{"answer":"wrong"}`, false)
						callback.OnStopReason("END_TURN")
					} else {
						if payload.ToolChoiceMode != "none" || !strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "structured_output_retry") {
							t.Error("format retry granted execution or lost correction")
						}
						callback.OnText("```json\n{\"answer\":91}\n```", false)
						callback.OnStopReason("END_TURN")
					}
					callback.OnCredits(.25)
					callback.OnComplete(7, 3)
					return nil
				})
				h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
					executions++
					return CodeExecutionResult{Stdout: "91\n"}, nil
				})
				key, err := config.AddApiKey(config.ApiKeyEntry{Name: "schema accounting", Key: "sk-schema-accounting-fixture", Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				req := &ClaudeRequest{Model: "claude-opus-4.8", Stream: stream, Messages: []ClaudeMessage{{Role: "user", Content: "Run and return JSON."}}, Tools: []ClaudeTool{{Type: codeExecutionToolType, Name: codeExecutionToolName}}, OutputConfig: &ClaudeOutputConfig{Format: &ClaudeOutputFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer","const":91}},"required":["answer"],"additionalProperties":false}`)}}}
				rec := httptest.NewRecorder()
				h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 5, key.ID, nil)
				if calls != 3 || executions != 1 || strings.Contains(rec.Body.String(), "wrong") {
					t.Fatalf("schema retry/execution limit mismatch calls=%d executions=%d body=%s", calls, executions, rec.Body.String())
				}
				if persistent {
					if rec.Code != 502 || !strings.Contains(rec.Body.String(), "upstream_structured_output_error") || strings.Contains(rec.Body.String(), "event: message_start") {
						t.Fatalf("invalid schema published: %s", rec.Body.String())
					}
				} else {
					text, stop := structuredContractResponse(t, rec, stream)
					if rec.Code != 200 || text != `{"answer":91}` || stop != "end_turn" {
						t.Fatalf("schema lost: status=%d text=%q stop=%q", rec.Code, text, stop)
					}
				}
				if h.totalCredits != .75 || config.GetApiKeyEntry(key.ID).CreditsUsed != .75 {
					t.Fatalf("schema retries double-settled credits: global=%v key=%+v", h.totalCredits, config.GetApiKeyEntry(key.ID))
				}
				accounts := h.pool.GetAllAccounts()
				if len(accounts) != 1 || accounts[0].RequestCount != 3 || accounts[0].TotalCredits != .75 {
					t.Fatalf("schema retry pool duplicated: %+v", accounts)
				}
			})
		}
	}
}

func TestCodeExecutionStructuredJSONKeepsLiteralThinkingTags(t *testing.T) {
	const literal = `{"text":"<thinking>literal data</thinking>"}`
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			callback.OnToolUse(KiroToolUse{ToolUseID: "literal-exec", Name: codeExecutionToolName, Input: map[string]interface{}{"code": "print('literal data')"}})
			callback.OnStopReason("TOOL_USE")
		} else {
			callback.OnText(literal, false)
			callback.OnStopReason("END_TURN")
		}
		callback.OnComplete(7, 3)
		return nil
	})
	h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
		return CodeExecutionResult{Stdout: "literal data\n"}, nil
	})
	req := &ClaudeRequest{Model: "claude-opus-4.8", Messages: []ClaudeMessage{{Role: "user", Content: "Execute and return literal tags in JSON."}}, Tools: []ClaudeTool{{Type: codeExecutionToolType, Name: codeExecutionToolName}}, OutputConfig: &ClaudeOutputConfig{Format: &ClaudeOutputFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","const":"<thinking>literal data</thinking>"}},"required":["text"]}`)}}}
	rec := httptest.NewRecorder()
	h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 5, "", nil)
	text, stop := structuredContractResponse(t, rec, false)
	if rec.Code != 200 || calls != 2 || text != literal || stop != "end_turn" {
		t.Fatalf("literal JSON changed: status=%d calls=%d text=%q body=%s", rec.Code, calls, text, rec.Body.String())
	}
}

func TestCodeExecutionAutoWithoutExecutionStillHonorsOriginalSchema(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, invalidFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/invalid_first=%v", stream, invalidFirst), func(t *testing.T) {
				calls, executions := 0, 0
				h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
					calls++
					if calls == 1 && invalidFirst {
						callback.OnText(`{"answer":"invalid"}`, false)
					} else {
						if calls > 1 && (payload.ToolChoiceMode != "none" || !strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "structured_output_retry")) {
							t.Error("schema-only correction regained tool execution")
						}
						callback.OnText(`{"answer":42}`, false)
					}
					callback.OnComplete(7, 3)
					callback.OnStopReason("END_TURN")
					return nil
				})
				h.codeExecutor = codeExecutorFunc(func(context.Context, string) (CodeExecutionResult, error) {
					executions++
					t.Error("auto final answer unexpectedly executed code")
					return CodeExecutionResult{}, nil
				})
				req := &ClaudeRequest{Model: "claude-opus-4.8", Stream: stream, Messages: []ClaudeMessage{{Role: "user", Content: "Return the known answer as JSON; no code is needed."}}, Tools: []ClaudeTool{{Type: codeExecutionToolType, Name: codeExecutionToolName}}, OutputConfig: &ClaudeOutputConfig{Format: &ClaudeOutputFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer","const":42}},"required":["answer"],"additionalProperties":false}`)}}}
				rec := httptest.NewRecorder()
				h.handleCodeExecutionRequest(rec, context.Background(), req, req.Model, 5, "", nil)
				wantCalls := 1
				if invalidFirst {
					wantCalls = 2
				}
				text, stop := structuredContractResponse(t, rec, stream)
				if rec.Code != 200 || calls != wantCalls || executions != 0 || text != `{"answer":42}` || stop != "end_turn" || strings.Contains(rec.Body.String(), `"type":"server_tool_use"`) || strings.Contains(rec.Body.String(), "invalid") {
					t.Fatalf("original schema lost: status=%d calls=%d executions=%d text=%q body=%s", rec.Code, calls, executions, text, rec.Body.String())
				}
			})
		}
	}
}
