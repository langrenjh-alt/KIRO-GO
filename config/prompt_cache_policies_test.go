package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPromptCachePolicyCRUDValidationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}

	created, err := AddPromptCachePolicy(PromptCachePolicy{Name: "  Alpha  "})
	if err != nil {
		t.Fatalf("AddPromptCachePolicy: %v", err)
	}
	if created.ID == "" || created.Name != "Alpha" || created.Revision != 1 {
		t.Fatalf("unexpected created identity: %+v", created)
	}
	if created.CacheMode != CacheModeNative || created.PromptCacheMaxRatio != 0.85 ||
		created.PromptCacheMaxEntries != defaultPromptCacheMaxEntries ||
		created.PromptCacheTTLSeconds != DefaultPromptCacheTTLSeconds {
		t.Fatalf("zero-valued settings did not receive defaults: %+v", created)
	}
	if created.CreatedAt == 0 || created.UpdatedAt == 0 {
		t.Fatalf("timestamps were not populated: %+v", created)
	}
	if created.FunctionCache.ScopeMode != FunctionCacheScopeBodyFingerprint || created.FunctionCache.TTLSeconds != 300 {
		t.Fatalf("function cache was not normalized: %+v", created.FunctionCache)
	}

	if _, err := AddPromptCachePolicy(PromptCachePolicy{Name: "aLPHa"}); err == nil {
		t.Fatal("expected case-insensitive duplicate name to fail")
	}
	invalid := []PromptCachePolicy{
		{Name: " "},
		{Name: "ratio", PromptCacheMaxRatio: MinPromptCacheRatio / 2},
		{Name: "entries", PromptCacheMaxEntries: MinPromptCacheEntries - 1},
		{Name: "ttl", PromptCacheTTLSeconds: 600},
		{Name: "mode", CacheMode: "unknown"},
	}
	for _, policy := range invalid {
		if _, err := AddPromptCachePolicy(policy); !errors.Is(err, ErrPromptCachePolicyInvalid) {
			t.Fatalf("invalid policy error = %v, want ErrPromptCachePolicyInvalid: %+v", err, policy)
		}
	}

	hitRate, maxRead := -10, 150
	updated, err := UpdatePromptCachePolicy(created.ID, PromptCachePolicy{
		Name:                  "Renamed",
		CacheMode:             CacheModeFunctionSimulation,
		PromptCacheMaxRatio:   0.93,
		PromptCacheMaxEntries: 4096,
		PromptCacheTTLSeconds: ExtendedPromptCacheTTLSeconds,
		FunctionCache: FunctionCacheConfig{
			Enabled:             true,
			TTLSeconds:          100000,
			HitRatePercent:      &hitRate,
			MaxCacheReadPercent: &maxRead,
		},
	})
	if err != nil {
		t.Fatalf("UpdatePromptCachePolicy: %v", err)
	}
	if updated.ID != created.ID || updated.CreatedAt != created.CreatedAt || updated.Name != "Renamed" || updated.Revision != 2 {
		t.Fatalf("immutable/update fields are wrong: %+v", updated)
	}
	if updated.CacheMode != CacheModeFunctionSimulation || updated.PromptCacheMaxRatio != 0.93 ||
		updated.PromptCacheMaxEntries != 4096 || updated.PromptCacheTTLSeconds != ExtendedPromptCacheTTLSeconds {
		t.Fatalf("settings were not updated: %+v", updated)
	}
	if *updated.FunctionCache.HitRatePercent != 0 || *updated.FunctionCache.MaxCacheReadPercent != 100 || updated.FunctionCache.TTLSeconds != 86400 {
		t.Fatalf("function settings were not normalized: %+v", updated.FunctionCache)
	}

	listed := ListPromptCachePolicies()
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("unexpected policy list: %+v", listed)
	}
	*listed[0].FunctionCache.HitRatePercent = 77
	if got := GetPromptCachePolicy(created.ID); got == nil || *got.FunctionCache.HitRatePercent != 0 {
		t.Fatal("list must return a deep copy of pointer-backed function settings")
	}

	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	persisted := GetPromptCachePolicy(created.ID)
	if persisted == nil || persisted.Name != "Renamed" || persisted.PromptCacheMaxEntries != 4096 || persisted.Revision != 2 {
		t.Fatalf("policy did not persist: %+v", persisted)
	}
	if err := DeletePromptCachePolicy(created.ID); err != nil {
		t.Fatalf("DeletePromptCachePolicy: %v", err)
	}
	if got := GetPromptCachePolicy(created.ID); got != nil {
		t.Fatalf("deleted policy still exists: %+v", got)
	}
	if err := DeletePromptCachePolicy(created.ID); !errors.Is(err, ErrPromptCachePolicyNotFound) {
		t.Fatalf("second delete error = %v, want ErrPromptCachePolicyNotFound", err)
	}
}

