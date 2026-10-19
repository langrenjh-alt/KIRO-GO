package proxy

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
)

func importRecoveryProxies(t *testing.T, urls ...string) []config.ProxyEntry {
	t.Helper()
	entries := make([]config.ProxyEntry, len(urls))
	for i, raw := range urls {
		entries[i] = config.ProxyEntry{URL: raw, Enabled: true}
	}
	if _, _, err := config.ImportProxies(entries); err != nil {
		t.Fatal(err)
	}
	return config.GetProxies()
}

func runRecoveryInspection(t *testing.T, h *Handler, ids []string, manual bool) {
	t.Helper()
	if !h.startProxyInspection(ids, manual) {
		t.Fatal("inspection did not start")
	}
	h.proxyPool.wg.Wait()
}

func TestProxyPoolAutomaticRecoveryRebindsAccountsWithoutReload(t *testing.T) {
	h := proxyPoolTestHandler(t)
	entries := importRecoveryProxies(t, "http://127.0.0.1:13001")
	for _, tier := range []string{"power", "max", "pro+"} {
		if err := config.AddAccount(config.Account{ID: tier, Enabled: true, AccountLevel: tier, AccessToken: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := config.UpdateProxyPoolSettings(config.ProxyPoolSettings{Enabled: true, BlockWithoutProxy: true}); err != nil {
		t.Fatal(err)
	}
	h.pool.Reload()
	// This same loaded account pool must observe both loss and restoration of
	// proxy availability, without a reload or process restart after either.
	assertAvailability := func(available bool) {
		t.Helper()
		bindings := config.GetProxyBindings()
		for _, tier := range []string{"power", "max", "pro+"} {
			if got := config.AccountProxyAllowed(tier); got != available {
				t.Fatalf("tier=%s allowed=%v want=%v", tier, got, available)
			}
			if available && bindings[tier] != entries[0].ID {
				t.Fatalf("tier=%s binding=%q want=%q", tier, bindings[tier], entries[0].ID)
			}
		}
		want := 0
		if available {
			want = 3
		}
		if len(bindings) != want || h.pool.AvailableCount() != want {
			t.Fatalf("bindings=%v available=%d want=%d", bindings, h.pool.AvailableCount(), want)
		}
		if got := h.pool.GetNext(); (got != nil) != available {
			t.Fatalf("loaded pool selected=%+v available=%v", got, available)
		}
	}
	for _, healthy := range []bool{true, false, true} {
		h.proxyPool.check = func(context.Context, config.ProxyEntry) config.ProxyCheckResult {
			// An injected checker cannot replace the scheduler's snapshot token.
			wrongVersion := ^uint64(0)
			return config.ProxyCheckResult{Healthy: healthy, CheckedAt: time.Now().Unix(), ExitIP: "203.0.113.31", Country: "US", City: "Ashburn", ExpectedStateVersion: &wrongVersion}
		}
		runRecoveryInspection(t, h, nil, false)
		entry := config.GetProxies()[0]
		if entry.Enabled != healthy {
			t.Fatalf("cycle healthy=%v entry=%+v", healthy, entry)
		}
		if !healthy && entry.DisabledBy != config.ProxyDisabledHealth {
			t.Fatalf("failed proxy is not recoverable: %+v", entry)
		}
		if healthy && (entry.Health != config.ProxyHealthHealthy || entry.DisabledBy != "") {
			t.Fatalf("recovered proxy did not leave quarantine: %+v", entry)
		}
		assertAvailability(healthy)
	}
}

func TestProxyPoolAutomaticRecoverySkipsManualAndPrioritizesHealthFailures(t *testing.T) {
	h := proxyPoolTestHandler(t)
	entries := importRecoveryProxies(t, "http://127.0.0.1:13001", "http://127.0.0.1:13002", "http://127.0.0.1:13003")
	manual := false
	if err := config.UpdateProxies([]string{entries[1].ID}, config.ProxyPatch{Enabled: &manual}); err != nil {
		t.Fatal(err)
	}
	if err := config.DisableProxy(entries[2].ID, entries[2].URL, "outbound proxy network failure"); err != nil {
		t.Fatal(err)
	}
	settings := config.GetProxyPoolSettings()
	settings.CheckConcurrency = 1
	if _, err := config.UpdateProxyPoolSettings(settings); err != nil {
		t.Fatal(err)
	}
	var calls []string
	var mu sync.Mutex
	h.proxyPool.check = func(_ context.Context, entry config.ProxyEntry) config.ProxyCheckResult {
		mu.Lock()
		calls = append(calls, entry.ID)
		mu.Unlock()
		return config.ProxyCheckResult{Healthy: true, CheckedAt: time.Now().Unix()}
	}
	runRecoveryInspection(t, h, nil, false)
	if !reflect.DeepEqual(calls, []string{entries[2].ID, entries[0].ID}) {
		t.Fatalf("automatic probe order=%v", calls)
	}
	now := config.GetProxies()
	if !now[2].Enabled || now[2].DisabledBy != "" || now[1].Enabled || now[1].DisabledBy != config.ProxyDisabledManual {
		t.Fatalf("wrong disable-source recovery: %+v", now)
	}
	for _, healthy := range []bool{false, true} {
		h.proxyPool.check = func(context.Context, config.ProxyEntry) config.ProxyCheckResult {
			return config.ProxyCheckResult{Healthy: healthy, CheckedAt: time.Now().Unix()}
		}
		runRecoveryInspection(t, h, []string{entries[1].ID}, true)
		entry := config.GetProxies()[1]
		if entry.Enabled || entry.DisabledBy != config.ProxyDisabledManual {
			t.Fatalf("manual probe changed manual disable intent: %+v", entry)
		}
	}
}

func TestProxyPoolAutomaticRecoveryIgnoresInflightSameSecondMutations(t *testing.T) {
	for _, mutation := range []string{"manual_disable", "runtime_failure", "explicit_enable", "same_url_edit"} {
		t.Run(mutation, func(t *testing.T) {
			h := proxyPoolTestHandler(t)
			entry := importRecoveryProxies(t, "http://127.0.0.1:13001")[0]
			started := make(chan struct{})
			gate := make(chan struct{})
			var checkedAt int64
			h.proxyPool.check = func(context.Context, config.ProxyEntry) config.ProxyCheckResult {
				close(started)
				<-gate
				return config.ProxyCheckResult{Healthy: true, CheckedAt: checkedAt, ExitIP: "203.0.113.32"}
			}
			if !h.startProxyInspection(nil, false) {
				t.Fatal("inspection did not start")
			}
			<-started
			var err error
			switch mutation {
			case "manual_disable":
				enabled := false
				err = config.UpdateProxies([]string{entry.ID}, config.ProxyPatch{Enabled: &enabled})
			case "runtime_failure":
				err = config.DisableProxyIfCurrent(entry.ID, entry.URL, entry.StateVersion, "newer network failure")
			case "explicit_enable":
				enabled := true
				err = config.UpdateProxies([]string{entry.ID}, config.ProxyPatch{Enabled: &enabled})
			case "same_url_edit":
				err = config.UpdateProxies([]string{entry.ID}, config.ProxyPatch{URL: &entry.URL})
			}
			if err != nil {
				close(gate)
				t.Fatal(err)
			}
			afterMutation := config.GetProxies()[0]
			// Give the old result the exact same timestamp as the mutation: only
			// the version token, not second-resolution time, can reject it.
			checkedAt = afterMutation.LastChecked
			close(gate)
			h.proxyPool.wg.Wait()
			if got := config.GetProxies()[0]; !reflect.DeepEqual(got, afterMutation) {
				t.Fatalf("inflight result overwrote %s: got=%+v want=%+v", mutation, got, afterMutation)
			}
		})
	}
}

func TestProxyPoolCanceledRecoveryPreservesQuarantine(t *testing.T) {
	h := proxyPoolTestHandler(t)
	entry := importRecoveryProxies(t, "http://127.0.0.1:13001")[0]
	if err := config.DisableProxy(entry.ID, entry.URL, "connection refused"); err != nil {
		t.Fatal(err)
	}
	before := config.GetProxies()[0]
	started := make(chan struct{})
	h.proxyPool.check = func(ctx context.Context, _ config.ProxyEntry) config.ProxyCheckResult {
		close(started)
		<-ctx.Done()
		return config.ProxyCheckResult{Healthy: true, CheckedAt: time.Now().Unix()}
	}
	if !h.startProxyInspection(nil, false) {
		t.Fatal("recovery did not start")
	}
	<-started
	if h.startProxyInspection(nil, false) {
		t.Fatal("recovery inspections overlapped")
	}
	h.proxyPool.cancel()
	h.proxyPool.wg.Wait()
	if after := config.GetProxies()[0]; !reflect.DeepEqual(after, before) {
		t.Fatalf("canceled recovery changed quarantine: before=%+v after=%+v", before, after)
	}
}
