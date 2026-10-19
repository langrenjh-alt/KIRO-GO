package costledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

func at(seconds int) time.Time { return epoch.Add(time.Duration(seconds) * time.Second) }
func credit(seconds int, used float64) CreditSnapshot {
	return CreditSnapshot{At: at(seconds), PeriodKey: "cycle-A", Used: used, Limit: 100, Components: map[string]float64{"base": used}}
}
func record(t *testing.T, l *Ledger, id string, seconds int, usd float64) {
	t.Helper()
	if err := l.RecordDelivery("a", id, "claude-opus-5-5", Usage{OutputTokens: int64(usd * 1_000_000 / 20)}, at(seconds)); err != nil {
		t.Fatal(err)
	}
}
func observe(t *testing.T, l *Ledger, s CreditSnapshot) {
	t.Helper()
	if err := l.ObserveCredits("a", s); err != nil {
		t.Fatal(err)
	}
}
func near(t *testing.T, actual, want float64) {
	t.Helper()
	if math.Abs(actual-want) > 1e-9*math.Max(1, math.Abs(want)) {
		t.Fatalf("got %.12f want %.12f", actual, want)
	}
}

func TestCatalogFinalCategoriesAndAliases(t *testing.T) {
	c := DefaultCatalog()
	u := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000, CacheWrite5mTokens: 1_000_000, CacheWrite1hTokens: 1_000_000}
	for _, name := range []string{"claude-opus-5-5", "claude-opus-5.5"} {
		value, err := c.Quote(name, u)
		if err != nil {
			t.Fatal(err)
		}
		near(t, value, 37.2)
	}
	for _, name := range []string{"claude-opus-4-5", "claude-opus-4-5-20251101", "claude-opus-4.5"} {
		value, err := c.Quote(name, u)
		if err != nil {
			t.Fatal(err)
		}
		near(t, value, 46.75)
	}
	for _, name := range []string{"auto", "claude-opus-99", "claude-opus-5-5-fake", "claude-opus-5-5-20990101", "CLAUDE-OPUS-5-5"} {
		if _, err := c.Quote(name, u); !errors.Is(err, ErrUnknownPrice) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := c.Quote("claude-opus-5-5", Usage{InputTokens: -1}); !errors.Is(err, ErrInvalidUsage) {
		t.Fatal(err)
	}
	value, err := c.Quote("claude-sonnet-5-5", u)
	if err != nil {
		t.Fatal(err)
	}
	near(t, value, 18.6)
	// Haiku5.5's all-token tier counts cached prompt tokens but NOT output.
	value, _ = c.Quote("claude-haiku-5-5", Usage{InputTokens: 100_000, OutputTokens: 1_000_000})
	near(t, value, .51)
	value, _ = c.Quote("claude-haiku-5-5", Usage{InputTokens: 100_000, CacheReadTokens: 1, OutputTokens: 1_000_000})
	near(t, value, 2.55000005)
}

func TestNonOverlappingLearningAndLifetimeVersusPeriod(t *testing.T) {
	l := NewMemory()
	defer l.Close()
	observe(t, l, credit(0, 0))
	record(t, l, "one", 30, 1)
	observe(t, l, credit(60, 1))
	e := l.Snapshot("a", at(60))
	if e.EstimatedTotalUSD == nil || e.SampleCount != 1 {
		t.Fatalf("%+v", e)
	}
	near(t, *e.EstimatedTotalUSD, 100)
	// A new equal-size interval costs twice as much; the first interval is not
	// counted again. Half-life and proportional consumption determine weighting.
	record(t, l, "two", 90, 2)
	observe(t, l, credit(120, 2))
	e = l.Snapshot("a", at(120))
	if e.SampleCount != 2 {
		t.Fatal(e.SampleCount)
	}
	decay := math.Exp2(-1.0 / 60)
	near(t, *e.EstimatedTotalUSD, (100*decay+200)/(decay+1))
	near(t, e.SpentUSD, 3)
	reset := credit(180, 0)
	reset.PeriodKey = "cycle-B"
	observe(t, l, reset)
	e = l.Snapshot("a", at(180))
	near(t, e.SpentUSD, 3)
	near(t, e.PeriodSpentUSD, 0)
	if e.EstimatedTotalUSD != nil || e.SampleCount != 0 {
		t.Fatalf("%+v", e)
	}
}

func TestMicroDeltaAndShortIntervalsDoNotAdvanceAnchor(t *testing.T) {
	l := NewMemory()
	defer l.Close()
	observe(t, l, credit(0, 0))
	record(t, l, "one", 10, 1)
	observe(t, l, credit(30, .05))
	observe(t, l, credit(60, .09))
	if e := l.Snapshot("a", at(60)); e.SampleCount != 0 || e.EstimatedTotalUSD != nil {
		t.Fatalf("%+v", e)
	}
	observe(t, l, credit(90, .1))
	e := l.Snapshot("a", at(90))
	if e.SampleCount != 1 {
		t.Fatalf("%+v", e)
	}
	near(t, *e.EstimatedTotalUSD, 1000)
}

