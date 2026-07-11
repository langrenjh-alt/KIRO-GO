package proxy

import (
	"kiro-go/config"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPromptCacheProfileRequiresStableIsolationSeed(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4-6",
		System: []interface{}{
			map[string]interface{}{
				"type":          "text",
				"text":          strings.Repeat("stable cached instructions ", 240),
				"cache_control": map[string]interface{}{"type": "ephemeral"},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	total := estimateClaudeRequestInputTokens(req)

	if profile := tracker.BuildClaudeProfile(req, total); profile != nil {
		t.Fatal("empty isolation seed must disable prompt-cache accounting")
	}
	first := tracker.BuildClaudeProfile(req, total, "tenant-a/session-1")
	repeat := tracker.BuildClaudeProfile(req, total, "tenant-a/session-1")
	isolated := tracker.BuildClaudeProfile(req, total, "tenant-b/session-1")
	if first == nil || repeat == nil || isolated == nil {
		t.Fatal("non-empty isolation seeds must build cache profiles")
	}
	if first.IsolationSeed != "tenant-a/session-1" {
		t.Fatalf("profile isolation seed = %q", first.IsolationSeed)
	}
	if first.Breakpoints[0].Fingerprint != repeat.Breakpoints[0].Fingerprint {
		t.Fatal("the same isolation seed must produce a stable fingerprint")
	}
	if first.Breakpoints[0].Fingerprint == isolated.Breakpoints[0].Fingerprint {
		t.Fatal("different isolation seeds must not share cache fingerprints")
	}

	tracker.Update("account", first)
	if got := tracker.Compute("account", repeat).CacheReadInputTokens; got == 0 {
		t.Fatal("the same isolation seed should hit")
	}
	if got := tracker.Compute("account", isolated).CacheReadInputTokens; got != 0 {
		t.Fatalf("different isolation seed cache_read = %d, want 0", got)
	}
}

func TestPromptCacheStatsExplainSkippedProfiles(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	withMarker := &ClaudeRequest{
		Model: "claude-sonnet-4-6",
		System: []interface{}{map[string]interface{}{
			"type":          "text",
			"text":          strings.Repeat("cacheable ", 500),
			"cache_control": map[string]interface{}{"type": "ephemeral"},
		}},
	}
	tracker.BuildClaudeProfile(withMarker, 2000)

	withoutMarker := &ClaudeRequest{Model: "claude-sonnet-4-6", System: "plain system"}
	tracker.BuildClaudeProfile(withoutMarker, 2000, "tenant")

	invalidMarker := &ClaudeRequest{
		Model: "claude-sonnet-4-6",
		System: []interface{}{map[string]interface{}{
			"type":          "text",
			"text":          strings.Repeat("cacheable ", 500),
			"cache_control": map[string]interface{}{"type": "persistent"},
		}},
	}
	tracker.BuildClaudeProfile(invalidMarker, 2000, "tenant")

	stats := tracker.Stats()
	if stats.SkippedNoIsolation != 1 || stats.SkippedNoCacheControl != 1 || stats.SkippedNoBreakpoints != 1 {
		t.Fatalf("unexpected skipped-profile counters: %+v", stats)
	}
}

func TestPromptCacheDynamicSystemBeforeBreakpointIsExcluded(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	stable := strings.Repeat("stable reusable system instructions ", 220)
	build := func(dynamic string, includeDynamic bool) *ClaudeRequest {
		system := make([]interface{}, 0, 2)
		if includeDynamic {
			system = append(system, map[string]interface{}{
				"type": "text",
				"text": dynamic,
			})
		}
		system = append(system, map[string]interface{}{
			"type":          "text",
			"text":          stable,
			"cache_control": map[string]interface{}{"type": "ephemeral"},
		})
		return &ClaudeRequest{
			Model:    "claude-sonnet-4-6",
			System:   system,
			Messages: []ClaudeMessage{{Role: "user", Content: "same question"}},
		}
	}

	reqA := build(strings.Repeat("volatile-a ", 180), true)
	reqB := build(strings.Repeat("volatile-b ", 180), true)
	withoutDynamic := build("", false)
	profileA := tracker.BuildClaudeProfile(reqA, estimateClaudeRequestInputTokens(reqA), "session")
	profileB := tracker.BuildClaudeProfile(reqB, estimateClaudeRequestInputTokens(reqB), "session")
	stableOnly := tracker.BuildClaudeProfile(withoutDynamic, estimateClaudeRequestInputTokens(withoutDynamic), "session")
	if profileA == nil || profileB == nil || stableOnly == nil {
		t.Fatal("expected cache profiles")
	}
	if len(profileA.Breakpoints) != len(stableOnly.Breakpoints) {
		t.Fatalf("breakpoint counts differ: dynamic=%d stable=%d", len(profileA.Breakpoints), len(stableOnly.Breakpoints))
	}
	for i := range profileA.Breakpoints {
		if profileA.Breakpoints[i].Fingerprint != profileB.Breakpoints[i].Fingerprint ||
			profileA.Breakpoints[i].Fingerprint != stableOnly.Breakpoints[i].Fingerprint {
			t.Fatalf("dynamic pre-breakpoint system changed fingerprint %d", i)
		}
		if profileA.Breakpoints[i].CumulativeTokens != stableOnly.Breakpoints[i].CumulativeTokens {
			t.Fatalf("dynamic pre-breakpoint system changed reusable coverage %d", i)
		}
	}
	if profileA.TotalInputTokens <= stableOnly.TotalInputTokens {
		t.Fatalf("dynamic system tokens missing from total: dynamic=%d stable=%d", profileA.TotalInputTokens, stableOnly.TotalInputTokens)
	}

	tracker.Update("account", profileA)
	if got := tracker.Compute("account", profileB).CacheReadInputTokens; got == 0 {
		t.Fatal("dynamic pre-breakpoint system drift should retain reusable-prefix hits")
	}
}

func TestPromptCacheSystemIsRetainedWhenBreakpointStartsInMessage(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	build := func(systemText string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4-6",
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": systemText,
				},
			},
			Messages: []ClaudeMessage{
				{
					Role: "user",
					Content: []interface{}{
						map[string]interface{}{
							"type":          "text",
							"text":          strings.Repeat("message-level cached content ", 240),
							"cache_control": map[string]interface{}{"type": "ephemeral"},
						},
					},
				},
			},
		}
	}

	firstReq := build("system variant a")
	secondReq := build("system variant b")
	first := tracker.BuildClaudeProfile(firstReq, estimateClaudeRequestInputTokens(firstReq), "session")
	second := tracker.BuildClaudeProfile(secondReq, estimateClaudeRequestInputTokens(secondReq), "session")
	if first == nil || second == nil {
		t.Fatal("expected message-breakpoint cache profiles")
	}
	if first.Breakpoints[0].Fingerprint == second.Breakpoints[0].Fingerprint {
		t.Fatal("system drift must change fingerprints when no system breakpoint exists")
	}

	tracker.Update("account", first)
	if got := tracker.Compute("account", second).CacheReadInputTokens; got != 0 {
		t.Fatalf("message-only breakpoint reused a different system: cache_read=%d", got)
	}
}

