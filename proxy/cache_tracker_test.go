package proxy

import (
	"crypto/sha256"
	"kiro-go/config"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPromptCacheTrackerComputeAndUpdate(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 120, "test-session")
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}

	first := tracker.Compute("acct-1", profile)
	if first.CacheCreationInputTokens <= 0 {
		t.Fatalf("expected first request to create cache tokens, got %+v", first)
	}
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected first request to have zero cache reads, got %+v", first)
	}

	tracker.Update("acct-1", profile)
	second := tracker.Compute("acct-1", profile)
	if second.CacheReadInputTokens <= 0 {
		t.Fatalf("expected repeated request to read cache tokens, got %+v", second)
	}
	if second.CacheCreationInputTokens != 0 {
		t.Fatalf("expected repeated request to avoid cache creation, got %+v", second)
	}
}

func TestPromptCacheDefaultTTLHotReloadAffectsOnlyNewProfiles(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	longSystem := strings.Repeat("stable prompt cache prefix ", 700)
	request := func(explicitTTL string) *ClaudeRequest {
		cacheControl := map[string]interface{}{"type": "ephemeral"}
		if explicitTTL != "" {
			cacheControl["ttl"] = explicitTTL
		}
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{map[string]interface{}{
				"type":          "text",
				"text":          longSystem,
				"cache_control": cacheControl,
			}},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
		}
	}

	profile5m := tracker.BuildClaudeProfile(request(""), 4096, "ttl-session")
	if profile5m == nil || len(profile5m.Breakpoints) == 0 {
		t.Fatal("expected a cache profile using the default TTL")
	}
	for _, breakpoint := range profile5m.Breakpoints {
		if breakpoint.TTL != 5*time.Minute {
			t.Fatalf("initial breakpoint TTL=%v, want 5m", breakpoint.TTL)
		}
	}
	tracker.Update("account", profile5m)
	firstFingerprint := profile5m.Breakpoints[0].Fingerprint
	tracker.mu.Lock()
	storedTTL := tracker.entries[firstFingerprint].TTL
	storedExpiry := tracker.entries[firstFingerprint].ExpiresAt
	tracker.mu.Unlock()

	tracker.SetDefaultTTL(time.Hour)
	if got := tracker.DefaultTTL(); got != time.Hour {
		t.Fatalf("runtime default TTL=%v, want 1h", got)
	}
	tracker.mu.Lock()
	entry := tracker.entries[firstFingerprint]
	if entry.TTL != storedTTL || !entry.ExpiresAt.Equal(storedExpiry) {
		tracker.mu.Unlock()
		t.Fatalf("hot reload changed existing entry: ttl=%v expiry=%v", entry.TTL, entry.ExpiresAt)
	}
	tracker.mu.Unlock()

	profile1h := tracker.BuildClaudeProfile(request(""), 4096, "ttl-session")
	for _, breakpoint := range profile1h.Breakpoints {
		if breakpoint.TTL != time.Hour {
			t.Fatalf("reloaded default breakpoint TTL=%v, want 1h", breakpoint.TTL)
		}
	}
	if profile1h.Breakpoints[0].Fingerprint == profile5m.Breakpoints[0].Fingerprint {
		t.Fatal("5m and 1h cache policies must not share a fingerprint")
	}
	if cold := tracker.Compute("account", profile1h); cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens == 0 {
		t.Fatalf("1h profile reused the existing 5m entry: %+v", cold)
	}
	tracker.Update("account", profile1h)
	if hot := tracker.Compute("account", profile1h); hot.CacheReadInputTokens == 0 {
		t.Fatalf("1h profile did not hit its own entry: %+v", hot)
	}
	oneHourFingerprint := profile1h.Breakpoints[0].Fingerprint
	tracker.mu.Lock()
	oneHourEntry := tracker.entries[oneHourFingerprint]
	oneHourTTL := oneHourEntry.TTL
	oneHourExpiry := oneHourEntry.ExpiresAt
	tracker.mu.Unlock()

	// A request that started before the hot reload may finish afterward. Its
	// 5m profile must not overwrite the newer 1h cache entry.
	tracker.Update("account", profile5m)
	tracker.mu.Lock()
	oneHourEntry = tracker.entries[oneHourFingerprint]
	if oneHourEntry.TTL != oneHourTTL || !oneHourEntry.ExpiresAt.Equal(oneHourExpiry) {
		tracker.mu.Unlock()
		t.Fatalf("stale 5m profile overwrote 1h entry: ttl=%v expiry=%v", oneHourEntry.TTL, oneHourEntry.ExpiresAt)
	}
	tracker.mu.Unlock()

	// Explicit request TTLs remain authoritative over the Admin default.
	explicit5m := tracker.BuildClaudeProfile(request("5m"), 4096, "ttl-session")
	if got := explicit5m.Breakpoints[0].TTL; got != 5*time.Minute {
		t.Fatalf("explicit 5m TTL=%v, want 5m", got)
	}
	if explicit5m.Breakpoints[0].Fingerprint != profile5m.Breakpoints[0].Fingerprint {
		t.Fatal("explicit and default 5m policies produced different fingerprints")
	}
	tracker.SetDefaultTTL(5 * time.Minute)
	explicit1h := tracker.BuildClaudeProfile(request("1h"), 4096, "ttl-session")
	if got := explicit1h.Breakpoints[0].TTL; got != time.Hour {
		t.Fatalf("explicit 1h TTL=%v, want 1h", got)
	}
	if explicit1h.Breakpoints[0].Fingerprint != profile1h.Breakpoints[0].Fingerprint {
		t.Fatal("explicit and default 1h policies produced different fingerprints")
	}
}

