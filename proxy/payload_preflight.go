package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"kiro-go/logger"
)

const (
	defaultKiroMaxRawBodyBytes   = 900 << 10
	defaultKiroInputTokenReserve = 8 << 10
	kiroHistoryTruncationNote    = "[Earlier conversation history was truncated to fit the upstream input limits. Older messages and tool activity were omitted.]"
	kiroHistoryTruncationAck     = "Understood."
)

// KiroPayloadBreakdown contains size and count diagnostics only. It never
// retains prompt text, tool descriptions, schemas, image data, or tool output.
type KiroPayloadBreakdown struct {
	TotalBytes           int
	EstimatedInputTokens int
	HistoryBytes         int
	CurrentBytes         int
	SystemBytes          int
	ToolsBytes           int
	SchemaBytes          int
	ImagesBytes          int
	FixedBytes           int
	HistoryEntries       int
	ProtectedEntries     int
	ToolCount            int
	SchemaCount          int
	ImageCount           int
}

// KiroPayloadPreflightError is a local, non-retryable client request error.
// The wire-facing message is deliberately limited to numeric diagnostics.
type KiroPayloadPreflightError struct {
	OriginalTotalBytes           int
	OriginalEstimatedInputTokens int
	RawBodyLimitBytes            int
	ModelInputLimitTokens        int
	InputTokenBudget             int
	DroppedHistoryEntries        int
	Breakdown                    KiroPayloadBreakdown
}

func (e *KiroPayloadPreflightError) Error() string {
	if e == nil {
		return "Kiro request failed local input preflight"
	}
	b := e.Breakdown
	return fmt.Sprintf(
		"Kiro request exceeds local input limits after safe history trimming: original_total_bytes=%d original_estimated_input_tokens=%d total_bytes=%d raw_body_limit_bytes=%d estimated_input_tokens=%d model_input_limit_tokens=%d input_token_budget=%d history_bytes=%d current_bytes=%d system_bytes=%d tools_bytes=%d schema_bytes=%d images_bytes=%d fixed_bytes=%d history_entries=%d protected_history_entries=%d dropped_history_entries=%d tool_count=%d schema_count=%d image_count=%d",
		e.OriginalTotalBytes,
		e.OriginalEstimatedInputTokens,
		b.TotalBytes,
		e.RawBodyLimitBytes,
		b.EstimatedInputTokens,
		e.ModelInputLimitTokens,
		e.InputTokenBudget,
		b.HistoryBytes,
		b.CurrentBytes,
		b.SystemBytes,
		b.ToolsBytes,
		b.SchemaBytes,
		b.ImagesBytes,
		b.FixedBytes,
		b.HistoryEntries,
		b.ProtectedEntries,
		e.DroppedHistoryEntries,
		b.ToolCount,
		b.SchemaCount,
		b.ImageCount,
	)
}

type kiroPayloadPreflightResult struct {
	Before                KiroPayloadBreakdown
	After                 KiroPayloadBreakdown
	RawBodyLimitBytes     int
	ModelInputLimitTokens int
	InputTokenBudget      int
	DroppedHistoryEntries int
}

