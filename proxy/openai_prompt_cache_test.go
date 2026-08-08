package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIChatPromptCacheUsesBoundPolicyAndReportsCachedTokens(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnText("ok", false)
		callback.OnComplete(4096, 1)
		return nil
	})
	t.Cleanup(h.stopPromptCachePolicyTrackers)

	nativePolicy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "openai-native",
		CacheMode:             config.CacheModeNative,
		PromptCacheMaxRatio:   0.25,
		PromptCacheMaxEntries: 256,
		PromptCacheTTLSeconds: 300,
	})
	if err != nil {
		t.Fatalf("add native policy: %v", err)
	}
	functionPolicy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "openai-function",
		CacheMode:             config.CacheModeFunctionSimulation,
		PromptCacheMaxRatio:   0.85,
		PromptCacheMaxEntries: 256,
		PromptCacheTTLSeconds: 300,
		FunctionCache: config.FunctionCacheConfig{
			Enabled:         true,
			SimulatedTokens: 2048,
			TTLSeconds:      300,
		},
	})
	if err != nil {
		t.Fatalf("add function policy: %v", err)
	}
	keyA, err := config.AddApiKey(config.ApiKeyEntry{
		Name: "openai-a", Key: "sk-openai-a", Enabled: true, PromptCachePolicyID: nativePolicy.ID,
	})
	if err != nil {
		t.Fatalf("add key A: %v", err)
	}
	keyB, err := config.AddApiKey(config.ApiKeyEntry{
		Name: "openai-b", Key: "sk-openai-b", Enabled: true, PromptCachePolicyID: functionPolicy.ID,
	})
	if err != nil {
		t.Fatalf("add key B: %v", err)
	}
	requireOpenAIAuth(t)

	longSystem := strings.Repeat("stable OpenAI cache prefix ", 320)
	nonStreamBody := marshalOpenAIChatCacheRequest(t, longSystem, false)
	first := serveOpenAIChatWithKey(t, h, "sk-openai-a", nonStreamBody)
	assertOpenAIChatPolicyHeaders(t, first, nativePolicy.ID)
	if got := openAIChatNonStreamCachedTokens(t, first); got != 0 {
		t.Fatalf("first native request cached_tokens=%d, want 0", got)
	}

	second := serveOpenAIChatWithKey(t, h, "sk-openai-a", nonStreamBody)
	if got := openAIChatNonStreamCachedTokens(t, second); got <= 0 || got > 4096 {
		t.Fatalf("second native request cached_tokens=%d, want 1..4096", got)
	}

	streamBody := marshalOpenAIChatCacheRequest(t, longSystem, true)
	streamed := serveOpenAIChatWithKey(t, h, "sk-openai-a", streamBody)
	if got := openAIChatStreamCachedTokens(t, streamed); got <= 0 || got > 4096 {
		t.Fatalf("stream request cached_tokens=%d, want 1..4096", got)
	}

	functionMiss := serveOpenAIChatWithKey(t, h, "sk-openai-b", nonStreamBody)
	assertOpenAIChatPolicyHeaders(t, functionMiss, functionPolicy.ID)
	if got := openAIChatNonStreamCachedTokens(t, functionMiss); got != 0 {
		t.Fatalf("first function-policy request cached_tokens=%d, want 0", got)
	}

	trackerA, _ := h.promptCacheRuntimeForAPIKey(keyA.ID)
	trackerB, _ := h.promptCacheRuntimeForAPIKey(keyB.ID)
	statsA, statsB := trackerA.Stats(), trackerB.Stats()
	if statsA.Misses != 1 || statsA.Hits != 2 || statsA.Entries == 0 {
		t.Fatalf("native policy stats=%+v, want one miss, two hits, and entries", statsA)
	}
	if statsB.Misses != 1 || statsB.Hits != 0 || statsB.FunctionEntries != 1 || statsB.Entries != 0 {
		t.Fatalf("function policy stats=%+v, want isolated function-cache miss", statsB)
	}
}