func TestPromptCacheExplicitInvalidTTLDoesNotUseAdminDefault(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	tracker.SetDefaultTTL(time.Hour)
	request := func(ttl interface{}, includeTTL bool) *ClaudeRequest {
		cacheControl := map[string]interface{}{"type": "ephemeral"}
		if includeTTL {
			cacheControl["ttl"] = ttl
		}
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{map[string]interface{}{
				"type":          "text",
				"text":          strings.Repeat("stable invalid ttl prefix ", 700),
				"cache_control": cacheControl,
			}},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
		}
	}

	if profile := tracker.BuildClaudeProfile(request(nil, false), 4096, "ttl-session"); profile == nil || profile.Breakpoints[0].TTL != time.Hour {
		t.Fatalf("omitted TTL did not use the 1h Admin default: %#v", profile)
	}
	for name, tc := range map[string]struct {
		value interface{}
		want  time.Duration
	}{
		"numeric-5m": {value: 300, want: 5 * time.Minute},
		"seconds-1h": {value: "3600s", want: time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			profile := tracker.BuildClaudeProfile(request(tc.value, true), 4096, "ttl-session")
			if profile == nil || profile.Breakpoints[0].TTL != tc.want {
				t.Fatalf("compatible explicit TTL=%v, want %v", profile, tc.want)
			}
		})
	}
	for name, value := range map[string]interface{}{
		"null": nil, "zero": 0, "unsupported-seconds": 600,
		"fractional-seconds": 300.9, "unsupported-duration": "10m", "garbage": "garbage",
	} {
		t.Run(name, func(t *testing.T) {
			if profile := tracker.BuildClaudeProfile(request(value, true), 4096, "ttl-session"); profile != nil {
				t.Fatalf("invalid explicit TTL used the Admin default: %+v", profile.Breakpoints)
			}
		})
	}
}