func preflightKiroPayload(payload *KiroPayload) (kiroPayloadPreflightResult, error) {
	result := kiroPayloadPreflightResult{}
	if payload == nil {
		return result, errors.New("Kiro request payload is nil")
	}

	model := payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
	result.RawBodyLimitBytes = positiveEnvInt("KIRO_MAX_RAW_BODY_BYTES", defaultKiroMaxRawBodyBytes)
	result.ModelInputLimitTokens = lookupModelTokenLimits(model).MaxInputTokens
	reserve := nonNegativeEnvInt("KIRO_INPUT_TOKEN_RESERVE", defaultKiroInputTokenReserve)
	result.InputTokenBudget = result.ModelInputLimitTokens - reserve
	if result.InputTokenBudget <= 0 {
		result.InputTokenBudget = result.ModelInputLimitTokens
	}

	before, err := measureKiroPayloadTotals(payload)
	if err != nil {
		return result, err
	}
	originalTotalBytes := before.TotalBytes
	originalEstimatedTokens := before.EstimatedInputTokens
	result.Before = before
	result.After = before
	setKiroPayloadAttemptSize(payload, before)
	if kiroPayloadFitsPreflight(before, result.RawBodyLimitBytes, result.InputTokenBudget) {
		return result, nil
	}
	// Images are not part of history trimming. Shrink them before dropping
	// conversation turns so a pair of screenshots can fit the raw body limit
	// without discarding the rest of the request.
	if result.RawBodyLimitBytes > 0 && before.TotalBytes > result.RawBodyLimitBytes {
		if _, shrinkErr := compressKiroPayloadImages(payload, result.RawBodyLimitBytes); shrinkErr != nil {
			return result, shrinkErr
		}
		before, err = measureKiroPayloadTotals(payload)
		if err != nil {
			return result, err
		}
		result.Before = before
		result.After = before
		setKiroPayloadAttemptSize(payload, before)
		if kiroPayloadFitsPreflight(before, result.RawBodyLimitBytes, result.InputTokenBudget) {
			after, measureErr := measureKiroPayloadBreakdown(payload)
			if measureErr != nil {
				return result, measureErr
			}
			result.After = after
			return result, nil
		}
	}
	before, err = measureKiroPayloadBreakdown(payload)
	if err != nil {
		return result, err
	}
	result.Before = before
	result.After = before

	originalHistory := payload.ConversationState.History
	primingCount := kiroSystemPrimingPrefixLen(originalHistory)
	conversation := originalHistory[primingCount:]
	activeToolTurn := kiroActiveToolHistoryIndex(payload, primingCount)
	maxDrop := len(conversation)
	if activeToolTurn >= 0 {
		maxDrop = activeToolTurn
	}

	validDrops := []int{0}
	for drop := 1; drop <= maxDrop; drop++ {
		if drop == len(conversation) ||
			(activeToolTurn >= 0 && drop == activeToolTurn) ||
			(drop < len(conversation) && conversation[drop].UserInputMessage != nil) {
			validDrops = append(validDrops, drop)
		}
	}

	var selectedHistory []KiroHistoryMessage
	selectedDrop := -1
	low, high := 1, len(validDrops)-1
	for low <= high {
		mid := low + (high-low)/2
		drop := validDrops[mid]
		candidateHistory := kiroTrimmedHistory(originalHistory, primingCount, drop)
		candidate := kiroPayloadWithHistory(payload, candidateHistory)
		candidateBreakdown, candidateErr := measureKiroPayloadTotals(candidate)
		if candidateErr != nil {
			return result, candidateErr
		}
		if kiroPayloadFitsPreflight(candidateBreakdown, result.RawBodyLimitBytes, result.InputTokenBudget) {
			selectedDrop = drop
			selectedHistory = candidateHistory
			high = mid - 1
		} else {
			low = mid + 1
		}
	}

	if selectedDrop >= 0 {
		payload.ConversationState.History = selectedHistory
		after, measureErr := measureKiroPayloadBreakdown(payload)
		if measureErr != nil {
			return result, measureErr
		}
		result.After = after
		result.DroppedHistoryEntries = selectedDrop
		setKiroPayloadAttemptSize(payload, after)
		logger.Infof(
			"[KiroPreflight] trimmed request_id=%s original_bytes=%d final_bytes=%d original_tokens=%d final_tokens=%d raw_limit_bytes=%d model_limit_tokens=%d token_budget=%d dropped_history_entries=%d remaining_history_entries=%d tools=%d schemas=%d images=%d",
			kiroPayloadRequestID(payload), before.TotalBytes, after.TotalBytes,
			before.EstimatedInputTokens, after.EstimatedInputTokens,
			result.RawBodyLimitBytes, result.ModelInputLimitTokens, result.InputTokenBudget,
			result.DroppedHistoryEntries, after.HistoryEntries, after.ToolCount, after.SchemaCount, after.ImageCount,
		)
		return result, nil
	}

	minimalDrop := 0
	minimalHistory := originalHistory
	if len(validDrops) > 1 {
		minimalDrop = validDrops[len(validDrops)-1]
		minimalHistory = kiroTrimmedHistory(originalHistory, primingCount, minimalDrop)
	}
	minimalPayload := kiroPayloadWithHistory(payload, minimalHistory)
	minimalBreakdown, measureErr := measureKiroPayloadBreakdown(minimalPayload)
	if measureErr != nil {
		return result, measureErr
	}
	result.After = minimalBreakdown
	result.DroppedHistoryEntries = minimalDrop
	setKiroPayloadAttemptSize(payload, minimalBreakdown)
	preflightErr := &KiroPayloadPreflightError{
		OriginalTotalBytes:           originalTotalBytes,
		OriginalEstimatedInputTokens: originalEstimatedTokens,
		RawBodyLimitBytes:            result.RawBodyLimitBytes,
		ModelInputLimitTokens:        result.ModelInputLimitTokens,
		InputTokenBudget:             result.InputTokenBudget,
		DroppedHistoryEntries:        minimalDrop,
		Breakdown:                    minimalBreakdown,
	}
	logger.Warnf("[KiroPreflight] rejected request_id=%s: %v", kiroPayloadRequestID(payload), preflightErr)
	return result, preflightErr
}

