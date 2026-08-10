package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestInterruptedAgentNarrationRetriesWithCorrection(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnReasoning(KiroReasoningContentEvent{Text: fmt.Sprintf("reasoning attempt %d", calls)})
		if calls > 1 {
			content := payload.ConversationState.CurrentMessage.UserInputMessage.Content
			if payload.AgentCompletionMarker == "" {
				t.Fatal("coding-agent completion marker was not enabled")
			}
			retryInstruction := agentCompletionRetryInstruction(payload.AgentCompletionMarker)
			if strings.Count(content, retryInstruction) != 1 {
				t.Fatalf("retry instruction missing or duplicated: %q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
			}
			if payload.ToolChoiceMode != "auto" {
				t.Fatalf("retry changed the original tool choice: mode=%q content=%q", payload.ToolChoiceMode, content)
			}
		}
		switch calls {
		case 1:
			callback.OnText("我先检查新代码里哪些方法可被测试访问。", false)
		default:
			callback.OnToolUse(KiroToolUse{
				ToolUseID: "call_search",
				Name:      "shell_command",
				Input:     map[string]interface{}{"command": "rg -n SFTP MainWindow.xaml"},
			})
			callback.OnStopReason("TOOL_USE")
		}
		callback.OnComplete(10, 4)
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-opus-5",
		"instructions":"You are Codex, a coding agent.",
		"input":"add the monitoring panel",
		"reasoning":{"effort":"high","summary":"auto"},
		"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
		"stream":true,
		"store":false
	}`))
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	body := rec.Body.String()
	if calls != 2 || rec.Code != http.StatusOK {
		t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, body)
	}
	if !strings.Contains(body, `"type":"function_call"`) || !strings.Contains(body, `"name":"shell_command"`) {
		t.Fatalf("recovered attempt did not emit a structured tool call: %s", body)
	}
	if !strings.Contains(body, "我先检查") || strings.Contains(body, "<agent_final_") {
		t.Fatalf("direct narration was missing or the internal completion marker leaked downstream: %s", body)
	}
	if strings.Contains(body, upstreamIncompleteResponseCode) {
		t.Fatalf("recovered action narration surfaced an error: %s", body)
	}
}

func TestClaudeFailedToolResultRecoversWithStructuredToolCall(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-opus-5",
		System: "You are Codex, a coding agent.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Create a.js with Write. If Write fails, retry it immediately."},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{
					"type": "tool_use", "id": "tool_write_previous", "name": "Write",
					"input": map[string]interface{}{"file_path": "a.js", "content": "const ok = true;"},
				},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type": "tool_result", "tool_use_id": "tool_write_previous",
					"content": "Write failed", "is_error": true,
				},
			}},
		},
		Tools: []ClaudeTool{{
			Name: "Write", Description: "Write a file",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string"},
					"content":   map[string]interface{}{"type": "string"},
				},
			},
		}},
	}
	payload := ClaudeToKiro(req, false)
	if payload.AgentCompletionMarker == "" {
		t.Fatal("coding-agent completion protocol was not enabled")
	}

	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, attempt *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		current := attempt.ConversationState.CurrentMessage.UserInputMessage
		switch calls {
		case 1:
			if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 1 ||
				current.UserInputMessageContext.ToolResults[0].Status != "error" ||
				current.UserInputMessageContext.ToolResults[0].Content[0].Text != "Write failed" {
				t.Fatalf("initial failed tool result was not structured: %#v", current.UserInputMessageContext)
			}
			callback.OnText("I will retry the write now.", false)
		case 2:
			var historyText strings.Builder
			structuredErrorPreserved := false
			if context := current.UserInputMessageContext; context != nil && len(context.ToolResults) == 1 &&
				context.ToolResults[0].ToolUseID == "tool_write_previous" && context.ToolResults[0].Status == "error" &&
				len(context.ToolResults[0].Content) == 1 && context.ToolResults[0].Content[0].Text == "Write failed" {
				structuredErrorPreserved = true
			}
			for _, message := range attempt.ConversationState.History {
				if message.UserInputMessage != nil {
					historyText.WriteString(message.UserInputMessage.Content)
					if context := message.UserInputMessage.UserInputMessageContext; context != nil && len(context.ToolResults) == 1 &&
						context.ToolResults[0].ToolUseID == "tool_write_previous" && context.ToolResults[0].Status == "error" &&
						len(context.ToolResults[0].Content) == 1 && context.ToolResults[0].Content[0].Text == "Write failed" {
						structuredErrorPreserved = true
					}
				}
			}
			if !structuredErrorPreserved ||
				(!strings.Contains(historyText.String(), "Create a.js") && !strings.Contains(current.Content, "Create a.js")) {
				t.Fatalf("recovery request lost structured failed tool evidence: history=%#v", attempt.ConversationState.History)
			}
			callback.OnToolUse(KiroToolUse{
				ToolUseID: "tool_write_retry",
				Name:      "Write",
				Input:     map[string]interface{}{"file_path": "a.js", "content": "const ok = true;"},
			})
			callback.OnStopReason("TOOL_USE")
		default:
			t.Fatalf("unexpected extra recovery call: %d", calls)
		}
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, payload, req.Model, req.Model, false, claudeThinkingResponseOptions{}, 10, nil, "")

	body := rec.Body.String()
	if calls != 2 || rec.Code != http.StatusOK || !strings.Contains(body, `"type":"tool_use"`) ||
		!strings.Contains(body, `"name":"Write"`) || !strings.Contains(body, `event: message_stop`) {
		t.Fatalf("failed Write was not recovered with a structured retry: calls=%d status=%d body=%s", calls, rec.Code, body)
	}
	if strings.Contains(body, `event: error`) || strings.Contains(body, "<agent_final_") {
		t.Fatalf("failed-tool recovery corrupted the downstream stream: %s", body)
	}
}

func TestClaudeFailedToolResultSurvivesRecoveryInputTooLong(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-opus-5",
		System: "You are Codex, a coding agent.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Create config.js with Write and correct any failed prerequisite."},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{
					"type": "tool_use", "id": "tool_write_previous", "name": "Write",
					"input": map[string]interface{}{"file_path": "config.js", "content": "export default {};"},
				},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type": "tool_result", "tool_use_id": "tool_write_previous",
					"content": "Write failed: file must be read first", "is_error": true,
				},
			}},
		},
		Tools: []ClaudeTool{{
			Name: "Write", Description: "Write a file",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string"},
					"content":   map[string]interface{}{"type": "string"},
				},
			},
		}},
	}
	payload := ClaudeToKiro(req, false)
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, attempt *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		switch calls {
		case 1:
			callback.OnText("I will correct the failed Write now.", false)
			callback.OnComplete(10, 4)
			return nil
		case 2:
			if attempt.AgentCompletionRetries != 1 || attempt.AgentRecoveryAggressive {
				t.Fatalf("unexpected first semantic recovery state: retries=%d aggressive=%v",
					attempt.AgentCompletionRetries, attempt.AgentRecoveryAggressive)
			}
			return fmt.Errorf(`HTTP 400 from test: {"message":"Input is too long.","reason":"REQUEST_BODY_INVALID"}`)
		case 3:
			current := attempt.ConversationState.CurrentMessage.UserInputMessage
			if attempt.AgentInputTooLongRecoveries != 1 || !attempt.agentInputTooLongRecoveryPending ||
				!attempt.AgentRecoveryAggressive || !strings.Contains(current.Content, "Write failed") ||
				!strings.Contains(current.Content, "tool_write_previous") ||
				!strings.Contains(current.Content, "Write") ||
				!strings.Contains(current.Content, "config.js") ||
				!strings.Contains(current.Content, "complete_tool_exchange") {
				t.Fatalf("bounded retry lost failed Write evidence: state=%#v content=%q", attempt, current.Content)
			}
			callback.OnToolUse(KiroToolUse{
				ToolUseID: "tool_write_retry",
				Name:      "Write",
				Input:     map[string]interface{}{"file_path": "config.js", "content": "export default {};"},
			})
			callback.OnStopReason("TOOL_USE")
			callback.OnComplete(10, 5)
			return nil
		default:
			t.Fatalf("unexpected extra upstream call: %d", calls)
			return nil
		}
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, payload, req.Model, req.Model, false, claudeThinkingResponseOptions{}, 10, nil, "")

	body := rec.Body.String()
	if calls != 3 || rec.Code != http.StatusOK || !strings.Contains(body, `"type":"tool_use"`) ||
		!strings.Contains(body, `"name":"Write"`) || !strings.Contains(body, `event: message_stop`) {
		t.Fatalf("input-too-long recovery did not return the corrected tool call: calls=%d status=%d body=%s", calls, rec.Code, body)
	}
	if strings.Contains(body, `event: error`) || strings.Contains(body, "Input is too long") {
		t.Fatalf("recovered tool stream leaked the internal input error: %s", body)
	}
}

func TestStreamingMarkedEndTurnDoesNotStartAgentContinuation(t *testing.T) {
	tests := []struct {
		name      string
		invoke    func(*Handler, *httptest.ResponseRecorder)
		terminal  string
		forbidden string
	}{
		{
			name: "Claude Messages",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
			terminal:  `event: message_stop`,
			forbidden: `event: error`,
		},
		{
			name: "Chat Completions",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"inspect"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"stream":true
				}`))
				h.handleOpenAIChat(rec, req)
			},
			terminal:  `data: [DONE]`,
			forbidden: `"error":`,
		},
		{
			name: "Responses",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"inspect the implementation",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"stream":true,
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
			terminal:  `"type":"response.completed"`,
			forbidden: `"type":"response.failed"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, attempt *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if calls > 1 {
					t.Fatalf("marked END_TURN unexpectedly started continuation call %d", calls)
				}
				if callback.OnReasoning != nil {
					callback.OnReasoning(KiroReasoningContentEvent{Text: "The answer is ready."})
				}
				callback.OnText(attempt.AgentCompletionMarker+"All core modules confirmed.", false)
				callback.OnStopReason("END_TURN")
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			if calls != 1 || !strings.Contains(body, "All core modules confirmed.") || !strings.Contains(body, test.terminal) {
				t.Fatalf("marked END_TURN did not finish directly: calls=%d body=%s", calls, body)
			}
			if strings.Contains(body, test.forbidden) || strings.Contains(body, upstreamAgentContinuationTimeoutCode) {
				t.Fatalf("native END_TURN produced a continuation error: %s", body)
			}
		})
	}
}

func TestStreamingAgentContinuationTimeoutKeepsPriorVisibleOutput(t *testing.T) {
	tests := []struct {
		name      string
		invoke    func(*Handler, *httptest.ResponseRecorder)
		terminal  string
		forbidden string
	}{
		{
			name: "Claude Messages",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
			terminal:  `event: message_stop`,
			forbidden: `event: error`,
		},
		{
			name: "Chat Completions",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"inspect"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"stream":true
				}`))
				h.handleOpenAIChat(rec, req)
			},
			terminal:  `data: [DONE]`,
			forbidden: `"error":`,
		},
		{
			name: "Responses",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"inspect the implementation",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"stream":true,
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
			terminal:  `"type":"response.completed"`,
			forbidden: `"type":"response.failed"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				switch calls {
				case 1:
					callback.OnText("I will inspect the implementation now.", false)
					callback.OnStopReason("END_TURN")
					callback.OnComplete(10, 4)
					return nil
				case 2:
					if payload.AgentActionContinuations == 0 {
						t.Fatal("second call was not the bounded agent continuation")
					}
					return newUpstreamAgentContinuationTimeoutError(1)
				default:
					t.Fatalf("continuation timeout triggered unexpected call %d", calls)
					return nil
				}
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			if calls != 2 || rec.Code != http.StatusOK ||
				!strings.Contains(body, "I will inspect the implementation now.") ||
				!strings.Contains(body, test.terminal) {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, body)
			}
			if strings.Contains(body, test.forbidden) || strings.Contains(body, upstreamAgentContinuationTimeoutCode) ||
				strings.Contains(body, "upstream returned no client-visible output") {
				t.Fatalf("bounded continuation success was overwritten: %s", body)
			}
		})
	}
}

func TestAgentContinuationTimeoutRequiresPreviouslyWrittenClientOutput(t *testing.T) {
	payload := completionProtocolTestPayload()
	payload.agentActionContinuationTimeout = true
	payload.AgentActionContinuations = 1
	err := newUpstreamAgentContinuationTimeoutError(1)

	if shouldFinishStreamAfterAgentContinuation(payload, err, 0, false) {
		t.Fatal("continuation timeout succeeded without prior client-visible output")
	}
	if !shouldFinishStreamAfterAgentContinuation(payload, err, 0, true) {
		t.Fatal("continuation timeout rejected a previously written visible prefix")
	}
}

func TestStreamingAgentContinuationTimeoutWithoutVisibleOutputFails(t *testing.T) {
	tests := []struct {
		name      string
		invoke    func(*Handler, *httptest.ResponseRecorder)
		required  string
		forbidden string
	}{
		{
			name: "Claude Messages",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
			required:  upstreamAgentContinuationTimeoutCode,
			forbidden: `event: message_stop`,
		},
		{
			name: "Chat Completions",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"inspect"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"stream":true
				}`))
				h.handleOpenAIChat(rec, req)
			},
			required:  `"error":`,
			forbidden: `"finish_reason":"stop"`,
		},
		{
			name: "Responses",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"inspect",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"stream":true,
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
			required:  `"type":"response.failed"`,
			forbidden: `"type":"response.completed"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				payload.agentActionContinuationTimeout = true
				payload.AgentActionContinuations = 1
				callback.OnComplete(10, 0)
				return newUpstreamAgentContinuationTimeoutError(1)
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			if calls == 0 || !strings.Contains(body, test.required) || strings.Contains(body, test.forbidden) {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, body)
			}
		})
	}
}

func TestStreamingMissingTerminalUsesFullContextFallbackAcrossProtocols(t *testing.T) {
	tests := []struct {
		name      string
		invoke    func(*Handler, *httptest.ResponseRecorder)
		terminal  string
		forbidden string
	}{
		{
			name: "Claude Messages",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
			terminal:  `event: message_stop`,
			forbidden: `event: error`,
		},
		{
			name: "Chat Completions",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"inspect the implementation"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"stream":true
				}`))
				h.handleOpenAIChat(rec, req)
			},
			terminal:  `data: [DONE]`,
			forbidden: `"error":`,
		},
		{
			name: "Responses",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"inspect the implementation",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"stream":true,
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
			terminal:  `"type":"response.completed"`,
			forbidden: `"type":"response.failed"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if calls <= maxKiroAgentActionContinuations+1 {
					callback.OnText(fmt.Sprintf("action narration %d", calls), false)
					callback.OnComplete(10, 4)
					return nil
				}

				content := payload.ConversationState.CurrentMessage.UserInputMessage.Content
				if payload.AgentFullContextRecoveries != 1 || !payload.agentFullContextRecoveryPending ||
					!strings.Contains(content, "<agent_full_context_recovery>") {
					t.Fatalf("full-context fallback was not active on call %d: full=%d pending=%v content=%q",
						calls, payload.AgentFullContextRecoveries, payload.agentFullContextRecoveryPending, content)
				}
				callback.OnToolUse(KiroToolUse{
					ToolUseID: "toolu_full_context_recovery",
					Name:      "shell_command",
					Input:     map[string]interface{}{"command": "rg -n TODO ."},
				})
				callback.OnStopReason("TOOL_USE")
				callback.OnComplete(10, 5)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			wantCalls := maxKiroAgentActionContinuations + maxKiroAgentFullContextRecoveries + 1
			if calls != wantCalls || rec.Code != http.StatusOK ||
				!strings.Contains(body, "shell_command") || !strings.Contains(body, test.terminal) {
				t.Fatalf("calls=%d want=%d status=%d body=%s", calls, wantCalls, rec.Code, body)
			}
			if !strings.Contains(body, "action narration 1") ||
				strings.Contains(body, "action narration 2") ||
				strings.Contains(body, "action narration 3") ||
				strings.Contains(body, test.forbidden) || strings.Contains(body, "<agent_full_context_recovery>") {
				t.Fatalf("recovery narration was not suppressed or the tool terminal was corrupted: %s", body)
			}
		})
	}
}

type agentRecoveryFormatCase struct {
	name            string
	stream          bool
	streamError     string
	successTerminal string
	invoke          func(*Handler, *httptest.ResponseRecorder)
}

func agentRecoveryFormatCases() []agentRecoveryFormatCase {
	return []agentRecoveryFormatCase{
		{
			name:            "Claude stream",
			stream:          true,
			streamError:     `event: error`,
			successTerminal: `event: message_stop`,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", true, claudeThinkingResponseOptions{Format: "thinking"}, 5, nil, "")
			},
		},
		{
			name: "Claude non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeNonStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", true, claudeThinkingResponseOptions{Format: "thinking"}, 5, nil, "")
			},
		},
		{
			name:        "Chat Completions stream",
			stream:      true,
			streamError: `"type":"upstream_incomplete_response"`,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"inspect"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"reasoning_effort":"high",
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
					"reasoning_effort":"high"
				}`))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name:            "Responses stream",
			stream:          true,
			streamError:     `"type":"response.failed"`,
			successTerminal: `"type":"response.completed"`,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"inspect",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"reasoning":{"effort":"high","summary":"auto"},
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
					"reasoning":{"effort":"high","summary":"auto"},
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
		},
	}
}

