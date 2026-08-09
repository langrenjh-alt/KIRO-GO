package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestAdvanceAgentRecoveryTurnPreservesPairedStructuredToolResult(t *testing.T) {
	payload := completionProtocolTestPayload()
	const toolUseID = "toolu_failed_write"
	payload.ConversationState.History = []KiroHistoryMessage{{
		AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content: "I will write the file.",
			ToolUses: []KiroToolUse{{
				ToolUseID: toolUseID,
				Name:      "shell_command",
				Input:     map[string]interface{}{"command": "write config"},
			}},
		},
	}}
	wantResult := KiroToolResult{
		ToolUseID: toolUseID,
		Status:    "error",
		Content:   []KiroResultContent{{Text: "write failed: inspect the file first"}},
	}
	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	current.Content = "Continue after the failed tool call."
	current.UserInputMessageContext.ToolResults = []KiroToolResult{wantResult}
	payload.agentRecoveryActionHint = "I will inspect the file before retrying the write."

	advanceAgentRecoveryTurn(payload)

	history := payload.ConversationState.History
	if len(history) != 3 {
		t.Fatalf("history length=%d, want assistant tool_use, user tool_result, failed assistant attempt", len(history))
	}
	if history[0].AssistantResponseMessage == nil || len(history[0].AssistantResponseMessage.ToolUses) != 1 ||
		history[0].AssistantResponseMessage.ToolUses[0].ToolUseID != toolUseID {
		t.Fatalf("assistant tool_use was not preserved before its result: %#v", history[0])
	}
	priorUser := history[1].UserInputMessage
	if priorUser == nil || priorUser.UserInputMessageContext == nil ||
		len(priorUser.UserInputMessageContext.ToolResults) != 1 ||
		!reflect.DeepEqual(priorUser.UserInputMessageContext.ToolResults[0], wantResult) {
		t.Fatalf("current structured tool_result was not retained in history: %#v", priorUser)
	}
	if len(priorUser.UserInputMessageContext.Tools) != 0 ||
		strings.Contains(priorUser.Content, wantResult.Content[0].Text) ||
		strings.Contains(priorUser.Content, "<tool_result") {
		t.Fatalf("tool_result was flattened into prose or copied with schemas: %#v", priorUser)
	}
	current = &payload.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.Tools) != 1 ||
		len(current.UserInputMessageContext.ToolResults) != 0 {
		t.Fatalf("new recovery turn did not retain only callable tool schemas: %#v", current.UserInputMessageContext)
	}
}

type agentRecoveryRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn agentRecoveryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestCallKiroAPIInputTooLongWithoutTextPreservesPreviousActionHint(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	payload := completionProtocolTestPayload()
	payload.ProfileArn = "arn:aws:codewhisperer:us-east-1:000000000000:profile/test"
	const previousHint = "PREVIOUS_ACTION_HINT: inspect config before retrying the write"
	const previousReasoning = "PREVIOUS_REASONING_CAUSE: the write failed because the file was stale"
	const previousVisible = "PREVIOUS_VISIBLE_ACTION: inspect config before retrying the write"
	payload.agentRecoveryActionHint = previousHint
	payload.agentRecoveryAttemptReasoning = previousReasoning
	payload.agentRecoveryAttemptVisibleText = previousVisible
	if !captureAgentRecoveryOriginalPayload(payload) {
		t.Fatal("failed to capture original recovery payload")
	}
	payload.AgentCompletionRetries = 1
	payload.AgentFullContextRecoveries = 1
	payload.agentFullContextRecoveryPending = true

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Transport: agentRecoveryRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"message":"Input is too long.","reason":"REQUEST_BODY_INVALID"}`,
			)),
		}, nil
	})})
	t.Cleanup(func() { kiroHttpStore.Store(oldClient) })

	account := &config.Account{
		ID:          "hint-preservation-test",
		AccessToken: "test-token",
		ProfileArn:  payload.ProfileArn,
	}
	err := CallKiroAPI(account, payload, &KiroStreamCallback{})
	if err == nil || !isKiroInputTooLongError(err) {
		t.Fatalf("CallKiroAPI error=%v, want input-too-long", err)
	}
	if payload.agentRecoveryActionHint != previousHint {
		t.Fatalf("zero-text HTTP 400 replaced previous action hint: got %q want %q", payload.agentRecoveryActionHint, previousHint)
	}
	if payload.agentRecoveryAttemptReasoning != previousReasoning ||
		payload.agentRecoveryAttemptVisibleText != previousVisible {
		t.Fatalf("zero-text HTTP 400 replaced split attempt buffers: reasoning=%q visible=%q",
			payload.agentRecoveryAttemptReasoning, payload.agentRecoveryAttemptVisibleText)
	}
	if !prepareAgentInputTooLongRecovery(payload) {
		t.Fatal("preserved action hint was not available to bounded input-too-long recovery")
	}
	content := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if !strings.Contains(content, previousHint) || !strings.Contains(content, previousReasoning) ||
		!strings.Contains(content, previousVisible) || !strings.Contains(content, "<agent_bounded_context_recovery>") {
		t.Fatalf("bounded recovery did not receive previous action hint: %q", content)
	}
}

func TestFullContextRecoveryPreflightUsesInstructionAndFixedReserve(t *testing.T) {
	const model = "claude-sonnet-4.5"
	window := getContextWindowSize(model)
	budget := window - agentRecoveryTokenReserve
	if window <= 0 || budget <= 0 {
		t.Fatalf("invalid model recovery budget: window=%d reserve=%d", window, agentRecoveryTokenReserve)
	}

	t.Run("above_eighty_percent_does_not_force_compaction", func(t *testing.T) {
		payload, _, withInstructionTokens := recoveryPayloadAtMostInstructionTokens(t, model, window*80/100+1024)
		if withInstructionTokens <= window*80/100 || withInstructionTokens >= budget {
			t.Fatalf("test payload did not isolate the old 80%% threshold: tokens=%d window=%d budget=%d", withInstructionTokens, window, budget)
		}
		assertRecoveryPayloadBelowLegacyWireThreshold(t, payload)
		if !captureAgentRecoveryOriginalPayload(payload) || !prepareFullContextAgentRecovery(payload) {
			t.Fatal("failed to prepare full-context recovery")
		}
		if payload.AgentRecoveryCompacted {
			t.Fatal("payload below fixed-reserve budget was compacted merely for crossing 80%")
		}
		if content := payload.ConversationState.CurrentMessage.UserInputMessage.Content; !strings.Contains(content, "<agent_full_context_recovery>") || strings.Contains(content, "<agent_bounded_full_context_recovery>") {
			t.Fatalf("unexpected recovery instruction for in-budget payload: %q", content)
		}
	})

	t.Run("instruction_crossing_fixed_reserve_is_compacted", func(t *testing.T) {
		payload, bareTokens, withInstructionTokens := recoveryPayloadCrossingInstructionBudget(t, model, budget)
		if bareTokens > budget || withInstructionTokens <= budget {
			t.Fatalf("test payload did not cross the instruction-aware budget: bare=%d with_instruction=%d budget=%d", bareTokens, withInstructionTokens, budget)
		}
		assertRecoveryPayloadBelowLegacyWireThreshold(t, payload)
		if !captureAgentRecoveryOriginalPayload(payload) || !prepareFullContextAgentRecovery(payload) {
			t.Fatal("failed to prepare near-limit full-context recovery")
		}
		if !payload.AgentRecoveryCompacted || payload.AgentRecoveryAggressive {
			t.Fatalf("near-limit recovery state compacted=%v aggressive=%v, want bounded non-aggressive preflight", payload.AgentRecoveryCompacted, payload.AgentRecoveryAggressive)
		}
		content := payload.ConversationState.CurrentMessage.UserInputMessage.Content
		if !strings.Contains(content, "<agent_bounded_full_context_recovery>") || strings.Contains(content, "<agent_full_context_recovery>") {
			t.Fatalf("near-limit request did not use bounded full recovery: %q", content)
		}
	})
}

func recoveryBudgetTestPayload(model string, fillerBytes int) *KiroPayload {
	payload := completionProtocolTestPayload()
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = model
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = strings.Repeat("!", fillerBytes)
	payload.agentRecoveryActionHint = "inspect the repository and perform the pending edit"
	return payload
}

func recoveryPayloadAtMostInstructionTokens(t *testing.T, model string, target int) (*KiroPayload, int, int) {
	t.Helper()
	instruction := agentFullContextRecoveryInstruction("inspect the repository and perform the pending edit")
	low, high := 0, getContextWindowSize(model)*3
	for low < high {
		mid := low + (high-low+1)/2
		_, tokens := agentRecoveryPayloadEstimate(recoveryBudgetTestPayload(model, mid), instruction)
		if tokens <= target {
			low = mid
		} else {
			high = mid - 1
		}
	}
	payload := recoveryBudgetTestPayload(model, low)
	_, bareTokens := agentRecoveryPayloadEstimate(payload, "")
	_, withInstructionTokens := agentRecoveryPayloadEstimate(payload, instruction)
	return payload, bareTokens, withInstructionTokens
}

func recoveryPayloadCrossingInstructionBudget(t *testing.T, model string, budget int) (*KiroPayload, int, int) {
	t.Helper()
	instruction := agentFullContextRecoveryInstruction("inspect the repository and perform the pending edit")
	low, high := 0, getContextWindowSize(model)*3
	for low < high {
		mid := low + (high-low)/2
		_, tokens := agentRecoveryPayloadEstimate(recoveryBudgetTestPayload(model, mid), instruction)
		if tokens > budget {
			high = mid
		} else {
			low = mid + 1
		}
	}
	payload := recoveryBudgetTestPayload(model, low)
	_, bareTokens := agentRecoveryPayloadEstimate(payload, "")
	_, withInstructionTokens := agentRecoveryPayloadEstimate(payload, instruction)
	return payload, bareTokens, withInstructionTokens
}

func assertRecoveryPayloadBelowLegacyWireThreshold(t *testing.T, payload *KiroPayload) {
	t.Helper()
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal recovery payload: %v", err)
	}
	if len(wire) >= longAgentRecoveryWireMinBytes {
		t.Fatalf("test payload bytes=%d reached legacy 512 KiB threshold=%d", len(wire), longAgentRecoveryWireMinBytes)
	}
}

func TestFullContextInputTooLongAllowsOnlyOneAggressiveFallback(t *testing.T) {
	payload := completionProtocolTestPayload()
	payload.agentRecoveryActionHint = "perform the pending edit"
	if !captureAgentRecoveryOriginalPayload(payload) {
		t.Fatal("failed to capture original recovery payload")
	}
	payload.AgentCompletionRetries = maxKiroAgentCompletionRetries + 1
	if !prepareFullContextAgentRecovery(payload) || !payload.agentFullContextRecoveryPending {
		t.Fatal("failed to enter full-context recovery")
	}

	handler := &Handler{}
	account := &config.Account{ID: "single-aggressive-fallback"}
	retrySameID := ""
	inputTooLong := &KiroHTTPError{
		StatusCode: http.StatusBadRequest,
		Endpoint:   "test",
		Body:       `{"message":"Input is too long.","reason":"REQUEST_BODY_INVALID"}`,
	}
	if !handler.retryAgentRecoveryAfterInputTooLong("test", payload, account, &retrySameID, inputTooLong) {
		t.Fatal("full-context HTTP 400 did not receive its one aggressive fallback")
	}
	if payload.AgentInputTooLongRecoveries != 1 || !payload.AgentRecoveryAggressive ||
		!payload.agentInputTooLongRecoveryPending || retrySameID != account.ID {
		t.Fatalf("unexpected first fallback state: retries=%d aggressive=%v pending=%v retry_account=%q",
			payload.AgentInputTooLongRecoveries, payload.AgentRecoveryAggressive,
			payload.agentInputTooLongRecoveryPending, retrySameID)
	}

	continuationID := payload.ConversationState.AgentContinuationId
	content := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if handler.retryAgentRecoveryAfterInputTooLong("test", payload, account, &retrySameID, inputTooLong) {
		t.Fatal("second input-too-long response was allowed another aggressive fallback")
	}
	if payload.AgentInputTooLongRecoveries != maxKiroAgentInputTooLongRecoveries ||
		payload.ConversationState.AgentContinuationId != continuationID ||
		payload.ConversationState.CurrentMessage.UserInputMessage.Content != content {
		t.Fatalf("exhausted fallback mutated recovery state: retries=%d continuation=%q",
			payload.AgentInputTooLongRecoveries, payload.ConversationState.AgentContinuationId)
	}
}
