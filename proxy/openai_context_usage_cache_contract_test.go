package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const contextUsageContractModel = "claude-opus-5"

func TestOpenAIChatUsageSeparatesClientCacheFromKiroContext(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h := newEmptyVisibleOutputTestHandler(t, 1, contextUsageContractCaller)
			t.Cleanup(h.stopPromptCachePolicyTrackers)
			key := addContextUsageCacheBinding(t, fmt.Sprintf("chat-context-%v", stream), fmt.Sprintf("sk-chat-context-%v", stream))
			requireOpenAIAuth(t)

			prompt := strings.Repeat("client visible chat cache prompt ", 64)
			body := marshalContextUsageChatRequest(t, prompt, stream)
			cold := serveOpenAIChatWithKey(t, h, key.Key, body)
			hit := serveOpenAIChatWithKey(t, h, key.Key, body)
			coldUsage := contextUsageChatUsage(t, cold, stream)
			hitUsage := contextUsageChatUsage(t, hit, stream)

			expectedInput := estimateOpenAIRequestInputTokens(&OpenAIRequest{
				Model:    contextUsageContractModel,
				Messages: []OpenAIMessage{{Role: "user", Content: prompt}},
			})
			assertContextUsageInputAndCache(t, coldUsage, "prompt_tokens", "prompt_tokens_details", expectedInput, false)
			assertContextUsageInputAndCache(t, hitUsage, "prompt_tokens", "prompt_tokens_details", expectedInput, true)
			assertOpenAIKiroContextUsageMap(t, hitUsage)

			completion := contextUsageNumber(t, hitUsage, "completion_tokens")
			total := contextUsageNumber(t, hitUsage, "total_tokens")
			if total != float64(expectedInput)+completion {
				t.Fatalf("Chat total_tokens=%v, want prompt(%d)+completion(%v)", total, expectedInput, completion)
			}
		})
	}
}

func TestResponsesUsageSeparatesClientCacheFromKiroContext(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h, cleanup := setupResponsesTestHandler(t)
			defer cleanup()
			defer h.stopPromptCachePolicyTrackers()
			h.kiroCaller = contextUsageContractCaller
			key := addContextUsageCacheBinding(t, fmt.Sprintf("responses-context-%v", stream), fmt.Sprintf("sk-responses-context-%v", stream))

			prompt := strings.Repeat("client visible responses cache prompt ", 64)
			cold := runContextUsageResponsesRequest(t, h, key, prompt, stream)
			hit := runContextUsageResponsesRequest(t, h, key, prompt, stream)
			expectedInput := estimateOpenAIRequestInputTokens(&OpenAIRequest{
				Model:    contextUsageContractModel,
				Messages: []OpenAIMessage{{Role: "user", Content: prompt}},
			})

			if cold.Usage.InputTokens != expectedInput || hit.Usage.InputTokens != expectedInput {
				t.Fatalf("Responses input_tokens cold=%d hit=%d, want client estimate %d", cold.Usage.InputTokens, hit.Usage.InputTokens, expectedInput)
			}
			if cold.Usage.InputTokensDetails.CachedTokens != 0 {
				t.Fatalf("cold Responses cached_tokens=%d, want 0", cold.Usage.InputTokensDetails.CachedTokens)
			}
			cached := hit.Usage.InputTokensDetails.CachedTokens
			if cached <= 0 || cached > hit.Usage.InputTokens {
				t.Fatalf("hit Responses cached_tokens=%d, want a positive subset of input_tokens=%d", cached, hit.Usage.InputTokens)
			}
			if hit.Usage.TotalTokens != hit.Usage.InputTokens+hit.Usage.OutputTokens {
				t.Fatalf("Responses total_tokens=%d, want input(%d)+output(%d)", hit.Usage.TotalTokens, hit.Usage.InputTokens, hit.Usage.OutputTokens)
			}
			assertKiroContextUsageStruct(t, hit.Usage.KiroContextUsage)
		})
	}
}

func contextUsageContractCaller(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
	callback.OnContextUsage(12.5)
	callback.OnText("cache contract response", false)
	// Public usage must remain anchored to the client request, not these
	// upstream counters or Kiro's hidden context occupancy.
	callback.OnComplete(987_654, 321)
	return nil
}

func addContextUsageCacheBinding(t *testing.T, name, keyValue string) config.ApiKeyEntry {
	t.Helper()
	hitRate, maxRead := 100, 50
	policy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  name,
		CacheMode:             config.CacheModeFunctionSimulation,
		PromptCacheMaxRatio:   0.9,
		PromptCacheMaxEntries: 256,
		PromptCacheTTLSeconds: config.DefaultPromptCacheTTLSeconds,
		FunctionCache: config.FunctionCacheConfig{
			Enabled:             true,
			SimulatedTokens:     1000,
			TTLSeconds:          config.DefaultPromptCacheTTLSeconds,
			HitRatePercent:      &hitRate,
			MaxCacheReadPercent: &maxRead,
		},
	})
	if err != nil {
		t.Fatalf("add prompt-cache policy: %v", err)
	}
	key, err := config.AddApiKey(config.ApiKeyEntry{
		Name:                name,
		Key:                 keyValue,
		Enabled:             true,
		PromptCachePolicyID: policy.ID,
	})
	if err != nil {
		t.Fatalf("add API key: %v", err)
	}
	return key
}

