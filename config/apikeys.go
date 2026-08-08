package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

var (
	ErrApiKeyNotFound = errors.New("api key not found")
	ErrApiKeyInvalid  = errors.New("invalid api key")
)

// ApiKeyPatch represents an atomic partial update. Pointer fields distinguish
// omitted values from explicit zero values without first reading a stale copy.
type ApiKeyPatch struct {
	Name                *string
	Key                 *string
	Enabled             *bool
	TokenLimit          *int64
	CreditLimit         *float64
	PromptCachePolicyID *string
}

// ListApiKeys returns a snapshot of all configured API key entries.
func ListApiKeys() []ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ApiKeyEntry, len(cfg.ApiKeys))
	copy(out, cfg.ApiKeys)
	for i := range out {
		mergeApiKeyUsage(&out[i])
	}
	return out
}

// GetApiKeyEntry returns a copy of the entry with the given ID, or nil if not found.
func GetApiKeyEntry(id string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cp := cfg.ApiKeys[i]
			mergeApiKeyUsage(&cp)
			return &cp
		}
	}
	return nil
}

// AddApiKey appends a new API key entry. Generates ID and CreatedAt if missing,
// rejects empty Key values, and refuses duplicates of an existing Key.
func AddApiKey(entry ApiKeyEntry) (ApiKeyEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ApiKeyEntry{}, errors.New("config not initialized")
	}
	entry.Key = strings.TrimSpace(entry.Key)
	entry.PromptCachePolicyID = strings.TrimSpace(entry.PromptCachePolicyID)
	if entry.Key == "" {
		return ApiKeyEntry{}, fmt.Errorf("%w: value must not be empty", ErrApiKeyInvalid)
	}
	if entry.TokenLimit < 0 {
		return ApiKeyEntry{}, fmt.Errorf("%w: tokenLimit must not be negative", ErrApiKeyInvalid)
	}
	if entry.CreditLimit < 0 {
		return ApiKeyEntry{}, fmt.Errorf("%w: creditLimit must not be negative", ErrApiKeyInvalid)
	}
	if err := validatePromptCachePolicyBindingLocked(entry.PromptCachePolicyID); err != nil {
		return ApiKeyEntry{}, err
	}
	for _, existing := range cfg.ApiKeys {
		if entry.ID != "" && existing.ID == entry.ID {
			return ApiKeyEntry{}, fmt.Errorf("%w: id already exists", ErrApiKeyInvalid)
		}
		if existing.Key == entry.Key {
			return ApiKeyEntry{}, fmt.Errorf("%w: value already exists", ErrApiKeyInvalid)
		}
	}
	if entry.ID == "" {
		entry.ID = newUUID()
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	cfg.ApiKeys = append(cfg.ApiKeys, entry)
	if err := saveLocked(); err != nil {
		// Roll back the in-memory append so we don't leave inconsistent state.
		cfg.ApiKeys = cfg.ApiKeys[:len(cfg.ApiKeys)-1]
		return ApiKeyEntry{}, err
	}
	registerApiKeyUsageStateLocked(entry.ID)
	return entry, nil
}

// UpdateApiKey applies a patch to an existing API key. Patch semantics:
//   - Name, Key are overwritten when non-empty in patch.
//   - Enabled, TokenLimit, CreditLimit are always overwritten (zero values are valid).
//   - Counters (TokensUsed/CreditsUsed/RequestsCount) are not touched here; use
//     RecordApiKeyUsage or ResetApiKeyUsage instead.
//   - Migrated stays as-is once true; only flips when explicitly set in patch.
//   - PromptCachePolicyID is always overwritten; an empty value selects the
//     legacy global prompt-cache settings.
func UpdateApiKey(id string, patch ApiKeyEntry) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrApiKeyNotFound
	}
	patch.PromptCachePolicyID = strings.TrimSpace(patch.PromptCachePolicyID)
	if err := validatePromptCachePolicyBindingLocked(patch.PromptCachePolicyID); err != nil {
		return err
	}
	previous := cfg.ApiKeys[idx]
	updated := previous
	if patch.Name != "" {
		updated.Name = patch.Name
	}
	if patch.Key != "" {
		newKey := strings.TrimSpace(patch.Key)
		// Reject duplicates against any other entry.
		for j := range cfg.ApiKeys {
			if j != idx && cfg.ApiKeys[j].Key == newKey {
				return errors.New("api key value collides with existing entry")
			}
		}
		updated.Key = newKey
	}
	updated.Enabled = patch.Enabled
	updated.TokenLimit = patch.TokenLimit
	updated.CreditLimit = patch.CreditLimit
	updated.PromptCachePolicyID = patch.PromptCachePolicyID
	if patch.Migrated {
		updated.Migrated = true
	}
	cfg.ApiKeys[idx] = updated
	if err := saveLocked(); err != nil {
		cfg.ApiKeys[idx] = previous
		return err
	}
	return nil
}