func TestUnknownPriceIntervalCannotTrainAsZero(t *testing.T) {
	l := NewMemory()
	defer l.Close()
	observe(t, l, credit(0, 0))
	record(t, l, "known", 10, 1)
	err := l.RecordDelivery("a", "unknown", "not-priced", Usage{OutputTokens: 3}, at(20))
	if !errors.Is(err, ErrUnknownPrice) {
		t.Fatal(err)
	}
	observe(t, l, credit(60, 1))
	e := l.Snapshot("a", at(60))
	if e.SpentComplete || e.UnpricedRequests != 1 || e.EstimatedTotalUSD != nil || e.SampleCount != 0 {
		t.Fatalf("%+v", e)
	}
	near(t, e.SpentUSD, 1)
	// The same delivery ID is consumed, even after a failed quote.
	if err = l.RecordDelivery("a", "unknown", "claude-opus-5-5", Usage{OutputTokens: 50_000}, at(65)); err != nil {
		t.Fatal(err)
	}
	record(t, l, "next", 70, 1)
	observe(t, l, credit(120, 2))
	e = l.Snapshot("a", at(120))
	if e.EstimatedTotalUSD == nil || e.SampleCount != 1 || e.SpentComplete {
		t.Fatalf("%+v", e)
	}
	near(t, *e.EstimatedTotalUSD, 100)
}

func TestEpochChangesAndComponentRollbackClearEstimate(t *testing.T) {
	for _, kind := range []string{"period", "limit", "aggregate_rollback", "component_rollback", "component_set"} {
		t.Run(kind, func(t *testing.T) {
			l := NewMemory()
			defer l.Close()
			initial := credit(0, 0)
			initial.Components = map[string]float64{"base": 0, "trial": 0}
			observe(t, l, initial)
			record(t, l, "one", 10, 1)
			s := credit(60, 2)
			s.Components = map[string]float64{"base": 1, "trial": 1}
			observe(t, l, s)
			changed := credit(120, 3)
			changed.Components = map[string]float64{"base": 2, "trial": 1}
			switch kind {
			case "period":
				changed.PeriodKey = "B"
			case "limit":
				changed.Limit = 200
			case "aggregate_rollback":
				changed.Used = 1
			case "component_rollback":
				changed.Components["trial"] = .5
			case "component_set":
				changed.Components = map[string]float64{"base": 2, "bonus": 1}
			}
			observe(t, l, changed)
			e := l.Snapshot("a", at(120))
			if e.EstimatedTotalUSD != nil || e.SampleCount != 0 || e.EstimateStatus != "credit_epoch_changed" {
				t.Fatalf("%+v", e)
			}
			near(t, e.SpentUSD, 1)
		})
	}
}

func TestOutOfOrderSnapshotAndFutureDeliveryNotMatched(t *testing.T) {
	l := NewMemory()
	defer l.Close()
	observe(t, l, credit(0, 0))
	record(t, l, "future", 120, 1)
	if err := l.ObserveCredits("a", credit(60, 1)); !errors.Is(err, ErrOutOfOrder) {
		t.Fatal(err)
	}
	e := l.Snapshot("a", at(120))
	if e.CreditUsed != 0 || e.SampleCount != 0 {
		t.Fatalf("%+v", e)
	}
	observe(t, l, credit(180, 1))
	before, _ := json.Marshal(l.Snapshot("a", at(180)))
	if err := l.ObserveCredits("a", credit(150, 3)); !errors.Is(err, ErrOutOfOrder) {
		t.Fatal(err)
	}
	after, _ := json.Marshal(l.Snapshot("a", at(180)))
	if string(before) != string(after) {
		t.Fatal("stale snapshot mutated state")
	}
	if err := l.RecordDelivery("a", "late", "claude-opus-5-5", Usage{OutputTokens: 50_000}, at(100)); !errors.Is(err, ErrOutOfOrder) {
		t.Fatal(err)
	}
	observe(t, l, credit(240, 2))
	if e = l.Snapshot("a", at(240)); e.EstimatedTotalUSD != nil || e.SampleCount != 0 {
		t.Fatalf("%+v", e)
	}
	if e.SpentComplete {
		t.Fatal("tainted delivery claimed complete lifetime tracking")
	}
	near(t, e.SpentUSD, 2)
}

