package proxy

import (
	"bytes"
	"container/heap"
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultPromptCacheTTL = 5 * time.Minute
const promptCachePersistenceVersion = 2

// Anthropic requires cached prefixes to reach a minimum token count before
// caching takes effect. Breakpoints below this threshold are excluded from
// matching and storage to avoid reporting unrealistic 100% cache hits on
// short requests.
const defaultMinCacheableTokens = 1024
const opusMinCacheableTokens = 4096

type promptCacheUsage struct {
	CacheCreationInputTokens   int
	CacheReadInputTokens       int
	CacheCreation5mInputTokens int
	CacheCreation1hInputTokens int
}

type promptCacheBreakpoint struct {
	Fingerprint      [32]byte
	CumulativeTokens int
	TTL              time.Duration
}

type promptCacheProfile struct {
	Breakpoints                []promptCacheBreakpoint
	TotalInputTokens           int
	Model                      string
	IsolationSeed              string
	SimulatedFunctionCache     bool
	SimulatedHitRatePercent    int
	SimulatedMaxReadPercent    int
	SimulatedMinCacheableToken int
}

func minCacheableTokensForModel(model string) int {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "opus") {
		return opusMinCacheableTokens
	}
	return defaultMinCacheableTokens
}

type promptCacheEntry struct {
	ExpiresAt  time.Time
	TTL        time.Duration
	lruElem    *list.Element // back-ref into t.order; Value = fingerprint [32]byte
	expiryItem *promptCacheExpiryItem
}

type simulatedFunctionCacheEntry struct {
	ExpiresAt time.Time
	TTL       time.Duration
}

type promptCacheExpiryItem struct {
	Fingerprint [32]byte
	ExpiresAt   time.Time
	index       int
}

type promptCacheExpiryHeap []*promptCacheExpiryItem

func (h promptCacheExpiryHeap) Len() int { return len(h) }
func (h promptCacheExpiryHeap) Less(i, j int) bool {
	return h[i].ExpiresAt.Before(h[j].ExpiresAt)
}
func (h promptCacheExpiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *promptCacheExpiryHeap) Push(value interface{}) {
	item := value.(*promptCacheExpiryItem)
	item.index = len(*h)
	*h = append(*h, item)
}
func (h *promptCacheExpiryHeap) Pop() interface{} {
	old := *h
	last := len(old) - 1
	item := old[last]
	old[last] = nil
	item.index = -1
	*h = old[:last]
	return item
}

type promptCacheTracker struct {
	mu                    sync.Mutex
	entries               map[[32]byte]*promptCacheEntry
	functionEntries       map[[32]byte]simulatedFunctionCacheEntry
	order                 *list.List // front = most-recently-used; Element.Value = [32]byte fingerprint
	expiryQueue           promptCacheExpiryHeap
	maxEntries            int
	maxSupportedTTL       time.Duration
	defaultTTL            time.Duration
	hits                  int64 // atomic — Compute calls returning CacheReadInputTokens > 0
	misses                int64 // atomic — Compute calls returning CacheReadInputTokens == 0
	evictions             int64 // atomic — LRU pop-backs in evictOverflowLocked
	expirations           int64 // atomic — TTL removals in pruneExpiredLocked
	skippedNoIsolation    int64 // atomic — requests without a stable session or client key
	skippedNoCacheControl int64 // atomic legacy counter; default no-marker requests now build profiles
	skippedNoBreakpoints  int64 // atomic — markers that produced no usable breakpoint
	dirty                 bool
	stopChan              chan struct{}
	stopDone              chan struct{}
	stopOnce              sync.Once
}

func newPromptCacheTracker(maxTTL time.Duration) *promptCacheTracker {
	return newPromptCacheTrackerWithCapacity(maxTTL, config.GetPromptCacheMaxEntries())
}

func newPromptCacheTrackerWithCapacity(maxTTL time.Duration, maxEntries int) *promptCacheTracker {
	if maxTTL <= 0 {
		maxTTL = defaultPromptCacheTTL
	}
	return &promptCacheTracker{
		entries:         make(map[[32]byte]*promptCacheEntry),
		functionEntries: make(map[[32]byte]simulatedFunctionCacheEntry),
		order:           list.New(),
		maxEntries:      maxEntries,
		maxSupportedTTL: maxTTL,
		defaultTTL:      defaultPromptCacheTTL,
	}
}

// on-disk format for prompt-cache persistence (C3).
type promptCacheEntryOnDisk struct {
	Fingerprint [32]byte
	ExpiresAt   int64 // unix seconds
	TTLSeconds  int64
}

// Load reads persisted cache entries from path. Entries already expired (by the
// time load finishes) are dropped. Best-effort: a corrupt/missing file is not
// fatal — the tracker starts empty, same as the pre-C3 behavior.
func (t *promptCacheTracker) Load(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // missing file = fresh start (normal on first run)
	}
	var disk struct {
		Version int                      `json:"version"`
		Entries []promptCacheEntryOnDisk `json:"entries"`
	}
	if json.Unmarshal(data, &disk) != nil {
		return // corrupt = fresh start
	}
	if disk.Version != promptCachePersistenceVersion {
		return // fingerprint schema changed; incompatible entries start cold
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range disk.Entries {
		exp := time.Unix(e.ExpiresAt, 0)
		if !exp.After(now) {
			continue // already expired
		}
		t.putLocked(e.Fingerprint, exp, time.Duration(e.TTLSeconds)*time.Second)
	}
	entriesBefore := len(t.entries)
	t.evictOverflowLocked()
	if len(t.entries) != entriesBefore {
		t.dirty = true
	}
}

// startSaveLoop launches a background goroutine that flushes the cache to path
// every flushInterval (if dirty). Call once after Load. The goroutine exits when
// stopChan is closed.
func (t *promptCacheTracker) startSaveLoop(path string, flushInterval time.Duration) {
	t.stopChan = make(chan struct{})
	t.stopDone = make(chan struct{})
	go func() {
		defer close(t.stopDone)
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.flush(path)
			case <-t.stopChan:
				t.flush(path) // final flush
				return
			}
		}
	}()
}

