package proxy

import "testing"

// The blend must be invisible below the threshold: an ordinary short session
// keeps exactly the bytes it had before this feature existed.
func TestContextUsageBlendInertBelowThreshold(t *testing.T) {
	// Derived from the constant so lowering the threshold cannot silently leave
	// this test asserting inertness inside the live band.
	below := contextUsageBlendStartPercent - 0.1
	for _, percentage := range []float64{0, 1, below / 2, below, contextUsageBlendStartPercent} {
		payload := &KiroPayload{}
		observeKiroContextUsage(payload, "claude-opus-5", percentage)
		if got := claudePublicInputTokens(payload, "claude-opus-5", 1000); got != 1000 {
			t.Fatalf("percentage=%v public input tokens=%d, want the untouched client total 1000", percentage, got)
		}
	}
}

func TestContextUsageBlendRampsMonotonically(t *testing.T) {
	const model = "glm-5"
	const client = 101_000
	previous := 0
	for _, percentage := range []float64{55, 65, 75, 85, 95, 100} {
		payload := &KiroPayload{}
		observeKiroContextUsage(payload, model, percentage)
		got := claudePublicInputTokens(payload, model, client)
		occupied := currentKiroContextUsageTokens(payload, model)
		if got < previous {
			t.Fatalf("percentage=%v public input tokens=%d dropped below %d", percentage, got, previous)
		}
		if got > occupied {
			t.Fatalf("percentage=%v public input tokens=%d exceeded occupancy %d", percentage, got, occupied)
		}
		previous = got
	}
	// At full occupancy the client must see essentially the whole window, or it
	// keeps believing there is headroom and never compacts. The reported total
	// stops just short of the limit so the lift itself never reads as an overflow.
	if want := int(200_000 * contextUsageMaxWindowShare); previous != want {
		t.Fatalf("full occupancy public input tokens=%d, want %d", previous, want)
	}
}

// Easing in squared keeps the first steps past the threshold small enough that a
// user watching the token counter does not see a jump. Anchored just past the
// threshold rather than at a fixed percentage, since the band moves with it.
func TestContextUsageBlendEasesInGently(t *testing.T) {
	const client = 101_000
	payload := &KiroPayload{}
	observeKiroContextUsage(payload, "glm-5", contextUsageBlendStartPercent+2)
	got := claudePublicInputTokens(payload, "glm-5", client)
	if lift := got - client; lift > 1_000 {
		t.Fatalf("lift just past the threshold=%d tokens, want a barely visible nudge", lift)
	}
}

func TestContextUsageBlendHasNoComparabilityCliff(t *testing.T) {
	const model = "glm-5"
	payload := &KiroPayload{}
	observeKiroContextUsage(payload, model, 90)
	parityOccupancy := contextUsageTokens(model, contextUsageBlendParityPercent)
	boundary := int(float64(parityOccupancy) * contextUsageMinComparableShare)
	below := contextUsageBlendedInputTokens(payload, model, boundary-1)
	at := contextUsageBlendedInputTokens(payload, model, boundary)
	if jump := at - below; jump < 0 || jump > 16 {
		t.Fatalf("one-token comparability crossing jumped by %d tokens: below=%d at=%d", jump, below, at)
	}
}

func TestContextUsageBlendDoesNotDropNearFullWindow(t *testing.T) {
	const model = "glm-5"
	// This estimate is below the old moving 25% boundary at full occupancy.
	// With an occupancy-relative denominator, 90% -> 100% made public usage
	// decrease even though the upstream context had only become fuller.
	const client = 20_000
	previous := 0
	for percentage := 85.0; percentage <= 100; percentage++ {
		payload := &KiroPayload{}
		observeKiroContextUsage(payload, model, percentage)
		got := contextUsageBlendedInputTokens(payload, model, client)
		if got < previous {
			t.Fatalf("percentage=%v public input tokens=%d dropped below %d", percentage, got, previous)
		}
		previous = got
	}
}

func TestContextUsageBlendKeepsTinyUnrelatedEstimateStable(t *testing.T) {
	payload := &KiroPayload{}
	observeKiroContextUsage(payload, "claude-opus-5", 80)
	if got := contextUsageBlendedInputTokens(payload, "claude-opus-5", 80); got != 80 {
		t.Fatalf("tiny unrelated estimate=%d, want unchanged 80", got)
	}
}

