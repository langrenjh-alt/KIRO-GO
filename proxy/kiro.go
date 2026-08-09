// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	maxKiroEventStreamFrameBytes       = 16 << 20
	maxKiroToolInputBytes              = 8 << 20
	maxKiroToolAggregateBytes          = 16 << 20
	maxKiroActiveToolUses              = 128
	maxKiroToolJSONRetries             = 2
	maxKiroTransportPreResponseRetries = 1
	maxKiroAgentCompletionRetries      = 2
	maxKiroAgentActionContinuations    = 2
	maxKiroAgentFullContextRecoveries  = 1
	maxKiroAgentInputTooLongRecoveries = 1
	maxKiroOutputContinuations         = 2
	maxKiroOutputContinuationBytes     = 256 << 10
	kiroInitialAgentThinkingTimeout    = 0
	kiroStreamKeepAliveInterval        = 25 * time.Second
	kiroDialTimeout                    = 30 * time.Second
	kiroTLSHandshakeTimeout            = 10 * time.Second
)

// Kiro streaming endpoint. Hosts containing us-east-1 are rewritten to the
// account's effective API region immediately before each request.
type kiroEndpoint struct {
	URL    string
	Origin string
	Name   string
}

var kiroEndpoints = []kiroEndpoint{
	{
		URL:    "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin: "AI_EDITOR",
		Name:   "Kiro IDE",
	},
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache sync.Map

func init() {
	InitKiroHttpClient("")
}

// GetClientForProxy returns an http.Client configured for the given proxy URL.
// If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		// Do not put a wall-clock deadline on a streaming response. Long agent
		// turns can legitimately run for more than five minutes; the request
		// context still cancels the upstream call when the downstream disconnects.
		Timeout:   0,
		Transport: buildKiroTransport(proxyURL),
	}
	actual, loaded := proxyClientCache.LoadOrStore(proxyURL, client)
	if loaded {
		client.CloseIdleConnections()
	}
	return actual.(*http.Client)
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroRestHttpStore.Load()
	}
	cacheKey := "rest:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	actual, loaded := proxyClientCache.LoadOrStore(cacheKey, client)
	if loaded {
		client.CloseIdleConnections()
	}
	return actual.(*http.Client)
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil && account.ProxyURL != "" {
		return account.ProxyURL
	}
	return config.GetProxyURL()
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy support.
func buildKiroTransport(proxyURL string) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   kiroDialTimeout,
		KeepAlive: 30 * time.Second,
	}
	proxied := strings.TrimSpace(proxyURL) != "" ||
		firstNonEmptyEnv("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy") != ""
	maxIdlePerHost := positiveEnvInt("KIRO_DIRECT_MAX_IDLE_CONNS_PER_HOST", 32)
	if proxied {
		maxIdlePerHost = positiveEnvInt("KIRO_PROXY_MAX_IDLE_CONNS_PER_HOST", 64)
	}
	responseHeaderTimeout := kiroTimeoutPolicyFromEnv().responseHeaderTimeout
	t := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          positiveEnvInt("KIRO_MAX_IDLE_CONNS", 256),
		MaxIdleConnsPerHost:   maxIdlePerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   kiroTLSHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     !proxied,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			// Proxied connections cannot negotiate HTTP/2.
			t.ForceAttemptHTTP2 = false
		}
	} else {
		// ProxyFromEnvironment caches the first process-wide environment it
		// observes. Capture the effective proxy when the transport is built so
		// runtime proxy reconfiguration and tests using t.Setenv are reliable.
		t.Proxy = proxyFromCurrentEnvironment()
	}
	return t
}

