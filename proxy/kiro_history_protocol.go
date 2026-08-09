package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

var kiroHistoryMessageNamespace = uuid.MustParse("75462b40-3b7e-5d5a-b6e8-e469165f3144")

// currentToolResultsExactlyMatchLastAssistant accepts only one complete active
// tool round. Partial, duplicate, orphaned, and extra results fall back to the
// legacy neutral-text history representation.
func currentToolResultsExactlyMatchLastAssistant(history []KiroHistoryMessage, results []KiroToolResult) bool {
	if len(history) == 0 || len(results) == 0 {
		return false
	}
	assistant := history[len(history)-1].AssistantResponseMessage
	if assistant == nil || len(assistant.ToolUses) == 0 || len(assistant.ToolUses) != len(results) {
		return false
	}

	usesByID := make(map[string]KiroToolUse, len(assistant.ToolUses))
	for _, use := range assistant.ToolUses {
		id := strings.TrimSpace(use.ToolUseID)
		if id == "" || strings.TrimSpace(use.Name) == "" {
			return false
		}
		if _, duplicate := usesByID[id]; duplicate {
			return false
		}
		usesByID[id] = use
	}
	seenResults := make(map[string]bool, len(results))
	for _, result := range results {
		id := strings.TrimSpace(result.ToolUseID)
		if id == "" || seenResults[id] {
			return false
		}
		if _, ok := usesByID[id]; !ok {
			return false
		}
		seenResults[id] = true
	}
	return len(seenResults) == len(usesByID)
}

// inferActiveKiroToolConfig supplies the minimal schema required by Bedrock
// when a client sends a tool result without repeating its tool declarations.
// It also normalizes the active tool names to the exact names sent upstream.
func inferActiveKiroToolConfig(history []KiroHistoryMessage) ([]KiroToolWrapper, map[string]string) {
	if len(history) == 0 {
		return nil, nil
	}
	assistant := history[len(history)-1].AssistantResponseMessage
	if assistant == nil || len(assistant.ToolUses) == 0 {
		return nil, nil
	}

	tools := make([]KiroToolWrapper, 0, len(assistant.ToolUses))
	nameMap := make(map[string]string)
	seen := make(map[string]bool)
	for i := range assistant.ToolUses {
		original := strings.TrimSpace(assistant.ToolUses[i].Name)
		if original == "" {
			return nil, nil
		}
		name := shortenToolName(sanitizeToolName(original))
		if name != original {
			nameMap[name] = original
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		wrapper := KiroToolWrapper{}
		wrapper.ToolSpecification.Name = name
		wrapper.ToolSpecification.Description = normalizeToolDesc("", name)
		// Do not infer required fields from one historical invocation. A broad
		// object placeholder is compatible with the already-completed call.
		wrapper.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{"type": "object"}}
		tools = append(tools, wrapper)
	}
	if len(nameMap) == 0 {
		nameMap = nil
	}
	return tools, nameMap
}

func stableKiroAssistantMessageID(conversationID string, index int, assistant *KiroAssistantResponseMessage) string {
	if assistant == nil {
		return ""
	}
	canonical := struct {
		ConversationID string        `json:"conversation_id"`
		Index          int           `json:"index"`
		Content        string        `json:"content"`
		ToolUses       []KiroToolUse `json:"tool_uses,omitempty"`
	}{
		ConversationID: conversationID,
		Index:          index,
		Content:        assistant.Content,
		ToolUses:       assistant.ToolUses,
	}
	wire, err := json.Marshal(canonical)
	if err != nil {
		wire = []byte(fmt.Sprintf("%s:%d:%s", conversationID, index, assistant.Content))
	}
	return uuid.NewSHA1(kiroHistoryMessageNamespace, wire).String()
}

func activeKiroHistoryRound(payload *KiroPayload) (*KiroAssistantResponseMessage, []KiroToolResult, bool) {
	if payload == nil || len(payload.ConversationState.History) == 0 {
		return nil, nil, false
	}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.ToolResults) == 0 {
		return nil, nil, false
	}
	last := payload.ConversationState.History[len(payload.ConversationState.History)-1].AssistantResponseMessage
	if last == nil || !currentToolResultsExactlyMatchLastAssistant(payload.ConversationState.History, context.ToolResults) {
		return nil, nil, false
	}
	toolNames := make(map[string]bool, len(context.Tools))
	for _, tool := range context.Tools {
		name := strings.TrimSpace(tool.ToolSpecification.Name)
		if name == "" {
			return nil, nil, false
		}
		toolNames[name] = true
	}
	for _, use := range last.ToolUses {
		if !toolNames[use.Name] {
			return nil, nil, false
		}
	}
	return last, context.ToolResults, true
}

