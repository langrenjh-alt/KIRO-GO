package proxy

import (
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
)

func TestFixedRatioSplitsEveryRequestWithoutCacheState(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	mode := config.CacheModeFixedRatio
	ratio := config.FixedRatioCacheConfig{CacheReadPercent: 80, CacheCreationPercent: 15}
	if err := config.UpdateCachePolicy(&mode, nil, &ratio); err != nil {
		t.Fatal(err)
	}
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 32)
	req := &ClaudeRequest{Model: "claude-opus-5.5", Messages: []ClaudeMessage{{Role: "user", Content: "hello"}}}
	profile := tracker.BuildClaudeProfile(req, 1000)
	if profile == nil || !profile.FixedRatioCache {
		t.Fatalf("profile = %#v", profile)
	}
	usage := tracker.Compute("", profile)
	if usage.CacheReadInputTokens != 800 || usage.CacheCreationInputTokens != 150 || usage.CacheCreation5mInputTokens != 150 || usage.CacheCreation1hInputTokens != 0 {
		t.Fatalf("usage = %+v", usage)
	}
	if billed := billedClaudeInputTokens(1000, usage); billed != 50 {
		t.Fatalf("billed input = %d", billed)
	}
	tracker.Update("account", profile)
	again := tracker.Compute("account", profile)
	if again != usage {
		t.Fatalf("second request = %+v, want the same split %+v", again, usage)
	}
	if stats := tracker.Stats(); stats.Entries != 0 || stats.FunctionEntries != 0 || stats.Hits != 0 {
		t.Fatalf("fixed ratio stored cache state: %+v", stats)
	}

	final := reanchorClaudeCacheUsage(profile, promptCacheUsage{}, 200)
	if final.CacheReadInputTokens != 160 || final.CacheCreationInputTokens != 30 {
		t.Fatalf("reanchored = %+v", final)
	}
	claude := buildClaudeUsageMap(200, 17, final, true)
	if claude["input_tokens"] != 10 || claude["output_tokens"] != 17 || claude["cache_read_input_tokens"] != 160 || claude["cache_creation_input_tokens"] != 30 {
		t.Fatalf("claude usage = %#v", claude)
	}
	openai := buildOpenAIUsageMap(200, 17, final)
	if openai["prompt_tokens"] != 200 || openai["completion_tokens"] != 17 {
		t.Fatalf("openai usage = %#v", openai)
	}
	details := openai["prompt_tokens_details"].(map[string]int)
	if details["cached_tokens"] != 160 {
		t.Fatalf("cached_tokens = %d", details["cached_tokens"])
	}
}

func TestFixedRatioScalesPercentagesThatExceedInput(t *testing.T) {
	usage := fixedRatioCacheUsage(1000, 80, 80)
	if usage.CacheReadInputTokens != 500 || usage.CacheCreationInputTokens != 500 {
		t.Fatalf("scaled usage = %+v", usage)
	}
	if billedClaudeInputTokens(1000, usage) != 0 {
		t.Fatal("scaled split left ordinary input")
	}
	zero := fixedRatioCacheUsage(1000, 0, 0)
	if zero.CacheReadInputTokens != 0 || zero.CacheCreationInputTokens != 0 || billedClaudeInputTokens(1000, zero) != 1000 {
		t.Fatalf("zero ratio = %+v", zero)
	}
}
