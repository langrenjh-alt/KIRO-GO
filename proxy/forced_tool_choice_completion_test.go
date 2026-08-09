package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestForcedToolChoiceTerminalValidation(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		selected  string
		observed  []string
		stop      string
		wantError bool
		wantTool  bool
	}{
		{name: "required without tool", mode: "required", stop: "END_TURN", wantError: true, wantTool: true},
		{name: "required with tool", mode: "required", observed: []string{"lookup"}, stop: "TOOL_USE"},
		{name: "required with unknown tool", mode: "required", observed: []string{"unknown"}, stop: "TOOL_USE", wantError: true, wantTool: true},
		{name: "specific correct tool", mode: "specific", selected: "lookup", observed: []string{"lookup"}, stop: "TOOL_USE"},
		{name: "specific wrong tool", mode: "specific", selected: "lookup", observed: []string{"other"}, stop: "TOOL_USE", wantError: true, wantTool: true},
		{name: "specific mixed tools", mode: "specific", selected: "lookup", observed: []string{"lookup", "other"}, stop: "TOOL_USE", wantError: true, wantTool: true},
		{name: "max tokens takes precedence", mode: "specific", selected: "lookup", observed: []string{"lookup"}, stop: "MAX_TOKENS", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := &KiroPayload{ToolChoiceMode: test.mode, ToolChoiceName: test.selected}
			if test.name == "required with unknown tool" {
				tool := KiroToolWrapper{}
				tool.ToolSpecification.Name = "lookup"
				payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{Tools: []KiroToolWrapper{tool}}
			}
			err := agentCompletionResponseError(payload, false, "unvalidated prose", false, test.observed, test.stop)
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v, wantError=%v", err, test.wantError)
			}
			if test.wantTool && !isUpstreamAgentToolChoiceMismatchError(err) {
				t.Fatalf("error=%v, want tool-choice mismatch", err)
			}
			if test.name == "max tokens takes precedence" && (!isUpstreamAgentMarkerMismatchError(err) || isUpstreamAgentToolChoiceMismatchError(err)) {
				t.Fatalf("max-token error did not retain precedence: %v", err)
			}
		})
	}
}

func TestForcedToolChoiceGateHoldsAndDiscardsProse(t *testing.T) {
	payload := &KiroPayload{
		ToolChoiceMode:        "required",
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
	}
	gate := newAgentCompletionGateForPayload(payload)
	if got := gate.Push("I will call the tool."); got != "" {
		t.Fatalf("forced prose escaped before a tool call: %q", got)
	}
	if got := gate.VisibleText(); got != "I will call the tool." {
		t.Fatalf("forced gate lost validation evidence: %q", got)
	}
	if got := gate.CommitPendingForTool(); got != "" || gate.VisibleText() != "" {
		t.Fatalf("forced prose was not discarded for the tool turn: released=%q visible=%q", got, gate.VisibleText())
	}

	gate = newAgentCompletionGateForPayload(payload)
	gate.Push("plain final answer")
	if got := gate.CommitPendingForTerminal(); got != "" || gate.VisibleText() != "" {
		t.Fatalf("invalid forced terminal prose was released: released=%q visible=%q", got, gate.VisibleText())
	}
}

