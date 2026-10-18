package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

func TestProxyPoolImportParser(t *testing.T) {
	entries, problems := parseProxyImport("# comment\nUS East|http://a:p@127.0.0.1:8000\nsocks5://[::1]:1080\nlocalhost:9000\nftp://localhost:21\nhttp://missing-port\nhttp://host:80/path", "socks5")
	if len(entries) != 3 || len(problems) != 3 {
		t.Fatalf("entries=%+v errors=%+v", entries, problems)
	}
	if entries[0].Name != "US East" || entries[2].URL != "socks5://localhost:9000" {
		t.Fatal(entries)
	}
	for _, p := range problems {
		if strings.Contains(p.Error, "a:p") {
			t.Fatal("credentials leaked")
		}
	}
}

func TestProxyPoolInspectionGeoAndProviderFailure(t *testing.T) {
	for _, geoWorks := range []bool{true, false} {
		t.Run(map[bool]string{true: "geo", false: "geo_throttled"}[geoWorks], func(t *testing.T) {
			p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/trace":
					w.Write([]byte("ip=203.0.113.7\nloc=US\n"))
				case "/geo":
					if !geoWorks {
						w.WriteHeader(429)
						return
					}
					w.Write([]byte(`{"ip":"203.0.113.7","success":true,"country":"United States","region":"Virginia","city":"Ashburn"}`))
				default:
					w.WriteHeader(500)
				}
			}))
			defer p.Close()
			result := inspectProxyEndpoints(context.Background(), config.ProxyEntry{ID: t.Name(), URL: p.URL}, "http://probe.invalid/trace", []string{"http://geo.invalid/geo"})
			if !result.Healthy || result.ExitIP != "203.0.113.7" {
				t.Fatalf("result=%+v", result)
			}
			if geoWorks && (result.City != "Ashburn" || result.Country != "United States") {
				t.Fatal(result)
			}
			if !geoWorks && !strings.Contains(result.LastError, "GeoIP") {
				t.Fatal(result)
			}
		})
	}
}

func TestProxyPoolInspectionDeadProxy(t *testing.T) {
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	raw := p.URL
	p.Close()
	r := inspectProxyEndpoints(context.Background(), config.ProxyEntry{URL: raw}, "http://probe.invalid/trace", []string{"http://geo.invalid/geo"})
	if r.Healthy || r.LastError == "" {
		t.Fatal(r)
	}
}

func proxyPoolTestHandler(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &proxyPoolController{ctx: ctx, cancel: cancel, check: func(context.Context, config.ProxyEntry) config.ProxyCheckResult {
		return config.ProxyCheckResult{Healthy: true, CheckedAt: time.Now().Unix(), ExitIP: "203.0.113.8", Country: "US", City: "Ashburn"}
	}}
	h := &Handler{pool: accountpool.GetPool(), proxyPool: c}
	t.Cleanup(func() { cancel(); c.wg.Wait() })
	return h
}

func proxyPoolRequest(h *Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/"+path, strings.NewReader(body))
	w := httptest.NewRecorder()
	switch path {
	case "settings":
		h.apiProxyPoolSettings(w, r)
	case "import":
		h.apiImportProxies(w, r)
	case "batch":
		h.apiBatchProxies(w, r)
	case "check":
		h.apiCheckProxies(w, r)
	}
	return w
}

