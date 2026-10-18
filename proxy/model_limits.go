package proxy

import (
	"math"
	"strings"
	"sync"
)

type modelTokenLimit struct {
	MaxInputTokens  int
	MaxOutputTokens int
}

type KiroContextUsage struct {
	InputTokens   int     `json:"input_tokens"`
	ContextWindow int     `json:"context_window"`
	Percentage    float64 `json:"percentage"`
	Exceeded      bool    `json:"exceeded"`
}

var liveModelTokenLimits = struct {
	sync.RWMutex
	byModel map[string]modelTokenLimit
}{
	byModel: make(map[string]modelTokenLimit),
}

// registerModelTokenLimits makes successful ListAvailableModels responses
// available to request accounting and recovery budgeting. Each update replaces
// the previous value for that model so Kiro can raise or lower limits without a
// process restart.
func registerModelTokenLimits(models []ModelInfo) {
	if len(models) == 0 {
		return
	}
	updates := make(map[string]modelTokenLimit, len(models))
	for _, model := range models {
		if model.TokenLimits == nil {
			continue
		}
		key := canonicalModelLimitKey(model.ModelId)
		if key == "" {
			continue
		}
		limit := modelTokenLimit{
			MaxInputTokens:  model.TokenLimits.MaxInputTokens,
			MaxOutputTokens: model.TokenLimits.MaxOutputTokens,
		}
		if limit.MaxInputTokens <= 0 && limit.MaxOutputTokens <= 0 {
			continue
		}
		updates[key] = limit
	}
	if len(updates) == 0 {
		return
	}
	liveModelTokenLimits.Lock()
	for key, limit := range updates {
		liveModelTokenLimits.byModel[key] = limit
	}
	liveModelTokenLimits.Unlock()
}

func lookupModelTokenLimits(model string) modelTokenLimit {
	key := canonicalModelLimitKey(model)
	liveModelTokenLimits.RLock()
	limit, ok := liveModelTokenLimits.byModel[key]
	liveModelTokenLimits.RUnlock()
	if !ok {
		return fallbackModelTokenLimits(key)
	}
	fallback := fallbackModelTokenLimits(key)
	if limit.MaxInputTokens <= 0 {
		limit.MaxInputTokens = fallback.MaxInputTokens
	}
	if limit.MaxOutputTokens <= 0 {
		limit.MaxOutputTokens = fallback.MaxOutputTokens
	}
	return limit
}

func canonicalModelLimitKey(model string) string {
	return strings.ToLower(strings.TrimSpace(MapModel(strings.TrimSpace(model))))
}

func observeKiroContextUsage(payload *KiroPayload, model string, percentage float64) (int, bool) {
	if math.IsNaN(percentage) || math.IsInf(percentage, 0) || percentage <= 0 {
		return currentKiroContextUsage(payload, model)
	}
	if payload != nil && percentage > payload.maxContextUsagePercentage {
		payload.maxContextUsagePercentage = percentage
	}
	observed := percentage
	if payload != nil && payload.maxContextUsagePercentage > observed {
		observed = payload.maxContextUsagePercentage
	}
	return contextUsageTokens(model, observed), observed >= 100
}

func currentKiroContextUsageTokens(payload *KiroPayload, model string) int {
	tokens, _ := currentKiroContextUsage(payload, model)
	return tokens
}

func currentKiroContextUsage(payload *KiroPayload, model string) (int, bool) {
	if payload == nil || payload.maxContextUsagePercentage <= 0 {
		return 0, false
	}
	return contextUsageTokens(model, payload.maxContextUsagePercentage), payload.maxContextUsagePercentage >= 100
}

func contextUsageTokens(model string, percentage float64) int {
	window := getContextWindowSize(model)
	if window <= 0 || percentage <= 0 || math.IsNaN(percentage) || math.IsInf(percentage, 0) {
		return 0
	}
	if percentage > 100 {
		percentage = 100
	}
	return minInt(int(percentage*float64(window)/100.0), window)
}

func kiroContextUsage(payload *KiroPayload, model string) *KiroContextUsage {
	if payload == nil || payload.maxContextUsagePercentage <= 0 {
		return nil
	}
	window := getContextWindowSize(model)
	percentage := payload.maxContextUsagePercentage
	exceeded := percentage >= 100
	if percentage > 100 {
		percentage = 100
	}
	return &KiroContextUsage{
		InputTokens:   contextUsageTokens(model, payload.maxContextUsagePercentage),
		ContextWindow: window,
		Percentage:    percentage,
		Exceeded:      exceeded,
	}
}

func kiroContextUsageFromTokens(model string, tokens int) *KiroContextUsage {
	if tokens <= 0 {
		return nil
	}
	window := getContextWindowSize(model)
	if window <= 0 {
		return nil
	}
	if tokens > window {
		tokens = window
	}
	percentage := float64(tokens) * 100 / float64(window)
	return &KiroContextUsage{
		InputTokens:   tokens,
		ContextWindow: window,
		Percentage:    percentage,
		Exceeded:      tokens >= window,
	}
}

func addKiroContextUsage(usage map[string]interface{}, payload *KiroPayload, model string) {
	if usage == nil {
		return
	}
	if contextUsage := kiroContextUsage(payload, model); contextUsage != nil {
		usage["kiro_context_usage"] = contextUsage
	}
}

// fallbackModelTokenLimits mirrors the last observed Kiro model catalog. Live
// ListAvailableModels values always take precedence; the fallback only covers
// startup and temporary catalog-refresh failures.
func fallbackModelTokenLimits(model string) modelTokenLimit {
	model = canonicalModelLimitKey(model)
	switch model {
	case "auto":
		return modelTokenLimit{MaxInputTokens: 1_000_000, MaxOutputTokens: 64_000}
	case "deepseek-3.2":
		return modelTokenLimit{MaxInputTokens: 164_000, MaxOutputTokens: 64_000}
	case "glm-5":
		return modelTokenLimit{MaxInputTokens: 200_000, MaxOutputTokens: 64_000}
	case "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra":
		return modelTokenLimit{MaxInputTokens: 272_000, MaxOutputTokens: 128_000}
	case "minimax-m2.1", "minimax-m2.5":
		return modelTokenLimit{MaxInputTokens: 196_000, MaxOutputTokens: 64_000}
	case "qwen3-coder-next":
		return modelTokenLimit{MaxInputTokens: 256_000, MaxOutputTokens: 64_000}
	}
	if opusHasExtendedOutputLimit(model) {
		return modelTokenLimit{MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000}
	}
	if isLargeContextModel(model) {
		return modelTokenLimit{MaxInputTokens: 1_000_000, MaxOutputTokens: 64_000}
	}
	return modelTokenLimit{MaxInputTokens: 200_000, MaxOutputTokens: 64_000}
}