func proxyFromCurrentEnvironment() func(*http.Request) (*url.URL, error) {
	httpsProxy := firstNonEmptyEnv("HTTPS_PROXY", "https_proxy")
	httpProxy := firstNonEmptyEnv("HTTP_PROXY", "http_proxy")
	noProxy := firstNonEmptyEnv("NO_PROXY", "no_proxy")
	return func(req *http.Request) (*url.URL, error) {
		if req == nil || req.URL == nil || proxyBypassed(req.URL.Hostname(), noProxy) {
			return nil, nil
		}
		raw := httpProxy
		if strings.EqualFold(req.URL.Scheme, "https") {
			raw = httpsProxy
		}
		if strings.TrimSpace(raw) == "" {
			return nil, nil
		}
		return url.Parse(raw)
	}
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func proxyBypassed(host, noProxy string) bool {
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	for _, raw := range strings.Split(noProxy, ",") {
		entry := strings.ToLower(strings.TrimSpace(raw))
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		if splitHost, _, err := net.SplitHostPort(entry); err == nil {
			entry = splitHost
		}
		entry = strings.TrimPrefix(entry, ".")
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
func InitKiroHttpClient(proxyURL string) {
	client := &http.Client{
		// http.Client.Timeout includes reading the response body, so a fixed
		// value here truncates otherwise healthy SSE streams mid-response.
		Timeout:   0,
		Transport: buildKiroTransport(proxyURL),
	}
	oldClient := kiroHttpStore.Swap(client)

	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	oldRestClient := kiroRestHttpStore.Swap(restClient)
	closeHTTPClientIdleConnections(oldClient)
	closeHTTPClientIdleConnections(oldRestClient)
}

// CloseKiroIdleConnections closes only idle upstream connections. Active
// requests continue; subsequent calls establish fresh direct/proxy tunnels.
func CloseKiroIdleConnections() {
	closeHTTPClientIdleConnections(kiroHttpStore.Load())
	closeHTTPClientIdleConnections(kiroRestHttpStore.Load())
	proxyClientCache.Range(func(_, value interface{}) bool {
		if client, ok := value.(*http.Client); ok {
			closeHTTPClientIdleConnections(client)
		}
		return true
	})
}

func closeHTTPClientIdleConnections(client *http.Client) {
	if client == nil || client.Transport == nil {
		return
	}
	if transport, ok := client.Transport.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

// ==================== Request Structs ====================

type kiroHistoryMode string

const (
	kiroHistoryModeLegacy       kiroHistoryMode = "legacy"
	kiroHistoryModeNativeActive kiroHistoryMode = "native_active"
)

func configuredKiroHistoryMode() kiroHistoryMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KIRO_NATIVE_STRUCTURED_HISTORY"))) {
	case "0", "false", "no", "off", "disable", "disabled", string(kiroHistoryModeLegacy):
		return kiroHistoryModeLegacy
	default:
		return kiroHistoryModeNativeActive
	}
}

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn                   string                        `json:"profileArn,omitempty"`
	InferenceConfig              *InferenceConfig              `json:"inferenceConfig,omitempty"`
	AdditionalModelRequestFields *AdditionalModelRequestFields `json:"additionalModelRequestFields,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`

	// StopSequences are enforced by the protocol adapter because Kiro's wire
	// inferenceConfig has no corresponding field.
	StopSequences []string `json:"-"`

	// StructuredOutput marks requests whose downstream contract requires the
	// assistant text to be a single JSON value. Kiro receives the instruction
	// through the priming prompt; this internal marker lets the response
	// adapter normalize the final text without changing the upstream payload.
	StructuredOutput bool `json:"-"`
	// StructuredOutputType and StructuredOutputSchema retain the downstream
	// contract for non-stream validation. They never leave the proxy.
	StructuredOutputType   string          `json:"-"`
	StructuredOutputSchema json.RawMessage `json:"-"`

	// ToolChoiceMode preserves the downstream choice semantics for response
	// normalization. In particular, Anthropic's any/required mode expects one
	// selected call even when Kiro proposes more than one.
	ToolChoiceMode string `json:"-"`
	// ToolChoiceName preserves the original client-facing name for a specific
	// tool choice. Kiro receives a sanitized name, but terminal validation runs
	// after callbacks restore the original name through ToolNameMap.
	ToolChoiceName string `json:"-"`

	// HistoryMode is bound once while translating the downstream request. It is
	// not re-read during retries, so one logical request cannot switch protocol
	// shapes because the process environment changed mid-flight.
	HistoryMode kiroHistoryMode `json:"-"`
	// ReasoningSignatureRetry records the one same-account retry allowed after
	// Kiro rejects a replayed native reasoning signature.
	ReasoningSignatureRetry bool `json:"-"`

	// AgentCompletionMarker is the optional private final-answer prefix for a
	// recognized tool-enabled coding harness. It is stripped from final output
	// and never serialized as a top-level Kiro request field.
	AgentCompletionMarker string `json:"-"`
	// AgentCompletionProtocolEnabled binds completion lifecycle handling to this
	// request and keeps forced tool-choice validation independent of the marker.
	AgentCompletionProtocolEnabled bool `json:"-"`
	// AgentReasoningCapEnabled independently marks recognized coding-agent tool
	// turns whose pre-action reasoning should be bounded. This deliberately does
	// not depend on AgentCompletionMarker or the completion lifecycle protocol.
	AgentReasoningCapEnabled bool `json:"-"`
	// AgentCompletionRetries counts semantic repair attempts independently from
	// transport/account retries. It is private to the handler's cloned payload.
	AgentCompletionRetries int `json:"-"`
	// AgentActionContinuations counts bounded continuations issued after a
	// native END_TURN contains neither a completion marker nor a structured call.
	AgentActionContinuations int `json:"-"`
	// AgentFullContextRecoveries counts the final recovery attempt that restores
	// the complete original upstream payload after short continuations fail.
	AgentFullContextRecoveries int `json:"-"`
	// AgentRecoveryCompacted records that a long, actionless turn was retried
	// with a bounded evidence-only history. Normal first attempts are unchanged.
	AgentRecoveryCompacted bool `json:"-"`
	// AgentRecoveryAggressive records the final, smaller evidence-only payload
	// used after Kiro rejects the normal recovery payload as too large.
	AgentRecoveryAggressive bool `json:"-"`
	// AgentInputTooLongRecoveries counts the one bounded fallback allowed after
	// an agent recovery attempt receives Kiro's HTTP 400 input-too-long error.
	AgentInputTooLongRecoveries int `json:"-"`
	// OutputContinuations counts bounded continuations after Kiro explicitly
	// terminates visible output with MAX_TOKENS.
	OutputContinuations int `json:"-"`
	// agentRecoveryActionHint retains only the UTF-8-safe tail of the current
	// failed attempt. It is injected as untrusted diagnostic context only when
	// constructing a semantic recovery request and is never serialized directly.
	agentRecoveryActionHint string
	// Recovery attempt output is split by semantic channel. Hidden reasoning is
	// never replayed as if it were visible assistant text.
	agentRecoveryAttemptReasoning   string
	agentRecoveryAttemptVisibleText string
	// Output continuation keeps the complete visible prefix separately from the
	// bounded diagnostic excerpt used by agent-action recovery.
	outputContinuationAttemptText     string
	outputContinuationPrefix          string
	outputContinuationAttemptOverflow bool
	outputContinuationPending         bool
	// agentRecoveryInjectedActionHint records the exact generated suffix so a
	// later semantic retry can replace it without scanning or altering user text.
	agentRecoveryInjectedActionHint string
	// agentRecoveryOriginalPayload preserves the complete initial upstream wire
	// payload so the last bounded recovery can undo any continuation compaction.
	agentRecoveryOriginalPayload []byte
	// agentFullContextRecoveryPending is true only between preparing the final
	// full-context request and receiving its terminal result.
	agentFullContextRecoveryPending bool
	// agentInputTooLongRecoveryPending is true only while the final aggressive
	// bounded-context fallback is in flight.
	agentInputTooLongRecoveryPending bool
	// agentTerminalRecoveryExhausted prevents a failed final bounded-context
	// attempt from borrowing another semantic retry and replaying the same body.
	agentTerminalRecoveryExhausted bool
	// maxContextUsagePercentage keeps the highest upstream occupancy observed
	// across the original generation and every semantic recovery attempt. A
	// bounded recovery request must not make terminal usage under-report the
	// original large context and delay downstream compaction.
	maxContextUsagePercentage float64
	// Enabled after an unmarked END_TURN starts its bounded continuation chain.
	// Streaming adapters may already have forwarded its visible text.
	agentActionContinuationTimeout bool
	// requestContext links the upstream request lifetime to the downstream
	// client. It is intentionally unexported and never serialized.
	requestContext context.Context
	// timeoutPolicy and recoveryStartedAt are shared by every transport,
	// semantic, and account attempt made for this cloned request.
	timeoutPolicy     kiroTimeoutPolicy
	recoveryStartedAt time.Time
	recoveryDeadline  time.Time
	// requestID and the last-attempt measurements make every upstream failure
	// correlatable even when Kiro omits its own request identifier.
	requestID                       string
	lastAttemptPayloadBytes         int
	lastAttemptEstimatedInputTokens int
	lastAttemptStartedAt            time.Time
}

func bindKiroPayloadContext(payload *KiroPayload, ctx context.Context) *KiroPayload {
	if payload != nil && ctx != nil {
		payload.requestContext = ctx
		if payload.requestID == "" {
			payload.requestID = "req_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		kiroRecoveryContext(payload, time.Now())
	}
	return payload
}

func ensureKiroTimeoutBudget(payload *KiroPayload, now time.Time) kiroTimeoutPolicy {
	if payload == nil {
		return kiroTimeoutPolicyFromEnv()
	}
	if payload.timeoutPolicy.generationTimeout < 0 ||
		payload.timeoutPolicy.recoveryChainTimeout < 0 ||
		payload.timeoutPolicy.bodyIdleTimeout < 0 ||
		payload.timeoutPolicy.responseHeaderTimeout <= 0 ||
		payload.timeoutPolicy.agentContinueTimeout < 0 {
		payload.timeoutPolicy = kiroTimeoutPolicyFromEnv()
	}
	if payload.recoveryStartedAt.IsZero() {
		payload.recoveryStartedAt = now
	}
	return payload.timeoutPolicy
}

func kiroRecoveryContext(payload *KiroPayload, now time.Time) (context.Context, kiroTimeoutPolicy) {
	policy := ensureKiroTimeoutBudget(payload, now)
	ctx := kiroPayloadContext(payload)
	if payload == nil {
		return ctx, policy
	}
	if policy.recoveryChainTimeout <= 0 {
		return ctx, policy
	}
	if payload.recoveryDeadline.IsZero() {
		payload.recoveryDeadline = payload.recoveryStartedAt.Add(policy.recoveryChainTimeout)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadlineCause(
			ctx,
			payload.recoveryDeadline,
			newUpstreamRecoveryTimeoutError(policy.recoveryChainTimeout),
		)
		// Generation request parents are canceled when the handler returns. The
		// callback also closes the deadline timer on every terminal path.
		context.AfterFunc(ctx, cancel)
		payload.requestContext = ctx
	}
	return payload.requestContext, policy
}

func kiroGenerationContext(payload *KiroPayload, now time.Time) (context.Context, context.CancelFunc, kiroTimeoutPolicy) {
	recoveryContext, policy := kiroRecoveryContext(payload, now)
	timeout := policy.generationTimeout
	cause := newUpstreamGenerationTimeoutError(timeout)
	if payload != nil && payload.agentActionContinuationTimeout &&
		policy.agentContinueTimeout > 0 && (timeout <= 0 || policy.agentContinueTimeout < timeout) {
		timeout = policy.agentContinueTimeout
		cause = newUpstreamAgentContinuationTimeoutError(timeout)
	}
	if timeout <= 0 {
		ctx, cancel := context.WithCancel(recoveryContext)
		return ctx, cancel, policy
	}
	ctx, cancel := context.WithTimeoutCause(
		recoveryContext,
		timeout,
		cause,
	)
	return ctx, cancel, policy
}

func kiroPayloadContext(payload *KiroPayload) context.Context {
	if payload != nil && payload.requestContext != nil {
		return payload.requestContext
	}
	return context.Background()
}

func isDownstreamRequestCanceled(payload *KiroPayload, err error) bool {
	if payload == nil || payload.requestContext == nil || payload.requestContext.Err() == nil {
		return false
	}
	return err == nil || errors.Is(err, payload.requestContext.Err()) || errors.Is(err, context.Canceled)
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	MessageID        string                `json:"messageId,omitempty"`
	Content          string                `json:"content"`
	ToolUses         []KiroToolUse         `json:"toolUses,omitempty"`
	ReasoningContent *KiroReasoningContent `json:"reasoningContent,omitempty"`
}

// KiroReasoningContent mirrors the upstream union. Exactly one branch is
// valid: signed plaintext reasoning, or an already-base64-encoded redacted
// blob. MarshalJSON rejects malformed internal values before they reach Kiro.
type KiroReasoningContent struct {
	ReasoningText   *KiroReasoningText `json:"reasoningText,omitempty"`
	RedactedContent string             `json:"redactedContent,omitempty"`
}

type KiroReasoningText struct {
	Text      string `json:"text"`
	Signature string `json:"signature"`
}

func (content KiroReasoningContent) MarshalJSON() ([]byte, error) {
	hasText := content.ReasoningText != nil
	hasRedacted := content.RedactedContent != ""
	if hasText == hasRedacted {
		return nil, errors.New("Kiro reasoningContent must contain exactly one union branch")
	}
	if hasText {
		if strings.TrimSpace(content.ReasoningText.Text) == "" ||
			strings.TrimSpace(content.ReasoningText.Signature) == "" {
			return nil, errors.New("Kiro reasoningText requires non-empty text and signature")
		}
		return json.Marshal(struct {
			ReasoningText *KiroReasoningText `json:"reasoningText"`
		}{ReasoningText: content.ReasoningText})
	}
	return json.Marshal(struct {
		RedactedContent string `json:"redactedContent"`
	}{RedactedContent: content.RedactedContent})
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	// OnStreamReady fires after Kiro returns a successful HTTP response and
	// before the first AWS Event Stream frame is read.
	OnStreamReady func()
	OnText        func(text string, isThinking bool)
	// OnReasoning receives native Kiro reasoning fields. When set, it replaces
	// the legacy OnText(text, true) callback for reasoning events.
	OnReasoning func(event KiroReasoningContentEvent)
	// OnToolUseStart and OnToolUseInputDelta expose validated Kiro tool event
	// framing before the complete argument object is available. OnToolUse still
	// fires exactly once after the accumulated JSON has been validated.
	OnToolUseStart      func(toolUseID, name string)
	OnToolUseInputDelta func(toolUseID, delta string)
	OnToolUse           func(toolUse KiroToolUse)
	OnComplete          func(inputTokens, outputTokens int)
	OnError             func(err error)
	OnStopReason        func(reason string)
	OnCredits           func(credits float64)
	OnContextUsage      func(percentage float64)
	// OnFirstEvent is an internal timing hook invoked once after the first
	// structurally valid AWS Event Stream frame arrives.
	OnFirstEvent func()
	// AgentCompletionMarker is copied from the request by CallKiroAPI so the
	// frame parser does not mistake uncommitted narration for a final answer.
	AgentCompletionMarker string
	// AgentCompletionRequired enables completion lifecycle handling for forced
	// tool-choice requests without requiring a text marker.
	AgentCompletionRequired bool
	// AgentThinkingTimeout limits only the initial agent attempt after native
	// reasoning begins and before a completion marker or tool call is observed.
	// Recovery attempts and ordinary non-agent streams leave it disabled.
	AgentThinkingTimeout time.Duration
	// ReasoningCapBytes bounds the reasoning a single response may emit before
	// its first tool call or visible text. Zero disables the accounting.
	ReasoningCapBytes int
	// ReasoningCapMode selects between silent accounting and termination once
	// ReasoningCapBytes is exceeded.
	ReasoningCapMode reasoningCapMode
	// BodyIdleTimeout limits continuous upstream silence between complete AWS
	// Event Stream frames. Downstream keepalive writes do not reset it.
	BodyIdleTimeout time.Duration
	// OnKeepAlive runs serially with the other stream callbacks while the
	// upstream is silent, so downstream SSE writes cannot interleave.
	OnKeepAlive func()
}

// KiroReasoningContentEvent preserves all fields carried by a native Kiro
// reasoningContentEvent. Text is normalized to a delta in the same way as
// assistant response content; signatures and redacted content are forwarded
// unchanged.
type KiroReasoningContentEvent struct {
	Text            string
	Signature       string
	RedactedContent string
}

type KiroStreamProtocolError struct {
	Code string
	Err  error
}

const (
	upstreamIncompleteResponseCode       = "upstream_incomplete_response"
	upstreamResponseHeaderTimeoutCode    = "upstream_response_header_timeout"
	upstreamBodyIdleTimeoutCode          = "upstream_body_idle_timeout"
	upstreamGenerationTimeoutCode        = "upstream_generation_timeout"
	upstreamReasoningCapCode             = "upstream_reasoning_cap_exceeded"
	upstreamOutputTruncatedCode          = "upstream_output_truncated"
	upstreamRecoveryTimeoutCode          = "upstream_recovery_timeout"
	upstreamAgentContinuationTimeoutCode = "upstream_agent_continuation_timeout"
)

var errUpstreamIncompleteResponse = errors.New("upstream did not complete the agent turn")
var errUpstreamAgentMarkerMismatch = errors.New("upstream did not complete the agent turn: completion marker prefix mismatch")
var errUpstreamAgentActionOnly = errors.New("upstream END_TURN had neither a completion marker nor a structured tool call")
var errUpstreamAgentToolChoiceMismatch = errors.New("upstream did not satisfy the required tool choice")
var errUpstreamOutputTruncated = errors.New("upstream output reached the token limit")

type KiroHTTPError struct {
	StatusCode int
	Endpoint   string
	Body       string
}

func (e *KiroHTTPError) Error() string {
	if e == nil {
		return "Kiro HTTP error"
	}
	return fmt.Sprintf("HTTP %d from %s: %s", e.StatusCode, e.Endpoint, e.Body)
}

func (e *KiroStreamProtocolError) Error() string {
	if e == nil || e.Err == nil {
		return "Kiro stream protocol error"
	}
	return e.Err.Error()
}

func (e *KiroStreamProtocolError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func kiroStreamProtocolErrorCode(err error) (string, bool) {
	var protocolErr *KiroStreamProtocolError
	if !errors.As(err, &protocolErr) {
		return "", false
	}
	return protocolErr.Code, true
}

func newUpstreamIncompleteResponseError() error {
	return &KiroStreamProtocolError{
		Code: upstreamIncompleteResponseCode,
		Err:  errUpstreamIncompleteResponse,
	}
}

func newUpstreamAgentMarkerMismatchError() error {
	return &KiroStreamProtocolError{
		Code: upstreamIncompleteResponseCode,
		Err:  fmt.Errorf("%w: upstream ended without the marker or a tool call", errUpstreamAgentMarkerMismatch),
	}
}

func newUpstreamAgentActionOnlyError() error {
	return &KiroStreamProtocolError{
		Code: upstreamIncompleteResponseCode,
		Err:  fmt.Errorf("%w: %w", errUpstreamAgentMarkerMismatch, errUpstreamAgentActionOnly),
	}
}

func newUpstreamAgentToolChoiceMismatchError(mode, selected string, observed []string) error {
	detail := "required tool choice ended without a completed structured tool call"
	if mode == "specific" {
		detail = fmt.Sprintf("specific tool choice %q was not satisfied", selected)
		if len(observed) > 0 {
			detail = fmt.Sprintf("%s; completed tools: %s", detail, strings.Join(observed, ", "))
		}
	}
	return &KiroStreamProtocolError{
		Code: upstreamIncompleteResponseCode,
		Err: fmt.Errorf(
			"%w: %w: %s",
			errUpstreamAgentMarkerMismatch,
			errUpstreamAgentToolChoiceMismatch,
			detail,
		),
	}
}

func newUpstreamOutputTruncatedError() error {
	return &KiroStreamProtocolError{
		Code: upstreamOutputTruncatedCode,
		Err:  errUpstreamOutputTruncated,
	}
}

func isUpstreamOutputTruncatedError(err error) bool {
	return errors.Is(err, errUpstreamOutputTruncated)
}

func newUpstreamAgentInterruptedError(cause error) error {
	detail := "upstream stream ended before the marker or a tool call"
	if cause != nil {
		detail = fmt.Sprintf("%s: %v", detail, cause)
	}
	return &KiroStreamProtocolError{
		Code: upstreamIncompleteResponseCode,
		Err:  fmt.Errorf("%w: %s", errUpstreamAgentMarkerMismatch, detail),
	}
}

func newUpstreamAgentThinkingTimeoutError(timeout time.Duration) error {
	return &KiroStreamProtocolError{
		Code: upstreamIncompleteResponseCode,
		Err: fmt.Errorf(
			"%w: initial reasoning exceeded %s without the marker or a tool call",
			errUpstreamAgentMarkerMismatch,
			timeout,
		),
	}
}

// newUpstreamReasoningCapError deliberately does not wrap
// errUpstreamAgentMarkerMismatch. That classification would route a cap hit
// into the narration-recovery chain, which pushes for immediate action and
// drops effort to "low" — the opposite of useful when the model is stuck inside
// reasoning. A timeout-family code lets the caller fail cleanly with the
// context intact instead.
func newUpstreamReasoningCapError(observed, limit int) error {
	return &KiroStreamProtocolError{
		Code: upstreamReasoningCapCode,
		Err: fmt.Errorf(
			"reasoning before the first tool call or visible text reached %d bytes, over the %d byte limit",
			observed, limit,
		),
	}
}

func newUpstreamTimeoutError(code, stage string, timeout time.Duration) error {
	return &KiroStreamProtocolError{
		Code: code,
		Err:  fmt.Errorf("%s exceeded %s", stage, timeout),
	}
}

func newUpstreamResponseHeaderTimeoutError(timeout time.Duration) error {
	return newUpstreamTimeoutError(upstreamResponseHeaderTimeoutCode, "upstream response header wait", timeout)
}

func newUpstreamBodyIdleTimeoutError(timeout time.Duration) error {
	return newUpstreamTimeoutError(upstreamBodyIdleTimeoutCode, "upstream response body silence", timeout)
}

func newUpstreamGenerationTimeoutError(timeout time.Duration) error {
	return newUpstreamTimeoutError(upstreamGenerationTimeoutCode, "upstream generation", timeout)
}

func newUpstreamRecoveryTimeoutError(timeout time.Duration) error {
	return newUpstreamTimeoutError(upstreamRecoveryTimeoutCode, "upstream recovery chain", timeout)
}

func newUpstreamAgentContinuationTimeoutError(timeout time.Duration) error {
	return newUpstreamTimeoutError(upstreamAgentContinuationTimeoutCode, "upstream agent continuation", timeout)
}

func isKiroTimeoutError(err error) bool {
	code, ok := kiroStreamProtocolErrorCode(err)
	if !ok {
		return false
	}
	switch code {
	case upstreamResponseHeaderTimeoutCode, upstreamBodyIdleTimeoutCode,
		upstreamGenerationTimeoutCode, upstreamRecoveryTimeoutCode,
		upstreamAgentContinuationTimeoutCode, upstreamReasoningCapCode:
		return true
	default:
		return false
	}
}

func isKiroRecoveryTimeoutError(err error) bool {
	code, ok := kiroStreamProtocolErrorCode(err)
	return ok && code == upstreamRecoveryTimeoutCode
}

func isKiroAgentContinuationTimeoutError(err error) bool {
	code, ok := kiroStreamProtocolErrorCode(err)
	return ok && code == upstreamAgentContinuationTimeoutCode
}

func isUpstreamAgentMarkerMismatchError(err error) bool {
	return errors.Is(err, errUpstreamAgentMarkerMismatch)
}

func isUpstreamAgentActionOnlyError(err error) bool {
	return errors.Is(err, errUpstreamAgentActionOnly)
}

func isUpstreamAgentToolChoiceMismatchError(err error) bool {
	return errors.Is(err, errUpstreamAgentToolChoiceMismatch)
}

func isUpstreamIncompleteResponseError(err error) bool {
	if errors.Is(err, errUpstreamIncompleteResponse) {
		return true
	}
	code, ok := kiroStreamProtocolErrorCode(err)
	return ok && code == upstreamIncompleteResponseCode
}

func kiroPayloadHasTools(payload *KiroPayload) bool {
	return payload != nil &&
		payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext != nil &&
		len(payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools) > 0
}

func initialAgentThinkingTimeout(payload *KiroPayload) time.Duration {
	if !kiroPayloadHasTools(payload) || !agentCompletionProtocolEnabled(payload) || payload.AgentCompletionRetries != 0 {
		return 0
	}
	return kiroInitialAgentThinkingTimeout
}

func recordAgentCompletionFailure(payload *KiroPayload, causes ...error) {
	if !kiroPayloadHasTools(payload) {
		return
	}
	if agentCompletionProtocolEnabled(payload) {
		payload.AgentCompletionRetries++
		if len(causes) > 0 && isUpstreamAgentActionOnlyError(causes[0]) {
			payload.AgentActionContinuations++
		}
	}
}

func prepareReasoningOnlyRetry(payload *KiroPayload, causes ...error) {
	if !kiroPayloadHasTools(payload) || !agentCompletionProtocolEnabled(payload) {
		return
	}
	captureAgentRecoveryOriginalPayload(payload)
	inputTooLongRecoveryFinished := payload.agentInputTooLongRecoveryPending
	finalRecoveryAttemptFinished := payload.agentFullContextRecoveryPending || inputTooLongRecoveryFinished
	payload.agentFullContextRecoveryPending = false
	payload.agentInputTooLongRecoveryPending = false
	if inputTooLongRecoveryFinished {
		payload.agentTerminalRecoveryExhausted = true
	}
	recordAgentCompletionFailure(payload, causes...)
	actionOnly := len(causes) > 0 && isUpstreamAgentActionOnlyError(causes[0])
	if actionOnly {
		payload.agentActionContinuationTimeout = true
	}
	if finalRecoveryAttemptFinished {
		return
	}
	if payload.AgentCompletionRetries > maxKiroAgentCompletionRetries ||
		payload.AgentActionContinuations > maxKiroAgentActionContinuations {
		prepareFullContextAgentRecovery(payload)
		return
	}
	// Never make a recovery less capable than the original request. The first
	// short continuation preserves the requested effort; later continuations
	// raise low-effort requests to at least medium without reducing high, xhigh,
	// or max.
	if payload.AdditionalModelRequestFields != nil && payload.AdditionalModelRequestFields.OutputConfig != nil {
		if payload.AgentCompletionRetries > 1 {
			setAgentRecoveryEffort(payload, "medium")
		}
	}
	escalatedAction := actionOnly && payload.AgentActionContinuations == maxKiroAgentActionContinuations
	if escalatedAction {
		compactAgentRecoveryPayload(payload, true, false)
	}
	advanceAgentRecoveryTurn(payload)
	message := &payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if escalatedAction {
		*message = appendSystemInstruction(*message, agentEscalatedActionContinuationInstruction())
	} else {
		instruction := agentCompletionRetryInstruction(payload.AgentCompletionMarker)
		if forcedToolChoiceMode(payload) {
			instruction = forcedToolChoiceRetryInstructionForPayload(payload)
		}
		if !strings.Contains(*message, instruction) {
			*message = appendSystemInstruction(*message, instruction)
		}
	}
	if actionOnly && !escalatedAction {
		*message = appendSystemInstruction(*message, agentActionContinuationInstruction())
	}
	*message = replaceAgentRecoveryActionHintInstruction(payload, *message, payload.agentRecoveryActionHint)
}

func agentCompletionRetryAllowed(payload *KiroPayload, causes ...error) bool {
	if payload == nil || !agentCompletionProtocolEnabled(payload) {
		return false
	}
	if payload.agentTerminalRecoveryExhausted {
		return false
	}
	if payload.agentFullContextRecoveryPending || payload.agentInputTooLongRecoveryPending {
		return true
	}
	if payload.AgentCompletionRetries > maxKiroAgentCompletionRetries {
		return false
	}
	if len(causes) > 0 && isUpstreamAgentActionOnlyError(causes[0]) &&
		payload.AgentActionContinuations > maxKiroAgentActionContinuations {
		return false
	}
	return len(causes) == 0 || isUpstreamAgentMarkerMismatchError(causes[0])
}

// isRetryableKiroStreamError identifies transient failures that are safe to
// retry before an answer or tool call. Kiro can terminate an otherwise valid
// stream while an MCP/tool JSON fragment is being assembled, and network/proxy
// layers commonly surface the same failure as a context deadline or timeout.
// Callers must gate transport retries with their response-started flag; the
// explicit incomplete-response case may continue after a closed reasoning block.
func isRetryableKiroStreamError(err error) bool {
	if err == nil {
		return false
	}
	if code, ok := kiroStreamProtocolErrorCode(err); ok {
		switch code {
		case "upstream_tool_json_error", upstreamIncompleteResponseCode,
			upstreamResponseHeaderTimeoutCode, upstreamBodyIdleTimeoutCode,
			upstreamGenerationTimeoutCode:
			return true
		default:
			return false
		}
	}
	if isKiroTransportError(err) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "encountered an unexpected error when processing the request") ||
		(strings.Contains(message, "kiro stream exception") && strings.Contains(message, "internal")) ||
		(strings.Contains(message, "internal_error") && strings.Contains(message, "stream")) ||
		(strings.Contains(message, "received from peer") && strings.Contains(message, "stream"))
}

func isKiroTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func isKiroClientRequestError(err error) bool {
	var preflightErr *KiroPayloadPreflightError
	if errors.As(err, &preflightErr) {
		return true
	}
	var httpErr *KiroHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode < http.StatusBadRequest || httpErr.StatusCode >= http.StatusInternalServerError {
		return false
	}
	switch httpErr.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	default:
		return true
	}
}

func isKiroTransientHTTPError(err error) bool {
	var httpErr *KiroHTTPError
	return errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusRequestTimeout || httpErr.StatusCode >= http.StatusInternalServerError)
}

