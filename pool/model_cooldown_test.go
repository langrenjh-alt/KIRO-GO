package pool

import (
	"testing"
	"time"

	"kiro-go/config"
)

func TestModelCooldownSkipsOnlyTheCooledModel(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		inFlight: make(map[string]int),
	}
	p.CooldownAccountModel("a", "claude-opus-5-5", time.Minute)

	got, _ := p.AcquireNextForModelExcluding("claude-opus-5.5", nil)
	if got == nil || got.ID != "b" {
		t.Fatalf("opus 5.5 selection = %#v, want account b", got)
	}
	p.Release("b")

	got, _ = p.AcquireNextForModelExcluding("claude-opus-4.8", map[string]bool{"b": true})
	if got == nil || got.ID != "a" {
		t.Fatalf("opus 4.8 selection = %#v, want account a", got)
	}
	p.Release(got.ID)

	p.CooldownAccountModel("b", "claude-opus-5.5", time.Minute)
	if !p.AllEligibleAccountsModelCooling("claude-opus-5.5") {
		t.Fatal("expected every account to be cooling for opus 5.5")
	}
	if p.AllEligibleAccountsModelCooling("claude-opus-4.8") {
		t.Fatal("opus 4.8 should remain available")
	}
	if got, _ = p.AcquireNextForModelExcluding("claude-opus-5.5", nil); got != nil {
		t.Fatalf("cooled model still selected %#v", got)
	}
}
