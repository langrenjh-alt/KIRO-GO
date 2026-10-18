package proxy

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

func setupBillingInflation(t *testing.T, percent float64) {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.Close() })
	setBillingInflation(t, percent)
}

func setBillingInflation(t *testing.T, percent float64) {
	t.Helper()
	if err := config.UpdateTokenBillingInflationPercent(percent); err != nil {
		t.Fatal(err)
	}
}

func TestTokenBillingInflationArithmetic(t *testing.T) {
	for _, test := range []struct {
		tokens  int
		percent float64
		want    int
	}{
		{100, 0, 100}, {100, 50, 150}, {100, 100, 200},
		{100, 99999, 100099}, {100, 0.5, 101}, {3, 50, 5},
		{8, 12.5, 9}, {0, 99999, 0},
	} {
		if got := inflateBillingTokensWithPercent(test.tokens, test.percent); got != test.want {
			t.Errorf("tokens=%d percent=%g: got %d want %d", test.tokens, test.percent, got, test.want)
		}
	}
	max := int(^uint(0) >> 1)
	if got := inflateBillingTokensWithPercent(max, 99999); got != max {
		t.Fatalf("overflow: got %d want %d", got, max)
	}
}

func TestTokenBillingInflationAfterCacheSplit(t *testing.T) {
	setupBillingInflation(t, 50)
	// The raw cache policy produces 1 ordinary, 1 creation, 1 read token.
	// Each becomes 2 tokens; scaling 3 before the split would produce only 5.
	cache := fixedRatioCacheUsage(3, 33, 33)
	before := cache
	claude := buildClaudeUsageMap(3, 3, cache, true)
	terminal := buildClaudeTerminalUsageMap(3, 3, cache)
	for _, usage := range []map[string]interface{}{claude, terminal} {
		for _, field := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
			if usage[field] != 2 {
				t.Errorf("%s=%v, want 2", field, usage[field])
			}
		}
		if usage["output_tokens"] != 5 {
			t.Errorf("output=%v, want 5", usage["output_tokens"])
		}
	}
	chat := buildOpenAIUsageMap(3, 3, cache)
	if chat["prompt_tokens"] != 6 || chat["completion_tokens"] != 5 || chat["total_tokens"] != 11 || chat["prompt_tokens_details"].(map[string]int)["cached_tokens"] != 2 {
		t.Fatalf("chat usage=%v", chat)
	}
	response := &ResponsesObject{Usage: ResponsesUsage{InputTokens: 3, OutputTokens: 3, TotalTokens: 6}}
	applyResponsesPromptCacheUsage(response, cache)
	if response.Usage.InputTokens != 6 || response.Usage.OutputTokens != 5 || response.Usage.TotalTokens != 11 || response.Usage.InputTokensDetails.CachedTokens != 2 {
		t.Fatalf("Responses usage=%+v", response.Usage)
	}
	if cache != before {
		t.Fatal("billing changed the cache policy's raw tokens")
	}
}

func TestTokenBillingInflationNestedDetailsAndContext(t *testing.T) {
	setupBillingInflation(t, 50)
	usage := ClaudeUsage{
		InputTokens: 10, OutputTokens: 6, CacheCreationInputTokens: 2, CacheReadInputTokens: 8,
		CacheCreation:       &ClaudeCacheCreationUsage{Ephemeral5mInputTokens: 1, Ephemeral1hInputTokens: 1},
		OutputTokensDetails: &ClaudeOutputTokensDetails{ThinkingTokens: 2},
		KiroContextUsage:    &KiroContextUsage{InputTokens: 200},
	}
	inflateClaudeUsage(&usage)
	if usage.InputTokens != 15 || usage.OutputTokens != 9 || usage.CacheCreationInputTokens != 3 || usage.CacheReadInputTokens != 12 {
		t.Fatalf("usage=%+v", usage)
	}
	if usage.CacheCreation.Ephemeral5mInputTokens+usage.CacheCreation.Ephemeral1hInputTokens != 3 || usage.OutputTokensDetails.ThinkingTokens != 3 || usage.KiroContextUsage.InputTokens != 200 {
		t.Fatalf("nested details or raw context changed incorrectly: %+v", usage)
	}
}

