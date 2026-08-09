package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
)

type discardedAttemptUsage struct {
	InputTokens  int
	OutputTokens int
	Credits      float64
}

func newDiscardedAttemptUsage(inputTokens, outputTokens int, credits float64) discardedAttemptUsage {
	if inputTokens <= 0 {
		inputTokens = 1
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	if credits < 0 {
		credits = 0
	}
	return discardedAttemptUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Credits:      credits,
	}
}

func (u discardedAttemptUsage) totalTokens() int {
	return u.InputTokens + u.OutputTokens
}

func discardedAttemptOutputTokens(estimated, upstream int) int {
	if upstream > estimated {
		return upstream
	}
	return estimated
}

func discardedAttemptInputTokens(estimated, upstream int) int {
	if upstream > estimated {
		return upstream
	}
	return estimated
}

// retryBillingUsage keeps discarded attempts separate from the final visible
// output. Their token cost is exposed as cache-read input on the final response,
// while each attempt still settles independently in internal accounting.
type retryBillingUsage struct {
	attempts          []discardedAttemptUsage
	cachedInputTokens int
}

func (u *retryBillingUsage) add(attempt discardedAttemptUsage) {
	if u == nil {
		return
	}
	u.attempts = append(u.attempts, attempt)
	u.cachedInputTokens += attempt.totalTokens()
}

func (u *retryBillingUsage) merge(other retryBillingUsage) {
	if u == nil || len(other.attempts) == 0 {
		return
	}
	u.attempts = append(u.attempts, other.attempts...)
	u.cachedInputTokens += other.cachedInputTokens
}

func (u retryBillingUsage) apply(inputTokens int, cacheUsage promptCacheUsage) (int, promptCacheUsage) {
	if u.cachedInputTokens <= 0 {
		return inputTokens, cacheUsage
	}
	inputTokens += u.cachedInputTokens
	cacheUsage.CacheReadInputTokens += u.cachedInputTokens
	return inputTokens, cacheUsage
}

// recordForAPIKey is used by buffered internal calls, which do not know the
// downstream key while they execute. The handler-level attempt counters were
// already updated when each discarded attempt settled.
func (u retryBillingUsage) recordForAPIKey(apiKeyID string) {
	if apiKeyID == "" {
		return
	}
	for _, attempt := range u.attempts {
		if err := config.RecordApiKeyUsage(apiKeyID, int64(attempt.totalTokens()), attempt.Credits); err != nil {
			logger.Warnf("[ApiKey] failed to record discarded attempt for key %s: %v", apiKeyID, err)
		}
	}
}