func TestPromptCacheRetainsProxyInjectedThinkingSystem(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4-6",
		System: []interface{}{
			map[string]interface{}{"type": "text", "text": "volatile Claude Code header"},
			map[string]interface{}{
				"type":          "text",
				"text":          strings.Repeat("stable cached instructions ", 240),
				"cache_control": map[string]interface{}{"type": "ephemeral"},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	standard := cloneClaudeRequestForThinking(req, false)
	thinking := cloneClaudeRequestForThinking(req, true)
	standardProfile := tracker.BuildClaudeProfile(standard, estimateClaudeRequestInputTokens(standard), "session")
	thinkingProfile := tracker.BuildClaudeProfile(thinking, estimateClaudeRequestInputTokens(thinking), "session")
	if standardProfile == nil || thinkingProfile == nil {
		t.Fatal("expected standard and thinking cache profiles")
	}
	if standardProfile.Breakpoints[0].Fingerprint == thinkingProfile.Breakpoints[0].Fingerprint {
		t.Fatal("thinking and standard requests must not share a cache fingerprint")
	}
	if thinkingProfile.Breakpoints[0].CumulativeTokens <= standardProfile.Breakpoints[0].CumulativeTokens {
		t.Fatalf("proxy-injected thinking system was excluded: standard=%d thinking=%d",
			standardProfile.Breakpoints[0].CumulativeTokens, thinkingProfile.Breakpoints[0].CumulativeTokens)
	}
}

func TestPromptCacheIgnoresToolLinkageIDsButPreservesSemanticInput(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	build := func(toolID, semanticID string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4-6",
			Messages: []ClaudeMessage{
				{
					Role: "assistant",
					Content: []interface{}{
						map[string]interface{}{
							"type": "tool_use",
							"id":   toolID,
							"name": "lookup",
							"input": map[string]interface{}{
								"id":      semanticID,
								"payload": strings.Repeat("semantic tool input ", 260),
							},
							"cache_control": map[string]interface{}{"type": "ephemeral"},
						},
					},
				},
				{
					Role: "user",
					Content: []interface{}{
						map[string]interface{}{
							"type":        "tool_result",
							"tool_use_id": toolID,
							"content":     "lookup complete",
						},
					},
				},
			},
		}
	}

	firstReq := build("toolu_111", "business-a")
	driftedReq := build("toolu_999", "business-a")
	changedReq := build("toolu_777", "business-b")
	first := tracker.BuildClaudeProfile(firstReq, estimateClaudeRequestInputTokens(firstReq), "session")
	drifted := tracker.BuildClaudeProfile(driftedReq, estimateClaudeRequestInputTokens(driftedReq), "session")
	changed := tracker.BuildClaudeProfile(changedReq, estimateClaudeRequestInputTokens(changedReq), "session")
	if first == nil || drifted == nil || changed == nil {
		t.Fatal("expected tool cache profiles")
	}
	if len(first.Breakpoints) != len(drifted.Breakpoints) || len(first.Breakpoints) != len(changed.Breakpoints) {
		t.Fatal("unexpected tool breakpoint count")
	}
	for i := range first.Breakpoints {
		if first.Breakpoints[i].Fingerprint != drifted.Breakpoints[i].Fingerprint {
			t.Fatalf("request-local tool ID changed fingerprint %d", i)
		}
		if first.Breakpoints[i].Fingerprint == changed.Breakpoints[i].Fingerprint {
			t.Fatalf("semantic input.id change did not change fingerprint %d", i)
		}
	}

	tracker.Update("account", first)
	if got := tracker.Compute("account", drifted).CacheReadInputTokens; got == 0 {
		t.Fatal("tool linkage ID drift should hit")
	}
	if got := tracker.Compute("account", changed).CacheReadInputTokens; got != 0 {
		t.Fatalf("semantic tool input change cache_read = %d, want 0", got)
	}
}

func TestPromptCachePreservesParallelToolResultAssociations(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	build := func(firstID, secondID, firstResultID, secondResultID string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4-6",
			Messages: []ClaudeMessage{
				{
					Role: "assistant",
					Content: []interface{}{
						map[string]interface{}{
							"type":          "tool_use",
							"id":            firstID,
							"name":          "lookup_user",
							"input":         map[string]interface{}{"user": "alice"},
							"cache_control": map[string]interface{}{"type": "ephemeral"},
						},
						map[string]interface{}{
							"type":  "tool_use",
							"id":    secondID,
							"name":  "lookup_user",
							"input": map[string]interface{}{"user": "bob"},
						},
					},
				},
				{
					Role: "user",
					Content: []interface{}{
						map[string]interface{}{"type": "tool_result", "tool_use_id": firstResultID, "content": "alice-result"},
						map[string]interface{}{"type": "tool_result", "tool_use_id": secondResultID, "content": "bob-result"},
					},
				},
			},
		}
	}

	first := build("toolu_a", "toolu_b", "toolu_a", "toolu_b")
	drifted := build("toolu_x", "toolu_y", "toolu_x", "toolu_y")
	swapped := build("toolu_x", "toolu_y", "toolu_y", "toolu_x")
	profile := tracker.BuildClaudeProfile(first, estimateClaudeRequestInputTokens(first), "session")
	driftedProfile := tracker.BuildClaudeProfile(drifted, estimateClaudeRequestInputTokens(drifted), "session")
	swappedProfile := tracker.BuildClaudeProfile(swapped, estimateClaudeRequestInputTokens(swapped), "session")
	if profile == nil || driftedProfile == nil || swappedProfile == nil {
		t.Fatal("expected parallel tool cache profiles")
	}
	last := len(profile.Breakpoints) - 1
	if profile.Breakpoints[last].Fingerprint != driftedProfile.Breakpoints[last].Fingerprint {
		t.Fatal("raw tool ID drift changed a stable association fingerprint")
	}
	if profile.Breakpoints[last].Fingerprint == swappedProfile.Breakpoints[last].Fingerprint {
		t.Fatal("swapping parallel tool-result associations did not change the fingerprint")
	}
}

