package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-go/config"
)

func TestProxyPoolRuntimeAllTiersAndCachedClients(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	InitKiroHttpClient("")
	t.Cleanup(func() {
		_, _ = config.UpdateProxyPoolSettings(config.ProxyPoolSettings{})
		InitKiroHttpClient("")
	})
	var directCalls, proxyCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { directCalls.Add(1); _, _ = io.WriteString(w, "direct") }))
	defer target.Close()
	forward := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxyCalls.Add(1)
		_, _ = io.Copy(w, req.Body)
	}))
	defer forward.Close()
	var accounts []config.Account
	for _, tier := range []string{"power", "max", "pro", "pro+", "free"} {
		account := config.Account{ID: "tier-" + tier, Enabled: true, AccountLevel: tier, SubscriptionType: strings.ToUpper(tier)}
		if err := config.AddAccount(account); err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, account)
	}
	// Cache the account's client while disabled, then turn the pool on. The
	// existing client must enforce the new policy without a process restart.
	cached := kiroUpstreamClient(&accounts[0], true)
	if _, _, err := config.ImportProxies([]config.ProxyEntry{{URL: forward.URL, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := config.UpdateProxyPoolSettings(config.ProxyPoolSettings{Enabled: true, BlockWithoutProxy: true}); err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		for _, rest := range []bool{false, true} {
			response, err := kiroUpstreamClient(&account, rest).Post(target.URL, "text/plain", strings.NewReader(account.AccountLevel))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(body) != account.AccountLevel {
				t.Fatalf("tier %s rest=%t body=%q err=%v", account.AccountLevel, rest, body, err)
			}
		}
	}
	if directCalls.Load() != 0 || proxyCalls.Load() != 10 {
		t.Fatalf("tier routing bypassed proxy: direct=%d proxy=%d", directCalls.Load(), proxyCalls.Load())
	}
	proxyEntry := config.GetProxies()[0]
	if err := config.DisableProxy(proxyEntry.ID, proxyEntry.URL, "test offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := cached.Get(target.URL); !errors.Is(err, config.ErrNoAvailableProxy) {
		t.Fatalf("cached client bypassed no-proxy block: %v", err)
	}
	if directCalls.Load() != 0 {
		t.Fatal("cached client leaked a direct request")
	}
}