func TestForcedToolChoiceProtocolAndRecoveryPreservePrivateState(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.6",
		Messages: []OpenAIMessage{
			{Role: "system", Content: "You are Codex, a coding agent."},
			{Role: "user", Content: "inspect"},
		},
		Tools:      []OpenAITool{openAITestTool("shell_command")},
		ToolChoice: map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "shell_command"}},
	}
	payload := OpenAIToKiro(req, false)
	priming := structuredOutputPriming(t, payload)
	if payload.ToolChoiceMode != "specific" || payload.ToolChoiceName != "shell_command" ||
		!payload.AgentCompletionProtocolEnabled || !payload.AgentReasoningCapEnabled {
		t.Fatalf("forced private state missing: mode=%q name=%q protocol=%v cap=%v",
			payload.ToolChoiceMode, payload.ToolChoiceName, payload.AgentCompletionProtocolEnabled, payload.AgentReasoningCapEnabled)
	}
	if !strings.Contains(priming, "<forced_tool_choice_completion_protocol>") {
		t.Fatalf("forced completion protocol missing: %q", priming)
	}
	if !strings.Contains(priming, `tool named "shell_command"`) || strings.Contains(priming, `tool named "shellCommand"`) {
		t.Fatalf("OpenAI tool choice instruction does not match the upstream tool specification: %q", priming)
	}
	if strings.Contains(priming, "<agent_final_") || strings.Contains(priming, "END_TURN") || strings.Contains(priming, "<agent_completion_protocol>") {
		t.Fatalf("forced prompt exposed a text-terminal escape hatch: %q", priming)
	}

	clone := cloneKiroPayload(payload)
	if clone.ToolChoiceMode != payload.ToolChoiceMode || clone.ToolChoiceName != payload.ToolChoiceName ||
		clone.AgentCompletionProtocolEnabled != payload.AgentCompletionProtocolEnabled ||
		clone.AgentReasoningCapEnabled != payload.AgentReasoningCapEnabled {
		t.Fatalf("clone lost forced state: %#v", clone)
	}
	if !captureAgentRecoveryOriginalPayload(clone) {
		t.Fatal("failed to capture recovery payload")
	}
	clone.ConversationState.CurrentMessage.UserInputMessage.Content = "mutated"
	if !restoreAgentRecoveryOriginalPayload(clone) || clone.ToolChoiceName != "shell_command" || !clone.AgentReasoningCapEnabled {
		t.Fatalf("restore lost forced state: mode=%q name=%q cap=%v",
			clone.ToolChoiceMode, clone.ToolChoiceName, clone.AgentReasoningCapEnabled)
	}

	prepareReasoningOnlyRetry(clone, newUpstreamAgentToolChoiceMismatchError("specific", "shell_command", nil))
	retry := forcedToolChoiceRetryInstructionForPayload(clone)
	current := clone.ConversationState.CurrentMessage.UserInputMessage.Content
	if strings.Count(current, retry) != 1 || strings.Contains(current, "<agent_final_") || strings.Contains(retry, "END_TURN") {
		t.Fatalf("forced retry exposed or duplicated a text terminal: %q", current)
	}
	if strings.Contains(current, "<agent_retry>") {
		t.Fatalf("forced retry included the auto-mode final-answer option: %q", current)
	}
	if !strings.Contains(retry, `"shell_command"`) || strings.Contains(retry, `"shellCommand"`) {
		t.Fatalf("forced retry used the wrong OpenAI upstream name: %q", retry)
	}
}

type forcedToolChoiceFormatCase struct {
	name   string
	invoke func(*Handler, *httptest.ResponseRecorder)
}

func forcedToolChoiceFormatCases() []forcedToolChoiceFormatCase {
	newClaudePayload := func() *KiroPayload {
		return ClaudeToKiro(&ClaudeRequest{
			Model:  "claude-opus-5",
			System: "You are an interactive agent that helps users with software engineering tasks.\n# Using your tools",
			Messages: []ClaudeMessage{{
				Role: "user", Content: "inspect",
			}},
			Tools: []ClaudeTool{{
				Name: "shell_command", Description: "run a command", InputSchema: map[string]interface{}{"type": "object"},
			}},
			ToolChoice: map[string]interface{}{"type": "any"},
		}, false)
	}

	return []forcedToolChoiceFormatCase{
		{
			name: "Claude stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, newClaudePayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
		},
		{
			name: "Claude non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeNonStream(rec, newClaudePayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
		},
		{
			name: "Chat Completions stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"inspect"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"tool_choice":"required",
					"stream":true
				}`))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name: "Chat Completions non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"inspect"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"tool_choice":"required"
				}`))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name: "Responses stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"inspect",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"tool_choice":"required",
					"stream":true,
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
		},
		{
			name: "Responses non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"inspect",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"tool_choice":"required",
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
		},
	}
}