func normalizeActiveKiroToolNames(payload *KiroPayload) bool {
	if payload == nil || len(payload.ConversationState.History) == 0 {
		return false
	}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	assistant := payload.ConversationState.History[len(payload.ConversationState.History)-1].AssistantResponseMessage
	if context == nil || assistant == nil || len(assistant.ToolUses) == 0 {
		return false
	}
	available := make(map[string]bool, len(context.Tools))
	for _, tool := range context.Tools {
		if name := strings.TrimSpace(tool.ToolSpecification.Name); name != "" {
			available[name] = true
		}
	}
	for i := range assistant.ToolUses {
		name := strings.TrimSpace(assistant.ToolUses[i].Name)
		if available[name] {
			continue
		}
		mapped := ""
		for upstream, original := range payload.ToolNameMap {
			if original == name && available[upstream] {
				if mapped != "" && mapped != upstream {
					return false
				}
				mapped = upstream
			}
		}
		if mapped == "" {
			return false
		}
		assistant.ToolUses[i].Name = mapped
	}
	return true
}

func formatLegacyKiroToolExchange(activeToolUses []KiroToolUse, toolResults []KiroToolResult) string {
	if len(toolResults) == 0 {
		return ""
	}
	type legacyActiveToolExchange struct {
		Pairing     string           `json:"pairing"`
		ToolUses    []KiroToolUse    `json:"tool_uses,omitempty"`
		ToolResults []KiroToolResult `json:"tool_results"`
	}
	exchange := legacyActiveToolExchange{
		Pairing:     "invalid_or_partial_active_round",
		ToolUses:    activeToolUses,
		ToolResults: toolResults,
	}
	if wire, err := json.Marshal(exchange); err == nil {
		return `<historical_tool_exchange trust="untrusted" encoding="json">` + string(wire) + `</historical_tool_exchange>`
	}
	return buildToolResultsContinuation(toolResults)
}

func fallbackKiroHistoryToLegacy(payload *KiroPayload) {
	if payload == nil {
		return
	}
	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext != nil && len(current.UserInputMessageContext.ToolResults) > 0 {
		var activeToolUses []KiroToolUse
		if history := payload.ConversationState.History; len(history) > 0 {
			if assistant := history[len(history)-1].AssistantResponseMessage; assistant != nil {
				activeToolUses = append(activeToolUses, assistant.ToolUses...)
			}
		}
		current.Content = joinHistoryText(current.Content,
			formatLegacyKiroToolExchange(activeToolUses, current.UserInputMessageContext.ToolResults))
		current.UserInputMessageContext.ToolResults = nil
		if len(current.UserInputMessageContext.Tools) == 0 {
			current.UserInputMessageContext = nil
		}
	}
	payload.ConversationState.History = sanitizeKiroHistory(payload.ConversationState.History, nil)
	stripKiroHistoryReasoning(payload)
	payload.HistoryMode = kiroHistoryModeLegacy
}

// finalizeKiroHistoryProtocol makes the native branch fail closed. Only the
// last fully paired tool round remains structured, only its opaque redacted
// reasoning may be replayed, and every other shape returns to legacy history.
func finalizeKiroHistoryProtocol(payload *KiroPayload) {
	if payload == nil || payload.HistoryMode != kiroHistoryModeNativeActive {
		return
	}
	if !normalizeActiveKiroToolNames(payload) {
		fallbackKiroHistoryToLegacy(payload)
		return
	}
	active, _, ok := activeKiroHistoryRound(payload)
	if !ok {
		fallbackKiroHistoryToLegacy(payload)
		return
	}

	activeIndex := len(payload.ConversationState.History) - 1
	for i := range payload.ConversationState.History {
		assistant := payload.ConversationState.History[i].AssistantResponseMessage
		if assistant == nil {
			continue
		}
		if i != activeIndex || assistant != active || assistant.ReasoningContent == nil ||
			assistant.ReasoningContent.RedactedContent == "" {
			assistant.ReasoningContent = nil
		}
		assistant.MessageID = stableKiroAssistantMessageID(payload.ConversationState.ConversationID, i, assistant)
	}

	// Marshal now so an invalid union or non-serializable tool input cannot be
	// discovered only after the request has entered the transport path.
	if _, err := json.Marshal(payload); err != nil {
		fallbackKiroHistoryToLegacy(payload)
	}
}