func isKiroToolJSONError(err error) bool {
	code, ok := kiroStreamProtocolErrorCode(err)
	return ok && code == "upstream_tool_json_error"
}

func isKiroInputTooLongBody(body []byte) bool {
	message := strings.ToLower(string(body))
	return strings.Contains(message, "input is too long") ||
		(strings.Contains(message, "request_body_invalid") && strings.Contains(message, "too long"))
}

func isKiroInputTooLongError(err error) bool {
	if err == nil {
		return false
	}
	return isKiroInputTooLongBody([]byte(err.Error()))
}

func isKiroThinkingSignatureInvalidError(err error) bool {
	var httpErr *KiroHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		return false
	}
	body := strings.ToLower(httpErr.Body)
	return strings.Contains(body, "thinking_signature_invalid") ||
		(strings.Contains(body, "thinking.signature") &&
			(strings.Contains(body, "field required") || strings.Contains(body, "invalid")))
}

func isKiroNativeHistoryValidationError(err error) bool {
	var httpErr *KiroHTTPError
	if !errors.As(err, &httpErr) ||
		(httpErr.StatusCode != http.StatusBadRequest && httpErr.StatusCode != http.StatusUnprocessableEntity) {
		return false
	}
	body := strings.ToLower(httpErr.Body)
	if isKiroInputTooLongBody([]byte(body)) {
		return false
	}
	// Kiro commonly collapses invalid tool-use/tool-result history into this
	// message without identifying the failing field.
	if strings.Contains(body, "improperly formed request") {
		return true
	}
	historyField := containsAnySubstring(body,
		"conversationstate.history", "conversation state history",
		"assistantresponsemessage", "assistant response message",
		"userinputmessagecontext", "user input message context",
		"toolresults", "tool results", "tool_result", "tool result",
		"tooluses", "tool uses", "tool_use", "tool use",
		"messageid", "message id", "reasoningcontent", "redactedcontent",
	)
	if !historyField {
		return false
	}
	return containsAnySubstring(body,
		"request_body_invalid", "invalid", "malformed", "validation",
		"field required", "missing", "not supported", "unexpected", "unknown field",
	)
}