func TestPromptCacheTTLIdentitySurvivesExplicitToImplicitMarkerDrift(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	build := func(explicitMessageMarker bool) *ClaudeRequest {
		messageBlock := map[string]interface{}{
			"type": "text",
			"text": strings.Repeat("stable multi-turn history ", 500),
		}
		if explicitMessageMarker {
			messageBlock["cache_control"] = map[string]interface{}{"type": "ephemeral", "ttl": "5m"}
		}
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{map[string]interface{}{
				"type":          "text",
				"text":          strings.Repeat("stable system prefix ", 500),
				"cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "5m"},
			}},
			Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{messageBlock}}},
		}
	}

	explicit := tracker.BuildClaudeProfile(build(true), 4096, "ttl-session")
	implicit := tracker.BuildClaudeProfile(build(false), 4096, "ttl-session")
	if explicit == nil || implicit == nil || len(explicit.Breakpoints) < 2 || len(implicit.Breakpoints) < 2 {
		t.Fatalf("expected explicit and implicit history breakpoints: explicit=%#v implicit=%#v", explicit, implicit)
	}
	explicitLast := explicit.Breakpoints[len(explicit.Breakpoints)-1]
	implicitLast := implicit.Breakpoints[len(implicit.Breakpoints)-1]
	if explicitLast.Fingerprint != implicitLast.Fingerprint {
		t.Fatal("moving a 5m marker from explicit to inherited changed cache identity")
	}
	tracker.Update("account", explicit)
	if usage := tracker.Compute("account", implicit); usage.CacheReadInputTokens == 0 || usage.CacheCreationInputTokens != 0 {
		t.Fatalf("implicit history failed to hit explicit history entry: %+v", usage)
	}
}

func TestPromptCacheTTLIdentityIncludesPolicyHistory(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	request := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{map[string]interface{}{
			"type":          "text",
			"text":          strings.Repeat("default ttl system prefix ", 500),
			"cache_control": map[string]interface{}{"type": "ephemeral"},
		}},
		Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{map[string]interface{}{
			"type":          "text",
			"text":          strings.Repeat("explicit five minute tail ", 500),
			"cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "5m"},
		}}}},
	}

	oldPolicy := tracker.BuildClaudeProfile(request, 4096, "ttl-session")
	if oldPolicy == nil || len(oldPolicy.Breakpoints) != 2 {
		t.Fatalf("expected [5m,5m] policy, got %#v", oldPolicy)
	}
	tracker.Update("account", oldPolicy)

	tracker.SetDefaultTTL(time.Hour)
	newPolicy := tracker.BuildClaudeProfile(request, 4096, "ttl-session")
	if newPolicy == nil || len(newPolicy.Breakpoints) != 2 {
		t.Fatalf("expected [1h,5m] policy, got %#v", newPolicy)
	}
	if oldPolicy.Breakpoints[1].Fingerprint == newPolicy.Breakpoints[1].Fingerprint {
		t.Fatal("mixed TTL history shared the full-prefix fingerprint")
	}
	if usage := tracker.Compute("account", newPolicy); usage.CacheReadInputTokens != 0 || usage.CacheCreationInputTokens == 0 {
		t.Fatalf("[1h,5m] policy reused the cached [5m,5m] prefix: %+v", usage)
	}
	tracker.Update("account", newPolicy)
	if usage := tracker.Compute("account", newPolicy); usage.CacheReadInputTokens == 0 {
		t.Fatalf("[1h,5m] policy did not hit its own entry: %+v", usage)
	}
}

func TestPromptCacheInvalidExplicitTTLStopsImplicitInheritance(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	request := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{map[string]interface{}{
			"type":          "text",
			"text":          strings.Repeat("stable valid system marker ", 500),
			"cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "5m"},
		}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: []interface{}{map[string]interface{}{
				"type":          "text",
				"text":          strings.Repeat("invalid marker history ", 300),
				"cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "garbage"},
			}}},
			{Role: "assistant", Content: "plain content after invalid marker"},
		},
	}

	profile := tracker.BuildClaudeProfile(request, 4096, "ttl-session")
	if profile == nil {
		t.Fatal("the valid system breakpoint should remain usable")
	}
	if len(profile.Breakpoints) != 1 {
		t.Fatalf("invalid marker inherited the earlier TTL; breakpoints=%+v", profile.Breakpoints)
	}
	if profile.Breakpoints[0].TTL != 5*time.Minute {
		t.Fatalf("valid system breakpoint TTL=%v, want 5m", profile.Breakpoints[0].TTL)
	}
}

