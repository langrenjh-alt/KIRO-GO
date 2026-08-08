package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestClaudeWebSearchUsesBoundPromptCacheTracker(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, nil)
	t.Cleanup(h.stopPromptCachePolicyTrackers)
	h.webSearchMCPCaller = func(_ context.Context, _ *config.Account, _ string) (*webSearchResultSet, error) {
		return testWebSearchResultSet(), nil
	}
	key, policy := addBusinessRouteFunctionCacheBinding(t, "web-search-bound", "sk-web-search-bound")
	body := `{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"messages":[{"role":"user","content":"Search for the current Go release."}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}]
	}`

	first := serveBoundClaudeBusinessRoute(t, h, key.Key, policy.ID, body)
	second := serveBoundClaudeBusinessRoute(t, h, key.Key, policy.ID, body)
	if first.Usage.CacheCreationInputTokens <= 0 || first.Usage.CacheReadInputTokens != 0 {
		t.Fatalf("first WebSearch cache usage=%+v", first.Usage)
	}
	if second.Usage.CacheReadInputTokens <= 0 || second.Usage.CacheCreationInputTokens != 0 {
		t.Fatalf("second WebSearch cache usage=%+v", second.Usage)
	}
	assertOnlyBoundPromptCacheTrackerUsed(t, h, key.ID)
}

func TestCodeExecutionUsesBoundPromptCacheTracker(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnToolUse(KiroToolUse{
			ToolUseID: "kiro-code-bound-cache",
			Name:      codeExecutionToolName,
			Input:     map[string]interface{}{"code": "print(6 * 7)"},
		})
		callback.OnComplete(120, 8)
		return nil
	})
	t.Cleanup(h.stopPromptCachePolicyTrackers)
	key, policy := addBusinessRouteFunctionCacheBinding(t, "code-execution-bound", "sk-code-execution-bound")
	body := `{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"messages":[{"role":"user","content":"Calculate six times seven."}],
		"tools":[{"type":"code_execution_20250522","name":"code_execution"}]
	}`

	first := serveBoundClaudeBusinessRoute(t, h, key.Key, policy.ID, body)
	second := serveBoundClaudeBusinessRoute(t, h, key.Key, policy.ID, body)
	if first.Usage.CacheCreationInputTokens <= 0 || first.Usage.CacheReadInputTokens != 0 {
		t.Fatalf("first Code Execution cache usage=%+v", first.Usage)
	}
	if second.Usage.CacheReadInputTokens <= 0 || second.Usage.CacheCreationInputTokens != 0 {
		t.Fatalf("second Code Execution cache usage=%+v", second.Usage)
	}
	assertOnlyBoundPromptCacheTrackerUsed(t, h, key.ID)
}

func addBusinessRouteFunctionCacheBinding(t *testing.T, name, keyValue string) (config.ApiKeyEntry, config.PromptCachePolicy) {
	t.Helper()
	hitRate, maxRead := 100, 80
	policy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  name,
		CacheMode:             config.CacheModeFunctionSimulation,
		PromptCacheMaxEntries: 256,
		FunctionCache: config.FunctionCacheConfig{
			Enabled:             true,
			SimulatedTokens:     1000,
			TTLSeconds:          300,
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
		t.Fatalf("add bound API key: %v", err)
	}
	return key, policy
}

func serveBoundClaudeBusinessRoute(t *testing.T, h *Handler, keyValue, policyID, body string) ClaudeResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Api-Key", keyValue)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Kiro-Prompt-Cache-Policy"); got != policyID {
		t.Fatalf("policy header=%q, want %q", got, policyID)
	}
	var response ClaudeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Claude response: %v body=%s", err, recorder.Body.String())
	}
	return response
}

func assertOnlyBoundPromptCacheTrackerUsed(t *testing.T, h *Handler, apiKeyID string) {
	t.Helper()
	tracker, policy := h.promptCacheRuntimeForAPIKey(apiKeyID)
	if tracker == nil || policy.ID == "" {
		t.Fatalf("bound runtime missing: tracker=%p policy=%+v", tracker, policy)
	}
	if stats := tracker.Stats(); stats.Misses != 1 || stats.Hits != 1 || stats.FunctionEntries != 1 {
		t.Fatalf("bound tracker stats=%+v, want one miss, one hit, one function entry", stats)
	}
	if stats := h.promptCache.Stats(); stats.Misses != 0 || stats.Hits != 0 || stats.Entries != 0 || stats.FunctionEntries != 0 {
		t.Fatalf("global tracker was used by bound request: %+v", stats)
	}
}