func TestZeroCostCreditsInvalidNumbersAndStaleness(t *testing.T) {
	l := NewMemory()
	defer l.Close()
	observe(t, l, credit(0, 0))
	observe(t, l, credit(60, 1))
	if e := l.Snapshot("a", at(60)); e.EstimateStatus != "unattributed_credit_usage" || e.EstimatedTotalUSD != nil {
		t.Fatalf("%+v", e)
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		s := credit(120, 2)
		s.Used = bad
		if err := l.ObserveCredits("a", s); err == nil {
			t.Fatal("invalid number accepted")
		}
	}
	record(t, l, "one", 90, 1)
	observe(t, l, credit(120, 2))
	if e := l.Snapshot("a", at(120+7*3600)); e.EstimatedTotalUSD != nil || e.EstimateStatus != "stale" {
		t.Fatalf("%+v", e)
	}
	bytes, _ := json.Marshal(l.Snapshot("empty", at(120)))
	if !l.Snapshot("empty", at(120)).TrackingSince.IsZero() {
		t.Fatal("untracked account given synthetic tracking start")
	}
	if !strings.Contains(string(bytes), `"estimatedTotalUSD":null`) || !strings.Contains(string(bytes), `"currency":"USD"`) {
		t.Fatal(string(bytes))
	}
}

func TestSnapshotInputMapCopiedAndDedupBounded(t *testing.T) {
	l, err := NewWithOptions(Options{MaxRequestIDs: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := credit(0, 0)
	observe(t, l, s)
	s.Components["base"] = 999
	record(t, l, "one", 10, 1)
	record(t, l, "one", 10, 1)
	record(t, l, "two", 20, 1)
	record(t, l, "three", 30, 1)
	observe(t, l, credit(60, 1))
	e := l.Snapshot("a", at(60))
	near(t, e.SpentUSD, 3)
	if e.EstimatedTotalUSD == nil {
		t.Fatalf("%+v", e)
	}
	l.mu.Lock()
	if len(l.accounts["a"].Seen) != 2 || len(l.accounts["a"].RecentIDs) != 2 {
		t.Fatal("unbounded dedup")
	}
	l.mu.Unlock()
}

func TestPersistenceRoundTripDedupAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "ledger.json")
	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	observe(t, l, credit(0, 0))
	record(t, l, "one", 30, 1)
	observe(t, l, credit(60, 1))
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	l2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	e := l2.Snapshot("a", at(60))
	if e.EstimatedTotalUSD == nil || e.SpentUSD != 1 || !e.SpentComplete {
		t.Fatalf("%+v", e)
	}
	record(t, l2, "one", 120, 99)
	near(t, l2.Snapshot("a", at(120)).SpentUSD, 1)
	if err = l.RecordDelivery("a", "closed", "claude-opus-5-5", Usage{}, at(120)); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestCorruptLedgerIsReportedAndNeverOverwritten(t *testing.T) {
	for _, raw := range []string{"not-json", `{"version":99,"accounts":{}}`, `{"version":1,"accounts":{}} {}`, `{"version":1,"accounts":{},"unknown":true}`} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			l, err := New(path)
			if err == nil || l == nil {
				t.Fatal("corrupt load silently accepted")
			}
			record(t, l, "new", 10, 1)
			e := l.Snapshot("a", at(10))
			if e.SpentComplete || e.EstimateStatus != "load_error" || e.LoadError == "" {
				t.Fatalf("%+v", e)
			}
			if l.Close() == nil {
				t.Fatal("lost load error")
			}
			saved, _ := os.ReadFile(path)
			if string(saved) != raw {
				t.Fatal("corrupt input overwritten")
			}
		})
	}
}

func TestRequestDoesNotWaitOnWriterAndDebouncePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := NewWithOptions(Options{Path: path, Debounce: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.writeMu.Lock()
	finished := make(chan struct{})
	go func() {
		_ = l.RecordDelivery("a", "one", "claude-opus-5-5", Usage{OutputTokens: 50_000}, at(10))
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		l.writeMu.Unlock()
		t.Fatal("request blocked on persistence")
	}
	l.writeMu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err = os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("debounced writer did not run")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConcurrentRecordingSnapshotsAndIndependentAccounts(t *testing.T) {
	l := NewMemory()
	defer l.Close()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for repeat := 0; repeat < 2; repeat++ {
				_ = l.RecordDelivery("a", fmt.Sprint(i), "claude-opus-5-5", Usage{OutputTokens: 500}, at(10))
				_ = l.Snapshot("a", at(11))
			}
		}(i)
	}
	wg.Wait()
	near(t, l.Snapshot("a", at(11)).SpentUSD, 1)
	near(t, l.Snapshot("b", at(11)).SpentUSD, 0)
}

