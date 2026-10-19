package costledger

import (
	"errors"
	"math"
	"sync"
	"time"
)

var ErrClosed = errors.New("cost ledger closed")
var ErrOutOfOrder = errors.New("out-of-order cost or credit observation")

type CreditSnapshot struct {
	At        time.Time `json:"at"`
	PeriodKey string    `json:"periodKey"`
	Used      float64   `json:"used"`
	Limit     float64   `json:"limit"`
	// Values are absolute per-bucket consumption. Include bucket limit/expiry
	// fingerprints in PeriodKey, since this map intentionally stores only usage.
	Components map[string]float64 `json:"components,omitempty"`
}

type Estimate struct {
	Currency           string     `json:"currency"`
	SpentUSD           float64    `json:"spentUSD"`
	PeriodSpentUSD     float64    `json:"periodSpentUSD"`
	SpentComplete      bool       `json:"spentComplete"`
	EstimatedTotalUSD  *float64   `json:"estimatedTotalUSD"`
	EstimateStatus     string     `json:"estimateStatus"`
	SampleCount        int        `json:"sampleCount"`
	LastSampleAt       *time.Time `json:"lastSampleAt"`
	TrackingSince      time.Time  `json:"trackingSince"`
	UnpricedRequests   uint64     `json:"unpricedRequests"`
	IncompleteRequests uint64     `json:"incompleteRequests,omitempty"`
	CreditUsed         float64    `json:"creditUsed"`
	CreditLimit        float64    `json:"creditLimit"`
	PriceAsOf          string     `json:"priceAsOf"`
	LoadError          string     `json:"loadError,omitempty"`
	PersistenceError   string     `json:"persistenceError,omitempty"`
}

// Options is optional advanced/test configuration. New(path) uses defaults.
type Options struct {
	Path                                        string
	Catalog                                     Catalog
	HalfLife, MinInterval, Debounce, StaleAfter time.Duration
	MinCreditDelta, MinFraction                 float64
	MaxRequestIDs                               int
	MaxStateBytes                               int
	Now                                         func() time.Time
}

type accountState struct {
	SpentUSD             float64         `json:"spentUSD"`
	PeriodSpentUSD       float64         `json:"periodSpentUSD"`
	Unpriced             uint64          `json:"unpriced"`
	Incomplete           uint64          `json:"incomplete"`
	LastIncompleteReason string          `json:"lastIncompleteReason,omitempty"`
	Tainted              uint64          `json:"tainted"`
	TrackingSince        time.Time       `json:"trackingSince"`
	LastDeliveryAt       time.Time       `json:"lastDeliveryAt"`
	RecentIDs            []string        `json:"recentIDs"`
	Seen                 map[string]bool `json:"-"`
	Latest               *CreditSnapshot `json:"latest,omitempty"`
	Anchor               *CreditSnapshot `json:"anchor,omitempty"`
	AnchorUSD            float64         `json:"anchorUSD"`
	AnchorUnpriced       uint64          `json:"anchorUnpriced"`
	AnchorIncomplete     uint64          `json:"anchorIncomplete"`
	AnchorTainted        uint64          `json:"anchorTainted"`
	WeightedUSD          float64         `json:"weightedUSD"`
	WeightedFraction     float64         `json:"weightedFraction"`
	Samples              int             `json:"samples"`
	LastSample           time.Time       `json:"lastSample"`
	Status               string          `json:"status"`
}

type Ledger struct {
	mu               sync.Mutex
	writeMu          sync.Mutex
	accounts         map[string]*accountState
	opts             Options
	loadErr, saveErr error
	closed           bool
	changed          chan struct{}
	stop, done       chan struct{}
	closeOnce        sync.Once
}

func New(path string) (*Ledger, error) { return NewWithOptions(Options{Path: path}) }
func NewMemory() *Ledger               { l, _ := New(""); return l }

