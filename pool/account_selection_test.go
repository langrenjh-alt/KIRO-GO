package pool

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"kiro-go/config"
)

var benchmarkSelectedAccount *config.Account

func TestReloadBuildsUniqueIndexesForWeightedAccounts(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, account := range []config.Account{
		{ID: "weighted-a", Enabled: true, Weight: 3},
		{ID: "weighted-b", Enabled: true, Weight: 2},
	} {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("config.AddAccount(%s): %v", account.ID, err)
		}
	}
	p := &AccountPool{
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
		inFlight:    make(map[string]int),
	}
	p.Reload()
	if len(p.accounts) != 5 {
		t.Fatalf("weighted account count = %d, want 5", len(p.accounts))
	}
	if len(p.uniqueIndexes) != 2 || p.uniqueIndexes[0] != 0 || p.uniqueIndexes[1] != 3 {
		t.Fatalf("unique indexes = %v, want [0 3]", p.uniqueIndexes)
	}
	if got := p.accountIndexes["weighted-a"]; len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("weighted-a indexes = %v, want [0 1 2]", got)
	}
	if account, allFull := p.selectNextLocked("", nil, true, config.SchedulingModeLeastRequests, accountSelectionPolicy{allowOverUsage: true}); account == nil || allFull {
		t.Fatalf("indexed selection=(%v,%v), want an available account", account, allFull)
	}
	p.UpdateStats("weighted-a", 12, 1.5)
	for _, index := range p.accountIndexes["weighted-a"] {
		account := p.accounts[index]
		if account.RequestCount != 1 || account.TotalTokens != 12 || account.TotalCredits != 1.5 || account.LastUsed == 0 {
			t.Fatalf("weighted copy %d did not receive indexed stats update: %+v", index, account)
		}
	}
}

func TestSinglePassSelectorsMatchStableSortReference(t *testing.T) {
	rng := rand.New(rand.NewSource(20260805))
	modes := []string{
		config.SchedulingModeLeastRequests,
		config.SchedulingModeLeastCredits,
		config.SchedulingModeLoadBalance,
	}
	selection := accountSelectionPolicy{allowOverUsage: true}

	for iteration := 0; iteration < 250; iteration++ {
		uniqueCount := 1 + rng.Intn(40)
		accounts := make([]config.Account, 0, uniqueCount*3)
		inFlight := make(map[string]int, uniqueCount)
		excluded := make(map[string]bool)
		for i := 0; i < uniqueCount; i++ {
			id := fmt.Sprintf("account-%d", i)
			account := config.Account{
				ID:             id,
				Weight:         1 + rng.Intn(5),
				MaxConcurrency: rng.Intn(5),
				RequestCount:   rng.Intn(1000),
				LastUsed:       int64(rng.Intn(100)),
				UsageCurrent:   float64(rng.Intn(50)) / 4,
				TotalCredits:   float64(rng.Intn(50)) / 4,
			}
			copies := 1 + rng.Intn(3)
			for copyIndex := 0; copyIndex < copies; copyIndex++ {
				accounts = append(accounts, account)
			}
			inFlight[id] = rng.Intn(5)
			if rng.Intn(8) == 0 {
				excluded[id] = true
			}
		}

		p := &AccountPool{
			accounts:   accounts,
			inFlight:   inFlight,
			cooldowns:  make(map[string]time.Time),
			modelLists: make(map[string]map[string]bool),
		}
		respectConcurrency := iteration%2 == 0
		for _, mode := range modes {
			got, gotFull := p.selectNextLocked("", excluded, respectConcurrency, mode, selection)
			want, wantFull := legacyStableSelectForTest(p, "", excluded, respectConcurrency, mode, selection)
			if accountID(got) != accountID(want) || gotFull != wantFull {
				t.Fatalf("iteration=%d mode=%s got=(%s,%v) want=(%s,%v)",
					iteration, mode, accountID(got), gotFull, accountID(want), wantFull)
			}
		}
	}
}

