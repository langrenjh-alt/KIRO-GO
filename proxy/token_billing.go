package proxy

import (
	"math"

	"kiro-go/config"
)

// The cap is on the final combined billing input, not on the real context or
// on output. Cache creation/read remain mutually exclusive input categories.
const maxBillingInputTokens = 1_000_000

func billingInputExceedsLimit(usage *ClaudeUsage) bool {
	return float64(usage.InputTokens)+float64(usage.CacheCreationInputTokens)+float64(usage.CacheReadInputTokens) > maxBillingInputTokens
}

func capBillingInputUsage(usage *ClaudeUsage) {
	if !billingInputExceedsLimit(usage) {
		return
	}
	// Sum in the floating-point domain to avoid integer overflow at extreme
	// inflation. Largest-remainder allocation preserves proportions and makes
	// the three whole-token categories add up to exactly the cap.
	values := [3]float64{float64(usage.InputTokens), float64(usage.CacheCreationInputTokens), float64(usage.CacheReadInputTokens)}
	total := values[0] + values[1] + values[2]
	var capped [3]int
	var remainders [3]float64
	remaining := maxBillingInputTokens
	for i, value := range values {
		scaled := value / total * maxBillingInputTokens
		capped[i] = int(math.Floor(scaled))
		remainders[i] = scaled - float64(capped[i])
		remaining -= capped[i]
	}
	for ; remaining > 0; remaining-- {
		largest := 0
		for i := 1; i < len(remainders); i++ {
			if remainders[i] > remainders[largest] {
				largest = i
			}
		}
		capped[largest]++
		remainders[largest] = -1
	}
	usage.InputTokens, usage.CacheCreationInputTokens, usage.CacheReadInputTokens = capped[0], capped[1], capped[2]
}

// Inflation is a final billing transformation. Cache matching, context limits,
// retries and upstream account credits must continue to use the raw tokens.
func inflateBillingTokens(tokens int) int {
	return inflateBillingTokensWithPercent(tokens, config.GetTokenBillingInflationPercent())
}

func inflateBillingTokensWithPercent(tokens int, percent float64) int {
	if percent == 0 || tokens <= 0 {
		return tokens
	}
	// Round the added tokens separately: the original count is already an
	// integer, and this avoids losing a half token to 1 + percent/100 (0.5%).
	extra := math.Round(float64(tokens) * percent / 100)
	maxInt := int(^uint(0) >> 1)
	if extra >= float64(maxInt-tokens) {
		return maxInt
	}
	return tokens + int(extra)
}

func inflateClaudeUsage(usage *ClaudeUsage) {
	inflateClaudeUsageWithPercent(usage, config.GetTokenBillingInflationPercent())
}

func inflateClaudeUsageWithPercent(usage *ClaudeUsage, percent float64) {
	if usage == nil || (percent == 0 && !billingInputExceedsLimit(usage)) {
		return
	}
	usage.InputTokens = inflateBillingTokensWithPercent(usage.InputTokens, percent)
	usage.OutputTokens = inflateBillingTokensWithPercent(usage.OutputTokens, percent)
	usage.CacheCreationInputTokens = inflateBillingTokensWithPercent(usage.CacheCreationInputTokens, percent)
	usage.CacheReadInputTokens = inflateBillingTokensWithPercent(usage.CacheReadInputTokens, percent)
	capBillingInputUsage(usage)
	if usage.CacheCreation != nil {
		usage.CacheCreation.Ephemeral5mInputTokens, usage.CacheCreation.Ephemeral1hInputTokens = scaleClaudeCodeCacheCreation(
			usage.CacheCreation.Ephemeral5mInputTokens, usage.CacheCreation.Ephemeral1hInputTokens, usage.CacheCreationInputTokens,
		)
	}
	if usage.OutputTokensDetails != nil {
		usage.OutputTokensDetails.ThinkingTokens = inflateBillingTokensWithPercent(usage.OutputTokensDetails.ThinkingTokens, percent)
	}
}

// Split into billing categories first; rounding the total before the split can
// alter cache ratios and differs from rounding each billable category.
func inflatedBillingUsage(inputTokens, outputTokens int, cacheUsage promptCacheUsage) ClaudeUsage {
	return inflatedBillingUsageWithPercent(inputTokens, outputTokens, cacheUsage, config.GetTokenBillingInflationPercent())
}

func inflatedBillingUsageWithPercent(inputTokens, outputTokens int, cacheUsage promptCacheUsage, percent float64) ClaudeUsage {
	usage := ClaudeUsage{
		InputTokens:              billedClaudeInputTokens(inputTokens, cacheUsage),
		OutputTokens:             outputTokens,
		CacheCreationInputTokens: cacheUsage.CacheCreationInputTokens,
		CacheReadInputTokens:     cacheUsage.CacheReadInputTokens,
	}
	inflateClaudeUsageWithPercent(&usage, percent)
	return usage
}

func billingUsageTotal(usage ClaudeUsage) int {
	return usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens + usage.OutputTokens
}