func TestPromptCacheExpirationHeapPreservesLiveLRUAtCapacity(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 2)
	now := time.Now()
	liveFingerprint := [32]byte{1}
	expiredFingerprint := [32]byte{2}
	newFingerprint := [32]byte{3}

	tracker.mu.Lock()
	tracker.putLocked(liveFingerprint, now.Add(time.Hour), time.Hour)
	// Inserted last, the expired entry is more recent in LRU order than the live entry.
	tracker.putLocked(expiredFingerprint, now.Add(-time.Minute), time.Hour)
	tracker.mu.Unlock()

	newProfile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6",
		TotalInputTokens: 2000,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      newFingerprint,
			CumulativeTokens: 2000,
			TTL:              time.Hour,
		}},
	}
	tracker.Update("account", newProfile)

	tracker.mu.Lock()
	_, livePresent := tracker.entries[liveFingerprint]
	_, expiredPresent := tracker.entries[expiredFingerprint]
	tracker.mu.Unlock()
	if !livePresent || expiredPresent {
		t.Fatalf("entries after overflow: live=%v expired=%v", livePresent, expiredPresent)
	}
	stats := tracker.Stats()
	if stats.Expirations != 1 || stats.Evictions != 0 || stats.Entries != 2 {
		t.Fatalf("unexpected expiration/eviction stats: %+v", stats)
	}
}

