package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrPromptCachePolicyNotFound is returned when a named cache policy does not exist.
	ErrPromptCachePolicyNotFound = errors.New("prompt cache policy not found")
	// ErrPromptCachePolicyInUse is returned when deleting a policy referenced by an API key.
	ErrPromptCachePolicyInUse = errors.New("prompt cache policy is in use")
	// ErrPromptCachePolicyInvalid identifies client-side policy validation errors.
	ErrPromptCachePolicyInvalid = errors.New("invalid prompt cache policy")
)

// PromptCachePolicy is a named prompt-cache configuration that can be bound to
// one or more API keys. The legacy global cache fields remain the fallback for
// API keys whose PromptCachePolicyID is empty or no longer resolves.
type PromptCachePolicy struct {
	ID                    string                `json:"id"`
	Name                  string                `json:"name"`
	Revision              uint64                `json:"revision,omitempty"`
	CacheMode             string                `json:"cacheMode"`
	FunctionCache         FunctionCacheConfig   `json:"functionCache"`
	FixedRatioCache       FixedRatioCacheConfig `json:"fixedRatioCache"`
	PromptCacheMaxRatio   float64               `json:"promptCacheMaxRatio"`
	PromptCacheMaxEntries int                   `json:"promptCacheMaxEntries"`
	PromptCacheTTLSeconds int                   `json:"promptCacheTTLSeconds"`
	CreatedAt             int64                 `json:"createdAt"`
	UpdatedAt             int64                 `json:"updatedAt"`
}

// ListPromptCachePolicies returns normalized copies of all named policies.
func ListPromptCachePolicies() []PromptCachePolicy {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]PromptCachePolicy, len(cfg.PromptCachePolicies))
	for i := range cfg.PromptCachePolicies {
		out[i] = normalizeStoredPromptCachePolicy(cfg.PromptCachePolicies[i])
	}
	return out
}

// GetPromptCachePolicy returns a normalized copy of a named policy.
func GetPromptCachePolicy(id string) *PromptCachePolicy {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	id = strings.TrimSpace(id)
	for i := range cfg.PromptCachePolicies {
		if cfg.PromptCachePolicies[i].ID == id {
			policy := normalizeStoredPromptCachePolicy(cfg.PromptCachePolicies[i])
			return &policy
		}
	}
	return nil
}

// AddPromptCachePolicy validates, normalizes, and persists a new named policy.
func AddPromptCachePolicy(policy PromptCachePolicy) (PromptCachePolicy, error) {
	normalized, err := normalizePromptCachePolicyInput(policy)
	if err != nil {
		return PromptCachePolicy{}, err
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return PromptCachePolicy{}, errors.New("config not initialized")
	}
	for _, existing := range cfg.PromptCachePolicies {
		if existing.ID == normalized.ID && normalized.ID != "" {
			return PromptCachePolicy{}, fmt.Errorf("%w: ID already exists", ErrPromptCachePolicyInvalid)
		}
		if strings.EqualFold(strings.TrimSpace(existing.Name), normalized.Name) {
			return PromptCachePolicy{}, fmt.Errorf("%w: name already exists", ErrPromptCachePolicyInvalid)
		}
	}

	if normalized.ID == "" {
		normalized.ID = newUUID()
	}
	normalized.Revision = 1
	now := time.Now().Unix()
	if normalized.CreatedAt == 0 {
		normalized.CreatedAt = now
	}
	normalized.UpdatedAt = now
	cfg.PromptCachePolicies = append(cfg.PromptCachePolicies, normalized)
	if err := saveLocked(); err != nil {
		cfg.PromptCachePolicies = cfg.PromptCachePolicies[:len(cfg.PromptCachePolicies)-1]
		return PromptCachePolicy{}, err
	}
	return clonePromptCachePolicy(normalized), nil
}

