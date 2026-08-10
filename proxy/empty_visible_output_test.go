package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"

	"github.com/google/uuid"
)

func TestClaudeStreamRetriesMalformedToolJSONBeforeOutput(t *testing.T) {
	var calls int
	var accountIDs []string
	h := newEmptyVisibleOutputTestHandler(t, 2, func(account *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		accountIDs = append(accountIDs, account.ID)
		if calls == 1 {
			return &KiroStreamProtocolError{
				Code: "upstream_tool_json_error",
				Err:  errors.New(`incomplete tool use: unexpected EOF`),
			}
		}
		callback.OnText("recovered", false)
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 2 || len(accountIDs) != 2 || accountIDs[0] == accountIDs[1] {
		t.Fatalf("calls=%d accounts=%v, want two distinct attempts", calls, accountIDs)
	}
	if !strings.Contains(rec.Body.String(), "recovered") || strings.Contains(rec.Body.String(), "upstream_tool_json_error") {
		t.Fatalf("stream did not recover cleanly: %s", rec.Body.String())
	}
}

func TestClaudeStreamToolJSONRetryExhaustionDoesNotUseTransportBudget(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, _ *KiroStreamCallback) error {
		calls++
		return &KiroStreamProtocolError{
			Code: "upstream_tool_json_error",
			Err:  errors.New(`incomplete tool use: unexpected EOF`),
		}
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if calls != maxKiroToolJSONRetries+1 {
		t.Fatalf("calls=%d, want initial attempt plus %d tool JSON retries", calls, maxKiroToolJSONRetries)
	}
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_tool_json_error") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestClaudeStreamRetriesTransientPreludeErrorBeforeOutput(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			return errors.New("read Kiro event stream prelude: stream error: stream ID 7; INTERNAL_ERROR; received from peer")
		}
		callback.OnText("recovered", false)
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if rec.Code != http.StatusOK || calls != 2 || !strings.Contains(rec.Body.String(), "recovered") {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
}

func TestClaudeStreamAutoToolsFlushesTextBeforeUpstreamCompletes(t *testing.T) {
	rec := httptest.NewRecorder()
	var observedDuringCall string
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnText("first token", false)
		// The upstream call has not returned yet. Seeing the delta here proves
		// that auto+tools is genuinely streamed rather than flushed at EOF.
		observedDuringCall = rec.Body.String()
		callback.OnComplete(10, 2)
		return nil
	})
	payload := emptyVisibleOutputPayload()
	payload.ToolChoiceMode = "auto"
	tool := KiroToolWrapper{}
	tool.ToolSpecification.Name = "lookup"
	tool.ToolSpecification.Description = "lookup"
	tool.ToolSpecification.InputSchema.JSON = map[string]interface{}{"type": "object"}
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		Tools: []KiroToolWrapper{tool},
	}

	h.handleClaudeStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if !strings.Contains(observedDuringCall, `"type":"text_delta"`) || !strings.Contains(observedDuringCall, "first token") {
		t.Fatalf("auto tool text was not flushed before upstream completion: %s", observedDuringCall)
	}
}

func TestClaudeToolResponseDoesNotExposeCopiedToolTranscript(t *testing.T) {
	polluted := "Now let me check misc.js against the CSS.\n" +
		"user Tool results:\n[Bash] CSS missing for msg-bubble\nassistant\n" +
		"I found the missing styles. Let me add them."
	toolUse := KiroToolUse{
		ToolUseID: "toolu_pollution_guard",
		Name:      "Bash",
		Input:     map[string]interface{}{"command": "apply the stylesheet edit"},
	}

	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non_stream", true: "stream"}[stream], func(t *testing.T) {
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				callback.OnText(polluted, false)
				callback.OnToolUse(toolUse)
				callback.OnStopReason("tool_use")
				callback.OnComplete(10, 8)
				return nil
			})
			payload := emptyVisibleOutputPayload()
			payload.AgentCompletionMarker = "<agent_final_0123456789abcdef>"
			payload.ToolChoiceMode = "auto"
			rec := httptest.NewRecorder()

			if stream {
				h.handleClaudeStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			} else {
				h.handleClaudeNonStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			}

			body := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, body)
			}
			for _, leaked := range []string{"user Tool results:", "[Bash] CSS missing", "\nassistant\n", "<historical_tool_results"} {
				if strings.Contains(body, leaked) {
					t.Fatalf("response exposed copied tool transcript %q: %s", leaked, body)
				}
			}
			for _, want := range []string{`"name":"Bash"`, `"type":"tool_use"`} {
				if !strings.Contains(body, want) {
					t.Fatalf("response lost validated tool content %q: %s", want, body)
				}
			}
			if stream && (!strings.Contains(body, "Now let me check misc.js") || !strings.Contains(body, "Let me add them")) {
				t.Fatalf("stream lost valid narration around the copied transcript: %s", body)
			}
		})
	}
}