func TestPromptCacheDefaultTTLConcurrentHotReload(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				tracker.SetDefaultTTL(5 * time.Minute)
			} else {
				tracker.SetDefaultTTL(time.Hour)
			}
		}(i)
		go func() {
			defer wg.Done()
			got := tracker.DefaultTTL()
			if got != 5*time.Minute && got != time.Hour {
				t.Errorf("unexpected runtime TTL %v", got)
			}
		}()
	}
	wg.Wait()
}

func TestBuildClaudeUsageMapIncludesCacheFields(t *testing.T) {
	usage := promptCacheUsage{
		CacheCreationInputTokens:   30,
		CacheReadInputTokens:       20,
		CacheCreation5mInputTokens: 10,
		CacheCreation1hInputTokens: 20,
	}

	m := buildClaudeUsageMap(100, 50, usage, true)

	if got := m["input_tokens"]; got != 50 {
		t.Fatalf("expected billed input tokens 50, got %#v", got)
	}
	if got := m["cache_creation_input_tokens"]; got != 30 {
		t.Fatalf("expected cache creation tokens 30, got %#v", got)
	}
	if got := m["cache_read_input_tokens"]; got != 20 {
		t.Fatalf("expected cache read tokens 20, got %#v", got)
	}
	creation, ok := m["cache_creation"].(map[string]int)
	if !ok {
		t.Fatalf("expected typed cache creation map, got %#v", m["cache_creation"])
	}
	if creation["ephemeral_5m_input_tokens"] != 10 || creation["ephemeral_1h_input_tokens"] != 20 {
		t.Fatalf("unexpected ttl breakdown: %#v", creation)
	}
}

// TestPromptCacheStableAcrossBillingHeaderDrift verifies that Claude Code's
// per-request "x-anthropic-billing-header: cc_version=...; cch=...;" system
// block (whose content drifts on every request) does not break cache hits.
// The tracker should ignore that metadata when fingerprinting cached prefixes.
func TestPromptCacheStableAcrossBillingHeaderDrift(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(billingHdr string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": billingHdr,
				},
				map[string]interface{}{
					"type": "text",
					"text": mainSystem,
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	req1 := build("x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;")
	profile1 := tracker.BuildClaudeProfile(req1, 2048, "test-session")
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	first := tracker.Compute("acct-1", profile1)
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected no cache read on first request, got %+v", first)
	}
	tracker.Update("acct-1", profile1)

	req2 := build("x-anthropic-billing-header: cc_version=2.1.87.42; cch=bbbb; padding=xxyyzz;")
	profile2 := tracker.BuildClaudeProfile(req2, 2048, "test-session")
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	second := tracker.Compute("acct-1", profile2)
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read after billing header drift, got %+v", second)
	}
}

func TestPromptCacheStableWhenBillingHeaderAppearsOrDisappears(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(includeBilling bool) *ClaudeRequest {
		system := []interface{}{}
		if includeBilling {
			system = append(system, map[string]interface{}{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;",
			})
		}
		system = append(system, map[string]interface{}{
			"type": "text",
			"text": mainSystem,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		})
		return &ClaudeRequest{
			Model:    "claude-sonnet-4.5",
			System:   system,
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	withBilling := tracker.BuildClaudeProfile(build(true), 2048, "test-session")
	if withBilling == nil {
		t.Fatalf("profile with billing header should be built")
	}
	tracker.Update("acct-1", withBilling)

	withoutBilling := tracker.BuildClaudeProfile(build(false), 2048, "test-session")
	if withoutBilling == nil {
		t.Fatalf("profile without billing header should be built")
	}
	result := tracker.Compute("acct-1", withoutBilling)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read when billing header disappears, got %+v", result)
	}
}

func TestCanonicalCacheValueIgnoresPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 0,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	second := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 1,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	if first != second {
		t.Fatalf("expected position keys to be ignored, got %q vs %q", first, second)
	}
}

func TestCanonicalCacheValuePreservesSemanticPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 1,
		},
	})
	second := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 2,
		},
	})
	if first == second {
		t.Fatalf("expected semantic block_index fields to remain fingerprinted")
	}
}

