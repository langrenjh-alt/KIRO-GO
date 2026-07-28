package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

func TestSettingsAPIPersistsDispatchAndCachePolicies(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool(), promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	body := `{
		"scheduling":{"mode":"least_requests"},
		"rateLimit":{"mode":"retry_same","retrySameAccount":2,"retryDelayMs":25,"retryBackoffMultiplier":2,"retryMaxDelayMs":100},
		"accountPriority":{"enabled":true,"rules":[{"id":"opus","models":["claude-opus"],"accountLevels":["power"],"enabled":true}]},
		"cacheMode":"function_simulation",
		"functionCache":{"enabled":true,"simulatedData":"stable","simulatedTokens":500,"ttlSeconds":60,"hitRatePercent":75,"maxCacheReadPercent":80}
	}`
	recorder := httptest.NewRecorder()
	h.apiUpdateSettings(recorder, httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := config.GetSchedulingConfig().Mode; got != config.SchedulingModeLeastRequests {
		t.Fatalf("scheduling mode=%q", got)
	}
	if got := config.GetRateLimitConfig(); got.Mode != config.RateLimitModeRetrySame || got.RetrySameAccount != 2 {
		t.Fatalf("rate policy=%+v", got)
	}
	if got := config.GetCacheMode(); got != config.CacheModeFunctionSimulation {
		t.Fatalf("cache mode=%q", got)
	}

	getRecorder := httptest.NewRecorder()
	h.apiGetSettings(getRecorder, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))
	var settings map[string]interface{}
	if err := json.NewDecoder(getRecorder.Body).Decode(&settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	for _, key := range []string{"rateLimit", "scheduling", "accountPriority", "cacheMode", "functionCache"} {
		if _, ok := settings[key]; !ok {
			t.Fatalf("settings response missing %q: %#v", key, settings)
		}
	}
}

func TestAccountAPIEditsLevelAndConcurrency(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "account-policy", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	recorder := httptest.NewRecorder()
	h.apiUpdateAccount(recorder, httptest.NewRequest(http.MethodPut, "/admin/api/accounts/account-policy", strings.NewReader(`{"accountLevel":"pro-plus","maxConcurrency":3}`)), "account-policy")
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 || accounts[0].AccountLevel != config.AccountLevelProPlus || accounts[0].MaxConcurrency != 3 {
		t.Fatalf("updated account=%#v", accounts)
	}
}