func TestPromptCachePolicyAPIKeyBindingResolutionAndDeleteProtection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ratio, entries, ttl := 0.42, 2048, ExtendedPromptCacheTTLSeconds
	if err := UpdatePromptCacheSettings(&ratio, &entries, &ttl); err != nil {
		t.Fatalf("seed global fallback: %v", err)
	}

	policy, err := AddPromptCachePolicy(PromptCachePolicy{
		Name:                  "Bound",
		PromptCacheMaxRatio:   0.91,
		PromptCacheMaxEntries: 8192,
		PromptCacheTTLSeconds: DefaultPromptCacheTTLSeconds,
	})
	if err != nil {
		t.Fatalf("add policy: %v", err)
	}
	bound, err := AddApiKey(ApiKeyEntry{
		Name:                "bound-key",
		Key:                 "sk-bound-policy",
		Enabled:             true,
		PromptCachePolicyID: policy.ID,
	})
	if err != nil {
		t.Fatalf("add bound API key: %v", err)
	}
	unbound, err := AddApiKey(ApiKeyEntry{Name: "global-key", Key: "sk-global-policy", Enabled: true})
	if err != nil {
		t.Fatalf("add unbound API key: %v", err)
	}
	if err := Load(); err != nil {
		t.Fatalf("reload policies and bindings: %v", err)
	}

	resolved := ResolvePromptCachePolicyForAPIKey(bound.ID)
	if resolved.ID != policy.ID || resolved.PromptCacheMaxRatio != 0.91 || resolved.PromptCacheMaxEntries != 8192 {
		t.Fatalf("bound resolution returned wrong policy: %+v", resolved)
	}
	fallback := ResolvePromptCachePolicyForAPIKey(unbound.ID)
	if fallback.ID != "" || fallback.PromptCacheMaxRatio != ratio || fallback.PromptCacheMaxEntries != entries || fallback.PromptCacheTTLSeconds != ttl {
		t.Fatalf("unbound resolution returned wrong global fallback: %+v", fallback)
	}
	if missing := ResolvePromptCachePolicyForAPIKey("missing-key"); missing.ID != "" || missing.PromptCacheMaxRatio != ratio {
		t.Fatalf("missing key should use global fallback: %+v", missing)
	}

	if _, err := AddApiKey(ApiKeyEntry{Key: "sk-unknown-policy", PromptCachePolicyID: "missing"}); !errors.Is(err, ErrPromptCachePolicyNotFound) {
		t.Fatalf("unknown binding add error = %v, want ErrPromptCachePolicyNotFound", err)
	}
	patch := *GetApiKeyEntry(bound.ID)
	patch.PromptCachePolicyID = "missing"
	if err := UpdateApiKey(bound.ID, patch); !errors.Is(err, ErrPromptCachePolicyNotFound) {
		t.Fatalf("unknown binding update error = %v, want ErrPromptCachePolicyNotFound", err)
	}
	if got := GetApiKeyEntry(bound.ID); got == nil || got.PromptCachePolicyID != policy.ID {
		t.Fatalf("failed binding update changed API key: %+v", got)
	}

	if err := DeletePromptCachePolicy(policy.ID); !errors.Is(err, ErrPromptCachePolicyInUse) {
		t.Fatalf("referenced delete error = %v, want ErrPromptCachePolicyInUse", err)
	}
	patch = *GetApiKeyEntry(bound.ID)
	patch.PromptCachePolicyID = ""
	if err := UpdateApiKey(bound.ID, patch); err != nil {
		t.Fatalf("unbind API key: %v", err)
	}
	if resolved := ResolvePromptCachePolicyForAPIKey(bound.ID); resolved.ID != "" || resolved.PromptCacheMaxRatio != ratio {
		t.Fatalf("explicit empty binding did not restore fallback: %+v", resolved)
	}
	if err := DeletePromptCachePolicy(policy.ID); err != nil {
		t.Fatalf("delete unreferenced policy: %v", err)
	}

	// A manually edited legacy config can contain a dangling binding. Resolution
	// remains fail-open to the global cache settings without rewriting the file.
	cfgLock.Lock()
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == unbound.ID {
			cfg.ApiKeys[i].PromptCachePolicyID = "dangling"
		}
	}
	cfgLock.Unlock()
	if resolved := ResolvePromptCachePolicyForAPIKey(unbound.ID); resolved.ID != "" || resolved.PromptCacheMaxEntries != entries {
		t.Fatalf("dangling binding did not use fallback: %+v", resolved)
	}
}