func TestSinglePassSelectorsPreserveTieBreakers(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		accounts []config.Account
		inFlight map[string]int
		wantID   string
	}{
		{
			name: "least requests",
			mode: config.SchedulingModeLeastRequests,
			accounts: []config.Account{
				{ID: "busy", LastUsed: 1, RequestCount: 1, Weight: 5},
				{ID: "recent", LastUsed: 20, RequestCount: 0, Weight: 5},
				{ID: "many-requests", LastUsed: 10, RequestCount: 50, Weight: 5},
				{ID: "low-weight", LastUsed: 10, RequestCount: 5, Weight: 1},
				{ID: "winner", LastUsed: 10, RequestCount: 5, Weight: 3},
				{ID: "stable-later", LastUsed: 10, RequestCount: 5, Weight: 3},
				{ID: "winner", LastUsed: 0, RequestCount: 0, Weight: 100}, // duplicate ID is ignored
			},
			inFlight: map[string]int{"busy": 1},
			wantID:   "winner",
		},
		{
			name: "least credits",
			mode: config.SchedulingModeLeastCredits,
			accounts: []config.Account{
				{ID: "higher-usage", UsageCurrent: 2},
				{ID: "busy", UsageCurrent: 1, LastUsed: 1},
				{ID: "recent", UsageCurrent: 1, LastUsed: 20},
				{ID: "winner", UsageCurrent: 1, LastUsed: 10},
				{ID: "stable-later", UsageCurrent: 1, LastUsed: 10},
			},
			inFlight: map[string]int{"busy": 2},
			wantID:   "winner",
		},
		{
			name: "load balance",
			mode: config.SchedulingModeLoadBalance,
			accounts: []config.Account{
				{ID: "higher-score", UsageCurrent: 10, Weight: 1},
				{ID: "busy", UsageCurrent: 8, Weight: 2, LastUsed: 1},
				{ID: "recent", UsageCurrent: 4, Weight: 1, LastUsed: 20},
				{ID: "many-requests", UsageCurrent: 4, Weight: 1, LastUsed: 10, RequestCount: 50},
				{ID: "winner", UsageCurrent: 4, Weight: 1, LastUsed: 10, RequestCount: 5},
				{ID: "stable-later", UsageCurrent: 4, Weight: 1, LastUsed: 10, RequestCount: 5},
			},
			inFlight: map[string]int{"busy": 2},
			wantID:   "winner",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &AccountPool{
				accounts:   tc.accounts,
				inFlight:   tc.inFlight,
				cooldowns:  make(map[string]time.Time),
				modelLists: make(map[string]map[string]bool),
			}
			got, allFull := p.selectNextLocked("", nil, true, tc.mode, accountSelectionPolicy{allowOverUsage: true})
			if got == nil || got.ID != tc.wantID || allFull {
				t.Fatalf("selection=(%v,%v), want account %q", got, allFull, tc.wantID)
			}
		})
	}
}

func TestSinglePassSelectorsPreserveAllFullSemantics(t *testing.T) {
	modes := []string{
		config.SchedulingModeLeastRequests,
		config.SchedulingModeLeastCredits,
		config.SchedulingModeLoadBalance,
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			p := &AccountPool{
				accounts: []config.Account{
					{ID: "a", MaxConcurrency: 1},
					{ID: "a", MaxConcurrency: 1},
					{ID: "b", MaxConcurrency: 2},
				},
				inFlight:   map[string]int{"a": 1, "b": 2},
				cooldowns:  make(map[string]time.Time),
				modelLists: make(map[string]map[string]bool),
			}
			selection := accountSelectionPolicy{allowOverUsage: true}
			if account, allFull := p.selectNextLocked("", nil, true, mode, selection); account != nil || !allFull {
				t.Fatalf("all-full selection=(%v,%v), want (nil,true)", account, allFull)
			}
			p.inFlight["b"] = 1
			if account, allFull := p.selectNextLocked("", nil, true, mode, selection); account == nil || account.ID != "b" || allFull {
				t.Fatalf("available selection=(%v,%v), want account b", account, allFull)
			}
			p.cooldowns["a"] = time.Now().Add(time.Minute)
			p.cooldowns["b"] = time.Now().Add(time.Minute)
			if account, allFull := p.selectNextLocked("", nil, true, mode, selection); account != nil || allFull {
				t.Fatalf("cooldown-only selection=(%v,%v), want (nil,false)", account, allFull)
			}
		})
	}
}

func TestAccountSelectionPolicyMatchesConfiguredPriorityRules(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	originalPriority := config.GetAccountPriorityConfig()
	t.Cleanup(func() {
		_ = config.UpdateDispatchPolicies(nil, nil, &originalPriority)
	})
	priority := config.AccountPriorityConfig{
		Enabled: true,
		Rules: []config.AccountPriorityRouteRule{
			{Enabled: false, Models: []string{"claude-opus"}, AccountLevels: []string{config.AccountLevelFree}},
			{Enabled: true, Models: []string{"claude-opus"}, AccountLevels: []string{config.AccountLevelPro, config.AccountLevelPower}},
			{Enabled: true, Models: []string{"*"}, AccountLevels: []string{config.AccountLevelFree}},
		},
	}
	if err := config.UpdateDispatchPolicies(nil, nil, &priority); err != nil {
		t.Fatalf("UpdateDispatchPolicies: %v", err)
	}

	configured := config.GetAccountPriorityConfig()
	models := []string{"claude-opus", "another-model", ""}
	levels := []string{
		config.AccountLevelFree,
		config.AccountLevelPro,
		config.AccountLevelProPlus,
		config.AccountLevelPower,
		config.AccountLevelUnknown,
	}
	for _, model := range models {
		selection := loadAccountSelectionPolicy(model)
		for _, level := range levels {
			account := config.Account{ID: level, AccountLevel: level}
			got := selection.accountAllowed(&account)
			want := config.AccountAllowedForModel(model, account, configured)
			if got != want {
				t.Fatalf("model=%q level=%q allowed=%v, want %v", model, level, got, want)
			}
		}
	}
}