func containsAnySubstring(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func stripKiroHistoryReasoning(payload *KiroPayload) bool {
	if payload == nil {
		return false
	}
	stripped := false
	for i := range payload.ConversationState.History {
		assistant := payload.ConversationState.History[i].AssistantResponseMessage
		if assistant == nil || assistant.ReasoningContent == nil {
			continue
		}
		assistant.ReasoningContent = nil
		stripped = true
	}
	return stripped
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
	}
}

func kiroEndpointError(ep kiroEndpoint, statusCode int, body []byte, retryAfter string) error {
	if statusCode == http.StatusTooManyRequests {
		return &UpstreamRateLimitError{
			Endpoint:   ep.Name,
			Body:       string(body),
			RetryAfter: parseRetryAfter(retryAfter, time.Now()),
		}
	}
	return &KiroHTTPError{StatusCode: statusCode, Endpoint: ep.Name, Body: string(body)}
}

func logKiroUpstreamAttemptError(payload *KiroPayload, account *config.Account, started time.Time, err error) {
	if err == nil {
		return
	}
	requestID := "-"
	phase := "transport"
	completionRetries := 0
	actionContinuations := 0
	fullContextRecoveries := 0
	inputTooLongRecoveries := 0
	payloadBytes := 0
	estimatedTokens := 0
	if payload != nil {
		if payload.requestID != "" {
			requestID = payload.requestID
		}
		phase = kiroAgentRecoveryPhase(payload, err)
		completionRetries = payload.AgentCompletionRetries
		actionContinuations = payload.AgentActionContinuations
		fullContextRecoveries = payload.AgentFullContextRecoveries
		inputTooLongRecoveries = payload.AgentInputTooLongRecoveries
		payloadBytes = payload.lastAttemptPayloadBytes
		estimatedTokens = payload.lastAttemptEstimatedInputTokens
	}
	accountID := "-"
	if account != nil && account.ID != "" {
		accountID = account.ID
	}
	logger.Warnf(
		"[KiroAPI] upstream attempt failed request_id=%s account_id=%s phase=%s completions=%d actions=%d full_context=%d input_too_long=%d elapsed_ms=%d payload_bytes=%d estimated_tokens=%d: %v",
		requestID, accountID, phase, completionRetries, actionContinuations, fullContextRecoveries,
		inputTooLongRecoveries, time.Since(started).Milliseconds(), payloadBytes, estimatedTokens, err,
	)
}