func TestPromptCacheFlushFailureStaysDirty(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 2)
	tracker.mu.Lock()
	tracker.putLocked([32]byte{1}, time.Now().Add(time.Hour), time.Hour)
	tracker.dirty = true
	tracker.mu.Unlock()

	tracker.flush(t.TempDir()) // A directory cannot be replaced by the cache file.
	tracker.mu.Lock()
	dirty := tracker.dirty
	tracker.mu.Unlock()
	if !dirty {
		t.Fatal("failed cache flush cleared dirty state")
	}
}

func TestPromptCachePruneExpiredMarksDirtyBeforeFutureEntry(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 4)
	now := time.Now()
	expired := [32]byte{1}
	future := [32]byte{2}

	tracker.mu.Lock()
	tracker.putLocked(expired, now.Add(-time.Minute), time.Hour)
	tracker.putLocked(future, now.Add(time.Hour), time.Hour)
	tracker.dirty = false
	tracker.pruneExpiredLocked(now)
	_, expiredStillPresent := tracker.entries[expired]
	_, futureStillPresent := tracker.entries[future]
	dirty := tracker.dirty
	tracker.mu.Unlock()

	if expiredStillPresent || !futureStillPresent {
		t.Fatalf("unexpected entries after prune: expired=%v future=%v", expiredStillPresent, futureStillPresent)
	}
	if !dirty {
		t.Fatal("removing an expired entry before a future heap head must mark the cache dirty")
	}
}

func TestPromptCacheStopWaitsForFinalFlush(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 2)
	path := filepath.Join(t.TempDir(), "prompt-cache.json")
	tracker.startSaveLoop(path, time.Hour)
	tracker.mu.Lock()
	tracker.putLocked([32]byte{1}, time.Now().Add(time.Hour), time.Hour)
	tracker.dirty = true
	tracker.mu.Unlock()

	tracker.Stop()
	loaded := newPromptCacheTrackerWithCapacity(time.Hour, 2)
	loaded.Load(path)
	if got := loaded.Stats().Entries; got != 1 {
		t.Fatalf("final flush entries=%d, want 1", got)
	}
}

