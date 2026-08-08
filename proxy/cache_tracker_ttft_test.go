package proxy

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
)

func TestPromptCacheComputeBoundsExpiredCleanup(t *testing.T) {
	totalExpired := promptCacheRequestPruneLimit + 19
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, totalExpired+8)
	now := time.Now()
	tracker.mu.Lock()
	for i := 0; i < totalExpired; i++ {
		tracker.putLocked([32]byte{byte(i), byte(i >> 8)}, now.Add(-time.Minute), time.Hour)
	}
	tracker.mu.Unlock()

	profile := &promptCacheProfile{
		Model:             "claude-sonnet-4-6",
		TotalInputTokens:  2000,
		MaxCacheReadRatio: 0.85,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      [32]byte{0xff, 0xff},
			CumulativeTokens: 2000,
			TTL:              time.Hour,
		}},
	}
	tracker.Compute("account", profile)

	tracker.mu.Lock()
	remaining := len(tracker.entries)
	tracker.mu.Unlock()
	if want := totalExpired - promptCacheRequestPruneLimit; remaining != want {
		t.Fatalf("expired entries remaining=%d, want bounded cleanup remainder %d", remaining, want)
	}
	if got := tracker.Stats().Expirations; got != int64(promptCacheRequestPruneLimit) {
		t.Fatalf("request cleaned %d entries, want exactly %d", got, promptCacheRequestPruneLimit)
	}
}

func TestPromptCacheBackgroundCleanupDrainsInBatches(t *testing.T) {
	totalExpired := promptCacheMaintenancePruneLimit + 17
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, totalExpired+1)
	now := time.Now()
	tracker.mu.Lock()
	for i := 0; i < totalExpired; i++ {
		tracker.putLocked([32]byte{byte(i), byte(i >> 8)}, now.Add(-time.Minute), time.Hour)
	}
	tracker.mu.Unlock()

	tracker.pruneExpiredBackground(now)
	if got, want := tracker.Stats().Entries, 17; got != want {
		t.Fatalf("entries after first maintenance batch=%d, want %d", got, want)
	}
	tracker.pruneExpiredBackground(now)
	if got := tracker.Stats().Entries; got != 0 {
		t.Fatalf("entries after second maintenance batch=%d, want 0", got)
	}
}

func TestFunctionCacheComputeUsesExpiryHeapWithoutFullScan(t *testing.T) {
	totalExpired := promptCacheRequestPruneLimit + 13
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, totalExpired+4)
	now := time.Now()
	tracker.functionMu.Lock()
	for i := 0; i < totalExpired; i++ {
		tracker.putFunctionEntryLocked([32]byte{byte(i), byte(i >> 8)}, now.Add(-time.Minute), time.Hour)
	}
	liveFingerprint := [32]byte{0xff, 0xfe}
	tracker.putFunctionEntryLocked(liveFingerprint, now.Add(time.Hour), time.Hour)
	tracker.functionMu.Unlock()

	profile := &promptCacheProfile{
		Model:                      "claude-sonnet-4-6",
		TotalInputTokens:           2000,
		SimulatedFunctionCache:     true,
		SimulatedHitRatePercent:    100,
		SimulatedMaxReadPercent:    100,
		SimulatedMinCacheableToken: 0,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      liveFingerprint,
			CumulativeTokens: 2000,
			TTL:              time.Hour,
		}},
	}
	if got := tracker.Compute("account", profile).CacheReadInputTokens; got != 2000 {
		t.Fatalf("function cache read=%d, want 2000", got)
	}

	tracker.functionMu.Lock()
	remaining := len(tracker.functionEntries)
	heapEntries := tracker.functionExpiryQueue.Len()
	tracker.functionMu.Unlock()
	if want := totalExpired - promptCacheRequestPruneLimit + 1; remaining != want {
		t.Fatalf("function entries remaining=%d, want %d", remaining, want)
	}
	if heapEntries != remaining {
		t.Fatalf("function expiry heap=%d, entries=%d", heapEntries, remaining)
	}
}

