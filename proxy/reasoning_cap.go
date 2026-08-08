package proxy

import (
	"kiro-go/config"
	"os"
	"strings"
)

// A looping agent turn produces reasoning frames forever without ever reaching
// a tool call or visible text, so neither the body-idle timer nor the
// generation timer can end it: both measure frame arrival, not progress. This
// file bounds the reasoning a single upstream response may emit *before* its
// first outward action.
//
// The cap is deliberately not a semantic loop detector. Distinguishing "stuck
// in a circle" from "working hard" needs a judgement no fingerprint can make
// reliably, and a false positive on legitimate deep reasoning is worse than a
// late stop. Counting bytes avoids the judgement entirely.

type reasoningCapMode uint8

const (
	// reasoningCapOff keeps the historical behaviour: no accounting at all.
	reasoningCapOff reasoningCapMode = iota
	// reasoningCapObserve accounts and logs the overshoot but never terminates
	// the stream. This is the data-collection mode used to find a defensible
	// limit before anything is enforced.
	reasoningCapObserve
	// reasoningCapEnforce ends the response once the limit is exceeded.
	reasoningCapEnforce
)

const (
	// Ratios of the model's usable input window. Effort is a behavioural dial
	// rather than a token allowance upstream, so these are calibration starting
	// points, not a documented effort-to-budget mapping.
	reasoningCapRatioLow    = 0.04
	reasoningCapRatioMedium = 0.08
	reasoningCapRatioHigh   = 0.15
	reasoningCapRatioXHigh  = 0.25

	// A pure ratio scales the wrong way on a 1M-token window: the resulting
	// limit is reachable in principle but takes so long that the account slot
	// stays occupied anyway. This absolute ceiling keeps the stop timely.
	reasoningCapMaxTokens = 52 << 10

	// Windows are reported in tokens while reasoning arrives as bytes. Reuse the
	// package's existing conservative ratio so both budgets agree.
	reasoningCapBytesPerToken = agentRecoveryBytesPerToken
)

// reasoningCapSettings is the resolved view of the admin configuration for one
// request. It is read per request rather than per process so an admin change
// takes effect on the next generation without a restart.
type reasoningCapSettings struct {
	Mode        reasoningCapMode
	MaxTokens   int
	RatioLow    float64
	RatioMedium float64
	RatioHigh   float64
	RatioXHigh  float64
}

func reasoningCapModeFromString(value string) (reasoningCapMode, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "observe", "log", "report":
		return reasoningCapObserve, true
	case "enforce", "on", "true", "1":
		return reasoningCapEnforce, true
	case "off", "false", "0", "disabled":
		return reasoningCapOff, true
	default:
		return reasoningCapOff, false
	}
}

// resolveReasoningCapSettings reads the admin config per request so a change in
// the admin UI takes effect on the next generation without a restart. The stored
// mode wins over KIRO_AGENT_REASONING_CAP_MODE; the env var only applies while
// the admin mode is unset, which keeps it useful for first boot and for
// deployments that never open the UI.
func resolveReasoningCapSettings() reasoningCapSettings {
	cfg := config.GetReasoningCapConfig()
	mode, ok := reasoningCapModeFromString(cfg.Mode)
	if !ok {
		mode, _ = reasoningCapModeFromString(os.Getenv("KIRO_AGENT_REASONING_CAP_MODE"))
	}
	return reasoningCapSettings{
		Mode:        mode,
		MaxTokens:   cfg.MaxTokens,
		RatioLow:    cfg.RatioLow,
		RatioMedium: cfg.RatioMedium,
		RatioHigh:   cfg.RatioHigh,
		RatioXHigh:  cfg.RatioXHigh,
	}
}

// ratioForEffort resolves the configured share of the window, falling back to
// the built-in ratio whenever the operator left that field at zero.
func (s reasoningCapSettings) ratioForEffort(effort string) float64 {
	configured, fallback := 0.0, reasoningCapRatioHigh
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		configured, fallback = s.RatioLow, reasoningCapRatioLow
	case "medium":
		configured, fallback = s.RatioMedium, reasoningCapRatioMedium
	case "xhigh", "max":
		configured, fallback = s.RatioXHigh, reasoningCapRatioXHigh
	default:
		// Includes "high" and every model without native reasoning fields.
		configured, fallback = s.RatioHigh, reasoningCapRatioHigh
	}
	if configured > 0 {
		return configured
	}
	return fallback
}

// reasoningCapModeFromEnvFallback returns the env-provided mode that applies only
// while the admin mode is unset. The admin UI surfaces it as the current
// effective value so an operator can see what the env var is contributing before
// overriding it, rather than as a lock on the selector.
func reasoningCapModeFromEnvFallback() string {
	if mode, ok := reasoningCapModeFromString(os.Getenv("KIRO_AGENT_REASONING_CAP_MODE")); ok {
		switch mode {
		case reasoningCapObserve:
			return config.ReasoningCapModeObserve
		case reasoningCapEnforce:
			return config.ReasoningCapModeEnforce
		default:
			return config.ReasoningCapModeOff
		}
	}
	return ""
}

// validateReasoningCapConfig rejects values the request path cannot honour. A
// ratio at or above 1 would place the limit beyond the context window, making it
// unreachable and silently disabling the cap.
func validateReasoningCapConfig(cfg config.ReasoningCapConfig) string {
	if _, ok := reasoningCapModeFromString(cfg.Mode); !ok && strings.TrimSpace(cfg.Mode) != "" {
		return "reasoningCap.mode must be one of: off, observe, enforce"
	}
	if cfg.MaxTokens < 0 {
		return "reasoningCap.maxTokens must not be negative"
	}
	for _, field := range []struct {
		name  string
		value float64
	}{
		{"ratioLow", cfg.RatioLow},
		{"ratioMedium", cfg.RatioMedium},
		{"ratioHigh", cfg.RatioHigh},
		{"ratioXHigh", cfg.RatioXHigh},
	} {
		if field.value < 0 || field.value >= 1 {
			return "reasoningCap." + field.name + " must be between 0 and 1 (0 uses the built-in default)"
		}
	}
	return ""
}

func (s reasoningCapSettings) maxTokens() int {
	if s.MaxTokens > 0 {
		return s.MaxTokens
	}
	return reasoningCapMaxTokens
}

func reasoningCapEffortForPayload(payload *KiroPayload) string {
	if payload == nil || payload.AdditionalModelRequestFields == nil ||
		payload.AdditionalModelRequestFields.OutputConfig == nil {
		return ""
	}
	return payload.AdditionalModelRequestFields.OutputConfig.Effort
}

// reasoningCapBytes converts the effort-scaled share of the usable window into
// a byte budget. A non-positive window means the model is unknown, so no cap is
// derived rather than guessing one.
func reasoningCapBytes(payload *KiroPayload, settings reasoningCapSettings) int {
	if payload == nil {
		return 0
	}
	window := getContextWindowSize(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)
	if window <= 0 {
		return 0
	}
	usable := window - agentRecoveryTokenReserve
	if usable < 1 {
		usable = window
	}
	tokens := int(float64(usable) * settings.ratioForEffort(reasoningCapEffortForPayload(payload)))
	if limit := settings.maxTokens(); tokens > limit {
		tokens = limit
	}
	if tokens < 1 {
		return 0
	}
	return tokens * reasoningCapBytesPerToken
}