// CallKiroAPI calls only the account's regional Q streaming endpoint. Retry
// and account-switching decisions remain the caller's job.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) (returnErr error) {
	attemptStarted := time.Now()
	if payload != nil {
		payload.lastAttemptStartedAt = attemptStarted
	}
	defer func() {
		if returnErr != nil {
			logKiroUpstreamAttemptError(payload, account, attemptStarted, returnErr)
		}
	}()
	requestContext, cancelGeneration, timeoutPolicy := kiroGenerationContext(payload, time.Now())
	defer cancelGeneration()
	if err := requestContext.Err(); err != nil {
		return kiroContextError(requestContext, err)
	}
	if payload == nil {
		return fmt.Errorf("Kiro request payload is nil")
	}
	if len(kiroEndpoints) == 0 {
		return fmt.Errorf("no Kiro upstream endpoints configured")
	}
	ep := kiroEndpoints[0]
	originalProfileArn := ""
	originalProfileArn = payload.ProfileArn
	defer func() {
		payload.ProfileArn = originalProfileArn
	}()
	setPayloadProfileArnForAccount(payload, account)
	payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin
	if _, err := preflightKiroPayload(payload); err != nil {
		return err
	}
	profileArnBeforeResolution := payload.ProfileArn
	callback = captureAgentRecoveryActionHint(payload, callback)
	if callback == nil {
		callback = &KiroStreamCallback{}
	} else {
		wrapped := *callback
		callback = &wrapped
	}
	if callback.BodyIdleTimeout <= 0 {
		callback.BodyIdleTimeout = timeoutPolicy.bodyIdleTimeout
	}
	timing := requestTimingFromContext(requestContext)

	// Restore original tool names for both the incremental start event and the
	// validated completion callback.
	if callback != nil && len(payload.ToolNameMap) > 0 {
		nameMap := payload.ToolNameMap
		wrapped := *callback
		if originalOnToolUseStart := callback.OnToolUseStart; originalOnToolUseStart != nil {
			wrapped.OnToolUseStart = func(toolUseID, name string) {
				if original, ok := nameMap[name]; ok {
					name = original
				}
				originalOnToolUseStart(toolUseID, name)
			}
		}
		if originalOnToolUse := callback.OnToolUse; originalOnToolUse != nil {
			wrapped.OnToolUse = func(tu KiroToolUse) {
				if original, ok := nameMap[tu.Name]; ok {
					tu.Name = original
				}
				originalOnToolUse(tu)
			}
		}
		callback = &wrapped
	}
	if payload != nil && agentCompletionProtocolEnabled(payload) {
		if callback.AgentCompletionMarker == "" {
			callback.AgentCompletionMarker = payload.AgentCompletionMarker
		}
		callback.AgentCompletionRequired = true
		callback.AgentThinkingTimeout = initialAgentThinkingTimeout(payload)
	}
	// The reasoning budget is an independent resource guard. It must remain
	// active for recognized coding-agent turns even when the optional marker
	// completion protocol is disabled in the WebUI. Resolve the settings for
	// every request so a successful admin update takes effect without restart.
	applyReasoningCapSettings(payload, callback, resolveReasoningCapSettings())
	callback = instrumentKiroCallbackTiming(timing, callback)

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" {
		if profileArn, err := ResolveProfileArnContext(requestContext, account); err == nil {
			payload.ProfileArn = profileArn
		} else if requestContext.Err() != nil {
			return kiroContextError(requestContext, err)
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	// Profile discovery can add a small serialized field after the initial local
	// check. Re-run the same idempotent preflight before creating the HTTP body;
	// recovery attempts also pass through here after mutating their payload.
	if payload.ProfileArn != profileArnBeforeResolution {
		if _, err := preflightKiroPayload(payload); err != nil {
			return err
		}
	}
	reqBody, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return marshalErr
	}
	if payload != nil {
		payload.lastAttemptPayloadBytes = len(reqBody)
		_, payload.lastAttemptEstimatedInputTokens = agentRecoveryPayloadEstimate(payload, "")
	}
	logger.Debugf("[KiroAPI] Request payload: %s", reqBody)

	epURL := regionalizeURLForProfile(ep.URL, account, payload.ProfileArn)
	if timing != nil {
		requestContext = httptrace.WithClientTrace(requestContext, timing.clientTrace())
	}
	req, requestErr := http.NewRequestWithContext(requestContext, http.MethodPost, epURL, bytes.NewReader(reqBody))
	if requestErr != nil {
		return requestErr
	}

	headerValues := buildStreamingHeaderValues(account, req.URL.Host)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	applyKiroBaseHeaders(req, account, headerValues)
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=1")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

	if timing != nil {
		timing.beginUpstream(time.Now())
	}
	resp, doErr := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if timing != nil && resp != nil {
		timing.markUpstreamHeader(time.Now())
	}
	if doErr != nil {
		logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, doErr)
		return normalizeKiroCallError(requestContext, doErr, timeoutPolicy)
	}

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		if callback.OnStreamReady != nil {
			callback.OnStreamReady()
		}
		streamErr := parseEventStream(resp.Body, callback)
		resp.Body.Close()
		streamErr = normalizeKiroCallError(requestContext, streamErr, timeoutPolicy)
		if isUpstreamIncompleteResponseError(streamErr) {
			prepareReasoningOnlyRetry(payload, streamErr)
		}
		return streamErr
	}

	errBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || requestContext.Err() != nil {
		return normalizeKiroCallError(requestContext, readErr, timeoutPolicy)
	}
	err := kiroEndpointError(ep, resp.StatusCode, errBody, resp.Header.Get("Retry-After"))
	logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, err)
	if payload.HistoryMode == kiroHistoryModeNativeActive &&
		!payload.ReasoningSignatureRetry && isKiroThinkingSignatureInvalidError(err) &&
		stripKiroHistoryReasoning(payload) {
		payload.ReasoningSignatureRetry = true
		logger.Warnf(
			"[KiroAPI] retrying once without replayed reasoning after signature rejection request_id=%s account=%s",
			kiroPayloadRequestID(payload), accountEmailForLog(account),
		)
		return CallKiroAPI(account, payload, callback)
	}
	if payload.HistoryMode == kiroHistoryModeNativeActive && isKiroNativeHistoryValidationError(err) {
		fallbackKiroHistoryToLegacy(payload)
		logger.Warnf(
			"[KiroAPI] retrying once with legacy history after native history validation rejection request_id=%s account=%s",
			kiroPayloadRequestID(payload), accountEmailForLog(account),
		)
		// fallbackKiroHistoryToLegacy changes HistoryMode to legacy before the
		// recursive call, which makes this retry intrinsically single-shot.
		return CallKiroAPI(account, payload, callback)
	}
	return err
}

func kiroContextError(ctx context.Context, fallback error) error {
	if ctx != nil && ctx.Err() != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return ctx.Err()
	}
	return fallback
}

func normalizeKiroCallError(ctx context.Context, err error, policy kiroTimeoutPolicy) error {
	if ctx != nil && ctx.Err() != nil {
		return kiroContextError(ctx, err)
	}
	if isResponseHeaderTimeoutError(err) {
		return newUpstreamResponseHeaderTimeoutError(policy.responseHeaderTimeout)
	}
	return err
}

func isResponseHeaderTimeoutError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "timeout awaiting response headers")
}

// accountEmailForLog returns the most informative stable identifier for an
// account in log lines. OAuth accounts log their email. api_key accounts have
// no email until their first successful refresh (RefreshAccountInfo populates
// it from getUsageLimits), so without a fallback every refresh/ ban/ model-
// cache line for them was blind ("for : ..."). Fall back to the masked key,
// which is stable and safe to log.
func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	if email := strings.TrimSpace(account.Email); email != "" {
		return email
	}
	if account.IsApiKeyCredential() && account.KiroApiKey != "" {
		return maskKey(account.KiroApiKey)
	}
	return account.Email
}

// ==================== Event Stream Parsing ====================

// parseEventStream decodes an AWS binary Event Stream response body.
func parseEventStream(body io.Reader, callback *KiroStreamCallback) error {
	return parseEventStreamWithKeepAlive(body, callback, kiroStreamKeepAliveInterval)
}

type kiroEventStreamFrame struct {
	prelude []byte
	message []byte
	err     error
}

func readKiroEventStreamFrame(body io.Reader) kiroEventStreamFrame {
	prelude := make([]byte, 12)
	if _, err := io.ReadFull(body, prelude); err != nil {
		if err == io.EOF {
			return kiroEventStreamFrame{err: io.EOF}
		}
		return kiroEventStreamFrame{err: fmt.Errorf("read Kiro event stream prelude: %w", err)}
	}

	totalLength := int(binary.BigEndian.Uint32(prelude[0:4]))
	headersLength := int(binary.BigEndian.Uint32(prelude[4:8]))
	if totalLength < 16 || totalLength > maxKiroEventStreamFrameBytes {
		return kiroEventStreamFrame{err: fmt.Errorf("invalid Kiro event stream frame length %d", totalLength)}
	}
	if headersLength > totalLength-16 {
		return kiroEventStreamFrame{err: fmt.Errorf(
			"invalid Kiro event stream headers length %d for frame length %d",
			headersLength,
			totalLength,
		)}
	}
	wantPreludeCRC := binary.BigEndian.Uint32(prelude[8:12])
	if gotPreludeCRC := crc32.ChecksumIEEE(prelude[:8]); gotPreludeCRC != wantPreludeCRC {
		return kiroEventStreamFrame{err: fmt.Errorf(
			"invalid Kiro event stream prelude CRC: got %08x, want %08x",
			gotPreludeCRC,
			wantPreludeCRC,
		)}
	}

	message := make([]byte, totalLength-12)
	if _, err := io.ReadFull(body, message); err != nil {
		return kiroEventStreamFrame{err: fmt.Errorf("read Kiro event stream frame: %w", err)}
	}
	return kiroEventStreamFrame{prelude: prelude, message: message}
}

