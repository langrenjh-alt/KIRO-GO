package proxy

import (
	"context"

	"kiro-go/config"
)

func (h *Handler) prepareResponsesPromptCache(
	req *ResponsesRequest,
	openAIReq *OpenAIRequest,
	ctx context.Context,
	thinking bool,
	apiKeyID string,
	providedKey string,
	estimatedInputTokens int,
	policy config.PromptCachePolicy,
	requestBody []byte,
) {
	if h == nil || req == nil || openAIReq == nil {
		return
	}
	tracker := h.promptCache
	if policy.ID != "" {
		tracker = h.promptCacheTrackerForPolicy(policy)
	}
	if tracker == nil {
		return
	}

	cacheReq := openAIRequestForPromptCache(openAIReq, thinking)
	requestFingerprint := ""
	if policy.CacheMode == config.CacheModeFunctionSimulation {
		requestFingerprint = functionCacheRequestBodyFingerprint(requestBody)
	}
	profile := tracker.BuildClaudeProfileForRequestWithPolicy(
		cacheReq,
		estimatedInputTokens,
		claudePromptCacheIsolationSeed(cacheReq, apiKeyID, providedKey),
		requestFingerprint,
		policy,
	)
	if profile != nil {
		profile.requestTiming = requestTimingFromContext(ctx)
	}
	req.promptCache = newOpenAIPromptCacheState(h.promptCache, profile)
}

func responsesPromptCacheUsageForInput(state *openAIPromptCacheState, inputTokens int) promptCacheUsage {
	if state == nil || !state.computed {
		return promptCacheUsage{}
	}
	return reanchorClaudeCacheUsage(state.profile, state.usage, inputTokens)
}

func applyResponsesPromptCacheUsage(response *ResponsesObject, usage promptCacheUsage) ClaudeUsage {
	if response == nil {
		return ClaudeUsage{}
	}
	cachedTokens := usage.CacheReadInputTokens
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > response.Usage.InputTokens {
		cachedTokens = response.Usage.InputTokens
	}
	response.Usage.InputTokensDetails.CachedTokens = cachedTokens
	// Cache policy works in the original token domain. Apply billing only once
	// the final cached portion has been constrained to the input total.
	percent := config.GetTokenBillingInflationPercent()
	usage.CacheReadInputTokens = cachedTokens
	usage.CacheCreationInputTokens = maxInt(0, minInt(usage.CacheCreationInputTokens, response.Usage.InputTokens-cachedTokens))
	billedUsage := ClaudeUsage{InputTokens: billedClaudeInputTokens(response.Usage.InputTokens, usage), OutputTokens: response.Usage.OutputTokens, CacheCreationInputTokens: usage.CacheCreationInputTokens, CacheReadInputTokens: cachedTokens, CacheCreation: &ClaudeCacheCreationUsage{Ephemeral5mInputTokens: usage.CacheCreation5mInputTokens, Ephemeral1hInputTokens: usage.CacheCreation1hInputTokens}}
	inflateClaudeUsageWithPercent(&billedUsage, percent)
	response.Usage.InputTokens = billedUsage.InputTokens + billedUsage.CacheCreationInputTokens + billedUsage.CacheReadInputTokens
	response.Usage.OutputTokens = billedUsage.OutputTokens
	response.Usage.InputTokensDetails.CachedTokens = billedUsage.CacheReadInputTokens
	response.Usage.OutputTokensDetails.ReasoningTokens = inflateBillingTokensWithPercent(response.Usage.OutputTokensDetails.ReasoningTokens, percent)
	response.Usage.TotalTokens = response.Usage.InputTokens + response.Usage.OutputTokens
	return billedUsage
}
