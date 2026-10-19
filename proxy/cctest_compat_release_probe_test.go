package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

// Compile this fixture unchanged against the production baseline and release.
// It validates real protocol paths rather than recognizing a detection site.
func TestCCTestCompatReleaseProbe(t *testing.T) {
	mode := os.Getenv("CCTEST_COMPAT_PROBE_MODE")
	if mode == "" {
		mode = "MODIFIED"
	}
	wantFixed := mode == "MODIFIED"
	aliasOK, executionOK := false, false
	t.Run("legacy_schema_alias", func(t *testing.T) {
		h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
			text := "Blue, 7."
			if payload.StructuredOutput {
				text = `{"color":"blue","count":7}`
			}
			callback.OnText(text, false)
			callback.OnStopReason("END_TURN")
			callback.OnComplete(10, 8)
			return nil
		})
		body := `{"model":"claude-opus-4-8","max_tokens":512,"messages":[{"role":"user","content":"Return blue and count 7."}],"output_format":{"type":"json_schema","schema":{"type":"object","properties":{"color":{"type":"string","enum":["blue"]},"count":{"type":"integer","const":7}},"required":["color","count"],"additionalProperties":false}}}`
		rec := httptest.NewRecorder()
		h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		var response ClaudeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		aliasOK = len(response.Content) == 1 && response.Content[0].Text == `{"color":"blue","count":7}`
	})
	t.Run("authenticated_execution_result", func(t *testing.T) {
		workerCalls := 0
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			workerCalls++
			if request.URL.Path != "/execute" || request.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Errorf("executor authentication or path mismatch")
			}
			var input struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Code != "print(7*13)" {
				t.Errorf("executor input mismatch")
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"stdout": fmt.Sprintf("%d\n", 7*13), "stderr": "", "return_code": 0})
		}))
		defer worker.Close()
		tokenFile := filepath.Join(t.TempDir(), "worker-token")
		if err := os.WriteFile(tokenFile, []byte("fixture-token\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KIRO_CODE_EXECUTOR_URL", worker.URL)
		t.Setenv("KIRO_CODE_EXECUTOR_TOKEN_FILE", tokenFile)
		modelCalls := 0
		h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
			modelCalls++
			if modelCalls == 1 {
				callback.OnToolUse(KiroToolUse{ToolUseID: "execution-probe", Name: "code_execution", Input: map[string]interface{}{"code": "print(7*13)"}})
				callback.OnStopReason("TOOL_USE")
			} else {
				encoded, _ := json.Marshal(payload.ConversationState.CurrentMessage.UserInputMessage)
				if !strings.Contains(string(encoded), "91") {
					t.Error("actual worker output was not fed back to the model")
				}
				callback.OnText("The executed result is 91.", false)
				callback.OnStopReason("END_TURN")
			}
			callback.OnComplete(10, 8)
			return nil
		})
		body := `{"model":"claude-opus-4-8","max_tokens":512,"messages":[{"role":"user","content":"Run Python to print 7*13."}],"tools":[{"type":"code_execution_20250522","name":"code_execution"}],"tool_choice":{"type":"auto"}}`
		rec := httptest.NewRecorder()
		h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		var response ClaudeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		for _, block := range response.Content {
			if block.Type == "code_execution_tool_result" {
				result, _ := block.Content.(map[string]interface{})
				executionOK = workerCalls == 1 && modelCalls == 2 && result["type"] == "code_execution_result" && result["stdout"] == "91\n" && result["return_code"] == float64(0)
			}
		}
	})
	if aliasOK != wantFixed || executionOK != wantFixed {
		t.Fatalf("mode=%s structured_alias=%v server_execution=%v expected=%v", mode, aliasOK, executionOK, wantFixed)
	}
	fmt.Printf("%s: structured_alias=%v server_execution=%v stdout_verified=%v exit=0\n", mode, aliasOK, executionOK, executionOK)
}
