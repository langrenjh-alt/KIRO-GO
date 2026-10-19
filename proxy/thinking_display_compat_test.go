package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestClaudeThinkingDisplayCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, kind, display, defaultFormat, wantFormat string
		wantOmit                                       bool
	}{
		{"unset preserves configured format", "adaptive", "", "think", "think", false},
		{"summarized", "adaptive", "summarized", "think", "thinking", false},
		{"summarized ignores case and whitespace", "enabled", " SUMMARIZED ", "think", "thinking", false},
		{"omitted", "adaptive", "omitted", "think", "thinking", true},
		{"omitted ignores case and whitespace", "enabled", " OMITTED ", "reasoning_content", "thinking", true},
		{"updates beta hides unclassified reasoning", "adaptive", "updates", "think", "thinking", true},
		{"updates ignores case and whitespace", "enabled", " UPDATES ", "think", "thinking", true},
		{"legacy visible uses configured format", "adaptive", "visible", "think", "think", false},
		{"legacy full uses configured format", "enabled", "full", "thinking", "thinking", false},
		{"legacy auto uses configured format", "adaptive", "auto", "reasoning_content", "reasoning_content", false},
		{"future optional value uses configured format", "enabled", "future-client-display", "think", "think", false},
		{"whitespace uses default format", "adaptive", " \t\n", "", "thinking", false},
		{"disabled ignores summarized", "disabled", "summarized", "think", "think", false},
		{"disabled ignores omitted", "disabled", "omitted", "think", "think", false},
		{"disabled ignores updates", "disabled", "updates", "reasoning_content", "reasoning_content", false},
		{"disabled ignores future value", "disabled", "future-client-display", "", "thinking", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thinking := &ClaudeThinkingConfig{Type: tc.kind, Display: tc.display}
			if tc.kind == "enabled" {
				thinking.BudgetTokens = 2048
			}
			original := *thinking
			if msg := validateClaudeThinkingConfig(thinking, 4096); msg != "" {
				t.Fatalf("optional display rejected: %s", msg)
			}
			opts := resolveClaudeThinkingResponseOptions(thinking, tc.defaultFormat)
			if opts.Format != tc.wantFormat || opts.OmitDisplay != tc.wantOmit {
				t.Fatalf("options=%+v, want format=%q omit=%v", opts, tc.wantFormat, tc.wantOmit)
			}
			if !reflect.DeepEqual(*thinking, original) {
				t.Fatalf("validation/response normalization mutated the original thinking config")
			}
			if got := isClaudeThinkingRequested(thinking); got != (tc.kind != "disabled") {
				t.Fatalf("display changed the requested thinking mode: requested=%v", got)
			}
		})
	}
}

