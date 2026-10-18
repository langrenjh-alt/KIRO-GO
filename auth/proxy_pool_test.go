package auth

import (
	"context"
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

func TestProxyPoolAuthRefreshAllTiersAndCachedClient(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	InitHttpClient("")
	originalURL := socialTokenURL
	t.Cleanup(func() {
		socialTokenURL = originalURL
		_, _ = config.UpdateProxyPoolSettings(config.ProxyPoolSettings{})
		InitHttpClient("")
	})
	var directCalls, proxyCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { directCalls.Add(1); w.WriteHeader(500) }))
	defer target.Close()
	socialTokenURL = func() string { return target.URL + "/refreshToken" }
	forward := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxyCalls.Add(1)
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), "refresh-secret") {
			t.Errorf("refresh body lost during proxy routing: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"accessToken":"pool-access","refreshToken":"rotated","expiresIn":3600}`)
	}))
	defer forward.Close()
	var accounts []config.Account
	for _, tier := range []string{"power", "max", "pro", "pro+", "free"} {
		account := config.Account{ID: "auth-" + tier, AuthMethod: "social", RefreshToken: "refresh-secret", Enabled: true, AccountLevel: tier}
		if err := config.AddAccount(account); err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, account)
	}
	cached := ClientForAccount(accounts[0].ID, "", "")
	if _, _, err := config.ImportProxies([]config.ProxyEntry{{URL: forward.URL, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := config.UpdateProxyPoolSettings(config.ProxyPoolSettings{Enabled: true, BlockWithoutProxy: true}); err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		access, refresh, _, _, err := RefreshTokenContext(context.Background(), &account)
		if err != nil || access != "pool-access" || refresh != "rotated" {
			t.Fatalf("tier=%s access=%s refresh=%s err=%v", account.AccountLevel, access, refresh, err)
		}
	}
	if directCalls.Load() != 0 || proxyCalls.Load() != 5 {
		t.Fatalf("auth bypassed proxy: direct=%d proxy=%d", directCalls.Load(), proxyCalls.Load())
	}
	entry := config.GetProxies()[0]
	if err := config.DisableProxy(entry.ID, entry.URL, "test offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := cached.Get(target.URL); !errors.Is(err, config.ErrNoAvailableProxy) {
		t.Fatalf("cached auth client bypassed no-proxy block: %v", err)
	}
	if _, _, err := GetUserInfoForAccount(&accounts[0]); !errors.Is(err, config.ErrNoAvailableProxy) {
		t.Fatalf("post-import identity bypassed no-proxy block: %v", err)
	}
	if directCalls.Load() != 0 {
		t.Fatal("blocked auth request reached target directly")
	}
}
