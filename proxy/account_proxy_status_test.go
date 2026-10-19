package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

func TestAccountsAPIProxyBlockedAppliesOnlyToActiveAccounts(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		banStatus   string
		poolEnabled bool
		failClosed  bool
		withProxy   bool
		wantBlocked bool
	}{
		{name: "active_without_proxy", enabled: true, banStatus: "ACTIVE", poolEnabled: true, failClosed: true, wantBlocked: true},
		{name: "legacy_active_without_status", enabled: true, poolEnabled: true, failClosed: true, wantBlocked: true},
		{name: "enabled_unknown_status", enabled: true, banStatus: "UNKNOWN", poolEnabled: true, failClosed: true, wantBlocked: true},
		{name: "operator_disabled", banStatus: "DISABLED", poolEnabled: true, failClosed: true},
		{name: "disabled_without_status", poolEnabled: true, failClosed: true},
		{name: "disabled_with_stale_active_status", banStatus: "ACTIVE", poolEnabled: true, failClosed: true},
		{name: "banned", banStatus: "BANNED", poolEnabled: true, failClosed: true},
		{name: "suspended", banStatus: "SUSPENDED", poolEnabled: true, failClosed: true},
		{name: "enabled_with_banned_status", enabled: true, banStatus: "BANNED", poolEnabled: true, failClosed: true},
		{name: "enabled_with_disabled_status", enabled: true, banStatus: "DISABLED", poolEnabled: true, failClosed: true},
		{name: "enabled_with_suspended_status", enabled: true, banStatus: "SUSPENDED", poolEnabled: true, failClosed: true},
		{name: "normalized_banned_status", enabled: true, banStatus: "  bAnNeD \t", poolEnabled: true, failClosed: true},
		{name: "pool_switch_off", enabled: true, banStatus: "ACTIVE", failClosed: true},
		{name: "fail_closed_switch_off", enabled: true, banStatus: "ACTIVE", poolEnabled: true},
		{name: "active_bound_proxy", enabled: true, banStatus: "ACTIVE", poolEnabled: true, failClosed: true, withProxy: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
				t.Fatal(err)
			}
			account := config.Account{
				ID: "status-account", Email: "status@example.test", Enabled: tc.enabled,
				BanStatus: tc.banStatus, BanReason: "existing account diagnosis", BanTime: 1234,
				AccessToken: "fixture-token", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			}
			if err := config.AddAccount(account); err != nil {
				t.Fatal(err)
			}
			if tc.withProxy {
				if _, _, err := config.ImportProxies([]config.ProxyEntry{{Name: "fixture proxy", URL: "http://127.0.0.1:7890", Enabled: true}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := config.UpdateProxyPoolSettings(config.ProxyPoolSettings{Enabled: tc.poolEnabled, BlockWithoutProxy: tc.failClosed}); err != nil {
				t.Fatal(err)
			}
			if bound := config.GetProxyBindings()[account.ID]; (bound != "") != tc.withProxy {
				t.Fatalf("fixture binding=%q withProxy=%v", bound, tc.withProxy)
			}
			p := accountpool.GetPool()
			p.Reload()
			h := &Handler{pool: p}
			response := httptest.NewRecorder()
			h.apiGetAccounts(response, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var result []struct {
				ID           string `json:"id"`
				Enabled      bool   `json:"enabled"`
				BanStatus    string `json:"banStatus"`
				BanReason    string `json:"banReason"`
				BanTime      int64  `json:"banTime"`
				ProxyBlocked bool   `json:"proxyBlocked"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result) != 1 || result[0].ID != account.ID {
				t.Fatalf("accounts=%+v", result)
			}
			got := result[0]
			if got.ProxyBlocked != tc.wantBlocked {
				t.Errorf("proxyBlocked=%v want=%v enabled=%v banStatus=%q", got.ProxyBlocked, tc.wantBlocked, tc.enabled, tc.banStatus)
			}
			if got.Enabled != account.Enabled || got.BanStatus != account.BanStatus || got.BanReason != account.BanReason || got.BanTime != account.BanTime {
				t.Errorf("account status projection mutated: got=%+v original=%+v", got, account)
			}
		})
	}
}
