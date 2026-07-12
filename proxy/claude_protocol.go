package proxy

import "strings"

const claudeThinkingSignaturePlaceholder = "kiro-go-thinking-signature"

func claudeThinkingSignature(signature string) string {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return claudeThinkingSignaturePlaceholder
	}
	return signature
}

func claudeReasoningUsageDetails(reasoning ClaudeReasoningResult, includeEmptyThinkingBlock bool) *ClaudeOutputTokensDetails {
	hasReasoning := includeEmptyThinkingBlock || strings.TrimSpace(reasoning.Text) != "" || strings.TrimSpace(reasoning.Signature) != ""
	if !hasReasoning {
		for _, redacted := range reasoning.Redacted {
			if redacted != "" {
				hasReasoning = true
				break
			}
		}
	}
	if !hasReasoning {
		return nil
	}

	return &ClaudeOutputTokensDetails{
		ThinkingTokens: estimateClaudeOutputTokensWithRedacted("", reasoning.Text, reasoning.Redacted, nil),
	}
}

func addClaudeReasoningUsageDetails(usage map[string]interface{}, reasoning ClaudeReasoningResult, includeEmptyThinkingBlock bool) {
	if usage == nil {
		return
	}
	details := claudeReasoningUsageDetails(reasoning, includeEmptyThinkingBlock)
	if details == nil {
		return
	}
	usage["output_tokens_details"] = map[string]int{"thinking_tokens": details.ThinkingTokens}
}

func resolveClaudeStopReason(toolUseCount int, contextWindowExceeded bool, upstreamStopReason string) string {
	if contextWindowExceeded {
		return "model_context_window_exceeded"
	}
	switch strings.TrimSpace(upstreamStopReason) {
	case "max_tokens", "model_context_window_exceeded":
		return strings.TrimSpace(upstreamStopReason)
	}
	if toolUseCount > 0 {
		return "tool_use"
	}
	return "end_turn"
}