func TestProxyPoolAdminLifecycle(t *testing.T) {
	h := proxyPoolTestHandler(t)
	w := proxyPoolRequest(h, "import", `{"text":"A|http://127.0.0.1:8011\nA|http://127.0.0.1:8011\nsocks5://localhost:1080\nftp://localhost:21","defaultScheme":"http"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	h.proxyPool.wg.Wait()
	var imported struct {
		Imported   int                `json:"imported"`
		Duplicates int                `json:"duplicates"`
		Errors     []proxyImportError `json:"errors"`
	}
	json.Unmarshal(w.Body.Bytes(), &imported)
	if imported.Imported != 2 || imported.Duplicates != 1 || len(imported.Errors) != 1 {
		t.Fatal(w.Body)
	}
	for _, level := range []string{"power", "max", "pro", "pro+"} {
		if err := config.AddAccount(config.Account{ID: level, AccountLevel: level, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	w = proxyPoolRequest(h, "settings", `{"enabled":true}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if len(config.GetProxyBindings()) != 4 {
		t.Fatal(config.GetProxyBindings())
	}
	entries := config.GetProxies()
	id := entries[0].ID
	w = proxyPoolRequest(h, "batch", `{"action":"disable","ids":["`+id+`"]}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	for _, bound := range config.GetProxyBindings() {
		if bound == id {
			t.Fatal("disabled node remained bound")
		}
	}
	if err := config.SetAccountEnabled("max", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := config.GetProxyBindings()["max"]; ok {
		t.Fatal("disabled account remained bound")
	}
	w = proxyPoolRequest(h, "batch", `{"action":"delete","ids":["`+entries[1].ID+`"]}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if len(config.GetProxyBindings()) != 0 {
		t.Fatal(config.GetProxyBindings())
	}
	w = proxyPoolRequest(h, "settings", `{"blockWithoutProxy":false}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	_, _, err := config.ResolveAccountProxy("power")
	if err != nil {
		t.Fatal(err)
	}
	w = proxyPoolRequest(h, "settings", `{"enabled":false}`)
	if w.Code != 200 || config.GetProxyPoolSettings().Enabled {
		t.Fatal(w.Body)
	}
	w = proxyPoolRequest(h, "batch", `{"action":"edit","ids":["`+id+`"],"patch":{"url":"ftp://bad:123"}}`)
	if w.Code != 400 {
		t.Fatal(w.Body)
	}
}

func TestProxyPoolInspectionConcurrentNonblockingAndNoOverlap(t *testing.T) {
	h := proxyPoolTestHandler(t)
	for _, raw := range []string{"http://localhost:8001", "http://localhost:8002", "http://localhost:8003"} {
		config.ImportProxies([]config.ProxyEntry{{URL: raw, Enabled: true}})
	}
	var current, peak atomic.Int64
	gate := make(chan struct{})
	h.proxyPool.check = func(ctx context.Context, e config.ProxyEntry) config.ProxyCheckResult {
		n := current.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-gate:
		case <-ctx.Done():
		}
		current.Add(-1)
		return config.ProxyCheckResult{Healthy: false, LastError: "dead", CheckedAt: time.Now().Unix()}
	}
	started := time.Now()
	if !h.startProxyInspection(nil, false) {
		t.Fatal("did not start")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("blocked request")
	}
	if h.startProxyInspection(nil, false) {
		t.Fatal("overlapped inspection")
	}
	deadline := time.Now().Add(time.Second)
	for peak.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	h.proxyPool.wg.Wait()
	if peak.Load() != 3 {
		t.Fatalf("peak=%d", peak.Load())
	}
	for _, e := range config.GetProxies() {
		if e.Enabled {
			t.Fatalf("dead proxy enabled: %+v", e)
		}
	}
}

func TestProxyPoolCredentialImportDefersNetworkUntilAccountBinding(t *testing.T) {
	h := proxyPoolTestHandler(t)
	s := config.GetProxyPoolSettings()
	s.Enabled = true
	if _, err := config.UpdateProxyPoolSettings(s); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/api/auth/credentials", strings.NewReader(`{"id":"import-pool-fixture","authMethod":"social","refreshToken":"synthetic-refresh-token","email":"fixture@example.invalid"}`))
	w := httptest.NewRecorder()
	h.apiImportCredentials(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 || accounts[0].ID != "import-pool-fixture" || accounts[0].RefreshToken != "synthetic-refresh-token" {
		t.Fatalf("accounts=%+v", accounts)
	}
	if len(config.GetProxyBindings()) != 0 || config.AccountProxyAllowed(accounts[0].ID) {
		t.Fatal("offline import should stay unbound and blocked")
	}
	if _, _, err := config.ResolveAccountProxy(accounts[0].ID); err != config.ErrNoAvailableProxy {
		t.Fatalf("err=%v", err)
	}
}