func TestOpenAIAndResponsesStreamsDoNotExposeCopiedToolTranscript(t *testing.T) {
	polluted := "I will inspect the stylesheet.\n" +
		"user Tool results:\n[Bash] copied repository output\nassistant\n" +
		"I found the issue. I will edit it now."
	newHandler := func(t *testing.T) *Handler {
		return newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
			callback.OnText(polluted, false)
			callback.OnToolUse(KiroToolUse{
				ToolUseID: "toolu_multi_protocol_pollution_guard",
				Name:      "Bash",
				Input:     map[string]interface{}{"command": "apply the edit"},
			})
			callback.OnStopReason("tool_use")
			callback.OnComplete(10, 8)
			return nil
		})
	}
	newPayload := func() *KiroPayload {
		payload := emptyVisibleOutputPayload()
		payload.AgentCompletionMarker = "<agent_final_0123456789abcdef>"
		payload.ToolChoiceMode = "auto"
		return payload
	}
	assertClean := func(t *testing.T, body string) {
		t.Helper()
		for _, leaked := range []string{"user Tool results:", "[Bash] copied repository output", "\nassistant\n", "<historical_tool_results"} {
			if strings.Contains(body, leaked) {
				t.Fatalf("response exposed copied tool transcript %q: %s", leaked, body)
			}
		}
		for _, want := range []string{"I will inspect the stylesheet", "I will edit it now", "Bash"} {
			if !strings.Contains(body, want) {
				t.Fatalf("response lost validated content %q: %s", want, body)
			}
		}
	}

	t.Run("chat_completions", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newHandler(t).handleOpenAIStream(rec, newPayload(), "claude-opus-5", "claude-opus-5", false, false, 5, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertClean(t, rec.Body.String())
		if !strings.Contains(rec.Body.String(), `"tool_calls"`) {
			t.Fatalf("chat stream lost structured tool call: %s", rec.Body.String())
		}
	})

	t.Run("responses", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := &ResponsesRequest{Model: "claude-opus-5", Stream: true}
		newHandler(t).handleResponsesStream(
			rec, newPayload(), "claude-opus-5", "claude-opus-5", false,
			5, "", "resp_pollution_guard", req, nil, false,
		)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertClean(t, rec.Body.String())
		if !strings.Contains(rec.Body.String(), `"type":"function_call"`) {
			t.Fatalf("Responses stream lost structured function call: %s", rec.Body.String())
		}
	})
}

func newEmptyVisibleOutputTestHandler(t *testing.T, accountCount int, caller kiroAPICaller) *Handler {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for i := 0; i < accountCount; i++ {
		id := "empty-output-" + uuid.NewString() + "-" + string(rune('a'+i))
		if err := config.AddAccount(config.Account{
			ID:          id,
			Email:       id + "@example.test",
			Enabled:     true,
			AccessToken: "token-" + id,
			ProfileArn:  "arn:aws:codewhisperer:profile/" + id,
		}); err != nil {
			t.Fatalf("add account %s: %v", id, err)
		}
	}
	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingStats)
	return &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		kiroCaller:  caller,
	}
}

func TestClaudeNonStreamReturnsInputTooLongWithoutAccountRotation(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 2, func(_ *config.Account, _ *KiroPayload, _ *KiroStreamCallback) error {
		calls++
		return errors.New(`HTTP 400 from test: {"message":"Input is too long.","reason":"REQUEST_BODY_INVALID"}`)
	})

	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 1, nil, "")

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1 without retry or account rotation", calls)
	}
	for _, account := range config.GetAccounts() {
		if !account.Enabled {
			t.Fatalf("input-too-long error disabled account %q", account.ID)
		}
	}
}

