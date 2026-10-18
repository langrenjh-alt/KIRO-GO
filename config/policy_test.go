package config

import (
	"path/filepath"
	"testing"
)

func TestDispatchPolicyNormalizationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rate := RateLimitConfig{
		Mode:                   RateLimitModeRetrySame,
		RetrySameAccount:       99,
		DisableSeconds:         -1,
		SwitchAccountRounds:    999,
		RetryDelayMs:           250,
		RetryBackoffMultiplier: 9,
		RetryMaxDelayMs:        10,
	}
	scheduling := SchedulingConfig{Mode: SchedulingModeLeastRequests}
	priority := AccountPriorityConfig{Enabled: true, Rules: []AccountPriorityRouteRule{{
		ID:            "route",
		Enabled:       true,
		Models:        []string{"Claude-Sonnet-4", "claude-sonnet-4"},
		AccountLevels: []string{"FREE", "auto", "pro-plus"},
	}}}
	if err := UpdateDispatchPolicies(&rate, &scheduling, &priority); err != nil {
		t.Fatalf("UpdateDispatchPolicies: %v", err)
	}
	gotRate := GetRateLimitConfig()
	if gotRate.Mode != RateLimitModeRetrySame || gotRate.RetrySameAccount != 20 || gotRate.DisableSeconds != 60 || gotRate.SwitchAccountRounds != 100 || gotRate.RetryBackoffMultiplier != 5 || gotRate.RetryMaxDelayMs != 250 {
		t.Fatalf("unexpected normalized rate policy: %+v", gotRate)
	}
	gotPriority := GetAccountPriorityConfig()
	if len(gotPriority.Rules) != 1 || len(gotPriority.Rules[0].Models) != 1 || len(gotPriority.Rules[0].AccountLevels) != 2 {
		t.Fatalf("unexpected normalized priority policy: %+v", gotPriority)
	}
	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := GetSchedulingConfig().Mode; got != SchedulingModeLeastRequests {
		t.Fatalf("scheduling mode did not persist: %q", got)
	}
}

func TestCacheModeAndFunctionPolicy(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mode := CacheModeFunctionSimulation
	hitRate, maxRead := -5, 200
	functionCache := FunctionCacheConfig{
		Enabled:               true,
		SimulatedData:         "stable",
		SimulatedTokens:       2_000_000,
		TTLSeconds:            100_000,
		TTLJitterPercent:      200,
		TokenJitterPercent:    -1,
		HitRatePercent:        &hitRate,
		MaxCacheReadPercent:   &maxRead,
		MinTokens:             -1,
		InputFingerprintChars: 99_999,
	}
	if err := UpdateCachePolicy(&mode, &functionCache, nil); err != nil {
		t.Fatalf("UpdateCachePolicy: %v", err)
	}
	if got := GetCacheMode(); got != CacheModeFunctionSimulation {
		t.Fatalf("cache mode = %q", got)
	}
	got := GetFunctionCacheConfig()
	if !got.Enabled || got.SimulatedTokens != 1_000_000 || got.TTLSeconds != 86_400 || got.TTLJitterPercent != 100 || got.TokenJitterPercent != 0 || *got.HitRatePercent != 0 || *got.MaxCacheReadPercent != 100 || got.MinTokens != 0 || got.InputFingerprintChars != 12_000 {
		t.Fatalf("unexpected normalized function cache: %+v", got)
	}
}

func TestFixedRatioCachePolicyPersistsAndClamps(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mode := CacheModeFixedRatio
	ratio := FixedRatioCacheConfig{CacheReadPercent: 180, CacheCreationPercent: -3}
	if err := UpdateCachePolicy(&mode, nil, &ratio); err != nil {
		t.Fatalf("UpdateCachePolicy: %v", err)
	}
	if got := GetCacheMode(); got != CacheModeFixedRatio {
		t.Fatalf("cache mode = %q", got)
	}
	got := GetFixedRatioCacheConfig()
	if got.CacheReadPercent != 100 || got.CacheCreationPercent != 0 {
		t.Fatalf("fixed ratio = %+v", got)
	}
	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if GetCacheMode() != CacheModeFixedRatio || GetFixedRatioCacheConfig().CacheReadPercent != 100 {
		t.Fatal("fixed ratio did not persist")
	}
	created, err := AddPromptCachePolicy(PromptCachePolicy{
		Name:            "fixed",
		CacheMode:       CacheModeFixedRatio,
		FixedRatioCache: FixedRatioCacheConfig{CacheReadPercent: 80, CacheCreationPercent: 15},
	})
	if err != nil {
		t.Fatalf("AddPromptCachePolicy: %v", err)
	}
	if created.CacheMode != CacheModeFixedRatio || created.FixedRatioCache.CacheReadPercent != 80 || created.FixedRatioCache.CacheCreationPercent != 15 {
		t.Fatalf("policy = %+v", created)
	}
}

func TestAccountAllowedForModelUsesManualAndInferredLevels(t *testing.T) {
	policy := AccountPriorityConfig{Enabled: true, Rules: []AccountPriorityRouteRule{{
		Enabled: true, Models: []string{"claude-opus"}, AccountLevels: []string{AccountLevelPower},
	}}}
	if !AccountAllowedForModel("claude-opus", Account{AccountLevel: AccountLevelPower}, policy) {
		t.Fatal("manual power account should be allowed")
	}
	if AccountAllowedForModel("claude-opus", Account{SubscriptionTitle: "Kiro Pro"}, policy) {
		t.Fatal("inferred pro account should not match power-only route")
	}
	if !AccountAllowedForModel("other", Account{}, policy) {
		t.Fatal("unmatched models should remain unrestricted")
	}
}
