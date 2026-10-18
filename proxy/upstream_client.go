package proxy

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/egress"
)

// accountClientPool is one IDE-shaped connection pool. Streaming and REST
// clients share the transport so one account reuses its own TLS session, and
// a different account never presents its bearer on that session.
type accountClientPool struct {
	transport *egress.Transport
	stream    *http.Client
	rest      *http.Client
}

var (
	kiroClientMu           sync.RWMutex
	defaultStreamingClient *http.Client
	defaultRESTClient      *http.Client
	accountClientPools     sync.Map
)

func InitKiroHttpClient(proxyURL string) {
	client := &http.Client{
		// http.Client.Timeout includes reading the response body, so a fixed
		// value here truncates otherwise healthy SSE streams mid-response.
		Timeout:   0,
		Transport: buildKiroTransport(proxyURL),
	}
	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}

	kiroClientMu.Lock()
	oldClient := kiroHttpStore.Swap(client)
	oldRestClient := kiroRestHttpStore.Swap(restClient)
	defaultStreamingClient = client
	defaultRESTClient = restClient
	var stale []*egress.Transport
	accountClientPools.Range(func(key, value any) bool {
		if pool, ok := value.(*accountClientPool); ok && pool.transport != nil {
			stale = append(stale, pool.transport)
		}
		accountClientPools.Delete(key)
		return true
	})
	kiroClientMu.Unlock()

	closeHTTPClientIdleConnections(oldClient)
	closeHTTPClientIdleConnections(oldRestClient)
	for _, transport := range stale {
		transport.CloseIdleConnections()
	}
}

func CloseKiroIdleConnections() {
	closeHTTPClientIdleConnections(kiroHttpStore.Load())
	closeHTTPClientIdleConnections(kiroRestHttpStore.Load())
	proxyClientCache.Range(func(_, value interface{}) bool {
		if client, ok := value.(*http.Client); ok {
			closeHTTPClientIdleConnections(client)
		}
		return true
	})
	accountClientPools.Range(func(_, value any) bool {
		if pool, ok := value.(*accountClientPool); ok {
			closeHTTPClientIdleConnections(pool.stream)
			closeHTTPClientIdleConnections(pool.rest)
		}
		return true
	})
}

// kiroUpstreamClient returns the HTTP client for one account. A test that
// replaces kiroHttpStore or kiroRestHttpStore still wins, so existing stubs
// keep intercepting the call.
func kiroUpstreamClient(account *config.Account, rest bool) *http.Client {
	kiroClientMu.RLock()
	var current *http.Client
	overridden := false
	if rest {
		current = kiroRestHttpStore.Load()
		overridden = current != defaultRESTClient
	} else {
		current = kiroHttpStore.Load()
		overridden = current != defaultStreamingClient
	}
	kiroClientMu.RUnlock()
	if overridden && !config.GetProxyPoolSettings().Enabled {
		return current
	}

	accountID := ""
	if account != nil {
		accountID = strings.TrimSpace(account.ID)
	}
	// This URL is only the old per-account/global fallback. Pool assignment is
	// resolved inside egress.Transport for every request, not at cache creation.
	proxyURL := config.GetProxyURL()
	if account != nil && strings.TrimSpace(account.ProxyURL) != "" {
		proxyURL = strings.TrimSpace(account.ProxyURL)
	}
	if accountID == "" && !config.GetProxyPoolSettings().Enabled {
		if rest {
			return GetRestClientForProxy(proxyURL)
		}
		return GetClientForProxy(proxyURL)
	}

	key := accountID + "\n" + proxyURL
	if value, ok := accountClientPools.Load(key); ok {
		return accountClient(value.(*accountClientPool), rest)
	}

	transport := egress.New(accountID, proxyURL, func(raw string) http.RoundTripper {
		base := buildKiroTransport(raw)
		// Each account retains an independent connection/TLS session pool.
		base.MaxIdleConns = 8
		base.MaxIdleConnsPerHost = 4
		return base
	})
	pool := &accountClientPool{
		transport: transport,
		stream: &http.Client{
			Timeout:   0,
			Transport: transport,
		},
		rest: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
	}
	actual, loaded := accountClientPools.LoadOrStore(key, pool)
	if loaded {
		transport.CloseIdleConnections()
		pool = actual.(*accountClientPool)
	}
	return accountClient(pool, rest)
}

func accountClient(pool *accountClientPool, rest bool) *http.Client {
	if rest {
		return pool.rest
	}
	return pool.stream
}
