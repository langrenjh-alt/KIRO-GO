package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

func testFactory(proxyURL string) http.RoundTripper {
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: time.Second}).DialContext}
	if proxyURL != "" {
		u, _ := url.Parse(proxyURL)
		transport.Proxy = http.ProxyURL(u)
	}
	return transport
}

func TestProxyNetworkErrorClassification(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"refused", context.Background(), &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"socks", context.Background(), errors.New("socks connect tcp: authentication failed"), true},
		{"connect502", context.Background(), proxyConnectError{status: 502}, true},
		{"closed", context.Background(), io.ErrUnexpectedEOF, true},
		{"canceled", canceled, &net.OpError{Op: "read", Err: errors.New("reset")}, false},
		{"deadline", context.Background(), context.DeadlineExceeded, false},
		{"proxyDialDeadline", context.Background(), &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}, true},
		{"tls", context.Background(), errors.New("tls: failed to verify certificate: x509: unknown authority"), false},
		{"upstreamHeaders", context.Background(), errors.New("net/http: timeout awaiting response headers"), false},
		{"upstream429", context.Background(), errors.New("HTTP 429: quota exhausted"), false},
		{"upstream500", context.Background(), errors.New("HTTP 500: Internal Server Error"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := IsProxyNetworkError(test.ctx, test.err); got != test.want {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

func TestStatsTrackPayloadConcurrencyAndWindow(t *testing.T) {
	id := t.Name()
	m := metricsFor(id)
	m.begin()
	m.add(150)
	if got := Stats(id); got.CurrentConcurrency != 1 || got.TotalBytes != 150 || got.CurrentBytesPerSecond != 30 {
		t.Fatalf("unexpected active stats: %+v", got)
	}
	if got := PendingTraffic(id); got != 150 {
		t.Fatalf("pending=%d", got)
	}
	m.end()
	m.mu.Lock()
	for i := range m.buckets {
		m.buckets[i].second -= 6
	}
	m.mu.Unlock()
	if got := Stats(id); got.CurrentConcurrency != 0 || got.CurrentBytesPerSecond != 0 || got.TotalBytes != 150 {
		t.Fatalf("unexpected idle stats: %+v", got)
	}
}

func TestHTTPAndSOCKS5Transports(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	var targetCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		targetCalls.Add(1)
		_, _ = io.Copy(w, req.Body)
	}))
	defer target.Close()
	var httpCalls atomic.Int64
	forward := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		httpCalls.Add(1)
		out := req.Clone(req.Context())
		out.RequestURI = ""
		resp, err := (&http.Transport{Proxy: nil}).RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(w, resp.Body)
	}))
	defer forward.Close()
	socksURL, socksCalls := startSOCKS5(t)
	for _, proxyURL := range []string{forward.URL, socksURL} {
		transport := New("", proxyURL, testFactory)
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		resp, err := client.Post(target.URL, "text/plain", strings.NewReader("payload-through-proxy"))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		transport.CloseIdleConnections()
		if err != nil || string(body) != "payload-through-proxy" {
			t.Fatalf("proxy %s body=%q err=%v", proxyURL, body, err)
		}
	}
	if httpCalls.Load() != 1 || socksCalls.Load() != 1 || targetCalls.Load() != 2 {
		t.Fatalf("wrong proxy routing: http=%d socks=%d target=%d", httpCalls.Load(), socksCalls.Load(), targetCalls.Load())
	}
}

// startSOCKS5 is a small test-only SOCKS5 CONNECT server. It forwards a real
// TCP connection, so the test exercises net/http's SOCKS protocol path.
func startSOCKS5(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var connections sync.Map
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(connection, true)
			go func() {
				defer connection.Close()
				defer connections.Delete(connection)
				_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
				header := make([]byte, 2)
				if _, err := io.ReadFull(connection, header); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, connection, int64(header[1])); err != nil {
					return
				}
				_, _ = connection.Write([]byte{5, 0})
				request := make([]byte, 4)
				if _, err := io.ReadFull(connection, request); err != nil {
					return
				}
				var host string
				switch request[3] {
				case 1:
					address := make([]byte, 4)
					_, _ = io.ReadFull(connection, address)
					host = net.IP(address).String()
				case 3:
					length := []byte{0}
					_, _ = io.ReadFull(connection, length)
					address := make([]byte, int(length[0]))
					_, _ = io.ReadFull(connection, address)
					host = string(address)
				case 4:
					address := make([]byte, 16)
					_, _ = io.ReadFull(connection, address)
					host = net.IP(address).String()
				default:
					return
				}
				port := make([]byte, 2)
				_, _ = io.ReadFull(connection, port)
				upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(port))), time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				calls.Add(1)
				_, _ = connection.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				go func() { _, _ = io.Copy(upstream, connection); _ = upstream.Close() }()
				_, _ = io.Copy(connection, upstream)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		connections.Range(func(key, _ any) bool { _ = key.(net.Conn).Close(); return true })
	})
	return "socks5://" + listener.Addr().String(), &calls
}
