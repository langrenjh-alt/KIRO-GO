package config

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultApiKeyUsageFlushInterval bounds API-key usage loss on an unclean
	// process exit without putting config.json persistence on the request path.
	DefaultApiKeyUsageFlushInterval = 5 * time.Second
	minApiKeyUsageFlushInterval     = time.Second
	maxApiKeyUsageFlushInterval     = 30 * time.Second
)

// apiKeyUsageState contains only usage accumulated since the last successful
// flush. The registry is immutable after publication, so RecordApiKeyUsage only
// performs a map lookup, a shared lifecycle lock, and atomic counter updates.
type apiKeyUsageState struct {
	lifecycle sync.RWMutex
	deleted   bool

	tokens   atomic.Int64
	credits  atomic.Uint64
	requests atomic.Int64
	lastUsed atomic.Int64
}

type apiKeyUsageRegistry struct {
	byID map[string]*apiKeyUsageState
}

var apiKeyUsageRegistryStore atomic.Pointer[apiKeyUsageRegistry]

type apiKeyUsageSnapshot struct {
	state      *apiKeyUsageState
	tokens     int64
	credits    float64
	requests   int64
	lastUsedAt int64

	baseTokens     int64
	baseCredits    float64
	baseRequests   int64
	baseLastUsedAt int64
}

func currentApiKeyUsageRegistry() *apiKeyUsageRegistry {
	registry := apiKeyUsageRegistryStore.Load()
	if registry != nil {
		return registry
	}
	empty := &apiKeyUsageRegistry{byID: map[string]*apiKeyUsageState{}}
	if apiKeyUsageRegistryStore.CompareAndSwap(nil, empty) {
		return empty
	}
	return apiKeyUsageRegistryStore.Load()
}

func apiKeyUsageStateForID(id string) *apiKeyUsageState {
	if id == "" {
		return nil
	}
	return currentApiKeyUsageRegistry().byID[id]
}

// rebuildApiKeyUsageStatesLocked replaces the immutable state registry after a
// config load. The caller must hold cfgLock, which serializes registry writers.
// A same-path reload reuses state so records arriving between its flush and file
// read remain pending; initializing a different config path starts clean.
func rebuildApiKeyUsageStatesLocked(preservePending bool) {
	old := currentApiKeyUsageRegistry()
	states := make(map[string]*apiKeyUsageState)
	if cfg != nil {
		states = make(map[string]*apiKeyUsageState, len(cfg.ApiKeys))
		for i := range cfg.ApiKeys {
			id := cfg.ApiKeys[i].ID
			if state := old.byID[id]; preservePending && state != nil {
				// Reusing the state preserves records that arrive after Load's
				// pre-read flush but before the replacement config is published.
				states[id] = state
			} else {
				states[id] = &apiKeyUsageState{}
			}
		}
	}
	for id, state := range old.byID {
		if states[id] == state {
			continue
		}
		state.lifecycle.Lock()
		state.deleted = true
		state.lifecycle.Unlock()
	}
	apiKeyUsageRegistryStore.Store(&apiKeyUsageRegistry{byID: states})
}

// registerApiKeyUsageStateLocked publishes a state for a newly persisted key.
// The caller must hold cfgLock.
func registerApiKeyUsageStateLocked(id string) {
	old := currentApiKeyUsageRegistry()
	states := make(map[string]*apiKeyUsageState, len(old.byID)+1)
	for existingID, state := range old.byID {
		states[existingID] = state
	}
	states[id] = &apiKeyUsageState{}
	apiKeyUsageRegistryStore.Store(&apiKeyUsageRegistry{byID: states})
}

// removeApiKeyUsageStateLocked removes state from future lookups. The caller
// must mark the state deleted while holding state.lifecycle before publishing.
func removeApiKeyUsageStateLocked(id string) {
	old := currentApiKeyUsageRegistry()
	states := make(map[string]*apiKeyUsageState, len(old.byID))
	for existingID, state := range old.byID {
		if existingID != id {
			states[existingID] = state
		}
	}
	apiKeyUsageRegistryStore.Store(&apiKeyUsageRegistry{byID: states})
}

func atomicAddFloat64(target *atomic.Uint64, delta float64) {
	for {
		oldBits := target.Load()
		updated := math.Float64frombits(oldBits) + delta
		if target.CompareAndSwap(oldBits, math.Float64bits(updated)) {
			return
		}
	}
}

func atomicMaxInt64(target *atomic.Int64, value int64) {
	for {
		old := target.Load()
		if value <= old || target.CompareAndSwap(old, value) {
			return
		}
	}
}

func mergeApiKeyUsage(entry *ApiKeyEntry) {
	state := apiKeyUsageStateForID(entry.ID)
	if state == nil {
		return
	}
	state.lifecycle.RLock()
	defer state.lifecycle.RUnlock()
	if state.deleted {
		return
	}
	entry.TokensUsed += state.tokens.Load()
	entry.CreditsUsed += math.Float64frombits(state.credits.Load())
	entry.RequestsCount += state.requests.Load()
	if lastUsedAt := state.lastUsed.Load(); lastUsedAt > entry.LastUsedAt {
		entry.LastUsedAt = lastUsedAt
	}
}

