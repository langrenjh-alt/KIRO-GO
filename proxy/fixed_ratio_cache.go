package proxy

import "kiro-go/config"

func buildFixedRatioCacheProfile(req *ClaudeRequest, totalInputTokens int, cfg config.FixedRatioCacheConfig) *promptCacheProfile {
	cfg = config.NormalizeFixedRatioCacheConfig(cfg)
	if totalInputTokens < 0 {
		totalInputTokens = 0
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	return &promptCacheProfile{
		TotalInputTokens:          totalInputTokens,
		Model:                     model,
		FixedRatioCache:           true,
		FixedCacheReadPercent:     cfg.CacheReadPercent,
		FixedCacheCreationPercent: cfg.CacheCreationPercent,
	}
}

// fixedRatioCacheUsage splits total input tokens into cache read, cache
// creation, and the remainder reported as ordinary input. Output tokens are
// not part of this split. Percentages above 100 together are scaled so the
// three input parts still add up to total.
func fixedRatioCacheUsage(total, readPercent, creationPercent int) promptCacheUsage {
	if total <= 0 {
		return promptCacheUsage{}
	}
	if readPercent < 0 {
		readPercent = 0
	} else if readPercent > 100 {
		readPercent = 100
	}
	if creationPercent < 0 {
		creationPercent = 0
	} else if creationPercent > 100 {
		creationPercent = 100
	}
	readShare := float64(readPercent)
	creationShare := float64(creationPercent)
	if sum := readShare + creationShare; sum > 100 {
		readShare = readShare * 100 / sum
		creationShare = creationShare * 100 / sum
	}
	read := int(float64(total)*readShare/100 + 0.5)
	creation := int(float64(total)*creationShare/100 + 0.5)
	if read < 0 {
		read = 0
	}
	if creation < 0 {
		creation = 0
	}
	if read > total {
		read = total
	}
	if creation > total-read {
		creation = total - read
	}
	return promptCacheUsage{
		CacheCreationInputTokens:   creation,
		CacheReadInputTokens:       read,
		CacheCreation5mInputTokens: creation,
	}
}