func TestPromptCachePolicyDoesNotMigrateLegacyGlobalSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"promptCacheMaxRatio":0.73,"promptCacheMaxEntries":4096,"promptCacheTTLSeconds":3600,"cacheMode":"native"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if policies := ListPromptCachePolicies(); len(policies) != 0 {
		t.Fatalf("legacy globals must not be auto-migrated: %+v", policies)
	}
	resolved := ResolvePromptCachePolicyForAPIKey("")
	if resolved.ID != "" || resolved.PromptCacheMaxRatio != 0.73 || resolved.PromptCacheMaxEntries != 4096 || resolved.PromptCacheTTLSeconds != 3600 {
		t.Fatalf("legacy global fallback mismatch: %+v", resolved)
	}
}

func TestLoadNormalizesPromptCachePolicyBindingsAndDuplicateAPIKeyValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{
  "promptCachePolicies": [
    {"id":" alpha ","name":" Primary ","cacheMode":"native"},
    {"id":"alpha","name":" Duplicate ID ","cacheMode":"native"},
    {"id":"   ","name":" Generated ID ","cacheMode":"native"},
    {"id":" beta ","name":" Beta ","cacheMode":"native"}
  ],
  "apiKeys": [
    {"id":" first ","name":" First ","key":" shared-secret ","enabled":true,"promptCachePolicyId":" alpha "},
    {"id":"second","name":" Duplicate key ","key":"shared-secret","enabled":true,"promptCachePolicyId":"alpha"},
    {"id":"third","name":" Trimmed binding ","key":" third-secret ","enabled":true,"promptCachePolicyId":" beta "},
    {"id":"fourth","name":" Dangling binding ","key":"fourth-secret","enabled":true,"promptCachePolicyId":" missing "}
  ]
}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}

	policies := ListPromptCachePolicies()
	if len(policies) != 4 {
		t.Fatalf("policies=%d, want 4", len(policies))
	}
	if policies[0].ID != "alpha" || policies[0].Name != "Primary" {
		t.Fatalf("first policy was not trimmed: %+v", policies[0])
	}
	if policies[1].ID == "" || policies[1].ID == "alpha" || policies[1].Name != "Duplicate ID" {
		t.Fatalf("duplicate policy ID was not repaired: %+v", policies[1])
	}
	if policies[2].ID == "" || policies[2].ID == policies[1].ID || policies[2].Name != "Generated ID" {
		t.Fatalf("blank policy ID was not repaired: %+v", policies[2])
	}
	if policies[3].ID != "beta" || policies[3].Name != "Beta" {
		t.Fatalf("unique policy identity was not trimmed: %+v", policies[3])
	}

	entries := ListApiKeys()
	if len(entries) != 4 {
		t.Fatalf("api keys=%d, want 4", len(entries))
	}
	if entries[0].ID != "first" || entries[0].Name != "First" || entries[0].Key != "shared-secret" || entries[0].PromptCachePolicyID != "alpha" || !entries[0].Enabled {
		t.Fatalf("first API key was not normalized: %+v", entries[0])
	}
	if entries[1].Key != "shared-secret" || entries[1].Enabled || entries[1].PromptCachePolicyID != "alpha" {
		t.Fatalf("later duplicate API key was not disabled: %+v", entries[1])
	}
	if entries[2].Key != "third-secret" || entries[2].PromptCachePolicyID != "beta" {
		t.Fatalf("trimmed binding did not resolve to beta: %+v", entries[2])
	}
	if entries[3].PromptCachePolicyID != "missing" {
		t.Fatalf("dangling binding was not preserved: %+v", entries[3])
	}
	if found := FindApiKeyByValue("shared-secret"); found == nil || found.ID != "first" {
		t.Fatalf("duplicate value resolved to wrong owner: %+v", found)
	}

	persistedData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var persisted Config
	if err := json.Unmarshal(persistedData, &persisted); err != nil {
		t.Fatalf("decode persisted config: %v", err)
	}
	if persisted.PromptCachePolicies[1].ID != policies[1].ID || persisted.PromptCachePolicies[2].ID != policies[2].ID || persisted.ApiKeys[3].PromptCachePolicyID != "missing" || persisted.ApiKeys[1].Enabled {
		t.Fatalf("normalization was not persisted: policies=%+v apiKeys=%+v", persisted.PromptCachePolicies, persisted.ApiKeys)
	}

	generatedDuplicateID := policies[1].ID
	generatedBlankID := policies[2].ID
	if err := Init(path); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	policies = ListPromptCachePolicies()
	if policies[1].ID != generatedDuplicateID || policies[2].ID != generatedBlankID {
		t.Fatalf("normalization was not idempotent: %+v", policies)
	}
}

