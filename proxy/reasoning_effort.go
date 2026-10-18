package proxy

import "strings"

const (
	// Enabled thinking uses the legacy budget form. Keep it bounded so a
	// client-supplied budget cannot turn into an unbounded reasoning pass.
	defaultThinkingBudgetTokens = 20_000
	maxThinkingBudgetTokens     = 24_576
)

// AdditionalModelRequestFields mirrors the mixed-case Kiro wire format. The
// outer payload key is camelCase, while output_config intentionally stays
// snake_case.
type AdditionalModelRequestFields struct {
	OutputConfig *KiroOutputConfig `json:"output_config,omitempty"`
	// MaxTokens is the Kiro additionalModelRequestFields.max_tokens value.
	// kiro2cc-proxy puts the client limit here and clamps it to at least 1024.
	MaxTokens int `json:"max_tokens,omitempty"`
}

type KiroOutputConfig struct {
	Effort string `json:"effort"`
}

func effortFromBudgetTokens(tokens int) string {
	tokens = clampThinkingBudgetTokens(tokens)
	switch {
	case tokens <= 4_000:
		return "low"
	case tokens <= 16_000:
		return "medium"
	default:
		return "high"
	}
}

func clampThinkingBudgetTokens(tokens int) int {
	if tokens <= 0 {
		return tokens
	}
	if tokens > maxThinkingBudgetTokens {
		return maxThinkingBudgetTokens
	}
	return tokens
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
	// Tool calls and structured output are scored on the answer itself. A
	// default effort=low makes those turns narrate or miss the schema when the
	// client did not ask for thinking. Named effort and real thinking turns
	// keep the kiro2cc low default.
	omitDefaultEffort := !thinking && explicitEffort == "" &&
		(claudeRequestHasClientTools(req) || claudeRequestHasStructuredOutput(req))
	return kiroAdditionalModelRequestFields(modelID, explicitEffort, omitDefaultEffort, req.MaxTokens)
}

// clampReferenceMaxTokens applies the Kiro additionalModelRequestFields limit
// used by kiro2cc-proxy: at least 1024, and at most 128000 for opus 4.7, 4.8,
// and opus 5, otherwise 64000.
func clampReferenceMaxTokens(modelID string, tokens int) int {
	if tokens < 1024 {
		tokens = 1024
	}
	limit := 64000
	model := strings.ToLower(modelID)
	if strings.Contains(model, "opus-4.7") || strings.Contains(model, "opus-4-7") ||
		strings.Contains(model, "opus-4.8") || strings.Contains(model, "opus-4-8") ||
		strings.Contains(model, "opus-5") || strings.Contains(model, "opus.5") ||
		strings.Contains(model, "opus 5") {
		limit = 128000
	}
	if tokens > limit {
		return limit
	}
	return tokens
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

	if strings.EqualFold(strings.TrimSpace(req.ReasoningEffort), "none") {
		return nil
	}
	explicitEffort, effortRequested := normalizeOpenAIReasoningEffort(modelID, req.ReasoningEffort)
	omitDefaultEffort := !thinking && !effortRequested &&
		(openAIRequestHasClientTools(req) || openAIRequestHasStructuredOutput(req))
	return kiroAdditionalModelRequestFields(modelID, explicitEffort, omitDefaultEffort, req.MaxTokens)
}

// kiroAdditionalModelRequestFields applies the kiro2cc default of effort=low
// unless omitDefaultEffort is set. The output limit still travels in
// max_tokens when the client supplied one.
func kiroAdditionalModelRequestFields(modelID, explicitEffort string, omitDefaultEffort bool, maxTokens int) *AdditionalModelRequestFields {
	if explicitEffort == "" && !omitDefaultEffort {
		explicitEffort = "low"
	}
	var fields *AdditionalModelRequestFields
	if explicitEffort != "" {
		fields = newReasoningFields(modelID, explicitEffort)
	} else {
		fields = &AdditionalModelRequestFields{}
	}
	if maxTokens > 0 {
		fields.MaxTokens = clampReferenceMaxTokens(modelID, maxTokens)
	}
	if fields.OutputConfig == nil && fields.MaxTokens == 0 {
		return nil
	}
	return fields
}

func claudeRequestHasClientTools(req *ClaudeRequest) bool {
	if req == nil {
		return false
	}
	for _, tool := range req.Tools {
		toolType := strings.TrimSpace(tool.Type)
		if toolType != "" && toolType != "custom" {
			continue
		}
		if strings.TrimSpace(tool.Name) != "" {
			return true
		}
	}
	return false
}

func claudeRequestHasStructuredOutput(req *ClaudeRequest) bool {
	return req != nil && req.OutputConfig != nil && req.OutputConfig.Format != nil
}

func openAIRequestHasClientTools(req *OpenAIRequest) bool {
	if req == nil {
		return false
	}
	for _, tool := range req.Tools {
		if isResponsesWebSearchTool(tool) {
			continue
		}
		if strings.TrimSpace(tool.Function.Name) != "" {
			return true
		}
	}
	return false
}

func openAIRequestHasStructuredOutput(req *OpenAIRequest) bool {
	if req == nil || req.ResponseFormat == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(req.ResponseFormat.Type)) {
	case "json_object", "json_schema":
		return true
	default:
		return false
	}
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
