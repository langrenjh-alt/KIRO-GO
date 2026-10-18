package proxy

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
)

// accountClientPool is one IDE-shaped connection pool. Streaming and REST
// clients share the transport so one account reuses its own TLS session, and
// a different account never presents its bearer on that session.
type accountClientPool struct {
	transport *http.Transport
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
	var stale []*http.Transport
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
	if overridden {
		return current
	}

	accountID := ""
	if account != nil {
		accountID = strings.TrimSpace(account.ID)
	}
	proxyURL := ResolveAccountProxyURL(account)
	if accountID == "" {
		if rest {
			return GetRestClientForProxy(proxyURL)
		}
		return GetClientForProxy(proxyURL)
	}

	key := accountID + "\n" + proxyURL
	if value, ok := accountClientPools.Load(key); ok {
		return accountClient(value.(*accountClientPool), rest)
	}

	transport := buildKiroTransport(proxyURL)
	// The shared process pool is sized for every account at once. One IDE
	// process only keeps a handful of idle connections.
	transport.MaxIdleConns = 8
	transport.MaxIdleConnsPerHost = 4
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
