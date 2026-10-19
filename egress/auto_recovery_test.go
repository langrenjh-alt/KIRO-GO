package egress

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

func recoverCurrentProxy(t *testing.T, entry config.ProxyEntry) config.ProxyEntry {
	t.Helper()
	current := config.GetProxies()[0]
	if err := config.RecordProxyCheck(entry.ID, entry.URL, config.ProxyCheckResult{Healthy: true, CheckedAt: time.Now().Unix(), ExitIP: "203.0.113.40", ExpectedStateVersion: &current.StateVersion}); err != nil {
		t.Fatal(err)
	}
	current = config.GetProxies()[0]
	if !current.Enabled || current.Health != config.ProxyHealthHealthy {
		t.Fatalf("fixture recovery failed: %+v", current)
	}
	return current
}

func TestInflightRequestFailureDoesNotDisableRecoveredProxy(t *testing.T) {
	entry := configurePool(t, true, "http://127.0.0.1:14001")[0]
	started := make(chan struct{})
	gate := make(chan struct{})
	var calls atomic.Int64
	transport := New("account", "", func(string) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-gate
				return nil, io.ErrUnexpectedEOF
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("new generation"))}, nil
		})
	})
	request, _ := http.NewRequest(http.MethodGet, "http://never-direct.invalid", nil)
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := transport.RoundTrip(request)
		done <- result{response, err}
	}()
	<-started
	if err := config.DisableProxyIfCurrent(entry.ID, entry.URL, entry.StateVersion, "newer runtime failure"); err != nil {
		close(gate)
		t.Fatal(err)
	}
	recovered := recoverCurrentProxy(t, entry)
	close(gate)
	finished := <-done
	if body := readResponse(t, finished.response, finished.err); body != "new generation" || calls.Load() != 2 {
		t.Fatalf("old request did not resolve latest generation: body=%q calls=%d", body, calls.Load())
	}
	if got := config.GetProxies()[0]; !reflect.DeepEqual(got, recovered) {
		t.Fatalf("old request failure disabled recovered proxy: got=%+v want=%+v", got, recovered)
	}
	if config.GetProxyBindings()["account"] != entry.ID {
		t.Fatal("old request failure removed recovered binding")
	}
}

func TestInflightStreamFailureDoesNotDisableRecoveredProxy(t *testing.T) {
	entry := configurePool(t, true, "http://127.0.0.1:14001")[0]
	transport := New("account", "", func(string) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &truncatedBody{}}, nil
		})
	})
	request, _ := http.NewRequest(http.MethodGet, "http://never-direct.invalid", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := config.DisableProxyIfCurrent(entry.ID, entry.URL, entry.StateVersion, "newer runtime failure"); err != nil {
		t.Fatal(err)
	}
	recovered := recoverCurrentProxy(t, entry)
	body, err := io.ReadAll(response.Body)
	if string(body) != "partial-stream" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body=%q err=%v", body, err)
	}
	if got := config.GetProxies()[0]; !reflect.DeepEqual(got, recovered) {
		t.Fatalf("old stream failure disabled recovered proxy: got=%+v want=%+v", got, recovered)
	}
	if config.GetProxyBindings()["account"] != entry.ID || Stats(entry.ID).CurrentConcurrency != 0 {
		t.Fatalf("binding/concurrency lost: bindings=%v stats=%+v", config.GetProxyBindings(), Stats(entry.ID))
	}
}

func TestRoutineHealthyInspectionDoesNotHideLaterStreamFailure(t *testing.T) {
	entry := configurePool(t, true, "http://127.0.0.1:14001")[0]
	transport := New("account", "", func(string) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &truncatedBody{}}, nil
		})
	})
	request, _ := http.NewRequest(http.MethodGet, "http://never-direct.invalid", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := config.RecordProxyCheck(entry.ID, entry.URL, config.ProxyCheckResult{Healthy: true, CheckedAt: time.Now().Unix(), ExitIP: "203.0.113.41", ExpectedStateVersion: &entry.StateVersion}); err != nil {
		t.Fatal(err)
	}
	if got := config.GetProxies()[0]; got.StateVersion != entry.StateVersion {
		t.Fatalf("metadata-only inspection changed the routing generation: got=%+v initial=%+v", got, entry)
	}
	if _, err := io.ReadAll(response.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("stream err=%v", err)
	}
	if got := config.GetProxies()[0]; got.Enabled || got.DisabledBy != config.ProxyDisabledHealth {
		t.Fatalf("routine inspection hid a later stream network failure: %+v", got)
	}
	if config.AccountProxyAllowed("account") || len(config.GetProxyBindings()) != 0 {
		t.Fatal("stream failure left the account routable without a usable proxy")
	}
}