func TestMixedActionAndReasoningOnlyUsesFullContextFallbackAcrossPublicFormats(t *testing.T) {
	scenarios := []struct {
		name      string
		emitStall func(int, *KiroStreamCallback)
	}{
		{
			name: "mixed_action_reasoning",
			emitStall: func(call int, callback *KiroStreamCallback) {
				if call == 1 {
					callback.OnText("I will inspect the implementation now.", false)
					return
				}
				reasoning := fmt.Sprintf("reasoning-only continuation %d", call)
				if callback.OnReasoning != nil {
					callback.OnReasoning(KiroReasoningContentEvent{Text: reasoning})
				} else {
					callback.OnText(reasoning, true)
				}
			},
		},
		{
			name: "pure_reasoning",
			emitStall: func(call int, callback *KiroStreamCallback) {
				reasoning := fmt.Sprintf("reasoning-only attempt %d", call)
				if callback.OnReasoning != nil {
					callback.OnReasoning(KiroReasoningContentEvent{Text: reasoning})
				} else {
					callback.OnText(reasoning, true)
				}
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			for _, test := range agentRecoveryFormatCases() {
				t.Run(test.name, func(t *testing.T) {
					calls := 0
					h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
						calls++
						if calls > 4 {
							t.Fatalf("unexpected fifth recovery call: calls=%d", calls)
						}
						if calls <= 3 {
							scenario.emitStall(calls, callback)
							callback.OnComplete(10, 4)
							return nil
						}

						content := payload.ConversationState.CurrentMessage.UserInputMessage.Content
						if payload.AgentFullContextRecoveries != 1 || !payload.agentFullContextRecoveryPending ||
							!strings.Contains(content, "<agent_full_context_recovery>") {
							t.Fatalf("scenario %s skipped full-context fallback: calls=%d full=%d pending=%v content=%q",
								scenario.name, calls, payload.AgentFullContextRecoveries,
								payload.agentFullContextRecoveryPending, content)
						}
						callback.OnToolUse(KiroToolUse{
							ToolUseID: "toolu_full_context_recovery",
							Name:      "shell_command",
							Input:     map[string]interface{}{"command": "rg -n TODO ."},
						})
						callback.OnStopReason("TOOL_USE")
						callback.OnComplete(10, 4)
						return nil
					})
					rec := httptest.NewRecorder()

					test.invoke(h, rec)

					body := rec.Body.String()
					if calls != 4 || rec.Code != http.StatusOK || !strings.Contains(body, "shell_command") {
						t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, body)
					}
					if strings.Contains(body, upstreamIncompleteResponseCode) || strings.Contains(body, "<agent_full_context_recovery>") {
						t.Fatalf("scenario %s leaked an internal recovery failure: %s", scenario.name, body)
					}
				})
			}
		})
	}
}