func (t *promptCacheTracker) Stop() {
	if t == nil {
		return
	}
	// Idempotent: a second Stop (e.g. test cleanup + main shutdown) would
	// otherwise close an already-closed channel and panic.
	t.stopOnce.Do(func() {
		if t.stopChan != nil {
			close(t.stopChan)
		}
	})
	if t.stopDone != nil {
		<-t.stopDone
	}
}

func (t *promptCacheTracker) flush(path string) {
	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return
	}
	now := time.Now()
	entries := make([]promptCacheEntryOnDisk, 0, len(t.entries))
	for fp, e := range t.entries {
		if !e.ExpiresAt.After(now) {
			continue
		}
		entries = append(entries, promptCacheEntryOnDisk{
			Fingerprint: fp,
			ExpiresAt:   e.ExpiresAt.Unix(),
			TTLSeconds:  int64(e.TTL.Seconds()),
		})
	}
	t.dirty = false
	t.mu.Unlock()

	data, err := json.MarshalIndent(map[string]interface{}{
		"version": promptCachePersistenceVersion,
		"entries": entries,
	}, "", "  ")
	if err == nil {
		err = writeFileAtomic(path, data)
	}
	if err != nil {
		t.mu.Lock()
		t.dirty = true
		t.mu.Unlock()
		logger.Warnf("[PromptCache] failed to persist cache state: %v", err)
	}
}

// writeFileAtomic writes data to path via a temp file in the same directory
// then renames it into place, so a crash mid-flush cannot leave a truncated
// state file (which would silently reset the cache on the next Load). Mirrors
// the Python shim's tmp+os.replace path. os.Rename is atomic on the same
// filesystem; the same-dir temp guarantees same-fs. Best-effort: a rename
// failure falls back to a direct write (matching pre-existing behavior).
func writeFileAtomic(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	tmp, err := os.CreateTemp(dirOf(path), ".prompt-cache-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			tmp.Close()
		}
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, path); err != nil {
		return os.WriteFile(path, data, 0o600)
	}
	return nil
}

func dirOf(path string) string {
	if dir := filepath.Dir(path); dir != "" {
		return dir
	}
	return "."
}

// interfaceHasCacheControl reports whether the value tree contains any
// "cache_control" key on a dict node. Mirrors the Python shim's has_cache_control:
// cache_control can nest inside system blocks or message content blocks.
func interfaceHasCacheControl(v interface{}) bool {
	switch val := v.(type) {
	case map[string]interface{}:
		if _, ok := val["cache_control"]; ok {
			return true
		}
		for _, x := range val {
			if interfaceHasCacheControl(x) {
				return true
			}
		}
	case []interface{}:
		for _, x := range val {
			if interfaceHasCacheControl(x) {
				return true
			}
		}
	}
	return false
}

// claudeRequestHasCacheControl reports whether any cache_control marker appears
// in the request's system or message content. ClaudeTool is a typed struct with
// no cache_control field, so only system/messages can carry one.
func claudeRequestHasCacheControl(req *ClaudeRequest) bool {
	if req == nil {
		return false
	}
	if interfaceHasCacheControl(req.System) {
		return true
	}
	for _, m := range req.Messages {
		if interfaceHasCacheControl(m.Content) {
			return true
		}
	}
	return false
}

func defaultPromptCachePrefixBreakpointIndex(blocks []cacheablePromptBlock) int {
	last := -1
	for i, block := range blocks {
		if block.ExcludeFromReusablePrefix {
			continue
		}
		if block.Kind == "message" {
			break
		}
		if block.Kind == "system" || block.Kind == "tool" {
			last = i
		}
	}
	return last
}

func (t *promptCacheTracker) BuildClaudeProfile(req *ClaudeRequest, totalInputTokens int, isolationSeeds ...string) *promptCacheProfile {
	if config.GetCacheMode() == config.CacheModeFunctionSimulation {
		return buildSimulatedFunctionCacheProfile(req, totalInputTokens, config.GetFunctionCacheConfig())
	}
	isolationSeed := ""
	if len(isolationSeeds) > 0 {
		isolationSeed = isolationSeeds[0]
	}
	if isolationSeed == "" {
		atomic.AddInt64(&t.skippedNoIsolation, 1)
		return nil
	}

	hasExplicitCacheControl := claudeRequestHasCacheControl(req)
	blocks := flattenClaudeCacheBlocks(req, t.DefaultTTL())
	if len(blocks) == 0 {
		atomic.AddInt64(&t.skippedNoBreakpoints, 1)
		return nil
	}
	defaultPrefixBreakpointIndex := -1
	if !hasExplicitCacheControl {
		defaultPrefixBreakpointIndex = defaultPromptCachePrefixBreakpointIndex(blocks)
	}

	hasher := sha256.New()
	writeHashChunk(hasher, canonicalizeCacheValue(map[string]interface{}{
		"kind": "isolation_seed",
		"seed": isolationSeed,
	}))
	policyHasher := sha256.New()
	writeHashChunk(policyHasher, "prompt_cache_policy_v2")
	breakpoints := make([]promptCacheBreakpoint, 0)
	cumulativeTokens := 0
	excludedPrefixTokens := 0
	var activeTTL time.Duration

	for blockIndex, block := range blocks {
		if block.ExcludeFromReusablePrefix {
			excludedPrefixTokens += block.Tokens
			continue
		}
		canonical := canonicalizeCacheValue(block.Value)
		writeHashChunk(hasher, canonical)
		cumulativeTokens += block.Tokens

		// Determine whether this block acts as a cache breakpoint:
		//   1) Explicit cache_control on the block itself.
		//   2) Once any explicit breakpoint has been seen, every message-end
		//      boundary becomes an implicit breakpoint so that multi-turn
		//      conversations can hit earlier stored prefixes.
		//   3) If the request has no cache_control at all, the proxy supplies
		//      default TTL breakpoints at the reusable prefix and message ends.
		breakpointTTL := time.Duration(0)
		if block.TTL > 0 {
			if activeTTL != block.TTL {
				writePromptCachePolicyChange(policyHasher, hasher, block.TTL)
			}
			breakpointTTL = block.TTL
			activeTTL = block.TTL
		} else if block.HasCacheControl {
			// A malformed explicit marker must not silently inherit a previous
			// marker's TTL at this or later message boundaries.
			activeTTL = 0
		} else if !hasExplicitCacheControl && (blockIndex == defaultPrefixBreakpointIndex || block.IsMessageEnd) {
			defaultTTL := t.DefaultTTL()
			if activeTTL != defaultTTL {
				writePromptCachePolicyChange(policyHasher, hasher, defaultTTL)
			}
			breakpointTTL = defaultTTL
			activeTTL = defaultTTL
		} else if block.IsMessageEnd && activeTTL > 0 {
			breakpointTTL = activeTTL
		}

		if breakpointTTL <= 0 {
			continue
		}

		fingerprint := promptCacheFingerprint(hasher, policyHasher)
		breakpoints = append(breakpoints, promptCacheBreakpoint{
			Fingerprint:      fingerprint,
			CumulativeTokens: cumulativeTokens,
			TTL:              breakpointTTL,
		})
	}

	if len(breakpoints) == 0 {
		atomic.AddInt64(&t.skippedNoBreakpoints, 1)
		return nil
	}

	accountedTokens := cumulativeTokens + excludedPrefixTokens
	if totalInputTokens < accountedTokens {
		totalInputTokens = accountedTokens
	}

	return &promptCacheProfile{
		Breakpoints:      breakpoints,
		TotalInputTokens: totalInputTokens,
		Model:            req.Model,
		IsolationSeed:    isolationSeed,
	}
}

