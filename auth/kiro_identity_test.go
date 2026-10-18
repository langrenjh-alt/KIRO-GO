package auth

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type identityRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn identityRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestApplyKiroRuntimeIdentityOmitsProxyName(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://q.us-east-1.amazonaws.com/getUsageLimits", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyKiroRuntimeIdentity(req, "machine-123")

	userAgent := req.Header.Get("User-Agent")
	amzUserAgent := req.Header.Get("x-amz-user-agent")
	if strings.Contains(userAgent, "KiroAPIProxy") || strings.Contains(userAgent, "Go-http-client") {
		t.Fatalf("user agent identifies the proxy: %s", userAgent)
	}
	if !strings.Contains(userAgent, "aws-sdk-js/1.0.0") || !strings.Contains(userAgent, "api/codewhispererruntime#1.0.0") || !strings.Contains(userAgent, "md/nodejs#") || !strings.Contains(userAgent, "KiroIDE-") || !strings.Contains(userAgent, "machine-123") {
		t.Fatalf("runtime user agent = %s", userAgent)
	}
	if !strings.Contains(amzUserAgent, "aws-sdk-js/1.0.0 KiroIDE-") || !strings.Contains(amzUserAgent, "machine-123") {
		t.Fatalf("x-amz-user-agent = %s", amzUserAgent)
	}
	if req.Header.Get("x-amzn-codewhisperer-optout") != "true" {
		t.Fatalf("optout = %q", req.Header.Get("x-amzn-codewhisperer-optout"))
	}
}

func TestIdentityTransportRewritesOnlyKiroHosts(t *testing.T) {
	var seen []string
	transport := identityTransport{
		machineID: "machine-9",
		base: identityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			seen = append(seen, req.URL.Host+" "+req.Header.Get("User-Agent"))
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		}),
	}

	kiroReq, err := http.NewRequest(http.MethodPost, "https://oidc.us-east-1.amazonaws.com/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	kiroReq.Header.Set("User-Agent", "Go-http-client/2.0")
	if _, err := transport.RoundTrip(kiroReq); err != nil {
		t.Fatal(err)
	}

	idpReq, err := http.NewRequest(http.MethodPost, "https://login.microsoftonline.com/tenant/oauth2/v2.0/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(idpReq); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 2 {
		t.Fatalf("seen = %#v", seen)
	}
	if !strings.Contains(seen[0], "oidc.us-east-1.amazonaws.com") || !strings.Contains(seen[0], "KiroIDE-") || strings.Contains(seen[0], "Go-http-client") || strings.Contains(seen[0], "KiroAPIProxy") {
		t.Fatalf("AWS call kept a non-IDE user agent: %s", seen[0])
	}
	if !strings.Contains(seen[1], "login.microsoftonline.com") || strings.Contains(seen[1], "KiroIDE-") {
		t.Fatalf("external IdP received a Kiro user agent: %s", seen[1])
	}
	if kiroReq.Header.Get("User-Agent") != "Go-http-client/2.0" {
		t.Fatalf("identity transport mutated the caller request: %s", kiroReq.Header.Get("User-Agent"))
	}
}

func TestAuthClientsAreScopedToAccountsUntilTestOverride(t *testing.T) {
	InitHttpClient("")
	t.Cleanup(func() { InitHttpClient("") })

	first := ClientForAccount("account-a", "", "machine-a")
	second := ClientForAccount("account-b", "", "machine-b")
	if first == nil || second == nil || first == second || first.Transport == second.Transport {
		t.Fatal("auth refresh shared one connection pool between accounts")
	}
	if again := ClientForAccount("account-a", "", "machine-a"); again != first {
		t.Fatal("the same account allocated a second auth client")
	}

	stub := &http.Client{}
	previous := SetGlobalAuthClientForTest(stub)
	t.Cleanup(func() { SetGlobalAuthClientForTest(previous) })
	if got := ClientForAccount("account-a", "", "machine-a"); got != stub {
		t.Fatal("a test-installed auth client was ignored")
	}
}