func streamKiroEventStreamFrames(body io.Reader, frames chan<- kiroEventStreamFrame, done <-chan struct{}) {
	defer close(frames)
	for {
		frame := readKiroEventStreamFrame(body)
		select {
		case frames <- frame:
		case <-done:
			return
		}
		if frame.err != nil {
			return
		}
	}
}

func parseEventStreamWithKeepAlive(body io.Reader, callback *KiroStreamCallback, keepAliveInterval time.Duration) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	agentCompletionRequired := callback.AgentCompletionRequired || callback.AgentCompletionMarker != ""

	// io.ReadFull must wait for a complete upstream frame. Read it on a dedicated
	// goroutine so the parser can keep the downstream SSE connection alive while
	// Kiro is thinking. Callback dispatch remains serial on this goroutine.
	var frames <-chan kiroEventStreamFrame
	var done chan struct{}
	var keepAlive <-chan time.Time
	var keepAliveTimer *time.Timer
	var thinkingTimeout <-chan time.Time
	var thinkingTimer *time.Timer
	var bodyIdleTimeout <-chan time.Time
	var bodyIdleTimer *time.Timer
	asyncRead := (callback.OnKeepAlive != nil && keepAliveInterval > 0) ||
		callback.AgentThinkingTimeout > 0 || callback.BodyIdleTimeout > 0
	if asyncRead {
		frameStream := make(chan kiroEventStreamFrame, 1)
		frames = frameStream
		done = make(chan struct{})
		defer close(done)
		go streamKiroEventStreamFrames(body, frameStream, done)

		if callback.OnKeepAlive != nil && keepAliveInterval > 0 {
			keepAliveTimer = time.NewTimer(keepAliveInterval)
			defer keepAliveTimer.Stop()
			keepAlive = keepAliveTimer.C
		}
		if callback.BodyIdleTimeout > 0 {
			bodyIdleTimer = time.NewTimer(callback.BodyIdleTimeout)
			defer bodyIdleTimer.Stop()
			bodyIdleTimeout = bodyIdleTimer.C
		}
	}
	resetKeepAlive := func() {
		if keepAliveTimer == nil {
			return
		}
		if !keepAliveTimer.Stop() {
			select {
			case <-keepAliveTimer.C:
			default:
			}
		}
		keepAliveTimer.Reset(keepAliveInterval)
	}
	resetBodyIdle := func() {
		if bodyIdleTimer == nil {
			return
		}
		if !bodyIdleTimer.Stop() {
			select {
			case <-bodyIdleTimer.C:
			default:
			}
		}
		bodyIdleTimer.Reset(callback.BodyIdleTimeout)
	}
	startThinkingTimer := func() {
		if thinkingTimer != nil || callback.AgentThinkingTimeout <= 0 {
			return
		}
		thinkingTimer = time.NewTimer(callback.AgentThinkingTimeout)
		thinkingTimeout = thinkingTimer.C
	}
	stopThinkingTimer := func() {
		if thinkingTimer == nil {
			return
		}
		if !thinkingTimer.Stop() {
			select {
			case <-thinkingTimer.C:
			default:
			}
		}
		thinkingTimer = nil
		thinkingTimeout = nil
	}
	defer stopThinkingTimer()

	var inputTokens, outputTokens int
	var totalCredits float64
	markerState := agentCompletionPrefixPending
	var markerProbe strings.Builder
	var toolObserved bool
	var completedToolUses int
	var agentOutputObserved bool
	var assistantTextObserved bool
	var agentVisibleTextObserved bool
	// Accounted only while no tool call and no visible text have appeared, so a
	// multi-round agent task resets it at every outward action and never
	// accumulates across the whole task.
	var preActionReasoningBytes int
	var reasoningCapReported bool
	var meteringObserved bool
	var upstreamStopReason string
	firstEventReported := false
	toolUses := newToolUseAccumulator()
	reportFrameError := func(err error) error {
		if agentCompletionRequired && agentOutputObserved &&
			markerState != agentCompletionPrefixComplete && !toolObserved {
			err = newUpstreamAgentInterruptedError(err)
		}
		return reportKiroStreamError(callback, err)
	}
	for {
		var frame kiroEventStreamFrame
		if !asyncRead {
			frame = readKiroEventStreamFrame(body)
		} else {
			if thinkingTimeout != nil {
				select {
				case <-thinkingTimeout:
					return reportKiroStreamError(callback, newUpstreamAgentThinkingTimeoutError(callback.AgentThinkingTimeout))
				default:
				}
			}
			select {
			case <-thinkingTimeout:
				return reportKiroStreamError(callback, newUpstreamAgentThinkingTimeoutError(callback.AgentThinkingTimeout))
			case <-bodyIdleTimeout:
				return reportKiroStreamError(callback, newUpstreamBodyIdleTimeoutError(callback.BodyIdleTimeout))
			case <-keepAlive:
				callback.OnKeepAlive()
				resetKeepAlive()
				continue
			case next, ok := <-frames:
				if !ok {
					frame.err = io.EOF
				} else {
					frame = next
				}
				resetKeepAlive()
			}
		}
		if frame.err == io.EOF {
			break
		}
		if frame.err != nil {
			if agentCompletionRequired && toolObserved && errors.Is(frame.err, io.ErrUnexpectedEOF) {
				if len(toolUses.states) > 0 {
					return reportKiroStreamError(callback, &KiroStreamProtocolError{
						Code: "upstream_tool_json_error",
						Err:  fmt.Errorf("tool stream disconnected with %d active tool use(s): %w", len(toolUses.states), frame.err),
					})
				}
				if completedToolUses > 0 {
					// A tool carrying stop=true has already been validated and
					// committed to the agent client. A disconnect in trailing
					// narration or metadata must not invalidate that tool turn.
					break
				}
			}
			return reportFrameError(frame.err)
		}
		resetBodyIdle()

		prelude := frame.prelude
		msgBuf := frame.message
		headersLength := int(binary.BigEndian.Uint32(prelude[4:8]))

		wantMessageCRC := binary.BigEndian.Uint32(msgBuf[len(msgBuf)-4:])
		messageCRC := crc32.NewIEEE()
		_, _ = messageCRC.Write(prelude)
		_, _ = messageCRC.Write(msgBuf[:len(msgBuf)-4])
		if gotMessageCRC := messageCRC.Sum32(); gotMessageCRC != wantMessageCRC {
			return reportFrameError(fmt.Errorf(
				"invalid Kiro event stream message CRC: got %08x, want %08x",
				gotMessageCRC,
				wantMessageCRC,
			))
		}

		headers, err := extractEventStreamStringHeaders(msgBuf[0:headersLength])
		if err != nil {
			return reportFrameError(fmt.Errorf("invalid Kiro event stream headers: %w", err))
		}
		eventType := headers[":event-type"]
		payloadBytes := msgBuf[headersLength : len(msgBuf)-4]
		if !firstEventReported {
			firstEventReported = true
			if callback.OnFirstEvent != nil {
				callback.OnFirstEvent()
			}
		}

		messageType := headers[":message-type"]
		if stopReason := kiroStopReasonFromEvent(headers, eventType); stopReason != "" {
			upstreamStopReason = normalizeKiroStopReason(stopReason)
			if callback.OnStopReason != nil {
				callback.OnStopReason(upstreamStopReason)
			}
			break
		}
		if messageType == "error" || messageType == "exception" {
			streamErr := newKiroEventStreamError(messageType, headers, payloadBytes)
			return reportFrameError(streamErr)
		}
		if messageType != "" && messageType != "event" {
			return reportFrameError(fmt.Errorf("unsupported Kiro event stream message type %q", messageType))
		}
		if eventType == "" {
			return reportFrameError(fmt.Errorf("Kiro event stream frame is missing :event-type"))
		}
		if len(payloadBytes) == 0 {
			continue
		}

		var event map[string]interface{}
		decoder := json.NewDecoder(bytes.NewReader(payloadBytes))
		if eventType == "toolUseEvent" {
			decoder.UseNumber()
		}
		if err := decoder.Decode(&event); err != nil {
			return reportFrameError(fmt.Errorf("invalid %s payload: %w", eventType, err))
		}
		var trailing interface{}
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				err = fmt.Errorf("multiple JSON values")
			}
			return reportFrameError(fmt.Errorf("invalid %s payload: %w", eventType, err))
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens)

		// Dispatch by event type.
		switch eventType {
		case "assistantResponseEvent":
			if toolObserved && agentCompletionRequired {
				// A structured tool call must terminate the assistant content for
				// agent clients. Kiro occasionally emits late narration after the
				// tool event; forwarding it after tool_use can stop the client's
				// automatic tool-result continuation.
				continue
			}
			if content, ok := event["content"].(string); ok && content != "" {
				agentOutputObserved = true
				assistantTextObserved = true
				if callback.AgentCompletionMarker != "" {
					if markerState == agentCompletionPrefixPending {
						markerProbe.WriteString(content)
						probe := markerProbe.String()
						markerState = classifyAgentCompletionPrefix(probe, callback.AgentCompletionMarker)
						if markerState != agentCompletionPrefixPending {
							markerProbe.Reset()
							switch markerState {
							case agentCompletionPrefixComplete:
								visible, _ := stripAgentCompletionMarker(probe, callback.AgentCompletionMarker)
								agentVisibleTextObserved = strings.TrimSpace(visible) != ""
								stopThinkingTimer()
							case agentCompletionPrefixMismatch:
								agentVisibleTextObserved = strings.TrimSpace(probe) != ""
							}
						}
					} else if strings.TrimSpace(content) != "" {
						agentVisibleTextObserved = true
					}
				}
				if callback.OnText != nil {
					callback.OnText(content, false)
				}
			}
		case "reasoningContentEvent":
			if toolObserved && agentCompletionRequired {
				continue
			}
			reasoning := KiroReasoningContentEvent{
				Signature:       firstStringField(event, "signature"),
				RedactedContent: firstStringField(event, "redactedContent", "redacted_content"),
			}
			if text, ok := event["text"].(string); ok && text != "" {
				reasoning.Text = text
			}
			if reasoning.Text != "" || reasoning.Signature != "" || reasoning.RedactedContent != "" {
				agentOutputObserved = true
				startThinkingTimer()
				if callback.ReasoningCapBytes > 0 && callback.ReasoningCapMode != reasoningCapOff &&
					!toolObserved && !assistantTextObserved {
					preActionReasoningBytes += len(reasoning.Text)
					if preActionReasoningBytes > callback.ReasoningCapBytes {
						if callback.ReasoningCapMode == reasoningCapEnforce {
							return reportKiroStreamError(callback, newUpstreamReasoningCapError(
								preActionReasoningBytes, callback.ReasoningCapBytes))
						}
						// Observation mode records the overshoot once and keeps
						// streaming, so a limit can be calibrated against real
						// traffic before it terminates anything.
						if !reasoningCapReported {
							reasoningCapReported = true
							logger.Warnf(
								"[reasoning-cap] observed %d bytes before the first action, limit %d bytes",
								preActionReasoningBytes, callback.ReasoningCapBytes,
							)
						}
					}
				}
				if callback.OnReasoning != nil {
					callback.OnReasoning(reasoning)
				} else if reasoning.Text != "" && callback.OnText != nil {
					callback.OnText(reasoning.Text, true)
				}
			}
		case "toolUseEvent":
			if err := toolUses.push(event, callback); err != nil {
				return reportKiroStreamError(callback, &KiroStreamProtocolError{Code: "upstream_tool_json_error", Err: err})
			}
			toolObserved = true
			if firstBoolField(event, "stop", "isStop", "done") {
				completedToolUses++
			}
			stopThinkingTimer()
		case "meteringEvent":
			meteringObserved = true
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
		case "metadataEvent":
			if reason := firstStringField(event, "stopReason", "stop_reason"); reason != "" {
				upstreamStopReason = normalizeKiroStopReason(reason)
				if callback.OnStopReason != nil {
					callback.OnStopReason(upstreamStopReason)
				}
			}
		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				if callback.OnContextUsage != nil {
					callback.OnContextUsage(pct)
				}
			}
		}
	}

	if err := toolUses.finish(callback); err != nil {
		return reportKiroStreamError(callback, &KiroStreamProtocolError{Code: "upstream_tool_json_error", Err: err})
	}
	// IAM Identity Center / Enterprise streams can end cleanly without a
	// metadataEvent. Their terminal metering frame is a safe fallback because
	// billing is emitted only after generation has finished. Explicit metadata
	// always wins.
	if upstreamStopReason == "" && meteringObserved {
		upstreamStopReason = "end_turn"
		if toolObserved {
			upstreamStopReason = "tool_use"
		}
		if callback.OnStopReason != nil {
			callback.OnStopReason(upstreamStopReason)
		}
	}
	// Terminal metering belongs to the attempt even when the agent completion
	// protocol rejects its output. Publish it before marker validation so the
	// handler can charge a discarded recovery attempt accurately.
	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}
	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	if upstreamStopReason == "max_tokens" {
		return reportKiroStreamError(callback, newUpstreamOutputTruncatedError())
	}
	if callback.AgentCompletionMarker != "" {
		if !toolObserved && (upstreamStopReason != "end_turn" || !agentVisibleTextObserved) {
			return reportKiroStreamError(callback, newUpstreamAgentMarkerMismatchError())
		}
	}

	return nil
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int) {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)

	inputTokens := currentInputTokens
	outputTokens := currentOutputTokens

	for _, usage := range candidates {
		if usage == nil {
			continue
		}

		if v, ok := readTokenNumber(usage,
			"outputTokens", "completionTokens", "totalOutputTokens",
			"output_tokens", "completion_tokens", "total_output_tokens",
		); ok {
			outputTokens = v
		}

		if v, ok := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		); ok {
			inputTokens = v
			continue
		}

		uncached, _ := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, _ := readTokenNumber(usage, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, _ := readTokenNumber(usage, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")
		if uncached+cacheRead+cacheWrite > 0 {
			inputTokens = uncached + cacheRead + cacheWrite
			continue
		}

		total, ok := readTokenNumber(usage, "totalTokens", "total_tokens")
		if ok && total > 0 {
			candidateOutput := outputTokens
			if v, vok := readTokenNumber(usage,
				"outputTokens", "completionTokens", "totalOutputTokens",
				"output_tokens", "completion_tokens", "total_output_tokens",
			); vok {
				candidateOutput = v
			}
			if total-candidateOutput > 0 {
				inputTokens = total - candidateOutput
			}
		}
	}

	return inputTokens, outputTokens
}

