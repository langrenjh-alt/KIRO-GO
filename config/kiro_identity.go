package config

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

const (
	// These match the two AWS SDK for JavaScript surfaces the Kiro IDE uses:
	// streaming generation versus the CodeWhisperer runtime control plane.
	KiroStreamingSDKVersion = "1.0.34"
	KiroRuntimeSDKVersion   = "1.0.0"
	KiroStreamingAPIName    = "codewhispererstreaming"
	KiroRuntimeAPIName      = "codewhispererruntime"
	KiroStreamingSDKMode    = "m/E"
	KiroRuntimeSDKMode      = "m/N,E"
)

// kiroClientOSChoices are operating-system tokens a real IDE install reports.
// They are used only when the operator has not pinned systemVersion, so a
// pool does not advertise the proxy host's OS on every account.
var kiroClientOSChoices = []string{
	"win32#10.0.19045",
	"win32#10.0.22631",
	"win32#10.0.26100",
	"darwin#23.6.0",
	"darwin#24.6.0",
	"linux#6.6.87",
	"linux#6.8.0",
}

// SystemVersionForMachine returns the configured OS token, or a stable
// per-machine choice when systemVersion was left empty.
func SystemVersionForMachine(machineID string) string {
	if KiroSystemVersionExplicit() {
		return GetKiroClientConfig().SystemVersion
	}
	machineID = strings.TrimSpace(machineID)
	if machineID == "" || len(kiroClientOSChoices) == 0 {
		return GetKiroClientConfig().SystemVersion
	}
	sum := sha256.Sum256([]byte("kiro-os:" + machineID))
	return kiroClientOSChoices[int(sum[0])%len(kiroClientOSChoices)]
}

// KiroSystemVersionExplicit reports whether config.json pinned systemVersion.
func KiroSystemVersionExplicit() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg != nil && strings.TrimSpace(cfg.SystemVersion) != ""
}

// KiroIdentityHeaders builds the User-Agent pair the IDE sends on AWS SDK calls.
// machineID is appended only when the account has one, matching an IDE install
// that has already persisted its machine id.
func KiroIdentityHeaders(machineID, apiName, sdkVersion, mode string) (userAgent, amzUserAgent string) {
	clientCfg := GetKiroClientConfig()
	userAgent = fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/%s#%s %s KiroIDE-%s",
		sdkVersion,
		SystemVersionForMachine(machineID),
		clientCfg.NodeVersion,
		apiName,
		sdkVersion,
		mode,
		clientCfg.KiroVersion,
	)
	amzUserAgent = fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s", sdkVersion, clientCfg.KiroVersion)
	if machineID != "" {
		userAgent += "-" + machineID
		amzUserAgent += "-" + machineID
	}
	return userAgent, amzUserAgent
}
