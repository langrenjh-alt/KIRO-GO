package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

const maxAccountRetryAttempts = 3
const allAccountsConcurrencyFullMessage = "所有账号并发已满请稍后再试"

func allAccountsModelCoolingMessage(model string) string {
	return "all accounts are cooling down for model " + strings.TrimSpace(model)
}

func isAllAccountsModelCoolingError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "all accounts are cooling down for model ")
}

func (h *Handler) unavailableAccountError(model string, allFull bool) error {
	if h != nil && h.pool != nil && h.pool.AllEligibleAccountsModelCooling(model) {
		return fmt.Errorf("%s", allAccountsModelCoolingMessage(model))
	}
	if allFull {
		return fmt.Errorf("%s", allAccountsConcurrencyFullMessage)
	}
	return nil
}

// UpstreamRateLimitError marks HTTP 429 returned by Kiro/AWS upstream.
type UpstreamRateLimitError struct {
	Endpoint   string
	Body       string
	RetryAfter time.Duration
}

func (e *UpstreamRateLimitError) Error() string {
	if e == nil {
		return "HTTP 429 upstream rate limit"
	}
	if strings.TrimSpace(e.Body) != "" {
		return fmt.Sprintf("HTTP 429 from %s: %s", e.Endpoint, e.Body)
	}
	if strings.TrimSpace(e.Endpoint) != "" {
		return fmt.Sprintf("HTTP 429 from %s: upstream rate limit", e.Endpoint)
	}
	return "HTTP 429 upstream rate limit"
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*UpstreamRateLimitError); ok {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 429") ||
		strings.Contains(msg, "status 429") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "rate limit")
}

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "quota") || strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "429") || strings.Contains(msg, "monthly_request_count")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
}

// isMonthlyRequestCountBody mirrors kiro.rs's endpoint classifier. The
// upstream has emitted this reason both at the top level and under error, and
// a few gateways return the literal reason as plain text.
func isMonthlyRequestCountBody(body string) bool {
	if strings.Contains(body, "MONTHLY_REQUEST_COUNT") {
		return true
	}
	var value map[string]interface{}
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		return false
	}
	if reason, ok := value["reason"].(string); ok && reason == "MONTHLY_REQUEST_COUNT" {
		return true
	}
	if nested, ok := value["error"].(map[string]interface{}); ok {
		if reason, ok := nested["reason"].(string); ok && reason == "MONTHLY_REQUEST_COUNT" {
			return true
		}
	}
	return false
}

func isMonthlyRequestCountError(err error) bool {
	var httpErr *KiroHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusPaymentRequired && isMonthlyRequestCountBody(httpErr.Body)
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isKiroTransportError(err), isRetryableKiroStreamError(err), isKiroTransientHTTPError(err), isKiroTimeoutError(err):
		// Connection resets, proxy failures, timeouts, and retryable stream
		// framing errors do not indicate a bad credential. Penalizing them can
		// cool every account after a shared network incident.
		return
	case isMonthlyRequestCountError(err):
		h.disableAccount(account, "DISABLED", "Monthly request quota exhausted (MONTHLY_REQUEST_COUNT)")
		h.pool.RecordError(account.ID, true)
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isRateLimitError(err):
		// Request handlers apply the configured 429 policy. Keep this as a
		// defensive fallback that records the error without the legacy 1h cooldown.
		h.pool.RecordRateLimit(account.ID)
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordError(account.ID, true)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case isAuthErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	case isKiroClientRequestError(err):
		// Request and configuration errors are independent of credential health.
		// Authentication, quota, and rate-limit statuses are handled above.
		return
	default:
		h.pool.RecordError(account.ID, false)
	}
}

type rateLimitDecision struct {
	Return429   bool
	RetrySameID string
	Delay       time.Duration
}

type retryBudget struct {
	mu          sync.Mutex
	limit       int
	windowStart time.Time
	used        int
}

func newRetryBudgetFromEnv() *retryBudget {
	return &retryBudget{limit: positiveEnvInt("KIRO_RETRY_BUDGET_PER_SECOND", 128)}
}

