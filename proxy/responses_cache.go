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

func applyResponsesPromptCacheUsage(response *ResponsesObject, usage promptCacheUsage) {
	if response == nil {
		return
	}
	cachedTokens := usage.CacheReadInputTokens
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > response.Usage.InputTokens {
		cachedTokens = response.Usage.InputTokens
	}
	response.Usage.InputTokensDetails.CachedTokens = cachedTokens
}
