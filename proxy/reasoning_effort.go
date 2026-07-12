package proxy

import "strings"

// AdditionalModelRequestFields mirrors the mixed-case Kiro wire format. The
// outer payload key is camelCase, while output_config intentionally stays
// snake_case.
type AdditionalModelRequestFields struct {
	OutputConfig *KiroOutputConfig `json:"output_config,omitempty"`
}

type KiroOutputConfig struct {
	Effort string `json:"effort"`
}

func effortFromBudgetTokens(tokens int) string {
	switch {
	case tokens <= 4_000:
		return "low"
	case tokens <= 16_000:
		return "medium"
	case tokens <= 64_000:
		return "high"
	default:
		return "xhigh"
	}
}

func normalizeEffortForModel(modelID, rawEffort string) string {
	var effort string
	switch strings.ToLower(strings.TrimSpace(rawEffort)) {
	case "low":
		effort = "low"
	case "medium":
		effort = "medium"
	case "high":
		effort = "high"
	case "xhigh", "x-high", "x_high":
		effort = "xhigh"
	case "max":
		effort = "max"
	default:
		effort = "high"
	}

	if effort == "xhigh" && !modelSupportsXHighEffort(modelID) {
		return "high"
	}
	return effort
}

// modelSupportsNativeReasoning is deliberately an allowlist. Kiro rejects
// additionalModelRequestFields for unconfirmed model families with HTTP 400.
func modelSupportsNativeReasoning(modelID string) bool {
	m := canonicalReasoningModelID(modelID)
	switch m {
	case "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8", "claude-sonnet-4.6":
		return true
	}

	for _, family := range []string{
		"claude-fable-5",
		"claude-mythos-5",
		"claude-sonnet-5",
		"claude-opus-5",
		"claude-5",
	} {
		if m == family || strings.HasPrefix(m, family+".") || strings.HasPrefix(m, family+"-") {
			return true
		}
	}
	return false
}

func modelSupportsXHighEffort(modelID string) bool {
	switch canonicalReasoningModelID(modelID) {
	case "claude-opus-4.6",
		"claude-sonnet-4.6",
		"claude-opus-4.5",
		"claude-sonnet-4.5",
		"claude-haiku-4.5":
		return false
	default:
		return true
	}
}

func canonicalReasoningModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(MapModel(modelID)))
}

func newReasoningFields(modelID, effort string) *AdditionalModelRequestFields {
	return &AdditionalModelRequestFields{
		OutputConfig: &KiroOutputConfig{
			Effort: normalizeEffortForModel(modelID, effort),
		},
	}
}

func buildClaudeAdditionalModelRequestFields(req *ClaudeRequest, modelID string, thinking bool) *AdditionalModelRequestFields {
	if req == nil || !modelSupportsNativeReasoning(modelID) {
		return nil
	}

	thinkingType := ""
	if req.Thinking != nil {
		thinkingType = strings.ToLower(strings.TrimSpace(req.Thinking.Type))
		if thinkingType == "disabled" {
			return nil
		}
	}

	// Opus 4.6 only accepts output_config when adaptive thinking is active.
	if canonicalReasoningModelID(modelID) == "claude-opus-4.6" && thinkingType != "adaptive" {
		return nil
	}

	explicitEffort := ""
	if req.OutputConfig != nil {
		explicitEffort = strings.TrimSpace(req.OutputConfig.Effort)
	}
	if !thinking && !isClaudeThinkingRequested(req.Thinking) && explicitEffort == "" {
		return nil
	}

	if explicitEffort != "" {
		return newReasoningFields(modelID, explicitEffort)
	}
	if thinkingType == "enabled" && req.Thinking.BudgetTokens > 0 {
		return newReasoningFields(modelID, effortFromBudgetTokens(req.Thinking.BudgetTokens))
	}
	return newReasoningFields(modelID, "high")
}

func buildOpenAIAdditionalModelRequestFields(req *OpenAIRequest, modelID string, thinking bool) *AdditionalModelRequestFields {
	if req == nil || !modelSupportsNativeReasoning(modelID) {
		return nil
	}

	// Chat Completions and Responses have no adaptive-thinking discriminator,
	// so they cannot safely enable the Opus 4.6-only wire mode.
	if canonicalReasoningModelID(modelID) == "claude-opus-4.6" {
		return nil
	}

	explicitEffort, effortRequested := normalizeOpenAIReasoningEffort(modelID, req.ReasoningEffort)
	if !thinking && explicitEffort == "" {
		return nil
	}
	if !effortRequested && explicitEffort == "" {
		explicitEffort = "high"
	}
	if explicitEffort == "" {
		return nil
	}
	return newReasoningFields(modelID, explicitEffort)
}

func normalizeOpenAIReasoningEffort(modelID, rawEffort string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(rawEffort)) {
	case "":
		return "", false
	case "none":
		return "", true
	case "minimal":
		return "low", true
	default:
		return normalizeEffortForModel(modelID, rawEffort), true
	}
}

func resolveClaudeRequestThinkingMode(req *ClaudeRequest, thinkingSuffix string) (string, bool) {
	if req == nil {
		return "", false
	}
	modelID, suffixThinking := ParseModelAndThinking(req.Model, thinkingSuffix)
	if suffixThinking && req.Thinking == nil && canonicalReasoningModelID(modelID) == "claude-opus-4.6" {
		req.Thinking = &ClaudeThinkingConfig{Type: "adaptive"}
	}
	thinking := suffixThinking || isClaudeThinkingRequested(req.Thinking)
	thinkingDisabled := req.Thinking != nil && strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "disabled")
	if !thinkingDisabled && req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != "" {
		thinking = true
	}
	return modelID, thinking
}

func resolveOpenAIThinkingMode(model, reasoningEffort, thinkingSuffix string) (string, bool) {
	modelID, thinking := ParseModelAndThinking(model, thinkingSuffix)
	switch strings.ToLower(strings.TrimSpace(reasoningEffort)) {
	case "none":
		return modelID, false
	case "":
		return modelID, thinking
	default:
		return modelID, true
	}
}
