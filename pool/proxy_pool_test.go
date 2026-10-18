package pool

import (
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
)

func TestProxyPoolAvailabilityUpdatesWithoutAccountReload(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		settings := config.GetProxyPoolSettings()
		settings.Enabled = false
		_, _ = config.UpdateProxyPoolSettings(settings)
	})
	account := config.Account{ID: "power-account", Enabled: true, SubscriptionType: "POWER", AccessToken: "test"}
	if err := config.AddAccount(account); err != nil {
		t.Fatal(err)
	}
	if _, err := config.UpdateProxyPoolSettings(config.ProxyPoolSettings{Enabled: true, BlockWithoutProxy: true}); err != nil {
		t.Fatal(err)
	}
	p := &AccountPool{cooldowns: make(map[string]time.Time), errorCounts: make(map[string]int), modelLists: make(map[string]map[string]bool), inFlight: make(map[string]int)}
	p.Reload()
	assertBlocked := func() {
		t.Helper()
		if got := p.GetNext(); got != nil {
			t.Fatalf("unproxied account selected: %+v", got)
		}
		if got, _ := p.AcquireByIDForModel(account.ID, "", nil); got != nil {
			t.Fatal("unproxied specific account leased")
		}
		if got, _ := p.AcquireForTest(&account); got != nil {
			t.Fatal("unproxied admin-test account leased")
		}
		if got := p.AvailableCount(); got != 0 {
			t.Fatalf("available count=%d", got)
		}
		if got := p.InspectAvailability("", nil); got.Selectable != 0 {
			t.Fatalf("availability=%+v", got)
		}
		if state := p.RuntimeStates()[account.ID]; state.Available || state.Reason != "proxy_unavailable" {
			t.Fatalf("runtime=%+v", state)
		}
	}
	assertBlocked()
	if _, _, err := config.ImportProxies([]config.ProxyEntry{{URL: "http://127.0.0.1:18080", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if got := p.GetNext(); got == nil || got.ID != account.ID {
		t.Fatalf("proxy import did not immediately unblock: %+v", got)
	}
	if got := p.AvailableCount(); got != 1 {
		t.Fatalf("available after proxy import=%d", got)
	}
	proxy := config.GetProxies()[0]
	if err := config.DisableProxy(proxy.ID, proxy.URL, "test network failure"); err != nil {
		t.Fatal(err)
	}
	assertBlocked()
	settings := config.GetProxyPoolSettings()
	settings.BlockWithoutProxy = false
	if _, err := config.UpdateProxyPoolSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := p.GetNext(); got == nil {
		t.Fatal("direct fallback switch did not immediately unblock")
	}
	if state := p.RuntimeStates()[account.ID]; !state.Available {
		t.Fatalf("direct fallback state=%+v", state)
	}
}