func TestFullContextRecoveryFailureIsBoundedAcrossPublicFormats(t *testing.T) {
	scenarios := []struct {
		name      string
		emitStall func(int, *KiroStreamCallback)
	}{
		{
			name: "pure_reasoning_only",
			emitStall: func(call int, callback *KiroStreamCallback) {
				if callback.OnReasoning != nil {
					callback.OnReasoning(KiroReasoningContentEvent{Text: fmt.Sprintf("reasoning-only attempt %d", call)})
				} else {
					callback.OnText(fmt.Sprintf("reasoning-only attempt %d", call), true)
				}
			},
		},
		{
			name: "pure_action_only",
			emitStall: func(call int, callback *KiroStreamCallback) {
				callback.OnText(fmt.Sprintf("I still need to invoke the tool now, attempt %d.", call), false)
			},
		},
		{
			name: "mixed_action_reasoning",
			emitStall: func(call int, callback *KiroStreamCallback) {
				if call%2 == 1 {
					callback.OnText(fmt.Sprintf("I will invoke the tool now, attempt %d.", call), false)
					return
				}
				if callback.OnReasoning != nil {
					callback.OnReasoning(KiroReasoningContentEvent{Text: fmt.Sprintf("reasoning-only attempt %d", call)})
				} else {
					callback.OnText(fmt.Sprintf("reasoning-only attempt %d", call), true)
				}
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			for _, test := range agentRecoveryFormatCases() {
				t.Run(test.name, func(t *testing.T) {
					calls := 0
					h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
						calls++
						if calls > 4 {
							t.Fatalf("full-context recovery exceeded its total budget: calls=%d", calls)
						}
						if calls == 4 {
							content := payload.ConversationState.CurrentMessage.UserInputMessage.Content
							if payload.AgentFullContextRecoveries != 1 || !payload.agentFullContextRecoveryPending ||
								!strings.Contains(content, "<agent_full_context_recovery>") {
								t.Fatalf("fourth call was not the full-context recovery: full=%d pending=%v content=%q",
									payload.AgentFullContextRecoveries, payload.agentFullContextRecoveryPending, content)
							}
						}
						scenario.emitStall(calls, callback)
						callback.OnComplete(10, 4)
						return nil
					})
					rec := httptest.NewRecorder()

					test.invoke(h, rec)

					body := rec.Body.String()
					if calls != 4 {
						t.Fatalf("calls=%d, want 4; status=%d body=%s", calls, rec.Code, body)
					}
					if test.stream {
						if !strings.Contains(body, test.streamError) {
							t.Fatalf("exhausted stream did not report %q: %s", test.streamError, body)
						}
						if test.successTerminal != "" && strings.Contains(body, test.successTerminal) {
							t.Fatalf("exhausted stream was reported as successful: %s", body)
						}
					} else if rec.Code != http.StatusBadGateway || !strings.Contains(body, upstreamIncompleteResponseCode) {
						t.Fatalf("exhausted non-stream response status=%d body=%s", rec.Code, body)
					}
					if strings.Contains(body, "<agent_full_context_recovery>") {
						t.Fatalf("internal full-context recovery tag leaked downstream: %s", body)
					}
				})
			}
		})
	}
}