func kiroPayloadFitsPreflight(b KiroPayloadBreakdown, rawBodyLimit, tokenBudget int) bool {
	return (rawBodyLimit <= 0 || b.TotalBytes <= rawBodyLimit) &&
		(tokenBudget <= 0 || b.EstimatedInputTokens <= tokenBudget)
}

func setKiroPayloadAttemptSize(payload *KiroPayload, b KiroPayloadBreakdown) {
	if payload == nil {
		return
	}
	payload.lastAttemptPayloadBytes = b.TotalBytes
	payload.lastAttemptEstimatedInputTokens = b.EstimatedInputTokens
}

func kiroPayloadRequestID(payload *KiroPayload) string {
	if payload == nil || strings.TrimSpace(payload.requestID) == "" {
		return "-"
	}
	return payload.requestID
}

func kiroSystemPrimingPrefixLen(history []KiroHistoryMessage) int {
	if len(history) < 2 || history[0].UserInputMessage == nil || history[1].AssistantResponseMessage == nil {
		return 0
	}
	if strings.TrimSpace(history[1].AssistantResponseMessage.Content) != "I will follow these instructions." {
		return 0
	}
	return 2
}

func kiroActiveToolHistoryIndex(payload *KiroPayload, primingCount int) int {
	if payload == nil || len(payload.ConversationState.History) <= primingCount {
		return -1
	}
	currentContext := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if currentContext == nil {
		return -1
	}
	resultIDs := collectToolResultIDs(currentContext.ToolResults)
	if !currentToolResultsMatchLastAssistant(payload.ConversationState.History, resultIDs) {
		return -1
	}
	return len(payload.ConversationState.History) - primingCount - 1
}

func kiroTrimmedHistory(history []KiroHistoryMessage, primingCount, drop int) []KiroHistoryMessage {
	if drop <= 0 {
		return history
	}
	priming := history[:primingCount]
	conversation := history[primingCount:]
	if drop > len(conversation) {
		drop = len(conversation)
	}
	tail := conversation[drop:]
	rebuilt := make([]KiroHistoryMessage, 0, len(priming)+len(tail)+2)
	rebuilt = append(rebuilt, priming...)

	if len(tail) > 0 && tail[0].UserInputMessage != nil {
		first := tail[0]
		user := *first.UserInputMessage
		if !strings.Contains(user.Content, kiroHistoryTruncationNote) {
			user.Content = joinHistoryText(kiroHistoryTruncationNote, user.Content)
		}
		first.UserInputMessage = &user
		rebuilt = append(rebuilt, first)
		rebuilt = append(rebuilt, tail[1:]...)
		return rebuilt
	}

	modelID := ""
	origin := "AI_EDITOR"
	if len(history) > 0 {
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].UserInputMessage != nil {
				modelID = history[i].UserInputMessage.ModelID
				if history[i].UserInputMessage.Origin != "" {
					origin = history[i].UserInputMessage.Origin
				}
				break
			}
		}
	}
	rebuilt = append(rebuilt, KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
		Content: kiroHistoryTruncationNote,
		ModelID: modelID,
		Origin:  origin,
	}})
	if len(tail) == 0 {
		rebuilt = append(rebuilt, KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content: kiroHistoryTruncationAck,
		}})
	}
	rebuilt = append(rebuilt, tail...)
	return rebuilt
}

func kiroPayloadWithHistory(payload *KiroPayload, history []KiroHistoryMessage) *KiroPayload {
	candidate := *payload
	candidate.ConversationState = payload.ConversationState
	candidate.ConversationState.History = history
	return &candidate
}

func estimateKiroPayloadWire(payload *KiroPayload) ([]byte, int, error) {
	wire, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	tokens := estimateApproxTokens(string(wire))
	if conservative := (len(wire) + agentRecoveryBytesPerToken - 1) / agentRecoveryBytesPerToken; conservative > tokens {
		tokens = conservative
	}
	return wire, tokens, nil
}