func marshalContextUsageChatRequest(t *testing.T, prompt string, stream bool) []byte {
	t.Helper()
	body := map[string]interface{}{
		"model":    contextUsageContractModel,
		"messages": []map[string]interface{}{{"role": "user", "content": prompt}},
		"stream":   stream,
	}
	if stream {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal Chat request: %v", err)
	}
	return encoded
}

func contextUsageChatUsage(t *testing.T, response *httptest.ResponseRecorder, stream bool) map[string]interface{} {
	t.Helper()
	if !stream {
		var envelope map[string]interface{}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode Chat response: %v body=%s", err, response.Body.String())
		}
		return openAIChatContractMap(t, envelope["usage"], "Chat usage")
	}
	chunks := decodeOpenAIChatContractStream(t, response.Body.String())
	if len(chunks) == 0 {
		t.Fatalf("Chat stream returned no chunks: %s", response.Body.String())
	}
	return openAIChatContractMap(t, chunks[len(chunks)-1]["usage"], "Chat terminal usage")
}

func assertContextUsageInputAndCache(
	t *testing.T,
	usage map[string]interface{},
	inputField, detailsField string,
	expectedInput int,
	wantHit bool,
) {
	t.Helper()
	input := contextUsageNumber(t, usage, inputField)
	if input != float64(expectedInput) {
		t.Fatalf("%s=%v, want client estimate %d (usage=%#v)", inputField, input, expectedInput, usage)
	}
	details := openAIChatContractMap(t, usage[detailsField], detailsField)
	cached := contextUsageNumber(t, details, "cached_tokens")
	if !wantHit && cached != 0 {
		t.Fatalf("cold cached_tokens=%v, want 0", cached)
	}
	if wantHit && (cached <= 0 || cached > input) {
		t.Fatalf("hit cached_tokens=%v, want a positive subset of %s=%v", cached, inputField, input)
	}
}

func assertOpenAIKiroContextUsageMap(t *testing.T, usage map[string]interface{}) {
	t.Helper()
	contextUsage := openAIChatContractMap(t, usage["kiro_context_usage"], "kiro_context_usage")
	if got := contextUsageNumber(t, contextUsage, "input_tokens"); got != 125_000 {
		t.Fatalf("kiro_context_usage.input_tokens=%v, want 125000", got)
	}
	if got := contextUsageNumber(t, contextUsage, "context_window"); got != 1_000_000 {
		t.Fatalf("kiro_context_usage.context_window=%v, want 1000000", got)
	}
	if got := contextUsageNumber(t, contextUsage, "percentage"); got != 12.5 {
		t.Fatalf("kiro_context_usage.percentage=%v, want 12.5", got)
	}
	if exceeded, ok := contextUsage["exceeded"].(bool); !ok || exceeded {
		t.Fatalf("kiro_context_usage.exceeded=%#v, want false", contextUsage["exceeded"])
	}
}

func assertKiroContextUsageStruct(t *testing.T, usage *KiroContextUsage) {
	t.Helper()
	if usage == nil {
		t.Fatal("Responses usage is missing kiro_context_usage")
	}
	if usage.InputTokens != 125_000 || usage.ContextWindow != 1_000_000 || usage.Percentage != 12.5 || usage.Exceeded {
		t.Fatalf("Responses kiro_context_usage=%+v, want 125000/1000000 at 12.5%% without exceeded", usage)
	}
}

func contextUsageNumber(t *testing.T, values map[string]interface{}, field string) float64 {
	t.Helper()
	value, ok := values[field].(float64)
	if !ok {
		t.Fatalf("%s=%#v, want JSON number", field, values[field])
	}
	return value
}

func runContextUsageResponsesRequest(
	t *testing.T,
	h *Handler,
	key config.ApiKeyEntry,
	prompt string,
	stream bool,
) ResponsesObject {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"model":  contextUsageContractModel,
		"input":  prompt,
		"stream": stream,
		"store":  false,
	})
	if err != nil {
		t.Fatalf("marshal Responses request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	request.Header.Set("X-Api-Key", key.Key)
	request = withApiKeyContext(request, &key)
	recorder := httptest.NewRecorder()
	h.handleOpenAIResponses(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("Responses status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	if !stream {
		var response ResponsesObject
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode Responses response: %v body=%s", err, recorder.Body.String())
		}
		return response
	}
	for _, event := range decodeWebSearchSSE(t, recorder.Body.String()) {
		if event.Name != "response.completed" && event.Name != "response.incomplete" {
			continue
		}
		encoded, err := json.Marshal(event.Data["response"])
		if err != nil {
			t.Fatalf("marshal terminal Responses event: %v", err)
		}
		var response ResponsesObject
		if err := json.Unmarshal(encoded, &response); err != nil {
			t.Fatalf("decode terminal Responses event: %v event=%#v", err, event.Data)
		}
		return response
	}
	t.Fatalf("Responses stream has no terminal event: %s", recorder.Body.String())
	return ResponsesObject{}
}