// BuildClaudeProfileForRequest preserves the native cache's tenant isolation
// while allowing function simulation to fingerprint the original request body.
func (t *promptCacheTracker) BuildClaudeProfileForRequest(req *ClaudeRequest, totalInputTokens int, isolationSeed, requestBodyFingerprint string) *promptCacheProfile {
	if config.GetCacheMode() == config.CacheModeFunctionSimulation {
		return buildSimulatedFunctionCacheProfile(req, totalInputTokens, config.GetFunctionCacheConfig(), requestBodyFingerprint)
	}
	return t.BuildClaudeProfile(req, totalInputTokens, isolationSeed)
}

// buildSimulatedFunctionCacheProfile creates usage-only cache metadata. It does
// not change tools, messages, the Kiro payload, or account selection.
func buildSimulatedFunctionCacheProfile(req *ClaudeRequest, totalInputTokens int, cfg config.FunctionCacheConfig, requestBodyFingerprints ...string) *promptCacheProfile {
	if req == nil || !cfg.Enabled {
		return nil
	}
	seedData := strings.TrimSpace(cfg.SimulatedData)
	if seedData == "" && len(req.Tools) > 0 {
		if encoded, err := json.Marshal(req.Tools); err == nil {
			seedData = string(encoded)
		}
	}
	bodyFingerprint := ""
	if len(requestBodyFingerprints) > 0 {
		bodyFingerprint = strings.TrimSpace(requestBodyFingerprints[0])
	}
	if bodyFingerprint == "" {
		bodyFingerprint = canonicalizeCacheValue(req)
	}
	if cfg.InputFingerprintChars > 0 {
		runes := []rune(bodyFingerprint)
		if len(runes) > cfg.InputFingerprintChars {
			bodyFingerprint = string(runes[len(runes)-cfg.InputFingerprintChars:])
		}
	}
	if bodyFingerprint == "" && seedData == "" && cfg.SimulatedTokens <= 0 {
		return nil
	}
	canonical := canonicalizeCacheValue(map[string]interface{}{
		"kind":                     "function_cache_simulation_body_fingerprint_v1",
		"request_body_fingerprint": bodyFingerprint,
		"simulated_data":           seedData,
		"simulated_tokens":         cfg.SimulatedTokens,
	})
	fingerprint := sha256.Sum256([]byte(canonical))
	tokens := cfg.SimulatedTokens
	if tokens <= 0 {
		tokens = estimateApproxTokens(seedData)
	}
	if tokens <= 0 {
		tokens = estimateApproxTokens(canonical)
	}
	if tokens <= 0 {
		return nil
	}
	tokens = jitterFunctionCacheValue(tokens, cfg.TokenJitterPercent, fingerprint, "tokens")
	ttlSeconds := cfg.TTLSeconds
	if ttlSeconds <= 0 {
		ttlSeconds = int(defaultPromptCacheTTL / time.Second)
	}
	ttlSeconds = jitterFunctionCacheValue(ttlSeconds, cfg.TTLJitterPercent, fingerprint, "ttl")
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	if totalInputTokens < tokens {
		totalInputTokens = tokens
	}
	hitRate, maxRead := 100, 85
	if cfg.HitRatePercent != nil {
		hitRate = *cfg.HitRatePercent
	}
	if cfg.MaxCacheReadPercent != nil {
		maxRead = *cfg.MaxCacheReadPercent
	}
	return &promptCacheProfile{
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      fingerprint,
			CumulativeTokens: tokens,
			TTL:              time.Duration(ttlSeconds) * time.Second,
		}},
		TotalInputTokens:           totalInputTokens,
		Model:                      req.Model,
		SimulatedFunctionCache:     true,
		SimulatedHitRatePercent:    hitRate,
		SimulatedMaxReadPercent:    maxRead,
		SimulatedMinCacheableToken: cfg.MinTokens,
	}
}

func functionCacheRequestBodyFingerprint(rawBody []byte) string {
	trimmed := bytes.TrimSpace(rawBody)
	if len(trimmed) == 0 {
		return ""
	}
	var decoded interface{}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err == nil {
		return canonicalizeCacheValue(decoded)
	}
	return string(trimmed)
}

