package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"

	"github.com/google/uuid"
)

// This marker is deliberately distinguishable from an Anthropic signature.
// Kiro normally forwards the real upstream value; inventing a base64-shaped
// replacement would pass shallow checks but fail end-to-end signature replay.
const claudeThinkingSignaturePlaceholder = "kiro-go-thinking-signature-unavailable"

func claudeThinkingSignature(signature string) string {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return claudeThinkingSignaturePlaceholder
	}
	return signature
}

// claudeThinkingSignatureFallback returns a stable opaque signature-shaped
// marker scoped to the reasoning payload. It is used only when Kiro exposes
// thinking text without its upstream signature. Native signatures always win.
// The prefix keeps the fallback distinguishable for diagnostics/replay.
func claudeThinkingSignatureFallback(reasoning ClaudeReasoningResult) string {
	source := reasoning.Text + "\x00" + strings.Join(reasoning.Redacted, "\x00")
	digest := sha256.Sum256([]byte(source))
	return "kiro-go-unverified." + base64.RawStdEncoding.EncodeToString(digest[:])
}

func resolveClaudeThinkingSignature(reasoning ClaudeReasoningResult, _ bool) string {
	if signature := strings.TrimSpace(reasoning.Signature); signature != "" {
		return signature
	}
	if strings.TrimSpace(reasoning.Text) != "" || len(reasoning.Redacted) > 0 {
		return claudeThinkingSignatureFallback(reasoning)
	}
	return claudeThinkingSignaturePlaceholder
}

// normalizeClaudeToolUseID enforces Anthropic's client-tool identifier
// contract while preserving already-valid IDs. Kiro occasionally emits a
// provider-specific id (for example call_x); exposing that verbatim makes
// Veridrop/Anthropic SDKs reject an otherwise valid tool call.
func normalizeClaudeToolUseID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "toolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	}
	if strings.HasPrefix(id, "toolu_") || strings.HasPrefix(id, "srvtoolu_") {
		return id
	}
	return "toolu_" + id
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
