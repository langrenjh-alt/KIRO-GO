package config

import (
	"fmt"
	"strings"
)

const (
	RateLimitModeSwitchAccount  = "switch_account"
	RateLimitModeRetrySame      = "retry_same"
	RateLimitModeReturn429      = "return_429"
	RateLimitModeDisableAccount = "disable_account"

	SchedulingModeLoadBalance   = "load_balance"
	SchedulingModeLeastRequests = "least_requests"
	SchedulingModeLeastCredits  = "least_credits"
	SchedulingModeRoundRobin    = "round_robin"

	AccountLevelAuto    = "auto"
	AccountLevelFree    = "free"
	AccountLevelPro     = "pro"
	AccountLevelProPlus = "pro_plus"
	AccountLevelPower   = "power"
	AccountLevelUnknown = "unknown"

	CacheModeNative             = "native"
	CacheModeFunctionSimulation = "function_simulation"
	CacheModeFixedRatio         = "fixed_ratio"

	FunctionCacheScopeBodyFingerprint = "body_fingerprint"

	ReasoningCapModeOff     = "off"
	ReasoningCapModeObserve = "observe"
	ReasoningCapModeEnforce = "enforce"
)

// ReasoningCapConfig bounds the reasoning an agent turn may emit before its
// first outward action. Every zero value falls back to the proxy's built-in
// default, so an existing config file keeps its current behaviour without
// migration.
type ReasoningCapConfig struct {
	Mode        string  `json:"mode,omitempty"`        // off | observe | enforce
	MaxTokens   int     `json:"maxTokens,omitempty"`   // 0 = built-in effort-specific ceilings
	RatioLow    float64 `json:"ratioLow,omitempty"`    // 0 = built-in ratio for effort=low
	RatioMedium float64 `json:"ratioMedium,omitempty"` // 0 = built-in ratio for effort=medium
	RatioHigh   float64 `json:"ratioHigh,omitempty"`   // 0 = built-in ratio for effort=high
	RatioXHigh  float64 `json:"ratioXHigh,omitempty"`  // 0 = built-in ratio for effort=xhigh/max
}

type RateLimitConfig struct {
	Mode                   string  `json:"mode,omitempty"`
	RetrySameAccount       int     `json:"retrySameAccount,omitempty"`
	DisableSeconds         int     `json:"disableSeconds,omitempty"`
	SwitchAccountRounds    int     `json:"switchAccountRounds,omitempty"`
	RetryDelayMs           int     `json:"retryDelayMs,omitempty"`
	RetryBackoffMultiplier float64 `json:"retryBackoffMultiplier,omitempty"`
	RetryMaxDelayMs        int     `json:"retryMaxDelayMs,omitempty"`
}

type SchedulingConfig struct {
	Mode string `json:"mode,omitempty"`
}

type AccountPriorityConfig struct {
	Enabled bool                       `json:"enabled,omitempty"`
	Rules   []AccountPriorityRouteRule `json:"rules,omitempty"`
}

type AccountPriorityRouteRule struct {
	ID            string   `json:"id,omitempty"`
	Models        []string `json:"models,omitempty"`
	AccountLevels []string `json:"accountLevels,omitempty"`
	Enabled       bool     `json:"enabled"`
}

// FunctionCacheConfig controls the reference-compatible usage simulator. It
// never injects data into the Kiro payload and only changes returned cache usage.
type FunctionCacheConfig struct {
	Enabled               bool   `json:"enabled,omitempty"`
	SimulatedData         string `json:"simulatedData,omitempty"`
	SimulatedTokens       int    `json:"simulatedTokens,omitempty"`
	ScopeMode             string `json:"scopeMode,omitempty"`
	TTLSeconds            int    `json:"ttlSeconds,omitempty"`
	TTLJitterPercent      int    `json:"ttlJitterPercent,omitempty"`
	TokenJitterPercent    int    `json:"tokenJitterPercent,omitempty"`
	HitRatePercent        *int   `json:"hitRatePercent,omitempty"`
	MaxCacheReadPercent   *int   `json:"maxCacheReadPercent,omitempty"`
	MinTokens             int    `json:"minTokens,omitempty"`
	InputFingerprintChars int    `json:"inputFingerprintChars,omitempty"`
}

