package proxy

// Kiro reports contextUsagePercentage over its own occupancy, which includes
// Kiro-owned system instructions plus this proxy's injected protocol blocks.
// None of that is present in the downstream request, so it must not be billed
// to the client while the window is comfortably empty. Claude Code, however,
// decides when to compact from the standard usage input totals alone, so a
// permanently client-only number makes it compact too late: it sees headroom
// that upstream no longer has.
//
// The blend below resolves that by reporting the client-domain number until the
// real occupancy passes contextUsageBlendStartPercent, then easing the hidden
// overhead in as occupancy approaches the window. Below the threshold the
// client-visible bytes are unchanged, and any lift is bounded by the observed
// overhead, so this can never invent tokens beyond what upstream really holds.
// Parity lands at 90% rather than 100%: a client that only learns the truth at
// 100% has no room left to act on it. contextUsageMaxWindowShare then keeps the
// reported total just under the window so the lift can never itself look like an
// overflow to a client that validates against its own limit.
//
// The band is deliberately wide. An early, low threshold spreads the lift over
// many turns, which reads as a slowly rising counter rather than a jump.
// Starting at 28% keeps the ramp gradual across a much wider band, so the lift
// is spread over many turns instead of arriving as a step in the middle of a
// session. The quadratic first half means the early part of that band moves the
// reported total by a few hundred tokens at most.
const (
	contextUsageBlendStartPercent  = 28.0
	contextUsageBlendParityPercent = 90.0
	contextUsageMaxWindowShare     = 0.98
	contextUsageMinComparableShare = 0.25
)

// contextUsageBlendWeight ramps 0 -> 1 across the blend band. The first half of
// the band is eased in quadratically so a request that briefly crosses the
// threshold shows no visible jump, then the ramp straightens out and reaches
// parity by contextUsageBlendParityPercent. Staying quadratic all the way would
// still report only ~67% of the window at 85% real occupancy, which is below
// where clients decide to compact -- exactly the failure this is meant to avoid.
func contextUsageBlendWeight(percentage float64) float64 {
	if percentage <= contextUsageBlendStartPercent {
		return 0
	}
	if percentage >= contextUsageBlendParityPercent {
		return 1
	}
	span := contextUsageBlendParityPercent - contextUsageBlendStartPercent
	linear := (percentage - contextUsageBlendStartPercent) / span
	if linear < 0.5 {
		// Quadratic through (0.5, 0.25) so the two segments meet without a step.
		return linear * linear
	}
	// Straight line from (0.5, 0.25) to (1, 1).
	return 0.25 + (linear-0.5)*1.5
}

// contextUsageBlendedInputTokens is the shared blend used by both the public
// input total and the cache split so the two stay in one token domain.
//
// The lift target is the gap between Kiro's real occupancy and the client
// estimate, so at full occupancy the reported total matches what upstream
// actually holds. That gap is only a valid target because the weight gates it:
// applied unconditionally it would report an enormous phantom overhead on any
// multi-turn session, since Kiro's percentage spans the whole upstream
// conversation while the client total covers just this request. Below the
// threshold the weight is zero and the client number passes through untouched;
// past it the squared ramp eases the gap in, reaching parity only when upstream
// is genuinely full. Anything less lets Claude Code believe it has headroom
// upstream no longer has, which is what makes Kiro reject the next turn.
func contextUsageBlendedInputTokens(payload *KiroPayload, model string, clientInputTokens int) int {
	if payload == nil || clientInputTokens <= 0 {
		return clientInputTokens
	}
	weight := contextUsageBlendWeight(payload.maxContextUsagePercentage)
	if weight <= 0 {
		return clientInputTokens
	}
	occupied := currentKiroContextUsageTokens(payload, model)
	if occupied <= clientInputTokens {
		return clientInputTokens
	}
	// The gap is only meaningful when both numbers describe the same
	// conversation. A client estimate far below the reported occupancy means they
	// are not comparable -- a peak carried over from an earlier turn, or a tiny
	// follow-up on a reused payload -- and blending would inflate by orders of
	// magnitude. Pass the client number through instead.
	if float64(clientInputTokens) < float64(occupied)*contextUsageMinComparableShare {
		return clientInputTokens
	}
	// Cap the target below the window before weighting. Upstream occupancy counts
	// Kiro's own preamble, so a small request against a nearly full conversation
	// would otherwise be lifted by orders of magnitude.
	target := occupied
	if window := getContextWindowSize(model); window > 0 {
		if ceiling := int(float64(window) * contextUsageMaxWindowShare); target > ceiling {
			target = ceiling
		}
	}
	if target <= clientInputTokens {
		return clientInputTokens
	}
	lift := int(float64(target-clientInputTokens) * weight)
	if lift <= 0 {
		return clientInputTokens
	}
	if blended := clientInputTokens + lift; blended < target {
		return blended
	}
	return target
}

// claudePublicInputTokens returns the client-visible input total used by the
// Anthropic usage contract.
func claudePublicInputTokens(payload *KiroPayload, model string, fallback int) int {
	return contextUsageBlendedInputTokens(payload, model, fallback)
}

// claudeTerminalCacheUsage keeps the cache split in the same token domain as the
// blended input total. The prompt-cache tracker sized its split against the
// client-domain total, so the blended increment is apportioned by the existing
// read/creation shares rather than being appended as uncached tokens. Without
// this, a blend-lifted request would look like a sudden cache miss.
func claudeTerminalCacheUsage(profile *promptCacheProfile, payload *KiroPayload, model string, fallback int, usage promptCacheUsage) promptCacheUsage {
	if fallback <= 0 {
		return usage
	}
	blended := contextUsageBlendedInputTokens(payload, model, fallback)
	if blended <= fallback {
		return usage
	}
	tracked := usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	if tracked <= 0 {
		return usage
	}

	scale := float64(blended) / float64(fallback)
	scaled := usage
	scaled.CacheReadInputTokens = int(float64(usage.CacheReadInputTokens) * scale)
	scaled.CacheCreation5mInputTokens = int(float64(usage.CacheCreation5mInputTokens) * scale)
	scaled.CacheCreation1hInputTokens = int(float64(usage.CacheCreation1hInputTokens) * scale)
	// Keep the bucket split authoritative for the creation total so the two TTL
	// buckets always sum to it, matching the tracker's own invariant.
	scaled.CacheCreationInputTokens = scaled.CacheCreation5mInputTokens + scaled.CacheCreation1hInputTokens
	if scaled.CacheCreationInputTokens == 0 && usage.CacheCreationInputTokens > 0 {
		scaled.CacheCreationInputTokens = int(float64(usage.CacheCreationInputTokens) * scale)
	}

	// The cached portion can never exceed the reported input total.
	if total := scaled.CacheCreationInputTokens + scaled.CacheReadInputTokens; total > blended {
		return usage
	}
	return scaled
}