func emptyVisibleOutputPayload() *KiroPayload {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "test",
		ModelID: "claude-opus-5",
		Origin:  "AI_EDITOR",
	}
	return payload
}

func TestClaudeStreamSuppressesReasoningEventWhenThinkingIsDisabled(t *testing.T) {
	var accountIDs []string
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 2, func(account *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		accountIDs = append(accountIDs, account.ID)
		callback.OnReasoning(KiroReasoningContentEvent{Text: "private reasoning"})
		callback.OnText("final answer", false)
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 || len(accountIDs) != 1 {
		t.Fatalf("calls=%d accounts=%v, want one attempt", calls, accountIDs)
	}

	events := decodeResponseContractSSE(t, rec.Body.String())
	var visibleText string
	var outputTokens float64
	var messageStops int
	for _, event := range events {
		switch event.Name {
		case "content_block_delta":
			delta, _ := event.Data["delta"].(map[string]interface{})
			if delta["type"] == "text_delta" {
				visibleText += delta["text"].(string)
			}
		case "message_delta":
			usage, _ := event.Data["usage"].(map[string]interface{})
			outputTokens, _ = usage["output_tokens"].(float64)
		case "message_stop":
			messageStops++
		}
	}
	if visibleText != "final answer" || strings.Contains(rec.Body.String(), "private reasoning") || outputTokens <= 0 || messageStops != 1 {
		t.Fatalf("visible=%q output_tokens=%v message_stops=%d\n%s", visibleText, outputTokens, messageStops, rec.Body.String())
	}
}

func TestClaudeStreamMarkerRecoveryRetainsTerminalUsage(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnComplete(17, 4)
		if calls == 1 {
			callback.OnReasoning(KiroReasoningContentEvent{Text: "planning only"})
			callback.OnText("unmarked narration", false)
			return nil
		}
		callback.OnText(payload.AgentCompletionMarker+"recovered", false)
		callback.OnStopReason("END_TURN")
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if calls != 2 || rec.Code != http.StatusOK {
		t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
	}
	events := decodeResponseContractSSE(t, rec.Body.String())
	var terminalUsage map[string]interface{}
	for _, event := range events {
		if event.Name == "message_delta" {
			terminalUsage, _ = event.Data["usage"].(map[string]interface{})
		}
	}
	inputTokens, inputOK := terminalUsage["input_tokens"].(float64)
	outputTokens, outputOK := terminalUsage["output_tokens"].(float64)
	if !inputOK || !outputOK || inputTokens <= 0 || outputTokens <= 0 {
		t.Fatalf("semantic recovery lost terminal usage: %#v\n%s", terminalUsage, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unmarked narration") || !strings.Contains(rec.Body.String(), "recovered") ||
		strings.Index(rec.Body.String(), "unmarked narration") > strings.Index(rec.Body.String(), "recovered") {
		t.Fatalf("semantic recovery output was malformed: %s", rec.Body.String())
	}
}

func TestClaudeNonStreamSuppressesReasoningEventWhenThinkingIsDisabled(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnReasoning(KiroReasoningContentEvent{Text: "private reasoning"})
		callback.OnText("final answer", false)
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeNonStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "final answer") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "private reasoning") {
		t.Fatalf("reasoning leaked into non-thinking response: %s", rec.Body.String())
	}
}

func TestClaudeBufferedCallSuppressesReasoningWhenThinkingIsDisabled(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnReasoning(KiroReasoningContentEvent{Text: "private reasoning"})
		callback.OnText("final answer", false)
		callback.OnComplete(10, 4)
		return nil
	})

	result, err := h.callClaudeBuffered("claude-opus-5", emptyVisibleOutputPayload(), false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "final answer" || result.Reasoning.Text != "" {
		t.Fatalf("content=%q reasoning=%q", result.Content, result.Reasoning.Text)
	}
}

