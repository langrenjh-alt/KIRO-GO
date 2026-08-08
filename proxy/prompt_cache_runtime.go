package proxy

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"kiro-go/config"
)

const promptCacheSaveInterval = 30 * time.Second

type promptCachePolicyLoadKey struct {
	handler  *Handler
	policyID string
}

type promptCachePolicyLoadCall struct {
	done    chan struct{}
	tracker *promptCacheTracker
}

var promptCachePolicyLoadFlights sync.Map

// promptCacheRuntimeForAPIKey resolves a stable policy snapshot and returns the
// tracker dedicated to that policy. The empty policy ID is the legacy global
// fallback and continues to use prompt_cache.json.
func (h *Handler) promptCacheRuntimeForAPIKey(apiKeyID string) (*promptCacheTracker, config.PromptCachePolicy) {
	policy := config.ResolvePromptCachePolicyForAPIKey(apiKeyID)
	if policy.ID == "" || h == nil {
		if h == nil {
			return nil, policy
		}
		return h.promptCache, policy
	}
	return h.promptCacheTrackerForPolicy(policy), policy
}

func (h *Handler) promptCacheTrackerForPolicy(policy config.PromptCachePolicy) *promptCacheTracker {
	if h == nil || policy.ID == "" {
		if h == nil {
			return nil
		}
		return h.promptCache
	}

	h.promptCachePoliciesMu.Lock()
	if h.promptCachePolicies == nil {
		h.promptCachePolicies = make(map[string]*promptCacheTracker)
	}
	if tracker := h.promptCachePolicies[policy.ID]; tracker != nil {
		h.promptCachePoliciesMu.Unlock()
		return tracker
	}
	h.promptCachePoliciesMu.Unlock()

	current := config.GetPromptCachePolicy(policy.ID)
	if current == nil {
		// A request may have resolved its snapshot just before an administrator
		// deleted the policy. Let that request finish without resurrecting a
		// persistent tracker that can no longer be selected by new requests.
		tracker := newPromptCacheTrackerWithCapacity(time.Hour, policy.PromptCacheMaxEntries)
		tracker.ApplyPolicySettings(policy.Revision, policy.PromptCacheMaxEntries, time.Duration(policy.PromptCacheTTLSeconds)*time.Second)
		return tracker
	}

	key := promptCachePolicyLoadKey{handler: h, policyID: policy.ID}
	call := &promptCachePolicyLoadCall{done: make(chan struct{})}
	actual, loaded := promptCachePolicyLoadFlights.LoadOrStore(key, call)
	if loaded {
		call = actual.(*promptCachePolicyLoadCall)
		<-call.done
		return call.tracker
	}
	func() {
		defer promptCachePolicyLoadFlights.Delete(key)
		defer close(call.done)
		call.tracker = h.loadPromptCachePolicyTracker(policy, *current)
	}()
	return call.tracker
}

// loadPromptCachePolicyTracker performs disk I/O outside the registry lock.
// The second policy lookup under the lock prevents an in-flight load from
// resurrecting a policy deleted while its persistence file was being read.
func (h *Handler) loadPromptCachePolicyTracker(snapshot, current config.PromptCachePolicy) *promptCacheTracker {
	// The request keeps its own policy snapshot for accounting, while physical
	// tracker limits always use the latest saved configuration. This prevents an
	// older in-flight request from reverting a newer hot update.
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, current.PromptCacheMaxEntries)
	tracker.ApplyPolicySettings(current.Revision, current.PromptCacheMaxEntries, time.Duration(current.PromptCacheTTLSeconds)*time.Second)
	// A request may have resolved its snapshot just before an administrator
	// deleted the policy after the current lookup. The delete path removes and
	// stops this tracker once it can acquire the registry lock.
	cachePath := filepath.Join(config.GetConfigDir(), promptCachePolicyFileName(snapshot.ID))
	tracker.Load(cachePath)

	h.promptCachePoliciesMu.Lock()
	defer h.promptCachePoliciesMu.Unlock()
	if h.promptCachePolicies == nil {
		h.promptCachePolicies = make(map[string]*promptCacheTracker)
	}
	if existing := h.promptCachePolicies[snapshot.ID]; existing != nil {
		return existing
	}
	latest := config.GetPromptCachePolicy(snapshot.ID)
	if latest == nil {
		return tracker
	}
	tracker.ApplyPolicySettings(latest.Revision, latest.PromptCacheMaxEntries, time.Duration(latest.PromptCacheTTLSeconds)*time.Second)
	tracker.startSaveLoop(cachePath, promptCacheSaveInterval)
	h.promptCachePolicies[snapshot.ID] = tracker
	return tracker
}