func TestOpenAIChatPromptCacheComputeRunsOnceAcrossRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				if calls == 1 {
					return fmt.Errorf("read Kiro event stream frame: %w", io.ErrUnexpectedEOF)
				}
				callback.OnText("recovered", false)
				callback.OnComplete(4096, 2)
				return nil
			})
			t.Cleanup(h.stopPromptCachePolicyTrackers)
			policy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
				Name:                  fmt.Sprintf("retry-%v", stream),
				CacheMode:             config.CacheModeNative,
				PromptCacheMaxRatio:   0.5,
				PromptCacheMaxEntries: 256,
				PromptCacheTTLSeconds: 300,
			})
			if err != nil {
				t.Fatalf("add policy: %v", err)
			}
			key, err := config.AddApiKey(config.ApiKeyEntry{
				Name: "retry-key", Key: "sk-openai-retry", Enabled: true, PromptCachePolicyID: policy.ID,
			})
			if err != nil {
				t.Fatalf("add key: %v", err)
			}
			requireOpenAIAuth(t)

			body := marshalOpenAIChatCacheRequest(t, strings.Repeat("retry cache prefix ", 400), stream)
			response := serveOpenAIChatWithKey(t, h, "sk-openai-retry", body)
			if response.Code != http.StatusOK || calls != 2 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
			}
			if stream {
				_ = openAIChatStreamCachedTokens(t, response)
			} else {
				_ = openAIChatNonStreamCachedTokens(t, response)
			}

			tracker, _ := h.promptCacheRuntimeForAPIKey(key.ID)
			stats := tracker.Stats()
			if stats.Misses != 1 || stats.Hits != 0 {
				t.Fatalf("retry counted cache more than once: %+v", stats)
			}
		})
	}
}

func requireOpenAIAuth(t *testing.T) {
	t.Helper()
	required := true
	if err := config.UpdateSettingsPatch(nil, &required, ""); err != nil {
		t.Fatalf("require API key: %v", err)
	}
}

func marshalOpenAIChatCacheRequest(t *testing.T, system string, stream bool) []byte {
	t.Helper()
	body := map[string]interface{}{
		"model": "claude-sonnet-4-6",
		"messages": []map[string]interface{}{
			{"role": "system", "content": system},
			{"role": "user", "content": "answer briefly"},
		},
		"stream": stream,
	}
	if stream {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return encoded
}

func serveOpenAIChatWithKey(t *testing.T, h *Handler, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	return recorder
}

func assertOpenAIChatPolicyHeaders(t *testing.T, response *httptest.ResponseRecorder, policyID string) {
	t.Helper()
	if got := response.Header().Get("X-Kiro-Api-Key-Matched"); got != "true" {
		t.Fatalf("X-Kiro-Api-Key-Matched=%q, want true", got)
	}
	if got := response.Header().Get("X-Kiro-Prompt-Cache-Policy"); got != policyID {
		t.Fatalf("X-Kiro-Prompt-Cache-Policy=%q, want %q", got, policyID)
	}
}

func openAIChatNonStreamCachedTokens(t *testing.T, response *httptest.ResponseRecorder) int {
	t.Helper()
	var envelope struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode non-stream response: %v body=%s", err, response.Body.String())
	}
	if envelope.Usage.TotalTokens != envelope.Usage.PromptTokens+envelope.Usage.CompletionTokens {
		t.Fatalf("inconsistent usage: %+v", envelope.Usage)
	}
	return envelope.Usage.PromptTokensDetails.CachedTokens
}

func openAIChatStreamCachedTokens(t *testing.T, response *httptest.ResponseRecorder) int {
	t.Helper()
	chunks := decodeOpenAIChatContractStream(t, response.Body.String())
	if len(chunks) == 0 {
		t.Fatal("stream returned no chunks")
	}
	usage := openAIChatContractMap(t, chunks[len(chunks)-1]["usage"], "stream usage")
	details := openAIChatContractMap(t, usage["prompt_tokens_details"], "stream prompt token details")
	cached, ok := details["cached_tokens"].(float64)
	if !ok {
		t.Fatalf("cached_tokens=%#v, want number", details["cached_tokens"])
	}
	return int(cached)
}
