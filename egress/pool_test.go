package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-go/config"
)

func configurePool(t *testing.T, block bool, urls ...string) []config.ProxyEntry {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "account", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	entries := make([]config.ProxyEntry, len(urls))
	for i, raw := range urls {
		entries[i] = config.ProxyEntry{URL: raw, Enabled: true}
	}
	if len(entries) > 0 {
		if _, _, err := config.ImportProxies(entries); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := config.UpdateProxyPoolSettings(config.ProxyPoolSettings{Enabled: true, BlockWithoutProxy: block}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = config.UpdateProxyPoolSettings(config.ProxyPoolSettings{})
	})
	return config.GetProxies()
}

func closedProxy(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return "http://" + address
}

func readResponse(t *testing.T, response *http.Response, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestPoolFailClosedAndOptionalDirect(t *testing.T) {
	var directCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directCalls.Add(1)
		_, _ = io.WriteString(w, "direct")
	}))
	defer target.Close()
	configurePool(t, true)
	client := &http.Client{Transport: New("account", closedProxy(t), testFactory)}
	if response, err := client.Get(target.URL); !errors.Is(err, config.ErrNoAvailableProxy) || response != nil {
		t.Fatalf("unbound account was not blocked: response=%v err=%v", response, err)
	}
	if directCalls.Load() != 0 {
		t.Fatal("blocked request reached target directly")
	}
	// The very same cached transport must observe the independent block switch.
	if _, err := config.UpdateProxyPoolSettings(config.ProxyPoolSettings{Enabled: true, BlockWithoutProxy: false}); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(target.URL)
	if got := readResponse(t, response, err); got != "direct" || directCalls.Load() != 1 {
		t.Fatalf("allowed direct fallback failed: %q, calls=%d", got, directCalls.Load())
	}
	if err := config.SetAccountEnabled("account", false); err != nil {
		t.Fatal(err)
	}
	if response, err := client.Get(target.URL); !errors.Is(err, config.ErrAccountProxyUnavailable) || response != nil {
		t.Fatalf("disabled account bypassed optional-direct policy: response=%v err=%v", response, err)
	}
	if directCalls.Load() != 1 {
		t.Fatal("disabled account reached target")
	}
}

func TestCachedTransportResolvesLiveBindingAndTraffic(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "proxy-first-response") }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "proxy-second-response") }))
	defer second.Close()
	entries := configurePool(t, true, first.URL, second.URL)
	transport := New("account", "", testFactory)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	response, err := client.Get("http://unreachable-target.invalid/test")
	if err != nil {
		t.Fatal(err)
	}
	if got := Stats(entries[0].ID); got.CurrentConcurrency != 1 {
		t.Fatalf("response lifetime not counted: %+v", got)
	}
	if got := readResponse(t, response, nil); got != "proxy-first-response" {
		t.Fatal(got)
	}
	disabled := false
	if err := config.UpdateProxies([]string{entries[0].ID}, config.ProxyPatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	response, err = client.Get("http://unreachable-target.invalid/test")
	if got := readResponse(t, response, err); got != "proxy-second-response" {
		t.Fatal(got)
	}
	for _, entry := range entries {
		if got := Stats(entry.ID); got.CurrentConcurrency != 0 || got.TotalBytes <= 0 || PendingTraffic(entry.ID) <= 0 {
			t.Fatalf("missing payload/closed-body stats: %+v pending=%d", got, PendingTraffic(entry.ID))
		}
	}
	if err := FlushTraffic(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range config.GetProxies() {
		if entry.TotalBytes <= 0 || PendingTraffic(entry.ID) != 0 {
			t.Fatalf("traffic did not persist exactly once: %+v pending=%d", entry, PendingTraffic(entry.ID))
		}
	}
	if err := FlushTraffic(); err != nil {
		t.Fatal(err)
	}
	if got := config.GetProxies()[0].TotalBytes; got != int64(len("proxy-first-response")) {
		t.Fatalf("traffic duplicated: %d", got)
	}
}

func TestProxyFailureDisablesAndReplaysBodyOnNewProxy(t *testing.T) {
	var calls atomic.Int64
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(w, req.Body)
	}))
	defer healthy.Close()
	entries := configurePool(t, true, closedProxy(t), healthy.URL)
	transport := New("account", "", testFactory)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	response, err := client.Post("http://never-direct.invalid/inference", "text/plain", strings.NewReader("replayable-body"))
	if got := readResponse(t, response, err); got != "replayable-body" || calls.Load() != 1 {
		t.Fatalf("failover payload=%q calls=%d", got, calls.Load())
	}
	if entriesNow := config.GetProxies(); entriesNow[0].Enabled || entriesNow[0].Health != config.ProxyHealthUnhealthy {
		t.Fatalf("failed proxy remains usable: %+v", entriesNow[0])
	}
	if bound := config.GetProxyBindings()["account"]; bound != entries[1].ID {
		t.Fatalf("new binding=%s", bound)
	}
}

