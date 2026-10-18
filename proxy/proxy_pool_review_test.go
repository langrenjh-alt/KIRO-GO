package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

func TestProxyPoolGeoCacheExpiresAndDoesNotCrossExitIPs(t *testing.T) {
	var failGeo atomic.Bool
	var traceIP atomic.Value
	traceIP.Store("203.0.113.17")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/trace" {
			_, _ = io.WriteString(w, "ip="+traceIP.Load().(string)+"\nloc=US\n")
			return
		}
		if failGeo.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"ip":"203.0.113.17","country":"United States","region":"Virginia","city":"Ashburn"}`)
	}))
	defer server.Close()
	entry := config.ProxyEntry{ID: t.Name(), URL: server.URL}
	defer proxyGeoCache.Delete(entry.ID)
	result := inspectProxyEndpoints(context.Background(), entry, "http://probe.invalid/trace", []string{"http://probe.invalid/geo"})
	if !result.Healthy || result.City != "Ashburn" {
		t.Fatalf("initial=%+v", result)
	}
	cached, _ := proxyGeoCache.Load(entry.ID)
	old := cached.(proxyGeoCacheEntry)
	old.CheckedAt = time.Now().Add(-7 * time.Hour)
	proxyGeoCache.Store(entry.ID, old)
	failGeo.Store(true)
	result = inspectProxyEndpoints(context.Background(), entry, "http://probe.invalid/trace", []string{"http://probe.invalid/geo"})
	if !result.Healthy || result.City != "Ashburn" || !strings.Contains(result.LastError, "refresh pending") {
		t.Fatalf("cached metadata lost on throttle: %+v", result)
	}
	traceIP.Store("203.0.113.18")
	result = inspectProxyEndpoints(context.Background(), entry, "http://probe.invalid/trace", []string{"http://probe.invalid/geo"})
	if !result.Healthy || result.ExitIP != "203.0.113.18" || result.City != "" {
		t.Fatalf("old exit metadata crossed to new IP: %+v", result)
	}
}

func TestProxyPoolMalformedInspectionNeverUsesDirectTransport(t *testing.T) {
	var directCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directCalls.Add(1)
		_, _ = io.WriteString(w, "ip=203.0.113.19\n")
	}))
	defer target.Close()
	result := inspectProxyEndpoints(context.Background(), config.ProxyEntry{URL: "not-a-valid-proxy"}, target.URL, nil)
	if result.Healthy || directCalls.Load() != 0 || result.LastError != "Invalid proxy URL" {
		t.Fatalf("invalid proxy leaked a direct probe: result=%+v calls=%d", result, directCalls.Load())
	}
}

func TestProxyPoolNilCheckerAndCancellationPreserveState(t *testing.T) {
	t.Run("nil_checker", func(t *testing.T) {
		h := proxyPoolTestHandler(t)
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		raw := server.URL
		server.Close()
		if _, _, err := config.ImportProxies([]config.ProxyEntry{{URL: raw, Enabled: true}}); err != nil {
			t.Fatal(err)
		}
		h.proxyPool.check = nil
		if !h.startProxyInspection(nil, false) {
			t.Fatal("inspection did not start")
		}
		h.proxyPool.wg.Wait()
		if config.GetProxies()[0].Enabled {
			t.Fatal("nil checker did not fall back to connectivity inspection")
		}
	})
	t.Run("canceled_probe", func(t *testing.T) {
		h := proxyPoolTestHandler(t)
		if _, _, err := config.ImportProxies([]config.ProxyEntry{{URL: "http://127.0.0.1:12001", Enabled: true}}); err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		h.proxyPool.check = func(ctx context.Context, _ config.ProxyEntry) config.ProxyCheckResult {
			close(started)
			<-ctx.Done()
			return config.ProxyCheckResult{Healthy: false, LastError: "context canceled"}
		}
		h.startProxyInspection(nil, false)
		<-started
		h.proxyPool.cancel()
		h.proxyPool.wg.Wait()
		if !config.GetProxies()[0].Enabled {
			t.Fatal("shutdown cancellation disabled a usable proxy")
		}
		if h.startProxyInspection(nil, false) {
			t.Fatal("inspection accepted work after cancellation")
		}
	})
}

func TestProxyPoolManualCheckRejectsUnknownAndBusySelections(t *testing.T) {
	h := proxyPoolTestHandler(t)
	if response := proxyPoolRequest(h, "check", `{"ids":["missing"]}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown selection status=%d body=%s", response.Code, response.Body)
	}
	if _, _, err := config.ImportProxies([]config.ProxyEntry{{URL: "http://127.0.0.1:12001", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	h.proxyPool.check = func(context.Context, config.ProxyEntry) config.ProxyCheckResult {
		<-gate
		return config.ProxyCheckResult{Healthy: true}
	}
	if response := proxyPoolRequest(h, "check", `{"ids":[]}`); response.Code != http.StatusAccepted {
		t.Fatalf("start status=%d body=%s", response.Code, response.Body)
	}
	if response := proxyPoolRequest(h, "check", `{"ids":[]}`); response.Code != http.StatusConflict {
		t.Fatalf("busy status=%d body=%s", response.Code, response.Body)
	}
	close(gate)
	h.proxyPool.wg.Wait()
}