// FixedRatioCacheConfig splits every request's input tokens by fixed
// percentages. Output tokens stay the real generated count. The two
// percentages are shares of the input; when they sum past 100 they are
// scaled down together so input + cache read + cache creation stays equal
// to the input token count.
type FixedRatioCacheConfig struct {
	CacheReadPercent     int `json:"cacheReadPercent"`
	CacheCreationPercent int `json:"cacheCreationPercent"`
}

func normalizeRateLimitConfig(c RateLimitConfig) RateLimitConfig {
	switch strings.TrimSpace(c.Mode) {
	case RateLimitModeRetrySame, RateLimitModeReturn429, RateLimitModeDisableAccount, RateLimitModeSwitchAccount:
	default:
		c.Mode = RateLimitModeSwitchAccount
	}
	c.RetrySameAccount = clampPolicyInt(c.RetrySameAccount, 0, 20)
	if c.DisableSeconds <= 0 {
		c.DisableSeconds = 60
	}
	c.DisableSeconds = clampPolicyInt(c.DisableSeconds, 1, 86400)
	if c.SwitchAccountRounds <= 0 {
		c.SwitchAccountRounds = 1
	}
	c.SwitchAccountRounds = clampPolicyInt(c.SwitchAccountRounds, 1, 100)
	if c.RetryDelayMs <= 0 {
		c.RetryDelayMs = 1000
	}
	c.RetryDelayMs = clampPolicyInt(c.RetryDelayMs, 1, 60000)
	if c.RetryBackoffMultiplier < 1 {
		c.RetryBackoffMultiplier = 1
	}
	if c.RetryBackoffMultiplier > 5 {
		c.RetryBackoffMultiplier = 5
	}
	if c.RetryMaxDelayMs < c.RetryDelayMs {
		c.RetryMaxDelayMs = c.RetryDelayMs
	}
	c.RetryMaxDelayMs = clampPolicyInt(c.RetryMaxDelayMs, c.RetryDelayMs, 300000)
	return c
}

func normalizeSchedulingConfig(c SchedulingConfig) SchedulingConfig {
	switch strings.TrimSpace(c.Mode) {
	case SchedulingModeLoadBalance, SchedulingModeLeastRequests, SchedulingModeLeastCredits, SchedulingModeRoundRobin:
	default:
		// Preserve the current project's historical weighted round-robin behavior
		// until an operator explicitly selects another scheduling strategy.
		c.Mode = SchedulingModeRoundRobin
	}
	return c
}

func NormalizeAccountLevel(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	level = strings.NewReplacer("-", "_", " ", "_").Replace(level)
	switch level {
	case "", AccountLevelAuto:
		return AccountLevelAuto
	case "free", "trial", "starter":
		return AccountLevelFree
	case "pro", "professional":
		return AccountLevelPro
	case "pro_plus", "proplus", "plus", "pro+":
		return AccountLevelProPlus
	case "power", "ultra", "max":
		return AccountLevelPower
	case AccountLevelUnknown:
		return AccountLevelUnknown
	default:
		return level
	}
}

func EffectiveAccountLevel(a Account) string {
	if manual := NormalizeAccountLevel(a.AccountLevel); manual != AccountLevelAuto {
		return manual
	}
	combined := strings.ToLower(a.SubscriptionType + " " + a.SubscriptionTitle)
	combined = strings.NewReplacer("-", "_", " ", "_").Replace(combined)
	switch {
	case strings.Contains(combined, "pro_plus"), strings.Contains(combined, "proplus"), strings.Contains(combined, "pro+"):
		return AccountLevelProPlus
	case strings.Contains(combined, "power"):
		return AccountLevelPower
	case strings.Contains(combined, "pro"):
		return AccountLevelPro
	case strings.Contains(combined, "free"), strings.Contains(combined, "trial"):
		return AccountLevelFree
	default:
		return AccountLevelUnknown
	}
}

func normalizeAccountPriorityConfig(c AccountPriorityConfig) AccountPriorityConfig {
	if len(c.Rules) > 100 {
		c.Rules = c.Rules[:100]
	}
	rules := make([]AccountPriorityRouteRule, 0, len(c.Rules))
	for _, rule := range c.Rules {
		models := uniqueNormalized(rule.Models, func(v string) string { return strings.ToLower(strings.TrimSpace(v)) })
		levels := uniqueNormalized(rule.AccountLevels, func(v string) string {
			v = NormalizeAccountLevel(v)
			if v == AccountLevelAuto {
				return ""
			}
			return v
		})
		if len(models) == 0 || len(levels) == 0 {
			continue
		}
		rule.ID = strings.TrimSpace(rule.ID)
		if len(rule.ID) > 64 {
			rule.ID = rule.ID[:64]
		}
		rule.Models = models
		rule.AccountLevels = levels
		rules = append(rules, rule)
	}
	c.Rules = rules
	return c
}