func BenchmarkAccountSelection(b *testing.B) {
	modes := []string{
		config.SchedulingModeLeastRequests,
		config.SchedulingModeLeastCredits,
		config.SchedulingModeLoadBalance,
	}
	for _, size := range []int{16, 128, 512} {
		accounts := make([]config.Account, 0, size*2)
		uniqueIndexes := make([]int, 0, size)
		inFlight := make(map[string]int, size)
		for i := 0; i < size; i++ {
			account := config.Account{
				ID:             fmt.Sprintf("account-%d", i),
				Weight:         1 + i%5,
				MaxConcurrency: 8,
				RequestCount:   (i * 17) % 1000,
				LastUsed:       int64((i * 13) % 100),
				UsageCurrent:   float64((i*19)%500) / 10,
			}
			uniqueIndexes = append(uniqueIndexes, len(accounts))
			accounts = append(accounts, account, account)
			inFlight[account.ID] = i % 7
		}
		p := &AccountPool{
			accounts:      accounts,
			uniqueIndexes: uniqueIndexes,
			inFlight:      inFlight,
			cooldowns:     make(map[string]time.Time),
			modelLists:    make(map[string]map[string]bool),
		}
		selection := accountSelectionPolicy{allowOverUsage: true}
		for _, mode := range modes {
			b.Run(fmt.Sprintf("%s/%d/single_pass", mode, size), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					benchmarkSelectedAccount, _ = p.selectNextLocked("", nil, true, mode, selection)
				}
			})
			b.Run(fmt.Sprintf("%s/%d/legacy_stable_sort", mode, size), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					benchmarkSelectedAccount, _ = legacyStableSelectForTest(p, "", nil, true, mode, selection)
				}
			})
		}
	}
}

func BenchmarkAccountStatsUpdate(b *testing.B) {
	const accountCount = 512
	accounts := make([]config.Account, 0, accountCount*2)
	accountIndexes := make(map[string][]int, accountCount)
	for i := 0; i < accountCount; i++ {
		id := fmt.Sprintf("account-%d", i)
		for copyIndex := 0; copyIndex < 2; copyIndex++ {
			accountIndexes[id] = append(accountIndexes[id], len(accounts))
			accounts = append(accounts, config.Account{ID: id})
		}
	}
	targetID := fmt.Sprintf("account-%d", accountCount-1)
	b.Run("indexed", func(b *testing.B) {
		p := &AccountPool{accounts: append([]config.Account(nil), accounts...), accountIndexes: accountIndexes}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			p.UpdateStats(targetID, 1, 0.25)
		}
	})
	b.Run("legacy_weighted_scan", func(b *testing.B) {
		p := &AccountPool{accounts: append([]config.Account(nil), accounts...)}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			legacyUpdateStatsForTest(p, targetID, 1, 0.25)
		}
	})
}

func legacyStableSelectForTest(p *AccountPool, model string, excluded map[string]bool, respectConcurrency bool, mode string, selection accountSelectionPolicy) (*config.Account, bool) {
	now := time.Now()
	seen := make(map[string]bool)
	candidates := make([]*config.Account, 0)
	eligible := 0
	full := 0
	for i := range p.accounts {
		account := &p.accounts[i]
		if seen[account.ID] {
			continue
		}
		seen[account.ID] = true
		if !p.isSelectableLocked(account, model, excluded, now, selection) {
			continue
		}
		eligible++
		if respectConcurrency && p.isConcurrencyFullLocked(account) {
			full++
			continue
		}
		candidates = append(candidates, account)
	}
	if len(candidates) == 0 {
		return nil, eligible > 0 && full == eligible
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return p.accountRanksBeforeLocked(candidates[i], candidates[j], mode)
	})
	return candidates[0], false
}

func accountID(account *config.Account) string {
	if account == nil {
		return ""
	}
	return account.ID
}

func legacyUpdateStatsForTest(p *AccountPool, id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var updated bool
	var requestCount, errorCount, totalTokens int
	var totalCredits float64
	var lastUsed int64
	for i := range p.accounts {
		if p.accounts[i].ID != id {
			continue
		}
		if !updated {
			p.accounts[i].RequestCount++
			p.accounts[i].TotalTokens += tokens
			p.accounts[i].TotalCredits += credits
			p.accounts[i].LastUsed = time.Now().Unix()
			requestCount = p.accounts[i].RequestCount
			errorCount = p.accounts[i].ErrorCount
			totalTokens = p.accounts[i].TotalTokens
			totalCredits = p.accounts[i].TotalCredits
			lastUsed = p.accounts[i].LastUsed
			updated = true
			continue
		}
		p.accounts[i].RequestCount = requestCount
		p.accounts[i].ErrorCount = errorCount
		p.accounts[i].TotalTokens = totalTokens
		p.accounts[i].TotalCredits = totalCredits
		p.accounts[i].LastUsed = lastUsed
	}
}
