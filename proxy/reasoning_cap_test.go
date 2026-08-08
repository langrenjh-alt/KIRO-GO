package proxy

import (
	"kiro-go/config"
	"os"
	"path/filepath"
	"testing"
)

func TestReasoningCapModeFromEnv(t *testing.T) {
	cases := []struct {
		value string
		want  reasoningCapMode
	}{
		{"", reasoningCapOff},
		{"off", reasoningCapOff},
		{"garbage", reasoningCapOff},
		{"observe", reasoningCapObserve},
		{"log", reasoningCapObserve},
		{"enforce", reasoningCapEnforce},
		{"1", reasoningCapEnforce},
	}
	for _, tc := range cases {
		t.Setenv("KIRO_AGENT_REASONING_CAP_MODE", tc.value)
		if got := resolveReasoningCapSettings().Mode; got != tc.want {
			t.Fatalf("mode for %q = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// The default must stay off so deployments that do not opt in keep the exact
// behaviour they had before the cap existed.
func TestReasoningCapDefaultsToOff(t *testing.T) {
	os.Unsetenv("KIRO_AGENT_REASONING_CAP_MODE")
	if got := resolveReasoningCapSettings().Mode; got != reasoningCapOff {
		t.Fatalf("unset mode = %d, want %d", got, reasoningCapOff)
	}
}

// The admin UI writes the mode into config, so a config-only mode must take
// effect without the env var being set at all.
func TestReasoningCapModeFromConfig(t *testing.T) {
	config.Init(filepath.Join(t.TempDir(), "config.json"))
	os.Unsetenv("KIRO_AGENT_REASONING_CAP_MODE")
	if err := config.UpdateReasoningCapConfig(config.ReasoningCapConfig{
		Mode: config.ReasoningCapModeEnforce, MaxTokens: 4096, RatioMedium: 0.5,
	}); err != nil {
		t.Fatalf("update reasoning cap config: %v", err)
	}

	got := resolveReasoningCapSettings()
	if got.Mode != reasoningCapEnforce {
		t.Fatalf("config mode = %d, want %d", got.Mode, reasoningCapEnforce)
	}
	if got.MaxTokens != 4096 {
		t.Fatalf("config maxTokens = %d, want 4096", got.MaxTokens)
	}
	if got.RatioMedium != 0.5 {
		t.Fatalf("config ratioMedium = %v, want 0.5", got.RatioMedium)
	}
}

// The admin UI is authoritative: a stored mode must beat the env var, otherwise
// an operator changes the selector and nothing happens.
func TestReasoningCapConfigModeBeatsEnv(t *testing.T) {
	config.Init(filepath.Join(t.TempDir(), "config.json"))
	if err := config.UpdateReasoningCapConfig(config.ReasoningCapConfig{
		Mode: config.ReasoningCapModeEnforce, MaxTokens: 4096,
	}); err != nil {
		t.Fatalf("update reasoning cap config: %v", err)
	}

	t.Setenv("KIRO_AGENT_REASONING_CAP_MODE", "off")
	got := resolveReasoningCapSettings()
	if got.Mode != reasoningCapEnforce {
		t.Fatalf("mode = %d, want the configured %d", got.Mode, reasoningCapEnforce)
	}
	if got.MaxTokens != 4096 {
		t.Fatalf("maxTokens = %d, want the configured 4096", got.MaxTokens)
	}

	// "off" chosen in the UI is an explicit decision, not an absent value, so it
	// must also survive an env var asking to enforce.
	if err := config.UpdateReasoningCapConfig(config.ReasoningCapConfig{
		Mode: config.ReasoningCapModeOff,
	}); err != nil {
		t.Fatalf("update reasoning cap config: %v", err)
	}
	t.Setenv("KIRO_AGENT_REASONING_CAP_MODE", "enforce")
	if got := resolveReasoningCapSettings().Mode; got != reasoningCapOff {
		t.Fatalf("explicit off = %d, want %d", got, reasoningCapOff)
	}
}

// With no stored mode the env var still has to work, so first boot and
// deployments that never open the UI keep a way to turn the cap on.
func TestReasoningCapEnvAppliesWhenConfigModeUnset(t *testing.T) {
	config.Init(filepath.Join(t.TempDir(), "config.json"))
	if err := config.UpdateReasoningCapConfig(config.ReasoningCapConfig{MaxTokens: 4096}); err != nil {
		t.Fatalf("update reasoning cap config: %v", err)
	}

	t.Setenv("KIRO_AGENT_REASONING_CAP_MODE", "observe")
	if got := resolveReasoningCapSettings().Mode; got != reasoningCapObserve {
		t.Fatalf("env fallback mode = %d, want %d", got, reasoningCapObserve)
	}
	if got := reasoningCapModeFromEnvFallback(); got != config.ReasoningCapModeObserve {
		t.Fatalf("reported fallback = %q, want %q", got, config.ReasoningCapModeObserve)
	}

	// An unrecognized value contributes nothing and must not be reported to the
	// UI as the effective mode.
	t.Setenv("KIRO_AGENT_REASONING_CAP_MODE", "garbage")
	if got := resolveReasoningCapSettings().Mode; got != reasoningCapOff {
		t.Fatalf("unrecognized env mode = %d, want %d", got, reasoningCapOff)
	}
	if got := reasoningCapModeFromEnvFallback(); got != "" {
		t.Fatalf("reported fallback = %q, want empty", got)
	}
}

func TestReasoningCapRatioForEffort(t *testing.T) {
	var s reasoningCapSettings
	if s.ratioForEffort("low") >= s.ratioForEffort("medium") {
		t.Fatal("low effort must allow less reasoning than medium")
	}
	if s.ratioForEffort("medium") >= s.ratioForEffort("high") {
		t.Fatal("medium effort must allow less reasoning than high")
	}
	if s.ratioForEffort("xhigh") != s.ratioForEffort("max") {
		t.Fatal("xhigh and max share one ratio")
	}
	// An unset effort covers models without native reasoning fields.
	if s.ratioForEffort("") != s.ratioForEffort("high") {
		t.Fatal("unset effort must fall back to the high ratio")
	}
}

// A zero field means "use the built-in default", so a partially filled config
// must not silently shrink the untouched efforts to nothing.
func TestReasoningCapConfiguredRatiosOverrideDefaults(t *testing.T) {
	s := reasoningCapSettings{RatioMedium: 0.5}
	if got := s.ratioForEffort("medium"); got != 0.5 {
		t.Fatalf("configured medium ratio = %v, want 0.5", got)
	}
	if got := s.ratioForEffort("high"); got != reasoningCapRatioHigh {
		t.Fatalf("unset high ratio = %v, want the built-in %v", got, reasoningCapRatioHigh)
	}
	if got := (reasoningCapSettings{}).maxTokens(); got != reasoningCapMaxTokens {
		t.Fatalf("unset maxTokens = %d, want the built-in %d", got, reasoningCapMaxTokens)
	}
	if got := (reasoningCapSettings{MaxTokens: 4096}).maxTokens(); got != 4096 {
		t.Fatalf("configured maxTokens = %d, want 4096", got)
	}
}

func newReasoningCapPayload(model, effort string) *KiroPayload {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = model
	if effort != "" {
		payload.AdditionalModelRequestFields = &AdditionalModelRequestFields{
			OutputConfig: &KiroOutputConfig{Effort: effort},
		}
	}
	return payload
}

// A fixed byte table would exceed what a small-window model can produce, making
// the limit unreachable. Scaling by the window keeps it always attainable.
func TestReasoningCapNeverExceedsWindow(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-sonnet-4.5", "claude-haiku-4-5"} {
		window := getContextWindowSize(model)
		if window <= 0 {
			continue
		}
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			capBytes := reasoningCapBytes(newReasoningCapPayload(model, effort), reasoningCapSettings{})
			if capBytes <= 0 {
				t.Fatalf("%s/%s produced no cap", model, effort)
			}
			if tokens := capBytes / reasoningCapBytesPerToken; tokens >= window {
				t.Fatalf("%s/%s cap %d tokens reaches the %d token window", model, effort, tokens, window)
			}
		}
	}
}

// A 1M-token window would otherwise scale to a limit so large that a looping
// turn holds the account slot for hours before hitting it.
func TestReasoningCapHonoursAbsoluteCeiling(t *testing.T) {
	capBytes := reasoningCapBytes(newReasoningCapPayload("claude-opus-5", "max"), reasoningCapSettings{})
	if capBytes <= 0 {
		t.Skip("model token limits unavailable in this environment")
	}
	if tokens := capBytes / reasoningCapBytesPerToken; tokens > reasoningCapMaxTokens {
		t.Fatalf("cap %d tokens exceeds the %d token ceiling", tokens, reasoningCapMaxTokens)
	}
}

func TestReasoningCapNilPayloadYieldsNoCap(t *testing.T) {
	if got := reasoningCapBytes(nil, reasoningCapSettings{}); got != 0 {
		t.Fatalf("cap for a nil payload = %d, want 0", got)
	}
}

// lookupModelTokenLimits falls back to a positive window for any key, so an
// unrecognized model still receives a usable cap rather than an unbounded turn.
func TestReasoningCapUnknownModelUsesFallbackWindow(t *testing.T) {
	capBytes := reasoningCapBytes(newReasoningCapPayload("some-unreleased-model", "high"), reasoningCapSettings{})
	if capBytes <= 0 {
		t.Fatalf("unknown model produced no cap: %d", capBytes)
	}
	if tokens := capBytes / reasoningCapBytesPerToken; tokens > reasoningCapMaxTokens {
		t.Fatalf("cap %d tokens exceeds the %d token ceiling", tokens, reasoningCapMaxTokens)
	}
}

// A cap hit must not be classified as a marker mismatch: that path drives the
// narration-recovery chain and drops effort to "low".
func TestReasoningCapErrorIsTimeoutFamily(t *testing.T) {
	err := newUpstreamReasoningCapError(1<<20, 1<<10)
	if !isKiroTimeoutError(err) {
		t.Fatal("cap error must belong to the timeout family")
	}
	if isUpstreamAgentMarkerMismatchError(err) {
		t.Fatal("cap error must not be a marker mismatch")
	}
	if isUpstreamIncompleteResponseError(err) {
		t.Fatal("cap error must not enter the incomplete-response recovery chain")
	}
}
