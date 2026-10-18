package proxy

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

func TestBillingInputCapOrderBoundariesAndOutput(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		input                                        int
		percent                                      float64
		wantInput, wantRead, wantCreation, wantTotal int
	}{
		{"previous_100k_example", 100_000, 40, 14_000, 112_000, 14_000, 140_000},
		{"cap_after_40_percent", 1_000_000, 40, 100_000, 800_000, 100_000, 1_000_000},
		{"exact_limit_zero_percent", 1_000_000, 0, 100_000, 800_000, 100_000, 1_000_000},
		{"cap_even_zero_percent", 2_000_000, 0, 100_000, 800_000, 100_000, 1_000_000},
		{"maximum_inflation", 1_000_000, 99999, 100_000, 800_000, 100_000, 1_000_000},
		{"zero_input", 0, 40, 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupBillingInflation(t, tc.percent)
			cache := fixedRatioCacheUsage(tc.input, 80, 10)
			before := cache
			usage := inflatedBillingUsage(tc.input, 2_000_000, cache)
			wantOutput := inflateBillingTokensWithPercent(2_000_000, tc.percent)
			if usage.InputTokens != tc.wantInput || usage.CacheReadInputTokens != tc.wantRead || usage.CacheCreationInputTokens != tc.wantCreation || usage.OutputTokens != wantOutput {
				t.Fatalf("usage=%+v want input/read/creation/output=%d/%d/%d/%d", usage, tc.wantInput, tc.wantRead, tc.wantCreation, wantOutput)
			}
			for _, m := range []map[string]interface{}{buildClaudeUsageMap(tc.input, 2_000_000, cache, true), buildClaudeTerminalUsageMap(tc.input, 2_000_000, cache)} {
				if m["input_tokens"] != tc.wantInput || m["cache_creation_input_tokens"] != tc.wantCreation || m["cache_read_input_tokens"] != tc.wantRead || m["output_tokens"] != wantOutput {
					t.Fatalf("Claude map=%+v", m)
				}
			}
			chat := buildOpenAIUsageMap(tc.input, 2_000_000, cache)
			if chat["prompt_tokens"] != tc.wantTotal || chat["completion_tokens"] != wantOutput || chat["total_tokens"] != tc.wantTotal+wantOutput || chat["prompt_tokens_details"].(map[string]int)["cached_tokens"] != tc.wantRead {
				t.Fatalf("Chat usage=%+v", chat)
			}
			response := &ResponsesObject{Usage: ResponsesUsage{InputTokens: tc.input, OutputTokens: 2_000_000, TotalTokens: tc.input + 2_000_000}}
			applyResponsesPromptCacheUsage(response, cache)
			if response.Usage.InputTokens != tc.wantTotal || response.Usage.OutputTokens != wantOutput || response.Usage.InputTokensDetails.CachedTokens != tc.wantRead || response.Usage.TotalTokens != tc.wantTotal+wantOutput {
				t.Fatalf("Responses usage=%+v", response.Usage)
			}
			if before != cache {
				t.Fatal("cap mutated the cache policy")
			}
		})
	}
	for _, n := range []int{999_999, 1_000_000, 1_000_001} {
		u := inflatedBillingUsageWithPercent(n, 2_000_000, promptCacheUsage{}, 0)
		if u.InputTokens != minInt(n, 1_000_000) || u.OutputTokens != 2_000_000 {
			t.Fatalf("boundary %d: %+v", n, u)
		}
	}
}

func TestBillingInputCapRoundingAndOverflow(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	max := int(^uint(0) >> 1)
	for i := 0; i < 2000; i++ {
		counts := [3]int{rng.Intn(max), rng.Intn(max), rng.Intn(max)}
		u := ClaudeUsage{InputTokens: counts[0], CacheCreationInputTokens: counts[1], CacheReadInputTokens: counts[2], OutputTokens: 2_000_000}
		capBillingInputUsage(&u)
		if u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens != 1_000_000 || u.OutputTokens != 2_000_000 {
			t.Fatalf("capped=%+v", u)
		}
		total := float64(counts[0]) + float64(counts[1]) + float64(counts[2])
		for index, value := range []int{u.InputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens} {
			if math.Abs(float64(value)-float64(counts[index])/total*1_000_000) > 1.000001 || value < 0 {
				t.Fatalf("proportions changed: %v -> %+v", counts, u)
			}
		}
	}
	u := ClaudeUsage{InputTokens: 1_000_001, CacheCreationInputTokens: 1_000_001, CacheReadInputTokens: 1_000_001}
	capBillingInputUsage(&u)
	if u.InputTokens != 333_334 || u.CacheCreationInputTokens != 333_333 || u.CacheReadInputTokens != 333_333 {
		t.Fatalf("rounding=%+v", u)
	}
}