// TestPromptCacheImplicitBreakpointAtMessageEnd verifies that once any
// explicit cache_control breakpoint has been seen, subsequent message-end
// boundaries act as implicit breakpoints. This allows multi-turn conversations
// to hit earlier stored prefix fingerprints even when the newest messages
// lack explicit cache_control.
func TestPromptCacheImplicitBreakpointAtMessageEnd(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	systemText := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	baseSystem := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": systemText,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		},
	}

	// Round 1: single user message.
	req1 := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   baseSystem,
		Messages: []ClaudeMessage{{Role: "user", Content: "question one"}},
	}
	profile1 := tracker.BuildClaudeProfile(req1, 2048, "test-session")
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	tracker.Update("acct-1", profile1)

	// Round 2: conversation continues with new messages. The latest user
	// message has no explicit cache_control; it should still hit the stored
	// prefix via the implicit message-end breakpoint.
	req2 := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: baseSystem,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "question one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "follow-up question"},
		},
	}
	profile2 := tracker.BuildClaudeProfile(req2, 4096, "test-session")
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	result := tracker.Compute("acct-1", profile2)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read via implicit message-end breakpoint, got %+v", result)
	}
}

// TestPromptCacheCrossAccountSharing verifies C1: two different accountIDs with
// the SAME prompt fingerprint share cache entries. Account B's request should
// HIT on the fingerprint Account A stored — no per-account isolation.
func TestPromptCacheCrossAccountSharing(t *testing.T) {
	tracker := newPromptCacheTracker(5 * time.Minute)

	// Build a profile with one explicit cache_control breakpoint above the
	// min-token threshold.
	block := cacheablePromptBlock{
		Value: map[string]interface{}{"kind": "system", "block": map[string]interface{}{
			"type": "text", "text": strings.Repeat("x ", 600), // ~600 tokens > 1024? use more
			"cache_control": map[string]interface{}{"type": "ephemeral"},
		}},
		Tokens: 1200,
		TTL:    5 * time.Minute,
	}
	hasher := sha256.New()
	writeHashChunk(hasher, canonicalizeCacheValue(block.Value))
	var fp [32]byte
	copy(fp[:], hasher.Sum(nil))

	profile := &promptCacheProfile{
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fp, CumulativeTokens: 1200, TTL: 5 * time.Minute}},
		TotalInputTokens: 1200,
		Model:            "claude-sonnet-4-5",
	}

	// Account A: first request → cache_creation.
	usageA := tracker.Compute("account-A", profile)
	if usageA.CacheCreationInputTokens == 0 {
		t.Fatalf("account A: expected cache_creation > 0, got %d", usageA.CacheCreationInputTokens)
	}
	tracker.Update("account-A", profile)

	// Account B: SAME prompt, DIFFERENT account → should be cache_read (C1 fix).
	// Before C1: account B had its own empty store → cache_creation.
	// After C1:  account B shares the global store → cache_read.
	usageB := tracker.Compute("account-B", profile)
	if usageB.CacheReadInputTokens == 0 {
		t.Fatalf("account B: expected cache_read > 0 (cross-account sharing), got 0. usage=%+v", usageB)
	}
}