func TestTokenBillingInflationAccounting(t *testing.T) {
	setupBillingInflation(t, 50)
	key, err := config.AddApiKey(config.ApiKeyEntry{Name: "billing", Key: "sk-billing-test", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{traffic: newTrafficTracker(time.Hour, time.Now)}
	h.recordSuccessForApiKeyWithCache(key.ID, 3, 3, 1.25, fixedRatioCacheUsage(3, 33, 33))
	if got := atomic.LoadInt64(&h.totalTokens); got != 11 {
		t.Fatalf("total tokens=%d, want 11", got)
	}
	got := h.traffic.Query(time.Hour).Totals
	want := TrafficCounters{Requests: 1, InputTokens: 2, OutputTokens: 5, CacheCreationTokens: 2, CacheReadTokens: 2}
	if got != want {
		t.Fatalf("traffic=%+v, want %+v", got, want)
	}
	if entry := config.GetApiKeyEntry(key.ID); entry.TokensUsed != 11 || entry.CreditsUsed != 1.25 {
		t.Fatalf("key tokens=%d credits=%g", entry.TokensUsed, entry.CreditsUsed)
	}
	var retries retryBillingUsage
	retries.add(newDiscardedAttemptUsage(3, 3, 0.5))
	retries.recordForAPIKey(key.ID)
	if entry := config.GetApiKeyEntry(key.ID); entry.TokensUsed != 21 || entry.CreditsUsed != 1.75 {
		t.Fatalf("retry key tokens=%d credits=%g", entry.TokensUsed, entry.CreditsUsed)
	}
}

func TestTokenBillingInflationSettingsAPI(t *testing.T) {
	setupBillingInflation(t, 0)
	h := &Handler{}
	for _, percent := range []float64{0, 0.5, 100, 99999, 0} {
		rec := httptest.NewRecorder()
		h.apiUpdateSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(fmt.Sprintf(`{"tokenBillingInflationPercent":%g}`, percent))))
		if rec.Code != 200 {
			t.Fatalf("POST %g: %d %s", percent, rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		h.apiGetSettings(rec, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))
		var settings map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil {
			t.Fatal(err)
		}
		if settings["tokenBillingInflationPercent"] != percent {
			t.Fatalf("GET inflation=%v want %g", settings["tokenBillingInflationPercent"], percent)
		}
	}
	setBillingInflation(t, 12.5)
	for _, value := range []string{"-1", "100000", "1e999", `"bad"`} {
		rec := httptest.NewRecorder()
		h.apiUpdateSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(`{"tokenBillingInflationPercent":`+value+`,"requireApiKey":true}`)))
		if rec.Code != 400 || config.GetTokenBillingInflationPercent() != 12.5 || config.IsApiKeyRequired() {
			t.Fatalf("invalid %s: status=%d, inflation=%g, requireKey=%v", value, rec.Code, config.GetTokenBillingInflationPercent(), config.IsApiKeyRequired())
		}
	}
	rec := httptest.NewRecorder()
	h.apiUpdateSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(`{"requireApiKey":false}`)))
	if rec.Code != 200 || config.GetTokenBillingInflationPercent() != 12.5 {
		t.Fatal("partial settings update lost inflation")
	}
}

