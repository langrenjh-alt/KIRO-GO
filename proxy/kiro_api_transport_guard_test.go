package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-go/config"
)

// These tests isolate RefreshAccountInfo's account-state classification from
// egress's independent proxy quarantine/failover. The private config retains a
// real binding, while the account's REST client is supplied explicitly. No
// production account, proxy, credential or external endpoint is contacted.
func refreshTransportFixture(t *testing.T, apiKey bool) (config.Account, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.Close() })
	account := config.Account{
		ID: "refresh-transport-account", Enabled: true, BanStatus: "ACTIVE",
		BanReason: "existing diagnostic note", BanTime: 42,
		AccessToken: "fixture-not-a-real-credential", AuthMethod: "social", Region: "us-east-1",
		ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/refresh-transport-fixture",
	}
	if apiKey {
		account.AuthMethod = "api_key"
		account.KiroApiKey = "fixture-not-a-real-api-key"
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.ImportProxies([]config.ProxyEntry{{Name: "binding-only fixture", URL: "http://127.0.0.1:9", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := config.UpdateProxyPoolSettings(config.ProxyPoolSettings{Enabled: true, BlockWithoutProxy: true}); err != nil {
		t.Fatal(err)
	}
	binding := config.GetProxyBindings()[account.ID]
	if binding == "" {
		t.Fatal("fixture account was not bound")
	}
	return config.GetAccounts()[0], binding, path
}

func installRefreshTransportClient(t *testing.T, account config.Account, transport http.RoundTripper) {
	t.Helper()
	key := account.ID + "\n" + account.ProxyURL
	previous, existed := accountClientPools.Load(key)
	client := &http.Client{Transport: transport}
	accountClientPools.Store(key, &accountClientPool{rest: client})
	t.Cleanup(func() {
		client.CloseIdleConnections()
		if existed {
			accountClientPools.Store(key, previous)
		} else {
			accountClientPools.Delete(key)
		}
	})
}

type refreshConnectStatusError struct{ status int }

func (e refreshConnectStatusError) Error() string {
	return fmt.Sprintf("proxy CONNECT failed with HTTP %d", e.status)
}

func TestRefreshAccountInfoTransportFailurePreservesAccountAndBinding(t *testing.T) {
	for _, name := range []string{"connect_403", "connect_401", "socks_authentication", "dns_not_found", "dns_timeout"} {
		t.Run(name, func(t *testing.T) {
			account, binding, configPath := refreshTransportFixture(t, false)
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			var connectCalls atomic.Int32
			if strings.HasPrefix(name, "connect_") {
				status := http.StatusForbidden
				if name == "connect_401" {
					status = http.StatusUnauthorized
				}
				// Real loopback CONNECT handshake; this server never forwards or
				// dials the requested AWS authority, so no external call occurs.
				proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					connectCalls.Add(1)
					if req.Method != http.MethodConnect {
						t.Errorf("method=%s, want CONNECT", req.Method)
					}
					w.WriteHeader(status)
				}))
				t.Cleanup(proxyServer.Close)
				proxyURL, err := url.Parse(proxyServer.URL)
				if err != nil {
					t.Fatal(err)
				}
				transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), OnProxyConnectResponse: func(_ context.Context, _ *url.URL, _ *http.Request, response *http.Response) error {
					return refreshConnectStatusError{status: response.StatusCode}
				}}
				installRefreshTransportClient(t, account, transport)
			} else {
				var cause error
				switch name {
				case "socks_authentication":
					cause = &net.OpError{Op: "socks connect", Net: "tcp", Err: errors.New("SOCKS5 username/password authentication failed")}
				case "dns_not_found":
					cause = &net.DNSError{Name: "diagnostic.invalid", Err: "no such host", IsNotFound: true}
				case "dns_timeout":
					cause = &net.DNSError{Name: "diagnostic.invalid", Err: "i/o timeout", IsTimeout: true}
				}
				installRefreshTransportClient(t, account, roundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, cause
				}))
			}
			requestAccount := account
			_, refreshErr := RefreshAccountInfo(&requestAccount)
			if refreshErr == nil {
				t.Fatal("transport failure must still propagate")
			}
			if !isKiroTransportError(refreshErr) {
				t.Fatalf("expected typed transport error, got %T: %v", refreshErr, refreshErr)
			}
			if strings.HasPrefix(name, "connect_") && connectCalls.Load() != 1 {
				t.Fatalf("CONNECT calls=%d, want one local handshake", connectCalls.Load())
			}
			accounts := config.GetAccounts()
			if len(accounts) != 1 || !reflect.DeepEqual(accounts[0], account) {
				t.Errorf("network failure changed account: got=%+v want=%+v", accounts, account)
			}
			if got := config.GetProxyBindings()[account.ID]; got != binding {
				t.Errorf("network failure changed binding: got=%q want=%q", got, binding)
			}
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Error("network classification wrote the private configuration")
			}
		})
	}
}

func TestRefreshAccountInfoRealHTTPAuthAndSuspensionStillDisableAndUnbind(t *testing.T) {
	tests := []struct {
		name, body, reason string
		status             int
		apiKey             bool
	}{
		{name: "aws_401", status: http.StatusUnauthorized, body: `{"message":"The bearer token included in the request is invalid."}`, reason: "Authentication failed - token invalid or expired"},
		{name: "aws_403", status: http.StatusForbidden, body: `{"message":"The bearer token included in the request is invalid."}`, reason: "Authentication failed - token invalid or expired"},
		{name: "aws_suspension_oauth", status: http.StatusForbidden, body: `{"message":"Your User ID temporarily is suspended due to unusual activity. Please contact support.","reason":null}`, reason: "AWS temporarily suspended - unusual user activity detected"},
		{name: "aws_suspension_api_key", status: http.StatusForbidden, apiKey: true, body: `{"message":"Your User ID temporarily is suspended due to unusual activity. Please contact support.","reason":null}`, reason: "AWS temporarily suspended - unusual user activity detected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			account, _, _ := refreshTransportFixture(t, tc.apiKey)
			installRefreshTransportClient(t, account, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/getUsageLimits" {
					t.Errorf("unexpected path %q", req.URL.Path)
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			requestAccount := account
			_, err := RefreshAccountInfo(&requestAccount)
			if err == nil || isKiroTransportError(err) {
				t.Fatalf("business HTTP error misclassified as transport: %v", err)
			}
			accounts := config.GetAccounts()
			if len(accounts) != 1 || accounts[0].Enabled || accounts[0].BanStatus != "BANNED" || accounts[0].BanReason != tc.reason || accounts[0].BanTime <= account.BanTime {
				t.Fatalf("genuine upstream error no longer disables: %+v", accounts)
			}
			if binding := config.GetProxyBindings()[account.ID]; binding != "" {
				t.Fatalf("disabled account retained proxy binding %q", binding)
			}
		})
	}
}