func jitterFunctionCacheValue(base, percent int, fingerprint [32]byte, salt string) int {
	if base <= 0 || percent <= 0 {
		return base
	}
	if percent > 100 {
		percent = 100
	}
	seed := append([]byte(salt), fingerprint[:]...)
	sum := sha256.Sum256(seed)
	delta := int(binary.BigEndian.Uint32(sum[:4])%uint32(percent*2+1)) - percent
	value := base * (100 + delta) / 100
	if value < 1 {
		return 1
	}
	return value
}

func simulatedFunctionCacheHitAllowed(profile *promptCacheProfile, now time.Time) bool {
	if profile == nil || profile.SimulatedHitRatePercent <= 0 || len(profile.Breakpoints) == 0 {
		return false
	}
	if profile.SimulatedHitRatePercent >= 100 {
		return true
	}
	seed := make([]byte, 0, 64)
	seed = strconv.AppendInt(seed, now.UnixNano(), 10)
	seed = append(seed, profile.Breakpoints[0].Fingerprint[:]...)
	sum := sha256.Sum256(seed)
	return int(binary.BigEndian.Uint32(sum[:4])%100) < profile.SimulatedHitRatePercent
}

func (t *promptCacheTracker) Compute(accountID string, profile *promptCacheProfile) (u promptCacheUsage) {
	if t == nil || profile == nil || len(profile.Breakpoints) == 0 || accountID == "" {
		return promptCacheUsage{}
	}
	if profile.SimulatedFunctionCache {
		return t.computeSimulatedFunctionCache(profile)
	}
	defer func() {
		if u.CacheReadInputTokens > 0 {
			atomic.AddInt64(&t.hits, 1)
		} else {
			atomic.AddInt64(&t.misses, 1)
		}
	}()

	minTokens := minCacheableTokensForModel(profile.Model)
	last := profile.Breakpoints[len(profile.Breakpoints)-1]
	lastTokens := minInt(last.CumulativeTokens, profile.TotalInputTokens)
	maxCacheRead := int(float64(profile.TotalInputTokens) * config.GetPromptCacheMaxRatio())
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredLocked(now)

	rawMatchedTokens := 0
	for i := len(profile.Breakpoints) - 1; i >= 0; i-- {
		breakpoint := profile.Breakpoints[i]
		// Skip breakpoints below the minimum cacheable token threshold.
		if breakpoint.CumulativeTokens < minTokens {
			continue
		}
		entry, ok := t.entries[breakpoint.Fingerprint]
		if !ok || !entry.ExpiresAt.After(now) {
			continue
		}
		t.refreshEntryLocked(entry, now.Add(entry.TTL), entry.TTL)
		t.order.MoveToFront(entry.lruElem)
		t.dirty = true // hit extends TTL — persist so a flush before the next Update doesn't lose it
		rawMatchedTokens = minInt(breakpoint.CumulativeTokens, profile.TotalInputTokens)
		if rawMatchedTokens > lastTokens {
			rawMatchedTokens = lastTokens
		}
		break
	}

	creation := maxInt(lastTokens-rawMatchedTokens, 0)
	if lastTokens < minTokens {
		creation = 0
	}
	cacheRead := rawMatchedTokens
	if cacheRead > maxCacheRead {
		cacheRead = maxCacheRead
	}
	cache5m, cache1h := computePromptCacheTTLBreakdown(profile, rawMatchedTokens)
	cache5m, cache1h = clampCacheBreakdownToCreation(cache5m, cache1h, creation)
	return clampCacheReadToModelWindow(promptCacheUsage{
		CacheCreationInputTokens:   creation,
		CacheReadInputTokens:       cacheRead,
		CacheCreation5mInputTokens: cache5m,
		CacheCreation1hInputTokens: cache1h,
	}, profile.Model)
}

func (t *promptCacheTracker) computeSimulatedFunctionCache(profile *promptCacheProfile) (usage promptCacheUsage) {
	defer func() {
		if usage.CacheReadInputTokens > 0 {
			atomic.AddInt64(&t.hits, 1)
		} else {
			atomic.AddInt64(&t.misses, 1)
		}
	}()
	last := profile.Breakpoints[len(profile.Breakpoints)-1]
	cacheTokens := minInt(last.CumulativeTokens, profile.TotalInputTokens)
	if cacheTokens < profile.SimulatedMinCacheableToken {
		return promptCacheUsage{}
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for fingerprint, entry := range t.functionEntries {
		if !entry.ExpiresAt.After(now) {
			delete(t.functionEntries, fingerprint)
		}
	}
	entry, hit := t.functionEntries[last.Fingerprint]
	hit = hit && entry.ExpiresAt.After(now) && simulatedFunctionCacheHitAllowed(profile, now)
	if !hit {
		cache5m, cache1h := computePromptCacheTTLBreakdown(profile, 0)
		return promptCacheUsage{
			CacheCreationInputTokens:   cacheTokens,
			CacheCreation5mInputTokens: cache5m,
			CacheCreation1hInputTokens: cache1h,
		}
	}
	entry.ExpiresAt = now.Add(entry.TTL)
	t.functionEntries[last.Fingerprint] = entry
	maxRead := int(float64(profile.TotalInputTokens) * float64(profile.SimulatedMaxReadPercent) / 100)
	if maxRead < 0 {
		maxRead = 0
	}
	if cacheTokens > maxRead {
		cacheTokens = maxRead
	}
	return clampCacheReadToModelWindow(promptCacheUsage{CacheReadInputTokens: cacheTokens}, profile.Model)
}

func clampCacheReadToModelWindow(usage promptCacheUsage, model string) promptCacheUsage {
	limit := getContextWindowSize(model)
	if limit > 0 && usage.CacheReadInputTokens > limit {
		usage.CacheReadInputTokens = limit
	}
	return usage
}

func (t *promptCacheTracker) updateSimulatedFunctionCache(profile *promptCacheProfile) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for fingerprint, entry := range t.functionEntries {
		if !entry.ExpiresAt.After(now) {
			delete(t.functionEntries, fingerprint)
		}
	}
	for _, breakpoint := range profile.Breakpoints {
		if breakpoint.CumulativeTokens < profile.SimulatedMinCacheableToken {
			continue
		}
		t.functionEntries[breakpoint.Fingerprint] = simulatedFunctionCacheEntry{
			ExpiresAt: now.Add(breakpoint.TTL),
			TTL:       breakpoint.TTL,
		}
	}
	// Keep simulation state bounded by the current tracker capacity without
	// persisting it or mixing it into the native LRU namespace.
	for len(t.functionEntries) > t.maxEntries {
		for fingerprint := range t.functionEntries {
			delete(t.functionEntries, fingerprint)
			break
		}
	}
}