func TestStreamingAgentContinuationRetriesTransientUpstreamFailure(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		switch calls {
		case 1:
			callback.OnText("I will inspect the test file now.", false)
			callback.OnComplete(10, 4)
			return nil
		case 2:
			return &KiroHTTPError{
				StatusCode: http.StatusInternalServerError,
				Endpoint:   "Kiro IDE",
				Body:       `{"message":"Encountered an unexpected error when processing the request, please try again."}`,
			}
		default:
			callback.OnToolUse(KiroToolUse{
				ToolUseID: "toolu_recovered_after_500",
				Name:      "shell_command",
				Input:     map[string]interface{}{"command": "rg -n TestAgent proxy"},
			})
			callback.OnStopReason("TOOL_USE")
			callback.OnComplete(10, 4)
			return nil
		}
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(
		rec,
		completionProtocolTestPayload(),
		"claude-opus-5",
		"claude-opus-5",
		false,
		claudeThinkingResponseOptions{},
		5,
		nil,
		"",
	)

	body := rec.Body.String()
	if calls != 3 || !strings.Contains(body, `"type":"tool_use"`) ||
		!strings.Contains(body, `event: message_stop`) {
		t.Fatalf("transient continuation failure was not retried: calls=%d body=%s", calls, body)
	}
	if strings.Contains(body, `event: error`) || strings.Contains(body, "<agent_final_") {
		t.Fatalf("transient continuation retry corrupted the downstream stream: %s", body)
	}
}

