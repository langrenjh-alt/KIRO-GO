package config

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestTokenBillingInflationDefaultsToZero(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "new config"
		if legacy {
			name = "legacy config"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if legacy {
				if err := os.WriteFile(path, []byte(`{"port":8080,"accounts":[]}`), 0600); err != nil {
					t.Fatalf("write legacy config: %v", err)
				}
			}
			if err := Init(path); err != nil {
				t.Fatalf("init config: %v", err)
			}
			t.Cleanup(func() { _ = Close() })
			if got := GetTokenBillingInflationPercent(); got != 0 {
				t.Fatalf("default inflation=%v, want 0", got)
			}
		})
	}
}

func TestTokenBillingInflationAcceptsBoundariesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	for _, percent := range []float64{99999, 12.5, 0} {
		if err := UpdateTokenBillingInflationPercent(percent); err != nil {
			t.Fatalf("update inflation to %v: %v", percent, err)
		}
		if got := GetTokenBillingInflationPercent(); got != percent {
			t.Fatalf("inflation=%v, want %v", got, percent)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read saved config: %v", err)
		}
		var persisted struct {
			Percent float64 `json:"tokenBillingInflationPercent"`
		}
		if err := json.Unmarshal(data, &persisted); err != nil {
			t.Fatalf("decode saved config: %v", err)
		}
		if persisted.Percent != percent {
			t.Fatalf("persisted inflation=%v, want %v", persisted.Percent, percent)
		}
		if err := Init(path); err != nil {
			t.Fatalf("reload config: %v", err)
		}
		if got := GetTokenBillingInflationPercent(); got != percent {
			t.Fatalf("reloaded inflation=%v, want %v", got, percent)
		}
	}
}

func TestTokenBillingInflationRejectsInvalidValuesWithoutChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	const original = 12.5
	if err := UpdateTokenBillingInflationPercent(original); err != nil {
		t.Fatalf("seed inflation: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	for _, percent := range []float64{-0.001, 99999.001, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := UpdateTokenBillingInflationPercent(percent); err == nil {
			t.Fatalf("invalid inflation %v was accepted", percent)
		}
		if got := GetTokenBillingInflationPercent(); got != original {
			t.Fatalf("invalid update changed inflation to %v", got)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read config after invalid update: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("invalid inflation %v changed persisted config", percent)
		}
	}
}

func TestTokenBillingInflationRollsBackOnSaveFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	const original = 12.5
	if err := UpdateTokenBillingInflationPercent(original); err != nil {
		t.Fatalf("seed inflation: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}

	cfgLock.Lock()
	originalPath := cfgPath
	cfgPath = t.TempDir() // An atomic rename cannot replace a directory.
	cfgLock.Unlock()
	t.Cleanup(func() {
		cfgLock.Lock()
		cfgPath = originalPath
		cfgLock.Unlock()
	})
	if err := UpdateTokenBillingInflationPercent(100); err == nil {
		t.Fatal("expected inflation persistence failure")
	}
	if got := GetTokenBillingInflationPercent(); got != original {
		t.Fatalf("failed save changed inflation to %v", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config after failed save: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed save changed persisted config")
	}
}

func TestTokenBillingInflationRejectsInvalidLoadedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	if err := UpdateTokenBillingInflationPercent(12.5); err != nil {
		t.Fatalf("seed inflation: %v", err)
	}
	invalidPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(invalidPath, []byte(`{"tokenBillingInflationPercent":100000}`), 0600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	if err := Init(invalidPath); err == nil {
		t.Fatal("expected out-of-range config inflation to fail")
	}
	if got := GetTokenBillingInflationPercent(); got != 12.5 {
		t.Fatalf("failed load replaced inflation with %v", got)
	}
}