// UpdatePromptCachePolicy replaces the editable fields of an existing policy.
// The ID and creation timestamp are immutable. Zero-valued settings reset to
// their current defaults.
func UpdatePromptCachePolicy(id string, policy PromptCachePolicy) (PromptCachePolicy, error) {
	normalized, err := normalizePromptCachePolicyInput(policy)
	if err != nil {
		return PromptCachePolicy{}, err
	}
	id = strings.TrimSpace(id)

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return PromptCachePolicy{}, errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.PromptCachePolicies {
		if cfg.PromptCachePolicies[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return PromptCachePolicy{}, ErrPromptCachePolicyNotFound
	}
	for i := range cfg.PromptCachePolicies {
		if i != idx && strings.EqualFold(strings.TrimSpace(cfg.PromptCachePolicies[i].Name), normalized.Name) {
			return PromptCachePolicy{}, fmt.Errorf("%w: name already exists", ErrPromptCachePolicyInvalid)
		}
	}

	previous := cfg.PromptCachePolicies[idx]
	normalized.ID = previous.ID
	normalized.CreatedAt = previous.CreatedAt
	previousRevision := previous.Revision
	if previousRevision == 0 {
		previousRevision = 1
	}
	normalized.Revision = previousRevision + 1
	if normalized.CreatedAt == 0 {
		normalized.CreatedAt = time.Now().Unix()
	}
	normalized.UpdatedAt = time.Now().Unix()
	cfg.PromptCachePolicies[idx] = normalized
	if err := saveLocked(); err != nil {
		cfg.PromptCachePolicies[idx] = previous
		return PromptCachePolicy{}, err
	}
	return clonePromptCachePolicy(normalized), nil
}

// DeletePromptCachePolicy removes a policy only when no API key references it.
// The reference check and deletion happen under the same configuration lock.
func DeletePromptCachePolicy(id string) error {
	id = strings.TrimSpace(id)
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.PromptCachePolicies {
		if cfg.PromptCachePolicies[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrPromptCachePolicyNotFound
	}
	for _, apiKey := range cfg.ApiKeys {
		if apiKey.PromptCachePolicyID == id {
			return fmt.Errorf("%w: API key %q", ErrPromptCachePolicyInUse, apiKey.ID)
		}
	}

	previous := append([]PromptCachePolicy(nil), cfg.PromptCachePolicies...)
	cfg.PromptCachePolicies = append(cfg.PromptCachePolicies[:idx], cfg.PromptCachePolicies[idx+1:]...)
	if err := saveLocked(); err != nil {
		cfg.PromptCachePolicies = previous
		return err
	}
	return nil
}

// ResolvePromptCachePolicyForAPIKey returns the named policy bound to apiKeyID.
// Missing API keys, unbound keys, and dangling policy references all resolve to
// a snapshot of the legacy global prompt-cache settings whose ID is empty.
func ResolvePromptCachePolicyForAPIKey(apiKeyID string) PromptCachePolicy {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return globalPromptCachePolicyLocked()
	}

	policyID := ""
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return globalPromptCachePolicyLocked()
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == apiKeyID {
			policyID = cfg.ApiKeys[i].PromptCachePolicyID
			break
		}
	}
	if policyID != "" {
		for i := range cfg.PromptCachePolicies {
			if cfg.PromptCachePolicies[i].ID == policyID {
				return normalizeStoredPromptCachePolicy(cfg.PromptCachePolicies[i])
			}
		}
	}
	return globalPromptCachePolicyLocked()
}