// The whole point of the feature: once upstream is nearly full, the reported
// share must be close enough to the real share that the client compacts in time.
func TestContextUsageBlendTracksRealOccupancyShare(t *testing.T) {
	const model = "glm-5"
	window := getContextWindowSize(model)
	for _, percentage := range []float64{85, 90, 95, 100} {
		payload := &KiroPayload{}
		observeKiroContextUsage(payload, model, percentage)
		got := claudePublicInputTokens(payload, model, 101_000)
		share := 100 * float64(got) / float64(window)
		if share < percentage-15 {
			t.Fatalf("at %v%% occupancy the client sees only %.1f%%, too low to trigger compaction", percentage, share)
		}
	}
}

func TestContextUsageBlendNeverExceedsUpstreamOccupancy(t *testing.T) {
	payload := &KiroPayload{}
	observeKiroContextUsage(payload, "claude-opus-5", 90)
	occupied := currentKiroContextUsageTokens(payload, "claude-opus-5")
	if got := claudePublicInputTokens(payload, "claude-opus-5", occupied); got > occupied {
		t.Fatalf("public input tokens=%d exceeded reported occupancy %d", got, occupied)
	}
}

func TestContextUsageBlendWithoutObservationIsIdentity(t *testing.T) {
	if got := claudePublicInputTokens(&KiroPayload{}, "claude-opus-5", 4242); got != 4242 {
		t.Fatalf("public input tokens=%d without a contextUsageEvent, want 4242", got)
	}
	if got := claudePublicInputTokens(nil, "claude-opus-5", 4242); got != 4242 {
		t.Fatalf("nil payload public input tokens=%d, want 4242", got)
	}
}

// The lift is apportioned across the existing cache split so a blended request
// does not look like a cache miss to the client. Keeping it in the cache buckets
// is deliberate: cache reads bill at a fraction of uncached input, so tracking
// real occupancy costs the user very little.
func TestContextUsageBlendScalesCacheSplit(t *testing.T) {
	const model = "glm-5"
	const client = 100_000
	payload := &KiroPayload{}
	observeKiroContextUsage(payload, model, 100)

	usage := promptCacheUsage{
		CacheCreationInputTokens:   40_000,
		CacheReadInputTokens:       40_000,
		CacheCreation5mInputTokens: 30_000,
		CacheCreation1hInputTokens: 10_000,
	}
	profile := &promptCacheProfile{TotalInputTokens: client, Model: model}
	scaled := claudeTerminalCacheUsage(profile, payload, model, client, usage)

	// 100k client lifted to the 98% ceiling of a 200k window scales every bucket
	// by 196000/100000.
	if scaled.CacheReadInputTokens != 78_400 {
		t.Fatalf("cache read=%d, want 78400 after the lift to parity", scaled.CacheReadInputTokens)
	}
	if scaled.CacheCreationInputTokens != 78_400 {
		t.Fatalf("cache creation=%d, want 78400 after the lift to parity", scaled.CacheCreationInputTokens)
	}
	if scaled.CacheCreation5mInputTokens+scaled.CacheCreation1hInputTokens != scaled.CacheCreationInputTokens {
		t.Fatalf("TTL buckets do not sum to the creation total: %+v", scaled)
	}
}

func TestContextUsageBlendLeavesCacheSplitAloneBelowThreshold(t *testing.T) {
	payload := &KiroPayload{}
	observeKiroContextUsage(payload, "claude-opus-5", contextUsageBlendStartPercent-1)

	usage := promptCacheUsage{
		CacheCreationInputTokens:   4_000,
		CacheReadInputTokens:       4_000,
		CacheCreation5mInputTokens: 4_000,
	}
	profile := &promptCacheProfile{TotalInputTokens: 10_000, Model: "claude-opus-5"}
	if scaled := claudeTerminalCacheUsage(profile, payload, "claude-opus-5", 10_000, usage); scaled != usage {
		t.Fatalf("cache split=%+v changed below the blend threshold, want %+v", scaled, usage)
	}
}
