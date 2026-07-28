package proxy

import (
	"errors"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

const (
	accountRefreshInProgressStatus  = 503
	accountRefreshInProgressMessage = "账号正在刷新中，请稍等3分钟重试"
	expiredAccountRefreshWait       = 3 * time.Minute
)

var errAccountRefreshInProgress = errors.New(accountRefreshInProgressMessage)

func isAccountRefreshInProgressError(err error) bool {
	return errors.Is(err, errAccountRefreshInProgress)
}

func (h *Handler) startExpiredAccountRefreshes(model string, excluded map[string]bool) bool {
	snap := h.pool.InspectAvailability(model, excluded)
	if snap.Expired == 0 {
		return false
	}

	startedOrPending := false
	for _, account := range snap.ExpiredAccounts {
		if h.startExpiredAccountRefresh(account) || h.isExpiredAccountRefreshPending(account.ID) {
			startedOrPending = true
		}
	}
	return startedOrPending
}

func (h *Handler) startExpiredAccountRefresh(account config.Account) bool {
	if account.ID == "" || account.RefreshToken == "" {
		return false
	}
	if !h.markExpiredAccountRefreshStarted(account.ID) {
		return false
	}

	logger.Warnf("[TokenRefresh] Account %s is expired/near-expired; starting background refresh", account.Email)
	go func() {
		err := h.ensureValidToken(&account)
		if err != nil {
			logger.Warnf("[TokenRefresh] Background refresh failed for %s: %v", account.Email, err)
			h.handleAccountFailure(&account, err)
			h.markExpiredAccountRefreshFinished(account.ID, false)
			return
		}
		logger.Infof("[TokenRefresh] Background refresh completed for %s", account.Email)
		h.markExpiredAccountRefreshFinished(account.ID, true)
	}()
	return true
}

func (h *Handler) markExpiredAccountRefreshStarted(id string) bool {
	now := time.Now().Unix()
	h.expiredRefreshMu.Lock()
	defer h.expiredRefreshMu.Unlock()

	if until, ok := h.refreshingExpiredTokens[id]; ok && until > now {
		return false
	}
	if retryAfter, ok := h.expiredRefreshRetryAfter[id]; ok && retryAfter > now {
		return false
	}

	h.refreshingExpiredTokens[id] = now + int64(expiredAccountRefreshWait.Seconds())
	return true
}

func (h *Handler) markExpiredAccountRefreshFinished(id string, success bool) {
	now := time.Now().Unix()
	h.expiredRefreshMu.Lock()
	defer h.expiredRefreshMu.Unlock()

	delete(h.refreshingExpiredTokens, id)
	if success {
		delete(h.expiredRefreshRetryAfter, id)
		return
	}
	h.expiredRefreshRetryAfter[id] = now + int64(expiredAccountRefreshWait.Seconds())
}

func (h *Handler) isExpiredAccountRefreshPending(id string) bool {
	now := time.Now().Unix()
	h.expiredRefreshMu.Lock()
	defer h.expiredRefreshMu.Unlock()

	if until, ok := h.refreshingExpiredTokens[id]; ok {
		if until > now {
			return true
		}
		delete(h.refreshingExpiredTokens, id)
	}
	if retryAfter, ok := h.expiredRefreshRetryAfter[id]; ok {
		if retryAfter > now {
			return true
		}
		delete(h.expiredRefreshRetryAfter, id)
	}
	return false
}
