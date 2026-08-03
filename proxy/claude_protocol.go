package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

const (
	claudeOpaqueIDSuffixLength = 24
	claudeOpaqueIDVersion      = "01"
	claudeMessageIDVersion     = "011Cdd"
)

var claudeOpaqueIDAlphabet = []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")

// newClaudeOpaqueID matches the opaque, fixed-width identifiers returned by
// Anthropic-compatible endpoints without leaking an upstream provider's ID.
func newClaudeOpaqueID(prefix string) string {
	return newClaudeOpaqueIDWithVersion(prefix, claudeOpaqueIDVersion)
}

func newClaudeOpaqueIDWithVersion(prefix, version string) string {
	random := make([]byte, claudeOpaqueIDSuffixLength-len(version))
	if _, err := rand.Read(random); err != nil {
		digest := sha256.Sum256([]byte(prefix + version))
		for i := range random {
			random[i] = claudeOpaqueIDAlphabet[int(digest[i])%len(claudeOpaqueIDAlphabet)]
		}
		return prefix + version + string(random)
	}
	for i := range random {
		random[i] = claudeOpaqueIDAlphabet[int(random[i])%len(claudeOpaqueIDAlphabet)]
	}
	return prefix + version + string(random)
}

func newClaudeMessageID() string {
	return newClaudeOpaqueIDWithVersion("msg_", claudeMessageIDVersion)
}

func newClaudeServerToolUseID() string {
	return newClaudeOpaqueID("srvtoolu_")
}

func newClaudeContainerID() string {
	return newClaudeOpaqueIDWithVersion("container_", claudeMessageIDVersion)
}

// This marker remains deliberately distinguishable from an Anthropic
// signature when Kiro does not provide any protocol envelope to transform.
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

func resolveClaudeThinkingSignature(reasoning ClaudeReasoningResult, _ bool, models ...string) string {
	model := ""
	if len(models) > 0 {
		model = models[0]
	}
	if signature := strings.TrimSpace(reasoning.Signature); signature != "" {
		return camouflageClaudeThinkingSignature(signature, model)
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
	if validClaudeOpaqueID(id, "toolu_") || validClaudeOpaqueID(id, "srvtoolu_") {
		return id
	}
	return newClaudeOpaqueID("toolu_")
}

func validClaudeOpaqueID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+claudeOpaqueIDSuffixLength {
		return false
	}
	if id[len(prefix):len(prefix)+len(claudeOpaqueIDVersion)] != claudeOpaqueIDVersion {
		return false
	}
	for i := len(prefix); i < len(id); i++ {
		char := id[i]
		if (char >= '0' && char <= '9') || (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') {
			continue
		}
		return false
	}
	return true
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
	details := &ClaudeOutputTokensDetails{}
	if hasReasoning {
		details.ThinkingTokens = estimateClaudeOutputTokensWithRedacted("", reasoning.Text, reasoning.Redacted, nil)
	}
	return details
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