func (t *promptCacheTracker) Update(accountID string, profile *promptCacheProfile) {
	if t == nil || profile == nil || len(profile.Breakpoints) == 0 || accountID == "" {
		return
	}
	if profile.SimulatedFunctionCache {
		t.updateSimulatedFunctionCache(profile)
		return
	}

	minTokens := minCacheableTokensForModel(profile.Model)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredLocked(now)

	// Entries share one bounded map, but the isolation seed is part of every
	// fingerprint, so tenants cannot match each other's prefixes.
	for _, breakpoint := range profile.Breakpoints {
		// Skip breakpoints below the minimum cacheable token threshold.
		if breakpoint.CumulativeTokens < minTokens {
			continue
		}
		t.putLocked(breakpoint.Fingerprint, now.Add(breakpoint.TTL), breakpoint.TTL)
	}
	t.dirty = true
	t.evictOverflowLocked()
}

func (t *promptCacheTracker) pruneExpiredLocked(now time.Time) {
	removed := false
	for t.expiryQueue.Len() > 0 {
		item := t.expiryQueue[0]
		if item.ExpiresAt.After(now) {
			break
		}
		heap.Pop(&t.expiryQueue)
		entry, ok := t.entries[item.Fingerprint]
		if !ok || entry.expiryItem != item {
			continue
		}
		t.order.Remove(entry.lruElem)
		delete(t.entries, item.Fingerprint)
		atomic.AddInt64(&t.expirations, 1)
		removed = true
	}
	if removed {
		t.dirty = true
	}
}

// putLocked inserts a fingerprint or refreshes its existing entry, marking it
// most-recently-used. Caller holds t.mu.
func (t *promptCacheTracker) putLocked(fp [32]byte, expiresAt time.Time, ttl time.Duration) {
	if e, ok := t.entries[fp]; ok {
		t.refreshEntryLocked(e, expiresAt, ttl)
		t.order.MoveToFront(e.lruElem)
		return
	}
	elem := t.order.PushFront(fp)
	expiryItem := &promptCacheExpiryItem{Fingerprint: fp, ExpiresAt: expiresAt}
	heap.Push(&t.expiryQueue, expiryItem)
	t.entries[fp] = &promptCacheEntry{ExpiresAt: expiresAt, TTL: ttl, lruElem: elem, expiryItem: expiryItem}
}

func (t *promptCacheTracker) refreshEntryLocked(entry *promptCacheEntry, expiresAt time.Time, ttl time.Duration) {
	entry.ExpiresAt = expiresAt
	entry.TTL = ttl
	entry.expiryItem.ExpiresAt = expiresAt
	heap.Fix(&t.expiryQueue, entry.expiryItem.index)
}

// evictOverflowLocked bounds the entries map to maxEntries by evicting the
// least-recently-used entries (the back of the order list). O(1) per eviction.
// Caller holds t.mu.
func (t *promptCacheTracker) evictOverflowLocked() {
	for len(t.entries) > t.maxEntries {
		back := t.order.Back()
		if back == nil {
			return
		}
		fp := back.Value.([32]byte)
		entry := t.entries[fp]
		t.order.Remove(back)
		if entry != nil && entry.expiryItem != nil && entry.expiryItem.index >= 0 {
			heap.Remove(&t.expiryQueue, entry.expiryItem.index)
		}
		delete(t.entries, fp)
		atomic.AddInt64(&t.evictions, 1)
	}
}