func TestTokenBillingInflationProtocolRoutes(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/cc/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				h, cleanup := setupResponsesTestHandler(t)
				defer cleanup()
				defer h.stopPromptCachePolicyTrackers()
				defer func() { _ = config.UpdateTokenBillingInflationPercent(0) }()
				h.kiroCaller = func(_ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
					cb.OnText("A deterministic billing response.", false)
					cb.OnContextUsage(12.5)
					cb.OnComplete(500, 8)
					return nil
				}
				mode := config.CacheModeFixedRatio
				ratio := config.FixedRatioCacheConfig{CacheReadPercent: 50, CacheCreationPercent: 30}
				if err := config.UpdateCachePolicy(&mode, nil, &ratio); err != nil {
					t.Fatal(err)
				}
				prompt := strings.Repeat("billing cache prompt ", 80)
				request := map[string]interface{}{"model": "claude-opus-5", "max_tokens": 64, "stream": stream}
				if path == "/v1/responses" {
					request["input"] = prompt
					request["store"] = false
				} else {
					request["messages"] = []map[string]string{{"role": "user", "content": prompt}}
				}
				if path == "/v1/chat/completions" && stream {
					request["stream_options"] = map[string]bool{"include_usage": true}
				}
				body, _ := json.Marshal(request)
				run := func() map[string]interface{} {
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body))))
					if rec.Code != 200 {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					return billingProtocolUsage(t, rec.Body.String(), stream)
				}
				base := run()
				setBillingInflation(t, 100)
				inflated := run()
				for _, field := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "prompt_tokens", "completion_tokens", "total_tokens"} {
					if raw, ok := base[field].(float64); ok && inflated[field] != raw*2 {
						t.Errorf("%s=%v want %g (raw=%g)", field, inflated[field], raw*2, raw)
					}
				}
				for _, detail := range []string{"input_tokens_details", "prompt_tokens_details"} {
					if raw, ok := base[detail].(map[string]interface{}); ok {
						got := inflated[detail].(map[string]interface{})
						if got["cached_tokens"] != raw["cached_tokens"].(float64)*2 {
							t.Errorf("cached tokens=%v raw=%v", got, raw)
						}
					}
				}
				if !reflect.DeepEqual(base["kiro_context_usage"], inflated["kiro_context_usage"]) {
					t.Fatal("billing inflation changed real context usage")
				}
				setBillingInflation(t, 0)
				if restored := run(); !reflect.DeepEqual(base, restored) {
					t.Fatalf("0%% did not restore usage: base=%v restored=%v", base, restored)
				}
			})
		}
	}
}

func billingProtocolUsage(t *testing.T, body string, stream bool) map[string]interface{} {
	t.Helper()
	var usage map[string]interface{}
	consume := func(data string) {
		var obj map[string]interface{}
		if json.Unmarshal([]byte(data), &obj) != nil {
			return
		}
		if response, ok := obj["response"].(map[string]interface{}); ok {
			obj = response
		}
		if u, ok := obj["usage"].(map[string]interface{}); ok {
			usage = u
		}
	}
	if stream {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "data: ") {
				consume(strings.TrimPrefix(line, "data: "))
			}
		}
	} else {
		consume(body)
	}
	if usage == nil {
		t.Fatalf("usage missing in %s", body)
	}
	return usage
}

func TestTokenBillingInflationBufferedRoundingAfterContext(t *testing.T) {
	setupBillingInflation(t, 50)
	body := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":99,"output_tokens":1,"cache_creation_input_tokens":1,"cache_read_input_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":0}}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":99,"output_tokens":3,"cache_creation_input_tokens":1,"cache_read_input_tokens":1,"kiro_context_usage":{"input_tokens":3}}}` + "\n\n")
	patched := patchClaudeCodeUsageTotals(body)
	for _, event := range decodeResponseContractSSE(t, string(patched)) {
		obj := event.Data
		if event.Name == "message_start" {
			obj = responseContractMap(t, obj["message"], "message")
		}
		usage := responseContractMap(t, obj["usage"], "usage")
		for _, field := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
			if usage[field] != float64(2) {
				t.Fatalf("%s %s=%v want 2 (one inflation after correction)", event.Name, field, usage[field])
			}
		}
	}
	usage := billingProtocolUsage(t, string(patched), true)
	if usage["output_tokens"] != math.Round(3*1.5) {
		t.Fatalf("output=%v want 5", usage["output_tokens"])
	}
}