func TestPromptCacheMaxRatioOnlyCapsCacheRead(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdatePromptCacheMaxRatio(0.5); err != nil {
		t.Fatalf("set ratio: %v", err)
	}
	defer config.UpdatePromptCacheMaxRatio(0.85)

	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6",
		TotalInputTokens: 2000,
		Breakpoints: []promptCacheBreakpoint{
			{Fingerprint: [32]byte{1}, CumulativeTokens: 2000, TTL: time.Hour},
		},
	}
	tracker.mu.Lock()
	tracker.putLocked([32]byte{9}, time.Now().Add(time.Hour), time.Hour)
	tracker.mu.Unlock()

	cold := tracker.Compute("account", profile)
	if cold.CacheReadInputTokens != 0 || cold.CacheCreationInputTokens != 2000 {
		t.Fatalf("cold miss = %+v, want read=0 creation=2000", cold)
	}

	tracker.Update("account", profile)
	hit := tracker.Compute("account", profile)
	if hit.CacheReadInputTokens != 1000 {
		t.Fatalf("cache_read = %d, want ratio-capped 1000", hit.CacheReadInputTokens)
	}
	if hit.CacheCreationInputTokens != 0 {
		t.Fatalf("ratio cap changed cache creation to %d, want 0", hit.CacheCreationInputTokens)
	}
}

func TestPromptCacheSetMaxEntriesShrinksImmediatelyByLRU(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 4)
	now := time.Now()
	tracker.mu.Lock()
	tracker.putLocked([32]byte{1}, now.Add(time.Hour), time.Hour)
	tracker.putLocked([32]byte{2}, now.Add(time.Hour), time.Hour)
	tracker.putLocked([32]byte{3}, now.Add(time.Hour), time.Hour)
	tracker.putLocked([32]byte{1}, now.Add(time.Hour), time.Hour)
	tracker.mu.Unlock()

	tracker.SetMaxEntries(2)
	stats := tracker.Stats()
	if stats.Capacity != 2 || stats.Entries != 2 || stats.Evictions != 1 {
		t.Fatalf("unexpected stats after shrink: %+v", stats)
	}
	tracker.mu.Lock()
	_, oldestSurvived := tracker.entries[[32]byte{2}]
	_, touchedSurvived := tracker.entries[[32]byte{1}]
	tracker.mu.Unlock()
	if oldestSurvived || !touchedSurvived {
		t.Fatalf("shrink did not preserve LRU order: oldest=%v touched=%v", oldestSurvived, touchedSurvived)
	}
	if !tracker.dirty {
		t.Fatal("capacity eviction must mark persisted state dirty")
	}
}

func TestPromptCacheLoadRespectsCurrentCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt_cache.json")
	now := time.Now()
	source := newPromptCacheTrackerWithCapacity(time.Hour, 4)
	source.mu.Lock()
	for i := 1; i <= 4; i++ {
		source.putLocked([32]byte{byte(i)}, now.Add(time.Hour), time.Hour)
	}
	source.dirty = true
	source.mu.Unlock()
	source.flush(path)

	loaded := newPromptCacheTrackerWithCapacity(time.Hour, 2)
	loaded.Load(path)
	stats := loaded.Stats()
	if stats.Capacity != 2 || stats.Entries != 2 || stats.Evictions != 2 {
		t.Fatalf("loaded cache exceeded configured capacity: %+v", stats)
	}
	if !loaded.dirty {
		t.Fatal("overflow discarded during load must be persisted on the next flush")
	}
}

func TestPromptCacheSetMaxEntriesIsConcurrentSafe(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 32)
	now := time.Now()
	tracker.mu.Lock()
	for i := 0; i < 32; i++ {
		tracker.putLocked([32]byte{byte(i)}, now.Add(time.Hour), time.Hour)
	}
	tracker.mu.Unlock()

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				tracker.SetMaxEntries(1 + (offset+i)%32)
				_ = tracker.Stats()
			}
		}(worker)
	}
	wg.Wait()
	tracker.SetMaxEntries(2)
	stats := tracker.Stats()
	if stats.Capacity != 2 || stats.Entries > 2 {
		t.Fatalf("concurrent resize left invalid state: %+v", stats)
	}
}
