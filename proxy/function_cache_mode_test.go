package proxy

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
)

func TestFunctionCacheModeSimulatesUsageWithoutNativeIsolation(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	mode := config.CacheModeFunctionSimulation
	hitRate, maxRead := 100, 85
	functionCache := config.FunctionCacheConfig{
		Enabled:             true,
		SimulatedData:       `{"name":"lookup"}`,
		SimulatedTokens:     200,
		TTLSeconds:          300,
		HitRatePercent:      &hitRate,
		MaxCacheReadPercent: &maxRead,
	}
	if err := config.UpdateCachePolicy(&mode, &functionCache); err != nil {
		t.Fatalf("UpdateCachePolicy: %v", err)
	}
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 256)
	req := &ClaudeRequest{Model: "claude-sonnet", Messages: []ClaudeMessage{{Role: "user", Content: "hello"}}}
	profile := tracker.BuildClaudeProfile(req, 1000)
	if profile == nil || !profile.SimulatedFunctionCache {
		t.Fatalf("expected simulated profile, got %#v", profile)
	}
	first := tracker.Compute("account-a", profile)
	if first.CacheCreationInputTokens != 200 || first.CacheReadInputTokens != 0 {
		t.Fatalf("first usage = %+v", first)
	}
	tracker.Update("account-a", profile)
	second := tracker.Compute("account-b", profile)
	if second.CacheReadInputTokens != 200 || second.CacheCreationInputTokens != 0 {
		t.Fatalf("cross-account simulated hit = %+v", second)
	}
	stats := tracker.Stats()
	if stats.FunctionEntries != 1 || stats.Entries != 0 {
		t.Fatalf("simulation must remain separate from native persistent LRU: %+v", stats)
	}
}

func TestCacheModeSwitchesBackToNativeBehavior(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	mode := config.CacheModeNative
	if err := config.UpdateCachePolicy(&mode, nil); err != nil {
		t.Fatalf("UpdateCachePolicy: %v", err)
	}
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 256)
	req := &ClaudeRequest{Model: "claude-sonnet", System: "stable", Messages: []ClaudeMessage{{Role: "user", Content: "hello"}}}
	if profile := tracker.BuildClaudeProfile(req, 2000); profile != nil {
		t.Fatalf("native mode should retain isolation requirement, got %#v", profile)
	}
	profile := tracker.BuildClaudeProfile(req, 2000, "tenant-a")
	if profile == nil || profile.SimulatedFunctionCache {
		t.Fatalf("expected native profile, got %#v", profile)
	}
}

func TestFunctionCacheUsesRawBodyFingerprintWithoutChangingNativeIsolation(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	hitRate, maxRead := 100, 85
	mode := config.CacheModeFunctionSimulation
	functionCache := config.FunctionCacheConfig{
		Enabled:             true,
		SimulatedTokens:     200,
		TTLSeconds:          300,
		HitRatePercent:      &hitRate,
		MaxCacheReadPercent: &maxRead,
	}
	if err := config.UpdateCachePolicy(&mode, &functionCache); err != nil {
		t.Fatalf("UpdateCachePolicy: %v", err)
	}

	rawA := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"hello"}],"vendor_extension":"a"}`)
	rawB := []byte(`{"messages":[{"content":"hello","role":"user"}],"vendor_extension":"b","model":"claude-sonnet"}`)
	var req ClaudeRequest
	if err := json.Unmarshal(rawA, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 256)
	profileA := tracker.BuildClaudeProfileForRequest(&req, 1000, "tenant-a", functionCacheRequestBodyFingerprint(rawA))
	profileB := tracker.BuildClaudeProfileForRequest(&req, 1000, "tenant-a", functionCacheRequestBodyFingerprint(rawB))
	profileOtherTenant := tracker.BuildClaudeProfileForRequest(&req, 1000, "tenant-b", functionCacheRequestBodyFingerprint(rawA))
	if profileA == nil || profileB == nil || profileOtherTenant == nil {
		t.Fatal("expected function cache profiles")
	}
	if profileA.Breakpoints[0].Fingerprint == profileB.Breakpoints[0].Fingerprint {
		t.Fatal("raw request extensions must participate in function-cache fingerprinting")
	}
	if profileA.Breakpoints[0].Fingerprint != profileOtherTenant.Breakpoints[0].Fingerprint {
		t.Fatal("function-cache fingerprint must remain shared across tenants")
	}

	mode = config.CacheModeNative
	if err := config.UpdateCachePolicy(&mode, nil); err != nil {
		t.Fatalf("UpdateCachePolicy(native): %v", err)
	}
	nativeA := tracker.BuildClaudeProfileForRequest(&req, 1000, "tenant-a", functionCacheRequestBodyFingerprint(rawA))
	nativeB := tracker.BuildClaudeProfileForRequest(&req, 1000, "tenant-b", functionCacheRequestBodyFingerprint(rawA))
	if nativeA == nil || nativeB == nil {
		t.Fatal("expected native cache profiles")
	}
	if nativeA.Breakpoints[0].Fingerprint == nativeB.Breakpoints[0].Fingerprint {
		t.Fatal("native cache must retain tenant isolation")
	}
}
