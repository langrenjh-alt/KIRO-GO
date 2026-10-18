package config

import (
	"path/filepath"
	"testing"
)

func TestSystemVersionForMachineIsStableAndDiverse(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	cfgLock.Lock()
	cfg.SystemVersion = ""
	cfgLock.Unlock()

	first := SystemVersionForMachine("machine-a")
	if SystemVersionForMachine("machine-a") != first {
		t.Fatal("the same machine id changed operating system")
	}
	if SystemVersionForMachine("") == "" {
		t.Fatal("a call without a machine id lost the host default")
	}
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		seen[SystemVersionForMachine(string(rune('a'+i%26))+string(rune('0'+i%10)))] = true
	}
	if len(seen) < 2 {
		t.Fatalf("every machine advertised the same OS: %#v", seen)
	}

	cfgLock.Lock()
	cfg.SystemVersion = "darwin#24.6.0"
	cfgLock.Unlock()
	t.Cleanup(func() {
		cfgLock.Lock()
		cfg.SystemVersion = ""
		cfgLock.Unlock()
	})
	if got := SystemVersionForMachine("machine-a"); got != "darwin#24.6.0" {
		t.Fatalf("pinned systemVersion = %s", got)
	}
}