// SetMaxEntries updates the live LRU capacity. Shrinking takes effect before
// the method returns and evicts the least-recently-used entries first.
func (t *promptCacheTracker) SetMaxEntries(maxEntries int) {
	if t == nil {
		return
	}
	if maxEntries < 1 {
		maxEntries = 1
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.maxEntries == maxEntries {
		return
	}

	entriesBefore := len(t.entries)
	t.maxEntries = maxEntries
	t.pruneExpiredLocked(time.Now())
	t.evictOverflowLocked()
	if len(t.entries) != entriesBefore {
		t.dirty = true
	}
}

// DefaultTTL returns the runtime default used when a cache_control marker does
// not specify ttl. The value is snapshotted once per profile build.
func (t *promptCacheTracker) DefaultTTL() time.Duration {
	if t == nil {
		return defaultPromptCacheTTL
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.defaultTTL
}

// SetDefaultTTL hot-reloads the default for new profiles. Existing entries
// retain their stored TTL and expiry.
func (t *promptCacheTracker) SetDefaultTTL(ttl time.Duration) {
	if t == nil {
		return
	}
	if ttl != time.Hour {
		ttl = defaultPromptCacheTTL
	}
	t.mu.Lock()
	t.defaultTTL = ttl
	t.mu.Unlock()
}

// PromptCacheStats is a point-in-time snapshot of cache counters, surfaced via
// /v1/stats. All counters are cumulative since tracker construction.
type PromptCacheStats struct {
	Entries               int   `json:"entries"`
	Capacity              int   `json:"capacity"`
	Hits                  int64 `json:"hits"`
	Misses                int64 `json:"misses"`
	Evictions             int64 `json:"evictions"`
	Expirations           int64 `json:"expirations"`
	SkippedNoIsolation    int64 `json:"skippedNoIsolation"`
	SkippedNoCacheControl int64 `json:"skippedNoCacheControl"`
	SkippedNoBreakpoints  int64 `json:"skippedNoBreakpoints"`
	FunctionEntries       int   `json:"functionEntries"`
}

func (t *promptCacheTracker) Stats() PromptCacheStats {
	if t == nil {
		return PromptCacheStats{}
	}
	t.mu.Lock()
	entries := len(t.entries)
	functionEntries := len(t.functionEntries)
	capacity := t.maxEntries
	t.mu.Unlock()
	return PromptCacheStats{
		Entries:               entries,
		Capacity:              capacity,
		Hits:                  atomic.LoadInt64(&t.hits),
		Misses:                atomic.LoadInt64(&t.misses),
		Evictions:             atomic.LoadInt64(&t.evictions),
		Expirations:           atomic.LoadInt64(&t.expirations),
		SkippedNoIsolation:    atomic.LoadInt64(&t.skippedNoIsolation),
		SkippedNoCacheControl: atomic.LoadInt64(&t.skippedNoCacheControl),
		SkippedNoBreakpoints:  atomic.LoadInt64(&t.skippedNoBreakpoints),
		FunctionEntries:       functionEntries,
	}
}

// splitAgainstTotal rescales an estimate-domain cache split onto a real input
// total, preserving input + creation + read == realTotal. The cache-covered
// fraction of the prompt (in estimate units) is applied to realTotal, then
// divided into read vs creation by their estimate-domain ratio; the 5m/1h
// breakdown is scaled to the new creation total.
func (u promptCacheUsage) splitAgainstTotal(estTotal, realTotal int) promptCacheUsage {
	if realTotal <= 0 {
		return u
	}
	coveredEst := u.CacheCreationInputTokens + u.CacheReadInputTokens
	if coveredEst <= 0 || estTotal <= 0 {
		return promptCacheUsage{} // no cache coverage → all fresh input
	}
	ratio := float64(coveredEst) / float64(estTotal)
	if ratio > 1 {
		ratio = 1
	}
	cacheTotal := int(float64(realTotal)*ratio + 0.5)
	if cacheTotal > realTotal {
		cacheTotal = realTotal
	}
	read := int(float64(cacheTotal)*float64(u.CacheReadInputTokens)/float64(coveredEst) + 0.5)
	if read > cacheTotal {
		read = cacheTotal
	}
	if read < 0 {
		read = 0
	}
	creation := cacheTotal - read
	cache5m, cache1h := creation, 0
	if u.CacheCreationInputTokens > 0 {
		cache1h = int(float64(u.CacheCreation1hInputTokens)*float64(creation)/float64(u.CacheCreationInputTokens) + 0.5)
		if cache1h > creation {
			cache1h = creation
		}
		cache5m = creation - cache1h
	}
	return promptCacheUsage{
		CacheCreationInputTokens:   creation,
		CacheReadInputTokens:       read,
		CacheCreation5mInputTokens: cache5m,
		CacheCreation1hInputTokens: cache1h,
	}
}

type cacheablePromptBlock struct {
	Value                     interface{}
	Tokens                    int
	TTL                       time.Duration
	HasCacheControl           bool
	Kind                      string
	IsMessageEnd              bool
	ExcludeFromReusablePrefix bool
	PreserveInReusablePrefix  bool
}

func flattenClaudeCacheBlocks(req *ClaudeRequest, defaultTTL time.Duration) []cacheablePromptBlock {
	blocks := make([]cacheablePromptBlock, 0)
	toolLinks := newToolLinkageNormalizer()
	blocks = append(blocks, buildCachePreludeBlock(req))

	for toolIndex, tool := range req.Tools {
		toolValue := map[string]interface{}{
			"kind":         "tool",
			"tool_index":   toolIndex,
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.InputSchema,
		}
		fingerprintValue := stripCachePositionKeys(toolValue)
		markerTTL, hasCacheControl := extractPromptCacheTTL(tool, defaultTTL)
		blocks = append(blocks, cacheablePromptBlock{
			Value:           fingerprintValue,
			Tokens:          estimateApproxTokens(canonicalizeCacheValue(fingerprintValue)),
			TTL:             normalizePromptCacheTTL(markerTTL),
			HasCacheControl: hasCacheControl,
			Kind:            "tool",
		})
	}

	appendSystemCacheBlocks(&blocks, req.System, defaultTTL)
	markStaticSystemPrefixBlocks(blocks, req.promptCacheStaticSystemPrefix)

	for messageIndex, msg := range req.Messages {
		appendMessageCacheBlocks(&blocks, messageIndex, msg, toolLinks, defaultTTL)
	}

	markDynamicSystemPrefixBlocks(blocks)
	return blocks
}

func markDynamicSystemPrefixBlocks(blocks []cacheablePromptBlock) {
	firstBreakpoint := -1
	for i := range blocks {
		if blocks[i].Kind == "system" && blocks[i].TTL > 0 {
			firstBreakpoint = i
			break
		}
	}
	if firstBreakpoint < 0 {
		return
	}
	for i := 0; i < firstBreakpoint; i++ {
		if blocks[i].Kind == "system" && !blocks[i].PreserveInReusablePrefix {
			blocks[i].ExcludeFromReusablePrefix = true
		}
	}
}

func markStaticSystemPrefixBlocks(blocks []cacheablePromptBlock, count int) {
	for i := range blocks {
		if count <= 0 {
			return
		}
		if blocks[i].Kind != "system" {
			continue
		}
		blocks[i].PreserveInReusablePrefix = true
		count--
	}
}

func buildCachePreludeBlock(req *ClaudeRequest) cacheablePromptBlock {
	prelude := map[string]interface{}{
		"kind":        "request_prelude",
		"model":       req.Model,
		"tool_choice": req.ToolChoice,
	}
	return cacheablePromptBlock{
		Value:  prelude,
		Tokens: estimateApproxTokens(canonicalizeCacheValue(prelude)),
		Kind:   "request_prelude",
	}
}

func appendSystemCacheBlocks(blocks *[]cacheablePromptBlock, system interface{}, defaultTTL time.Duration) {
	switch v := system.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":         "system",
			"system_index": 0,
			"block": map[string]interface{}{
				"type": "text",
				"text": v,
			},
		}, false, nil, defaultTTL)
	case []interface{}:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block":        block,
			}, false, nil, defaultTTL)
		}
	case []string:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block": map[string]interface{}{
					"type": "text",
					"text": block,
				},
			}, false, nil, defaultTTL)
		}
	}
}

