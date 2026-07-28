package proxy

import (
	"testing"

	"kiro-go/config"
)

func TestExpiredTokenRefreshSingleFlightAndFailureThrottle(t *testing.T) {
	h := &Handler{
		refreshingExpiredTokens:  make(map[string]int64),
		expiredRefreshRetryAfter: make(map[string]int64),
	}

	if !h.markExpiredAccountRefreshStarted("account") {
		t.Fatal("first refresh should start")
	}
	if h.markExpiredAccountRefreshStarted("account") {
		t.Fatal("concurrent refresh should be deduplicated")
	}
	if !h.isExpiredAccountRefreshPending("account") {
		t.Fatal("started refresh should be reported pending")
	}
	h.markExpiredAccountRefreshFinished("account", true)
	if h.isExpiredAccountRefreshPending("account") {
		t.Fatal("successful refresh should clear pending state")
	}

	if !h.markExpiredAccountRefreshStarted("account") {
		t.Fatal("refresh should be startable after success")
	}
	h.markExpiredAccountRefreshFinished("account", false)
	if !h.isExpiredAccountRefreshPending("account") {
		t.Fatal("failed refresh should remain throttled")
	}
	if h.markExpiredAccountRefreshStarted("account") {
		t.Fatal("failed refresh should be throttled for the retry window")
	}

	if h.startExpiredAccountRefresh(config.Account{ID: "no-refresh-token"}) {
		t.Fatal("account without refresh material must not start background refresh")
	}
}
