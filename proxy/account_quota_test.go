package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/costledger"
)

func quotaRaw(t *testing.T, s string) *UsageLimitsResponse {
	t.Helper()
	var r UsageLimitsResponse
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestAccountQuotaRawCredits(t *testing.T) {
	now := time.Unix(1800000000, 0)
	raw := quotaRaw(t, `{"nextDateReset":1801000000,"usageBreakdownList":[{"resourceType":"OTHER","usageLimit":9999},{"resourceType":"AGENTIC_REQUEST","currentUsage":10,"currentUsageWithPrecision":10.125,"usageLimit":100,"usageLimitWithPrecision":100.5,"freeTrialInfo":{"freeTrialStatus":"ACTIVE","currentUsage":2,"usageLimit":50,"freeTrialExpiry":1800500000},"bonuses":[{"bonusCode":"b","status":"ACTIVE","currentUsage":1,"usageLimit":25,"expiresAt":1800600000000},{"bonusCode":"old","status":"ACTIVE","currentUsage":9,"usageLimit":200,"expiresAt":1799999999},{"bonusCode":"off","status":"EXPIRED","usageLimit":1000}]}]}`)
	s, err := accountCreditSnapshot(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Used != 13.125 || s.Limit != 175.5 || len(s.Components) != 3 || s.Components["base"] != 10.125 {
		t.Fatalf("wrong credit buckets: %+v", s)
	}
	// Order and consumption do not alter the credit epoch.
	b := &raw.UsageBreakdownList[1]
	b.CurrentUsageWithPrecision = nil
	b.CurrentUsage = 12
	same, _ := accountCreditSnapshot(raw, now.Add(time.Minute))
	if same.PeriodKey != s.PeriodKey {
		t.Fatal("consumption changed epoch")
	}
	// A per-bucket limit change must reset even when the total stays identical.
	b.UsageLimitWithPrecision = nil
	b.UsageLimit = 110.5
	b.FreeTrialInfo.UsageLimit = 40
	next, _ := accountCreditSnapshot(raw, now)
	if next.Limit != s.Limit || next.PeriodKey == s.PeriodKey {
		t.Fatal("hidden bucket changes not detected")
	}
	b.FreeTrialInfo.FreeTrialStatus = "EXPIRED"
	expired, _ := accountCreditSnapshot(raw, now)
	if _, ok := expired.Components["trial"]; ok || expired.PeriodKey == next.PeriodKey {
		t.Fatal("trial expiration not detected")
	}
}

func TestAccountQuotaInvalidCredits(t *testing.T) {
	for _, doc := range []string{
		`{}`, `{"usageBreakdownList":[{"resourceType":"OTHER","usageLimit":100}]}`,
		`{"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","usageLimit":100},{"resourceType":"AGENTIC_REQUEST","usageLimit":100}]}`,
		`{"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","currentUsage":-1,"usageLimit":100}]}`,
		`{"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","usageLimit":100,"bonuses":[{"status":"ACTIVE","usageLimit":10}]}]}`,
		`{"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","usageLimit":100,"bonuses":[{"bonusCode":"same","status":"ACTIVE","usageLimit":10},{"bonusCode":"same","status":"ACTIVE","usageLimit":10}]}]}`,
	} {
		if _, err := accountCreditSnapshot(quotaRaw(t, doc), time.Now()); err == nil {
			t.Fatalf("accepted invalid credit response: %s", doc)
		}
	}
	raw := &UsageLimitsResponse{UsageBreakdownList: []UsageBreakdown{{ResourceType: "AGENTIC_REQUEST", UsageLimit: 100, CurrentUsage: math.Inf(1)}}}
	if _, err := accountCreditSnapshot(raw, time.Now()); err == nil {
		t.Fatal("accepted infinity")
	}
	raw.UsageBreakdownList[0].CurrentUsage = 0
	raw.NextDateReset = json.Number("oops")
	if _, err := accountCreditSnapshot(raw, time.Now()); err == nil {
		t.Fatal("accepted invalid reset")
	}
}

func TestAccountQuotaModernCreditResource(t *testing.T) {
	raw := quotaRaw(t, `{"nextDateReset":1801000000,"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","usageLimit":99999},{"resourceType":"CREDIT","currentUsage":282,"currentUsageWithPrecision":282.49,"usageLimit":5000}]}`)
	s, err := accountCreditSnapshot(raw, time.Now())
	if err != nil || s.Used != 282.49 || s.Limit != 5000 {
		t.Fatalf("modern CREDIT response rejected or double counted: %+v %v", s, err)
	}
	raw.UsageBreakdownList = append(raw.UsageBreakdownList, raw.UsageBreakdownList[1])
	if _, err := accountCreditSnapshot(raw, time.Now()); err == nil {
		t.Fatal("duplicate CREDIT accepted")
	}
	if !quotaAccountEligible(config.Account{Enabled: true, AuthMethod: "api_key", KiroApiKey: "fixture"}) {
		t.Fatal("api_key-only credential excluded")
	}
	if quotaAccountEligible(config.Account{Enabled: true}) {
		t.Fatal("empty bearer eligible")
	}
}

func TestAccountQuotaWeightedExampleIntegration(t *testing.T) {
	l := costledger.NewMemory()
	defer l.Close()
	start := time.Unix(1800000000, 0)
	raw := quotaRaw(t, `{"nextDateReset":1801000000,"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","currentUsage":10,"usageLimit":100} ]}`)
	s, _ := accountCreditSnapshot(raw, start)
	if err := l.ObserveCredits("a", s); err != nil {
		t.Fatal(err)
	}
	// Opus5.5: final public 100k input + 10k output + 50k read +
	// 20k 5m write + 10k 1h write => $.40+.20+.01+.10+.08 = $.79.
	// These are post-inflation counts; no multiplier applies a second time.
	if err := l.RecordDelivery("a", "r", "claude-opus-5-5", costledger.Usage{InputTokens: 100000, OutputTokens: 10000, CacheReadTokens: 50000, CacheWrite5mTokens: 20000, CacheWrite1hTokens: 10000}, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	raw.UsageBreakdownList[0].CurrentUsage = 12
	s, _ = accountCreditSnapshot(raw, start.Add(5*time.Minute))
	if err := l.ObserveCredits("a", s); err != nil {
		t.Fatal(err)
	}
	e := l.Snapshot("a", s.At)
	if math.Abs(e.SpentUSD-.79) > 1e-9 || e.EstimatedTotalUSD == nil || math.Abs(*e.EstimatedTotalUSD-39.5) > 1e-9 {
		t.Fatalf("wrong USD/fraction: %+v", e)
	}
}

func TestAccountQuotaPollBoundedAndNonDestructive(t *testing.T) {
	now := time.Unix(1800000000, 0)
	l := costledger.NewMemory()
	defer l.Close()
	accounts := []config.Account{}
	for i := 0; i < 9; i++ {
		accounts = append(accounts, config.Account{ID: fmt.Sprint(i), Enabled: true, AuthMethod: "api_key", AccessToken: "fixture"})
	}
	accounts = append(accounts, config.Account{ID: "banned", Enabled: true, AccessToken: "fixture", BanStatus: "BANNED"}, config.Account{ID: "disabled", AccessToken: "fixture"})
	var active, peak, calls atomic.Int32
	q := &accountQuotaController{ledger: l, wake: make(chan struct{}, 1), recent: map[string]time.Time{}, polled: map[string]time.Time{}, now: func() time.Time { return now }, accounts: func() []config.Account { return accounts }}
	q.fetch = func(ctx context.Context, a *config.Account) (*UsageLimitsResponse, error) {
		n := active.Add(1)
		defer active.Add(-1)
		calls.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		a.Enabled = false // Only the goroutine's copy may change.
		return &UsageLimitsResponse{UsageBreakdownList: []UsageBreakdown{{ResourceType: "AGENTIC_REQUEST", UsageLimit: 100}}}, nil
	}
	q.poll(context.Background(), true)
	if calls.Load() != 9 || peak.Load() > 4 || peak.Load() < 2 {
		t.Fatalf("calls=%d peak=%d", calls.Load(), peak.Load())
	}
	for i := 0; i < 9; i++ {
		if !accounts[i].Enabled {
			t.Fatal("sampler changed source account")
		}
	}
	q.poll(context.Background(), true)
	if calls.Load() != 9 {
		t.Fatal("same-minute poll wasn't deduped")
	}
	now = now.Add(time.Minute)
	q.poll(context.Background(), false)
	if calls.Load() != 9 {
		t.Fatal("idle account repeatedly polled")
	}
	q.noteActivity("0", now)
	q.poll(context.Background(), false)
	if calls.Load() != 10 {
		t.Fatal("active account not sampled")
	}
}

func TestAccountQuotaPollCancellationAndFailure(t *testing.T) {
	l := costledger.NewMemory()
	defer l.Close()
	now := time.Now()
	var mu sync.Mutex
	called := false
	q := &accountQuotaController{ledger: l, wake: make(chan struct{}, 1), recent: map[string]time.Time{}, polled: map[string]time.Time{}, now: func() time.Time { return now }, accounts: func() []config.Account { return []config.Account{{ID: "a", Enabled: true, AccessToken: "x"}} }, fetch: func(ctx context.Context, a *config.Account) (*UsageLimitsResponse, error) {
		mu.Lock()
		called = true
		mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.poll(ctx, true); close(done) }()
	for {
		mu.Lock()
		ok := called
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll ignores cancellation")
	}
	if e := l.Snapshot("a", now); e.CreditLimit != 0 || e.EstimatedTotalUSD != nil {
		t.Fatal("failed poll created fake snapshot")
	}
	now = now.Add(time.Minute)
	q.fetch = func(context.Context, *config.Account) (*UsageLimitsResponse, error) {
		return nil, errors.New("network failure")
	}
	q.poll(context.Background(), true)
	if e := l.Snapshot("a", now); e.CreditLimit != 0 {
		t.Fatal("network failure changed credits")
	}
}

func TestAccountQuotaUsageLimitsContextCancellation(t *testing.T) {
	a, _, _ := refreshTransportFixture(t, true)
	var reached atomic.Bool
	installRefreshTransportClient(t, a, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reached.Store(true)
		if r.URL.Query().Get("resourceType") != "AGENTIC_REQUEST" {
			t.Error("sampling changed resource query")
		}
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(time.Second):
			return nil, errors.New("request lost caller context")
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := GetUsageLimitsContext(ctx, &a)
	if !reached.Load() || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("REST request did not preserve context: reached=%v err=%v", reached.Load(), err)
	}
}
