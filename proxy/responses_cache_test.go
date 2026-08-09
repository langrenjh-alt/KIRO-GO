package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestPrepareResponsesPromptCacheCarriesRequestTiming(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	ctx, timing := withRequestTiming(context.Background(), &latencyTracker{})
	req := &ResponsesRequest{}
	openAIReq := &OpenAIRequest{
		Model: "claude-sonnet-4-6",
		Messages: []OpenAIMessage{{
			Role:    "user",
			Content: strings.Repeat("timed responses cache prompt ", 80),
		}},
	}
	policy := config.ResolvePromptCachePolicyForAPIKey("")
	h.prepareResponsesPromptCache(req, openAIReq, ctx, false, "key-id", "key-value", 1000, policy, nil)
	if req.promptCache == nil || req.promptCache.profile == nil {
		t.Fatal("expected prompt cache profile")
	}
	if req.promptCache.profile.requestTiming != timing {
		t.Fatal("Responses prompt cache profile did not retain request timing")
	}
}

func TestResponsesPromptCacheUsesBoundPolicyNonStream(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()
	defer h.stopPromptCachePolicyTrackers()
	h.kiroCaller = successfulResponsesCacheCaller

	key25 := addResponsesFunctionCacheBinding(t, "responses-25", "sk-responses-25", 25)
	key75 := addResponsesFunctionCacheBinding(t, "responses-75", "sk-responses-75", 75)

	for _, test := range []struct {
		name          string
		key           config.ApiKeyEntry
		cachedPercent int
	}{
		{name: "25 percent policy", key: key25, cachedPercent: 25},
		{name: "75 percent policy", key: key75, cachedPercent: 75},
	} {
		t.Run(test.name, func(t *testing.T) {
			cold := runResponsesCacheRequest(t, h, test.key, false, `"stable prompt"`)
			if got := cold.Usage.InputTokensDetails.CachedTokens; got != 0 {
				t.Fatalf("cold cached_tokens=%d, want 0", got)
			}
			hit := runResponsesCacheRequest(t, h, test.key, false, `"stable prompt"`)
			wantCached := roundedTokenPercent(hit.Usage.InputTokens, test.cachedPercent)
			if got := hit.Usage.InputTokensDetails.CachedTokens; got != wantCached {
				t.Fatalf("hit cached_tokens=%d, want %d%% of client input (%d); usage=%+v", got, test.cachedPercent, wantCached, hit.Usage)
			}

			tracker, _ := h.promptCacheRuntimeForAPIKey(test.key.ID)
			stats := tracker.Stats()
			if stats.Misses != 1 || stats.Hits != 1 {
				t.Fatalf("policy stats=%+v, want one miss and one hit", stats)
			}
		})
	}
}

func TestResponsesPromptCacheStreamIncludesCachedTokens(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()
	defer h.stopPromptCachePolicyTrackers()
	h.kiroCaller = successfulResponsesCacheCaller
	key := addResponsesFunctionCacheBinding(t, "responses-stream", "sk-responses-stream", 50)

	cold := runResponsesCacheRequest(t, h, key, true, `"stream cache prompt"`)
	if got := cold.Usage.InputTokensDetails.CachedTokens; got != 0 {
		t.Fatalf("cold cached_tokens=%d, want 0", got)
	}
	hit := runResponsesCacheRequest(t, h, key, true, `"stream cache prompt"`)
	if got, want := hit.Usage.InputTokensDetails.CachedTokens, roundedTokenPercent(hit.Usage.InputTokens, 50); got != want {
		t.Fatalf("hit cached_tokens=%d, want 50%% of client input (%d); usage=%+v", got, want, hit.Usage)
	}
}

