package proxy

import (
	"testing"
	"time"

	"kiro-go/config"
)

func TestAssignS2APProxyKeepsTheSameExitUntilItExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	available := []s2apCandidate{
		{ID: "one", URL: "socks5://one.example:1", ExitIP: "203.0.113.1"},
		{ID: "two", URL: "socks5://two.example:1", ExitIP: "203.0.113.2"},
	}
	lease, proxyURL, changed := assignS2APProxy(now, 12, config.S2APAccountLease{}, false, available, nil, func(int) int { return 1 })
	if !changed || lease.ProxyID != "two" || proxyURL != available[1].URL || lease.ExitIP != "203.0.113.2" {
		t.Fatalf("first draw = %+v url=%s changed=%v", lease, proxyURL, changed)
	}
	again, againURL, changed := assignS2APProxy(now.Add(time.Hour), 12, lease, true, available, nil, func(int) int { return 0 })
	if changed || again.ProxyID != "two" || againURL != available[1].URL {
		t.Fatalf("hold changed: %+v url=%s changed=%v", again, againURL, changed)
	}
	next, nextURL, changed := assignS2APProxy(time.Unix(lease.Until+1, 0), 12, lease, true, available, nil, func(int) int { return 0 })
	if !changed || next.ProxyID != "one" || nextURL != available[0].URL {
		t.Fatalf("expired draw = %+v url=%s changed=%v", next, nextURL, changed)
	}
}

func TestExitIPFromProxyName(t *testing.T) {
	if got := exitIPFromProxyName("149.119.133.250:443"); got != "149.119.133.250" {
		t.Fatalf("host:port = %s", got)
	}
	if got := exitIPFromProxyName("203.0.113.8"); got != "203.0.113.8" {
		t.Fatalf("bare ip = %s", got)
	}
	if got := exitIPFromProxyName("edge-a"); got != "" {
		t.Fatalf("name = %s", got)
	}
}

func TestAccountS2APLeaseViewHidesExpiredHolds(t *testing.T) {
	now := int64(1_700_000_000)
	exitIP, until := accountS2APLeaseView(config.S2APAccountLease{ExitIP: "203.0.113.8", Until: now + 3600}, true, now)
	if exitIP != "203.0.113.8" || until != now+3600 {
		t.Fatalf("active view = %s %d", exitIP, until)
	}
	exitIP, until = accountS2APLeaseView(config.S2APAccountLease{ExitIP: "203.0.113.8", Until: now}, true, now)
	if exitIP != "" || until != 0 {
		t.Fatalf("expired view = %s %d", exitIP, until)
	}
}

func TestAssignS2APProxyFillsAnEmptyExitBeforeSharing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	available := []s2apCandidate{
		{ID: "one", URL: "socks5://one.example:1", ExitIP: "203.0.113.1"},
		{ID: "two", URL: "socks5://two.example:1", ExitIP: "203.0.113.2"},
	}
	held := config.S2APAccountLease{ProxyID: "two", Until: now.Add(time.Hour).Unix(), ExitIP: "203.0.113.2"}
	exitLoad := map[string]int{"203.0.113.2": 1}
	lease, proxyURL, changed := assignS2APProxy(now, 12, held, true, available, exitLoad, func(int) int { return 0 })
	if !changed || lease.ProxyID != "one" || lease.ExitIP != "203.0.113.1" || proxyURL != available[0].URL {
		t.Fatalf("empty exit redraw = %+v url=%s changed=%v", lease, proxyURL, changed)
	}
}

func TestAssignS2APProxyReusesTheLeastLoadedExit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	available := []s2apCandidate{
		{ID: "one", URL: "socks5://one.example:1", ExitIP: "203.0.113.1"},
		{ID: "two", URL: "socks5://two.example:1", ExitIP: "203.0.113.2"},
		{ID: "three", URL: "socks5://three.example:1", ExitIP: "203.0.113.2"},
	}
	exitLoad := map[string]int{"203.0.113.1": 1, "203.0.113.2": 3}
	lease, proxyURL, changed := assignS2APProxy(now, 12, config.S2APAccountLease{}, false, available, exitLoad, func(int) int { return 0 })
	if !changed || lease.ProxyID != "one" || lease.ExitIP != "203.0.113.1" || proxyURL != available[0].URL {
		t.Fatalf("least loaded draw = %+v url=%s changed=%v", lease, proxyURL, changed)
	}
}

func TestAssignS2APProxyKeepsASharedExitOnceEveryExitIsTaken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	available := []s2apCandidate{
		{ID: "one", URL: "socks5://one.example:1", ExitIP: "203.0.113.1"},
		{ID: "two", URL: "socks5://two.example:1", ExitIP: "203.0.113.2"},
	}
	held := config.S2APAccountLease{ProxyID: "two", Until: now.Add(time.Hour).Unix(), ExitIP: "203.0.113.2"}
	exitLoad := map[string]int{"203.0.113.1": 1, "203.0.113.2": 1}
	lease, proxyURL, changed := assignS2APProxy(now, 12, held, true, available, exitLoad, func(int) int { return 0 })
	if changed || lease.ProxyID != "two" || proxyURL != available[1].URL {
		t.Fatalf("balanced share changed: %+v url=%s changed=%v", lease, proxyURL, changed)
	}
}

func TestAssignS2APProxyMovesAnExtraOffAHeavierExit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	available := []s2apCandidate{
		{ID: "one", URL: "socks5://one.example:1", ExitIP: "203.0.113.1"},
		{ID: "two", URL: "socks5://two.example:1", ExitIP: "203.0.113.2"},
	}
	held := config.S2APAccountLease{ProxyID: "two", Until: now.Add(time.Hour).Unix(), ExitIP: "203.0.113.2"}
	exitLoad := map[string]int{"203.0.113.1": 1, "203.0.113.2": 2}
	lease, proxyURL, changed := assignS2APProxy(now, 12, held, true, available, exitLoad, func(int) int { return 0 })
	if !changed || lease.ProxyID != "one" || proxyURL != available[0].URL {
		t.Fatalf("heavier exit move = %+v url=%s changed=%v", lease, proxyURL, changed)
	}
}

func TestAssignS2APProxyRedrawsWhenTheHeldProxyIsOffline(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	held := config.S2APAccountLease{ProxyID: "gone", Until: now.Add(time.Hour).Unix(), ExitIP: "203.0.113.9"}
	available := []s2apCandidate{{ID: "live", URL: "socks5://live.example:1", ExitIP: "203.0.113.4"}}
	lease, proxyURL, changed := assignS2APProxy(now, 12, held, true, available, nil, nil)
	if !changed || lease.ProxyID != "live" || proxyURL != available[0].URL {
		t.Fatalf("offline redraw = %+v url=%s changed=%v", lease, proxyURL, changed)
	}
}