func TestMarkIncompleteIdempotentPartialAndSanitizedReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	observe(t, l, credit(0, 0))
	record(t, l, "logical:part", 10, 1)
	for i := 0; i < 2; i++ {
		if err = l.MarkIncomplete("a", "logical", "private raw error and credential text", at(15)); err != nil {
			t.Fatal(err)
		}
	}
	e := l.Snapshot("a", at(20))
	if e.SpentComplete || e.UnpricedRequests != 0 || e.IncompleteRequests != 1 {
		t.Fatalf("%+v", e)
	}
	near(t, e.SpentUSD, 1)
	observe(t, l, credit(60, 1))
	if e = l.Snapshot("a", at(60)); e.EstimatedTotalUSD != nil || e.SampleCount != 0 {
		t.Fatalf("%+v", e)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "private raw") || !strings.Contains(string(raw), "incomplete_delivery") {
		t.Fatal("reason was not sanitized")
	}
	l2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if err = l2.MarkIncomplete("a", "logical", "cancelled", at(70)); err != nil {
		t.Fatal(err)
	}
	if l2.Snapshot("a", at(70)).IncompleteRequests != 1 {
		t.Fatal("incomplete ID lost across restart")
	}
}

func TestOversizedPersistenceNeverProducesUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := NewWithOptions(Options{Path: path, MaxStateBytes: 1200})
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Flush(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for i := 0; i < 20; i++ {
		record(t, l, fmt.Sprintf("%03d-%s", i, strings.Repeat("x", 100)), i+1, 1)
	}
	if err = l.Flush(); err == nil {
		t.Fatal("unloadable output not rejected")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("known previous persistence overwritten")
	}
	if e := l.Snapshot("a", at(30)); e.PersistenceError == "" {
		t.Fatalf("%+v", e)
	}
	if l.Close() == nil {
		t.Fatal("close hid persistence failure")
	}
}

func TestExtremeCreditFractionDoesNotPoisonState(t *testing.T) {
	l := NewMemory()
	defer l.Close()
	s := credit(0, 0)
	s.Limit = math.SmallestNonzeroFloat64
	observe(t, l, s)
	record(t, l, "one", 30, 1)
	s = credit(60, 1)
	s.Limit = math.SmallestNonzeroFloat64
	if err := l.ObserveCredits("a", s); err == nil {
		t.Fatal("infinite fraction accepted")
	}
	e := l.Snapshot("a", at(60))
	if e.EstimatedTotalUSD != nil || e.EstimateStatus != "invalid_credit_interval" {
		t.Fatalf("%+v", e)
	}
	if _, err := json.Marshal(e); err != nil {
		t.Fatal(err)
	}
}

func TestPriceAndThresholdConfigurationValidated(t *testing.T) {
	if l, err := NewWithOptions(Options{MinFraction: math.NaN()}); err == nil || l != nil {
		t.Fatal("NaN option accepted")
	}
	if l, err := NewWithOptions(Options{Catalog: Catalog{"x": Price{Input: math.Inf(1)}}}); err == nil || l != nil {
		t.Fatal("infinite price accepted")
	}
}

func TestFullPromptTierPreservedAcrossAccountParts(t *testing.T) {
	l := NewMemory()
	defer l.Close()
	part := Usage{InputTokens: 60_000, OutputTokens: 1000}
	for _, account := range []string{"a", "b"} {
		if err := l.RecordDeliveryWithPromptTokens(account, "same-logical-request", "claude-haiku-5-5", part, 120_000, at(10)); err != nil {
			t.Fatal(err)
		}
		near(t, l.Snapshot(account, at(10)).SpentUSD, .0325)
	}
	whole, err := DefaultCatalog().Quote("claude-haiku-5-5", Usage{InputTokens: 120_000, OutputTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	near(t, l.Snapshot("a", at(10)).SpentUSD+l.Snapshot("b", at(10)).SpentUSD, whole)
	if _, err = DefaultCatalog().QuoteWithPromptTokens("claude-haiku-5-5", part, 50_000); !errors.Is(err, ErrInvalidUsage) {
		t.Fatal("invalid full prompt accepted", err)
	}
}

func TestFailedWriterRecoversWithoutAnyNewDirtyEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := NewWithOptions(Options{Path: path, Debounce: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	} // inject a non-regular destination AFTER successful startup
	record(t, l, "one", 10, 1)
	deadline := time.Now().Add(2 * time.Second)
	for l.Snapshot("a", at(11)).PersistenceError == "" {
		if time.Now().After(deadline) {
			t.Fatal("writer failure not observed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	} // only an empty owned fixture directory
	// No RecordDelivery, ObserveCredits or manual Flush after repairing the path.
	deadline = time.Now().Add(2 * time.Second)
	for {
		if info, e := os.Stat(path); e == nil && info.Mode().IsRegular() && l.Snapshot("a", at(11)).PersistenceError == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle failed writer never retried")
		}
		time.Sleep(5 * time.Millisecond)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"spentUSD":1`) {
		t.Fatal("recovered ledger missing cost", err)
	}
}