func TestPromptCachePersistenceSnapshotDoesNotTakeMainLock(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 4)
	tracker.mu.Lock()
	tracker.putLocked([32]byte{1}, time.Now().Add(time.Hour), time.Hour)
	tracker.mu.Unlock()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	done := make(chan []promptCacheEntryOnDisk, 1)
	go func() {
		done <- tracker.snapshotPersistentEntries(time.Now(), 1)
	}()
	select {
	case entries := <-done:
		if len(entries) != 1 || entries[0].Fingerprint != [32]byte{1} {
			t.Fatalf("unexpected persistence snapshot: %+v", entries)
		}
	case <-time.After(time.Second):
		t.Fatal("persistence snapshot blocked on the request-path mutex")
	}
}

func TestPromptCachePolicyPreloadRestoresIndependentTrackers(t *testing.T) {
	dir := t.TempDir()
	if err := config.Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	policyA, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "preload-a",
		PromptCacheMaxEntries: 256,
		PromptCacheTTLSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("add policy A: %v", err)
	}
	policyB, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "preload-b",
		PromptCacheMaxEntries: 512,
	})
	if err != nil {
		t.Fatalf("add policy B: %v", err)
	}

	fingerprint := [32]byte{9, 9}
	source := newPromptCacheTrackerWithCapacity(time.Hour, 256)
	source.mu.Lock()
	source.putLocked(fingerprint, time.Now().Add(time.Hour), time.Hour)
	source.markDirtyLocked()
	source.mu.Unlock()
	source.flush(filepath.Join(dir, promptCachePolicyFileName(policyA.ID)))

	h := &Handler{promptCachePolicies: make(map[string]*promptCacheTracker)}
	h.preloadPromptCachePolicyTrackers()
	defer h.stopPromptCachePolicyTrackers()

	h.promptCachePoliciesMu.Lock()
	trackerA := h.promptCachePolicies[policyA.ID]
	trackerB := h.promptCachePolicies[policyB.ID]
	h.promptCachePoliciesMu.Unlock()
	if trackerA == nil || trackerB == nil || trackerA == trackerB {
		t.Fatalf("preloaded trackers not independent: A=%p B=%p", trackerA, trackerB)
	}
	if got := trackerA.Stats().Entries; got != 1 {
		t.Fatalf("policy A restored entries=%d, want 1", got)
	}
	if got := trackerB.Stats().Entries; got != 0 {
		t.Fatalf("policy B restored entries=%d, want 0", got)
	}
	trackerA.mu.Lock()
	_, restored := trackerA.entries[fingerprint]
	trackerA.mu.Unlock()
	if !restored {
		t.Fatal("policy A did not restore its persisted fingerprint")
	}

	policyA.PromptCacheMaxEntries = 300
	updated, err := config.UpdatePromptCachePolicy(policyA.ID, policyA)
	if err != nil {
		t.Fatalf("update policy A: %v", err)
	}
	h.applyPromptCachePolicy(updated)
	if got := trackerA.Stats().Capacity; got != 300 {
		t.Fatalf("preloaded tracker ignored hot resize: capacity=%d", got)
	}
}

func TestPromptCachePolicyLazyLoadIsSingleFlight(t *testing.T) {
	dir := t.TempDir()
	if err := config.Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	policy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "single-flight",
		PromptCacheMaxEntries: 256,
	})
	if err != nil {
		t.Fatalf("add policy: %v", err)
	}
	h := &Handler{promptCachePolicies: make(map[string]*promptCacheTracker)}
	defer h.stopPromptCachePolicyTrackers()

	const workers = 32
	trackers := make(chan *promptCacheTracker, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			trackers <- h.promptCacheTrackerForPolicy(policy)
		}()
	}
	wg.Wait()
	close(trackers)
	var first *promptCacheTracker
	for tracker := range trackers {
		if first == nil {
			first = tracker
			continue
		}
		if tracker != first {
			t.Fatalf("lazy load returned multiple trackers: first=%p current=%p", first, tracker)
		}
	}
}
