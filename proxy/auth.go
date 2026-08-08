package proxy

import (
	"context"
	"kiro-go/config"
	"net/http"
	"strings"
)

// apiKeyContextKey is an unexported type used as the context key for the matched ApiKeyEntry
// so it cannot collide with keys defined in other packages.
type apiKeyContextKey struct{}

// authError describes why authentication failed. status is the HTTP status code to send.
type authError struct {
	status  int
	code    string
	message string
}

func (e *authError) Error() string { return e.message }

func newAuthError(status int, code, message string) *authError {
	return &authError{status: status, code: code, message: message}
}

// extractProvidedKeys returns supported credentials in protocol-preferred order.
// Claude clients conventionally use X-Api-Key, while OpenAI clients use Bearer.
// Unknown credentials are kept in the list so they cannot mask a valid value in
// the other supported header.
func extractProvidedKeys(r *http.Request) []string {
	if r == nil {
		return nil
	}

	xAPIKeys := make([]string, 0, 1)
	for _, value := range r.Header.Values("X-Api-Key") {
		if value = strings.TrimSpace(value); value != "" {
			xAPIKeys = appendUniqueCredential(xAPIKeys, value)
		}
	}

	bearerKeys := make([]string, 0, 1)
	for _, value := range r.Header.Values("Authorization") {
		fields := strings.Fields(value)
		if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
			bearerKeys = appendUniqueCredential(bearerKeys, fields[1])
		}
	}

	keys := make([]string, 0, len(xAPIKeys)+len(bearerKeys))
	if isClaudeCredentialPath(r.URL.Path) {
		keys = append(keys, xAPIKeys...)
		keys = append(keys, bearerKeys...)
	} else {
		keys = append(keys, bearerKeys...)
		keys = append(keys, xAPIKeys...)
	}
	return keys
}

func appendUniqueCredential(keys []string, candidate string) []string {
	for _, key := range keys {
		if key == candidate {
			return keys
		}
	}
	return append(keys, candidate)
}

func isClaudeCredentialPath(path string) bool {
	switch path {
	case "/v1/messages", "/messages", "/anthropic/v1/messages",
		"/v1/messages/count_tokens", "/messages/count_tokens":
		return true
	default:
		return false
	}
}

// extractProvidedKey preserves the single-key helper used for anonymous cache
// isolation. Authentication itself checks every supported credential candidate.
func extractProvidedKey(r *http.Request) string {
	keys := extractProvidedKeys(r)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func firstConfiguredApiKey(r *http.Request) (*config.ApiKeyEntry, bool) {
	keys := extractProvidedKeys(r)
	for _, key := range keys {
		if entry := config.FindApiKeyByValue(key); entry != nil {
			return entry, true
		}
	}
	return nil, len(keys) > 0
}

func firstEnabledConfiguredApiKey(r *http.Request) *config.ApiKeyEntry {
	for _, key := range extractProvidedKeys(r) {
		if entry := config.FindApiKeyByValue(key); entry != nil && entry.Enabled {
			return entry
		}
	}
	return nil
}

// authenticate validates an incoming request against the configured API keys.
//
// Master switch: config.RequireApiKey. When false, requests always pass. A
// provided, enabled configured key is still recognized so downstream features
// such as prompt-cache policy binding and usage attribution can use its stable
// ID; missing, unknown, disabled, or over-limit keys are not rejected.
//
// When RequireApiKey is true:
//  1. If ApiKeys is non-empty, the provided key MUST match an enabled, in-quota
//     entry. Returns the matched entry (a copy) so callers can attribute usage.
//  2. Else if the legacy single ApiKey field is set, the provided key MUST match it.
//  3. Else (switch on but nothing configured) → fail-closed: every request is rejected.
//     This prevents the prior bug where toggling auth on without keys silently
//     left the service open.
//
// Returns (entry, nil) on success. entry is nil for anonymous public requests or
// when the legacy single-key path is used.
func (h *Handler) authenticate(r *http.Request) (*config.ApiKeyEntry, error) {
	if !config.IsApiKeyRequired() {
		if entry := firstEnabledConfiguredApiKey(r); entry != nil {
			return entry, nil
		}
		return nil, nil
	}

	entry, provided := firstConfiguredApiKey(r)

	if config.HasApiKeys() {
		if !provided {
			return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
		}
		if entry == nil {
			return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
		}
		if !entry.Enabled {
			return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "API key disabled")
		}
		if overToken, overCredit := config.ApiKeyOverLimit(*entry); overToken || overCredit {
			if overToken {
				return nil, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "token limit exceeded")
			}
			return nil, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "credit limit exceeded")
		}
		return entry, nil
	}

	// Legacy single-key path.
	expected := config.GetApiKey()
	if expected == "" {
		// Auth required but nothing configured → fail closed.
		return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "API key authentication is required but no keys are configured")
	}
	matched := false
	for _, candidate := range extractProvidedKeys(r) {
		if candidate == expected {
			matched = true
			break
		}
	}
	if !matched {
		return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
	}
	return nil, nil
}

// withApiKeyContext attaches the matched entry to the request context so downstream
// handlers (recordSuccess, etc.) can credit usage against the correct key.
func withApiKeyContext(r *http.Request, entry *config.ApiKeyEntry) *http.Request {
	if entry == nil {
		return r
	}
	ctx := context.WithValue(r.Context(), apiKeyContextKey{}, entry.ID)
	return r.WithContext(ctx)
}

// apiKeyIDFromContext returns the matched API key ID stored in ctx, or empty string.
func apiKeyIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(apiKeyContextKey{}).(string); ok {
		return v
	}
	return ""
}
