package proxy

import (
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"kiro-go/config"
)

const (
	kiroStreamingSDKVersion = config.KiroStreamingSDKVersion
	kiroRuntimeSDKVersion   = config.KiroRuntimeSDKVersion
)

type kiroHeaderValues struct {
	UserAgent    string
	AmzUserAgent string
	Host         string
}

func buildStreamingHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererstreaming", kiroStreamingSDKVersion, "m/E")
}

func buildRuntimeHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererruntime", kiroRuntimeSDKVersion, "m/N,E")
}

func buildKiroHeaderValues(account *config.Account, host, apiName, sdkVersion, mode string) kiroHeaderValues {
	machineID := ""
	if account != nil {
		machineID = account.MachineId
	}

	userAgent, amzUserAgent := config.KiroIdentityHeaders(machineID, apiName, sdkVersion, mode)

	return kiroHeaderValues{
		UserAgent:    userAgent,
		AmzUserAgent: amzUserAgent,
		Host:         host,
	}
}

func applyKiroBaseHeaders(req *http.Request, account *config.Account, values kiroHeaderValues) {
	if account != nil {
		if account.IsApiKeyCredential() {
			if account.KiroApiKey != "" {
				req.Header.Set("Authorization", "Bearer "+account.KiroApiKey)
				req.Header.Set("tokentype", "API_KEY")
			}
		} else if account.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+account.AccessToken)
		}
	}
	req.Header.Set("User-Agent", values.UserAgent)
	req.Header.Set("x-amz-user-agent", values.AmzUserAgent)
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	// External IdP (enterprise SSO, e.g. Azure AD) tokens MUST carry this header or
	// CodeWhisperer does not recognize the token type and silently returns an empty
	// profile list (and rejects data-plane calls). With it, a provisioned account
	// resolves its profile; an unprovisioned one gets a clear 403.
	if account != nil && account.AuthMethod == "external_idp" {
		req.Header.Set("TokenType", "EXTERNAL_IDP")
	}
	if values.Host != "" {
		req.Host = values.Host
	}
}

// applyKiroSDKCallHeaders mirrors the AWS SDK for JavaScript attempt counter.
// attempt is capped at max=3 because that is the counter the IDE SDK advertises.
func applyKiroSDKCallHeaders(req *http.Request, attempt int) {
	if req == nil {
		return
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 3 {
		attempt = 3
	}
	req.Header.Set("Amz-Sdk-Request", fmt.Sprintf("attempt=%d; max=3", attempt))
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.NewString())
}