func TestClaudeBufferedCallExtractsTaggedReasoningWhenThinkingIsEnabled(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnText("<thinking>inspect the request</thinking>answer", false)
		callback.OnComplete(10, 4)
		return nil
	})

	result, err := h.callClaudeBuffered("claude-opus-5", emptyVisibleOutputPayload(), true, 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "answer" || result.Reasoning.Text != "inspect the request" {
		t.Fatalf("content=%q reasoning=%q", result.Content, result.Reasoning.Text)
	}
}

func TestClaudeStreamRejectsEmptyUpstreamAfterBoundedRetries(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if calls != maxKiroTransportPreResponseRetries+1 {
		t.Fatalf("calls=%d, want bounded attempts=%d", calls, maxKiroTransportPreResponseRetries+1)
	}
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "message_stop") ||
		!strings.Contains(rec.Body.String(), "upstream returned no client-visible output") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	logs := h.getRequestLogs()
	if len(logs) != calls+1 {
		t.Fatalf("request logs=%#v, want one settled success plus terminal error per attempt", logs)
	}
	settled := 0
	for _, log := range logs {
		if log.Status == "success" {
			settled++
			if log.Tokens <= 0 {
				t.Fatalf("empty attempt was not charged: %#v", log)
			}
		}
	}
	if settled != calls {
		t.Fatalf("settled attempts=%d, want %d: %#v", settled, calls, logs)
	}
}

func TestClaudeStreamRetriesHiddenReasoningUntilVisibleAnswer(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			callback.OnReasoning(KiroReasoningContentEvent{Text: "private reasoning"})
		} else {
			callback.OnText("visible answer", false)
		}
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	body := rec.Body.String()
	if calls != 2 || rec.Code != http.StatusOK || !strings.Contains(body, "visible answer") ||
		strings.Contains(body, "private reasoning") || !strings.Contains(body, "message_stop") {
		t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, body)
	}
	for _, log := range h.getRequestLogs() {
		if log.Status == "success" && log.Tokens <= 0 {
			t.Fatalf("hidden-reasoning attempt was not charged: %#v", log)
		}
	}
}

func TestClaudeNonStreamRetriesHiddenReasoningUntilVisibleAnswer(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			callback.OnReasoning(KiroReasoningContentEvent{Text: "private reasoning"})
		} else {
			callback.OnText("visible answer", false)
		}
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeNonStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	body := rec.Body.String()
	if calls != 2 || rec.Code != http.StatusOK || !strings.Contains(body, "visible answer") || strings.Contains(body, "private reasoning") {
		t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, body)
	}
	for _, log := range h.getRequestLogs() {
		if log.Status == "success" && log.Tokens <= 0 {
			t.Fatalf("hidden-reasoning attempt was not charged: %#v", log)
		}
	}
}

