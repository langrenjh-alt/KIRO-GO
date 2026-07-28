package pool

import (
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
)

func TestLeaseRespectsConcurrencyReleaseAndCopyIsolation(t *testing.T) {
	p := &AccountPool{
		accounts:  []config.Account{{ID: "a", Email: "original", MaxConcurrency: 1}},
		inFlight:  make(map[string]int),
		cooldowns: make(map[string]time.Time),
	}
	account, allFull := p.AcquireNextForModelExcluding("model", nil)
	if account == nil || allFull || p.InFlight("a") != 1 {
		t.Fatalf("first acquire = %v, allFull=%v, inFlight=%d", account, allFull, p.InFlight("a"))
	}
	account.Email = "mutated"
	if stored := p.GetByID("a"); stored == nil || stored.Email != "original" {
		t.Fatalf("lease escaped pool copy isolation: %#v", stored)
	}
	if next, full := p.AcquireNextForModelExcluding("model", nil); next != nil || !full {
		t.Fatalf("expected full account, got next=%v full=%v", next, full)
	}
	p.Release("a")
	if next, full := p.AcquireNextForModelExcluding("model", nil); next == nil || full {
		t.Fatalf("expected reusable account after release, got next=%v full=%v", next, full)
	}
}

func TestSchedulingModesAndPriorityRouting(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	leastRequests := config.SchedulingConfig{Mode: config.SchedulingModeLeastRequests}
	priority := config.AccountPriorityConfig{Enabled: true, Rules: []config.AccountPriorityRouteRule{{
		Enabled: true, Models: []string{"claude-opus"}, AccountLevels: []string{config.AccountLevelPower},
	}}}
	if err := config.UpdateDispatchPolicies(nil, &leastRequests, &priority); err != nil {
		t.Fatalf("UpdateDispatchPolicies: %v", err)
	}
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "busy-power", AccountLevel: config.AccountLevelPower, RequestCount: 1},
			{ID: "idle-pro", AccountLevel: config.AccountLevelPro, RequestCount: 100},
		},
		inFlight:  map[string]int{"busy-power": 2},
		cooldowns: make(map[string]time.Time),
	}
	if account, _ := p.AcquireNextForModelExcluding("claude-opus", nil); account == nil || account.ID != "busy-power" {
		t.Fatalf("priority route should force power account, got %v", account)
	}
	if account, _ := p.AcquireNextForModelExcluding("unrestricted", nil); account == nil || account.ID != "idle-pro" {
		t.Fatalf("least_requests should choose idle account, got %v", account)
	}

	leastCredits := config.SchedulingConfig{Mode: config.SchedulingModeLeastCredits}
	if err := config.UpdateDispatchPolicies(nil, &leastCredits, nil); err != nil {
		t.Fatalf("UpdateDispatchPolicies: %v", err)
	}
	p.accounts[0].UsageCurrent = 10
	p.accounts[1].UsageCurrent = 2
	if account := p.GetNextForModel("unrestricted"); account == nil || account.ID != "idle-pro" {
		t.Fatalf("least_credits should choose lower usage, got %v", account)
	}
}

func TestAvailabilitySeparatesExpiredFullAndSelectableAccounts(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "expired", ExpiresAt: time.Now().Add(time.Minute).Unix()},
			{ID: "full", MaxConcurrency: 1},
			{ID: "ready"},
		},
		inFlight:  map[string]int{"full": 1},
		cooldowns: make(map[string]time.Time),
	}

	snapshot := p.InspectAvailability("model", nil)
	if snapshot.Expired != 1 || snapshot.Full != 1 || snapshot.Selectable != 1 || len(snapshot.ExpiredAccounts) != 1 || snapshot.ExpiredAccounts[0].ID != "expired" {
		t.Fatalf("availability snapshot = %+v", snapshot)
	}
	account, allFull := p.AcquireNextForModelExcluding("model", nil)
	if account == nil || account.ID != "ready" || allFull {
		t.Fatalf("expected ready account, got account=%v allFull=%v", account, allFull)
	}
	p.Release(account.ID)
}