func TestClaudeLongContextRecoveryCompactsBeforeRetry(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		switch calls {
		case 1:
			if payload.AgentRecoveryCompacted {
				t.Fatal("initial long-context attempt was compacted")
			}
			callback.OnReasoning(KiroReasoningContentEvent{Text: "long planning pass"})
			callback.OnText("I will inspect the implementation now.", false)
			err := newUpstreamAgentActionOnlyError()
			prepareReasoningOnlyRetry(payload, err)
			return err
		case 2:
			if payload.AgentRecoveryCompacted {
				t.Fatal("first short continuation did not retain the original context")
			}
			wire, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal first short continuation: %v", err)
			}
			if !bytes.Contains(wire, []byte("assistant-planning-inertia")) {
				t.Fatal("first short continuation dropped the original planning context")
			}
			callback.OnReasoning(KiroReasoningContentEvent{Text: "repeated recovery reasoning"})
			callback.OnText("I will inspect the implementation now.", false)
			err = newUpstreamAgentActionOnlyError()
			prepareReasoningOnlyRetry(payload, err)
			return err
		default:
			if !payload.AgentRecoveryCompacted {
				t.Fatal("second short continuation did not receive bounded fresh context")
			}
			wire, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal compacted retry: %v", err)
			}
			if bytes.Contains(wire, []byte("UNRELATED_PLANNING_HEAD_SENTINEL")) ||
				!bytes.Contains(wire, []byte("DECISION_CAUSE_SENTINEL")) ||
				!bytes.Contains(wire, []byte("recent_causal_evidence")) {
				t.Fatal("bounded recovery did not retain the causal tail while dropping unrelated planning")
			}
			callback.OnReasoning(KiroReasoningContentEvent{Text: "act"})
			callback.OnToolUse(KiroToolUse{
				ToolUseID: "toolu_recovered",
				Name:      "shell_command",
				Input:     map[string]interface{}{"command": "rg -n TODO ."},
			})
			callback.OnComplete(10, 4)
			return nil
		}
	})

	payload := completionProtocolTestPayload()
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: agentCompletionProtocolInstruction(payload.AgentCompletionMarker)}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
		{UserInputMessage: &KiroUserInputMessage{Content: strings.Repeat("repository evidence ", 10000)}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "UNRELATED_PLANNING_HEAD_SENTINEL " + strings.Repeat("assistant-planning-inertia ", 20000) + " DECISION_CAUSE_SENTINEL: inspect the implementation before editing."}},
	}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "continue implementation"
	rec := httptest.NewRecorder()

	h.handleClaudeStream(
		rec,
		payload,
		"claude-opus-5",
		"claude-opus-5",
		true,
		claudeThinkingResponseOptions{Format: "thinking"},
		200000,
		nil,
		"",
	)

	body := rec.Body.String()
	if calls != 3 || rec.Code != http.StatusOK {
		t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, body)
	}
	if !strings.Contains(body, `"type":"tool_use"`) || !strings.Contains(body, `"name":"shell_command"`) {
		t.Fatalf("compacted recovery did not emit tool call: %s", body)
	}
	if strings.Contains(body, upstreamIncompleteResponseCode) ||
		strings.Contains(body, "UNRELATED_PLANNING_HEAD_SENTINEL") ||
		strings.Contains(body, "repeated recovery reasoning") {
		t.Fatalf("recovery leaked failure or old planning: %s", body)
	}
}

func TestCompletionMarkerRecoveryAcrossPublicResponseFormats(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
		invoke func(*Handler, *httptest.ResponseRecorder)
	}{
		{
			name:   "Claude stream",
			stream: true,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", true, claudeThinkingResponseOptions{Format: "thinking"}, 5, nil, "")
			},
		},
		{
			name: "Claude non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeNonStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", true, claudeThinkingResponseOptions{Format: "thinking"}, 5, nil, "")
			},
		},
		{
			name:   "Chat Completions stream",
			stream: true,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"inspect"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"reasoning_effort":"high",
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
					"reasoning_effort":"high"
				}`))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name:   "Responses stream",
			stream: true,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"inspect",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"reasoning":{"effort":"high","summary":"auto"},
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
					"reasoning":{"effort":"high","summary":"auto"},
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if payload.AgentCompletionMarker == "" {
					t.Fatal("completion protocol was not active")
				}
				reasoning := fmt.Sprintf("reasoning %d", calls)
				if callback.OnReasoning != nil {
					callback.OnReasoning(KiroReasoningContentEvent{Text: reasoning})
				} else {
					callback.OnText(reasoning, true)
				}
				if calls == 1 {
					callback.OnText("I will inspect the file now.", false)
				} else {
					callback.OnText(payload.AgentCompletionMarker[:12], false)
					callback.OnText(payload.AgentCompletionMarker[12:]+"\nfinal answer", false)
					callback.OnStopReason("END_TURN")
				}
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			if calls != 2 || rec.Code != http.StatusOK || !strings.Contains(body, "final answer") {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, body)
			}
			if test.stream && !strings.Contains(body, "I will inspect") {
				t.Fatalf("streaming recovery did not pass through first-attempt narration: %s", body)
			}
			if !test.stream && strings.Contains(body, "I will inspect") {
				t.Fatalf("non-stream response included superseded first-attempt narration: %s", body)
			}
			if strings.Contains(body, "<agent_final_") || strings.Contains(body, upstreamIncompleteResponseCode) {
				t.Fatalf("completion protocol leaked internal output: %s", body)
			}
		})
	}
}

func TestStreamingProtocolsPreserveReasoningTextToolOrder(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(*Handler, *httptest.ResponseRecorder)
	}{
		{
			name: "Claude Messages",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", true, claudeThinkingResponseOptions{Format: "thinking"}, 5, nil, "")
			},
		},
		{
			name: "Chat Completions",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"inspect"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"reasoning_effort":"high",
					"stream":true
				}`))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name: "Responses",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"inspect",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"reasoning":{"effort":"high","summary":"auto"},
					"stream":true,
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				if payload.AgentCompletionMarker == "" {
					t.Fatal("completion marker was not enabled")
				}
				if callback.OnReasoning != nil {
					callback.OnReasoning(KiroReasoningContentEvent{Text: "ORDER_REASONING"})
				} else {
					callback.OnText("ORDER_REASONING", true)
				}
				callback.OnText("ORDER_TEXT", false)
				callback.OnToolUse(KiroToolUse{
					ToolUseID: "call_order",
					Name:      "shell_command",
					Input:     map[string]interface{}{"command": "rg -n TODO ."},
				})
				callback.OnComplete(10, 5)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			reasoningAt := strings.Index(body, "ORDER_REASONING")
			textAt := strings.Index(body, "ORDER_TEXT")
			toolAt := strings.Index(body, `"name":"shell_command"`)
			if rec.Code != http.StatusOK || reasoningAt < 0 || textAt <= reasoningAt || toolAt <= textAt {
				t.Fatalf("status=%d order reasoning=%d text=%d tool=%d body=%s", rec.Code, reasoningAt, textAt, toolAt, body)
			}
		})
	}
}