func TestClaudeRejectsThinkingTagOnlyWhenThinkingIsDisabled(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var calls int
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				callback.OnText("<thinking>private tagged reasoning</thinking>", false)
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()
			if stream {
				h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			} else {
				h.handleClaudeNonStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			}
			if calls != maxKiroTransportPreResponseRetries+1 || rec.Code != http.StatusBadGateway ||
				strings.Contains(rec.Body.String(), "private tagged reasoning") ||
				!strings.Contains(rec.Body.String(), "upstream returned no client-visible output") {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestClaudeReasoningOnlyWithOmittedThinkingDisplayRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var calls int
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				callback.OnReasoning(KiroReasoningContentEvent{Text: "private omitted reasoning"})
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()
			opts := claudeThinkingResponseOptions{Format: "thinking", OmitDisplay: true}
			if stream {
				h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", true, opts, 5, nil, "")
			} else {
				h.handleClaudeNonStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", true, opts, 5, nil, "")
			}
			if calls != maxKiroTransportPreResponseRetries+1 || rec.Code != http.StatusBadGateway ||
				strings.Contains(rec.Body.String(), "private omitted reasoning") ||
				!strings.Contains(rec.Body.String(), "upstream returned no client-visible output") {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestClaudeNonStreamRejectsEmptyUpstreamAfterBoundedRetries(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeNonStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	body := rec.Body.String()
	if calls != maxKiroTransportPreResponseRetries+1 || rec.Code != http.StatusBadGateway ||
		strings.Contains(body, "\"content\"") || !strings.Contains(body, "upstream returned no client-visible output") {
		t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, body)
	}
	settled := 0
	for _, log := range h.getRequestLogs() {
		if log.Status == "success" {
			settled++
			if log.Tokens <= 0 {
				t.Fatalf("empty attempt was not charged: %#v", log)
			}
		}
	}
	if settled != calls {
		t.Fatalf("settled attempts=%d, want %d", settled, calls)
	}
}

func TestOpenAIAdaptersRejectEmptyUpstreamAfterBoundedRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			var calls int
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()
			payload := emptyVisibleOutputPayload()
			if stream {
				h.handleOpenAIStream(rec, payload, "claude-opus-5", "claude-opus-5", false, false, 5, "")
			} else {
				h.handleOpenAINonStream(rec, payload, "claude-opus-5", "claude-opus-5", false, 5, "")
			}
			if calls != maxKiroTransportPreResponseRetries+1 || rec.Code != http.StatusBadGateway ||
				!strings.Contains(rec.Body.String(), "upstream returned no client-visible output") {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestResponsesAdaptersRejectEmptyUpstreamAfterBoundedRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			var calls int
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()
			payload := emptyVisibleOutputPayload()
			req := &ResponsesRequest{Model: "claude-opus-5", Stream: stream}
			if stream {
				h.handleResponsesStream(rec, payload, "claude-opus-5", "claude-opus-5", false, 5, "", "resp_empty", req, nil, false)
			} else {
				h.handleResponsesNonStream(rec, payload, "claude-opus-5", "claude-opus-5", false, 5, "", "resp_empty", req, nil, false)
			}
			if calls != maxKiroTransportPreResponseRetries+1 || !strings.Contains(rec.Body.String(), "upstream returned no client-visible output") {
				t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
			}
			if stream && rec.Code != http.StatusOK {
				t.Fatalf("stream status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !stream && rec.Code != http.StatusBadGateway {
				t.Fatalf("non-stream status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPublicAdaptersShareInvalidBearerRefresh(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		stream   bool
	}{
		{name: "claude stream", protocol: "claude", stream: true},
		{name: "claude non-stream", protocol: "claude"},
		{name: "chat stream", protocol: "chat", stream: true},
		{name: "chat non-stream", protocol: "chat"},
		{name: "responses stream", protocol: "responses", stream: true},
		{name: "responses non-stream", protocol: "responses"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls, refreshes int
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if calls == 1 {
					return &KiroHTTPError{
						StatusCode: http.StatusForbidden,
						Endpoint:   "test",
						Body:       `{"message":"The bearer token included in the request is invalid."}`,
					}
				}
				if callback.OnStreamReady != nil {
					callback.OnStreamReady()
				}
				callback.OnText("visible after bearer refresh", false)
				callback.OnComplete(10, 4)
				return nil
			})
			h.tokenRefresher = func(_ context.Context, account *config.Account, force bool) error {
				if !force {
					t.Fatal("invalid bearer recovery did not force refresh")
				}
				refreshes++
				account.AccessToken = "fresh-token"
				return nil
			}
			rec := httptest.NewRecorder()
			payload := emptyVisibleOutputPayload()
			switch test.protocol {
			case "claude":
				if test.stream {
					h.handleClaudeStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
				} else {
					h.handleClaudeNonStream(rec, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
				}
			case "chat":
				if test.stream {
					h.handleOpenAIStream(rec, payload, "claude-opus-5", "claude-opus-5", false, false, 5, "")
				} else {
					h.handleOpenAINonStream(rec, payload, "claude-opus-5", "claude-opus-5", false, 5, "")
				}
			case "responses":
				req := &ResponsesRequest{Model: "claude-opus-5", Stream: test.stream}
				if test.stream {
					h.handleResponsesStream(rec, payload, "claude-opus-5", "claude-opus-5", false, 5, "", "resp_refresh", req, nil, false)
				} else {
					h.handleResponsesNonStream(rec, payload, "claude-opus-5", "claude-opus-5", false, 5, "", "resp_refresh", req, nil, false)
				}
			}
			if calls != 2 || refreshes != 1 || !strings.Contains(rec.Body.String(), "visible after bearer refresh") {
				t.Fatalf("calls=%d refreshes=%d status=%d body=%s", calls, refreshes, rec.Code, rec.Body.String())
			}
		})
	}
}
