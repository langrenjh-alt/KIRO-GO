package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

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

func toolResultsExactlyMatchUses(uses []KiroToolUse, results []KiroToolResult) bool {
	if len(uses) == 0 || len(uses) != len(results) {
		return false
	}
	usesByID := make(map[string]bool, len(uses))
	for _, use := range uses {
		id := strings.TrimSpace(use.ToolUseID)
		if id == "" || strings.TrimSpace(use.Name) == "" || usesByID[id] {
			return false
		}
		usesByID[id] = true
	}
	seen := make(map[string]bool, len(results))
	for _, result := range results {
		id := strings.TrimSpace(result.ToolUseID)
		if id == "" || seen[id] || !usesByID[id] {
			return false
		}
		seen[id] = true
	}
	return len(seen) == len(usesByID)
}

// validateFullKiroToolHistory verifies every structured tool round, including
// the final assistant round answered by the current user message. Kiro expects
// each result set to immediately follow the calls it answers.
func validateFullKiroToolHistory(payload *KiroPayload) (*KiroAssistantResponseMessage, bool) {
	if payload == nil {
		return nil, false
	}
	var pending *KiroAssistantResponseMessage
	for i := range payload.ConversationState.History {
		message := &payload.ConversationState.History[i]
		if (message.UserInputMessage == nil) == (message.AssistantResponseMessage == nil) {
			return nil, false
		}
		if assistant := message.AssistantResponseMessage; assistant != nil {
			if pending != nil {
				return nil, false
			}
			if len(assistant.ToolUses) > 0 {
				pending = assistant
			}
			continue
		}

		user := message.UserInputMessage
		var results []KiroToolResult
		if user.UserInputMessageContext != nil {
			results = user.UserInputMessageContext.ToolResults
			user.UserInputMessageContext.Tools = nil
		}
		if pending == nil {
			if len(results) > 0 {
				return nil, false
			}
			continue
		}
		if !toolResultsExactlyMatchUses(pending.ToolUses, results) {
			return nil, false
		}
		pending = nil
	}

	current := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	var currentResults []KiroToolResult
	if current != nil {
		currentResults = current.ToolResults
	}
	if pending == nil {
		return nil, len(currentResults) == 0
	}
	return pending, toolResultsExactlyMatchUses(pending.ToolUses, currentResults)
}

func ensureFullKiroHistoryToolConfig(payload *KiroPayload) bool {
	if payload == nil {
		return false
	}
	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil {
		current.UserInputMessageContext = &UserInputMessageContext{}
	}
	context := current.UserInputMessageContext
	available := make(map[string]bool, len(context.Tools))
	for _, tool := range context.Tools {
		name := strings.TrimSpace(tool.ToolSpecification.Name)
		if name == "" || available[name] {
			return false
		}
		available[name] = true
	}
	if payload.ToolNameMap == nil {
		payload.ToolNameMap = make(map[string]string)
	}

	for i := range payload.ConversationState.History {
		assistant := payload.ConversationState.History[i].AssistantResponseMessage
		if assistant == nil {
			continue
		}
		for j := range assistant.ToolUses {
			original := strings.TrimSpace(assistant.ToolUses[j].Name)
			if original == "" {
				return false
			}
			upstream := ""
			if available[original] {
				upstream = original
			} else {
				for candidate, mappedOriginal := range payload.ToolNameMap {
					if mappedOriginal == original && available[candidate] {
						if upstream != "" && upstream != candidate {
							return false
						}
						upstream = candidate
					}
				}
			}
			if upstream == "" {
				upstream = shortenToolName(sanitizeToolName(original))
				if mappedOriginal, occupied := payload.ToolNameMap[upstream]; occupied && mappedOriginal != original {
					return false
				}
				if !available[upstream] {
					wrapper := KiroToolWrapper{}
					wrapper.ToolSpecification.Name = upstream
					wrapper.ToolSpecification.Description = normalizeToolDesc("", upstream)
					wrapper.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{"type": "object"}}
					context.Tools = append(context.Tools, wrapper)
					available[upstream] = true
				}
				if upstream != original {
					payload.ToolNameMap[upstream] = original
				}
			}
			assistant.ToolUses[j].Name = upstream
		}
	}
	if len(payload.ToolNameMap) == 0 {
		payload.ToolNameMap = nil
	}
	if len(context.Tools) == 0 && len(context.ToolResults) == 0 {
		current.UserInputMessageContext = nil
	}
	return true
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
	return newUUIDv4FromMaterial(wire)
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
	for i := range payload.ConversationState.History {
		if assistant := payload.ConversationState.History[i].AssistantResponseMessage; assistant != nil {
			assistant.MessageID = ""
		}
	}
	payload.HistoryMode = kiroHistoryModeLegacy
}

