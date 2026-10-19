package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// This deterministic probe is also built against the archived baseline to
// compare the same failure/recovery sequence without real proxy credentials.
func TestProxyRecoveryReleaseProbe(t *testing.T) {
	mode := os.Getenv("RECOVERY_PROBE_MODE")
	if mode == "" {
		mode = "MODIFIED"
	}
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ImportProxies([]ProxyEntry{{URL: "http://fixture.invalid:8080", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "fixture-power", Enabled: true, AccountLevel: "power"}); err != nil {
		t.Fatal(err)
	}
	settings := GetProxyPoolSettings()
	settings.Enabled = true
	if _, err := UpdateProxyPoolSettings(settings); err != nil {
		t.Fatal(err)
	}
	p := GetProxies()[0]
	if err := RecordProxyCheck(p.ID, p.URL, ProxyCheckResult{LastError: "fixture network failure"}); err != nil {
		t.Fatal(err)
	}
	faultEnabled := GetProxies()[0].Enabled
	if err := RecordProxyCheck(p.ID, p.URL, ProxyCheckResult{Healthy: true, ExitIP: "203.0.113.7", Country: "US", City: "Fixture"}); err != nil {
		t.Fatal(err)
	}
	recoveredEnabled, recoveredBindings := GetProxies()[0].Enabled, len(GetProxyBindings())
	no := false
	if err := UpdateProxies([]string{p.ID}, ProxyPatch{Enabled: &no}); err != nil {
		t.Fatal(err)
	}
	if err := RecordProxyCheck(p.ID, p.URL, ProxyCheckResult{Healthy: true}); err != nil {
		t.Fatal(err)
	}
	manualEnabled := GetProxies()[0].Enabled
	expectRecovery := mode == "MODIFIED"
	if faultEnabled || recoveredEnabled != expectRecovery || manualEnabled || recoveredBindings != map[bool]int{true: 1, false: 0}[expectRecovery] {
		t.Fatalf("mode=%s fault=%v recovered=%v manual=%v bindings=%d", mode, faultEnabled, recoveredEnabled, manualEnabled, recoveredBindings)
	}
	fmt.Printf("%s: fault_enabled=%v recovered_enabled=%v manual_enabled=%v recovered_bindings=%d exit=0\n", mode, faultEnabled, recoveredEnabled, manualEnabled, recoveredBindings)
}