func TestForcedToolChoiceRecoversAcrossPublicResponseFormats(t *testing.T) {
	for _, test := range forcedToolChoiceFormatCases() {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if payload.ToolChoiceMode != "required" || !payload.AgentCompletionProtocolEnabled {
					t.Fatalf("forced guard missing on call %d: mode=%q protocol=%v", calls, payload.ToolChoiceMode, payload.AgentCompletionProtocolEnabled)
				}
				if calls == 1 {
					callback.OnText("premature forced answer", false)
					callback.OnStopReason("END_TURN")
					callback.OnComplete(10, 4)
					return nil
				}
				if calls != 2 {
					t.Fatalf("unexpected upstream call %d", calls)
				}
				current := payload.ConversationState.CurrentMessage.UserInputMessage.Content
				if !strings.Contains(current, "<forced_tool_choice_retry>") || strings.Contains(current, "<agent_final_") {
					t.Fatalf("forced retry instruction invalid: %q", current)
				}
				callback.OnToolUse(KiroToolUse{ToolUseID: "toolu_forced", Name: "shell_command", Input: map[string]interface{}{"command": "pwd"}})
				callback.OnStopReason("TOOL_USE")
				callback.OnComplete(10, 3)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			if calls != 2 || rec.Code != http.StatusOK || !strings.Contains(body, "shell_command") {
				t.Fatalf("forced recovery failed: calls=%d status=%d body=%s", calls, rec.Code, body)
			}
			if strings.Contains(body, "premature forced answer") || strings.Contains(body, upstreamIncompleteResponseCode) || strings.Contains(body, "response.failed") {
				t.Fatalf("forced recovery leaked invalid prose or failure: %s", body)
			}
		})
	}
}

func TestForcedToolChoiceAcceptsFirstCompletedToolAcrossPublicResponseFormats(t *testing.T) {
	for _, test := range forcedToolChoiceFormatCases() {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if payload.AgentCompletionRetries != 0 {
					t.Fatalf("valid first tool entered recovery: retries=%d", payload.AgentCompletionRetries)
				}
				callback.OnText("tool-call narration", false)
				callback.OnToolUse(KiroToolUse{ToolUseID: "toolu_first", Name: "shell_command", Input: map[string]interface{}{"command": "pwd"}})
				callback.OnStopReason("TOOL_USE")
				callback.OnComplete(10, 3)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			if calls != 1 || rec.Code != http.StatusOK || !strings.Contains(body, "shell_command") {
				t.Fatalf("valid forced tool failed: calls=%d status=%d body=%s", calls, rec.Code, body)
			}
			if strings.Contains(body, "tool-call narration") || strings.Contains(body, upstreamIncompleteResponseCode) || strings.Contains(body, "response.failed") {
				t.Fatalf("valid forced tool leaked narration or failure: %s", body)
			}
		})
	}
}

func TestForcedToolChoiceMismatchIsIncompleteResponse(t *testing.T) {
	err := newUpstreamAgentToolChoiceMismatchError("specific", "lookup", []string{"other"})
	if !errors.Is(err, errUpstreamAgentToolChoiceMismatch) || !isUpstreamIncompleteResponseError(err) {
		t.Fatalf("tool-choice mismatch lost incomplete-response classification: %v", err)
	}
}

