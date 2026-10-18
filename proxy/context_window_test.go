package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetContextWindowSize verifies models are classified into the correct
// context window. This drives the input-token count that clients use to decide
// when to compact; misclassifying opus-4.8 (1M) as 200K under-reports tokens by
// 5x and prevents timely compaction.
func TestGetContextWindowSize(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-4.8", 1_000_000},
		{"claude-opus-4-8", 1_000_000},
		{"claude-opus-4.7", 1_000_000},
		{"claude-opus-4.6", 1_000_000},
		{"claude-sonnet-4.6", 1_000_000},
		{"claude-opus-4.8-thinking", 1_000_000},
		{"claude-fable-5", 1_000_000},
		{"claude-mythos-5.1", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-opus-5", 1_000_000},
		{"claude-opus-5.5", 1_000_000},
		{"claude-opus-5-5", 1_000_000},
		{"Claude-opus-5.5", 1_000_000},
		{"claude-opus-5-thinking", 1_000_000},
		{"CLAUDE-OPUS-4.8", 1_000_000},
		{"claude-opus-4.5", 200_000},
		{"claude-sonnet-4.5", 200_000},
		{"claude-sonnet-4", 200_000},
		{"claude-haiku-4.5", 200_000},
		{"claude-3-5-sonnet", 200_000},
		{"unknown-model", 200_000},
	}
	for _, c := range cases {
		if got := getContextWindowSize(c.model); got != c.want {
			t.Errorf("getContextWindowSize(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

func TestKiroContextUsageClampsTokensAtModelWindow(t *testing.T) {
	payload := &KiroPayload{}
	if got, exceeded := observeKiroContextUsage(payload, "claude-opus-5", 101); got != 1_000_000 || !exceeded {
		t.Fatalf("observed context usage tokens=%d exceeded=%v, want 1000000/true", got, exceeded)
	}
	usage := kiroContextUsage(payload, "claude-opus-5")
	assertKiroContextUsageValue(t, usage, 1_000_000, 1_000_000, 100, true)
}

func TestKiroContextUsageIsAbsentUntilUpstreamReportsIt(t *testing.T) {
	if got := kiroContextUsage(&KiroPayload{}, "claude-opus-5"); got != nil {
		t.Fatalf("kiro_context_usage=%+v, want nil before contextUsageEvent", got)
	}
}

func TestLiveModelTokenLimitsOverrideFallbackForAliases(t *testing.T) {
	const model = "claude-sonnet-4.5"
	key := canonicalModelLimitKey(model)
	liveModelTokenLimits.Lock()
	previous, existed := liveModelTokenLimits.byModel[key]
	liveModelTokenLimits.byModel[key] = modelTokenLimit{MaxInputTokens: 345_678, MaxOutputTokens: 12_345}
	liveModelTokenLimits.Unlock()
	t.Cleanup(func() {
		liveModelTokenLimits.Lock()
		defer liveModelTokenLimits.Unlock()
		if existed {
			liveModelTokenLimits.byModel[key] = previous
		} else {
			delete(liveModelTokenLimits.byModel, key)
		}
	})

	for _, alias := range []string{
		model,
		"claude-sonnet-4-5",
		"claude-sonnet-4.5-thinking",
		"claude-3-5-sonnet",
		"gpt-4o",
	} {
		if got := getContextWindowSize(alias); got != 345_678 {
			t.Errorf("getContextWindowSize(%q) = %d, want live limit %d", alias, got, 345_678)
		}
	}
	limits := lookupModelTokenLimits("gpt-4")
	if limits.MaxOutputTokens != 12_345 {
		t.Fatalf("alias max_output_tokens = %d, want %d", limits.MaxOutputTokens, 12_345)
	}
}

func TestObservedNonClaudeModelLimitFallbacks(t *testing.T) {
	cases := []struct {
		model       string
		inputLimit  int
		outputLimit int
	}{
		{model: "auto", inputLimit: 1_000_000, outputLimit: 64_000},
		{model: "claude-haiku-4.5", inputLimit: 200_000, outputLimit: 64_000},
		{model: "claude-opus-4.5", inputLimit: 200_000, outputLimit: 64_000},
		{model: "claude-opus-4.6", inputLimit: 1_000_000, outputLimit: 64_000},
		{model: "claude-opus-4.7", inputLimit: 1_000_000, outputLimit: 128_000},
		{model: "claude-opus-4.8", inputLimit: 1_000_000, outputLimit: 128_000},
		{model: "claude-opus-5", inputLimit: 1_000_000, outputLimit: 128_000},
		{model: "claude-opus-5.5", inputLimit: 1_000_000, outputLimit: 128_000},
		{model: "Claude-opus-5-5", inputLimit: 1_000_000, outputLimit: 128_000},
		{model: "claude-sonnet-4", inputLimit: 200_000, outputLimit: 64_000},
		{model: "claude-sonnet-4.5", inputLimit: 200_000, outputLimit: 64_000},
		{model: "claude-sonnet-4.6", inputLimit: 1_000_000, outputLimit: 64_000},
		{model: "claude-sonnet-5", inputLimit: 1_000_000, outputLimit: 64_000},
		{model: "deepseek-3.2", inputLimit: 164_000, outputLimit: 64_000},
		{model: "glm-5", inputLimit: 200_000, outputLimit: 64_000},
		{model: "gpt-5.6-luna", inputLimit: 272_000, outputLimit: 128_000},
		{model: "gpt-5.6-sol", inputLimit: 272_000, outputLimit: 128_000},
		{model: "gpt-5.6-terra", inputLimit: 272_000, outputLimit: 128_000},
		{model: "minimax-m2.1", inputLimit: 196_000, outputLimit: 64_000},
		{model: "minimax-m2.5", inputLimit: 196_000, outputLimit: 64_000},
		{model: "qwen3-coder-next", inputLimit: 256_000, outputLimit: 64_000},
	}
	for _, tc := range cases {
		limits := fallbackModelTokenLimits(tc.model)
		if limits.MaxInputTokens != tc.inputLimit || limits.MaxOutputTokens != tc.outputLimit {
			t.Errorf("fallbackModelTokenLimits(%q) = %#v, want input=%d output=%d", tc.model, limits, tc.inputLimit, tc.outputLimit)
		}
	}
}

func TestBuildModelInfoIncludesTokenLimits(t *testing.T) {
	const model = "unit-test-live-model"
	key := canonicalModelLimitKey(model)
	liveModelTokenLimits.Lock()
	liveModelTokenLimits.byModel[key] = modelTokenLimit{MaxInputTokens: 321_000, MaxOutputTokens: 45_000}
	liveModelTokenLimits.Unlock()
	t.Cleanup(func() {
		liveModelTokenLimits.Lock()
		delete(liveModelTokenLimits.byModel, key)
		liveModelTokenLimits.Unlock()
	})

	info := buildModelInfo(model, "anthropic", true)
	if info["context_window"] != 321_000 || info["max_input_tokens"] != 321_000 || info["max_output_tokens"] != 45_000 {
		t.Fatalf("model token metadata = %#v", info)
	}
	thinkingInfo := buildThinkingModelInfo(model, "-custom-thinking", "anthropic", true)
	if thinkingInfo["context_window"] != 321_000 || thinkingInfo["max_output_tokens"] != 45_000 {
		t.Fatalf("custom thinking model token metadata = %#v", thinkingInfo)
	}

	h, cleanup := setupResponsesTestHandler(t)
	t.Cleanup(cleanup)
	h.cachedModels = []ModelInfo{{ModelId: model, InputTypes: []string{"text"}}}
	recorder := httptest.NewRecorder()
	h.handleModels(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/v1/models status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	for _, listed := range response.Data {
		if listed["id"] != model {
			continue
		}
		if listed["context_window"] != float64(321_000) || listed["max_input_tokens"] != float64(321_000) || listed["max_output_tokens"] != float64(45_000) {
			t.Fatalf("/v1/models token metadata = %#v", listed)
		}
		return
	}
	t.Fatalf("/v1/models did not include %q: %#v", model, response.Data)
}
