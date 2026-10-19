// Package costledger tracks delivered-token USD equivalents and calibrates
// account allowances against non-overlapping upstream credit snapshots.
package costledger

import (
	"errors"
	"fmt"
	"math"
)

const PriceAsOf = "2026-10-19"
const PriceSource = "https://platform.claude.com/docs/en/about-claude/pricing"

var ErrUnknownPrice = errors.New("unknown model price")
var ErrInvalidUsage = errors.New("invalid final delivered usage")

// Usage categories must be mutually exclusive FINAL delivered token counts.
// Do not pass raw counts here and apply inflation a second time.
type Usage struct {
	InputTokens        int64 `json:"inputTokens"`
	OutputTokens       int64 `json:"outputTokens"`
	CacheReadTokens    int64 `json:"cacheReadTokens"`
	CacheWrite5mTokens int64 `json:"cacheWrite5mTokens"`
	CacheWrite1hTokens int64 `json:"cacheWrite1hTokens"`
}

type Price struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cacheRead"`
	CacheWrite5m float64 `json:"cacheWrite5m"`
	CacheWrite1h float64 `json:"cacheWrite1h"`
	// Optional all-token rate tier for models explicitly priced by prompt length.
	LongContextThreshold int64  `json:"longContextThreshold,omitempty"`
	LongContextPrice     *Price `json:"longContextPrice,omitempty"`
}

// Catalog is explicit configuration, never a fuzzy family-prefix price guess.
type Catalog map[string]Price

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validatePrice(p Price, nested bool) error {
	for _, v := range []float64{p.Input, p.Output, p.CacheRead, p.CacheWrite5m, p.CacheWrite1h} {
		if !finite(v) || v < 0 {
			return errors.New("invalid USD/MTok price")
		}
	}
	if p.LongContextPrice != nil {
		if nested || p.LongContextThreshold <= 0 {
			return errors.New("invalid price tier")
		}
		return validatePrice(*p.LongContextPrice, true)
	}
	if p.LongContextThreshold != 0 {
		return errors.New("missing long-context tier")
	}
	return nil
}

func (c Catalog) Quote(model string, usage Usage) (float64, error) {
	prompt, err := usage.promptTokens()
	if err != nil {
		return 0, err
	}
	return c.QuoteWithPromptTokens(model, usage, prompt)
}

func (u Usage) promptTokens() (int64, error) {
	var total int64
	for _, n := range []int64{u.InputTokens, u.CacheReadTokens, u.CacheWrite5mTokens, u.CacheWrite1hTokens} {
		if n < 0 || n > math.MaxInt64-total {
			return 0, ErrInvalidUsage
		}
		total += n
	}
	return total, nil
}

// QuoteWithPromptTokens selects a request-level tier before pricing an account
// part. Splitting one prompt across accounts must never move it into a low tier.
func (c Catalog) QuoteWithPromptTokens(model string, usage Usage, requestPromptTokens int64) (float64, error) {
	counts := []int64{usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWrite5mTokens, usage.CacheWrite1hTokens}
	for _, n := range counts {
		if n < 0 {
			return 0, ErrInvalidUsage
		}
	}
	partPrompt, err := usage.promptTokens()
	if err != nil || requestPromptTokens < partPrompt {
		return 0, ErrInvalidUsage
	}
	p, ok := c[model]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrUnknownPrice, model)
	}
	if err := validatePrice(p, false); err != nil {
		return 0, err
	}
	if p.LongContextPrice != nil && requestPromptTokens > p.LongContextThreshold {
		p = *p.LongContextPrice
	}
	rates := []float64{p.Input, p.Output, p.CacheRead, p.CacheWrite5m, p.CacheWrite1h}
	var usd float64
	for i, n := range counts {
		usd += float64(n) / 1_000_000 * rates[i]
	}
	if !finite(usd) {
		return 0, ErrInvalidUsage
	}
	return usd, nil
}

// DefaultCatalog is the verified standard/global first-party list price, not
// Kiro billing, fast mode, regional, batch or negotiated pricing.
func DefaultCatalog() Catalog {
	c := Catalog{}
	add := func(p Price, names ...string) {
		for _, name := range names {
			c[name] = p
		}
	}
	add(Price{Input: 4, Output: 20, CacheRead: .2, CacheWrite5m: 5, CacheWrite1h: 8}, "claude-opus-5-5", "claude-opus-5.5")
	add(Price{Input: 2, Output: 10, CacheRead: .1, CacheWrite5m: 2.5, CacheWrite1h: 4}, "claude-sonnet-5-5", "claude-sonnet-5.5")
	opus := Price{Input: 5, Output: 25, CacheRead: .5, CacheWrite5m: 6.25, CacheWrite1h: 10}
	add(opus, "claude-opus-5", "claude-opus-4-8", "claude-opus-4.8", "claude-opus-4-7", "claude-opus-4.7", "claude-opus-4-6", "claude-opus-4.6", "claude-opus-4-5", "claude-opus-4.5", "claude-opus-4-5-20251101")
	add(Price{Input: 15, Output: 75, CacheRead: 1.5, CacheWrite5m: 18.75, CacheWrite1h: 30}, "claude-opus-4", "claude-opus-4-1", "claude-opus-4.1")
	add(Price{Input: 2, Output: 10, CacheRead: .2, CacheWrite5m: 2.5, CacheWrite1h: 4}, "claude-sonnet-5")
	sonnet := Price{Input: 3, Output: 15, CacheRead: .3, CacheWrite5m: 3.75, CacheWrite1h: 6}
	add(sonnet, "claude-sonnet-4", "claude-sonnet-4-6", "claude-sonnet-4.6", "claude-sonnet-4-5", "claude-sonnet-4.5", "claude-sonnet-4-5-20250929")
	add(Price{Input: 1, Output: 5, CacheRead: .1, CacheWrite5m: 1.25, CacheWrite1h: 2}, "claude-haiku-4-5", "claude-haiku-4.5", "claude-haiku-4-5-20251001")
	add(Price{Input: .8, Output: 4, CacheRead: .08, CacheWrite5m: 1, CacheWrite1h: 1.6}, "claude-haiku-3-5", "claude-haiku-3.5")
	add(Price{Input: 10, Output: 50, CacheRead: .25, CacheWrite5m: 12.5, CacheWrite1h: 20}, "claude-fable-5-1", "claude-fable-5.1", "claude-mythos-5-1", "claude-mythos-5.1")
	add(Price{Input: 10, Output: 50, CacheRead: 1, CacheWrite5m: 12.5, CacheWrite1h: 20}, "claude-fable-5", "claude-mythos-5")
	haikuHigh := Price{Input: .5, Output: 2.5, CacheRead: .05, CacheWrite5m: .625, CacheWrite1h: 1}
	add(Price{Input: .1, Output: .5, CacheRead: .01, CacheWrite5m: .125, CacheWrite1h: .2, LongContextThreshold: 100_000, LongContextPrice: &haikuHigh}, "claude-haiku-5-5", "claude-haiku-5.5")
	return c
}