func TestClaudeThinkingDisplayDoesNotWeakenRequiredValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  ClaudeThinkingConfig
		max  int
		want string
	}{
		{"missing type", ClaudeThinkingConfig{Display: "updates"}, 4096, "thinking.type"},
		{"invalid type", ClaudeThinkingConfig{Type: "future", Display: "updates"}, 4096, "thinking.type"},
		{"missing enabled budget", ClaudeThinkingConfig{Type: "enabled", Display: "visible"}, 4096, "thinking.budget_tokens"},
		{"negative enabled budget", ClaudeThinkingConfig{Type: "enabled", BudgetTokens: -1, Display: "updates"}, 4096, "thinking.budget_tokens"},
		{"zero enabled max tokens", ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048, Display: "updates"}, 0, "max_tokens=0"},
		{"adaptive budget", ClaudeThinkingConfig{Type: "adaptive", BudgetTokens: 2048, Display: "visible"}, 4096, "thinking.budget_tokens"},
		{"disabled budget", ClaudeThinkingConfig{Type: "disabled", BudgetTokens: 2048, Display: "summarized"}, 4096, "thinking.budget_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateClaudeThinkingConfig(&tc.cfg, tc.max); !strings.Contains(got, tc.want) {
				t.Fatalf("error=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestClaudeThinkingDisplayCountTokensHTTPCompatibility(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	var tokens int
	for _, kind := range []string{"adaptive", "enabled", "disabled"} {
		for _, display := range []string{"summarized", "omitted", "updates", "visible", "future-client-display", " \t "} {
			t.Run(kind+"/"+display, func(t *testing.T) {
				thinking := ClaudeThinkingConfig{Type: kind, Display: display}
				if kind == "enabled" {
					thinking.BudgetTokens = 2048
				}
				raw, err := json.Marshal(ClaudeRequest{
					Model: "claude-sonnet-4.6", MaxTokens: 4096, Thinking: &thinking,
					Messages: []ClaudeMessage{{Role: "user", Content: "compatibility fixture"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				h.handleCountTokens(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(string(raw))))
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				var result map[string]int
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result["input_tokens"] <= 0 {
					t.Fatalf("invalid token count response: %s err=%v", rec.Body.String(), err)
				}
				if tokens == 0 {
					tokens = result["input_tokens"]
				} else if result["input_tokens"] != tokens {
					t.Fatalf("decorative display/type inflated public input count: got=%d want=%d", result["input_tokens"], tokens)
				}
			})
		}
	}
}

func TestClaudeThinkingDisplayInvalidJSONTypeRejectedByBothHTTPRoutes(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	for _, display := range []string{"true", "42", "{}", "[]"} {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			t.Run(path+"/"+display, func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"claude-sonnet-4.6","max_tokens":4096,"thinking":{"type":"adaptive","display":%s},"messages":[{"role":"user","content":"fixture"}]}`, display)
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				if path == "/v1/messages/count_tokens" {
					h.handleCountTokens(rec, req)
				} else {
					h.handleClaudeMessagesInternal(rec, req)
				}
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request_error") {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestClaudeThinkingDisplayHTTPPreservesGenerationAndOmission(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, toolTurn := range []bool{false, true} {
			for _, tc := range []struct {
				kind, display string
				wantThinking  bool
				wantOmit      bool
			}{
				{"adaptive", "summarized", true, false},
				{"adaptive", "omitted", true, true},
				{"adaptive", "updates", true, true},
				{"adaptive", " UPDATES ", true, true},
				{"enabled", "visible", true, false},
				{"enabled", "future-client-display", true, false},
				{"disabled", "summarized", false, false},
				{"disabled", "updates", false, false},
			} {
				t.Run(fmt.Sprintf("stream=%v/tool=%v/%s/%s", stream, toolTurn, tc.kind, tc.display), func(t *testing.T) {
					var calls int
					h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
						calls++
						raw, err := json.Marshal(payload)
						if err != nil {
							t.Fatal(err)
						}
						if got := strings.Contains(string(raw), "thinking_mode"); got != tc.wantThinking {
							t.Fatalf("upstream thinking=%v want=%v", got, tc.wantThinking)
						}
						if tc.kind == "enabled" && !strings.Contains(string(raw), "2048") {
							t.Fatal("optional display changed the requested thinking budget")
						}
						callback.OnReasoning(KiroReasoningContentEvent{Text: "fixture reasoning summary", Signature: "fixture-native-signature"})
						if toolTurn {
							callback.OnToolUse(KiroToolUse{ToolUseID: "toolu_fixture", Name: "fixture_tool", Input: map[string]interface{}{"value": "kept"}})
						} else {
							callback.OnText("fixture final answer", false)
						}
						callback.OnComplete(20, 10)
						return nil
					})
					thinking := ClaudeThinkingConfig{Type: tc.kind, Display: tc.display}
					if tc.kind == "enabled" {
						thinking.BudgetTokens = 2048
					}
					request := ClaudeRequest{
						Model: "claude-sonnet-4.6", MaxTokens: 4096, Stream: stream, Thinking: &thinking,
						Messages: []ClaudeMessage{{Role: "user", Content: "compatibility fixture"}},
					}
					if toolTurn {
						request.Tools = []ClaudeTool{{Name: "fixture_tool", Description: "fixture", InputSchema: map[string]interface{}{"type": "object"}}}
					}
					raw, err := json.Marshal(request)
					if err != nil {
						t.Fatal(err)
					}
					rec := httptest.NewRecorder()
					h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(raw))))
					body := rec.Body.String()
					if rec.Code != http.StatusOK || calls != 1 {
						t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, body)
					}
					if toolTurn {
						if !strings.Contains(body, "fixture_tool") || !strings.Contains(body, "kept") {
							t.Fatalf("display lost tool output: %s", body)
						}
					} else if !strings.Contains(body, "fixture final answer") {
						t.Fatalf("display lost normal text output: %s", body)
					}
					if got := strings.Contains(body, "fixture reasoning summary"); got != (tc.wantThinking && !tc.wantOmit) {
						t.Fatalf("thinking text present=%v want=%v body=%s", got, tc.wantThinking && !tc.wantOmit, body)
					}
					if tc.wantThinking && !strings.Contains(body, "fixture-native-signature") {
						t.Fatalf("display lost the thinking signature needed for continuation: %s", body)
					}
					if !tc.wantThinking && strings.Contains(body, "fixture-native-signature") {
						t.Fatalf("disabled thinking leaked an upstream reasoning signature: %s", body)
					}
				})
			}
		}
	}
}