// TestPromptCacheCapConfigurable verifies C2: the cache-read cap can be set
// above the default 0.85 via config, so a request where 90% of input is from
// cache reports the full 90% (not clamped to 85%).
func TestPromptCacheCapConfigurable(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	// Build a profile: 1024 tokens cached (>= defaultMinCacheableTokens), total
	// 1100. Default cap clamps cache_read to 0.85*1100=935; cap=0.95 allows up
	// to 0.95*1100=1045 >= 1024, so the full breakpoint is reported.
	hasher := sha256.New()
	writeHashChunk(hasher, canonicalizeCacheValue(map[string]interface{}{"k": strings.Repeat("v ", 500)}))
	var fp [32]byte
	copy(fp[:], hasher.Sum(nil))
	profile := &promptCacheProfile{
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fp, CumulativeTokens: 1024, TTL: 5 * time.Minute}},
		TotalInputTokens: 1100,
		Model:            "claude-sonnet-4-5",
	}
	tracker := newPromptCacheTracker(5 * time.Minute)
	tracker.Update("acc", profile) // store it

	// Default cap 0.85: cache_read clamped to min(1000, 0.85*1100=935) = 935.
	usage85 := tracker.Compute("acc", profile)
	if usage85.CacheReadInputTokens > 940 {
		t.Fatalf("default cap: expected cache_read ~935, got %d", usage85.CacheReadInputTokens)
	}

	// Raise cap to 0.95: cache_read should be min(1000, 0.95*1100=1045) = 1000.
	config.UpdatePromptCacheMaxRatio(0.95)
	defer config.UpdatePromptCacheMaxRatio(0.85)
	usage95 := tracker.Compute("acc", profile)
	if usage95.CacheReadInputTokens < 990 {
		t.Fatalf("cap 0.95: expected cache_read ~1000, got %d", usage95.CacheReadInputTokens)
	}
}

// TestPromptCacheDiskPersistence verifies C3: entries saved to disk are
// reloaded on startup, surviving a "restart" (new tracker instance).
func TestPromptCacheDiskPersistence(t *testing.T) {
	path := t.TempDir() + "/prompt_cache.json"

	// Tracker 1: store an entry, flush to disk.
	t1 := newPromptCacheTracker(5 * time.Minute)
	hasher := sha256.New()
	writeHashChunk(hasher, "test-cache-value-disk")
	var fp [32]byte
	copy(fp[:], hasher.Sum(nil))
	t1.mu.Lock()
	t1.putLocked(fp, time.Now().Add(3*time.Minute), 5*time.Minute)
	t1.dirty = true
	t1.mu.Unlock()
	t1.flush(path)

	// Tracker 2: load from disk → should have the entry.
	t2 := newPromptCacheTracker(5 * time.Minute)
	t2.Load(path)
	t2.mu.Lock()
	_, ok := t2.entries[fp]
	t2.mu.Unlock()
	if !ok {
		t.Fatalf("C3: entry not reloaded from disk after 'restart'")
	}

	// Expired entry should NOT reload.
	path2 := t.TempDir() + "/expired.json"
	t1b := newPromptCacheTracker(5 * time.Minute)
	t1b.mu.Lock()
	fpExpired := sha256.Sum256([]byte("expired"))
	t1b.putLocked(fpExpired, time.Now().Add(-1*time.Minute), 5*time.Minute) // already expired
	t1b.dirty = true
	t1b.mu.Unlock()
	t1b.flush(path2)

	t3 := newPromptCacheTracker(5 * time.Minute)
	t3.Load(path2)
	t3.mu.Lock()
	_, okExpired := t3.entries[fpExpired]
	t3.mu.Unlock()
	if okExpired {
		t.Fatalf("C3: expired entry should not be reloaded")
	}
}

// TestComputeBreakdownClampedToCreation verifies the Anthropic usage invariant
// cache_creation_input_tokens == ephemeral_5m + ephemeral_1h holds even when the
// 0.85 cacheable cap reduces creation below the raw breakpoint deltas.
// Profile: TotalInputTokens=10000 → maxCacheable=8500. A matched breakpoint at
// 2000 leaves creation=6500, but the uncapped breakdown from matched=2000 to the
// 10000 breakpoint is 8000 (1h). Without clamping, 5m+1h (8000) != creation (6500).
func TestComputeBreakdownClampedToCreation(t *testing.T) {
	tr := newPromptCacheTracker(time.Hour)
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6",
		TotalInputTokens: 10000,
		Breakpoints: []promptCacheBreakpoint{
			{Fingerprint: [32]byte{1}, CumulativeTokens: 2000, TTL: 5 * time.Minute},
			{Fingerprint: [32]byte{2}, CumulativeTokens: 10000, TTL: time.Hour},
		},
	}
	now := time.Now()
	tr.mu.Lock()
	tr.putLocked([32]byte{1}, now.Add(time.Hour), time.Hour)
	tr.mu.Unlock()

	usage := tr.Compute("acct", profile)

	if usage.CacheCreation5mInputTokens+usage.CacheCreation1hInputTokens != usage.CacheCreationInputTokens {
		t.Fatalf("breakdown must sum to creation: 5m=%d 1h=%d creation=%d",
			usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens, usage.CacheCreationInputTokens)
	}
	if usage.CacheCreation1hInputTokens > usage.CacheCreationInputTokens {
		t.Fatalf("1h breakdown must not exceed creation: 1h=%d creation=%d",
			usage.CacheCreation1hInputTokens, usage.CacheCreationInputTokens)
	}
}

