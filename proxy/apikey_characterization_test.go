package proxy

import (
	"kiro-go/auth"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"testing"
)

// TestApiKeyAccountDoesNotRefreshOnHandlerPath pins the api_key invariant: an
// API-key account must never trigger OAuth refresh, including for an explicit
// admin refresh request.
func TestApiKeyAccountDoesNotRefreshOnHandlerPath(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Sentinel: if a refresh fires, the OIDC URL builder is consulted (to derive
	// the token endpoint), which would mean a round-trip was about to happen.
	prev := auth.GetOIDCTokenURLForTest()
	auth.SetOIDCTokenURLForTest(func(string) string {
		t.Fatal("refresh must not be called for an api_key account")
		return ""
	})
	defer auth.SetOIDCTokenURLForTest(prev)

	account := &config.Account{
		ID:           "ak-1",
		AuthMethod:   "api_key",
		KiroApiKey:   "k",
		AccessToken:  "k",
		ExpiresAt:    0,
		RefreshToken: "",
		Enabled:      true,
	}
	if !account.IsApiKeyCredential() {
		t.Fatalf("expected IsApiKeyCredential() == true")
	}
	h := &Handler{pool: accountpool.GetPool()}
	for _, force := range []bool{false, true} {
		if err := h.refreshAccountToken(account, force); err != nil {
			t.Fatalf("refreshAccountToken(force=%v) returned error: %v", force, err)
		}
	}
}