func (b *retryBudget) allow(now time.Time) bool {
	if b == nil || b.limit <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.windowStart.IsZero() || now.Sub(b.windowStart) >= time.Second || now.Before(b.windowStart) {
		b.windowStart = now
		b.used = 0
	}
	if b.used >= b.limit {
		return false
	}
	b.used++
	return true
}

func (h *Handler) allowUpstreamRetry() bool {
	return h == nil || h.retryBudget == nil || h.retryBudget.allow(time.Now())
}

func (h *Handler) maxAccountAttempts() int {
	count := h.pool.Count()
	if count < 1 {
		count = 1
	}
	cfg := config.GetRateLimitConfig()
	switch cfg.Mode {
	case config.RateLimitModeRetrySame:
		attempts := count * (cfg.RetrySameAccount + 1)
		if attempts < maxAccountRetryAttempts {
			return maxAccountRetryAttempts
		}
		return attempts
	case config.RateLimitModeSwitchAccount:
		attempts := count * cfg.SwitchAccountRounds
		if attempts < maxAccountRetryAttempts {
			return maxAccountRetryAttempts
		}
		return attempts
	default:
		if count < maxAccountRetryAttempts {
			return maxAccountRetryAttempts
		}
		return count
	}
}

const switchRoundMarkerPrefix = "__kiro_switch_round_used_"

func (h *Handler) acquireAccountForModel(model string, excluded map[string]bool, preferredID string, requestContexts ...context.Context) (*config.Account, bool) {
	started := time.Now()
	var requestContext context.Context
	if len(requestContexts) > 0 {
		requestContext = requestContexts[0]
	}
	defer func() {
		observeRequestStage(requestContext, stageAccountPoolWait, time.Since(started))
	}()
	// Requests that skip expired/near-expired accounts should kick off one
	// background refresh attempt, so operators do not have to manually refresh
	// accounts that still have a valid refresh token.
	h.startExpiredAccountRefreshes(model, excluded)

	if preferredID != "" {
		if acc, full := h.pool.AcquireByIDForModel(preferredID, model, excluded); acc != nil || full {
			return acc, full
		}
	}
	acc, full := h.pool.AcquireNextForModelExcluding(model, excluded)
	if acc != nil || full {
		return acc, full
	}
	if h.shouldStartNextSwitchAccountRound(excluded) {
		acc, full = h.pool.AcquireNextForModelExcluding(model, excluded)
	}
	return acc, full
}

func (h *Handler) shouldStartNextSwitchAccountRound(excluded map[string]bool) bool {
	if excluded == nil {
		return false
	}
	cfg := config.GetRateLimitConfig()
	if cfg.Mode != config.RateLimitModeSwitchAccount || cfg.SwitchAccountRounds <= 1 {
		return false
	}
	used := getSwitchRoundsUsed(excluded)
	if used+1 >= cfg.SwitchAccountRounds {
		return false
	}
	clearExcludedAccountsForNextSwitchRound(excluded)
	setSwitchRoundsUsed(excluded, used+1)
	logger.Warnf("[RateLimit] Starting switch-account retry round %d/%d", used+2, cfg.SwitchAccountRounds)
	return true
}

func getSwitchRoundsUsed(excluded map[string]bool) int {
	for key := range excluded {
		if strings.HasPrefix(key, switchRoundMarkerPrefix) {
			var n int
			_, _ = fmt.Sscanf(key, switchRoundMarkerPrefix+"%d", &n)
			if n > 0 {
				return n
			}
		}
	}
	return 0
}

func setSwitchRoundsUsed(excluded map[string]bool, n int) {
	for key := range excluded {
		if strings.HasPrefix(key, switchRoundMarkerPrefix) {
			delete(excluded, key)
		}
	}
	excluded[fmt.Sprintf("%s%d", switchRoundMarkerPrefix, n)] = true
}

func clearExcludedAccountsForNextSwitchRound(excluded map[string]bool) {
	for key := range excluded {
		if !strings.HasPrefix(key, switchRoundMarkerPrefix) {
			delete(excluded, key)
		}
	}
}

func (h *Handler) releaseAccount(account *config.Account) {
	if account == nil {
		return
	}
	h.pool.Release(account.ID)
}