func measureKiroPayloadTotals(payload *KiroPayload) (KiroPayloadBreakdown, error) {
	var result KiroPayloadBreakdown
	wire, tokens, err := estimateKiroPayloadWire(payload)
	if err != nil {
		return result, err
	}
	result.TotalBytes = len(wire)
	result.EstimatedInputTokens = tokens
	result.HistoryEntries = len(payload.ConversationState.History)
	return result, nil
}

func measureKiroPayloadBreakdown(payload *KiroPayload) (KiroPayloadBreakdown, error) {
	var result KiroPayloadBreakdown
	wire, tokens, err := estimateKiroPayloadWire(payload)
	if err != nil {
		return result, err
	}
	result.TotalBytes = len(wire)
	result.EstimatedInputTokens = tokens
	result.HistoryEntries = len(payload.ConversationState.History)
	primingCount := kiroSystemPrimingPrefixLen(payload.ConversationState.History)
	result.ProtectedEntries = primingCount
	if kiroActiveToolHistoryIndex(payload, primingCount) >= 0 {
		result.ProtectedEntries++
	}

	working, err := cloneKiroPayloadForMeasurement(wire)
	if err != nil {
		return result, err
	}
	previous := len(wire)

	result.ImageCount = stripKiroPayloadImages(working)
	current, err := marshaledKiroPayloadSize(working)
	if err != nil {
		return result, err
	}
	result.ImagesBytes = positiveDifference(previous, current)
	previous = current

	result.ToolCount, result.SchemaCount = stripKiroPayloadSchemas(working)
	current, err = marshaledKiroPayloadSize(working)
	if err != nil {
		return result, err
	}
	result.SchemaBytes = positiveDifference(previous, current)
	previous = current

	stripKiroPayloadTools(working)
	current, err = marshaledKiroPayloadSize(working)
	if err != nil {
		return result, err
	}
	result.ToolsBytes = positiveDifference(previous, current)
	previous = current

	working.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{}
	current, err = marshaledKiroPayloadSize(working)
	if err != nil {
		return result, err
	}
	result.CurrentBytes = positiveDifference(previous, current)
	previous = current

	history := working.ConversationState.History
	if primingCount > len(history) {
		primingCount = len(history)
	}
	working.ConversationState.History = history[primingCount:]
	current, err = marshaledKiroPayloadSize(working)
	if err != nil {
		return result, err
	}
	result.SystemBytes = positiveDifference(previous, current)
	previous = current

	working.ConversationState.History = nil
	current, err = marshaledKiroPayloadSize(working)
	if err != nil {
		return result, err
	}
	result.HistoryBytes = positiveDifference(previous, current)
	result.FixedBytes = current
	return result, nil
}

func cloneKiroPayloadForMeasurement(wire []byte) (*KiroPayload, error) {
	var clone KiroPayload
	if err := json.Unmarshal(wire, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func marshaledKiroPayloadSize(payload *KiroPayload) (int, error) {
	wire, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	return len(wire), nil
}

func stripKiroPayloadImages(payload *KiroPayload) int {
	if payload == nil {
		return 0
	}
	count := len(payload.ConversationState.CurrentMessage.UserInputMessage.Images)
	payload.ConversationState.CurrentMessage.UserInputMessage.Images = nil
	for i := range payload.ConversationState.History {
		if user := payload.ConversationState.History[i].UserInputMessage; user != nil {
			count += len(user.Images)
			user.Images = nil
		}
	}
	return count
}

func stripKiroPayloadSchemas(payload *KiroPayload) (int, int) {
	if payload == nil {
		return 0, 0
	}
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil {
		return 0, 0
	}
	toolCount := len(ctx.Tools)
	schemaCount := 0
	for i := range ctx.Tools {
		if ctx.Tools[i].ToolSpecification.InputSchema.JSON != nil {
			schemaCount++
		}
		ctx.Tools[i].ToolSpecification.InputSchema.JSON = nil
	}
	return toolCount, schemaCount
}

func stripKiroPayloadTools(payload *KiroPayload) {
	if payload == nil {
		return
	}
	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil {
		return
	}
	current.UserInputMessageContext.Tools = nil
	if len(current.UserInputMessageContext.ToolResults) == 0 {
		current.UserInputMessageContext = nil
	}
}

func positiveDifference(before, after int) int {
	if before <= after {
		return 0
	}
	return before - after
}