func TestDanglingPromptCacheBindingRecoversWhenPolicyReturns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{
  "apiKeys": [{"id":"key-1","key":"secret-1","enabled":true,"promptCachePolicyId":"policy-restored"}],
  "promptCacheMaxRatio": 0.42
}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := GetApiKeyEntry("key-1"); got == nil || got.PromptCachePolicyID != "policy-restored" {
		t.Fatalf("Init cleared the dangling binding: %+v", got)
	}
	if resolved := ResolvePromptCachePolicyForAPIKey("key-1"); resolved.ID != "" || resolved.PromptCacheMaxRatio != 0.42 {
		t.Fatalf("dangling binding should temporarily use global settings: %+v", resolved)
	}

	if _, err := AddPromptCachePolicy(PromptCachePolicy{ID: "policy-restored", Name: "Restored"}); err != nil {
		t.Fatalf("restore policy: %v", err)
	}
	if resolved := ResolvePromptCachePolicyForAPIKey("key-1"); resolved.ID != "policy-restored" {
		t.Fatalf("binding did not recover after policy restore: %+v", resolved)
	}
}

func TestEmptyAPIKeyIDAlwaysResolvesGlobalPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	policy, err := AddPromptCachePolicy(PromptCachePolicy{
		Name:                  "must-not-match-anonymous",
		PromptCacheMaxRatio:   0.99,
		PromptCacheMaxEntries: 512,
		PromptCacheTTLSeconds: ExtendedPromptCacheTTLSeconds,
	})
	if err != nil {
		t.Fatalf("add policy: %v", err)
	}

	// Simulate a malformed hand-edited legacy entry. Anonymous requests use an
	// empty context ID and must never inherit that entry's named policy.
	cfgLock.Lock()
	cfg.ApiKeys = append(cfg.ApiKeys, ApiKeyEntry{
		ID:                  "",
		Key:                 "sk-malformed-empty-id",
		Enabled:             true,
		PromptCachePolicyID: policy.ID,
	})
	cfgLock.Unlock()

	resolved := ResolvePromptCachePolicyForAPIKey("")
	if resolved.ID != "" || resolved.PromptCacheMaxRatio == policy.PromptCacheMaxRatio || resolved.PromptCacheMaxEntries == policy.PromptCacheMaxEntries {
		t.Fatalf("anonymous request resolved named policy: %+v", resolved)
	}
}

func TestPromptCachePolicyMutationsRollBackOnSaveFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	created, err := AddPromptCachePolicy(PromptCachePolicy{Name: "stable"})
	if err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	apiKey, err := AddApiKey(ApiKeyEntry{Name: "stable-key", Key: "sk-stable-key", Enabled: true})
	if err != nil {
		t.Fatalf("seed API key: %v", err)
	}

	cfgLock.Lock()
	originalPath := cfgPath
	cfgPath = t.TempDir() // an atomic rename cannot replace a directory
	cfgLock.Unlock()
	t.Cleanup(func() {
		cfgLock.Lock()
		cfgPath = originalPath
		cfgLock.Unlock()
	})
	if _, err := UpdatePromptCachePolicy(created.ID, PromptCachePolicy{Name: "changed"}); err == nil {
		t.Fatal("expected policy update persistence failure")
	}
	if got := GetPromptCachePolicy(created.ID); got == nil || got.Name != "stable" {
		t.Fatalf("failed update was not rolled back: %+v", got)
	}
	if _, err := AddPromptCachePolicy(PromptCachePolicy{Name: "not-saved"}); err == nil {
		t.Fatal("expected policy add persistence failure")
	}
	if got := ListPromptCachePolicies(); len(got) != 1 {
		t.Fatalf("failed add was not rolled back: %+v", got)
	}
	apiKeyPatch := *GetApiKeyEntry(apiKey.ID)
	apiKeyPatch.Name = "changed-key"
	apiKeyPatch.PromptCachePolicyID = created.ID
	if err := UpdateApiKey(apiKey.ID, apiKeyPatch); err == nil {
		t.Fatal("expected API key binding persistence failure")
	}
	if got := GetApiKeyEntry(apiKey.ID); got == nil || got.Name != "stable-key" || got.PromptCachePolicyID != "" {
		t.Fatalf("failed API key binding was not rolled back: %+v", got)
	}
	if err := DeletePromptCachePolicy(created.ID); err == nil {
		t.Fatal("expected policy delete persistence failure")
	}
	if got := GetPromptCachePolicy(created.ID); got == nil || got.Name != "stable" {
		t.Fatalf("failed delete was not rolled back: %+v", got)
	}
	if err := DeleteApiKey(apiKey.ID); err == nil {
		t.Fatal("expected API key delete persistence failure")
	}
	if got := GetApiKeyEntry(apiKey.ID); got == nil || got.Name != "stable-key" {
		t.Fatalf("failed API key delete was not rolled back: %+v", got)
	}
	cfgLock.Lock()
	cfgPath = originalPath
	cfgLock.Unlock()
}