// normalizeNativeKiroHistory removes only proxy-authored pollution while
// preserving native toolUse/toolResult blocks. Some compatible clients emit a
// tool-result message and a separate user continuation; Kiro expects one user
// turn after the assistant tool call, so those adjacent user messages are
// combined without flattening their structured results.
func normalizeNativeKiroHistory(history []KiroHistoryMessage) []KiroHistoryMessage {
	if len(history) == 0 {
		return history
	}

	cleaned := make([]KiroHistoryMessage, 0, len(history))
	for i := range history {
		message := history[i]
		if assistant := message.AssistantResponseMessage; assistant != nil {
			if assistant.Content != "" {
				assistant.Content = stripPollutedToolCallText(assistant.Content)
			}
			if len(assistant.ToolUses) == 0 {
				content := strings.TrimSpace(assistant.Content)
				if content == "" || content == minimalFallbackUserContent {
					continue
				}
			}
		}

		if user := message.UserInputMessage; user != nil {
			if context := user.UserInputMessageContext; context != nil {
				// Tool definitions belong to currentMessage. Historical user
				// contexts carry results only.
				context.Tools = nil
				if len(context.ToolResults) == 0 {
					user.UserInputMessageContext = nil
				}
			}
			if strings.TrimSpace(user.Content) == "" && len(user.Images) == 0 &&
				(user.UserInputMessageContext == nil || len(user.UserInputMessageContext.ToolResults) == 0) {
				user.Content = minimalFallbackUserContent
			}

			if len(cleaned) > 0 {
				previous := cleaned[len(cleaned)-1].UserInputMessage
				previousContext := (*UserInputMessageContext)(nil)
				if previous != nil {
					previousContext = previous.UserInputMessageContext
				}
				// Only coalesce the adjacent user messages that form a native
				// tool-result turn. Ordinary consecutive user messages are
				// separate causal turns and must remain separate.
				coalescesToolTurn := (previousContext != nil && len(previousContext.ToolResults) > 0) ||
					(user.UserInputMessageContext != nil && len(user.UserInputMessageContext.ToolResults) > 0)
				if previous != nil && coalescesToolTurn {
					previous.Content = joinHistoryText(previous.Content, user.Content)
					previous.Images = append(previous.Images, user.Images...)
					if user.UserInputMessageContext != nil && len(user.UserInputMessageContext.ToolResults) > 0 {
						if previous.UserInputMessageContext == nil {
							previous.UserInputMessageContext = &UserInputMessageContext{}
						}
						previous.UserInputMessageContext.ToolResults = append(
							previous.UserInputMessageContext.ToolResults,
							user.UserInputMessageContext.ToolResults...,
						)
					}
					continue
				}
			}
		}

		cleaned = append(cleaned, message)
	}
	return cleaned
}

// finalizeKiroHistoryProtocol makes the native branch fail closed. Every fully
// paired tool round remains structured, while opaque reasoning is replayed only
// for the active round answered by the current message.
func finalizeKiroHistoryProtocol(payload *KiroPayload) {
	if payload == nil || payload.HistoryMode != kiroHistoryModeNativeActive {
		return
	}
	payload.ConversationState.History = normalizeNativeKiroHistory(payload.ConversationState.History)
	if !ensureFullKiroHistoryToolConfig(payload) {
		fallbackKiroHistoryToLegacy(payload)
		return
	}
	active, ok := validateFullKiroToolHistory(payload)
	if !ok {
		fallbackKiroHistoryToLegacy(payload)
		return
	}

	for i := range payload.ConversationState.History {
		assistant := payload.ConversationState.History[i].AssistantResponseMessage
		if assistant == nil {
			continue
		}
		if assistant != active || assistant.ReasoningContent == nil ||
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