// getContextWindowSize returns the context window size (in tokens) for a model.
//
// Live ListAvailableModels token limits take precedence over the startup
// fallback. This value converts contextUsagePercentage into the absolute input
// count clients use to decide when to compact.
func getContextWindowSize(model string) int {
	return lookupModelTokenLimits(model).MaxInputTokens
}

// largeContextMinor matches "claude-<family>-<major>.<minor>" (dot or dash form)
// and is used to classify 1M-window models by version.
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)[.-](\d+)`)

func isLargeContextModel(model string) bool {
	m := strings.ToLower(model)
	for _, family := range []string{"claude-fable-5", "claude-mythos-5", "claude-sonnet-5", "claude-opus-5", "claude-5"} {
		if m == family || strings.HasPrefix(m, family+".") || strings.HasPrefix(m, family+"-") {
			return true
		}
	}
	if match := claudeVersionExtractor.FindStringSubmatch(m); match != nil {
		major, errMaj := strconv.Atoi(match[1])
		minor, errMin := strconv.Atoi(match[2])
		if errMaj == nil && errMin == nil {
			// 1M window for Claude >= 4.6 (4.6, 4.7, 4.8, ...) and any major >= 5.
			if major > 4 {
				return true
			}
			if major == 4 && minor >= 6 {
				return true
			}
			return false
		}
	}
	// Fallback substring checks for non-standard identifiers.
	for _, tag := range []string{"4.6", "4-6", "4.7", "4-7", "4.8", "4-8", "4.9", "4-9"} {
		if strings.Contains(m, tag) {
			return true
		}
	}
	return false
}

func collectUsageMaps(v interface{}, out *[]map[string]interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]interface{}); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

func readTokenNumber(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// ==================== Tool Use Handling ====================

type toolUseState struct {
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
	GeneratedID bool
	Streamed    bool
}

type toolUseAccumulator struct {
	states          map[string]*toolUseState
	totalInputBytes int
}

func newToolUseAccumulator() *toolUseAccumulator {
	return &toolUseAccumulator{states: make(map[string]*toolUseState)}
}

func (a *toolUseAccumulator) push(event map[string]interface{}, callback *KiroStreamCallback) error {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	resolvedID, err := a.resolveID(toolUseID, name)
	if err != nil {
		return err
	}
	state := a.states[resolvedID]
	if state == nil {
		if name == "" {
			return fmt.Errorf("tool use %q is missing a name", resolvedID)
		}
		if len(a.states) >= maxKiroActiveToolUses {
			return fmt.Errorf("too many active tool uses: limit is %d", maxKiroActiveToolUses)
		}
		state = &toolUseState{
			ToolUseID:   resolvedID,
			Name:        name,
			GeneratedID: toolUseID == "",
		}
		a.states[resolvedID] = state
	} else if state.Name == "" {
		state.Name = name
	} else if name != "" && state.Name != name {
		return fmt.Errorf("tool use %q changed name from %q to %q", resolvedID, state.Name, name)
	}

	inputDelta := ""
	if input, exists := event["input"]; exists && input != nil {
		switch value := input.(type) {
		case string:
			if state.InputBuffer.Len()+len(value) > maxKiroToolInputBytes {
				return fmt.Errorf("tool use %q input exceeds %d bytes", resolvedID, maxKiroToolInputBytes)
			}
			if a.totalInputBytes+len(value) > maxKiroToolAggregateBytes {
				return fmt.Errorf("active tool inputs exceed %d bytes", maxKiroToolAggregateBytes)
			}
			state.InputBuffer.WriteString(value)
			a.totalInputBytes += len(value)
			inputDelta = value
		case map[string]interface{}:
			data, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("marshal tool use %q input: %w", resolvedID, err)
			}
			if len(data) > maxKiroToolInputBytes {
				return fmt.Errorf("tool use %q input exceeds %d bytes", resolvedID, maxKiroToolInputBytes)
			}
			nextTotal := a.totalInputBytes - state.InputBuffer.Len() + len(data)
			if nextTotal > maxKiroToolAggregateBytes {
				return fmt.Errorf("active tool inputs exceed %d bytes", maxKiroToolAggregateBytes)
			}
			if state.Streamed && state.InputBuffer.Len() > 0 {
				return fmt.Errorf("tool use %q replaced input after streaming began", state.ToolUseID)
			}
			state.InputBuffer.Reset()
			state.InputBuffer.Write(data)
			a.totalInputBytes = nextTotal
			inputDelta = string(data)
		default:
			return fmt.Errorf("tool use %q input has unsupported type %T", resolvedID, input)
		}
	}

	if !isStop {
		emitToolUseFragment(state, inputDelta, callback)
		return nil
	}

	toolUse, err := completeToolUse(state)
	if err != nil {
		return err
	}
	emitToolUseFragment(state, inputDelta, callback)
	delete(a.states, resolvedID)
	a.totalInputBytes -= state.InputBuffer.Len()
	if callback != nil && callback.OnToolUse != nil {
		callback.OnToolUse(toolUse)
	}
	return nil
}

func emitToolUseFragment(state *toolUseState, inputDelta string, callback *KiroStreamCallback) {
	if state == nil || callback == nil {
		return
	}
	if !state.Streamed && callback.OnToolUseStart != nil {
		callback.OnToolUseStart(state.ToolUseID, state.Name)
	}
	if callback.OnToolUseStart != nil || callback.OnToolUseInputDelta != nil {
		state.Streamed = true
	}
	if inputDelta != "" && callback.OnToolUseInputDelta != nil {
		callback.OnToolUseInputDelta(state.ToolUseID, inputDelta)
	}
}

func (a *toolUseAccumulator) resolveID(toolUseID, name string) (string, error) {
	if toolUseID != "" {
		if _, exists := a.states[toolUseID]; exists {
			return toolUseID, nil
		}

		// Retain compatibility with streams whose first fragment omits the ID:
		// migrate the sole generated state with the same tool name.
		generatedID := ""
		for id, state := range a.states {
			if state.GeneratedID && name != "" && state.Name == name {
				if generatedID != "" {
					generatedID = ""
					break
				}
				generatedID = id
			}
		}
		if generatedID != "" {
			state := a.states[generatedID]
			delete(a.states, generatedID)
			if !state.Streamed {
				state.ToolUseID = toolUseID
			}
			state.GeneratedID = false
			a.states[toolUseID] = state
		}
		return toolUseID, nil
	}

	// Missing IDs are accepted for compatibility only when the fragment can be
	// associated unambiguously. All internal state is still keyed by an ID.
	candidateID := ""
	for id, state := range a.states {
		if name != "" && state.Name != name {
			continue
		}
		if candidateID != "" {
			return "", fmt.Errorf("tool use fragment without toolUseId is ambiguous")
		}
		candidateID = id
	}
	if candidateID != "" {
		return candidateID, nil
	}
	if name == "" {
		return "", fmt.Errorf("tool use fragment is missing toolUseId and name")
	}
	return "toolu_" + uuid.New().String(), nil
}

func (a *toolUseAccumulator) finish(callback *KiroStreamCallback) error {
	if len(a.states) == 0 {
		return nil
	}

	// Some Kiro endpoints omit the final stop=true fragment even though the
	// accumulated tool input is already a complete JSON object. Accept that
	// wire variant, but continue rejecting truncated or otherwise invalid JSON.
	completed := make([]KiroToolUse, 0, len(a.states))
	for _, state := range a.states {
		toolUse, err := completeToolUse(state)
		if err != nil {
			return fmt.Errorf(
				"incomplete tool use %q (%s): stream ended before complete JSON after %d input bytes: %w",
				state.ToolUseID,
				state.Name,
				state.InputBuffer.Len(),
				err,
			)
		}
		completed = append(completed, toolUse)
	}
	for _, toolUse := range completed {
		if callback != nil && callback.OnToolUse != nil {
			callback.OnToolUse(toolUse)
		}
	}
	clear(a.states)
	a.totalInputBytes = 0
	return nil
}

func completeToolUse(state *toolUseState) (KiroToolUse, error) {
	if state == nil || state.Name == "" {
		return KiroToolUse{}, fmt.Errorf("cannot complete tool use without a name")
	}

	input := make(map[string]interface{})
	if strings.TrimSpace(state.InputBuffer.String()) != "" {
		decoder := json.NewDecoder(strings.NewReader(state.InputBuffer.String()))
		decoder.UseNumber()
		if err := decoder.Decode(&input); err != nil {
			return KiroToolUse{}, fmt.Errorf(
				"invalid JSON for tool use %q (%s): %w",
				state.ToolUseID,
				state.Name,
				err,
			)
		}
		var trailing interface{}
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				err = fmt.Errorf("multiple JSON values")
			}
			return KiroToolUse{}, fmt.Errorf("invalid JSON for tool use %q (%s): %w", state.ToolUseID, state.Name, err)
		}
		if input == nil {
			return KiroToolUse{}, fmt.Errorf("invalid JSON for tool use %q (%s): expected an object", state.ToolUseID, state.Name)
		}
	}

	return KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	}, nil
}

func handleToolUseEvent(event map[string]interface{}, current *toolUseState, callback *KiroStreamCallback) *toolUseState {
	accumulator := newToolUseAccumulator()
	if current != nil {
		accumulator.states[current.ToolUseID] = current
		accumulator.totalInputBytes = current.InputBuffer.Len()
	}
	if err := accumulator.push(event, callback); err != nil {
		if callback != nil && callback.OnError != nil {
			callback.OnError(err)
		}
		return current
	}
	for _, state := range accumulator.states {
		return state
	}
	return nil
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) {
	if state == nil || callback == nil {
		return
	}
	toolUse, err := completeToolUse(state)
	if err != nil {
		if callback.OnError != nil {
			callback.OnError(err)
		}
		return
	}
	if callback.OnToolUse != nil {
		callback.OnToolUse(toolUse)
	}
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstBoolField(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if v, ok := m[key].(bool); ok {
			return v
		}
	}
	return false
}

// extractEventStreamStringHeaders extracts string-valued AWS Event Stream
// headers. The protocol uses these for event, error, and exception metadata.
func extractEventStreamStringHeaders(headers []byte) (map[string]string, error) {
	result := make(map[string]string)
	offset := 0
	for offset < len(headers) {
		nameLen := int(headers[offset])
		offset++
		if offset+nameLen > len(headers) {
			return nil, fmt.Errorf("truncated header name")
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			return nil, fmt.Errorf("header %q is missing a value type", name)
		}
		valueType := headers[offset]
		offset++

		if valueType == 7 { // String
			if offset+2 > len(headers) {
				return nil, fmt.Errorf("header %q has a truncated string length", name)
			}
			valueLen := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2
			if offset+valueLen > len(headers) {
				return nil, fmt.Errorf("header %q has a truncated string value", name)
			}
			value := string(headers[offset : offset+valueLen])
			offset += valueLen
			result[name] = value
			continue
		}

		// Skip other value types by their fixed byte widths.
		skipSizes := map[byte]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 4, 5: 8, 8: 8, 9: 16}
		if valueType == 6 {
			if offset+2 > len(headers) {
				return nil, fmt.Errorf("header %q has a truncated byte-array length", name)
			}
			l := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2
			if offset+l > len(headers) {
				return nil, fmt.Errorf("header %q has a truncated byte-array value", name)
			}
			offset += l
		} else if skip, ok := skipSizes[valueType]; ok {
			if offset+skip > len(headers) {
				return nil, fmt.Errorf("header %q has a truncated value", name)
			}
			offset += skip
		} else {
			return nil, fmt.Errorf("header %q has unsupported value type %d", name, valueType)
		}
	}
	return result, nil
}

// extractEventType extracts the event type string from AWS Event Stream message headers.
func extractEventType(headers []byte) string {
	parsed, _ := extractEventStreamStringHeaders(headers)
	return parsed[":event-type"]
}

func kiroStopReasonFromEvent(headers map[string]string, eventType string) string {
	parts := []string{eventType}
	for _, key := range []string{":exception-type", ":error-code"} {
		parts = append(parts, headers[key])
	}
	combined := strings.ToLower(strings.Join(parts, " "))
	switch {
	case strings.Contains(combined, "contentlengthexceeded"):
		return "max_tokens"
	case strings.Contains(combined, "contextwindowexceeded"), strings.Contains(combined, "contextlengthexceeded"):
		return "model_context_window_exceeded"
	default:
		return ""
	}
}

func normalizeKiroStopReason(reason string) string {
	reason = strings.TrimSpace(reason)
	switch strings.ToUpper(reason) {
	case "END_TURN":
		return "end_turn"
	case "TOOL_USE":
		return "tool_use"
	case "MAX_TOKENS":
		return "max_tokens"
	case "MODEL_CONTEXT_WINDOW_EXCEEDED", "CONTEXT_WINDOW_EXCEEDED":
		return "model_context_window_exceeded"
	default:
		return reason
	}
}

func isKiroLengthStopReason(reason string) bool {
	switch normalizeKiroStopReason(reason) {
	case "max_tokens", "model_context_window_exceeded":
		return true
	default:
		return false
	}
}

func newKiroEventStreamError(messageType string, headers map[string]string, payload []byte) error {
	errorType := headers[":error-code"]
	if messageType == "exception" {
		errorType = headers[":exception-type"]
	}
	if errorType == "" {
		errorType = "unknown"
	}

	message := ""
	var body map[string]interface{}
	if json.Unmarshal(payload, &body) == nil {
		if parsed := firstStringField(body, "message", "errorMessage", "error_message", "Message"); parsed != "" {
			message = parsed
		}
	}
	if message == "" {
		message = strings.TrimSpace(string(payload))
	}
	if message == "" {
		message = strings.TrimSpace(headers[":error-message"])
	}
	if message == "" {
		message = "no error message"
	}
	return fmt.Errorf("Kiro stream %s %s: %s", messageType, errorType, message)
}

func reportKiroStreamError(callback *KiroStreamCallback, err error) error {
	if callback != nil && callback.OnError != nil {
		callback.OnError(err)
	}
	return err
}