func TestResponsesPromptCacheBindingWhenAuthenticationIsOptional(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()
	defer h.stopPromptCachePolicyTrackers()
	h.kiroCaller = successfulResponsesCacheCaller
	key := addResponsesFunctionCacheBinding(t, "responses-public-auth", "sk-responses-public-auth", 50)
	if config.IsApiKeyRequired() {
		t.Fatal("test requires RequireApiKey=false")
	}

	var hit ResponsesObject
	for requestNumber := 0; requestNumber < 2; requestNumber++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
			"model":"claude-sonnet-4.6",
			"input":"public deployment cache prompt",
			"store":false
		}`))
		request.Header.Set("Authorization", "bEaReR   "+key.Key)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", requestNumber+1, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("X-Kiro-Api-Key-Matched"); got != "true" {
			t.Fatalf("request %d matched header=%q, want true", requestNumber+1, got)
		}
		if got := recorder.Header().Get("X-Kiro-Prompt-Cache-Policy"); got != key.PromptCachePolicyID {
			t.Fatalf("request %d policy header=%q, want %q", requestNumber+1, got, key.PromptCachePolicyID)
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &hit); err != nil {
			t.Fatalf("decode request %d: %v body=%s", requestNumber+1, err, recorder.Body.String())
		}
	}
	if got, want := hit.Usage.InputTokensDetails.CachedTokens, roundedTokenPercent(hit.Usage.InputTokens, 50); got != want {
		t.Fatalf("optional-auth binding cached_tokens=%d, want 50%% of client input (%d)", got, want)
	}
}

func TestResponsesFunctionCacheUsesOriginalRequestBodyFingerprint(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()
	defer h.stopPromptCachePolicyTrackers()
	h.kiroCaller = successfulResponsesCacheCaller
	key := addResponsesFunctionCacheBinding(t, "responses-raw-body", "sk-responses-raw-body", 50)

	input := `"same decoded prompt"`
	firstScope := runResponsesCacheRequestWithSuffix(t, h, key, false, input, `,"cache_scope":"first"`)
	if got := firstScope.Usage.InputTokensDetails.CachedTokens; got != 0 {
		t.Fatalf("first body cached_tokens=%d, want 0", got)
	}
	secondScopeCold := runResponsesCacheRequestWithSuffix(t, h, key, false, input, `,"cache_scope":"second"`)
	if got := secondScopeCold.Usage.InputTokensDetails.CachedTokens; got != 0 {
		t.Fatalf("unknown raw-body field was dropped from fingerprint: cached_tokens=%d", got)
	}
	secondScopeHit := runResponsesCacheRequestWithSuffix(t, h, key, false, input, `,"cache_scope":"second"`)
	if got, want := secondScopeHit.Usage.InputTokensDetails.CachedTokens, roundedTokenPercent(secondScopeHit.Usage.InputTokens, 50); got != want {
		t.Fatalf("repeated raw body cached_tokens=%d, want 50%% of client input (%d)", got, want)
	}

	tracker, _ := h.promptCacheRuntimeForAPIKey(key.ID)
	stats := tracker.Stats()
	if stats.Misses != 2 || stats.Hits != 1 {
		t.Fatalf("raw-body fingerprint stats=%+v, want two misses and one hit", stats)
	}
}

func TestResponsesPromptCacheComputesOnceAcrossRetry(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()
	defer h.stopPromptCachePolicyTrackers()
	key := addResponsesFunctionCacheBinding(t, "responses-retry", "sk-responses-retry", 50)

	calls := 0
	h.kiroCaller = func(_ *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			callback.OnText("I will inspect the file now.", false)
			callback.OnComplete(400, 4)
			return nil
		}
		callback.OnText(payload.AgentCompletionMarker+"\nfinal answer", false)
		callback.OnStopReason("END_TURN")
		callback.OnComplete(400, 4)
		return nil
	}

	body := `{
		"model":"claude-opus-5",
		"instructions":"You are Codex, a coding agent.",
		"input":"inspect the repository",
		"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
		"store":false
	}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("X-Api-Key", key.Key)
	request = withApiKeyContext(request, &key)
	recorder := httptest.NewRecorder()
	h.handleOpenAIResponses(recorder, request)

	if calls != 2 || recorder.Code != http.StatusOK {
		t.Fatalf("upstream calls=%d status=%d body=%s", calls, recorder.Code, recorder.Body.String())
	}
	tracker, _ := h.promptCacheRuntimeForAPIKey(key.ID)
	stats := tracker.Stats()
	if stats.Misses != 1 || stats.Hits != 0 {
		t.Fatalf("retry counted cache more than once: %+v", stats)
	}
}

