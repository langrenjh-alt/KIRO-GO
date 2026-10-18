// Package auth 提供认证相关功能的 HTTP 客户端
package auth

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 全局 HTTP 客户端存储，支持运行时代理重配置
var httpClientStore atomic.Pointer[http.Client]

// defaultAuthClient is the client installed by InitHttpClient. Tests that
// replace the store leave this pointer unchanged so account-scoped clients
// can fall back to the stub.
var (
	authClientMu      sync.RWMutex
	defaultAuthClient *http.Client
)

// authProxyClientCache caches per-proxy auth HTTP clients for callers that
// do not yet have an account id.
var authProxyClientCache sync.Map

// authAccountClients keeps token refresh for each account on its own connection.
var authAccountClients sync.Map

// httpClient 返回当前全局 auth HTTP 客户端
func httpClient() *http.Client {
	return httpClientStore.Load()
}

func init() {
	InitHttpClient("")
}

// GetAuthClientForProxy returns an auth HTTP client for the given proxy URL.
// If proxyURL is empty, returns the global auth HTTP client.
func GetAuthClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return httpClient()
	}
	if cached, ok := authProxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := newAuthHTTPClient(proxyURL, "", false)
	authProxyClientCache.Store(proxyURL, client)
	return client
}

// ClientForAccount returns the auth client for one account. An empty account
// id uses the proxy-level client. A test-installed global client is returned
// for direct connections so httptest stubs keep working.
func ClientForAccount(accountID, proxyURL, machineID string) *http.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	authClientMu.RLock()
	replaced := httpClientStore.Load() != defaultAuthClient
	authClientMu.RUnlock()
	if replaced && proxyURL == "" {
		return httpClient()
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return GetAuthClientForProxy(proxyURL)
	}
	key := accountID + "\n" + proxyURL
	if value, ok := authAccountClients.Load(key); ok {
		return value.(*http.Client)
	}
	client := newAuthHTTPClient(proxyURL, machineID, true)
	actual, loaded := authAccountClients.LoadOrStore(key, client)
	if loaded {
		client.CloseIdleConnections()
		return actual.(*http.Client)
	}
	return client
}

// buildAuthTransport 构建带可选代理的 Transport
func buildAuthTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

func newAuthHTTPClient(proxyURL, machineID string, perAccount bool) *http.Client {
	base := buildAuthTransport(proxyURL)
	if perAccount {
		base.MaxIdleConns = 4
		base.MaxIdleConnsPerHost = 2
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: identityTransport{
			base:      base,
			machineID: strings.TrimSpace(machineID),
		},
	}
}

// InitHttpClient 初始化（或重新初始化）auth 模块的全局 HTTP 客户端
func InitHttpClient(proxyURL string) {
	client := newAuthHTTPClient(proxyURL, "", false)
	authClientMu.Lock()
	defaultAuthClient = client
	httpClientStore.Store(client)
	authClientMu.Unlock()
	authProxyClientCache.Range(func(key, value any) bool {
		if cached, ok := value.(*http.Client); ok {
			cached.CloseIdleConnections()
		}
		authProxyClientCache.Delete(key)
		return true
	})
	authAccountClients.Range(func(key, value any) bool {
		if cached, ok := value.(*http.Client); ok {
			cached.CloseIdleConnections()
		}
		authAccountClients.Delete(key)
		return true
	})
}