func appendMessageCacheBlocks(blocks *[]cacheablePromptBlock, messageIndex int, msg ClaudeMessage, toolLinks *toolLinkageNormalizer, defaultTTL time.Duration) {
	role := msg.Role
	switch content := msg.Content.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":          "message",
			"message_index": messageIndex,
			"role":          role,
			"block_index":   0,
			"block": map[string]interface{}{
				"type": "text",
				"text": content,
			},
		}, true, toolLinks, defaultTTL)
	case []interface{}:
		lastIdx := len(content) - 1
		for blockIndex, block := range content {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   blockIndex,
				"block":         block,
			}, blockIndex == lastIdx, toolLinks, defaultTTL)
		}
	default:
		if content != nil {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   0,
				"block":         content,
			}, true, toolLinks, defaultTTL)
		}
	}
}

func appendPromptBlock(blocks *[]cacheablePromptBlock, wrapper map[string]interface{}, isMessageEnd bool, toolLinks *toolLinkageNormalizer, defaultTTL time.Duration) {
	blockValue := wrapper["block"]
	markerTTL, hasCacheControl := extractPromptCacheTTL(blockValue, defaultTTL)
	ttl := normalizePromptCacheTTL(markerTTL)

	// Drop volatile billing metadata from the cache fingerprint. Claude Code's
	// x-anthropic-billing-header can drift, appear, or disappear across
	// otherwise identical requests, and it does not change model semantics.
	if isAnthropicBillingHeaderBlock(blockValue) {
		return
	}

	wrapper["block"] = normalizeToolLinkageForCacheFingerprint(blockValue, toolLinks)
	fingerprintValue := stripCachePositionKeys(wrapper)
	tokenValue := make(map[string]interface{}, len(fingerprintValue))
	for key, value := range fingerprintValue {
		tokenValue[key] = value
	}
	tokenValue["block"] = normalizeToolLinkageForCacheFingerprint(wrapper["block"], nil)
	kind, _ := wrapper["kind"].(string)
	*blocks = append(*blocks, cacheablePromptBlock{
		Value:           fingerprintValue,
		Tokens:          estimateApproxTokens(canonicalizeCacheValue(tokenValue)),
		TTL:             ttl,
		HasCacheControl: hasCacheControl,
		Kind:            kind,
		IsMessageEnd:    isMessageEnd,
	})
}

type toolLinkageNormalizer struct {
	aliases map[string]string
	next    int
}

func newToolLinkageNormalizer() *toolLinkageNormalizer {
	return &toolLinkageNormalizer{aliases: make(map[string]string)}
}

func (n *toolLinkageNormalizer) register(rawID string) string {
	if alias, ok := n.aliases[rawID]; ok {
		return alias
	}
	n.next++
	alias := "tool-ref-" + strconv.Itoa(n.next)
	n.aliases[rawID] = alias
	return alias
}

func (n *toolLinkageNormalizer) reference(rawID string) string {
	if alias, ok := n.aliases[rawID]; ok {
		return alias
	}
	return "unresolved-tool-ref"
}

// normalizeToolLinkageForCacheFingerprint replaces request-local IDs with
// stable ordinal aliases, preserving result-to-call relationships.
func normalizeToolLinkageForCacheFingerprint(value interface{}, normalizer *toolLinkageNormalizer) interface{} {
	block, ok := value.(map[string]interface{})
	if !ok {
		return value
	}
	blockType, _ := block["type"].(string)
	blockType = strings.ToLower(blockType)
	switch blockType {
	case "tool_use", "tool_result", "server_tool_use", "mcp_tool_use", "mcp_tool_result":
	default:
		return value
	}

	cloned := make(map[string]interface{}, len(block))
	for key, item := range block {
		if key == "id" || key == "tool_use_id" {
			continue
		}
		cloned[key] = item
	}
	if normalizer == nil {
		return cloned
	}
	switch blockType {
	case "tool_use", "server_tool_use", "mcp_tool_use":
		if rawID, ok := block["id"].(string); ok && rawID != "" {
			cloned["id"] = normalizer.register(rawID)
		}
	case "tool_result", "mcp_tool_result":
		if rawID, ok := block["tool_use_id"].(string); ok && rawID != "" {
			cloned["tool_use_id"] = normalizer.reference(rawID)
		}
	}
	return cloned
}

func stripCachePositionKeys(value map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(value))
	for key, item := range value {
		if isCachePositionKey(key) {
			continue
		}
		cloned[key] = item
	}
	return cloned
}

func isAnthropicBillingHeaderBlock(value interface{}) bool {
	blockMap, ok := value.(map[string]interface{})
	if !ok {
		return false
	}

	// Only normalize text blocks (or blocks without an explicit type but containing text).
	if t, ok := blockMap["type"].(string); ok && t != "" && t != "text" {
		return false
	}

	text, ok := blockMap["text"].(string)
	if !ok {
		return false
	}

	trimmed := strings.TrimLeft(text, " \t\r\n")
	return strings.HasPrefix(strings.ToLower(trimmed), "x-anthropic-billing-header:")
}

func extractPromptCacheTTL(value interface{}, defaultTTL time.Duration) (time.Duration, bool) {
	block, ok := value.(map[string]interface{})
	if !ok {
		if raw, err := json.Marshal(value); err == nil {
			var decoded map[string]interface{}
			if json.Unmarshal(raw, &decoded) == nil {
				block = decoded
				ok = true
			}
		}
	}
	if !ok {
		return 0, false
	}

	rawCache, ok := block["cache_control"]
	if !ok {
		return 0, false
	}
	cacheControl, ok := rawCache.(map[string]interface{})
	if !ok {
		return 0, true
	}
	cacheType, _ := cacheControl["type"].(string)
	if !strings.EqualFold(cacheType, "ephemeral") {
		return 0, true
	}

	rawTTL, hasTTL := cacheControl["ttl"]
	if !hasTTL {
		return normalizePromptCacheTTL(defaultTTL), true
	}
	if ttl, ok := parsePromptCacheTTLValue(rawTTL); ok &&
		(ttl == defaultPromptCacheTTL || ttl == time.Hour) {
		return ttl, true
	}
	return 0, true
}

