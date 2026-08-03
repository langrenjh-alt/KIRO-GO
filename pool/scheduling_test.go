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

func TestRefillEligibleCountIgnoresTransientConcurrencyAndCooldown(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "leased", MaxConcurrency: 1},
			{ID: "cooling"},
			{ID: "leased"}, // weighted duplicate must not inflate capacity
		},
		inFlight: map[string]int{"leased": 1},
		cooldowns: map[string]time.Time{
			"cooling": time.Now().Add(time.Minute),
		},
	}

	if got := p.AvailableCount(); got != 0 {
		t.Fatalf("runtime available = %d, want 0", got)
	}
	if got := p.RefillEligibleCount(); got != 2 {
		t.Fatalf("refill eligible = %d, want 2", got)
	}
}

func TestRuntimeStatesMirrorAvailableCount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "ready"},
			{ID: "cooling"},
			{ID: "full", MaxConcurrency: 1},
			{ID: "full"}, // weighted duplicate must not change the state count
		},
		inFlight: map[string]int{"full": 1},
		cooldowns: map[string]time.Time{
			"cooling": time.Now().Add(time.Minute),
		},
	}

	states := p.RuntimeStates()
	if len(states) != 3 {
		t.Fatalf("runtime states = %#v, want three unique accounts", states)
	}
	if !states["ready"].Available || states["cooling"].Reason != "cooldown" || states["full"].Reason != "concurrency_full" {
		t.Fatalf("runtime states = %#v", states)
	}
	available := 0
	for _, state := range states {
		if state.Available {
			available++
		}
	}
	if available != p.AvailableCount() {
		t.Fatalf("runtime state available = %d, available count = %d", available, p.AvailableCount())
	}
}

func TestAcquireForTestBypassesCooldownButHonorsConcurrency(t *testing.T) {
	account := config.Account{ID: "test-account", MaxConcurrency: 1}
	p := &AccountPool{
		accounts:   []config.Account{account},
		inFlight:   make(map[string]int),
		cooldowns:  map[string]time.Time{account.ID: time.Now().Add(time.Minute)},
		modelLists: map[string]map[string]bool{account.ID: {"another-model": true}},
	}
	leased, full := p.AcquireForTest(&account)
	if leased == nil || full {
		t.Fatalf("first test lease=%#v full=%v", leased, full)
	}
	if second, full := p.AcquireForTest(&account); second != nil || !full {
		t.Fatalf("second test lease=%#v full=%v, want concurrency rejection", second, full)
	}
	p.Release(account.ID)
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
