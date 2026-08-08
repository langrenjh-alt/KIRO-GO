package proxy

// claudePublicInputTokens returns the client-visible input total used by the
// Anthropic usage contract. Kiro's contextUsagePercentage includes Kiro-owned
// system instructions that are not present in the downstream request, so it
// must never replace this value.
func claudePublicInputTokens(payload *KiroPayload, model string, fallback int) int {
	return fallback
}

// claudeTerminalCacheUsage keeps the cache split in the same token domain as
// the prompt-cache tracker. Re-anchoring it to Kiro's context occupancy would
// scale client-visible cache tokens by hidden Kiro prompt tokens.
func claudeTerminalCacheUsage(profile *promptCacheProfile, payload *KiroPayload, model string, fallback int, usage promptCacheUsage) promptCacheUsage {
	return usage
}
