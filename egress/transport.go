// Package egress enforces the proxy-pool policy at the last outbound boundary.
// A cached HTTP client never caches an account's assignment: every RoundTrip
// consults config again, including token refresh, REST and streaming requests.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"kiro-go/config"
)

type Transport struct {
	accountID string
	fallback  string
	factory   func(string) http.RoundTripper
	mu        sync.Mutex
	clients   map[string]http.RoundTripper
}

func New(accountID, fallbackURL string, factory func(string) http.RoundTripper) *Transport {
	return &Transport{accountID: strings.TrimSpace(accountID), fallback: strings.TrimSpace(fallbackURL), factory: factory, clients: make(map[string]http.RoundTripper)}
}

func (t *Transport) transport(proxyURL string, direct bool) http.RoundTripper {
	key := proxyURL
	if direct {
		key = "pool:direct"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing := t.clients[key]; existing != nil {
		return existing
	}
	base := t.factory(proxyURL)
	if proxyURL != "" {
		if transport, ok := base.(*http.Transport); ok {
			previous := transport.OnProxyConnectResponse
			transport.OnProxyConnectResponse = func(ctx context.Context, proxyURL *url.URL, request *http.Request, response *http.Response) error {
				if previous != nil {
					if err := previous(ctx, proxyURL, request, response); err != nil {
						return err
					}
				}
				if response.StatusCode != http.StatusOK {
					return proxyConnectError{status: response.StatusCode}
				}
				return nil
			}
		}
	}
	// An explicitly allowed pool fallback is genuinely direct, rather than an
	// old account/global proxy or an ambient HTTP_PROXY environment variable.
	if direct {
		if transport, ok := base.(*http.Transport); ok {
			transport.Proxy = nil
		}
	}
	t.clients[key] = base
	return base
}

func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, transport := range t.clients {
		if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("nil outbound request")
	}
	// Failover happens only before a response exists and only when the body can
	// be recreated. Never replay a streaming response or an arbitrary reader.
	for attempt := 0; attempt < 4; attempt++ {
		if err := req.Context().Err(); err != nil {
			if attempt == 0 && req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, err
		}
		// The shared switch and binding must be one snapshot. Reading them under
		// separate locks can mistake a concurrent switch-off for an unbound
		// account and accidentally choose direct fallback under an enabled policy.
		settings, entry, bound, policyErr := config.ResolveAccountProxyPolicy(t.accountID)
		if policyErr != nil {
			if attempt == 0 && req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, policyErr
		}
		proxyURL, proxyID := t.fallback, ""
		if settings.Enabled {
			proxyURL = ""
			if bound {
				proxyURL, proxyID = entry.URL, entry.ID
				// Do not let a malformed restored proxy silently become a direct
				// transport when a transport builder cannot parse its URL.
				u, parseErr := url.Parse(proxyURL)
				if parseErr != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
					if attempt == 0 && req.Body != nil {
						_ = req.Body.Close()
					}
					return nil, fmt.Errorf("invalid assigned proxy URL")
				}
			}
		}
		outbound := req.Clone(req.Context())
		if attempt > 0 && req.Body != nil && req.Body != http.NoBody {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("recreate outbound request body: %w", err)
			}
			outbound.Body = body
		}
		metrics := metricsFor(proxyID)
		finish := func() {}
		if metrics != nil {
			metrics.begin()
			var once sync.Once
			finish = func() { once.Do(metrics.end) }
			if outbound.Body != nil && outbound.Body != http.NoBody {
				outbound.Body = &requestBody{ReadCloser: outbound.Body, metrics: metrics}
			}
		}
		resp, err := t.transport(proxyURL, settings.Enabled && proxyID == "").RoundTrip(outbound)
		if err == nil {
			if resp == nil {
				finish()
				return nil, fmt.Errorf("outbound transport returned an empty response")
			}
			if resp.Body == nil {
				finish()
			} else if metrics != nil {
				resp.Body = &responseBody{ReadCloser: resp.Body, ctx: req.Context(), metrics: metrics, finish: finish, proxyID: proxyID, proxyURL: proxyURL}
			}
			return resp, nil
		}
		finish()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if proxyID == "" || !IsProxyNetworkError(req.Context(), err) {
			return nil, err
		}
		if disableErr := config.DisableProxy(proxyID, proxyURL, "outbound proxy network failure: "+err.Error()); disableErr != nil {
			return nil, fmt.Errorf("disable failed proxy: %v (outbound: %w)", disableErr, err)
		}
		if attempt == 3 || (req.Body != nil && req.Body != http.NoBody && req.GetBody == nil) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("proxy failover exhausted")
}

// IsProxyNetworkError excludes cancellation, upstream HTTP status responses,
// certificate failures and response-header timeouts. The caller only invokes
// this for a pool-bound request, so a connection reset/closed socket is the
// bound proxy's network path, not a reason to send the account directly.
func IsProxyNetworkError(ctx context.Context, err error) bool {
	if err == nil || (ctx != nil && ctx.Err() != nil) || errors.Is(err, context.Canceled) {
		return false
	}
	var connectErr proxyConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "tls:") || strings.Contains(lower, "x509:") || strings.Contains(lower, "timeout awaiting response headers") || strings.Contains(lower, "tls handshake timeout") {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) {
		if errors.Is(err, context.DeadlineExceeded) && op.Op != "dial" && op.Op != "proxyconnect" && !strings.Contains(op.Op, "socks") {
			return false
		}
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	for _, indicator := range []string{"proxyconnect", "socks connect", "socks5", "proxy authentication required", "connection refused", "connection reset", "network is unreachable", "no route to host", "broken pipe"} {
		if strings.Contains(lower, indicator) {
			return true
		}
	}
	return false
}

type proxyConnectError struct{ status int }

func (e proxyConnectError) Error() string {
	return fmt.Sprintf("proxy CONNECT failed with HTTP %d", e.status)
}

type requestBody struct {
	io.ReadCloser
	metrics *proxyMetrics
}

func (b *requestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.metrics.add(int64(n))
	return n, err
}

type responseBody struct {
	io.ReadCloser
	ctx      context.Context
	metrics  *proxyMetrics
	finish   func()
	proxyID  string
	proxyURL string
	failOnce sync.Once
}

func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.metrics.add(int64(n))
	if err != nil {
		b.finish()
		if err != io.EOF && IsProxyNetworkError(b.ctx, err) {
			b.failOnce.Do(func() {
				_ = config.DisableProxy(b.proxyID, b.proxyURL, "proxy stream network failure: "+err.Error())
			})
		}
	}
	return n, err
}

func (b *responseBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}
