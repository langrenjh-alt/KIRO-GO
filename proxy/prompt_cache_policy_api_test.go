package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

func TestPromptCachePolicyAdminAPIAndApiKeyBinding(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{}
	defer h.stopPromptCachePolicyTrackers()

	createBody := `{"name":"Short TTL","cacheMode":"native","promptCacheMaxRatio":0.4,"promptCacheMaxEntries":512,"promptCacheTTLSeconds":300}`
	createRecorder := httptest.NewRecorder()
	h.apiCreatePromptCachePolicy(createRecorder, httptest.NewRequest(http.MethodPost, "/admin/api/prompt-cache-policies", strings.NewReader(createBody)))
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		Policy config.PromptCachePolicy `json:"policy"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Policy.ID == "" || created.Policy.Name != "Short TTL" {
		t.Fatalf("unexpected created policy: %+v", created.Policy)
	}
	h.promptCachePoliciesMu.Lock()
	createdTracker := h.promptCachePolicies[created.Policy.ID]
	h.promptCachePoliciesMu.Unlock()
	if createdTracker == nil {
		t.Fatal("new policy runtime tracker was not preloaded on the admin path")
	}

	keyBody, _ := json.Marshal(map[string]interface{}{
		"name":                "bound-key",
		"key":                 "sk-bound-policy",
		"enabled":             true,
		"promptCachePolicyId": created.Policy.ID,
	})
	keyRecorder := httptest.NewRecorder()
	h.apiCreateApiKey(keyRecorder, httptest.NewRequest(http.MethodPost, "/admin/api/api-keys", bytes.NewReader(keyBody)))
	if keyRecorder.Code != http.StatusOK {
		t.Fatalf("create key status=%d body=%s", keyRecorder.Code, keyRecorder.Body.String())
	}
	var keyResponse struct {
		ID     string     `json:"id"`
		ApiKey apiKeyView `json:"apiKey"`
	}
	if err := json.Unmarshal(keyRecorder.Body.Bytes(), &keyResponse); err != nil {
		t.Fatalf("decode key response: %v", err)
	}
	if keyResponse.ApiKey.PromptCachePolicyID != created.Policy.ID || keyResponse.ApiKey.PromptCachePolicyName != created.Policy.Name {
		t.Fatalf("API key view omitted binding: %+v", keyResponse.ApiKey)
	}

	disableRecorder := httptest.NewRecorder()
	h.apiUpdateApiKey(disableRecorder, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"enabled":false}`)), keyResponse.ID)
	if disableRecorder.Code != http.StatusOK {
		t.Fatalf("partial key update status=%d body=%s", disableRecorder.Code, disableRecorder.Body.String())
	}
	if got := config.GetApiKeyEntry(keyResponse.ID); got == nil || got.Enabled || got.PromptCachePolicyID != created.Policy.ID {
		t.Fatalf("partial key update overwrote another field: %+v", got)
	}

	deleteRecorder := httptest.NewRecorder()
	h.apiDeletePromptCachePolicy(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/", nil), created.Policy.ID)
	if deleteRecorder.Code != http.StatusConflict {
		t.Fatalf("delete in-use status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}

	unbindRecorder := httptest.NewRecorder()
	h.apiUpdateApiKey(unbindRecorder, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"promptCachePolicyId":""}`)), keyResponse.ID)
	if unbindRecorder.Code != http.StatusOK {
		t.Fatalf("unbind status=%d body=%s", unbindRecorder.Code, unbindRecorder.Body.String())
	}
	if got := config.GetApiKeyEntry(keyResponse.ID); got == nil || got.PromptCachePolicyID != "" {
		t.Fatalf("API key was not unbound: %+v", got)
	}

	deleteRecorder = httptest.NewRecorder()
	h.apiDeletePromptCachePolicy(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/", nil), created.Policy.ID)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
}

func TestPromptCachePolicyPersistenceFailureReturnsServerError(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	created, err := config.AddPromptCachePolicy(config.PromptCachePolicy{Name: "stable"})
	if err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatalf("remove config file: %v", err)
	}
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatalf("replace config with directory: %v", err)
	}

	recorder := httptest.NewRecorder()
	body := `{"name":"changed","promptCacheMaxRatio":0.85,"promptCacheMaxEntries":512,"promptCacheTTLSeconds":300}`
	(&Handler{}).apiUpdatePromptCachePolicy(recorder, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body)), created.ID)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("persistence failure status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := config.GetPromptCachePolicy(created.ID); got == nil || got.Name != "stable" {
		t.Fatalf("failed update changed in-memory policy: %+v", got)
	}
}

func TestDeletePromptCachePolicyStopsRuntimeTracker(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	policy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{Name: "temporary"})
	if err != nil {
		t.Fatalf("add policy: %v", err)
	}
	h := &Handler{promptCachePolicies: make(map[string]*promptCacheTracker)}
	tracker := h.promptCacheTrackerForPolicy(policy)
	tracker.mu.Lock()
	tracker.dirty = true
	tracker.mu.Unlock()

	recorder := httptest.NewRecorder()
	h.apiDeletePromptCachePolicy(recorder, httptest.NewRequest(http.MethodDelete, "/", nil), policy.ID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	h.promptCachePoliciesMu.Lock()
	_, stillTracked := h.promptCachePolicies[policy.ID]
	h.promptCachePoliciesMu.Unlock()
	if stillTracked {
		t.Fatal("deleted policy tracker remains registered")
	}
	select {
	case <-tracker.stopDone:
	default:
		t.Fatal("deleted policy tracker save loop is still running")
	}
	cachePath := filepath.Join(config.GetConfigDir(), promptCachePolicyFileName(policy.ID))
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("deleted policy cache file still exists: %v", err)
	}
}

func TestSetMaxEntriesShrinksFunctionCacheImmediately(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 4)
	tracker.functionEntries[[32]byte{1}] = simulatedFunctionCacheEntry{}
	tracker.functionEntries[[32]byte{2}] = simulatedFunctionCacheEntry{}
	tracker.functionEntries[[32]byte{3}] = simulatedFunctionCacheEntry{}

	tracker.SetMaxEntries(1)
	if got := tracker.Stats().FunctionEntries; got != 1 {
		t.Fatalf("function entries after shrink=%d, want 1", got)
	}
}

func TestPromptCachePolicyHotReloadRejectsStaleRevision(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 256)
	h := &Handler{
		promptCachePolicies: map[string]*promptCacheTracker{
			"policy": tracker,
		},
	}

	h.applyPromptCachePolicy(config.PromptCachePolicy{
		ID:                    "policy",
		Revision:              2,
		PromptCacheMaxEntries: 512,
		PromptCacheTTLSeconds: config.ExtendedPromptCacheTTLSeconds,
	})
	h.applyPromptCachePolicy(config.PromptCachePolicy{
		ID:                    "policy",
		Revision:              1,
		PromptCacheMaxEntries: 256,
		PromptCacheTTLSeconds: config.DefaultPromptCacheTTLSeconds,
	})

	if got := tracker.Stats().Capacity; got != 512 {
		t.Fatalf("stale policy reverted capacity to %d", got)
	}
	if got := tracker.DefaultTTL(); got != time.Hour {
		t.Fatalf("stale policy reverted TTL to %v", got)
	}
}

func TestPromptCacheStatsAggregatesNamedTrackers(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	activePolicy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "active",
		PromptCacheMaxEntries: 256,
	})
	if err != nil {
		t.Fatalf("add active policy: %v", err)
	}
	inactivePolicy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "inactive",
		PromptCacheMaxEntries: 512,
	})
	if err != nil {
		t.Fatalf("add inactive policy: %v", err)
	}
	global := newPromptCacheTrackerWithCapacity(time.Hour, 10)
	named := newPromptCacheTrackerWithCapacity(time.Hour, 256)
	named.functionEntries[[32]byte{1}] = simulatedFunctionCacheEntry{}
	h := &Handler{
		promptCache: global,
		promptCachePolicies: map[string]*promptCacheTracker{
			activePolicy.ID: named,
		},
	}

	stats, policies := h.promptCacheStatsSnapshot()
	if stats.Capacity != 266 || stats.FunctionEntries != 1 {
		t.Fatalf("aggregated cache stats = %+v", stats)
	}
	if len(policies) != 2 {
		t.Fatalf("policy stats count=%d, want 2: %+v", len(policies), policies)
	}
	if policies[0].ID != activePolicy.ID || policies[0].Name != "active" || policies[0].Stats.Capacity != 256 || policies[0].Stats.FunctionEntries != 1 {
		t.Fatalf("active policy stats = %+v", policies[0])
	}
	if policies[1].ID != inactivePolicy.ID || policies[1].Name != "inactive" || policies[1].Stats.Capacity != 512 || policies[1].Stats.Entries != 0 {
		t.Fatalf("inactive policy stats = %+v", policies[1])
	}
}

func TestPromptCacheRuntimeUsesIndependentNamedPolicyTrackers(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	hitRate, maxRead := 100, 70
	short, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "short-native",
		CacheMode:             config.CacheModeNative,
		PromptCacheMaxRatio:   0.25,
		PromptCacheMaxEntries: 256,
		PromptCacheTTLSeconds: 300,
	})
	if err != nil {
		t.Fatalf("add short policy: %v", err)
	}
	functionPolicy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "function",
		CacheMode:             config.CacheModeFunctionSimulation,
		PromptCacheMaxRatio:   0.9,
		PromptCacheMaxEntries: 512,
		PromptCacheTTLSeconds: 3600,
		FunctionCache: config.FunctionCacheConfig{
			Enabled:             true,
			SimulatedTokens:     2048,
			TTLSeconds:          120,
			HitRatePercent:      &hitRate,
			MaxCacheReadPercent: &maxRead,
		},
	})
	if err != nil {
		t.Fatalf("add function policy: %v", err)
	}
	keyA, err := config.AddApiKey(config.ApiKeyEntry{Name: "a", Key: "sk-a-policy", Enabled: true, PromptCachePolicyID: short.ID})
	if err != nil {
		t.Fatalf("add key A: %v", err)
	}
	keyB, err := config.AddApiKey(config.ApiKeyEntry{Name: "b", Key: "sk-b-policy", Enabled: true, PromptCachePolicyID: functionPolicy.ID})
	if err != nil {
		t.Fatalf("add key B: %v", err)
	}

	h := &Handler{
		promptCache:         newPromptCacheTrackerWithCapacity(time.Hour, 1024),
		promptCachePolicies: make(map[string]*promptCacheTracker),
	}
	defer h.stopPromptCachePolicyTrackers()

	trackerA, resolvedA := h.promptCacheRuntimeForAPIKey(keyA.ID)
	trackerAAgain, _ := h.promptCacheRuntimeForAPIKey(keyA.ID)
	trackerB, resolvedB := h.promptCacheRuntimeForAPIKey(keyB.ID)
	if trackerA == nil || trackerB == nil || trackerA == trackerB || trackerA != trackerAAgain {
		t.Fatalf("unexpected tracker partitioning: A=%p A2=%p B=%p", trackerA, trackerAAgain, trackerB)
	}
	if trackerA.Stats().Capacity != 256 || trackerB.Stats().Capacity != 512 {
		t.Fatalf("policy capacities not applied: A=%+v B=%+v", trackerA.Stats(), trackerB.Stats())
	}

	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   strings.Repeat("stable cache prefix ", 800),
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	profileA := trackerA.BuildClaudeProfileWithPolicy(req, 4096, resolvedA, "key-a")
	profileB := trackerB.BuildClaudeProfileWithPolicy(req, 4096, resolvedB, "key-b")
	if profileA == nil || profileA.SimulatedFunctionCache || profileA.MaxCacheReadRatio != 0.25 {
		t.Fatalf("unexpected native profile: %+v", profileA)
	}
	if got := profileA.Breakpoints[0].TTL; got != 5*time.Minute {
		t.Fatalf("native TTL=%v, want 5m", got)
	}
	if profileB == nil || !profileB.SimulatedFunctionCache || profileB.SimulatedMaxReadPercent != 70 {
		t.Fatalf("unexpected function profile: %+v", profileB)
	}
	if profileA.runtimeTracker != trackerA || profileB.runtimeTracker != trackerB {
		t.Fatal("profiles did not retain their request-scoped tracker")
	}

	short.PromptCacheMaxEntries = 300
	updated, err := config.UpdatePromptCachePolicy(short.ID, short)
	if err != nil {
		t.Fatalf("update short policy: %v", err)
	}
	h.applyPromptCachePolicy(updated)
	if trackerA.Stats().Capacity != 300 {
		t.Fatalf("hot resize did not update policy A: %+v", trackerA.Stats())
	}
	if trackerB.Stats().Capacity != 512 {
		t.Fatalf("hot resize of A changed policy B: %+v", trackerB.Stats())
	}
	if got := h.promptCacheTrackerForPolicy(resolvedA).Stats().Capacity; got != 300 {
		t.Fatalf("old request snapshot reverted hot capacity to %d", got)
	}

	lazy, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "lazy",
		PromptCacheMaxEntries: 256,
	})
	if err != nil {
		t.Fatalf("add lazy policy: %v", err)
	}
	lazySnapshot := lazy
	lazy.PromptCacheMaxEntries = 400
	if _, err := config.UpdatePromptCachePolicy(lazy.ID, lazy); err != nil {
		t.Fatalf("update lazy policy: %v", err)
	}
	if got := h.promptCacheTrackerForPolicy(lazySnapshot).Stats().Capacity; got != 400 {
		t.Fatalf("lazy tracker used stale snapshot capacity %d", got)
	}
}

func TestAuthenticateRecognizesConfiguredKeyWhenAuthIsOptional(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	created, err := config.AddApiKey(config.ApiKeyEntry{Name: "optional", Key: "sk-optional", Enabled: true})
	if err != nil {
		t.Fatalf("add key: %v", err)
	}

	h := &Handler{}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer sk-optional")
	entry, err := h.authenticate(request)
	if err != nil || entry == nil || entry.ID != created.ID {
		t.Fatalf("optional configured key was not recognized: entry=%+v err=%v", entry, err)
	}

	unknown := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	unknown.Header.Set("Authorization", "Bearer unknown")
	if entry, err := h.authenticate(unknown); err != nil || entry != nil {
		t.Fatalf("unknown optional key must pass anonymously: entry=%+v err=%v", entry, err)
	}
}

func TestServeHTTPReportsEffectivePromptCachePolicyForConfiguredKey(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	policyA, err := config.AddPromptCachePolicy(config.PromptCachePolicy{Name: "policy-a"})
	if err != nil {
		t.Fatalf("add policy A: %v", err)
	}
	policyB, err := config.AddPromptCachePolicy(config.PromptCachePolicy{Name: "policy-b"})
	if err != nil {
		t.Fatalf("add policy B: %v", err)
	}
	keyA, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-policy-a", Enabled: true, PromptCachePolicyID: policyA.ID})
	if err != nil {
		t.Fatalf("add key A: %v", err)
	}
	keyB, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-policy-b", Enabled: true, PromptCachePolicyID: policyB.ID})
	if err != nil {
		t.Fatalf("add key B: %v", err)
	}
	if _, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-disabled-claude", Enabled: false}); err != nil {
		t.Fatalf("add disabled Claude key: %v", err)
	}
	if _, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-disabled-openai", Enabled: false}); err != nil {
		t.Fatalf("add disabled OpenAI key: %v", err)
	}

	h := &Handler{
		promptCache:         newPromptCacheTracker(defaultPromptCacheTTL),
		promptCachePolicies: make(map[string]*promptCacheTracker),
	}
	defer h.stopPromptCachePolicyTrackers()

	tests := []struct {
		name          string
		path          string
		xAPIKey       string
		authorization string
		wantKeyID     string
		wantPolicyID  string
	}{
		{
			name:          "Claude X-Api-Key is not masked by injected bearer",
			path:          "/v1/messages",
			xAPIKey:       "sk-policy-a",
			authorization: "Bearer unknown-injected-key",
			wantKeyID:     keyA.ID,
			wantPolicyID:  policyA.ID,
		},
		{
			name:          "case-insensitive bearer",
			path:          "/v1/messages",
			authorization: "bEaReR   sk-policy-b",
			wantKeyID:     keyB.ID,
			wantPolicyID:  policyB.ID,
		},
		{
			name:          "disabled Claude credential does not mask enabled bearer",
			path:          "/v1/messages",
			xAPIKey:       "sk-disabled-claude",
			authorization: "Bearer sk-policy-b",
			wantKeyID:     keyB.ID,
			wantPolicyID:  policyB.ID,
		},
		{
			name:          "disabled OpenAI credential does not mask enabled X-Api-Key",
			path:          "/v1/chat/completions",
			xAPIKey:       "sk-policy-a",
			authorization: "Bearer sk-disabled-openai",
			wantKeyID:     keyA.ID,
			wantPolicyID:  policyA.ID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{"))
			if tc.xAPIKey != "" {
				req.Header.Set("X-Api-Key", tc.xAPIKey)
			}
			if tc.authorization != "" {
				req.Header.Set("Authorization", tc.authorization)
			}
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("X-Kiro-Api-Key-Matched"); got != "true" {
				t.Fatalf("matched header=%q, want true for key %s", got, tc.wantKeyID)
			}
			if got := recorder.Header().Get("X-Kiro-Prompt-Cache-Policy"); got != tc.wantPolicyID {
				t.Fatalf("policy header=%q, want %q", got, tc.wantPolicyID)
			}
		})
	}
}

func TestServeHTTPExecutesIndependentPromptCachePoliciesForTwoAPIKeys(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "cache-policy-e2e-account",
		Enabled:     true,
		AccessToken: "cache-policy-e2e-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:1:profile/cache-policy-e2e",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	hitRate := 100
	readA, readB := 20, 80
	policyA, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "function-a",
		CacheMode:             config.CacheModeFunctionSimulation,
		PromptCacheMaxEntries: 256,
		FunctionCache: config.FunctionCacheConfig{
			Enabled:             true,
			SimulatedTokens:     1200,
			TTLSeconds:          300,
			HitRatePercent:      &hitRate,
			MaxCacheReadPercent: &readA,
		},
	})
	if err != nil {
		t.Fatalf("add policy A: %v", err)
	}
	policyB, err := config.AddPromptCachePolicy(config.PromptCachePolicy{
		Name:                  "function-b",
		CacheMode:             config.CacheModeFunctionSimulation,
		PromptCacheMaxEntries: 256,
		FunctionCache: config.FunctionCacheConfig{
			Enabled:             true,
			SimulatedTokens:     2400,
			TTLSeconds:          3600,
			HitRatePercent:      &hitRate,
			MaxCacheReadPercent: &readB,
		},
	})
	if err != nil {
		t.Fatalf("add policy B: %v", err)
	}
	keyA, err := config.AddApiKey(config.ApiKeyEntry{
		Name: "key-a", Key: "sk-cache-e2e-a", Enabled: true, PromptCachePolicyID: policyA.ID,
	})
	if err != nil {
		t.Fatalf("add key A: %v", err)
	}
	keyB, err := config.AddApiKey(config.ApiKeyEntry{
		Name: "key-b", Key: "sk-cache-e2e-b", Enabled: true, PromptCachePolicyID: policyB.ID,
	})
	if err != nil {
		t.Fatalf("add key B: %v", err)
	}
	for _, tc := range []struct {
		key    config.ApiKeyEntry
		policy config.PromptCachePolicy
	}{
		{key: keyA, policy: policyA},
		{key: keyB, policy: policyB},
	} {
		resolved := config.ResolvePromptCachePolicyForAPIKey(tc.key.ID)
		if resolved.ID != tc.policy.ID || resolved.CacheMode != config.CacheModeFunctionSimulation ||
			resolved.FunctionCache.SimulatedTokens != tc.policy.FunctionCache.SimulatedTokens {
			t.Fatalf("resolved policy for key %q=%+v, want %+v", tc.key.ID, resolved, tc.policy)
		}
	}

	pool := accountpool.GetPool()
	pool.Reload()
	t.Cleanup(pool.WaitForPendingStats)
	h := &Handler{
		pool:                pool,
		promptCache:         newPromptCacheTracker(defaultPromptCacheTTL),
		promptCachePolicies: make(map[string]*promptCacheTracker),
		kiroCaller: func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
			callback.OnText("ok", false)
			callback.OnComplete(4000, 1)
			return nil
		},
	}
	defer h.stopPromptCachePolicyTrackers()

	body := `{"model":"claude-sonnet-4-6","max_tokens":64,"system":"stable cache prefix","messages":[{"role":"user","content":"hello"}]}`
	call := func(key, policyID string) ClaudeResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("X-Api-Key", key)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("key %q status=%d body=%s", key, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("X-Kiro-Api-Key-Matched"); got != "true" {
			t.Fatalf("key %q matched header=%q", key, got)
		}
		if got := recorder.Header().Get("X-Kiro-Prompt-Cache-Policy"); got != policyID {
			t.Fatalf("key %q policy header=%q, want %q", key, got, policyID)
		}
		var response ClaudeResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode key %q response: %v", key, err)
		}
		return response
	}

	firstA := call(keyA.Key, policyA.ID)
	firstB := call(keyB.Key, policyB.ID)
	secondA := call(keyA.Key, policyA.ID)
	secondB := call(keyB.Key, policyB.ID)
	if firstA.Usage.CacheCreationInputTokens <= 0 || firstA.Usage.CacheReadInputTokens != 0 {
		t.Fatalf("policy A first usage=%+v", firstA.Usage)
	}
	if firstB.Usage.CacheCreationInputTokens <= 0 || firstB.Usage.CacheReadInputTokens != 0 {
		t.Fatalf("policy B first usage=%+v", firstB.Usage)
	}
	if secondA.Usage.CacheReadInputTokens <= 0 || secondA.Usage.CacheCreationInputTokens != 0 {
		t.Fatalf("policy A second usage=%+v", secondA.Usage)
	}
	if secondB.Usage.CacheReadInputTokens <= secondA.Usage.CacheReadInputTokens || secondB.Usage.CacheCreationInputTokens != 0 {
		t.Fatalf("policy B second usage=%+v, policy A=%+v", secondB.Usage, secondA.Usage)
	}

	h.promptCachePoliciesMu.Lock()
	trackerA := h.promptCachePolicies[policyA.ID]
	trackerB := h.promptCachePolicies[policyB.ID]
	h.promptCachePoliciesMu.Unlock()
	if trackerA == nil || trackerB == nil || trackerA == trackerB {
		t.Fatalf("policy trackers are not independent: A=%p B=%p", trackerA, trackerB)
	}
	if stats := trackerA.Stats(); stats.Misses != 1 || stats.Hits != 1 {
		t.Fatalf("policy A stats=%+v, want one miss and one hit", stats)
	}
	if stats := trackerB.Stats(); stats.Misses != 1 || stats.Hits != 1 {
		t.Fatalf("policy B stats=%+v, want one miss and one hit", stats)
	}
}