// PatchApiKey applies only the provided fields while holding cfgLock for the
// complete read-modify-write transaction.
func PatchApiKey(id string, patch ApiKeyPatch) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrApiKeyNotFound
	}

	previous := cfg.ApiKeys[idx]
	updated := previous
	if patch.Name != nil {
		updated.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.Key != nil {
		newKey := strings.TrimSpace(*patch.Key)
		if newKey != "" {
			for j := range cfg.ApiKeys {
				if j != idx && cfg.ApiKeys[j].Key == newKey {
					return fmt.Errorf("%w: value collides with existing entry", ErrApiKeyInvalid)
				}
			}
			updated.Key = newKey
		}
	}
	if patch.Enabled != nil {
		updated.Enabled = *patch.Enabled
	}
	if patch.TokenLimit != nil {
		if *patch.TokenLimit < 0 {
			return fmt.Errorf("%w: tokenLimit must not be negative", ErrApiKeyInvalid)
		}
		updated.TokenLimit = *patch.TokenLimit
	}
	if patch.CreditLimit != nil {
		if *patch.CreditLimit < 0 {
			return fmt.Errorf("%w: creditLimit must not be negative", ErrApiKeyInvalid)
		}
		updated.CreditLimit = *patch.CreditLimit
	}
	if patch.PromptCachePolicyID != nil {
		policyID := strings.TrimSpace(*patch.PromptCachePolicyID)
		if err := validatePromptCachePolicyBindingLocked(policyID); err != nil {
			return err
		}
		updated.PromptCachePolicyID = policyID
	}

	cfg.ApiKeys[idx] = updated
	if err := saveLocked(); err != nil {
		cfg.ApiKeys[idx] = previous
		return err
	}
	return nil
}

// DeleteApiKey removes the API key entry with the given ID. Returns nil even if
// the ID is unknown (idempotent), matching the existing DeleteAccount style.
func DeleteApiKey(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i, e := range cfg.ApiKeys {
		if e.ID == id {
			state := apiKeyUsageStateForID(id)
			if state == nil {
				return ErrApiKeyNotFound
			}
			state.lifecycle.Lock()
			wasDeleted := state.deleted
			state.deleted = true
			previous := append([]ApiKeyEntry(nil), cfg.ApiKeys...)
			cfg.ApiKeys = append(cfg.ApiKeys[:i], cfg.ApiKeys[i+1:]...)
			if err := saveLocked(); err != nil {
				cfg.ApiKeys = previous
				state.deleted = wasDeleted
				state.lifecycle.Unlock()
				return err
			}
			removeApiKeyUsageStateLocked(id)
			state.lifecycle.Unlock()
			return nil
		}
	}
	return nil
}

// FindApiKeyByValue returns a copy of the entry whose Key matches the given value,
// or nil if no match. O(n) linear scan.
func FindApiKeyByValue(key string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || key == "" {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].Key == key {
			cp := cfg.ApiKeys[i]
			mergeApiKeyUsage(&cp)
			return &cp
		}
	}
	return nil
}

// HasApiKeys returns true when at least one API key entry is configured.
func HasApiKeys() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return len(cfg.ApiKeys) > 0
}

// RecordApiKeyUsage atomically adds usage to an in-memory per-key accumulator.
// Persistence is intentionally deferred to FlushApiKeyUsage so this request-path
// operation never serializes or fsyncs the complete configuration.
func RecordApiKeyUsage(id string, tokens int64, credits float64) error {
	state := apiKeyUsageStateForID(id)
	if state == nil {
		return ErrApiKeyNotFound
	}
	state.lifecycle.RLock()
	defer state.lifecycle.RUnlock()
	if state.deleted {
		return ErrApiKeyNotFound
	}
	if tokens > 0 {
		state.tokens.Add(tokens)
	}
	if credits > 0 {
		atomicAddFloat64(&state.credits, credits)
	}
	state.requests.Add(1)
	atomicMaxInt64(&state.lastUsed, time.Now().Unix())
	return nil
}

// ResetApiKeyUsage clears TokensUsed/CreditsUsed/RequestsCount for the entry.
// LastUsedAt is preserved so operators can still see when the key was last used.
func ResetApiKeyUsage(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			state := apiKeyUsageStateForID(id)
			if state == nil {
				return ErrApiKeyNotFound
			}
			state.lifecycle.Lock()
			previous := cfg.ApiKeys[i]
			pendingTokens := state.tokens.Swap(0)
			pendingCredits := state.credits.Swap(0)
			pendingRequests := state.requests.Swap(0)
			pendingLastUsed := state.lastUsed.Swap(0)
			cfg.ApiKeys[i].TokensUsed = 0
			cfg.ApiKeys[i].CreditsUsed = 0
			cfg.ApiKeys[i].RequestsCount = 0
			if pendingLastUsed > cfg.ApiKeys[i].LastUsedAt {
				cfg.ApiKeys[i].LastUsedAt = pendingLastUsed
			}
			state.lifecycle.Unlock()
			if err := saveLocked(); err != nil {
				cfg.ApiKeys[i] = previous
				state.lifecycle.Lock()
				state.tokens.Add(pendingTokens)
				atomicAddFloat64(&state.credits, math.Float64frombits(pendingCredits))
				state.requests.Add(pendingRequests)
				atomicMaxInt64(&state.lastUsed, pendingLastUsed)
				state.lifecycle.Unlock()
				return err
			}
			return nil
		}
	}
	return ErrApiKeyNotFound
}

// GenerateApiKeyValue returns a new random 32-byte hex API key prefixed with "sk-".
func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

// MaskApiKey produces a display-friendly masked version: keeps first 6 and last 4
// characters, replaces the middle with "****". Returns "" for empty input and
// the original string if it's too short to mask meaningfully.
func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return key
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// ApiKeyOverLimit returns (overToken, overCredit) for the entry. Limits with value 0
// are ignored. The function does not lock; callers should pass a copied entry.
func ApiKeyOverLimit(e ApiKeyEntry) (overToken bool, overCredit bool) {
	if e.TokenLimit > 0 && e.TokensUsed >= e.TokenLimit {
		overToken = true
	}
	if e.CreditLimit > 0 && e.CreditsUsed >= e.CreditLimit {
		overCredit = true
	}
	return
}