func parsePromptCacheTTLValue(value interface{}) (time.Duration, bool) {
	// Anthropic uses "5m"/"1h" on the wire. Exact second equivalents remain
	// accepted for compatibility with clients that serialize durations numerically.
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(v))
		if trimmed == "" {
			return 0, false
		}
		if d, err := time.ParseDuration(trimmed); err == nil {
			return d, true
		}
		if seconds, err := strconv.Atoi(trimmed); err == nil {
			return time.Duration(seconds) * time.Second, true
		}
	case float64:
		if v > 0 && v == float64(int64(v)) {
			return time.Duration(v) * time.Second, true
		}
	case int:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	}
	return 0, false
}

func writePromptCachePolicyChange(policyHasher, prefixHasher hashWriter, ttl time.Duration) {
	writeHashChunk(policyHasher, "ttl_change")
	_, _ = policyHasher.Write(prefixHasher.Sum(nil))
	writeHashChunk(policyHasher, strconv.FormatInt(int64(ttl/time.Second), 10))
}

func promptCacheFingerprint(prefixHasher, policyHasher hashWriter) [32]byte {
	hasher := sha256.New()
	writeHashChunk(hasher, "prompt_cache_fingerprint_v2")
	_, _ = hasher.Write(prefixHasher.Sum(nil))
	_, _ = hasher.Write(policyHasher.Sum(nil))
	var fingerprint [32]byte
	copy(fingerprint[:], hasher.Sum(nil))
	return fingerprint
}

func normalizePromptCacheTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 0
	}
	if ttl > time.Hour {
		return time.Hour
	}
	if ttl > defaultPromptCacheTTL {
		return time.Hour
	}
	return defaultPromptCacheTTL
}

func computePromptCacheTTLBreakdown(profile *promptCacheProfile, matchedTokens int) (int, int) {
	if profile == nil || len(profile.Breakpoints) == 0 {
		return 0, 0
	}

	cache5m := 0
	cache1h := 0
	previous := matchedTokens
	for _, breakpoint := range profile.Breakpoints {
		current := minInt(breakpoint.CumulativeTokens, profile.TotalInputTokens)
		if current <= previous {
			continue
		}
		delta := current - previous
		if breakpoint.TTL >= time.Hour {
			cache1h += delta
		} else {
			cache5m += delta
		}
		previous = current
	}
	return cache5m, cache1h
}

// clampCacheBreakdownToCreation scales the 5m/1h cache-creation split down to
// creation when the raw breakpoint deltas (uncapped by the 0.85 cacheable ratio)
// exceed it, preserving the 1h:5m ratio. Guarantees the Anthropic invariant
// cache_creation_input_tokens == ephemeral_5m + ephemeral_1h.
func clampCacheBreakdownToCreation(cache5m, cache1h, creation int) (int, int) {
	total := cache5m + cache1h
	if total <= creation || total <= 0 {
		return cache5m, cache1h
	}
	scale := float64(creation) / float64(total)
	one := int(float64(cache1h)*scale + 0.5)
	if one > creation {
		one = creation
	}
	if one < 0 {
		one = 0
	}
	return creation - one, one
}

func billedClaudeInputTokens(inputTokens int, usage promptCacheUsage) int {
	return maxInt(inputTokens-usage.CacheCreationInputTokens-usage.CacheReadInputTokens, 0)
}

func buildClaudeUsageMap(inputTokens, outputTokens int, usage promptCacheUsage, includeCache bool) map[string]interface{} {
	result := map[string]interface{}{
		"input_tokens":  billedClaudeInputTokens(inputTokens, usage),
		"output_tokens": outputTokens,
		"service_tier":  "standard",
		"inference_geo": "not_available",
	}
	if !includeCache {
		return result
	}
	result["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
	result["cache_read_input_tokens"] = usage.CacheReadInputTokens
	result["cache_creation"] = map[string]int{
		"ephemeral_5m_input_tokens": usage.CacheCreation5mInputTokens,
		"ephemeral_1h_input_tokens": usage.CacheCreation1hInputTokens,
	}
	return result
}

func buildClaudeTerminalUsageMap(inputTokens, outputTokens int, usage promptCacheUsage) map[string]interface{} {
	return map[string]interface{}{
		"input_tokens":                billedClaudeInputTokens(inputTokens, usage),
		"output_tokens":               outputTokens,
		"output_tokens_details":       map[string]int{"thinking_tokens": 0},
		"cache_creation_input_tokens": usage.CacheCreationInputTokens,
		"cache_read_input_tokens":     usage.CacheReadInputTokens,
	}
}

func canonicalizeCacheValue(value interface{}) string {
	var buf bytes.Buffer
	writeCanonicalJSON(&buf, value)
	return buf.String()
}

func writeCanonicalJSON(buf *bytes.Buffer, value interface{}) {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case string:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case []interface{}:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalJSON(buf, item)
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		buf.WriteByte('{')
		keys := make([]string, 0, len(v))
		for key := range v {
			if key == "cache_control" {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			buf.Write(encoded)
			buf.WriteByte(':')
			writeCanonicalJSON(buf, v[key])
		}
		buf.WriteByte('}')
	default:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	}
}

func isCachePositionKey(key string) bool {
	switch key {
	case "tool_index", "system_index", "message_index", "block_index":
		return true
	default:
		return false
	}
}

func writeHashChunk(hasher hashWriter, chunk string) {
	length := strconv.Itoa(len(chunk))
	hasher.Write([]byte(length))
	hasher.Write([]byte{0})
	hasher.Write([]byte(chunk))
	hasher.Write([]byte{0})
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
