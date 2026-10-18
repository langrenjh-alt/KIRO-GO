package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestS2APAccountLeaseSurvivesUntilExpiryCount(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	settings, err := UpdateS2APBind(true, 12)
	if err != nil || !settings.Enabled || settings.Hours != 12 || settings.Leases != 0 {
		t.Fatalf("settings = %+v err=%v", settings, err)
	}
	until := time.Now().Add(12 * time.Hour).Unix()
	if err := PutS2APAccountLease("acct-1", S2APAccountLease{ProxyID: "p1", Until: until, ExitIP: "203.0.113.8"}); err != nil {
		t.Fatal(err)
	}
	lease, ok := GetS2APAccountLease("acct-1")
	if !ok || lease.ProxyID != "p1" || lease.ExitIP != "203.0.113.8" {
		t.Fatalf("lease = %+v ok=%v", lease, ok)
	}
	if got := GetS2APBindSettings(); got.Leases != 1 {
		t.Fatalf("leases = %+v", got)
	}
}
