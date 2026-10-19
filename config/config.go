// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID       string `json:"id"`                 // Unique account identifier (UUID)
	Email    string `json:"email,omitempty"`    // User email address
	UserId   string `json:"userId,omitempty"`   // Kiro user ID
	Nickname string `json:"nickname,omitempty"` // Display name for admin panel

	// Authentication credentials
	AccessToken  string `json:"accessToken"`            // OAuth access token for API calls
	RefreshToken string `json:"refreshToken"`           // OAuth refresh token for token renewal
	ClientID     string `json:"clientId,omitempty"`     // OIDC client ID (for IdC auth)
	ClientSecret string `json:"clientSecret,omitempty"` // OIDC client secret (for IdC auth)
	AuthMethod   string `json:"authMethod"`             // Authentication method: "idc" (AWS IdC), "social" (GitHub/Google), "external_idp" (enterprise SSO, e.g. Azure AD), or "api_key" (Kiro API key used directly as bearer)
	KiroApiKey   string `json:"kiroApiKey,omitempty"`   // API key credential, used directly as the bearer token when AuthMethod == "api_key"
	AuthRegion   string `json:"authRegion,omitempty"`   // Region for token-refresh endpoints; falls back to Region
	ApiRegion    string `json:"apiRegion,omitempty"`    // Region for API request hosts; falls back to Region
	Provider     string `json:"provider,omitempty"`     // Identity provider name (e.g., "BuilderId", "GitHub", "AzureAD")
	Region       string `json:"region"`                 // AWS region for OIDC endpoints
	StartUrl     string `json:"startUrl,omitempty"`     // AWS SSO start URL
	ExpiresAt    int64  `json:"expiresAt,omitempty"`    // Token expiration timestamp (Unix seconds)
	MachineId    string `json:"machineId,omitempty"`    // UUID machine identifier for request tracking
	ProfileArn   string `json:"profileArn,omitempty"`   // CodeWhisperer/Kiro profile ARN for generation requests

	// External IdP (enterprise SSO, e.g. Microsoft 365 / Entra ID / Azure AD) refresh material.
	// When AuthMethod == "external_idp" the credential is an IdP-issued OAuth token refreshed
	// against TokenEndpoint using ClientID and Scopes (refresh_token grant), NOT the AWS SSO
	// OIDC endpoint. IssuerURL is the OIDC issuer the endpoints were discovered from.
	TokenEndpoint string `json:"tokenEndpoint,omitempty"` // External IdP OAuth2 token endpoint (refresh)
	IssuerURL     string `json:"issuerUrl,omitempty"`     // External IdP OIDC issuer URL
	Scopes        string `json:"scopes,omitempty"`        // Space-separated scopes granted by the external IdP

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// AccountLevel optionally overrides the subscription tier inferred for
	// model-routing rules. Empty and "auto" both use the upstream tier.
	AccountLevel string `json:"accountLevel,omitempty"`

	// Priority weight for load balancing (higher = more requests)
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher priority

	// MaxConcurrency limits concurrent leased requests for this account.
	// Zero means unlimited.
	MaxConcurrency int `json:"maxConcurrency,omitempty"`

	// Upstream Overages state (mirrored from AWS Q `setUserPreference` / `getUsageLimits`).
	// OverageStatus is the only switch that decides whether to keep dispatching once UsageLimit is reached.
	// Allowed values: "ENABLED", "DISABLED", "UNKNOWN" (or empty when not yet fetched).
	OverageStatus     string  `json:"overageStatus,omitempty"`
	OverageCapability string  `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" / "NOT_OVERAGE_CAPABLE"
	OverageCap        float64 `json:"overageCap,omitempty"`        // Hard upper bound (USD)
	OverageRate       float64 `json:"overageRate,omitempty"`       // Per-invocation rate (USD)
	CurrentOverages   float64 `json:"currentOverages,omitempty"`   // Cumulative overage charges (USD)
	OverageCheckedAt  int64   `json:"overageCheckedAt,omitempty"`  // Last successful upstream sync (Unix seconds)

	// LegacyAllowOverage is kept for backward-compatible JSON loading only.
	// Pre-Overages-switch deployments persisted `allowOverage: true` to mean
	// "keep dispatching when quota is exhausted". On first load we migrate it
	// into OverageStatus="ENABLED" and zero this field so it does not get
	// re-emitted on future saves. Do not read this field elsewhere.
	LegacyAllowOverage bool `json:"allowOverage,omitempty"`

	// Account status
	Enabled   bool   `json:"enabled"`             // Whether account is active in the pool
	BanStatus string `json:"banStatus,omitempty"` // Ban status: "ACTIVE", "BANNED", "SUSPENDED"
	BanReason string `json:"banReason,omitempty"` // Reason for ban/suspension
	BanTime   int64  `json:"banTime,omitempty"`   // Timestamp when ban was detected

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation)
	RequestCount int     `json:"requestCount,omitempty"` // Total requests processed
	ErrorCount   int     `json:"errorCount,omitempty"`   // Total errors encountered
	LastUsed     int64   `json:"lastUsed,omitempty"`     // Last request timestamp
	TotalTokens  int     `json:"totalTokens,omitempty"`  // Cumulative tokens processed
	TotalCredits float64 `json:"totalCredits,omitempty"` // Cumulative credits consumed
}

// IsApiKeyCredential reports whether the account authenticates with a Kiro API
// key used directly as the bearer token (tokentype: API_KEY), bypassing OAuth
// refresh and profile-ARN resolution. True when KiroApiKey is set OR the
// authMethod is "api_key"/"apikey" (case-insensitive).
func (a *Account) IsApiKeyCredential() bool {
	if a == nil {
		return false
	}
	if a.KiroApiKey != "" {
		return true
	}
	m := strings.ToLower(a.AuthMethod)
	return m == "api_key" || m == "apikey"
}

// EffectiveAuthRegion returns the region used for token-refresh endpoints, with
// the fallback chain: account.AuthRegion > account.Region > global AuthRegion
// (if set and not us-east-1) > global Region (if set and not us-east-1) > us-east-1.
func (a *Account) EffectiveAuthRegion() string {
	if a != nil {
		if r := strings.TrimSpace(a.AuthRegion); r != "" {
			return r
		}
		if r := strings.TrimSpace(a.Region); r != "" {
			return r
		}
	}
	if r := GetGlobalAuthRegion(); r != "" && r != "us-east-1" {
		return r
	}
	if r := GetGlobalRegion(); r != "" && r != "us-east-1" {
		return r
	}
	return "us-east-1"
}

// EffectiveApiRegion returns the region used for API request hosts, with the
// same fallback chain as EffectiveAuthRegion but using ApiRegion.
func (a *Account) EffectiveApiRegion() string {
	if a != nil {
		if r := strings.TrimSpace(a.ApiRegion); r != "" {
			return r
		}
		if r := strings.TrimSpace(a.Region); r != "" {
			return r
		}
	}
	if r := GetGlobalApiRegion(); r != "" && r != "us-east-1" {
		return r
	}
	if r := GetGlobalRegion(); r != "" && r != "us-east-1" {
		return r
	}
	return "us-east-1"
}

// PromptFilterRule defines a single custom prompt sanitization rule.
// Type can be: "regex" (regexp find/replace within prompt) or
// "lines-containing" (remove lines containing the match substring).
type PromptFilterRule struct {
	ID      string `json:"id"`                // Unique rule identifier
	Name    string `json:"name"`              // Human-readable rule name
	Type    string `json:"type"`              // "regex" or "lines-containing"
	Match   string `json:"match"`             // Pattern to match (regex pattern or substring)
	Replace string `json:"replace,omitempty"` // Replacement string (only for regex; empty = delete match)
	Enabled bool   `json:"enabled"`           // Whether this rule is active
}

// ApiKeyEntry represents a single API key with optional usage limits and counters.
// Limits with value 0 are treated as "no limit". Counters are cumulative and never reset
// automatically; operators can use the admin endpoint to manually reset them.
type ApiKeyEntry struct {
	ID                  string `json:"id"`                            // Unique identifier (UUID)
	Name                string `json:"name,omitempty"`                // Human-readable label
	Key                 string `json:"key"`                           // The actual key value clients send
	Enabled             bool   `json:"enabled"`                       // Whether this key may authenticate
	Migrated            bool   `json:"migrated,omitempty"`            // True if migrated from legacy single ApiKey field
	PromptCachePolicyID string `json:"promptCachePolicyId,omitempty"` // Empty uses the global prompt-cache settings
	CreatedAt           int64  `json:"createdAt"`                     // Creation timestamp (Unix seconds)
	LastUsedAt          int64  `json:"lastUsedAt,omitempty"`

	// Limits (0 = unlimited)
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`

	// Cumulative usage (never auto-reset)
	TokensUsed    int64   `json:"tokensUsed,omitempty"`
	CreditsUsed   float64 `json:"creditsUsed,omitempty"`
	RequestsCount int64   `json:"requestsCount,omitempty"`
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string        `json:"password"`          // Admin panel password
	Port          int           `json:"port"`              // HTTP server port (default: 8080)
	Host          string        `json:"host"`              // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string        `json:"apiKey,omitempty"`  // [Deprecated] Legacy single API key, migrated into ApiKeys on first load
	RequireApiKey bool          `json:"requireApiKey"`     // [Deprecated] Whether to enforce API key validation; with multi-key support, len(ApiKeys)>0 implicitly enforces auth
	ApiKeys       []ApiKeyEntry `json:"apiKeys,omitempty"` // Multiple API keys, each with independent quota
	// PromptCachePolicies are named prompt-cache configurations that API keys may
	// reference. An unbound API key continues to use the legacy global fields.
	PromptCachePolicies []PromptCachePolicy `json:"promptCachePolicies,omitempty"`
	KiroVersion         string              `json:"kiroVersion,omitempty"`
	SystemVersion       string              `json:"systemVersion,omitempty"`
	NodeVersion         string              `json:"nodeVersion,omitempty"`
	Accounts            []Account           `json:"accounts"` // Registered Kiro accounts

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`       // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"` // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"` // Claude output format: "reasoning_content", "thinking", or "think"

	// Endpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// EmptyOutputCooldownEnabled cools the failing account and model when the
	// upstream returns an events=none empty body. Nil means enabled.
	EmptyOutputCooldownEnabled *bool `json:"emptyOutputCooldownEnabled,omitempty"`
	// EmptyOutputCooldownMinutes is the cooldown length. Zero means 30 minutes.
	EmptyOutputCooldownMinutes int `json:"emptyOutputCooldownMinutes,omitempty"`
	// Opus55EmptyMapToOpus5 retries opus 5.5 on opus 5 after every account
	// returns an events=none body. Nil means disabled. Downstream model ids
	// stay on opus 5.5.
	Opus55EmptyMapToOpus5 *bool `json:"opus55EmptyMapToOpus5,omitempty"`

	// Proxy pool storage is independent of other services and applies to every account tier.
	ProxyPoolSettings    ProxyPoolSettings `json:"proxyPoolSettings"`
	Proxies              []ProxyEntry      `json:"proxies,omitempty"`
	AccountProxyBindings map[string]string `json:"accountProxyBindings,omitempty"`

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// AccountConcurrencyRefreshSeconds controls how often the admin account list
	// polls current in-flight request counts.
	AccountConcurrencyRefreshSeconds int `json:"accountConcurrencyRefreshSeconds,omitempty"`

	// Dispatch and failure policies.
	RateLimit       RateLimitConfig       `json:"rateLimit,omitempty"`
	Scheduling      SchedulingConfig      `json:"scheduling,omitempty"`
	AccountPriority AccountPriorityConfig `json:"accountPriority,omitempty"`

	// CacheMode switches between native persistent prompt-cache accounting,
	// usage-only function-cache simulation, and a fixed input-token split.
	CacheMode       string                `json:"cacheMode,omitempty"`
	FunctionCache   FunctionCacheConfig   `json:"functionCache,omitempty"`
	FixedRatioCache FixedRatioCacheConfig `json:"fixedRatioCache,omitempty"`

	// TokenBillingInflationPercent scales every billable token category after
	// the selected cache policy has calculated its final usage. Zero preserves
	// the original usage; a value of 100 doubles it.
	TokenBillingInflationPercent float64 `json:"tokenBillingInflationPercent,omitempty"`

	// ReasoningCap bounds pre-action reasoning so a looping agent turn cannot
	// occupy an account slot indefinitely.
	ReasoningCap ReasoningCapConfig `json:"reasoningCap,omitempty"`

	// AgentCompletionMarkerEnabled controls the private coding-agent final-answer
	// marker protocol. Nil defaults to enabled for backward compatibility.
	AgentCompletionMarkerEnabled *bool `json:"agentCompletionMarkerEnabled,omitempty"`

	// Region defaults for accounts that omit per-account region/authRegion/apiRegion.
	// Defaults to "us-east-1" when empty (see GetGlobalRegion* / Account.Effective*Region).
	Region     string `json:"region,omitempty"`
	AuthRegion string `json:"authRegion,omitempty"`
	ApiRegion  string `json:"apiRegion,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Leave empty to connect directly.
	ProxyURL string `json:"proxyURL,omitempty"`

	// SanitizeClaudeCodePrompt is kept for backward-compatible JSON loading only.
	// Migrated to FilterClaudeCode on first load. Do not use directly.
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	// FilterClaudeCode detects the Claude Code CLI built-in system prompt and replaces it
	// with a compact backend-only prompt, reducing token usage significantly.
	FilterClaudeCode bool `json:"filterClaudeCode,omitempty"`

	// FilterEnvNoise strips environment metadata lines from system prompts:
	// git status, recent commits, environment sections, fast_mode_info tags, etc.
	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	// FilterStripBoundaries removes --- SYSTEM PROMPT --- / --- END SYSTEM PROMPT --- markers.
	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	// PromptFilterRules is a list of user-defined prompt sanitization rules (regex or line-filter).
	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// PromptCacheMaxRatio caps the fraction of input tokens reported as cache_read
	// in a single turn. Default 0.85. Raise to 0.95 for "continue"-heavy workloads
	// where the newest content is minimal and >85% of input is genuinely from cache.
	PromptCacheMaxRatio float64 `json:"promptCacheMaxRatio,omitempty"`

	// PromptCacheMaxEntries bounds the in-memory prompt-cache map; once exceeded,
	// the least-recently-used entries are evicted (LRU). Default 131072. Sized so
	// the prefix write-rate × TTL does not evict multi-turn history prefixes
	// before the next turn reuses them (mirrors kiro-rs's 131072 default). The
	// tracker clamps explicit small values up to 256.
	PromptCacheMaxEntries int `json:"promptCacheMaxEntries,omitempty"`

	// PromptCacheTTLSeconds is the default lifetime used when an Anthropic
	// cache_control marker omits ttl. Anthropic supports 5-minute and 1-hour
	// ephemeral cache lifetimes, represented here as 300 or 3600 seconds.
	PromptCacheTTLSeconds int `json:"promptCacheTTLSeconds,omitempty"`

	// Global statistics (persisted across restarts)
	TotalRequests   int     `json:"totalRequests,omitempty"`   // Total API requests received
	SuccessRequests int     `json:"successRequests,omitempty"` // Successful requests count
	FailedRequests  int     `json:"failedRequests,omitempty"`  // Failed requests count
	TotalTokens     int     `json:"totalTokens,omitempty"`     // Total tokens processed
	TotalCredits    float64 `json:"totalCredits,omitempty"`    // Total credits consumed
}

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

// Version current version
const Version = "1.1.10"

const (
	autoQuarantineSuspicious429Reason = "auto-quarantine: suspicious 429 pattern"
	operatorDisabledReason            = "operator-disabled"
	autoQuarantineDuration            = 30 * time.Minute
)

var (
	cfg              *Config
	cfgLock          sync.RWMutex
	cfgPath          string
	cfgProcessLock   configProcessLock
	cfgProcessLockID string
)

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created.
func Init(path string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	lockID, err := configPathIdentity(path)
	if err != nil {
		return err
	}
	reuseProcessLock := cfgProcessLock != nil && sameConfigPathIdentity(cfgProcessLockID, lockID)
	lock := cfgProcessLock
	if !reuseProcessLock {
		lock, err = acquireConfigProcessLock(lockID)
		if err != nil {
			return configPathLockError(path, err)
		}
	}

	preserveApiKeyUsage := cfg != nil && reuseProcessLock
	previousCfg, previousPath := cfg, cfgPath
	if preserveApiKeyUsage {
		if err := flushApiKeyUsageLocked(); err != nil {
			return err
		}
	}
	cfgPath = path
	if err := loadLocked(preserveApiKeyUsage); err != nil {
		// Keep a previously working configuration and lock when a reload fails.
		// A failed second-instance startup must not release the owner lock.
		cfg, cfgPath = previousCfg, previousPath
		if !reuseProcessLock {
			_ = lock.Close()
		}
		return err
	}
	if !reuseProcessLock {
		if cfgProcessLock != nil {
			_ = cfgProcessLock.Close()
		}
		cfgProcessLock = lock
		cfgProcessLockID = lockID
	}
	return nil
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	acquiredProcessLock := false
	if cfgProcessLock == nil && cfgPath != "" {
		lockID, err := configPathIdentity(cfgPath)
		if err != nil {
			return err
		}
		lock, err := acquireConfigProcessLock(lockID)
		if err != nil {
			return configPathLockError(cfgPath, err)
		}
		cfgProcessLock = lock
		cfgProcessLockID = lockID
		acquiredProcessLock = true
	}
	if cfg != nil {
		if err := flushApiKeyUsageLocked(); err != nil {
			if acquiredProcessLock {
				_ = cfgProcessLock.Close()
				cfgProcessLock = nil
				cfgProcessLockID = ""
			}
			return err
		}
	}
	if err := loadLocked(true); err != nil {
		if acquiredProcessLock {
			_ = cfgProcessLock.Close()
			cfgProcessLock = nil
			cfgProcessLockID = ""
		}
		return err
	}
	return nil
}

// Close releases the process-wide configuration lock. The lock is also
// released automatically when the process exits; this method is useful for
// graceful shutdowns and tests that switch configuration files explicitly.
func Close() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfgProcessLock == nil {
		return nil
	}
	err := cfgProcessLock.Close()
	cfgProcessLock = nil
	cfgProcessLockID = ""
	return err
}

// loadLocked replaces the in-memory configuration from cfgPath. The caller
// must hold cfgLock so Init cannot switch paths while another save is running.
func loadLocked(preserveApiKeyUsage bool) error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create default configuration.
			// Binds to 0.0.0.0 by default for Docker/container compatibility.
			cfg = &Config{
				Password:          "changeme",
				Port:              8080,
				Host:              "0.0.0.0",
				RequireApiKey:     false,
				Accounts:          []Account{},
				ProxyPoolSettings: defaultProxyPoolSettings(),
			}
			rebuildApiKeyUsageStatesLocked(preserveApiKeyUsage)
			return saveLocked()
		}
		return err
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	if err := ValidateTokenBillingInflationPercent(c.TokenBillingInflationPercent); err != nil {
		return err
	}
	cfg = &c
	dirty := false
	if normalizeLoadedProxyPoolLocked(data) {
		dirty = true
	}

	// Migration: if a legacy single ApiKey is present and the new ApiKeys list is empty,
	// promote it into the new structure. The migrated entry inherits the legacy
	// RequireApiKey state — if the legacy deployment was public (RequireApiKey=false),
	// we mark the entry disabled so it doesn't accidentally start enforcing auth.
	// Operators can flip it on later from the admin UI. The legacy field is kept
	// for backward compatibility when reading older config files.
	legacyAPIKey := strings.TrimSpace(cfg.ApiKey)
	if cfg.ApiKey != legacyAPIKey {
		cfg.ApiKey = legacyAPIKey
		dirty = true
	}
	if legacyAPIKey != "" && len(cfg.ApiKeys) == 0 {
		cfg.ApiKeys = append(cfg.ApiKeys, ApiKeyEntry{
			ID:        newUUID(),
			Name:      "legacy",
			Key:       legacyAPIKey,
			Enabled:   cfg.RequireApiKey,
			Migrated:  true,
			CreatedAt: time.Now().Unix(),
		})
		dirty = true
	}

	if normalizeLoadedPromptCacheBindingsLocked() {
		dirty = true
	}

	// Migration: per-account AllowOverage → OverageStatus.
	// Pre-Overages-switch deployments stored `allowOverage: true` to mean "keep
	// dispatching when quota is exhausted". The new model reads OverageStatus
	// from the upstream AWS Q switch instead. To avoid silently disabling
	// previously-allowed accounts on first launch, treat allowOverage=true as
	// OverageStatus="ENABLED" (operators can refresh from AWS later). The
	// legacy field is then cleared so future saves don't re-emit it.
	for i := range cfg.Accounts {
		if cfg.Accounts[i].LegacyAllowOverage {
			if cfg.Accounts[i].OverageStatus == "" {
				cfg.Accounts[i].OverageStatus = "ENABLED"
			}
			cfg.Accounts[i].LegacyAllowOverage = false
			dirty = true
		}
	}

	// Publish the normalized IDs before saving so a different-path Init cannot
	// accidentally merge pending usage from the previous configuration. A
	// same-path Load still reuses states for IDs that remained stable.
	rebuildApiKeyUsageStatesLocked(preserveApiKeyUsage)
	if dirty {
		if err := saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

// normalizeLoadedPromptCacheBindingsLocked repairs identity and reference
// fields that may have been hand-edited or written by older releases. The
// caller persists the returned dirty state together with all other migrations.
func normalizeLoadedPromptCacheBindingsLocked() bool {
	dirty := false
	usedPolicyIDs := make(map[string]struct{}, len(cfg.PromptCachePolicies))
	for i := range cfg.PromptCachePolicies {
		policy := &cfg.PromptCachePolicies[i]
		originalID := policy.ID
		id := strings.TrimSpace(originalID)
		_, duplicate := usedPolicyIDs[id]
		if id == "" || duplicate {
			id = newUniqueConfigID(usedPolicyIDs)
			logger.Warnf("[Config] repaired prompt-cache policy ID at index %d (blank=%t duplicate=%t); new ID=%s", i, strings.TrimSpace(originalID) == "", duplicate, id)
		}
		if policy.ID != id {
			policy.ID = id
			dirty = true
		}
		usedPolicyIDs[id] = struct{}{}

		name := strings.TrimSpace(policy.Name)
		if policy.Name != name {
			policy.Name = name
			dirty = true
		}
	}

	usedAPIKeyIDs := make(map[string]struct{}, len(cfg.ApiKeys))
	firstAPIKeyByValue := make(map[string]string, len(cfg.ApiKeys))
	for i := range cfg.ApiKeys {
		entry := &cfg.ApiKeys[i]
		id := strings.TrimSpace(entry.ID)
		_, duplicateID := usedAPIKeyIDs[id]
		if id == "" || duplicateID {
			id = newUniqueConfigID(usedAPIKeyIDs)
			logger.Warnf("[Config] repaired API key ID at index %d (blank=%t duplicate=%t); new ID=%s", i, strings.TrimSpace(entry.ID) == "", duplicateID, id)
		}
		if entry.ID != id {
			entry.ID = id
			dirty = true
		}
		usedAPIKeyIDs[id] = struct{}{}

		name := strings.TrimSpace(entry.Name)
		if entry.Name != name {
			entry.Name = name
			dirty = true
		}
		key := strings.TrimSpace(entry.Key)
		if entry.Key != key {
			entry.Key = key
			dirty = true
		}
		if key == "" {
			if entry.Enabled {
				entry.Enabled = false
				dirty = true
			}
			logger.Warnf("[Config] disabled API key %s at index %d because its value is empty after trimming", entry.ID, i)
		} else if firstID, duplicate := firstAPIKeyByValue[key]; duplicate {
			if entry.Enabled {
				entry.Enabled = false
				dirty = true
			}
			logger.Warnf("[Config] disabled duplicate API key %s at index %d; value is already owned by API key %s", entry.ID, i, firstID)
		} else {
			firstAPIKeyByValue[key] = entry.ID
		}

		policyID := strings.TrimSpace(entry.PromptCachePolicyID)
		if policyID != "" {
			if _, exists := usedPolicyIDs[policyID]; !exists {
				// Keep the reference intact. A policy can be temporarily absent
				// during an external restore or a mixed-version write; runtime
				// resolution already falls back to global settings and retaining the
				// ID lets the binding recover automatically when the policy returns.
				logger.Warnf("[Config] preserving dangling prompt-cache policy binding %q from API key %s", policyID, entry.ID)
			}
		}
		if entry.PromptCachePolicyID != policyID {
			entry.PromptCachePolicyID = policyID
			dirty = true
		}
	}
	return dirty
}

func newUniqueConfigID(used map[string]struct{}) string {
	for {
		id := newUUID()
		if _, exists := used[id]; !exists {
			return id
		}
	}
}

// saveLocked persists cfg to disk. Caller MUST already hold cfgLock.
// This is identical to Save() (which does not take the lock either) but is named
// distinctly so call sites that already hold cfgLock are explicit about it.
func saveLocked() error {
	return Save()
}

// newUUID returns a UUID v4 string. Defined here to avoid pulling extra deps in this file.
func newUUID() string {
	return GenerateMachineId()
}

// Save persists the current configuration without exposing a partially written
// JSON file if the process or filesystem fails during the write.
func Save() error {
	data, err := json.MarshalIndent(configSnapshotWithApiKeyUsageLocked(), "", "  ")
	if err != nil {
		return err
	}
	return writeConfigAtomic(cfgPath, data)
}

func writeConfigAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}

	// Syncing the directory makes the rename durable on filesystems that
	// support it. Some platforms reject directory Sync, so this is best-effort.
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

// SetPassword updates the admin password.
// Primarily used for environment variable override in containerized deployments.
func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = password
}

// GetConfigDir returns the directory containing the config JSON file.
// Useful for sibling state (e.g. stored Responses, caches) that should live
// alongside the configuration file.
func GetConfigDir() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfgPath == "" {
		return "."
	}
	dir := cfgPath
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			return dir[:i]
		}
	}
	return "."
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

func GetAccounts() []Account {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	applyAutoRestoreLocked()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

// AccountIDExists reports whether an account with the given ID is already stored.
// Used by the credential-import path to reuse a pasted record's id when it does
// not collide, so re-importing a backup never creates a duplicate entry.
func AccountIDExists(id string) bool {
	if id == "" {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for _, a := range cfg.Accounts {
		if a.ID == id {
			return true
		}
	}
	return false
}

func GetEnabledAccounts() []Account {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	applyAutoRestoreLocked()
	var accounts []Account
	for _, a := range cfg.Accounts {
		if proxyAccountActive(a) {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

func AddAccount(account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	// Reject a duplicate id under the write lock. The import path pre-checks with
	// AccountIDExists (RLock) and mints a fresh id on collision, but that check and this
	// append are not atomic; two concurrent imports of the same pasted id could both
	// pass the pre-check. This makes "add if id absent" the atomic invariant.
	if account.ID != "" {
		for _, a := range cfg.Accounts {
			if a.ID == account.ID {
				return fmt.Errorf("account with id %s already exists", account.ID)
			}
		}
	}
	previousAccounts := append([]Account(nil), cfg.Accounts...)
	previousPool := snapshotProxyPoolLocked()
	cfg.Accounts = append(cfg.Accounts, account)
	return saveAccountMutationLocked(previousAccounts, previousPool)
}

func UpdateAccount(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previousAccounts := append([]Account(nil), cfg.Accounts...)
			previousPool := snapshotProxyPoolLocked()
			account.ID = id
			cfg.Accounts[i] = account
			return saveAccountMutationLocked(previousAccounts, previousPool)
		}
	}
	return nil
}

// UpdateAccountOverageStatus persists the cached upstream overage status fields.
// Called after a successful setUserPreference or getUsageLimits round-trip.
func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if status != "" {
				cfg.Accounts[i].OverageStatus = status
			}
			if capability != "" {
				cfg.Accounts[i].OverageCapability = capability
			}
			cfg.Accounts[i].OverageCap = cap
			cfg.Accounts[i].OverageRate = rate
			cfg.Accounts[i].CurrentOverages = current
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return Save()
		}
	}
	return nil
}

// SetAccountEnabled toggles the enabled state of an account and persists the change.
// Used to disable accounts whose refresh token has been revoked (401 Bad credentials)
// so subsequent requests skip them automatically.
func SetAccountEnabled(id string, enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previousAccounts := append([]Account(nil), cfg.Accounts...)
			previousPool := snapshotProxyPoolLocked()
			cfg.Accounts[i].Enabled = enabled
			if enabled {
				cfg.Accounts[i].BanStatus = "ACTIVE"
				cfg.Accounts[i].BanReason = ""
				cfg.Accounts[i].BanTime = 0
			} else {
				cfg.Accounts[i].BanStatus = "DISABLED"
				cfg.Accounts[i].BanTime = time.Now().Unix()
			}
			return saveAccountMutationLocked(previousAccounts, previousPool)
		}
	}
	return nil
}

// SetAccountBanStatus marks an account as banned/disabled with a reason.
// Reason is recorded so operators can see why the account was auto-disabled.
func SetAccountBanStatus(id, status, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previousAccounts := append([]Account(nil), cfg.Accounts...)
			previousPool := snapshotProxyPoolLocked()
			status = strings.ToUpper(strings.TrimSpace(status))
			cfg.Accounts[i].BanStatus = status
			cfg.Accounts[i].BanReason = reason
			cfg.Accounts[i].BanTime = time.Now().Unix()
			if status == "BANNED" || status == "DISABLED" || status == "SUSPENDED" {
				cfg.Accounts[i].Enabled = false
			}
			return saveAccountMutationLocked(previousAccounts, previousPool)
		}
	}
	return nil
}

func AutoQuarantineSuspicious429Reason() string {
	return autoQuarantineSuspicious429Reason
}

// OperatorDisabledReason is the BanReason stamped when a human operator disables
// an account. It marks the account as DISABLED (not SUSPENDED), which keeps the
// auto-restore sweep from ever re-enabling it.
func OperatorDisabledReason() string {
	return operatorDisabledReason
}

func shouldAutoRestoreSuspendedAccount(a Account, now time.Time) bool {
	return a.BanStatus == "SUSPENDED" && a.BanReason == autoQuarantineSuspicious429Reason && a.BanTime > 0 && now.Unix()-a.BanTime >= int64(autoQuarantineDuration/time.Second)
}

func applyAutoRestoreLocked() bool {
	if cfg == nil {
		return false
	}
	now := time.Now()
	var previousAccounts []Account
	var previousPool proxyPoolSnapshot
	changed := false
	for i := range cfg.Accounts {
		if shouldAutoRestoreSuspendedAccount(cfg.Accounts[i], now) {
			if !changed {
				previousAccounts = append([]Account(nil), cfg.Accounts...)
				previousPool = snapshotProxyPoolLocked()
			}
			cfg.Accounts[i].Enabled = true
			cfg.Accounts[i].BanStatus = "ACTIVE"
			cfg.Accounts[i].BanReason = ""
			cfg.Accounts[i].BanTime = 0
			changed = true
		}
	}
	if changed {
		if saveAccountMutationLocked(previousAccounts, previousPool) != nil {
			return false
		}
	}
	return changed
}

// AddAccounts appends multiple accounts in a single locked pass and persists
// with exactly one Save(), avoiding the O(n²) write amplification that calling
// AddAccount in a loop would cause (each AddAccount re-serializes the entire
// config.json). Accounts whose RefreshToken already exists (against the current
// config or earlier entries in the same batch) are skipped to keep bulk imports
// idempotent across retries/re-pastes. Entries with an empty RefreshToken are
// also skipped — there is no stable identity to dedup on and they cannot be
// activated later. Returns how many were added and how many were skipped.
//
// Save() is only invoked when at least one account is actually added, so a
// fully-duplicate batch does not churn the config file.
func AddAccounts(accounts []Account) (added int, skipped int, err error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	previousAccounts := append([]Account(nil), cfg.Accounts...)
	previousPool := snapshotProxyPoolLocked()

	// Seed the seen-set with refresh tokens already persisted so the batch
	// dedups against existing accounts, not just within itself.
	seen := make(map[string]struct{}, len(cfg.Accounts)+len(accounts))
	for i := range cfg.Accounts {
		if rt := cfg.Accounts[i].RefreshToken; rt != "" {
			seen[rt] = struct{}{}
		}
	}

	for _, a := range accounts {
		if a.RefreshToken == "" {
			skipped++
			continue
		}
		if _, dup := seen[a.RefreshToken]; dup {
			skipped++
			continue
		}
		seen[a.RefreshToken] = struct{}{}
		cfg.Accounts = append(cfg.Accounts, a)
		added++
	}

	if added == 0 {
		return 0, skipped, nil
	}
	if err := saveAccountMutationLocked(previousAccounts, previousPool); err != nil {
		return 0, skipped, err
	}
	return added, skipped, nil
}

// RefreshTokenExists reports whether any account already holds the given refresh
// token. Used by bulk import to dedup candidates before spending an upstream
// token-exchange round-trip on a duplicate.
func RefreshTokenExists(refreshToken string) bool {
	if refreshToken == "" {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].RefreshToken == refreshToken {
			return true
		}
	}
	return false
}

// FindAccountIDByRefreshToken returns the id of the account that already holds
// the given refresh token, or "" if none. Used by the single credential-import
// path to update an existing entry in place when a backup is re-imported, rather
// than minting a fresh id and leaving two live accounts sharing the same token.
// Mirrors the refresh-token dedup the bulk path applies (AddAccounts).
func FindAccountIDByRefreshToken(refreshToken string) string {
	if refreshToken == "" {
		return ""
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].RefreshToken == refreshToken {
			return cfg.Accounts[i].ID
		}
	}
	return ""
}

func SuspendAccountTemporarily(id, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	now := time.Now().Unix()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previousAccounts := append([]Account(nil), cfg.Accounts...)
			previousPool := snapshotProxyPoolLocked()
			cfg.Accounts[i].Enabled = false
			cfg.Accounts[i].BanStatus = "SUSPENDED"
			cfg.Accounts[i].BanReason = reason
			cfg.Accounts[i].BanTime = now
			return saveAccountMutationLocked(previousAccounts, previousPool)
		}
	}
	return nil
}

// ClearAccountCurrentOverages zeroes the cached CurrentOverages for an account
// while preserving the OverageStatus switch and the cap/rate billing config.
// Called when upstream usage has fallen back within the subscription quota
// (e.g. after a billing-period reset): overage points are zero by definition
// when usage is within quota, so stale points from a previous period must not
// linger in the UI/scheduler. Returns without writing if already zero, so the
// periodic refresh loop does not churn the config file every cycle.
func ClearAccountCurrentOverages(id string, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if cfg.Accounts[i].CurrentOverages == 0 {
				return nil
			}
			cfg.Accounts[i].CurrentOverages = 0
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return Save()
		}
	}
	return nil
}

func UpdateAccountProfileArn(id, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ProfileArn = profileArn
			return Save()
		}
	}
	return nil
}

func DeleteAccount(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previousAccounts := append([]Account(nil), cfg.Accounts...)
			previousPool := snapshotProxyPoolLocked()
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			return saveAccountMutationLocked(previousAccounts, previousPool)
		}
	}
	return nil
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				cfg.Accounts[i].RefreshToken = refreshToken
			}
			cfg.Accounts[i].ExpiresAt = expiresAt
			return Save()
		}
	}
	return nil
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettings(apiKey string, requireApiKey bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ApiKey = apiKey
	cfg.RequireApiKey = requireApiKey
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateSettingsPatch(apiKey *string, requireApiKey *bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if apiKey != nil {
		cfg.ApiKey = *apiKey
	}
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	return UpdateRuntimeStats(totalReq, successReq, failedReq, totalTokens, totalCredits, nil)
}

// AccountStatsSnapshot is the non-sensitive subset of an account persisted by
// the periodic runtime statistics flush.
type AccountStatsSnapshot struct {
	RequestCount int
	ErrorCount   int
	TotalTokens  int
	TotalCredits float64
	LastUsed     int64
}

// UpdateRuntimeStats persists global and per-account counters with one atomic
// config write. Request handlers only mutate the in-memory pool between flushes.
func UpdateRuntimeStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64, accountStats map[string]AccountStatsSnapshot) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	previousTotalReq, previousSuccessReq := cfg.TotalRequests, cfg.SuccessRequests
	previousFailedReq, previousTotalTokens := cfg.FailedRequests, cfg.TotalTokens
	previousTotalCredits := cfg.TotalCredits
	previousAccountStats := make(map[string]AccountStatsSnapshot, len(accountStats))
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	for i := range cfg.Accounts {
		stats, ok := accountStats[cfg.Accounts[i].ID]
		if !ok {
			continue
		}
		previousAccountStats[cfg.Accounts[i].ID] = AccountStatsSnapshot{
			RequestCount: cfg.Accounts[i].RequestCount,
			ErrorCount:   cfg.Accounts[i].ErrorCount,
			TotalTokens:  cfg.Accounts[i].TotalTokens,
			TotalCredits: cfg.Accounts[i].TotalCredits,
			LastUsed:     cfg.Accounts[i].LastUsed,
		}
		cfg.Accounts[i].RequestCount = stats.RequestCount
		cfg.Accounts[i].ErrorCount = stats.ErrorCount
		cfg.Accounts[i].TotalTokens = stats.TotalTokens
		cfg.Accounts[i].TotalCredits = stats.TotalCredits
		cfg.Accounts[i].LastUsed = stats.LastUsed
	}
	if err := Save(); err != nil {
		cfg.TotalRequests = previousTotalReq
		cfg.SuccessRequests = previousSuccessReq
		cfg.FailedRequests = previousFailedReq
		cfg.TotalTokens = previousTotalTokens
		cfg.TotalCredits = previousTotalCredits
		for i := range cfg.Accounts {
			if stats, ok := previousAccountStats[cfg.Accounts[i].ID]; ok {
				cfg.Accounts[i].RequestCount = stats.RequestCount
				cfg.Accounts[i].ErrorCount = stats.ErrorCount
				cfg.Accounts[i].TotalTokens = stats.TotalTokens
				cfg.Accounts[i].TotalCredits = stats.TotalCredits
				cfg.Accounts[i].LastUsed = stats.LastUsed
			}
		}
		return err
	}
	return nil
}

// UpdateAccountStatsSnapshots persists account counters in one config write.
// It is used by explicit pool flushes without touching global runtime totals.
func UpdateAccountStatsSnapshots(accountStats map[string]AccountStatsSnapshot) error {
	if len(accountStats) == 0 {
		return nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	previous := make(map[string]AccountStatsSnapshot, len(accountStats))
	for i := range cfg.Accounts {
		stats, ok := accountStats[cfg.Accounts[i].ID]
		if !ok {
			continue
		}
		previous[cfg.Accounts[i].ID] = AccountStatsSnapshot{
			RequestCount: cfg.Accounts[i].RequestCount,
			ErrorCount:   cfg.Accounts[i].ErrorCount,
			TotalTokens:  cfg.Accounts[i].TotalTokens,
			TotalCredits: cfg.Accounts[i].TotalCredits,
			LastUsed:     cfg.Accounts[i].LastUsed,
		}
		cfg.Accounts[i].RequestCount = stats.RequestCount
		cfg.Accounts[i].ErrorCount = stats.ErrorCount
		cfg.Accounts[i].TotalTokens = stats.TotalTokens
		cfg.Accounts[i].TotalCredits = stats.TotalCredits
		cfg.Accounts[i].LastUsed = stats.LastUsed
	}
	if err := Save(); err != nil {
		for i := range cfg.Accounts {
			if stats, ok := previous[cfg.Accounts[i].ID]; ok {
				cfg.Accounts[i].RequestCount = stats.RequestCount
				cfg.Accounts[i].ErrorCount = stats.ErrorCount
				cfg.Accounts[i].TotalTokens = stats.TotalTokens
				cfg.Accounts[i].TotalCredits = stats.TotalCredits
				cfg.Accounts[i].LastUsed = stats.LastUsed
			}
		}
		return err
	}
	return nil
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].RequestCount = requestCount
			cfg.Accounts[i].ErrorCount = errorCount
			cfg.Accounts[i].TotalTokens = totalTokens
			cfg.Accounts[i].TotalCredits = totalCredits
			cfg.Accounts[i].LastUsed = lastUsed
			return Save()
		}
	}
	return nil
}

// UpdateAccountInfo updates an account's subscription and usage information.
// Called after refreshing account data from Kiro API.
func UpdateAccountInfo(id string, info AccountInfo) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if info.Email != "" {
				cfg.Accounts[i].Email = info.Email
			}
			if info.UserId != "" {
				cfg.Accounts[i].UserId = info.UserId
			}
			cfg.Accounts[i].SubscriptionType = info.SubscriptionType
			cfg.Accounts[i].SubscriptionTitle = info.SubscriptionTitle
			cfg.Accounts[i].DaysRemaining = info.DaysRemaining
			cfg.Accounts[i].UsageCurrent = info.UsageCurrent
			cfg.Accounts[i].UsageLimit = info.UsageLimit
			cfg.Accounts[i].UsagePercent = info.UsagePercent
			cfg.Accounts[i].NextResetDate = info.NextResetDate
			cfg.Accounts[i].LastRefresh = info.LastRefresh
			cfg.Accounts[i].TrialUsageCurrent = info.TrialUsageCurrent
			cfg.Accounts[i].TrialUsageLimit = info.TrialUsageLimit
			cfg.Accounts[i].TrialUsagePercent = info.TrialUsagePercent
			cfg.Accounts[i].TrialStatus = info.TrialStatus
			cfg.Accounts[i].TrialExpiresAt = info.TrialExpiresAt
			return Save()
		}
	}
	return nil
}

// GetFilterClaudeCode returns whether Claude Code system prompt detection is enabled.
// Also checks the legacy SanitizeClaudeCodePrompt flag for backward compatibility.
func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt
}

// GetFilterEnvNoise returns whether environment noise line stripping is enabled.
func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

// GetFilterStripBoundaries returns whether boundary marker stripping is enabled.
func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

// PromptFilterConfig holds all prompt filter settings for API responses.
type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	Rules                 []PromptFilterRule `json:"rules"`
}

// GetPromptFilterConfig returns all prompt filter settings.
func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		Rules:                 rules,
	}
}

// UpdatePromptFilterConfig saves all prompt filter settings atomically.
func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.FilterClaudeCode = filterClaudeCode
	cfg.FilterEnvNoise = filterEnvNoise
	cfg.FilterStripBoundaries = filterStripBoundaries
	// Clear legacy flag to avoid double-applying after first save
	cfg.SanitizeClaudeCodePrompt = false
	if rules != nil {
		cfg.PromptFilterRules = rules
	}
	return Save()
}

// GetPromptFilterRules returns the current prompt filter rules.
func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix       string `json:"suffix"`       // Model name suffix that triggers thinking mode
	OpenAIFormat string `json:"openaiFormat"` // Output format for OpenAI-compatible responses
	ClaudeFormat string `json:"claudeFormat"` // Output format for Claude-compatible responses
}

// GetThinkingConfig 获取 thinking 配置
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: openaiFormat,
		ClaudeFormat: claudeFormat,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	return Save()
}

// GetPreferredEndpoint 获取首选端点配置
func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

// GetEndpointFallback returns whether endpoint fallback is enabled. Defaults to true.
func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

// Opus55EmptyMapToOpus5Enabled reports whether an exhausted opus 5.5 empty
// stream may be retried upstream as opus 5. The default is off.
func Opus55EmptyMapToOpus5Enabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg != nil && cfg.Opus55EmptyMapToOpus5 != nil && *cfg.Opus55EmptyMapToOpus5
}

// UpdateOpus55EmptyMapToOpus5 stores the fallback switch.
func UpdateOpus55EmptyMapToOpus5(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Opus55EmptyMapToOpus5 = &enabled
	return saveLocked()
}

// EmptyOutputModelCooldownEnabled reports whether an events=none empty body
// should cool the account and model pair. The default is enabled.
func EmptyOutputModelCooldownEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.EmptyOutputCooldownEnabled == nil {
		return true
	}
	return *cfg.EmptyOutputCooldownEnabled
}

// EmptyOutputModelCooldownMinutes returns the configured cooldown, or 30.
func EmptyOutputModelCooldownMinutes() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.EmptyOutputCooldownMinutes <= 0 {
		return 30
	}
	return cfg.EmptyOutputCooldownMinutes
}

// UpdateEmptyOutputCooldown stores the switch and duration.
func UpdateEmptyOutputCooldown(enabled bool, minutes int) error {
	if minutes <= 0 {
		minutes = 30
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EmptyOutputCooldownEnabled = &enabled
	cfg.EmptyOutputCooldownMinutes = minutes
	return saveLocked()
}

// GetProxyURL 获取出站代理地址
func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ProxyURL
}

// UpdateProxySettings 更新出站代理配置
func UpdateProxySettings(proxyURL string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	return Save()
}

// GetAllowOverUsage returns whether over-usage is allowed when account quota is exhausted.
func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

// UpdateAllowOverUsage sets the over-usage setting and persists the change.
func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// GetAgentCompletionMarkerEnabled reports whether automatic coding-agent turns
// use the private final-answer marker protocol. Unset legacy configs default on.
func GetAgentCompletionMarkerEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg == nil || cfg.AgentCompletionMarkerEnabled == nil || *cfg.AgentCompletionMarkerEnabled
}

// UpdateAgentCompletionMarkerEnabled persists the marker setting. Translators
// read this value for every request, so a successful update takes effect at once.
func UpdateAgentCompletionMarkerEnabled(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("config is not initialized")
	}
	previous := cfg.AgentCompletionMarkerEnabled
	value := enabled
	cfg.AgentCompletionMarkerEnabled = &value
	if err := Save(); err != nil {
		cfg.AgentCompletionMarkerEnabled = previous
		return err
	}
	return nil
}

const (
	DefaultAccountConcurrencyRefreshSeconds = 5
	MinAccountConcurrencyRefreshSeconds     = 5
	MaxAccountConcurrencyRefreshSeconds     = 60
)

// GetAccountConcurrencyRefreshSeconds returns the admin account-card polling interval.
func GetAccountConcurrencyRefreshSeconds() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.AccountConcurrencyRefreshSeconds < MinAccountConcurrencyRefreshSeconds || cfg.AccountConcurrencyRefreshSeconds > MaxAccountConcurrencyRefreshSeconds {
		return DefaultAccountConcurrencyRefreshSeconds
	}
	return cfg.AccountConcurrencyRefreshSeconds
}

// UpdateAccountConcurrencyRefreshSeconds validates and persists the admin polling interval.
func UpdateAccountConcurrencyRefreshSeconds(seconds int) error {
	if seconds < MinAccountConcurrencyRefreshSeconds || seconds > MaxAccountConcurrencyRefreshSeconds {
		return fmt.Errorf("accountConcurrencyRefreshSeconds must be between %d and %d", MinAccountConcurrencyRefreshSeconds, MaxAccountConcurrencyRefreshSeconds)
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("config is not initialized")
	}
	previous := cfg.AccountConcurrencyRefreshSeconds
	cfg.AccountConcurrencyRefreshSeconds = seconds
	if err := Save(); err != nil {
		cfg.AccountConcurrencyRefreshSeconds = previous
		return err
	}
	return nil
}

// GetGlobalRegion returns the configured default region (empty → "us-east-1").
func GetGlobalRegion() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.Region == "" {
		return "us-east-1"
	}
	return cfg.Region
}

// GetGlobalAuthRegion returns the configured default token-refresh region.
func GetGlobalAuthRegion() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.AuthRegion == "" {
		return "us-east-1"
	}
	return cfg.AuthRegion
}

// GetGlobalApiRegion returns the configured default API-request host region.
func GetGlobalApiRegion() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ApiRegion == "" {
		return "us-east-1"
	}
	return cfg.ApiRegion
}

// GetLogLevel returns the configured log level (debug/info/warn/error). Defaults to "info".
func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

const (
	// MinPromptCacheRatio is the smallest cache-read ratio accepted by Admin.
	MinPromptCacheRatio = 0.01
	// MaxPromptCacheRatio is the largest cache-read ratio accepted by Admin.
	MaxPromptCacheRatio = 1.0
)

// GetPromptCacheMaxRatio returns the cache-read cap ratio. Defaults to 0.85 and
// clamps legacy configuration values to the range supported by Admin.
func GetPromptCacheMaxRatio() float64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PromptCacheMaxRatio <= 0 {
		return 0.85
	}
	if cfg.PromptCacheMaxRatio < MinPromptCacheRatio {
		return MinPromptCacheRatio
	}
	if cfg.PromptCacheMaxRatio > MaxPromptCacheRatio {
		return MaxPromptCacheRatio
	}
	return cfg.PromptCacheMaxRatio
}

// UpdatePromptCacheMaxRatio sets the cache-read cap ratio and persists the change.
func UpdatePromptCacheMaxRatio(ratio float64) error {
	return UpdatePromptCacheSettings(&ratio, nil, nil)
}

const defaultPromptCacheMaxEntries = 131072

// MinPromptCacheEntries is the safety floor accepted by the Admin API.
const MinPromptCacheEntries = 256

// MaxPromptCacheEntries limits live Admin resizes to a predictable memory bound.
const MaxPromptCacheEntries = 1048576

// GetPromptCacheMaxEntries returns the prompt-cache LRU bound. Defaults to
// 131072 when unset (≤ 0); an explicit small value is clamped up to
// MinPromptCacheEntries (256) so a misconfigured tiny value cannot make the
// cache useless. This is the production safety floor — the tracker constructor
// trusts its caller (tests may use any capacity).
func GetPromptCacheMaxEntries() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PromptCacheMaxEntries <= 0 {
		return defaultPromptCacheMaxEntries
	}
	if cfg.PromptCacheMaxEntries < MinPromptCacheEntries {
		return MinPromptCacheEntries
	}
	if cfg.PromptCacheMaxEntries > MaxPromptCacheEntries {
		return MaxPromptCacheEntries
	}
	return cfg.PromptCacheMaxEntries
}

// UpdatePromptCacheMaxEntries persists the prompt-cache LRU bound. Runtime
// callers apply the returned configuration to the live tracker with SetMaxEntries.
func UpdatePromptCacheMaxEntries(n int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	previous := cfg.PromptCacheMaxEntries
	cfg.PromptCacheMaxEntries = n
	if err := Save(); err != nil {
		cfg.PromptCacheMaxEntries = previous
		return err
	}
	return nil
}

const (
	// DefaultPromptCacheTTLSeconds is Anthropic's default ephemeral cache TTL.
	DefaultPromptCacheTTLSeconds = 5 * 60
	// ExtendedPromptCacheTTLSeconds is Anthropic's optional one-hour cache TTL.
	ExtendedPromptCacheTTLSeconds = 60 * 60
)

// IsValidPromptCacheTTLSeconds reports whether seconds is one of Anthropic's
// supported ephemeral cache TTLs.
func IsValidPromptCacheTTLSeconds(seconds int) bool {
	return seconds == DefaultPromptCacheTTLSeconds || seconds == ExtendedPromptCacheTTLSeconds
}

// GetPromptCacheTTLSeconds returns the default TTL used for cache_control
// markers that omit ttl. Unset or legacy invalid values fall back to 5 minutes.
func GetPromptCacheTTLSeconds() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || !IsValidPromptCacheTTLSeconds(cfg.PromptCacheTTLSeconds) {
		return DefaultPromptCacheTTLSeconds
	}
	return cfg.PromptCacheTTLSeconds
}

// UpdatePromptCacheTTLSeconds persists the default prompt-cache TTL.
func UpdatePromptCacheTTLSeconds(seconds int) error {
	return UpdatePromptCacheSettings(nil, nil, &seconds)
}

// UpdatePromptCacheSettings validates and persists all cache controls as one
// update. In-memory values are rolled back when persistence fails.
func UpdatePromptCacheSettings(ratio *float64, maxEntries *int, ttlSeconds *int) error {
	if ratio != nil && (*ratio < MinPromptCacheRatio || *ratio > MaxPromptCacheRatio) {
		return fmt.Errorf("promptCacheMaxRatio must be between %.2f and %.2f", MinPromptCacheRatio, MaxPromptCacheRatio)
	}
	if maxEntries != nil && (*maxEntries < MinPromptCacheEntries || *maxEntries > MaxPromptCacheEntries) {
		return fmt.Errorf("promptCacheMaxEntries must be between %d and %d", MinPromptCacheEntries, MaxPromptCacheEntries)
	}
	if ttlSeconds != nil && !IsValidPromptCacheTTLSeconds(*ttlSeconds) {
		return fmt.Errorf("promptCacheTTLSeconds must be either %d or %d", DefaultPromptCacheTTLSeconds, ExtendedPromptCacheTTLSeconds)
	}
	if ratio == nil && maxEntries == nil && ttlSeconds == nil {
		return nil
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	previousRatio := cfg.PromptCacheMaxRatio
	previousMaxEntries := cfg.PromptCacheMaxEntries
	previousTTLSeconds := cfg.PromptCacheTTLSeconds
	if ratio != nil {
		cfg.PromptCacheMaxRatio = *ratio
	}
	if maxEntries != nil {
		cfg.PromptCacheMaxEntries = *maxEntries
	}
	if ttlSeconds != nil {
		cfg.PromptCacheTTLSeconds = *ttlSeconds
	}
	if err := Save(); err != nil {
		cfg.PromptCacheMaxRatio = previousRatio
		cfg.PromptCacheMaxEntries = previousMaxEntries
		cfg.PromptCacheTTLSeconds = previousTTLSeconds
		return err
	}
	return nil
}

// UpdateLogLevel updates the log level setting and persists the change.
func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LogLevel = level
	return Save()
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}