// configSnapshotWithApiKeyUsageLocked returns a shallow config snapshot with a
// private API-key slice. This lets unrelated configuration saves include pending
// usage without consuming it or changing the in-memory persistence baseline.
func configSnapshotWithApiKeyUsageLocked() *Config {
	if cfg == nil {
		return nil
	}
	snapshot := *cfg
	snapshot.ApiKeys = append([]ApiKeyEntry(nil), cfg.ApiKeys...)
	for i := range snapshot.ApiKeys {
		mergeApiKeyUsage(&snapshot.ApiKeys[i])
	}
	return &snapshot
}

// FlushApiKeyUsage atomically promotes all pending API-key deltas into cfg and
// persists them with one config write. A failed write restores every detached
// delta, including usage recorded concurrently while the write was in flight.
func FlushApiKeyUsage() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return flushApiKeyUsageLocked()
}

func flushApiKeyUsageLocked() error {
	if cfg == nil {
		return errors.New("config not initialized")
	}

	snapshots := make([]apiKeyUsageSnapshot, 0, len(cfg.ApiKeys))
	for i := range cfg.ApiKeys {
		entry := &cfg.ApiKeys[i]
		state := apiKeyUsageStateForID(entry.ID)
		if state == nil {
			continue
		}

		state.lifecycle.Lock()
		if state.deleted {
			state.lifecycle.Unlock()
			continue
		}
		snapshot := apiKeyUsageSnapshot{
			state:          state,
			tokens:         state.tokens.Swap(0),
			credits:        math.Float64frombits(state.credits.Swap(0)),
			requests:       state.requests.Swap(0),
			lastUsedAt:     state.lastUsed.Swap(0),
			baseTokens:     entry.TokensUsed,
			baseCredits:    entry.CreditsUsed,
			baseRequests:   entry.RequestsCount,
			baseLastUsedAt: entry.LastUsedAt,
		}
		state.lifecycle.Unlock()

		if snapshot.tokens == 0 && snapshot.credits == 0 && snapshot.requests == 0 && snapshot.lastUsedAt == 0 {
			continue
		}
		entry.TokensUsed += snapshot.tokens
		entry.CreditsUsed += snapshot.credits
		entry.RequestsCount += snapshot.requests
		if snapshot.lastUsedAt > entry.LastUsedAt {
			entry.LastUsedAt = snapshot.lastUsedAt
		}
		snapshots = append(snapshots, snapshot)
	}

	if len(snapshots) == 0 {
		return nil
	}
	if err := saveLocked(); err != nil {
		// cfgLock prevents Reset/Delete/Load from changing these entries while the
		// write runs. RecordApiKeyUsage may add new pending values, so restore by
		// addition/max instead of overwriting the state atomics.
		for i := range cfg.ApiKeys {
			entry := &cfg.ApiKeys[i]
			for _, snapshot := range snapshots {
				if snapshot.state != apiKeyUsageStateForID(entry.ID) {
					continue
				}
				entry.TokensUsed = snapshot.baseTokens
				entry.CreditsUsed = snapshot.baseCredits
				entry.RequestsCount = snapshot.baseRequests
				entry.LastUsedAt = snapshot.baseLastUsedAt
				snapshot.state.lifecycle.Lock()
				snapshot.state.tokens.Add(snapshot.tokens)
				atomicAddFloat64(&snapshot.state.credits, snapshot.credits)
				snapshot.state.requests.Add(snapshot.requests)
				atomicMaxInt64(&snapshot.state.lastUsed, snapshot.lastUsedAt)
				snapshot.state.lifecycle.Unlock()
				break
			}
		}
		return err
	}
	return nil
}

// ApiKeyUsageFlusher periodically persists pending API-key usage. Close is
// idempotent and performs the required final flush before returning.
type ApiKeyUsageFlusher struct {
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	onError   func(error)

	errMu   sync.Mutex
	lastErr error
}

// StartApiKeyUsageFlusher starts a background flusher. Production intervals are
// constrained to 1-30 seconds; invalid values fall back to five seconds.
func StartApiKeyUsageFlusher(interval time.Duration, onError func(error)) *ApiKeyUsageFlusher {
	if interval < minApiKeyUsageFlushInterval || interval > maxApiKeyUsageFlushInterval {
		interval = DefaultApiKeyUsageFlushInterval
	}
	flusher := &ApiKeyUsageFlusher{
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		onError: onError,
	}
	go flusher.run(interval)
	return flusher
}

func (flusher *ApiKeyUsageFlusher) run(interval time.Duration) {
	defer close(flusher.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			flusher.flush()
		case <-flusher.stop:
			flusher.flush()
			return
		}
	}
}

func (flusher *ApiKeyUsageFlusher) flush() {
	err := FlushApiKeyUsage()
	flusher.errMu.Lock()
	flusher.lastErr = err
	flusher.errMu.Unlock()
	if err != nil && flusher.onError != nil {
		flusher.onError(err)
	}
}

// Close stops the background loop, waits for its final flush, and returns that
// final attempt's error. Failed periodic flushes retain their deltas for retry.
func (flusher *ApiKeyUsageFlusher) Close() error {
	if flusher == nil {
		return nil
	}
	flusher.closeOnce.Do(func() { close(flusher.stop) })
	<-flusher.done
	flusher.errMu.Lock()
	defer flusher.errMu.Unlock()
	return flusher.lastErr
}