func forcedSpecificStreamCases() []forcedToolChoiceFormatCase {
	return []forcedToolChoiceFormatCase{
		{
			name: "Claude stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				payload := ClaudeToKiro(&ClaudeRequest{
					Model:      "claude-opus-5",
					System:     "You are an interactive agent that helps users with software engineering tasks.\n# Using your tools",
					Messages:   []ClaudeMessage{{Role: "user", Content: "inspect"}},
					Tools:      []ClaudeTool{{Name: "shell_command", InputSchema: map[string]interface{}{"type": "object"}}},
					ToolChoice: map[string]interface{}{"type": "tool", "name": "shell_command"},
				}, false)
				h.handleClaudeStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
		},
		{
			name: "Chat Completions stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"inspect"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"tool_choice":{"type":"function","function":{"name":"shell_command"}},
					"stream":true
				}`))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name: "Responses stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"inspect",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"tool_choice":{"type":"function","name":"shell_command"},
					"stream":true,
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
		},
	}
}

func TestForcedSpecificWrongToolStreamIsHeldUntilValidated(t *testing.T) {
	for _, test := range forcedSpecificStreamCases() {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if payload.ToolChoiceMode != "specific" || payload.ToolChoiceName != "shell_command" {
					t.Fatalf("specific state missing: mode=%q name=%q", payload.ToolChoiceMode, payload.ToolChoiceName)
				}
				if calls == 1 {
					callback.OnToolUseStart("toolu_wrong", "other_tool")
					callback.OnToolUseInputDelta("toolu_wrong", `{"command":"WRONG_ARGUMENT_SENTINEL"}`)
					callback.OnToolUse(KiroToolUse{ToolUseID: "toolu_wrong", Name: "other_tool", Input: map[string]interface{}{"command": "WRONG_ARGUMENT_SENTINEL"}})
					callback.OnStopReason("TOOL_USE")
					callback.OnComplete(10, 3)
					return nil
				}
				callback.OnToolUseStart("toolu_correct", "shell_command")
				callback.OnToolUseInputDelta("toolu_correct", `{"command":"pwd"}`)
				callback.OnToolUse(KiroToolUse{ToolUseID: "toolu_correct", Name: "shell_command", Input: map[string]interface{}{"command": "pwd"}})
				callback.OnStopReason("TOOL_USE")
				callback.OnComplete(10, 3)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			if calls != 2 || rec.Code != http.StatusOK || !strings.Contains(body, "shell_command") {
				t.Fatalf("specific recovery failed: calls=%d status=%d body=%s", calls, rec.Code, body)
			}
			for _, leaked := range []string{"other_tool", "toolu_wrong", "WRONG_ARGUMENT_SENTINEL", upstreamIncompleteResponseCode, "response.failed"} {
				if strings.Contains(body, leaked) {
					t.Fatalf("unvalidated tool event %q leaked: %s", leaked, body)
				}
			}
		})
	}
}

func TestForcedSpecificLongMappedToolNameValidatesClientName(t *testing.T) {
	name := "mcp__workspace_server_with_a_very_long_namespace__read_file_with_a_very_long_suffix"
	payload := ClaudeToKiro(&ClaudeRequest{
		Model:      "claude-opus-5",
		System:     "You are an interactive agent that helps users with software engineering tasks.\n# Using your tools",
		Messages:   []ClaudeMessage{{Role: "user", Content: "inspect"}},
		Tools:      []ClaudeTool{{Name: name, InputSchema: map[string]interface{}{"type": "object"}}},
		ToolChoice: map[string]interface{}{"type": "tool", "name": name},
	}, false)
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.Tools) != 1 {
		t.Fatalf("mapped tool missing: %+v", context)
	}
	upstreamName := context.Tools[0].ToolSpecification.Name
	if upstreamName == name || payload.ToolNameMap[upstreamName] != name {
		t.Fatalf("long name mapping missing: upstream=%q map=%v", upstreamName, payload.ToolNameMap)
	}
	if err := agentCompletionResponseError(payload, false, "", false, []string{name}, "TOOL_USE"); err != nil {
		t.Fatalf("restored client tool name was rejected: %v", err)
	}
	if err := agentCompletionResponseError(payload, false, "", false, []string{upstreamName}, "TOOL_USE"); err != nil {
		t.Fatalf("upstream mapped tool name was rejected: %v", err)
	}
	if retry := forcedToolChoiceRetryInstructionForPayload(payload); !strings.Contains(retry, `"`+upstreamName+`"`) {
		t.Fatalf("retry did not use the exact long upstream name %q: %q", upstreamName, retry)
	}
}
