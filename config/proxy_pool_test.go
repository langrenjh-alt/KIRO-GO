package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func initProxyPoolTest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func enableProxyPoolTest(t *testing.T, block bool) {
	t.Helper()
	if _, err := UpdateProxyPoolSettings(ProxyPoolSettings{Enabled: true, BlockWithoutProxy: block}); err != nil {
		t.Fatal(err)
	}
}

func addProxyPoolTest(t *testing.T, n int) []ProxyEntry {
	t.Helper()
	entries := make([]ProxyEntry, n)
	for i := range entries {
		entries[i] = ProxyEntry{URL: fmt.Sprintf("http://127.0.0.1:%d", 15000+i), Enabled: true}
	}
	if added, skipped, err := ImportProxies(entries); err != nil || added != n || skipped != 0 {
		t.Fatalf("import=(%d,%d,%v)", added, skipped, err)
	}
	return GetProxies()
}

func TestProxyPoolDefaultsAndURLValidation(t *testing.T) {
	initProxyPoolTest(t)
	if got := GetProxyPoolSettings(); got.Enabled || !got.BlockWithoutProxy || got.CheckIntervalSeconds != 60 || got.CheckConcurrency != 16 {
		t.Fatalf("defaults=%+v", got)
	}
	for raw, want := range map[string]string{
		" HTTP://EXAMPLE.COM:8080/ ":     "http://example.com:8080",
		"socks5h://user:pass@[::1]:1080": "socks5://user:pass@[::1]:1080",
		"https://proxy.test:443":         "https://proxy.test:443",
	} {
		if got, err := NormalizeProxyURL(raw); err != nil || got != want {
			t.Errorf("normalize %q=(%q,%v), want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "127.0.0.1:80", "ftp://host:1", "http://host:0", "http://host:65536", "http://host", "http://host:80/a", "http://host:80?x=1", "http://host:80#x"} {
		if _, err := NormalizeProxyURL(raw); !errors.Is(err, ErrInvalidProxy) {
			t.Errorf("invalid %q error=%v", raw, err)
		}
	}
	if _, err := UpdateProxyPoolSettings(ProxyPoolSettings{CheckIntervalSeconds: 1}); err == nil {
		t.Fatal("accepted tight check loop")
	}
	if _, err := UpdateProxyPoolSettings(ProxyPoolSettings{CheckConcurrency: 129}); err == nil {
		t.Fatal("accepted excessive check concurrency")
	}
}

func TestProxyPoolMigratesOnlyLegacySwitchAndCleansOldStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"password":"test","s2apBindEnabled":true,"s2apBindHours":12,"s2apAccountLeases":{"a":{"proxyId":"old","exitIP":"192.0.2.1","until":9999999999}},"accounts":[{"id":"a","enabled":true}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := GetProxyPoolSettings(); !got.Enabled || !got.BlockWithoutProxy {
		t.Fatalf("migration=%+v", got)
	}
	if len(GetProxies()) != 0 || len(GetProxyBindings()) != 0 {
		t.Fatal("legacy external proxy state was imported")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s2ap") {
		t.Fatalf("legacy state remains: %s", data)
	}
	if _, _, err := ResolveAccountProxy("a"); !errors.Is(err, ErrNoAvailableProxy) {
		t.Fatalf("expected blocked empty migrated pool, got %v", err)
	}
}

func TestProxyPoolOmittedBlockSettingDefaultsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"proxyPoolSettings":{"enabled":true},"accounts":[{"id":"a","enabled":true}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if !GetProxyPoolSettings().BlockWithoutProxy {
		t.Fatal("omitted block setting must fail closed")
	}
	if _, _, err := ResolveAccountProxy("a"); !errors.Is(err, ErrNoAvailableProxy) {
		t.Fatalf("empty pool error=%v", err)
	}
}

func TestProxyPoolConcurrentSettingPatchesAndPolicySnapshot(t *testing.T) {
	initProxyPoolTest(t)
	addProxyPoolTest(t, 1)
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, patch := range []ProxyPoolSettingsPatch{{Enabled: &yes}, {BlockWithoutProxy: &no}} {
		wg.Add(1)
		go func(p ProxyPoolSettingsPatch) {
			defer wg.Done()
			_, err := PatchProxyPoolSettings(p)
			if err != nil {
				errCh <- err
			}
		}(patch)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	settings, entry, bound, err := ResolveAccountProxyPolicy("a")
	if err != nil || !settings.Enabled || settings.BlockWithoutProxy || !bound || entry.ID == "" {
		t.Fatalf("inconsistent patch/snapshot: %+v %+v %v %v", settings, entry, bound, err)
	}
}

func TestProxyPoolAssignmentsBalancedStableAndAllTiers(t *testing.T) {
	path := initProxyPoolTest(t)
	proxies := addProxyPoolTest(t, 3)
	for i, tier := range []string{"FREE", "PRO", "PRO_PLUS", "POWER", "MAX", "pro+", "power"} {
		if err := AddAccount(Account{ID: fmt.Sprint(i), Enabled: true, AccountLevel: tier}); err != nil {
			t.Fatal(err)
		}
	}
	enableProxyPoolTest(t, true)
	bindings := GetProxyBindings()
	if len(bindings) != 7 {
		t.Fatalf("bindings=%v", bindings)
	}
	for i := 0; i < 3; i++ {
		if bindings[fmt.Sprint(i)] != proxies[i].ID {
			t.Fatalf("unused proxies not preferred: %v", bindings)
		}
	}
	counts := map[string]int{}
	for _, id := range bindings {
		counts[id]++
	}
	if counts[proxies[0].ID] != 3 || counts[proxies[1].ID] != 2 || counts[proxies[2].ID] != 2 {
		t.Fatalf("unbalanced=%v", counts)
	}
	if err := ReconcileProxyBindings(); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bindings, GetProxyBindings()) {
		t.Fatal("stable assignments changed on reload")
	}
	for accountID, proxyID := range bindings {
		entry, bound, err := ResolveAccountProxy(accountID)
		if err != nil || !bound || entry.ID != proxyID {
			t.Fatalf("resolve %s=(%+v,%v,%v)", accountID, entry, bound, err)
		}
	}
}

func TestProxyPoolAccountLifecycleReleasesAndAssigns(t *testing.T) {
	initProxyPoolTest(t)
	proxies := addProxyPoolTest(t, 2)
	enableProxyPoolTest(t, true)
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "b", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := SetAccountEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := GetProxyBindings()["a"]; ok {
		t.Fatal("disabled account kept binding")
	}
	if _, _, err := ResolveAccountProxy("a"); !errors.Is(err, ErrAccountProxyUnavailable) {
		t.Fatalf("disabled error=%v", err)
	}
	if added, skipped, err := AddAccounts([]Account{{ID: "new", Enabled: true, RefreshToken: "new-token"}}); err != nil || added != 1 || skipped != 0 {
		t.Fatalf("bulk add=(%d,%d,%v)", added, skipped, err)
	}
	if GetProxyBindings()["new"] != proxies[0].ID {
		t.Fatal("new account did not reuse freed proxy")
	}
	if err := SetAccountBanStatus("new", "BANNED", "test"); err != nil {
		t.Fatal(err)
	}
	if _, ok := GetProxyBindings()["new"]; ok {
		t.Fatal("banned account kept binding")
	}
	if err := SuspendAccountTemporarily("b", "test"); err != nil {
		t.Fatal(err)
	}
	if len(GetProxyBindings()) != 0 {
		t.Fatal("suspended account kept binding")
	}
	if err := SetAccountEnabled("a", true); err != nil {
		t.Fatal(err)
	}
	if len(GetProxyBindings()) != 1 {
		t.Fatal("re-enabled account not assigned")
	}
	if err := UpdateAccount("a", Account{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if len(GetProxyBindings()) != 0 {
		t.Fatal("updated disabled account kept binding")
	}
	if err := SetAccountEnabled("a", true); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAccount("a"); err != nil {
		t.Fatal(err)
	}
	if len(GetProxyBindings()) != 0 {
		t.Fatal("deleted account kept binding")
	}
}

func TestProxyPoolCheckFailureRebindAndAutomaticRecovery(t *testing.T) {
	initProxyPoolTest(t)
	proxies := addProxyPoolTest(t, 2)
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	enableProxyPoolTest(t, true)
	if err := RecordProxyCheck(proxies[0].ID, proxies[0].URL, ProxyCheckResult{LastError: "dial failed", CheckedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if GetProxies()[0].Enabled || GetProxies()[0].Health != ProxyHealthUnhealthy || GetProxyBindings()["a"] != proxies[1].ID {
		t.Fatalf("check failure not reconciled: %+v %v", GetProxies(), GetProxyBindings())
	}
	if err := RecordProxyCheck(proxies[0].ID, proxies[0].URL, ProxyCheckResult{Healthy: true, ExitIP: "192.0.2.3", Country: "US", City: "New York"}); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; !got.Enabled || got.DisabledBy != "" || got.Health != ProxyHealthHealthy {
		t.Fatalf("healthy check did not automatically recover fault-disabled proxy: %+v", got)
	}
	if err := DisableProxy(proxies[1].ID, proxies[1].URL, "connection failed"); err != nil {
		t.Fatal(err)
	}
	if GetProxyBindings()["a"] != proxies[0].ID {
		t.Fatal("recovered proxy did not receive the displaced account")
	}
	if err := DisableProxy(proxies[0].ID, proxies[0].URL, "connection failed again"); err != nil {
		t.Fatal(err)
	}
	if len(GetProxyBindings()) != 0 {
		t.Fatal("unhealthy proxies retained binding")
	}
	if _, _, err := ResolveAccountProxy("a"); !errors.Is(err, ErrNoAvailableProxy) {
		t.Fatalf("blocked error=%v", err)
	}
	settings := GetProxyPoolSettings()
	settings.BlockWithoutProxy = false
	if _, err := UpdateProxyPoolSettings(settings); err != nil {
		t.Fatal(err)
	}
	if _, bound, err := ResolveAccountProxy("a"); err != nil || bound {
		t.Fatalf("direct fallback=(%v,%v)", bound, err)
	}
	yes := true
	if err := UpdateProxies([]string{proxies[0].ID}, ProxyPatch{Enabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if GetProxyBindings()["a"] != proxies[0].ID {
		t.Fatal("manually enabled proxy not assigned")
	}
	settings.Enabled = false
	if _, err := UpdateProxyPoolSettings(settings); err != nil {
		t.Fatal(err)
	}
	if len(GetProxyBindings()) != 0 {
		t.Fatal("pool off retained bindings")
	}
}

func TestProxyPoolImportAndAtomicBatchEdit(t *testing.T) {
	initProxyPoolTest(t)
	proxies := addProxyPoolTest(t, 2)
	if added, skipped, err := ImportProxies([]ProxyEntry{{URL: proxies[0].URL, Enabled: true}, {URL: proxies[0].URL, Enabled: true}}); err != nil || added != 0 || skipped != 2 {
		t.Fatalf("dedup=(%d,%d,%v)", added, skipped, err)
	}
	if _, _, err := ImportProxies([]ProxyEntry{{URL: "http://good:80", Enabled: true}, {URL: "invalid"}}); err == nil {
		t.Fatal("accepted partial invalid batch")
	}
	if len(GetProxies()) != 2 {
		t.Fatal("partial import leaked")
	}
	name := "renamed"
	invalid := "invalid"
	if err := UpdateProxyPatches(map[string]ProxyPatch{proxies[0].ID: {Name: &name}, proxies[1].ID: {URL: &invalid}}); err == nil {
		t.Fatal("accepted invalid batch edit")
	}
	if GetProxies()[0].Name != proxies[0].Name {
		t.Fatal("partial batch edit leaked")
	}
	duplicate := proxies[0].URL
	if err := UpdateProxies([]string{proxies[1].ID}, ProxyPatch{URL: &duplicate}); !errors.Is(err, ErrInvalidProxy) {
		t.Fatalf("duplicate error=%v", err)
	}
	first, second := proxies[0].URL, proxies[1].URL
	if err := UpdateProxyPatches(map[string]ProxyPatch{proxies[0].ID: {URL: &second}, proxies[1].ID: {URL: &first}}); err != nil {
		t.Fatal(err)
	}
	if GetProxies()[0].URL != second || GetProxies()[1].URL != first {
		t.Fatal("atomic URL swap failed")
	}
	if err := RecordProxyCheck(proxies[0].ID, first, ProxyCheckResult{Healthy: false, LastError: "stale"}); err != nil {
		t.Fatal(err)
	}
	if !GetProxies()[0].Enabled {
		t.Fatal("stale URL probe disabled edited proxy")
	}
	if err := DeleteProxies([]string{proxies[0].ID, "missing"}); !errors.Is(err, ErrProxyNotFound) {
		t.Fatalf("delete missing error=%v", err)
	}
	if len(GetProxies()) != 2 {
		t.Fatal("partial delete leaked")
	}
}

func TestProxyPoolPersistenceRollbackIncludesAccountsAndTraffic(t *testing.T) {
	initProxyPoolTest(t)
	proxies := addProxyPoolTest(t, 2)
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	enableProxyPoolTest(t, true)
	beforeEntries, beforeBindings, beforeAccounts, beforeSettings := GetProxies(), GetProxyBindings(), GetAccounts(), GetProxyPoolSettings()
	cfgLock.Lock()
	previousPath := cfgPath
	cfgPath = t.TempDir()
	cfgLock.Unlock()
	t.Cleanup(func() { cfgLock.Lock(); cfgPath = previousPath; cfgLock.Unlock() })
	no := false
	cases := []func() error{
		func() error { s := beforeSettings; s.Enabled = false; _, err := UpdateProxyPoolSettings(s); return err },
		func() error {
			_, _, err := ImportProxies([]ProxyEntry{{URL: "http://new:8080", Enabled: true}})
			return err
		},
		func() error { return UpdateProxies([]string{proxies[0].ID}, ProxyPatch{Enabled: &no}) },
		func() error { return DeleteProxies([]string{proxies[0].ID}) },
		func() error { return RecordProxyCheck(proxies[0].ID, proxies[0].URL, ProxyCheckResult{}) },
		func() error { return DisableProxy(proxies[0].ID, proxies[0].URL, "failed") },
		func() error { return AddProxyTraffic(map[string]int64{proxies[0].ID: 1024}) },
		func() error { return SetAccountEnabled("a", false) },
		func() error { return SetAccountBanStatus("a", "BANNED", "test") },
		func() error { return DeleteAccount("a") },
		func() error { return AddAccount(Account{ID: "failed", Enabled: true}) },
	}
	for i, run := range cases {
		if err := run(); err == nil {
			t.Fatalf("case %d unexpectedly saved to directory", i)
		}
		if !reflect.DeepEqual(GetProxies(), beforeEntries) || !reflect.DeepEqual(GetProxyBindings(), beforeBindings) || !reflect.DeepEqual(GetAccounts(), beforeAccounts) || GetProxyPoolSettings() != beforeSettings {
			t.Fatalf("case %d failed rollback", i)
		}
	}
}

func TestProxyPoolCheckTimestampAndCredentialSanitization(t *testing.T) {
	initProxyPoolTest(t)
	if _, _, err := ImportProxies([]ProxyEntry{{URL: "http://alice:secret%21@127.0.0.1:18080", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	entry := GetProxies()[0]
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: true, CheckedAt: 200, LastError: "GeoIP lookup pending; connectivity verified"}); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got.Health != ProxyHealthHealthy || !got.Enabled || got.LastError == "" {
		t.Fatalf("GeoIP warning disabled healthy proxy or was dropped: %+v", got)
	}
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: false, CheckedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if !GetProxies()[0].Enabled {
		t.Fatal("older health check overwrote newer result")
	}
	if err := DisableProxy(entry.ID, entry.URL, "dial "+entry.URL+": secret! rejected"); err != nil {
		t.Fatal(err)
	}
	message := GetProxies()[0].LastError
	if strings.Contains(message, "secret") || strings.Contains(message, "alice") {
		t.Fatalf("credential leak=%q", message)
	}
}

func TestProxyPoolConcurrentBindingsAndCountersPersist(t *testing.T) {
	path := initProxyPoolTest(t)
	proxies := addProxyPoolTest(t, 2)
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	enableProxyPoolTest(t, true)
	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, _, err := ResolveAccountProxy("a")
				if err != nil {
					errCh <- err
					return
				}
				_ = GetProxyBindings()
				_ = GetProxies()
				if err := AddProxyTraffic(map[string]int64{proxies[0].ID: 1}); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := GetProxies()[0].TotalBytes; got != 200 {
		t.Fatalf("traffic=%d, want 200", got)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if GetProxies()[0].TotalBytes != 200 {
		t.Fatal("traffic did not survive restart")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.AccountProxyBindings["a"] != proxies[0].ID {
		t.Fatal("bindings not persisted")
	}
}

func TestProxyPoolAutomaticRecoveryRebindsAfterRestart(t *testing.T) {
	path := initProxyPoolTest(t)
	entry := addProxyPoolTest(t, 1)[0]
	if err := AddAccount(Account{ID: "account", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	enableProxyPoolTest(t, true)
	version := entry.StateVersion
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{ExpectedStateVersion: &version, LastError: "network unavailable"}); err != nil {
		t.Fatal(err)
	}
	failed := GetProxies()[0]
	if failed.Enabled || failed.DisabledBy != ProxyDisabledHealth || failed.StateVersion <= version || len(GetProxyBindings()) != 0 {
		t.Fatalf("failure state not recorded: %+v %v", failed, GetProxyBindings())
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got != failed {
		t.Fatalf("fault provenance changed after restart: %+v, want %+v", got, failed)
	}
	version = failed.StateVersion
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: true, ExpectedStateVersion: &version, ExitIP: "192.0.2.4", Country: "US", City: "New York"}); err != nil {
		t.Fatal(err)
	}
	recovered := GetProxies()[0]
	if !recovered.Enabled || recovered.Health != ProxyHealthHealthy || recovered.DisabledBy != "" || recovered.StateVersion <= version || recovered.ExitIP != "192.0.2.4" || GetProxyBindings()["account"] != entry.ID {
		t.Fatalf("automatic recovery did not restore assignment: %+v %v", recovered, GetProxyBindings())
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got != recovered || GetProxyBindings()["account"] != entry.ID {
		t.Fatalf("recovery not persisted: %+v %v", got, GetProxyBindings())
	}
}

func TestProxyPoolManualDisableSurvivesFailedAndHealthyChecks(t *testing.T) {
	path := initProxyPoolTest(t)
	entry := addProxyPoolTest(t, 1)[0]
	no, yes := false, true
	if err := UpdateProxies([]string{entry.ID}, ProxyPatch{Enabled: &no}); err != nil {
		t.Fatal(err)
	}
	for _, healthy := range []bool{false, true} {
		before := GetProxies()[0]
		version := before.StateVersion
		if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: healthy, ExpectedStateVersion: &version, LastError: "test observation"}); err != nil {
			t.Fatal(err)
		}
		if got := GetProxies()[0]; got.Enabled || got.DisabledBy != ProxyDisabledManual || got.StateVersion != version {
			t.Fatalf("check healthy=%v overrode manual disable: %+v", healthy, got)
		}
	}
	if err := DisableProxy(entry.ID, entry.URL, "late runtime error"); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got.DisabledBy != ProxyDisabledManual {
		t.Fatalf("runtime failure erased manual intent: %+v", got)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got.Enabled || got.DisabledBy != ProxyDisabledManual {
		t.Fatalf("manual state lost on restart: %+v", got)
	}
	if err := UpdateProxies([]string{entry.ID}, ProxyPatch{Enabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; !got.Enabled || got.DisabledBy != "" || got.Health != ProxyHealthUnknown || got.LastError != "" {
		t.Fatalf("explicit re-enable did not clear failure state: %+v", got)
	}
}

func TestProxyPoolIdempotentManualDisableOverridesAutomaticFault(t *testing.T) {
	initProxyPoolTest(t)
	entry := addProxyPoolTest(t, 1)[0]
	if err := DisableProxy(entry.ID, entry.URL, "automatic fault"); err != nil {
		t.Fatal(err)
	}
	failed := GetProxies()[0]
	no := false
	if err := UpdateProxies([]string{entry.ID}, ProxyPatch{Enabled: &no}); err != nil {
		t.Fatal(err)
	}
	manual := GetProxies()[0]
	if manual.Enabled || manual.DisabledBy != ProxyDisabledManual || manual.StateVersion <= failed.StateVersion {
		t.Fatalf("idempotent disable did not record manual intent: %+v", manual)
	}
	version := manual.StateVersion
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: true, ExpectedStateVersion: &version}); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got.Enabled || got.DisabledBy != ProxyDisabledManual || got.Health != ProxyHealthHealthy {
		t.Fatalf("healthy check overrode manual override: %+v", got)
	}
}

func TestProxyPoolLoadConservativelyMigratesDisabledProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"proxies":[
		{"id":"legacy-healthy","url":"http://proxy:1001","enabled":false,"health":"healthy"},
		{"id":"legacy-failed","url":"http://proxy:1002","enabled":false,"health":"unhealthy"},
		{"id":"known-fault","url":"http://proxy:1003","enabled":false,"health":"healthy","disabledBy":"health","stateVersion":7},
		{"id":"manual","url":"http://proxy:1004","enabled":false,"health":"healthy","disabledBy":"manual","stateVersion":8},
		{"id":"invalid","url":"ftp://proxy:1005","enabled":true,"health":"healthy"},
		{"id":"enabled","url":"http://proxy:1006","enabled":true,"health":"healthy","disabledBy":"health"}
	]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	entries := GetProxies()
	for _, index := range []int{0, 1, 3} {
		if got := entries[index]; got.Enabled || got.DisabledBy != ProxyDisabledManual || got.StateVersion == 0 {
			t.Errorf("unknown/manual disable incorrectly migrated: %+v", got)
		}
	}
	if got := entries[2]; got.Enabled || got.DisabledBy != ProxyDisabledHealth || got.StateVersion != 7 {
		t.Fatalf("saved healthy observation enabled known fault before real check: %+v", got)
	}
	if got := entries[4]; got.Enabled || got.DisabledBy != ProxyDisabledInvalid || got.Health != ProxyHealthUnhealthy {
		t.Fatalf("invalid URL not marked invalid: %+v", got)
	}
	if got := entries[5]; !got.Enabled || got.DisabledBy != "" {
		t.Fatalf("enabled proxy retained obsolete disabled provenance: %+v", got)
	}
	yes := true
	if err := UpdateProxies([]string{entries[4].ID}, ProxyPatch{Enabled: &yes}); !errors.Is(err, ErrInvalidProxy) {
		t.Fatalf("manual enable accepted an invalid proxy URL: %v", err)
	}
	for _, index := range []int{0, 1, 2, 3, 4} {
		entry := GetProxies()[index]
		version := entry.StateVersion
		if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: true, ExpectedStateVersion: &version}); err != nil {
			t.Fatal(err)
		}
		if got := GetProxies()[index]; got.Enabled != (index == 2) {
			t.Errorf("fresh check recovered wrong disable source at %d: %+v", index, got)
		}
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got.DisabledBy != ProxyDisabledManual || got.Enabled {
		t.Fatalf("legacy migration did not persist: %+v", got)
	}
}

func TestProxyPoolStateVersionRejectsSameSecondStaleResults(t *testing.T) {
	initProxyPoolTest(t)
	entry := addProxyPoolTest(t, 1)[0]
	no, yes := false, true
	if err := UpdateProxies([]string{entry.ID}, ProxyPatch{Enabled: &no}); err != nil {
		t.Fatal(err)
	}
	manual := GetProxies()[0]
	oldVersion := entry.StateVersion
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: true, CheckedAt: manual.LastChecked, ExpectedStateVersion: &oldVersion}); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got != manual {
		t.Fatalf("same-second old probe overwrote manual disable: %+v", got)
	}
	if err := UpdateProxies([]string{entry.ID}, ProxyPatch{Enabled: &no}); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got.StateVersion != manual.StateVersion+1 {
		t.Fatalf("idempotent enabled patch did not advance version: %+v", got)
	}
	if err := UpdateProxies([]string{entry.ID}, ProxyPatch{Enabled: &yes}); err != nil {
		t.Fatal(err)
	}
	enabled := GetProxies()[0]
	if err := DisableProxyIfCurrent(entry.ID, entry.URL, manual.StateVersion, "stale request error"); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got != enabled {
		t.Fatalf("same-second old request overrode manual enable: %+v", got)
	}
	if err := DisableProxyIfCurrent(entry.ID, entry.URL, enabled.StateVersion, "current fault"); err != nil {
		t.Fatal(err)
	}
	failed := GetProxies()[0]
	version := failed.StateVersion
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: true, CheckedAt: failed.LastChecked, ExpectedStateVersion: &version}); err != nil {
		t.Fatal(err)
	}
	recovered := GetProxies()[0]
	if !recovered.Enabled || recovered.DisabledBy != "" {
		t.Fatalf("current healthy check did not recover proxy: %+v", recovered)
	}
	if err := DisableProxyIfCurrent(entry.ID, entry.URL, failed.StateVersion, "stale request after recovery"); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got != recovered {
		t.Fatalf("stale request undid newer recovery: %+v", got)
	}
	if err := UpdateProxies([]string{entry.ID}, ProxyPatch{URL: &entry.URL}); err != nil {
		t.Fatal(err)
	}
	edited := GetProxies()[0]
	if edited.StateVersion != recovered.StateVersion+1 {
		t.Fatalf("idempotent URL edit did not advance version: %+v", edited)
	}
	version = recovered.StateVersion
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{ExpectedStateVersion: &version, CheckedAt: edited.LastChecked}); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got != edited {
		t.Fatalf("old check overrode same-URL edit: %+v", got)
	}
}

func TestProxyPoolRecoverySaveFailureRestoresStateAndBindings(t *testing.T) {
	initProxyPoolTest(t)
	entry := addProxyPoolTest(t, 1)[0]
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	enableProxyPoolTest(t, true)
	if err := DisableProxy(entry.ID, entry.URL, "test fault"); err != nil {
		t.Fatal(err)
	}
	before, bindings := GetProxies()[0], GetProxyBindings()
	cfgLock.Lock()
	previousPath := cfgPath
	cfgPath = t.TempDir()
	cfgLock.Unlock()
	t.Cleanup(func() { cfgLock.Lock(); cfgPath = previousPath; cfgLock.Unlock() })
	version := before.StateVersion
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: true, ExpectedStateVersion: &version}); err == nil {
		t.Fatal("recovery unexpectedly persisted to directory")
	}
	if got := GetProxies()[0]; got != before || !reflect.DeepEqual(GetProxyBindings(), bindings) {
		t.Fatalf("failed recovery leaked enabled state/version/bindings: %+v %v", got, GetProxyBindings())
	}
}

func TestProxyPoolHealthyObservationDoesNotInvalidateLiveRequest(t *testing.T) {
	initProxyPoolTest(t)
	entry := addProxyPoolTest(t, 1)[0]
	version := entry.StateVersion
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: true, ExpectedStateVersion: &version, ExitIP: "192.0.2.9"}); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got.StateVersion != entry.StateVersion || !got.Enabled || got.Health != ProxyHealthHealthy {
		t.Fatalf("metadata-only healthy probe invalidated a live request: %+v", got)
	}
	if err := DisableProxyIfCurrent(entry.ID, entry.URL, entry.StateVersion, "live request connection failed"); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got.Enabled || got.DisabledBy != ProxyDisabledHealth || got.StateVersion <= entry.StateVersion {
		t.Fatalf("genuine live request failure ignored after healthy probe: %+v", got)
	}
	failed := GetProxies()[0]
	version = failed.StateVersion
	if err := RecordProxyCheck(entry.ID, entry.URL, ProxyCheckResult{Healthy: false, ExpectedStateVersion: &version}); err != nil {
		t.Fatal(err)
	}
	if got := GetProxies()[0]; got.StateVersion != version || got.DisabledBy != ProxyDisabledHealth {
		t.Fatalf("metadata-only failed recheck changed control revision: %+v", got)
	}
}