func NewWithOptions(opts Options) (*Ledger, error) {
	if opts.HalfLife <= 0 {
		opts.HalfLife = time.Hour
	}
	if opts.MinInterval <= 0 {
		opts.MinInterval = time.Minute
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 2 * time.Second
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 6 * opts.HalfLife
	}
	if opts.MinCreditDelta <= 0 {
		opts.MinCreditDelta = .1
	}
	if opts.MinFraction <= 0 {
		opts.MinFraction = .0001
	}
	if opts.MaxRequestIDs <= 0 {
		opts.MaxRequestIDs = 1024
	}
	if opts.MaxStateBytes <= 0 {
		opts.MaxStateBytes = 32 << 20
	}
	if !finite(opts.MinCreditDelta) || !finite(opts.MinFraction) {
		return nil, errors.New("invalid observation threshold")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Catalog == nil {
		opts.Catalog = DefaultCatalog()
	}
	copyCatalog := Catalog{}
	for name, p := range opts.Catalog {
		if err := validatePrice(p, false); err != nil {
			return nil, err
		}
		if p.LongContextPrice != nil {
			p2 := *p.LongContextPrice
			p.LongContextPrice = &p2
		}
		copyCatalog[name] = p
	}
	opts.Catalog = copyCatalog
	l := &Ledger{accounts: map[string]*accountState{}, opts: opts, changed: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	if opts.Path != "" {
		l.loadErr = l.load()
	}
	if opts.Path != "" && l.loadErr == nil {
		go l.writer()
	} else {
		close(l.done)
	}
	return l, l.loadErr
}

func (l *Ledger) account(id string, at time.Time) *accountState {
	a := l.accounts[id]
	if a == nil {
		a = &accountState{TrackingSince: at, Seen: map[string]bool{}, Status: "awaiting_credit_snapshot"}
		l.accounts[id] = a
	}
	return a
}

func (l *Ledger) dirty() {
	select {
	case l.changed <- struct{}{}:
	default:
	}
}

func (l *Ledger) consumeID(a *accountState, id string) bool {
	if a.Seen[id] {
		return false
	}
	a.Seen[id] = true
	a.RecentIDs = append(a.RecentIDs, id)
	if len(a.RecentIDs) > l.opts.MaxRequestIDs {
		delete(a.Seen, a.RecentIDs[0])
		a.RecentIDs = a.RecentIDs[1:]
	}
	return true
}

// MarkIncomplete records missing/partial attribution without inventing a model
// or storing the supplied raw reason. Known partial usage may first be recorded
// under a DISTINCT request ID suffix; this logical ID then marks incompleteness.
func (l *Ledger) MarkIncomplete(accountID, requestID, reason string, at time.Time) error {
	if accountID == "" || requestID == "" || at.IsZero() {
		return errors.New("account, request ID and delivery time required")
	}
	switch reason {
	case "partial_delivery", "write_failed", "cancelled", "missing_usage", "unknown_attribution", "missing_cache_ttl":
	default:
		reason = "incomplete_delivery"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	a := l.account(accountID, at)
	if !l.consumeID(a, requestID) {
		return nil
	}
	a.Incomplete++
	a.LastIncompleteReason = reason
	if at.After(a.LastDeliveryAt) {
		a.LastDeliveryAt = at
	}
	l.dirty()
	return nil
}

// RecordDelivery does no I/O and is idempotent within the bounded recent-ID set.
// Unknown prices still consume the ID and increment unpricedRequests.
func (l *Ledger) RecordDelivery(accountID, requestID, model string, usage Usage, at time.Time) error {
	prompt, err := usage.promptTokens()
	if err != nil {
		prompt = -1
	}
	return l.RecordDeliveryWithPromptTokens(accountID, requestID, model, usage, prompt, at)
}

// RecordDeliveryWithPromptTokens preserves the complete request's pricing tier
// when its FINAL delivered categories have been attributed to several accounts.
func (l *Ledger) RecordDeliveryWithPromptTokens(accountID, requestID, model string, usage Usage, requestPromptTokens int64, at time.Time) error {
	if accountID == "" || requestID == "" || at.IsZero() {
		return errors.New("account, request ID and delivery time required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	a := l.account(accountID, at)
	if !l.consumeID(a, requestID) {
		return nil
	}
	if at.After(a.LastDeliveryAt) {
		a.LastDeliveryAt = at
	}
	usd, err := l.opts.Catalog.QuoteWithPromptTokens(model, usage, requestPromptTokens)
	if err != nil {
		a.Unpriced++
		l.dirty()
		return err
	}
	if !finite(a.SpentUSD+usd) || !finite(a.PeriodSpentUSD+usd) {
		a.Unpriced++
		l.dirty()
		return ErrInvalidUsage
	}
	a.SpentUSD += usd
	a.PeriodSpentUSD += usd
	if a.Latest != nil && at.Before(a.Latest.At) {
		a.Tainted++
		l.dirty()
		return ErrOutOfOrder
	}
	l.dirty()
	return nil
}

func copyCredit(s CreditSnapshot) *CreditSnapshot {
	c := s
	c.Components = map[string]float64{}
	for k, v := range s.Components {
		c.Components[k] = v
	}
	return &c
}

func validCredit(s CreditSnapshot) bool {
	if s.At.IsZero() || s.PeriodKey == "" || !finite(s.Used) || !finite(s.Limit) || s.Used < 0 || s.Limit <= 0 {
		return false
	}
	for k, v := range s.Components {
		if k == "" || !finite(v) || v < 0 {
			return false
		}
	}
	return true
}

func epochChanged(old, s *CreditSnapshot) bool {
	if old.PeriodKey != s.PeriodKey || old.Limit != s.Limit || s.Used < old.Used || len(old.Components) != len(s.Components) {
		return true
	}
	for k, v := range old.Components {
		next, ok := s.Components[k]
		if !ok || next < v {
			return true
		}
	}
	return false
}

func anchor(a *accountState, s CreditSnapshot) {
	a.Anchor = copyCredit(s)
	a.AnchorUSD = a.SpentUSD
	a.AnchorUnpriced = a.Unpriced
	a.AnchorIncomplete = a.Incomplete
	a.AnchorTainted = a.Tainted
}

func clearEstimate(a *accountState, status string) {
	a.WeightedUSD = 0
	a.WeightedFraction = 0
	a.Samples = 0
	a.LastSample = time.Time{}
	a.Status = status
}

func (l *Ledger) ObserveCredits(accountID string, s CreditSnapshot) error {
	if accountID == "" || !validCredit(s) {
		return errors.New("invalid credit snapshot")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	a := l.account(accountID, s.At)
	if (a.Latest != nil && !s.At.After(a.Latest.At)) || s.At.Before(a.LastDeliveryAt) {
		return ErrOutOfOrder
	}
	if a.Latest == nil {
		a.Latest = copyCredit(s)
		anchor(a, s)
		a.Status = "awaiting_credit_delta"
		l.dirty()
		return nil
	}
	changed := epochChanged(a.Latest, &s)
	a.Latest = copyCredit(s)
	if changed {
		a.PeriodSpentUSD = 0
		anchor(a, s)
		clearEstimate(a, "credit_epoch_changed")
		l.dirty()
		return nil
	}
	if a.Unpriced != a.AnchorUnpriced || a.Incomplete != a.AnchorIncomplete || a.Tainted != a.AnchorTainted {
		anchor(a, s)
		clearEstimate(a, "incomplete_cost_interval")
		l.dirty()
		return nil
	}
	delta := s.Used - a.Anchor.Used
	if delta < math.Max(l.opts.MinCreditDelta, s.Limit*l.opts.MinFraction) || s.At.Sub(a.Anchor.At) < l.opts.MinInterval {
		l.dirty()
		return nil // Do NOT move the anchor; delayed/micro credits accumulate.
	}
	usd := a.SpentUSD - a.AnchorUSD
	if usd <= 0 {
		anchor(a, s)
		clearEstimate(a, "unattributed_credit_usage")
		l.dirty()
		return nil
	}
	fraction := delta / s.Limit
	if !finite(fraction) || fraction <= 0 {
		anchor(a, s)
		clearEstimate(a, "invalid_credit_interval")
		l.dirty()
		return errors.New("invalid credit fraction")
	}
	decay := 1.0
	if !a.LastSample.IsZero() {
		decay = math.Exp2(-s.At.Sub(a.LastSample).Seconds() / l.opts.HalfLife.Seconds())
	}
	// Uniform-in-interval approximation to exponential time weighting. Both
	// monetary and credit-fraction masses use the SAME factor, preserving ratios.
	x := math.Ln2 * s.At.Sub(a.Anchor.At).Seconds() / l.opts.HalfLife.Seconds()
	w := -math.Expm1(-x) / x
	weightedUSD := a.WeightedUSD*decay + usd*w
	weightedFraction := a.WeightedFraction*decay + fraction*w
	if !finite(weightedUSD) || !finite(weightedFraction) {
		anchor(a, s)
		clearEstimate(a, "invalid_credit_interval")
		l.dirty()
		return errors.New("invalid weighted credit interval")
	}
	a.WeightedUSD = weightedUSD
	a.WeightedFraction = weightedFraction
	a.Samples++
	a.LastSample = s.At
	a.Status = "available"
	anchor(a, s)
	l.dirty()
	return nil
}

func (l *Ledger) Snapshot(accountID string, now time.Time) Estimate {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Estimate{Currency: "USD", SpentComplete: l.loadErr == nil, EstimateStatus: "awaiting_usage", PriceAsOf: PriceAsOf}
	if l.loadErr != nil {
		e.LoadError = "persisted ledger could not be loaded"
		e.EstimateStatus = "load_error"
	}
	if l.saveErr != nil {
		e.PersistenceError = "ledger persistence failed"
	}
	a := l.accounts[accountID]
	if a == nil {
		return e
	}
	e.SpentUSD = a.SpentUSD
	e.PeriodSpentUSD = a.PeriodSpentUSD
	e.UnpricedRequests = a.Unpriced
	e.IncompleteRequests = a.Incomplete
	e.SpentComplete = e.SpentComplete && a.Unpriced == 0 && a.Incomplete == 0 && a.Tainted == 0
	e.TrackingSince = a.TrackingSince
	e.SampleCount = a.Samples
	e.EstimateStatus = a.Status
	if a.Latest != nil {
		e.CreditUsed = a.Latest.Used
		e.CreditLimit = a.Latest.Limit
	}
	if !a.LastSample.IsZero() {
		t := a.LastSample
		e.LastSampleAt = &t
	}
	if l.loadErr != nil {
		e.EstimateStatus = "load_error"
		return e
	}
	if a.Anchor != nil && (a.AnchorUnpriced != a.Unpriced || a.AnchorIncomplete != a.Incomplete || a.AnchorTainted != a.Tainted) {
		e.EstimateStatus = "incomplete_cost_interval"
		return e
	}
	if a.Samples > 0 && a.WeightedFraction > 0 && a.WeightedUSD > 0 && a.Status == "available" {
		if now.Sub(a.LastSample) > l.opts.StaleAfter {
			e.EstimateStatus = "stale"
			return e
		}
		value := a.WeightedUSD / a.WeightedFraction
		if finite(value) && value > 0 {
			e.EstimatedTotalUSD = &value
		}
	}
	return e
}

func (l *Ledger) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		if l.opts.Path != "" && l.loadErr == nil {
			close(l.stop)
		}
		<-l.done
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loadErr != nil {
		return l.loadErr
	}
	return l.saveErr
}