// TestComputeBreakdownClampedToCreationEmptyCacheBelowMin verifies the empty-cache
// (first-request) path still preserves the Anthropic invariant
// cache_creation_input_tokens == ephemeral_5m + ephemeral_1h when the input is
// below the minimum-cacheable threshold. effectiveCreation is zeroed
// (lastTokens < minTokens), but computePromptCacheTTLBreakdown(profile, 0) still
// returns lastTokens > 0 — the empty-path clamp (cache_tracker.go:255) must force
// 5m/1h to 0 so message_start doesn't emit an invariant-violating usage. Guards
// the empty-cache clamp site, which the matched-path TestComputeBreakdownClampedToCreation
// does not exercise (it always has lastTokens >= minTokens).
func TestComputeBreakdownClampedToCreationEmptyCacheBelowMin(t *testing.T) {
	tr := newPromptCacheTracker(time.Hour) // empty → empty-cache (first-request) path
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6", // minTokens = 1024 (non-opus default)
		TotalInputTokens: 200,
		Breakpoints: []promptCacheBreakpoint{
			{Fingerprint: [32]byte{1}, CumulativeTokens: 100, TTL: time.Hour}, // 100 < 1024 → effectiveCreation = 0
		},
	}

	usage := tr.Compute("acct", profile)

	if usage.CacheCreationInputTokens != 0 {
		t.Fatalf("below-minTokens first request should yield zero creation; got %d", usage.CacheCreationInputTokens)
	}
	if sum := usage.CacheCreation5mInputTokens + usage.CacheCreation1hInputTokens; sum != usage.CacheCreationInputTokens {
		t.Fatalf("breakdown must sum to creation (empty path, below min): 5m=%d 1h=%d creation=%d",
			usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens, usage.CacheCreationInputTokens)
	}
}

// TestPromptCacheMaxEntriesConfigurable verifies the cache LRU bound is
// configurable via config and defaults to 131072.
func TestPromptCacheMaxEntriesConfigurable(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	if got := config.GetPromptCacheMaxEntries(); got != 131072 {
		t.Fatalf("default cap: expected 131072, got %d", got)
	}

	if err := config.UpdatePromptCacheMaxEntries(50000); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := config.GetPromptCacheMaxEntries(); got != 50000 {
		t.Fatalf("after update: expected 50000, got %d", got)
	}

	// ≤ 0 falls back to the default.
	if err := config.UpdatePromptCacheMaxEntries(0); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := config.GetPromptCacheMaxEntries(); got != 131072 {
		t.Fatalf("zero should fall back to default 131072, got %d", got)
	}

	// An explicit small value is clamped to the 256 floor.
	if err := config.UpdatePromptCacheMaxEntries(10); err != nil {
		t.Fatalf("set small: %v", err)
	}
	if got := config.GetPromptCacheMaxEntries(); got != 256 {
		t.Fatalf("small value 10 should clamp to 256, got %d", got)
	}
}