func TestResponsesWebSearchPromptCacheIncludesCachedTokens(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()
	defer h.stopPromptCachePolicyTrackers()
	key := addResponsesFunctionCacheBinding(t, "responses-web-search", "sk-responses-web-search", 50)
	h.webSearchMCPCaller = func(_ context.Context, _ *config.Account, _ string) (*webSearchResultSet, error) {
		return testWebSearchResultSet(), nil
	}
	input := `"current Go release"`
	bodySuffix := `,"tools":[{"type":"web_search_preview"}]`

	cold := runResponsesCacheRequestWithSuffix(t, h, key, false, input, bodySuffix)
	if got := cold.Usage.InputTokensDetails.CachedTokens; got != 0 {
		t.Fatalf("cold cached_tokens=%d, want 0", got)
	}
	hit := runResponsesCacheRequestWithSuffix(t, h, key, false, input, bodySuffix)
	if got := hit.Usage.InputTokensDetails.CachedTokens; got <= 0 {
		t.Fatalf("web search hit did not report cached_tokens: %+v", hit.Usage)
	}
}

func TestResponsesWebSearchDegradedResultDoesNotCountCacheMiss(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()
	defer h.stopPromptCachePolicyTrackers()
	key := addResponsesFunctionCacheBinding(t, "responses-web-search-degraded", "sk-responses-web-search-degraded", 50)
	calls := 0
	h.webSearchMCPCaller = func(_ context.Context, _ *config.Account, _ string) (*webSearchResultSet, error) {
		calls++
		code := jsonRPCInvalidParams
		return nil, &webSearchMCPError{
			StatusCode: http.StatusBadGateway,
			RPCCode:    &code,
			Message:    "web search MCP upstream rejected the request",
		}
	}

	response := runResponsesCacheRequestWithSuffix(
		t, h, key, false, `"current Go release"`, `,"tools":[{"type":"web_search_preview"}]`,
	)
	if calls != 1 {
		t.Fatalf("MCP calls=%d, want 1", calls)
	}
	if got := response.Usage.InputTokensDetails.CachedTokens; got != 0 {
		t.Fatalf("degraded response cached_tokens=%d, want 0", got)
	}
	tracker, _ := h.promptCacheRuntimeForAPIKey(key.ID)
	stats := tracker.Stats()
	if stats.Misses != 0 || stats.Hits != 0 {
		t.Fatalf("degraded WebSearch changed cache counters: %+v", stats)
	}
}

func successfulResponsesCacheCaller(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
	callback.OnText("cached response", false)
	callback.OnComplete(400, 4)
	return nil
}

func roundedTokenPercent(tokens, percent int) int {
	return int(float64(tokens)*float64(percent)/100 + 0.5)
}

func addResponsesFunctionCacheBinding(t *testing.T, name, keyValue string, maxReadPercent int) config.ApiKeyEntry {
	t.Helper()
	hitRate := 100
	policy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  name,
		CacheMode:             config.CacheModeFunctionSimulation,
		PromptCacheMaxRatio:   0.85,
		PromptCacheMaxEntries: 512,
		PromptCacheTTLSeconds: config.ExtendedPromptCacheTTLSeconds,
		FunctionCache: config.FunctionCacheConfig{
			Enabled:             true,
			SimulatedTokens:     1000,
			TTLSeconds:          300,
			HitRatePercent:      &hitRate,
			MaxCacheReadPercent: &maxReadPercent,
		},
	})
	if err != nil {
		t.Fatalf("add prompt cache policy: %v", err)
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

func runResponsesCacheRequest(
	t *testing.T,
	h *Handler,
	key config.ApiKeyEntry,
	stream bool,
	inputJSON string,
) ResponsesObject {
	t.Helper()
	return runResponsesCacheRequestWithSuffix(t, h, key, stream, inputJSON, "")
}

func runResponsesCacheRequestWithSuffix(
	t *testing.T,
	h *Handler,
	key config.ApiKeyEntry,
	stream bool,
	inputJSON string,
	bodySuffix string,
) ResponsesObject {
	t.Helper()
	body := fmt.Sprintf(`{"model":"claude-sonnet-4.6","input":%s,"stream":%t,"store":false%s}`, inputJSON, stream, bodySuffix)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("X-Api-Key", key.Key)
	request = withApiKeyContext(request, &key)
	recorder := httptest.NewRecorder()
	h.handleOpenAIResponses(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	if !stream {
		var response ResponsesObject
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode non-stream response: %v body=%s", err, recorder.Body.String())
		}
		return response
	}

	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			continue
		}
		if event.Type != "response.completed" && event.Type != "response.incomplete" {
			continue
		}
		var response ResponsesObject
		if err := json.Unmarshal(event.Response, &response); err != nil {
			t.Fatalf("decode terminal response: %v event=%s", err, line)
		}
		return response
	}
	t.Fatalf("stream has no terminal response: %s", recorder.Body.String())
	return ResponsesObject{}
}