func validatePromptCachePolicyBindingLocked(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	for i := range cfg.PromptCachePolicies {
		if cfg.PromptCachePolicies[i].ID == id {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrPromptCachePolicyNotFound, id)
}

func normalizePromptCachePolicyInput(policy PromptCachePolicy) (PromptCachePolicy, error) {
	policy.ID = strings.TrimSpace(policy.ID)
	policy.Name = strings.TrimSpace(policy.Name)
	if policy.Name == "" {
		return PromptCachePolicy{}, fmt.Errorf("%w: name must not be empty", ErrPromptCachePolicyInvalid)
	}

	switch mode := strings.TrimSpace(policy.CacheMode); mode {
	case "":
		policy.CacheMode = CacheModeNative
	case CacheModeNative, CacheModeFunctionSimulation, CacheModeFixedRatio:
		policy.CacheMode = mode
	default:
		return PromptCachePolicy{}, fmt.Errorf("%w: cacheMode must be native, function_simulation, or fixed_ratio", ErrPromptCachePolicyInvalid)
	}

	if policy.PromptCacheMaxRatio == 0 {
		policy.PromptCacheMaxRatio = 0.85
	}
	if policy.PromptCacheMaxRatio < MinPromptCacheRatio || policy.PromptCacheMaxRatio > MaxPromptCacheRatio {
		return PromptCachePolicy{}, fmt.Errorf("%w: promptCacheMaxRatio must be between %.2f and %.2f", ErrPromptCachePolicyInvalid, MinPromptCacheRatio, MaxPromptCacheRatio)
	}
	if policy.PromptCacheMaxEntries == 0 {
		policy.PromptCacheMaxEntries = defaultPromptCacheMaxEntries
	}
	if policy.PromptCacheMaxEntries < MinPromptCacheEntries || policy.PromptCacheMaxEntries > MaxPromptCacheEntries {
		return PromptCachePolicy{}, fmt.Errorf("%w: promptCacheMaxEntries must be between %d and %d", ErrPromptCachePolicyInvalid, MinPromptCacheEntries, MaxPromptCacheEntries)
	}
	if policy.PromptCacheTTLSeconds == 0 {
		policy.PromptCacheTTLSeconds = DefaultPromptCacheTTLSeconds
	}
	if !IsValidPromptCacheTTLSeconds(policy.PromptCacheTTLSeconds) {
		return PromptCachePolicy{}, fmt.Errorf("%w: promptCacheTTLSeconds must be either %d or %d", ErrPromptCachePolicyInvalid, DefaultPromptCacheTTLSeconds, ExtendedPromptCacheTTLSeconds)
	}
	policy.FunctionCache = normalizeFunctionCacheConfig(policy.FunctionCache)
	policy.FixedRatioCache = normalizeFixedRatioCacheConfig(policy.FixedRatioCache)
	return policy, nil
}

func normalizeStoredPromptCachePolicy(policy PromptCachePolicy) PromptCachePolicy {
	policy.ID = strings.TrimSpace(policy.ID)
	policy.Name = strings.TrimSpace(policy.Name)
	if policy.Revision == 0 {
		policy.Revision = 1
	}
	policy.CacheMode = normalizeCacheMode(policy.CacheMode)
	policy.FunctionCache = normalizeFunctionCacheConfig(policy.FunctionCache)
	policy.FixedRatioCache = normalizeFixedRatioCacheConfig(policy.FixedRatioCache)
	policy.PromptCacheMaxRatio = effectivePromptCacheRatio(policy.PromptCacheMaxRatio)
	policy.PromptCacheMaxEntries = effectivePromptCacheMaxEntries(policy.PromptCacheMaxEntries)
	if !IsValidPromptCacheTTLSeconds(policy.PromptCacheTTLSeconds) {
		policy.PromptCacheTTLSeconds = DefaultPromptCacheTTLSeconds
	}
	return policy
}

func globalPromptCachePolicyLocked() PromptCachePolicy {
	policy := PromptCachePolicy{
		CacheMode:             CacheModeNative,
		FunctionCache:         normalizeFunctionCacheConfig(FunctionCacheConfig{}),
		FixedRatioCache:       normalizeFixedRatioCacheConfig(FixedRatioCacheConfig{}),
		PromptCacheMaxRatio:   0.85,
		PromptCacheMaxEntries: defaultPromptCacheMaxEntries,
		PromptCacheTTLSeconds: DefaultPromptCacheTTLSeconds,
	}
	if cfg == nil {
		return policy
	}
	policy.CacheMode = normalizeCacheMode(cfg.CacheMode)
	policy.FunctionCache = normalizeFunctionCacheConfig(cfg.FunctionCache)
	policy.FixedRatioCache = normalizeFixedRatioCacheConfig(cfg.FixedRatioCache)
	policy.PromptCacheMaxRatio = effectivePromptCacheRatio(cfg.PromptCacheMaxRatio)
	policy.PromptCacheMaxEntries = effectivePromptCacheMaxEntries(cfg.PromptCacheMaxEntries)
	if IsValidPromptCacheTTLSeconds(cfg.PromptCacheTTLSeconds) {
		policy.PromptCacheTTLSeconds = cfg.PromptCacheTTLSeconds
	}
	return policy
}

func effectivePromptCacheRatio(ratio float64) float64 {
	if ratio <= 0 {
		return 0.85
	}
	if ratio < MinPromptCacheRatio {
		return MinPromptCacheRatio
	}
	if ratio > MaxPromptCacheRatio {
		return MaxPromptCacheRatio
	}
	return ratio
}

func effectivePromptCacheMaxEntries(entries int) int {
	if entries <= 0 {
		return defaultPromptCacheMaxEntries
	}
	if entries < MinPromptCacheEntries {
		return MinPromptCacheEntries
	}
	if entries > MaxPromptCacheEntries {
		return MaxPromptCacheEntries
	}
	return entries
}

func clonePromptCachePolicy(policy PromptCachePolicy) PromptCachePolicy {
	policy.FunctionCache = normalizeFunctionCacheConfig(policy.FunctionCache)
	policy.FixedRatioCache = normalizeFixedRatioCacheConfig(policy.FixedRatioCache)
	return policy
}