func TestEventStreamKeepsToolUseTerminalWhenUpstreamTextArrivesLate(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "TERMINAL_REASONING"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "TERMINAL_TEXT_BEFORE_TOOL"}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "call_terminal",
			"name":      "shell_command",
			"input":     `{"command":"rg -n TODO ."}`,
			"stop":      true,
		}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "TERMINAL_LATE_TEXT"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "TERMINAL_LATE_REASONING"}),
		{0, 0, 0},
	}, nil))

	var reasoning, text string
	var starts, deltas int
	var toolUses []KiroToolUse
	var callbackErr error
	var completed bool
	err := parseEventStream(stream, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		OnReasoning: func(event KiroReasoningContentEvent) {
			reasoning += event.Text
		},
		OnText: func(value string, _ bool) {
			text += value
		},
		OnToolUseStart: func(_, _ string) {
			starts++
		},
		OnToolUseInputDelta: func(_, _ string) {
			deltas++
		},
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
		OnComplete: func(_, _ int) {
			completed = true
		},
		OnError: func(err error) {
			callbackErr = err
		},
	})

	if err != nil {
		t.Fatalf("parse event stream: %v", err)
	}
	if callbackErr != nil || !completed {
		t.Fatalf("callbackErr=%v completed=%v", callbackErr, completed)
	}
	if reasoning != "TERMINAL_REASONING" || text != "TERMINAL_TEXT_BEFORE_TOOL" {
		t.Fatalf("reasoning=%q text=%q", reasoning, text)
	}
	if starts != 1 || deltas != 1 || len(toolUses) != 1 || toolUses[0].Name != "shell_command" {
		t.Fatalf("starts=%d deltas=%d toolUses=%#v", starts, deltas, toolUses)
	}
}

func TestEventStreamPreservesPostToolContentWithoutAgentProtocol(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "call_ordinary",
			"name":      "shell_command",
			"input":     `{}`,
			"stop":      true,
		}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ORDINARY_LATE_TEXT"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "ORDINARY_LATE_REASONING"}),
	}, nil))

	var text, reasoning string
	var toolUses []KiroToolUse
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText:      func(value string, _ bool) { text += value },
		OnReasoning: func(event KiroReasoningContentEvent) { reasoning += event.Text },
		OnToolUse:   func(toolUse KiroToolUse) { toolUses = append(toolUses, toolUse) },
	})

	if err != nil || text != "ORDINARY_LATE_TEXT" || reasoning != "ORDINARY_LATE_REASONING" || len(toolUses) != 1 {
		t.Fatalf("err=%v text=%q reasoning=%q toolUses=%#v", err, text, reasoning, toolUses)
	}
}

func TestEventStreamRejectsPendingAgentToolUseOnUnexpectedEOF(t *testing.T) {
	tests := []struct {
		name   string
		frames [][]byte
	}{
		{
			name: "truncated JSON",
			frames: [][]byte{awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
				"toolUseId": "call_truncated",
				"name":      "shell_command",
				"input":     `{"command":`,
			})},
		},
		{
			name: "valid JSON without explicit completion",
			frames: [][]byte{awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
				"toolUseId": "call_unstopped",
				"name":      "shell_command",
				"input":     `{}`,
			})},
		},
		{
			name: "completed tool followed by pending tool",
			frames: [][]byte{
				awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
					"toolUseId": "call_complete",
					"name":      "Read",
					"input":     `{"file_path":"test.go"}`,
					"stop":      true,
				}),
				awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
					"toolUseId": "call_pending",
					"name":      "Write",
					"input":     `{"file_path":"test.go"}`,
				}),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := bytes.NewReader(bytes.Join(append(test.frames, []byte{0, 0, 0}), nil))
			var callbackErr error
			var completed bool
			err := parseEventStream(stream, &KiroStreamCallback{
				AgentCompletionMarker: "<agent_final_0123456789abcdef>",
				OnComplete:            func(_, _ int) { completed = true },
				OnError:               func(err error) { callbackErr = err },
			})

			if !isKiroToolJSONError(err) || callbackErr == nil || callbackErr.Error() != err.Error() || completed {
				t.Fatalf("err=%v callbackErr=%v completed=%v", err, callbackErr, completed)
			}
		})
	}
}

func TestCompletionProtocolBoundsRepeatedUnterminatedRetries(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnReasoning(KiroReasoningContentEvent{Text: fmt.Sprintf("reasoning %d", calls)})
		callback.OnText(fmt.Sprintf("unmarked narration %d", calls), false)
		callback.OnComplete(10, 4)
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-opus-5",
		"instructions":"You are Codex, a coding agent.",
		"input":"inspect",
		"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
		"reasoning":{"effort":"high","summary":"auto"},
		"stream":true,
		"store":false
	}`))
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	body := rec.Body.String()
	wantCalls := maxKiroAgentCompletionRetries + maxKiroAgentFullContextRecoveries + 1
	if calls != wantCalls {
		t.Fatalf("calls=%d, want %d; body=%s", calls, wantCalls, body)
	}
	if !strings.Contains(body, upstreamIncompleteResponseCode) ||
		!strings.Contains(body, `"type":"response.failed"`) ||
		strings.Contains(body, `"type":"response.completed"`) ||
		!strings.Contains(body, "reasoning 1") ||
		!strings.Contains(body, "unmarked narration 1") {
		t.Fatalf("semantic retry exhaustion missed the original output or explicit failure: %s", body)
	}
	for attempt := 2; attempt <= wantCalls; attempt++ {
		if strings.Contains(body, fmt.Sprintf("reasoning %d", attempt)) ||
			strings.Contains(body, fmt.Sprintf("unmarked narration %d", attempt)) {
			t.Fatalf("recovery attempt %d leaked narration or reasoning: %s", attempt, body)
		}
	}
}

func completionProtocolTestPayload() *KiroPayload {
	payload := emptyVisibleOutputPayload()
	payload.ToolChoiceMode = "auto"
	payload.AgentCompletionMarker = "<agent_final_0123456789abcdef>"
	tool := KiroToolWrapper{}
	tool.ToolSpecification.Name = "shell_command"
	tool.ToolSpecification.InputSchema.JSON = map[string]interface{}{"type": "object"}
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		Tools: []KiroToolWrapper{tool},
	}
	return payload
}

func TestAgentMaxTokenContinuesFromPartialAssistantOutput(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 && strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "<output_continuation>") {
			t.Fatal("continuation instruction was present on the initial attempt")
		}
		if calls == 2 {
			if strings.Count(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "<output_continuation>") != 1 {
				t.Fatalf("continuation instruction missing or duplicated: %q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
			}
			history := payload.ConversationState.History
			if len(history) < 2 || history[len(history)-1].AssistantResponseMessage == nil ||
				history[len(history)-1].AssistantResponseMessage.Content != "partial answer: " {
				t.Fatalf("partial assistant output was not replayed natively: %+v", history)
			}
		}
		callback.OnReasoning(KiroReasoningContentEvent{Text: fmt.Sprintf("reasoning attempt %d", calls)})
		if calls == 1 {
			callback.OnText("partial answer: ", false)
			callback.OnStopReason("max_tokens")
		} else {
			callback.OnText(payload.AgentCompletionMarker+"completed", false)
			callback.OnStopReason("END_TURN")
		}
		callback.OnComplete(10, 4)
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-opus-5",
		"instructions":"You are Codex, a coding agent.",
		"input":"inspect the repository",
		"reasoning":{"effort":"high","summary":"auto"},
		"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
		"stream":true,
		"store":false
	}`))
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	body := rec.Body.String()
	if calls != 2 || rec.Code != http.StatusOK || strings.Count(body, "partial answer: ") == 0 ||
		strings.Count(body, "completed") == 0 {
		t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
	}
	if strings.Contains(body, upstreamIncompleteResponseCode) || strings.Contains(body, "<agent_final_") {
		t.Fatalf("recovered max-token attempt surfaced an error or marker: %s", body)
	}
}