// TestCacheDoesNotChurnAtHighCapacity is the regression guard for the prefix
// churn bug: at the production default capacity (131072), a 5000-prefix
// workload (far above the old 4096 cap) does not evict the oldest seeded prefix
// before it is replayed, so the replay reads from cache.
func TestCacheDoesNotChurnAtHighCapacity(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 131072)
	now := time.Now()
	tr.mu.Lock()
	for i := 0; i < 5000; i++ {
		var fp [32]byte
		fp[0] = byte(i)
		fp[1] = byte(i >> 8)
		fp[2] = byte(i >> 16)
		tr.putLocked(fp, now.Add(time.Hour), time.Hour)
	}
	tr.evictOverflowLocked()
	tr.mu.Unlock()

	// The oldest seeded prefix (i=0, all-zero bytes) must survive at cap 131072.
	var fp0 [32]byte
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6",
		TotalInputTokens: 2000,
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fp0, CumulativeTokens: 2000, TTL: time.Hour}},
	}
	if usage := tr.Compute("acct", profile); usage.CacheReadInputTokens == 0 {
		t.Fatalf("expected old prefix to survive at cap=131072 (no churn); got cache_read=0")
	}
}

// TestCacheChurnsAtLowCapacity proves the churn mechanism: at the old cap 4096
// the same 5000-prefix workload evicts the oldest prefix (i=0), so its replay
// misses. This is the sensitivity companion to TestCacheDoesNotChurnAtHighCapacity.
func TestCacheChurnsAtLowCapacity(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 4096)
	now := time.Now()
	tr.mu.Lock()
	for i := 0; i < 5000; i++ {
		var fp [32]byte
		fp[0] = byte(i)
		fp[1] = byte(i >> 8)
		fp[2] = byte(i >> 16)
		tr.putLocked(fp, now.Add(time.Hour), time.Hour)
	}
	tr.evictOverflowLocked()
	tr.mu.Unlock()

	// 5000 > 4096 → LRU evicts the 904 oldest; i=0 (oldest, never re-touched) is gone.
	var fp0 [32]byte
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6",
		TotalInputTokens: 2000,
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: fp0, CumulativeTokens: 2000, TTL: time.Hour}},
	}
	if usage := tr.Compute("acct", profile); usage.CacheReadInputTokens != 0 {
		t.Fatalf("expected oldest prefix churned at cap=4096; got cache_read=%d", usage.CacheReadInputTokens)
	}
}

// TestCacheStats verifies the atomic counters and Stats() snapshot: one miss,
// one hit, one expiration, and one LRU eviction are all counted, and capacity
// reflects the configured bound.
func TestCacheStats(t *testing.T) {
	tr := newPromptCacheTrackerWithCapacity(time.Hour, 3)
	hit := &promptCacheProfile{
		Model:            "claude-sonnet-4-6",
		TotalInputTokens: 2000,
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: [32]byte{7}, CumulativeTokens: 2000, TTL: time.Hour}},
	}
	now := time.Now()

	// Seed an already-expired entry, then Compute: pruneExpiredLocked drops it
	// (expirations=1) and the empty cache yields a miss (misses=1).
	tr.mu.Lock()
	tr.putLocked([32]byte{99}, now.Add(-time.Minute), time.Hour)
	tr.mu.Unlock()
	tr.Compute("acct", hit)

	// Store the hit profile, then Compute → hit (hits=1).
	tr.Update("acct", hit)
	tr.Compute("acct", hit)

	// Overflow: entries were {7}; add 3 more → {7,1,2,3} len=4 →
	// evictOverflowLocked pops the LRU back (7), leaving 3 (evictions=1).
	tr.mu.Lock()
	for i := 1; i <= 3; i++ {
		tr.putLocked([32]byte{byte(i)}, now.Add(time.Hour), time.Hour)
	}
	tr.evictOverflowLocked()
	tr.mu.Unlock()

	stats := tr.Stats()
	if stats.Hits != 1 {
		t.Errorf("hits = %d, want 1", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("misses = %d, want 1", stats.Misses)
	}
	if stats.Evictions != 1 {
		t.Errorf("evictions = %d, want 1", stats.Evictions)
	}
	if stats.Expirations != 1 {
		t.Errorf("expirations = %d, want 1", stats.Expirations)
	}
	if stats.Capacity != 3 {
		t.Errorf("capacity = %d, want 3", stats.Capacity)
	}
	if stats.Entries != 3 {
		t.Errorf("entries = %d, want 3", stats.Entries)
	}
}