func uniqueNormalized(values []string, normalize func(string) string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = normalize(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func AccountAllowedForModel(model string, account Account, policy AccountPriorityConfig) bool {
	policy = normalizeAccountPriorityConfig(policy)
	if !policy.Enabled || strings.TrimSpace(model) == "" {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	level := EffectiveAccountLevel(account)
	for _, rule := range policy.Rules {
		if !rule.Enabled {
			continue
		}
		matched := false
		for _, candidate := range rule.Models {
			if candidate == "*" || candidate == model {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		for _, allowed := range rule.AccountLevels {
			if allowed == level {
				return true
			}
		}
		return false
	}
	return true
}

func normalizeCacheMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case CacheModeFunctionSimulation:
		return CacheModeFunctionSimulation
	case CacheModeFixedRatio:
		return CacheModeFixedRatio
	default:
		return CacheModeNative
	}
}

// IsKnownCacheMode reports whether mode is one of the selectable cache strategies.
func IsKnownCacheMode(mode string) bool {
	switch strings.TrimSpace(mode) {
	case CacheModeNative, CacheModeFunctionSimulation, CacheModeFixedRatio:
		return true
	default:
		return false
	}
}

// NormalizeFixedRatioCacheConfig clamps both percentages to 0-100.
func NormalizeFixedRatioCacheConfig(c FixedRatioCacheConfig) FixedRatioCacheConfig {
	return normalizeFixedRatioCacheConfig(c)
}

func normalizeFixedRatioCacheConfig(c FixedRatioCacheConfig) FixedRatioCacheConfig {
	c.CacheReadPercent = clampPolicyInt(c.CacheReadPercent, 0, 100)
	c.CacheCreationPercent = clampPolicyInt(c.CacheCreationPercent, 0, 100)
	return c
}

func normalizeFunctionCacheConfig(c FunctionCacheConfig) FunctionCacheConfig {
	c.ScopeMode = FunctionCacheScopeBodyFingerprint
	if c.TTLSeconds <= 0 {
		c.TTLSeconds = 300
	}
	c.TTLSeconds = clampPolicyInt(c.TTLSeconds, 1, 86400)
	c.SimulatedTokens = clampPolicyInt(c.SimulatedTokens, 0, 1000000)
	c.TTLJitterPercent = clampPolicyInt(c.TTLJitterPercent, 0, 100)
	c.TokenJitterPercent = clampPolicyInt(c.TokenJitterPercent, 0, 100)
	if c.HitRatePercent == nil {
		v := 100
		c.HitRatePercent = &v
	} else {
		v := clampPolicyInt(*c.HitRatePercent, 0, 100)
		c.HitRatePercent = &v
	}
	if c.MaxCacheReadPercent == nil {
		v := 85
		c.MaxCacheReadPercent = &v
	} else {
		v := clampPolicyInt(*c.MaxCacheReadPercent, 1, 100)
		c.MaxCacheReadPercent = &v
	}
	c.MinTokens = clampPolicyInt(c.MinTokens, 0, 100000)
	c.InputFingerprintChars = clampPolicyInt(c.InputFingerprintChars, 0, 12000)
	return c
}

// normalizeReasoningCapMode preserves an empty mode instead of collapsing it to
// "off". The proxy needs to tell "never configured" apart from "explicitly off":
// the admin choice wins over the environment variable, so an unset mode is what
// lets the env var still act as the fallback.
func normalizeReasoningCapMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "":
		return ""
	case ReasoningCapModeObserve, "log", "report":
		return ReasoningCapModeObserve
	case ReasoningCapModeEnforce, "on", "true", "1":
		return ReasoningCapModeEnforce
	default:
		return ReasoningCapModeOff
	}
}

// normalizeReasoningCapConfig keeps zero values as zero: the proxy reads them as
// "use the built-in default" rather than "no budget", which would disable the
// cap while the mode says it is enforcing.
func normalizeReasoningCapConfig(c ReasoningCapConfig) ReasoningCapConfig {
	c.Mode = normalizeReasoningCapMode(c.Mode)
	c.MaxTokens = clampPolicyInt(c.MaxTokens, 0, 1000000)
	c.RatioLow = clampPolicyRatio(c.RatioLow)
	c.RatioMedium = clampPolicyRatio(c.RatioMedium)
	c.RatioHigh = clampPolicyRatio(c.RatioHigh)
	c.RatioXHigh = clampPolicyRatio(c.RatioXHigh)
	return c
}

// clampPolicyRatio bounds a share-of-window ratio. The upper bound is below 1.0
// because reasoning that fills the entire window leaves no room for the answer.
func clampPolicyRatio(value float64) float64 {
	if value <= 0 {
		return 0
	}
	if value > 0.9 {
		return 0.9
	}
	return value
}

func GetReasoningCapConfig() ReasoningCapConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return normalizeReasoningCapConfig(ReasoningCapConfig{})
	}
	return normalizeReasoningCapConfig(cfg.ReasoningCap)
}

func clampPolicyInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func GetRateLimitConfig() RateLimitConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return normalizeRateLimitConfig(RateLimitConfig{})
	}
	return normalizeRateLimitConfig(cfg.RateLimit)
}

func GetSchedulingConfig() SchedulingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return normalizeSchedulingConfig(SchedulingConfig{})
	}
	return normalizeSchedulingConfig(cfg.Scheduling)
}

func GetAccountPriorityConfig() AccountPriorityConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return AccountPriorityConfig{}
	}
	return normalizeAccountPriorityConfig(cfg.AccountPriority)
}

func GetCacheMode() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return CacheModeNative
	}
	return normalizeCacheMode(cfg.CacheMode)
}

func GetFunctionCacheConfig() FunctionCacheConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return normalizeFunctionCacheConfig(FunctionCacheConfig{})
	}
	return normalizeFunctionCacheConfig(cfg.FunctionCache)
}

func GetFixedRatioCacheConfig() FixedRatioCacheConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return normalizeFixedRatioCacheConfig(FixedRatioCacheConfig{})
	}
	return normalizeFixedRatioCacheConfig(cfg.FixedRatioCache)
}

// UpdateDispatchPolicies validates and persists all low-coupling policy fields
// as one atomic config update, rolling memory back if persistence fails.
func UpdateDispatchPolicies(rate *RateLimitConfig, scheduling *SchedulingConfig, priority *AccountPriorityConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("config is not initialized")
	}
	previousRate, previousScheduling, previousPriority := cfg.RateLimit, cfg.Scheduling, cfg.AccountPriority
	if rate != nil {
		cfg.RateLimit = normalizeRateLimitConfig(*rate)
	}
	if scheduling != nil {
		cfg.Scheduling = normalizeSchedulingConfig(*scheduling)
	}
	if priority != nil {
		cfg.AccountPriority = normalizeAccountPriorityConfig(*priority)
	}
	if err := Save(); err != nil {
		cfg.RateLimit, cfg.Scheduling, cfg.AccountPriority = previousRate, previousScheduling, previousPriority
		return err
	}
	return nil
}

func UpdateReasoningCapConfig(value ReasoningCapConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("config is not initialized")
	}
	previous := cfg.ReasoningCap
	cfg.ReasoningCap = normalizeReasoningCapConfig(value)
	if err := Save(); err != nil {
		cfg.ReasoningCap = previous
		return err
	}
	return nil
}

// UpdateCachePolicy switches cache modes and updates simulation settings in one
// atomic operation. Nil fields preserve existing values.
func UpdateCachePolicy(mode *string, functionCache *FunctionCacheConfig, fixedRatio *FixedRatioCacheConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("config is not initialized")
	}
	previousMode, previousFunctionCache, previousFixedRatio := cfg.CacheMode, cfg.FunctionCache, cfg.FixedRatioCache
	if mode != nil {
		cfg.CacheMode = normalizeCacheMode(*mode)
	}
	if functionCache != nil {
		cfg.FunctionCache = normalizeFunctionCacheConfig(*functionCache)
	}
	if fixedRatio != nil {
		cfg.FixedRatioCache = normalizeFixedRatioCacheConfig(*fixedRatio)
	}
	if err := Save(); err != nil {
		cfg.CacheMode, cfg.FunctionCache, cfg.FixedRatioCache = previousMode, previousFunctionCache, previousFixedRatio
		return err
	}
	return nil
}