func TestUnmarkedEndTurnStripsOrphanToolTailAcrossProtocols(t *testing.T) {
	tests := []struct {
		name      string
		invoke    func(*Handler, *httptest.ResponseRecorder)
		terminal  string
		forbidden string
	}{
		{
			name: "Claude stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
			terminal:  `"stop_reason":"end_turn"`,
			forbidden: upstreamIncompleteResponseCode,
		},
		{
			name: "Claude non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeNonStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
			terminal:  `"stop_reason":"end_turn"`,
			forbidden: upstreamIncompleteResponseCode,
		},
		{
			name: "Chat Completions stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"answer"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"stream":true
				}`))
				h.handleOpenAIChat(rec, req)
			},
			terminal:  `"finish_reason":"stop"`,
			forbidden: `"finish_reason":"length"`,
		},
		{
			name: "Chat Completions non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"answer"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}]
				}`))
				h.handleOpenAIChat(rec, req)
			},
			terminal:  `"finish_reason":"stop"`,
			forbidden: `"finish_reason":"length"`,
		},
		{
			name: "Responses stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"answer",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"stream":true,
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
			terminal:  `"type":"response.completed"`,
			forbidden: `"status":"incomplete"`,
		},
		{
			name: "Responses non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"answer",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
			terminal:  `"status":"completed"`,
			forbidden: `"status":"incomplete"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, attempt *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if calls == 1 {
					callback.OnText("draft answer", false)
				} else {
					callback.OnText(attempt.AgentCompletionMarker+"final answer", false)
					callback.OnText("\n</para", false)
					callback.OnText("meter>", false)
				}
				callback.OnStopReason("END_TURN")
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			if calls != 2 || rec.Code != http.StatusOK || !strings.Contains(body, "final answer") || !strings.Contains(body, test.terminal) {
				t.Fatalf("calls=%d status=%d terminal=%q body=%s", calls, rec.Code, test.terminal, body)
			}
			if strings.Contains(body, test.forbidden) || strings.Contains(body, "<agent_final_") ||
				strings.Contains(body, "</parameter>") || strings.Contains(body, `\u003c/para`) ||
				strings.Contains(body, upstreamIncompleteResponseCode) {
				t.Fatalf("visible END_TURN produced an invalid terminal response: %s", body)
			}
		})
	}
}

func TestSerializedToolCarrierRecoversToStructuredCallAcrossProtocols(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(*Handler, *httptest.ResponseRecorder)
	}{
		{
			name: "Claude stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
		},
		{
			name: "Claude non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeNonStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
		},
		{
			name: "Chat Completions stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"edit the file"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
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
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"edit the file"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}]
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
					"input":"edit the file",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
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
					"input":"edit the file",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
		},
	}

	const carrier = `<invoke name="shell_command"><parameter name="command">printf RECOVERY_SENTINEL</parameter></invoke>`
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if calls == 1 {
					callback.OnText(carrier[:12], false)
					callback.OnText(carrier[12:], false)
					callback.OnStopReason("END_TURN")
				} else {
					if payload.AgentActionContinuations == 0 {
						t.Fatal("serialized tool carrier did not enter action continuation")
					}
					callback.OnToolUse(KiroToolUse{
						ToolUseID: "toolu_recovered",
						Name:      "shell_command",
						Input:     map[string]interface{}{"command": "printf RECOVERY_SENTINEL"},
					})
					callback.OnStopReason("TOOL_USE")
				}
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			if calls != 2 || rec.Code != http.StatusOK || !strings.Contains(body, "shell_command") || !strings.Contains(body, "RECOVERY_SENTINEL") {
				t.Fatalf("serialized carrier did not recover to a structured call: calls=%d status=%d body=%s", calls, rec.Code, body)
			}
			if strings.Contains(body, carrier) || strings.Contains(body, upstreamIncompleteResponseCode) {
				t.Fatalf("serialized carrier leaked or recovery failed: %s", body)
			}
		})
	}
}

func TestEventStreamAllowsReasoningBeyondOldEightKiBLimit(t *testing.T) {
	const marker = "<agent_final_0123456789abcdef>"
	reasoning := strings.Repeat("plan the implementation carefully; ", 1000)
	if len(reasoning) <= 8<<10 {
		t.Fatalf("test reasoning length=%d does not exceed the old limit", len(reasoning))
	}
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": reasoning}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": marker + "done"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"}),
	}, nil))

	err := parseEventStream(stream, &KiroStreamCallback{AgentCompletionMarker: marker})
	if err != nil {
		t.Fatalf("normal long planning was cut off: %v", err)
	}
}

func TestEventStreamAllowsLongUnmarkedTextWhenToolFollows(t *testing.T) {
	narration := strings.Repeat("inspect the repository; ", 1000)
	if len(narration) <= 8<<10 {
		t.Fatalf("test narration length=%d does not exceed the old limit", len(narration))
	}
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": narration}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_read", "name": "Read", "input": `{"file_path":"test.cs"}`, "stop": true,
		}),
	}, nil))
	var toolUses []KiroToolUse
	err := parseEventStream(stream, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})

	if err != nil || len(toolUses) != 1 || toolUses[0].Name != "Read" {
		t.Fatalf("err=%v toolUses=%#v", err, toolUses)
	}
}

func TestEventStreamChecksLongUnmarkedTextAtEOF(t *testing.T) {
	content := strings.Repeat("x", 32<<10)
	stream := bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": content,
	}))
	var observed string
	err := parseEventStream(stream, &KiroStreamCallback{
		AgentCompletionMarker: "<agent_final_0123456789abcdef>",
		OnText:                func(text string, _ bool) { observed += text },
	})
	if !isUpstreamIncompleteResponseError(err) || !isUpstreamAgentMarkerMismatchError(err) || !strings.Contains(err.Error(), "completion marker") {
		t.Fatalf("long unmarked completion was not rejected at EOF: %v", err)
	}
	if observed != content {
		t.Fatalf("long unmarked text was truncated before EOF: got=%d want=%d", len(observed), len(content))
	}
}

func TestVisibleReasoningOnlyWithoutMarkerDoesNotTriggerSemanticRetry(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
		invoke func(*Handler, *httptest.ResponseRecorder)
	}{
		{
			name:   "Claude stream",
			stream: true,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", true, claudeThinkingResponseOptions{Format: "thinking"}, 5, nil, "")
			},
		},
		{
			name: "Claude non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeNonStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", true, claudeThinkingResponseOptions{Format: "thinking"}, 5, nil, "")
			},
		},
		{
			name:   "Chat Completions stream",
			stream: true,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
					`{"model":"claude-opus-5","messages":[{"role":"user","content":"test"}],"reasoning_effort":"high","stream":true}`,
				))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name: "Chat Completions non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
					`{"model":"claude-opus-5","messages":[{"role":"user","content":"test"}],"reasoning_effort":"high"}`,
				))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name:   "Responses stream",
			stream: true,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
					`{"model":"claude-opus-5","input":"test","reasoning":{"effort":"high","summary":"auto"},"stream":true,"store":false}`,
				))
				h.handleOpenAIResponses(rec, req)
			},
		},
		{
			name: "Responses non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
					`{"model":"claude-opus-5","input":"test","reasoning":{"effort":"high","summary":"auto"},"store":false}`,
				))
				h.handleOpenAIResponses(rec, req)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				reasoning := "stalled reasoning"
				if callback.OnReasoning != nil {
					callback.OnReasoning(KiroReasoningContentEvent{Text: reasoning})
				} else {
					callback.OnText(reasoning, true)
				}
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			body := rec.Body.String()
			if calls != 1 {
				t.Fatalf("upstream calls=%d, want 1; body=%s", calls, body)
			}
			if rec.Code != http.StatusOK || strings.Contains(body, upstreamIncompleteResponseCode) {
				t.Fatalf("status=%d body=%s", rec.Code, body)
			}
			if test.stream && !strings.Contains(body, "stalled reasoning") {
				t.Fatalf("streamed reasoning disappeared: %s", body)
			}
		})
	}
}

func TestHiddenReasoningOnlyWithoutMarkerRetriesForOpenAIFormats(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(*Handler, *httptest.ResponseRecorder)
	}{
		{
			name: "Chat Completions stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
					`{"model":"claude-opus-5","messages":[{"role":"user","content":"test"}],"stream":true}`,
				))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name: "Chat Completions non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
					`{"model":"claude-opus-5","messages":[{"role":"user","content":"test"}]}`,
				))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name: "Responses stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
					`{"model":"claude-opus-5","input":"test","stream":true,"store":false}`,
				))
				h.handleOpenAIResponses(rec, req)
			},
		},
		{
			name: "Responses non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
					`{"model":"claude-opus-5","input":"test","store":false}`,
				))
				h.handleOpenAIResponses(rec, req)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				reasoning := fmt.Sprintf("private reasoning %d", calls)
				if callback.OnReasoning != nil {
					callback.OnReasoning(KiroReasoningContentEvent{Text: reasoning})
				} else {
					callback.OnText(reasoning, true)
				}
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			if calls != maxKiroTransportPreResponseRetries+1 {
				t.Fatalf("upstream calls=%d, want %d", calls, maxKiroTransportPreResponseRetries+1)
			}
			body := rec.Body.String()
			if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", rec.Code, body)
			}
			if strings.Contains(body, "private reasoning") || !strings.Contains(body, "upstream returned no client-visible output") {
				t.Fatalf("discarded reasoning attempt was accepted or leaked: %s", body)
			}
			if strings.Contains(body, upstreamIncompleteResponseCode) {
				t.Fatalf("empty-output retry was mislabeled incomplete agent response: %s", body)
			}
		})
	}
}

func TestClaudeStreamReasoningOnlyWithoutMarkerIsRejectedAfterBoundedRetries(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnReasoning(KiroReasoningContentEvent{Text: "private reasoning"})
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if calls != maxKiroTransportPreResponseRetries+1 {
		t.Fatalf("upstream calls=%d, want %d", calls, maxKiroTransportPreResponseRetries+1)
	}
	body := rec.Body.String()
	if rec.Code != http.StatusBadGateway || strings.Contains(body, "message_stop") ||
		!strings.Contains(body, "upstream returned no client-visible output") {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	if strings.Contains(body, "private reasoning") {
		t.Fatalf("hidden reasoning-only response was encoded incorrectly: %s", body)
	}
}

func TestBufferedClaudeReasoningOnlyWithoutMarkerIsRejectedAfterBoundedRetries(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnReasoning(KiroReasoningContentEvent{Text: "private reasoning"})
		callback.OnComplete(10, 4)
		return nil
	})

	result, err := h.callClaudeBuffered("claude-opus-5", emptyVisibleOutputPayload(), false, 5)
	if !isUpstreamEmptyResponseError(err) || calls != maxKiroTransportPreResponseRetries+1 || result != nil {
		t.Fatalf("calls=%d err=%v result=%#v", calls, err, result)
	}
}
