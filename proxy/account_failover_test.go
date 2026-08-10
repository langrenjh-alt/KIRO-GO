package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
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

func TestMonthlyRequestCountClassifier(t *testing.T) {
	for _, body := range []string{
		`{"reason":"MONTHLY_REQUEST_COUNT"}`,
		`{"error":{"reason":"MONTHLY_REQUEST_COUNT"}}`,
		`MONTHLY_REQUEST_COUNT`,
	} {
		err := &KiroHTTPError{StatusCode: http.StatusPaymentRequired, Endpoint: "test", Body: body}
		if !isMonthlyRequestCountError(err) {
			t.Fatalf("monthly request limit was not recognized: %s", body)
		}
	}
	for _, err := range []error{
		&KiroHTTPError{StatusCode: http.StatusPaymentRequired, Endpoint: "test", Body: `{"reason":"OVERAGE"}`},
		&KiroHTTPError{StatusCode: http.StatusPaymentRequired, Endpoint: "test", Body: `{"reason":"monthly_request_count"}`},
		&KiroHTTPError{StatusCode: http.StatusForbidden, Endpoint: "test", Body: `{"reason":"MONTHLY_REQUEST_COUNT"}`},
	} {
		if isMonthlyRequestCountError(err) {
			t.Fatalf("non-monthly-limit error was misclassified: %v", err)
		}
	}
}

func TestMonthlyRequestCountIsClassifiedAsQuota(t *testing.T) {
	err := (&KiroHTTPError{
		StatusCode: http.StatusPaymentRequired,
		Body:       `{"reason":"MONTHLY_REQUEST_COUNT"}`,
	}).Error()
	if !isQuotaErrorMessage(err) {
		t.Fatalf("MONTHLY_REQUEST_COUNT was not classified as quota: %q", err)
	}
}

func TestMonthlyRequestCountDisablesCredentialWithoutOverageRefresh(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	account := config.Account{ID: "monthly-exhausted", Enabled: true, AccessToken: "token"}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	h.handleAccountFailure(&account, &KiroHTTPError{
		StatusCode: http.StatusPaymentRequired,
		Endpoint:   "test",
		Body:       `{"error":{"reason":"MONTHLY_REQUEST_COUNT"}}`,
	})

	accounts := config.GetAccounts()
	if len(accounts) != 1 || accounts[0].Enabled || accounts[0].BanStatus != "DISABLED" ||
		!strings.Contains(accounts[0].BanReason, "MONTHLY_REQUEST_COUNT") {
		t.Fatalf("monthly exhausted credential was not disabled: %#v", accounts)
	}
}

func TestCallKiroWithBearerRefreshRetriesOncePerAccountAndRequest(t *testing.T) {
	account := &config.Account{ID: "oauth-refresh", Enabled: true, AccessToken: "stale", RefreshToken: "refresh"}
	payload := &KiroPayload{requestContext: context.Background()}
	var calls, refreshes int
	var forced []bool
	h := &Handler{
		kiroCaller: func(account *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
			calls++
			if calls == 1 || calls == 3 {
				return &KiroHTTPError{StatusCode: http.StatusUnauthorized, Body: `{"message":"The bearer token included in the request is invalid."}`}
			}
			if account.AccessToken != "fresh" {
				return fmt.Errorf("retry used access token %q, want fresh", account.AccessToken)
			}
			if callback != nil && callback.OnText != nil {
				callback.OnText("recovered", false)
			}
			return nil
		},
		tokenRefresher: func(_ context.Context, account *config.Account, force bool) error {
			refreshes++
			forced = append(forced, force)
			account.AccessToken = "fresh"
			return nil
		},
	}
	if err := h.callKiroWithBearerRefresh(account, payload, &KiroStreamCallback{}); err != nil {
		t.Fatalf("first refreshed call: %v", err)
	}
	if calls != 2 || refreshes != 1 || len(forced) != 1 || !forced[0] {
		t.Fatalf("calls=%d refreshes=%d forced=%v, want one forced refresh and two upstream calls", calls, refreshes, forced)
	}
	if err := h.callKiroWithBearerRefresh(account, payload, &KiroStreamCallback{}); err == nil {
		t.Fatal("second invalid bearer response unexpectedly succeeded")
	}
	if calls != 3 || refreshes != 1 {
		t.Fatalf("refresh repeated for same account/request: calls=%d refreshes=%d", calls, refreshes)
	}
}

func TestCallKiroWithBearerRefreshSkipsAPIKeyCredential(t *testing.T) {
	account := &config.Account{ID: "api-key", Enabled: true, AuthMethod: "api_key", KiroApiKey: "ksk_test", AccessToken: "ksk_test"}
	var calls, refreshes int
	h := &Handler{
		kiroCaller: func(_ *config.Account, _ *KiroPayload, _ *KiroStreamCallback) error {
			calls++
			return &KiroHTTPError{StatusCode: http.StatusForbidden, Body: `{"message":"The bearer token included in the request is invalid."}`}
		},
		tokenRefresher: func(context.Context, *config.Account, bool) error {
			refreshes++
			return nil
		},
	}
	if err := h.callKiroWithBearerRefresh(account, &KiroPayload{}, &KiroStreamCallback{}); err == nil {
		t.Fatal("invalid API key unexpectedly succeeded")
	}
	if calls != 1 || refreshes != 0 {
		t.Fatalf("API key used OAuth refresh path: calls=%d refreshes=%d", calls, refreshes)
	}
}

func TestCallKiroWithBearerRefreshSkipsUnrelatedForbidden(t *testing.T) {
	account := &config.Account{ID: "oauth-forbidden", AccessToken: "oauth", RefreshToken: "refresh"}
	var calls, refreshes int
	h := &Handler{
		kiroCaller: func(_ *config.Account, _ *KiroPayload, _ *KiroStreamCallback) error {
			calls++
			return &KiroHTTPError{
				StatusCode: http.StatusForbidden,
				Body:       `{"message":"User is not entitled to use this service"}`,
			}
		},
		tokenRefresher: func(_ context.Context, _ *config.Account, _ bool) error {
			refreshes++
			return nil
		},
	}
	err := h.callKiroWithBearerRefresh(account, &KiroPayload{}, &KiroStreamCallback{})
	if err == nil || calls != 1 || refreshes != 0 {
		t.Fatalf("err=%v calls=%d refreshes=%d, want original error and no refresh", err, calls, refreshes)
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
