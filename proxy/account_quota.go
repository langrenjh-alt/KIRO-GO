package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/costledger"
	"kiro-go/logger"
)

const quotaPollInterval = time.Minute
const quotaActivityWindow = 15 * time.Minute

// The ledger is independent of config snapshots. Request goroutines only touch
// memory; its single debounced writer persists no keys, tokens or prompt data.
type accountQuotaController struct {
	ledger   *costledger.Ledger
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	wake     chan struct{}
	mu       sync.Mutex
	recent   map[string]time.Time
	polled   map[string]time.Time
	fetch    func(context.Context, *config.Account) (*UsageLimitsResponse, error)
	accounts func() []config.Account
	refresh  func(context.Context, *config.Account, bool) error
	now      func() time.Time
}

func (h *Handler) startAccountQuota() {
	l, err := costledger.New(filepath.Join(config.GetConfigDir(), "account_quota_ledger.json"))
	if err != nil {
		logger.Warnf("[AccountQuota] persisted ledger unavailable; showing incomplete totals")
	}
	if l == nil {
		l = costledger.NewMemory()
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &accountQuotaController{ledger: l, cancel: cancel, wake: make(chan struct{}, 1), recent: map[string]time.Time{}, polled: map[string]time.Time{}, fetch: GetUsageLimitsContext, accounts: config.GetAccounts, refresh: h.refreshAccountTokenContext, now: time.Now}
	h.accountQuota = q
	q.wg.Add(1)
	go func() { defer q.wg.Done(); q.run(ctx) }()
}

func (h *Handler) closeAccountQuota() {
	if q := h.accountQuota; q != nil {
		q.cancel()
		q.wg.Wait()
		if err := q.ledger.Close(); err != nil {
			logger.Warnf("[AccountQuota] ledger flush failed; totals marked incomplete")
		}
	}
}

func (h *Handler) accountQuotaSnapshot(id string) costledger.Estimate {
	if q := h.accountQuota; q != nil {
		return q.ledger.Snapshot(id, q.now())
	}
	return costledger.Estimate{Currency: "USD", SpentComplete: true, EstimateStatus: "awaiting_usage", PriceAsOf: costledger.PriceAsOf}
}

func (h *Handler) recordAccountDollarUsage(id, requestID, model string, u costledger.Usage) {
	q := h.accountQuota
	if q == nil || id == "" {
		return
	}
	now := q.now()
	_ = q.ledger.RecordDelivery(id, requestID, model, u, now)
	q.noteActivity(id, now)
}

func (h *Handler) recordAccountDollarUsageWithPromptTokens(id, requestID, model string, u costledger.Usage, requestPromptTokens int64) {
	q := h.accountQuota
	if q == nil || id == "" {
		return
	}
	now := q.now()
	_ = q.ledger.RecordDeliveryWithPromptTokens(id, requestID, model, u, requestPromptTokens, now)
	q.noteActivity(id, now)
}

func (h *Handler) recordAccountDollarUsageIncomplete(id, requestID, model, reason string) {
	q := h.accountQuota
	if q == nil || id == "" {
		return
	}
	now := q.now()
	_ = q.ledger.MarkIncomplete(id, requestID, reason, now)
	q.noteActivity(id, now)
}

func (q *accountQuotaController) noteActivity(id string, now time.Time) {
	q.mu.Lock()
	q.recent[id] = now
	last := q.polled[id]
	q.mu.Unlock()
	if now.Sub(last) >= quotaPollInterval {
		select {
		case q.wake <- struct{}{}:
		default:
		}
	}
}

func quotaAccountEligible(a config.Account) bool {
	if !a.Enabled || (strings.TrimSpace(a.AccessToken) == "" && strings.TrimSpace(a.KiroApiKey) == "") {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(a.BanStatus)) {
	case "BANNED", "SUSPENDED", "DISABLED":
		return false
	}
	return true
}

func (q *accountQuotaController) run(ctx context.Context) {
	ticker := time.NewTicker(quotaPollInterval)
	defer ticker.Stop()
	// Establish a real upstream anchor, never the potentially stale config copy.
	startup := time.NewTimer(5 * time.Second)
	defer startup.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-startup.C:
			q.poll(ctx, true)
		case <-ticker.C:
			q.poll(ctx, false)
		case <-q.wake:
			q.poll(ctx, false)
		}
	}
}

