package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

func TestRetryBudgetAndRetryAfterParsing(t *testing.T) {
	budget := &retryBudget{limit: 2}
	now := time.Now()
	if !budget.allow(now) || !budget.allow(now) || budget.allow(now) {
		t.Fatal("retry budget did not enforce its per-second limit")
	}
	if !budget.allow(now.Add(time.Second)) {
		t.Fatal("retry budget did not reset after one second")
	}
	if got := parseRetryAfter("3", now); got != 3*time.Second {
		t.Fatalf("numeric Retry-After=%s", got)
	}
	if got := parseRetryAfter(now.Add(5*time.Second).UTC().Format(http.TimeFormat), now); got < 4*time.Second || got > 5*time.Second {
		t.Fatalf("date Retry-After=%s", got)
	}
	for i := 0; i < 100; i++ {
		got := jitterRetryDelay(time.Second)
		if got < time.Second || got > 1200*time.Millisecond {
			t.Fatalf("jittered delay out of bounds: %s", got)
		}
	}
}

func TestTransientTransportFailureDoesNotCooldownAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	account := config.Account{ID: "transport-account", Enabled: true, AccessToken: "token"}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	transientErrors := []error{
		&url.Error{Op: "Post", URL: "https://q.example", Err: errors.New("connection reset")},
		fmt.Errorf("read Kiro event stream frame: %w", io.ErrUnexpectedEOF),
		&KiroHTTPError{StatusCode: http.StatusServiceUnavailable, Endpoint: "test", Body: "high load"},
		&KiroHTTPError{StatusCode: http.StatusBadRequest, Endpoint: "test", Body: "invalid request"},
	}
	for _, transientErr := range transientErrors {
		for i := 0; i < 5; i++ {
			h.handleAccountFailure(&account, transientErr)
		}
	}
	if leased, _ := p.AcquireNextForModelExcluding("", nil); leased == nil || leased.ID != account.ID {
		t.Fatalf("transport failure made healthy account unavailable: %#v", leased)
	} else {
		p.Release(leased.ID)
	}
}

func TestRateLimitPolicyDecisionsAndSwitchRounds(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{ID: "rate-account", Email: "rate@example.invalid"}

	retry := config.RateLimitConfig{
		Mode:                   config.RateLimitModeRetrySame,
		RetrySameAccount:       2,
		RetryDelayMs:           10,
		RetryBackoffMultiplier: 2,
		RetryMaxDelayMs:        25,
	}
	if err := config.UpdateDispatchPolicies(&retry, nil, nil); err != nil {
		t.Fatalf("UpdateDispatchPolicies(retry): %v", err)
	}
	retryCounts := make(map[string]int)
	excluded := make(map[string]bool)
	first := h.applyRateLimitPolicy(account, retryCounts, excluded)
	second := h.applyRateLimitPolicy(account, retryCounts, excluded)
	third := h.applyRateLimitPolicy(account, retryCounts, excluded)
	if first.RetrySameID != account.ID || first.Delay != 10*time.Millisecond {
		t.Fatalf("first retry decision = %+v", first)
	}
	if second.RetrySameID != account.ID || second.Delay != 20*time.Millisecond {
		t.Fatalf("second retry decision = %+v", second)
	}
	if third.RetrySameID != "" || !excluded[account.ID] {
		t.Fatalf("exhausted retry decision = %+v excluded=%v", third, excluded)
	}
	retryAfterDecision := h.applyRateLimitPolicy(
		account,
		make(map[string]int),
		make(map[string]bool),
		&UpstreamRateLimitError{RetryAfter: 3 * time.Second},
	)
	if retryAfterDecision.Delay != 3*time.Second {
		t.Fatalf("Retry-After was not preserved as the delay floor: %+v", retryAfterDecision)
	}
	for i := 0; i < 100; i++ {
		if got := jitterRetryDelay(retryAfterDecision.Delay); got < retryAfterDecision.Delay {
			t.Fatalf("jitter shortened Retry-After floor: got %s want >= %s", got, retryAfterDecision.Delay)
		}
	}

	return429 := config.RateLimitConfig{Mode: config.RateLimitModeReturn429}
	if err := config.UpdateDispatchPolicies(&return429, nil, nil); err != nil {
		t.Fatalf("UpdateDispatchPolicies(return429): %v", err)
	}
	if decision := h.applyRateLimitPolicy(account, make(map[string]int), make(map[string]bool)); !decision.Return429 {
		t.Fatalf("expected direct 429 decision, got %+v", decision)
	}

	switchAccounts := config.RateLimitConfig{Mode: config.RateLimitModeSwitchAccount, SwitchAccountRounds: 3}
	if err := config.UpdateDispatchPolicies(&switchAccounts, nil, nil); err != nil {
		t.Fatalf("UpdateDispatchPolicies(switch): %v", err)
	}
	roundExcluded := map[string]bool{"a": true, "b": true}
	if !h.shouldStartNextSwitchAccountRound(roundExcluded) || getSwitchRoundsUsed(roundExcluded) != 1 || roundExcluded["a"] || roundExcluded["b"] {
		t.Fatalf("first switch round state = %#v", roundExcluded)
	}
	roundExcluded["a"] = true
	if !h.shouldStartNextSwitchAccountRound(roundExcluded) || getSwitchRoundsUsed(roundExcluded) != 2 || roundExcluded["a"] {
		t.Fatalf("second switch round state = %#v", roundExcluded)
	}
	if h.shouldStartNextSwitchAccountRound(roundExcluded) {
		t.Fatal("switch rounds exceeded configured limit")
	}
}