// preloadPromptCachePolicyTrackers moves named-policy disk reads and heap
// reconstruction into Handler startup, before the server begins accepting
// traffic. The create-policy admin path performs the same initialization for
// policies added after startup.
func (h *Handler) preloadPromptCachePolicyTrackers() {
	if h == nil {
		return
	}
	for _, policy := range config.ListPromptCachePolicies() {
		h.promptCacheTrackerForPolicy(policy)
	}
}

func (h *Handler) removePromptCachePolicyTracker(policyID string) {
	if h == nil || policyID == "" {
		return
	}
	h.promptCachePoliciesMu.Lock()
	tracker := h.promptCachePolicies[policyID]
	delete(h.promptCachePolicies, policyID)
	h.promptCachePoliciesMu.Unlock()
	if tracker != nil {
		tracker.Stop()
	}
	_ = os.Remove(filepath.Join(config.GetConfigDir(), promptCachePolicyFileName(policyID)))
}

// applyPromptCachePolicy hot-resizes a preloaded or post-startup tracker.
func (h *Handler) applyPromptCachePolicy(policy config.PromptCachePolicy) {
	if h == nil || policy.ID == "" {
		return
	}
	h.promptCachePoliciesMu.Lock()
	tracker := h.promptCachePolicies[policy.ID]
	h.promptCachePoliciesMu.Unlock()
	if tracker == nil {
		return
	}
	tracker.ApplyPolicySettings(policy.Revision, policy.PromptCacheMaxEntries, time.Duration(policy.PromptCacheTTLSeconds)*time.Second)
}

func (h *Handler) stopPromptCachePolicyTrackers() {
	if h == nil {
		return
	}
	h.promptCachePoliciesMu.Lock()
	trackers := make([]*promptCacheTracker, 0, len(h.promptCachePolicies))
	for _, tracker := range h.promptCachePolicies {
		if tracker != nil {
			trackers = append(trackers, tracker)
		}
	}
	h.promptCachePolicies = nil
	h.promptCachePoliciesMu.Unlock()
	for _, tracker := range trackers {
		tracker.Stop()
	}
}

func promptCachePolicyFileName(policyID string) string {
	sum := sha256.Sum256([]byte(policyID))
	return fmt.Sprintf("prompt_cache_policy_%x.json", sum[:8])
}

func promptCacheTrackerForProfile(fallback *promptCacheTracker, profile *promptCacheProfile) *promptCacheTracker {
	if profile != nil && profile.runtimeTracker != nil {
		return profile.runtimeTracker
	}
	return fallback
}

// PromptCachePolicyStats associates a configured named policy with its live
// tracker counters. Policies that have not been used yet still appear with a
// zero-valued snapshot and their configured capacity.
type PromptCachePolicyStats struct {
	ID    string           `json:"id"`
	Name  string           `json:"name"`
	Stats PromptCacheStats `json:"stats"`
}

// promptCacheStatsSnapshot aggregates the global fallback and active named
// trackers while also returning one entry for every configured named policy.
func (h *Handler) promptCacheStatsSnapshot() (PromptCacheStats, []PromptCachePolicyStats) {
	if h == nil {
		return PromptCacheStats{}, []PromptCachePolicyStats{}
	}
	total := PromptCacheStats{}
	if h.promptCache != nil {
		total = h.promptCache.Stats()
	}
	policies := config.ListPromptCachePolicies()
	h.promptCachePoliciesMu.Lock()
	trackers := make(map[string]*promptCacheTracker, len(h.promptCachePolicies))
	for id, tracker := range h.promptCachePolicies {
		if tracker != nil {
			trackers[id] = tracker
		}
	}
	h.promptCachePoliciesMu.Unlock()
	for _, tracker := range trackers {
		stats := tracker.Stats()
		addPromptCacheStats(&total, stats)
	}

	policyStats := make([]PromptCachePolicyStats, 0, len(policies))
	for _, policy := range policies {
		stats := PromptCacheStats{Capacity: policy.PromptCacheMaxEntries}
		if tracker := trackers[policy.ID]; tracker != nil {
			stats = tracker.Stats()
		}
		policyStats = append(policyStats, PromptCachePolicyStats{
			ID:    policy.ID,
			Name:  policy.Name,
			Stats: stats,
		})
	}
	return total, policyStats
}

func (h *Handler) promptCacheStats() PromptCacheStats {
	total, _ := h.promptCacheStatsSnapshot()
	return total
}

func addPromptCacheStats(total *PromptCacheStats, stats PromptCacheStats) {
	total.Entries += stats.Entries
	total.Capacity += stats.Capacity
	total.Hits += stats.Hits
	total.Misses += stats.Misses
	total.Evictions += stats.Evictions
	total.Expirations += stats.Expirations
	total.SkippedNoIsolation += stats.SkippedNoIsolation
	total.SkippedNoCacheControl += stats.SkippedNoCacheControl
	total.SkippedNoBreakpoints += stats.SkippedNoBreakpoints
	total.FunctionEntries += stats.FunctionEntries
}