func TestNonReplayableBodyIsNotRetried(t *testing.T) {
	var calls atomic.Int64
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer healthy.Close()
	configurePool(t, true, closedProxy(t), healthy.URL)
	transport := New("account", "", testFactory)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, _ := http.NewRequest(http.MethodPost, "http://never-direct.invalid", io.NopCloser(strings.NewReader("not-replayable")))
	if _, err := client.Do(request); err == nil || calls.Load() != 0 {
		t.Fatalf("non-replayable request retried: err=%v calls=%d", err, calls.Load())
	}
	if config.GetProxies()[0].Enabled {
		t.Fatal("non-replayable failure did not disable failed proxy")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type truncatedBody struct{ read bool }

func (b *truncatedBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, io.ErrUnexpectedEOF
	}
	b.read = true
	return copy(p, "partial-stream"), nil
}
func (b *truncatedBody) Close() error { return nil }

func TestStreamFailureDisablesWithoutReplaying(t *testing.T) {
	entries := configurePool(t, true, "http://127.0.0.1:12001", "http://127.0.0.1:12002")
	var calls atomic.Int64
	transport := New("account", "", func(string) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &truncatedBody{}}, nil
		})
	})
	request, _ := http.NewRequest(http.MethodGet, "http://never-direct.invalid", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !errors.Is(err, io.ErrUnexpectedEOF) || string(body) != "partial-stream" || calls.Load() != 1 {
		t.Fatalf("stream replayed/lost: body=%q err=%v calls=%d", body, err, calls.Load())
	}
	if config.GetProxies()[0].Enabled || config.GetProxyBindings()["account"] != entries[1].ID {
		t.Fatal("stream network failure did not disable/reassign")
	}
	if Stats(entries[0].ID).CurrentConcurrency != 0 {
		t.Fatal("stream failure leaked concurrency")
	}
}

func TestCancellationAndHTTPStatusDoNotDisableProxy(t *testing.T) {
	configurePool(t, true, "http://127.0.0.1:12001")
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		transport := New("account", "", func(string) http.RoundTripper {
			return roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("upstream failure"))}, nil
			})
		})
		request, _ := http.NewRequest(http.MethodGet, "http://never-direct.invalid", nil)
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	transport := New("account", "", func(string) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })
	})
	request, _ := http.NewRequest(http.MethodGet, "http://never-direct.invalid", nil)
	if _, err := transport.RoundTrip(request); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if !config.GetProxies()[0].Enabled {
		t.Fatal("upstream status or caller cancellation disabled a healthy proxy")
	}
}

func TestCONNECTFailureIsProxyFailure(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			t.Errorf("expected CONNECT, got %s", req.Method)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	configurePool(t, true, bad.URL)
	client := &http.Client{Transport: New("account", "", testFactory)}
	if _, err := client.Get("https://never-direct.invalid"); !errors.Is(err, config.ErrNoAvailableProxy) {
		t.Fatalf("CONNECT did not disable/fail closed: %v", err)
	}
	if config.GetProxies()[0].Enabled {
		t.Fatal("CONNECT-failed proxy remains enabled")
	}
}

func TestProxyPoolConcurrentSwitchNeverProducesMixedDirectPolicy(t *testing.T) {
	configurePool(t, true, "http://127.0.0.1:12001")
	var directCalls, allowedCalls atomic.Int64
	transport := New("account", "http://legacy-proxy.invalid:8080", func(raw string) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			if raw == "" {
				directCalls.Add(1)
			} else {
				allowedCalls.Add(1)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})
	})
	flipped := make(chan error, 1)
	go func() {
		for i := 0; i < 80; i++ {
			if _, err := config.UpdateProxyPoolSettings(config.ProxyPoolSettings{Enabled: i%2 == 0, BlockWithoutProxy: true}); err != nil {
				flipped <- err
				return
			}
			runtime.Gosched()
		}
		flipped <- nil
	}()
	for i := 0; i < 800; i++ {
		request, _ := http.NewRequest(http.MethodGet, "http://never-direct.invalid", nil)
		response, err := transport.RoundTrip(request)
		if err != nil {
			<-flipped
			t.Fatalf("atomic policy unexpectedly rejected configured account: %v", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		runtime.Gosched()
	}
	if err := <-flipped; err != nil {
		t.Fatal(err)
	}
	// On uses the pool; off uses the configured legacy fallback. Neither valid
	// policy is direct, so a direct call proves a mixed-version policy read.
	if directCalls.Load() != 0 || allowedCalls.Load() != 800 {
		t.Fatalf("mixed switch/binding snapshot: direct=%d allowed=%d", directCalls.Load(), allowedCalls.Load())
	}
}
