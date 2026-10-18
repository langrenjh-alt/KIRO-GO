package auth

import (
	"fmt"
	"net/http"
	"strings"

	"kiro-go/config"
)

// applyKiroRuntimeIdentity sets the runtime-plane IDE identity used for
// CodeWhisperer control-plane calls such as getUsageLimits.
func applyKiroRuntimeIdentity(req *http.Request, machineID string) {
	if req == nil {
		return
	}
	userAgent, amzUserAgent := config.KiroIdentityHeaders(
		machineID,
		config.KiroRuntimeAPIName,
		config.KiroRuntimeSDKVersion,
		config.KiroRuntimeSDKMode,
	)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("x-amz-user-agent", amzUserAgent)
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
}

// identityTransport fills a missing or self-identifying User-Agent on calls
// to Kiro and AWS hosts. Third-party IdP token endpoints are left alone.
type identityTransport struct {
	base      http.RoundTripper
	machineID string
}

func (t identityTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t identityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	host := ""
	if req.URL != nil {
		host = req.URL.Hostname()
	}
	if !kiroIdentityHost(host) || (!needsKiroIdentity(req.Header.Get("User-Agent")) && !needsKiroIdentity(req.Header.Get("X-Amz-User-Agent"))) {
		return base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	userAgent, amzUserAgent := config.KiroIdentityHeaders(
		t.machineID,
		config.KiroRuntimeAPIName,
		config.KiroRuntimeSDKVersion,
		config.KiroRuntimeSDKMode,
	)
	if needsKiroIdentity(clone.Header.Get("User-Agent")) {
		clone.Header.Set("User-Agent", userAgent)
	}
	if needsKiroIdentity(clone.Header.Get("X-Amz-User-Agent")) {
		clone.Header.Set("x-amz-user-agent", amzUserAgent)
	}
	return base.RoundTrip(clone)
}

func needsKiroIdentity(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	lower := strings.ToLower(value)
	return strings.Contains(lower, "go-http-client") || strings.Contains(lower, "kiroapiproxy")
}

func kiroIdentityHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	for _, suffix := range []string{"amazonaws.com", "amazonaws.com.cn", "kiro.dev", "awsapps.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}