func (q *accountQuotaController) poll(ctx context.Context, bootstrap bool) {
	now := q.now()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, a := range q.accounts() {
		if !quotaAccountEligible(a) {
			continue
		}
		q.mu.Lock()
		last, active := q.polled[a.ID], q.recent[a.ID]
		due := now.Sub(last) >= quotaPollInterval && (bootstrap || (!active.IsZero() && now.Sub(active) <= quotaActivityWindow))
		if due {
			q.polled[a.ID] = now
		}
		q.mu.Unlock()
		if !due {
			continue
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(a config.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			if q.refresh != nil {
				if err := q.refresh(c, &a, false); err != nil {
					return
				}
			}
			raw, err := q.fetch(c, &a)
			if err != nil || c.Err() != nil {
				return
			}
			s, err := accountCreditSnapshot(raw, q.now())
			if err != nil {
				return
			}
			// A removed/disabled account must not get re-enabled by the sampler. No
			// account config/status is changed here, including on network/auth errors.
			_ = q.ledger.ObserveCredits(a.ID, s)
		}(a)
	}
	wg.Wait()
}

func preciseCredit(value float64, p *float64) float64 {
	if p != nil {
		return *p
	}
	return value
}
func creditNumberValid(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func creditExpiry(raw json.Number) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := raw.Float64()
	if err != nil || !creditNumberValid(v) || v > 1e16 {
		return 0, errors.New("invalid credit timestamp")
	}
	if v > 1e12 {
		v /= 1000
	}
	return int64(v), nil
}

// accountCreditSnapshot selects credits, not the request counter/statistics.
// Each bucket retains its own consumption so rollover in one bucket cannot be
// hidden by increased usage in another. Limits/expiry/active-set form the epoch.
func accountCreditSnapshot(raw *UsageLimitsResponse, at time.Time) (costledger.CreditSnapshot, error) {
	s := costledger.CreditSnapshot{At: at, Components: map[string]float64{}}
	if raw == nil || at.IsZero() {
		return s, errors.New("missing credit snapshot")
	}
	var b, legacy *UsageBreakdown
	for i := range raw.UsageBreakdownList {
		candidate := &raw.UsageBreakdownList[i]
		switch strings.ToUpper(strings.TrimSpace(candidate.ResourceType)) {
		case "CREDIT":
			if b != nil {
				return s, errors.New("ambiguous credit breakdown")
			}
			b = candidate
		case "AGENTIC_REQUEST":
			if legacy != nil {
				return s, errors.New("ambiguous legacy credit breakdown")
			}
			legacy = candidate
		}
	}
	// The request's resourceType query and the returned accounting resource
	// differ: modern Kiro reports CREDIT. Never sum it with legacy counters.
	if b == nil {
		b = legacy
	}
	if b == nil {
		return s, errors.New("missing agentic credit breakdown")
	}
	reset, err := creditExpiry(raw.NextDateReset)
	if err != nil {
		return s, err
	}
	type bucket struct {
		ID     string
		Limit  float64
		Expiry int64
	}
	buckets := []bucket{}
	add := func(id string, used, limit float64, expiry int64) error {
		if !creditNumberValid(used) || !creditNumberValid(limit) {
			return errors.New("invalid credit bucket")
		}
		if limit == 0 {
			return nil
		}
		if _, ok := s.Components[id]; ok {
			return errors.New("duplicate credit bucket")
		}
		s.Components[id] = used
		s.Used += used
		s.Limit += limit
		buckets = append(buckets, bucket{id, limit, expiry})
		return nil
	}
	if err = add("base", preciseCredit(b.CurrentUsage, b.CurrentUsageWithPrecision), preciseCredit(b.UsageLimit, b.UsageLimitWithPrecision), reset); err != nil {
		return s, err
	}
	if t := b.FreeTrialInfo; t != nil && strings.EqualFold(strings.TrimSpace(t.FreeTrialStatus), "ACTIVE") {
		expiry, e := creditExpiry(t.FreeTrialExpiry)
		if e != nil {
			return s, e
		}
		if expiry == 0 || expiry > at.Unix() {
			if e = add("trial", preciseCredit(t.CurrentUsage, t.CurrentUsageWithPrecision), preciseCredit(t.UsageLimit, t.UsageLimitWithPrecision), expiry); e != nil {
				return s, e
			}
		}
	}
	for _, bonus := range b.Bonuses {
		if !strings.EqualFold(strings.TrimSpace(bonus.Status), "ACTIVE") {
			continue
		}
		expiry, e := creditExpiry(bonus.ExpiresAt)
		if e != nil {
			return s, e
		}
		if expiry != 0 && expiry <= at.Unix() {
			continue
		}
		id := strings.TrimSpace(bonus.BonusCode)
		if id == "" {
			return s, errors.New("credit bonus missing identity")
		}
		if e = add("bonus:"+id, preciseCredit(bonus.CurrentUsage, bonus.CurrentUsageWithPrecision), preciseCredit(bonus.UsageLimit, bonus.UsageLimitWithPrecision), expiry); e != nil {
			return s, e
		}
	}
	if s.Limit <= 0 || !creditNumberValid(s.Used) || !creditNumberValid(s.Limit) {
		return s, errors.New("no finite credit allowance")
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].ID < buckets[j].ID })
	fingerprint, _ := json.Marshal(struct {
		Reset   int64
		Buckets []bucket
	}{reset, buckets})
	sum := sha256.Sum256(fingerprint)
	s.PeriodKey = fmt.Sprintf("agentic:%s", hex.EncodeToString(sum[:]))
	return s, nil
}