func TestBillingInputCapDetailsAndAccounting(t *testing.T) {
	setupBillingInflation(t, 40)
	u := ClaudeUsage{InputTokens: 100_000, CacheCreationInputTokens: 100_000, CacheReadInputTokens: 800_000, OutputTokens: 2_000_000,
		CacheCreation:       &ClaudeCacheCreationUsage{Ephemeral5mInputTokens: 25_000, Ephemeral1hInputTokens: 75_000},
		OutputTokensDetails: &ClaudeOutputTokensDetails{ThinkingTokens: 1_200_000},
		KiroContextUsage:    &KiroContextUsage{InputTokens: 1_100_000}}
	inflateClaudeUsage(&u)
	if u.CacheCreation.Ephemeral5mInputTokens != 25_000 || u.CacheCreation.Ephemeral1hInputTokens != 75_000 || u.OutputTokensDetails.ThinkingTokens != 1_680_000 || u.KiroContextUsage.InputTokens != 1_100_000 {
		t.Fatalf("details=%+v", u)
	}
	key, err := config.AddApiKey(config.ApiKeyEntry{Name: "cap", Key: "sk-cap-test", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{traffic: newTrafficTracker(time.Hour, time.Now)}
	h.recordSuccessForApiKeyWithCache(key.ID, 1_000_000, 2_000_000, 1.25, fixedRatioCacheUsage(1_000_000, 80, 10))
	if atomic.LoadInt64(&h.totalTokens) != 3_800_000 || config.GetApiKeyEntry(key.ID).TokensUsed != 3_800_000 || config.GetApiKeyEntry(key.ID).CreditsUsed != 1.25 {
		t.Fatal("accounting cap/output mismatch")
	}
	want := TrafficCounters{Requests: 1, InputTokens: 100_000, CacheCreationTokens: 100_000, CacheReadTokens: 800_000, OutputTokens: 2_800_000}
	if got := h.traffic.Query(time.Hour).Totals; got != want {
		t.Fatalf("traffic=%+v", got)
	}
}

func TestBillingInputCapBufferedStream(t *testing.T) {
	for _, percent := range []float64{0, 40} {
		for _, context := range []bool{false, true} {
			t.Run(fmt.Sprintf("percent=%g/context=%v", percent, context), func(t *testing.T) {
				setupBillingInflation(t, percent)
				contextField := ""
				if context {
					contextField = `,"kiro_context_usage":{"input_tokens":2000000}`
				}
				body := []byte("event: message_start\n" + `data: {"type":"message_start","message":{"usage":{"input_tokens":200000,"output_tokens":1,"cache_creation_input_tokens":200000,"cache_read_input_tokens":1600000,"cache_creation":{"ephemeral_5m_input_tokens":50000,"ephemeral_1h_input_tokens":150000}}}}` + "\n\n" +
					"event: message_delta\n" + `data: {"type":"message_delta","usage":{"input_tokens":200000,"output_tokens":2000000,"cache_creation_input_tokens":200000,"cache_read_input_tokens":1600000` + contextField + `}}` + "\n\n")
				patched := patchClaudeCodeUsageTotals(body)
				for _, event := range decodeResponseContractSSE(t, string(patched)) {
					obj := event.Data
					if event.Name == "message_start" {
						obj = responseContractMap(t, obj["message"], "message")
					}
					u := responseContractMap(t, obj["usage"], "usage")
					if u["input_tokens"] != float64(100_000) || u["cache_creation_input_tokens"] != float64(100_000) || u["cache_read_input_tokens"] != float64(800_000) {
						t.Fatalf("usage=%v", u)
					}
					if event.Name == "message_delta" && u["output_tokens"] != float64(inflateBillingTokensWithPercent(2_000_000, percent)) {
						t.Fatalf("output=%v", u["output_tokens"])
					}
					if event.Name == "message_start" {
						c := responseContractMap(t, u["cache_creation"], "cache_creation")
						if c["ephemeral_5m_input_tokens"] != float64(25_000) || c["ephemeral_1h_input_tokens"] != float64(75_000) {
							t.Fatalf("cache detail=%v", c)
						}
					}
				}
			})
		}
	}
}

func TestBillingInputCapProtocolRoutes(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/cc/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				h, cleanup := setupResponsesTestHandler(t)
				defer cleanup()
				defer h.stopPromptCachePolicyTrackers()
				defer func() { _ = config.UpdateTokenBillingInflationPercent(0) }()
				answer := strings.Repeat("Generated output remains uncapped. ", 400)
				h.kiroCaller = func(_ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
					cb.OnText(answer, false)
					cb.OnContextUsage(12.5)
					cb.OnComplete(500, 8)
					return nil
				}
				mode := config.CacheModeFixedRatio
				ratio := config.FixedRatioCacheConfig{CacheReadPercent: 80, CacheCreationPercent: 10}
				if err := config.UpdateCachePolicy(&mode, nil, &ratio); err != nil {
					t.Fatal(err)
				}
				request := map[string]interface{}{"model": "claude-opus-5", "max_tokens": 20000, "stream": stream}
				prompt := strings.Repeat("Client input cap test. ", 400)
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
				setBillingInflation(t, 99999)
				got := run()
				inputField, outputField := "input_tokens", "output_tokens"
				if path == "/v1/chat/completions" {
					inputField = "prompt_tokens"
					outputField = "completion_tokens"
				}
				input := got[inputField].(float64)
				for _, field := range []string{"cache_creation_input_tokens", "cache_read_input_tokens"} {
					if n, ok := got[field].(float64); ok {
						input += n
					}
				}
				if input != 1_000_000 {
					t.Fatalf("input total=%v usage=%v", input, got)
				}
				wantOutput := inflateBillingTokensWithPercent(int(base[outputField].(float64)), 99999)
				if wantOutput <= 1_000_000 || got[outputField] != float64(wantOutput) {
					t.Fatalf("output=%v want=%d >1M", got[outputField], wantOutput)
				}
				if total, ok := got["total_tokens"].(float64); ok && total != input+float64(wantOutput) {
					t.Fatalf("total=%v", total)
				}
				for _, field := range []string{"input_tokens_details", "prompt_tokens_details"} {
					if details, ok := got[field].(map[string]interface{}); ok && details["cached_tokens"].(float64) > input {
						t.Fatalf("cache exceeds input: %v", details)
					}
				}
				if !reflect.DeepEqual(base["kiro_context_usage"], got["kiro_context_usage"]) {
					t.Fatal("real context changed")
				}
			})
		}
	}
}