func (h *Handler) applyRateLimitPolicy(account *config.Account, retryCounts map[string]int, excluded map[string]bool, causes ...error) rateLimitDecision {
	if account == nil {
		return rateLimitDecision{}
	}
	cfg := config.GetRateLimitConfig()
	var cause error
	if len(causes) > 0 {
		cause = causes[0]
	}
	if (cfg.Mode == config.RateLimitModeRetrySame || cfg.Mode == config.RateLimitModeSwitchAccount) && !h.allowUpstreamRetry() {
		h.pool.RecordRateLimit(account.ID)
		logger.Warnf("[RateLimit] Global retry budget exhausted; returning 429")
		return rateLimitDecision{Return429: true}
	}
	switch cfg.Mode {
	case config.RateLimitModeReturn429:
		h.pool.RecordRateLimit(account.ID)
		return rateLimitDecision{Return429: true}
	case config.RateLimitModeRetrySame:
		used := retryCounts[account.ID]
		if used < cfg.RetrySameAccount {
			retryCounts[account.ID] = used + 1
			h.pool.RecordRateLimit(account.ID)
			logger.Warnf("[RateLimit] Upstream 429 for %s, retrying same account %d/%d", account.Email, used+1, cfg.RetrySameAccount)
			delay := retrySameDelay(cfg, used+1)
			if retryAfter := retryAfterFromError(cause); retryAfter > delay {
				delay = retryAfter
			}
			return rateLimitDecision{RetrySameID: account.ID, Delay: delay}
		}
		excluded[account.ID] = true
		h.pool.RecordRateLimit(account.ID)
		logger.Warnf("[RateLimit] Upstream 429 for %s after same-account retries, switching account", account.Email)
		return rateLimitDecision{}
	case config.RateLimitModeDisableAccount:
		d := time.Duration(cfg.DisableSeconds) * time.Second
		excluded[account.ID] = true
		h.pool.CooldownAccount(account.ID, d)
		logger.Warnf("[RateLimit] Upstream 429 for %s, temporarily disabled for %s", account.Email, d)
		return rateLimitDecision{}
	default:
		excluded[account.ID] = true
		h.pool.RecordRateLimit(account.ID)
		logger.Warnf("[RateLimit] Upstream 429 for %s, switching account", account.Email)
		return rateLimitDecision{}
	}
}

func retrySameDelay(cfg config.RateLimitConfig, retryNumber int) time.Duration {
	if retryNumber < 1 {
		retryNumber = 1
	}
	delayMs := float64(cfg.RetryDelayMs)
	if cfg.RetryBackoffMultiplier > 1 && retryNumber > 1 {
		delayMs *= math.Pow(cfg.RetryBackoffMultiplier, float64(retryNumber-1))
	}
	if maxMs := float64(cfg.RetryMaxDelayMs); maxMs > 0 && delayMs > maxMs {
		delayMs = maxMs
	}
	if delayMs < 0 {
		delayMs = 0
	}
	return time.Duration(delayMs) * time.Millisecond
}

func (h *Handler) waitBeforeRateLimitRetry(payload *KiroPayload, decision rateLimitDecision) bool {
	if decision.Delay <= 0 {
		return true
	}
	logger.Warnf("[RateLimit] Waiting %s before same-account retry", decision.Delay)
	return waitForRateLimitRetry(kiroPayloadContext(payload), decision)
}

func waitForRateLimitRetry(ctx context.Context, decision rateLimitDecision) bool {
	if decision.Delay <= 0 {
		return true
	}
	timer := time.NewTimer(jitterRetryDelay(decision.Delay))
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func jitterRetryDelay(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	// Only add jitter. The configured delay can include an upstream Retry-After
	// value and therefore acts as a lower bound that must never be shortened.
	factor := 1.0 + rand.Float64()*0.2
	return time.Duration(float64(delay) * factor)
}

func retryAfterFromError(err error) time.Duration {
	var rateErr *UpstreamRateLimitError
	if errors.As(err, &rateErr) && rateErr.RetryAfter > 0 {
		return rateErr.RetryAfter
	}
	return 0
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func isAllAccountsConcurrencyFullError(err error) bool {
	return err != nil && err.Error() == allAccountsConcurrencyFullMessage
}
