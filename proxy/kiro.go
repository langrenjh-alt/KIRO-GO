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
	maxKiroEventStreamFrameBytes = 16 << 20
	maxKiroToolInputBytes        = 8 << 20
	maxKiroToolAggregateBytes    = 16 << 20
	maxKiroActiveToolUses        = 128
	maxKiroToolJSONRetries       = 2
	maxKiroPreResponseRetries    = 8
)

// Kiro streaming endpoint. The us-east-1 host is rewritten to the account's
// effective API region immediately before each request.
type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

var kiroEndpoints = []kiroEndpoint{
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "",
		Name:      "Kiro IDE",
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
		Timeout:   5 * time.Minute,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(proxyURL, client)
	return client
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
	proxyClientCache.Store(cacheKey, client)
	return client
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
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
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
		Timeout:   5 * time.Minute,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroHttpStore.Store(client)

	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroRestHttpStore.Store(restClient)
}

// CloseKiroIdleConnections closes only idle upstream connections. Active
// requests continue; subsequent calls establish fresh direct/proxy tunnels.
func CloseKiroIdleConnections() {
	closeIdle := func(client *http.Client) {
		if client == nil || client.Transport == nil {
			return
		}
		if transport, ok := client.Transport.(interface{ CloseIdleConnections() }); ok {
			transport.CloseIdleConnections()
		}
	}
	closeIdle(kiroHttpStore.Load())
	closeIdle(kiroRestHttpStore.Load())
	proxyClientCache.Range(func(_, value interface{}) bool {
		if client, ok := value.(*http.Client); ok {
			closeIdle(client)
		}
		return true
	})
}

// ==================== Request Structs ====================

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
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
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
	OnText func(text string, isThinking bool)
	// OnReasoning receives native Kiro reasoning fields. When set, it replaces
	// the legacy OnText(text, true) callback for reasoning events.
	OnReasoning    func(event KiroReasoningContentEvent)
	OnToolUse      func(toolUse KiroToolUse)
	OnComplete     func(inputTokens, outputTokens int)
	OnError        func(err error)
	OnStopReason   func(reason string)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)
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

// isRetryableKiroStreamError identifies failures that are safe to retry only
// before any client-visible response has been emitted. Kiro can terminate an
// otherwise valid stream while an MCP/tool JSON fragment is being assembled,
// and network/proxy layers commonly surface the same transient failure as a
// context deadline or Client.Timeout. Callers must gate this helper with their
// response-started flag so a retry never duplicates an Anthropic/OpenAI stream.
func isRetryableKiroStreamError(err error) bool {
	if err == nil {
		return false
	}
	if code, ok := kiroStreamProtocolErrorCode(err); ok {
		return code == "upstream_tool_json_error"
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

// CallKiroAPI calls the single regional Kiro streaming endpoint. Legacy
// preferredEndpoint and endpointFallback settings are intentionally ignored.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() {
			payload.ProfileArn = originalProfileArn
		}()
	}
	setPayloadProfileArnForAccount(payload, account)

	if _, err := json.Marshal(payload); err != nil {
		return err
	}

	// Debug: dump full payload for troubleshooting upstream rejections
	if payloadJSON, err := json.Marshal(payload); err == nil {
		logger.Debugf("[KiroAPI] Request payload: %s", string(payloadJSON))
	}

	// Wrap OnToolUse to restore original tool names for the client.
	if callback != nil && callback.OnToolUse != nil && len(payload.ToolNameMap) > 0 {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	ep := kiroEndpoints[0]
	// Update the origin field for the selected endpoint.
	payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

	// Target the data-plane region via the unified regionalizeURLForProfile.
	// kiroRegionForProfile branches on IsApiKeyCredential: api_key accounts
	// resolve from EffectiveApiRegion (no profile ARN), OAuth accounts derive
	// from the profile ARN. regionalizeURLForRegion collapses both us-east-1
	// hosts (q. and codewhisperer.) onto q.<region> for non-us-east-1 — there
	// is no regional codewhisperer host — so this also routes the CodeWhisperer
	// endpoint correctly for regional api_key accounts.
	epURL := regionalizeURLForProfile(ep.URL, account, payload.ProfileArn)

	host := ""
	if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
		host = parsedURL.Host
	}
	headerValues := buildStreamingHeaderValues(account, host)
	reqBody, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return marshalErr
	}
	req, requestErr := http.NewRequest("POST", epURL, bytes.NewReader(reqBody))
	if requestErr != nil {
		return requestErr
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if ep.AmzTarget != "" {
		req.Header.Set("X-Amz-Target", ep.AmzTarget)
	}
	applyKiroBaseHeaders(req, account, headerValues)
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=1")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

	resp, doErr := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if doErr != nil {
		logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, doErr)
		return doErr
	}

	if resp.StatusCode == 429 {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return &UpstreamRateLimitError{Endpoint: ep.Name, Body: string(errBody)}
	}

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		err := fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, ep.Name, string(errBody))
		logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, err)
		return err
	}

	err := parseEventStream(resp.Body, callback)
	resp.Body.Close()
	return err
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
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
	var inputTokens, outputTokens int
	var totalCredits float64
	toolUses := newToolUseAccumulator()
	for {
		// Prelude: 12 bytes (total_len + headers_len + crc)
		prelude := make([]byte, 12)
		_, err := io.ReadFull(body, prelude)
		if err == io.EOF {
			break
		}
		if err != nil {
			return reportKiroStreamError(callback, fmt.Errorf("read Kiro event stream prelude: %w", err))
		}

		totalLength := int(binary.BigEndian.Uint32(prelude[0:4]))
		headersLength := int(binary.BigEndian.Uint32(prelude[4:8]))

		if totalLength < 16 || totalLength > maxKiroEventStreamFrameBytes {
			return reportKiroStreamError(callback, fmt.Errorf("invalid Kiro event stream frame length %d", totalLength))
		}
		if headersLength > totalLength-16 {
			return reportKiroStreamError(callback, fmt.Errorf(
				"invalid Kiro event stream headers length %d for frame length %d",
				headersLength,
				totalLength,
			))
		}
		wantPreludeCRC := binary.BigEndian.Uint32(prelude[8:12])
		if gotPreludeCRC := crc32.ChecksumIEEE(prelude[:8]); gotPreludeCRC != wantPreludeCRC {
			return reportKiroStreamError(callback, fmt.Errorf(
				"invalid Kiro event stream prelude CRC: got %08x, want %08x",
				gotPreludeCRC,
				wantPreludeCRC,
			))
		}

		// Read the remaining message bytes.
		remaining := totalLength - 12
		msgBuf := make([]byte, remaining)
		_, err = io.ReadFull(body, msgBuf)
		if err != nil {
			return reportKiroStreamError(callback, fmt.Errorf("read Kiro event stream frame: %w", err))
		}

		wantMessageCRC := binary.BigEndian.Uint32(msgBuf[len(msgBuf)-4:])
		messageCRC := crc32.NewIEEE()
		_, _ = messageCRC.Write(prelude)
		_, _ = messageCRC.Write(msgBuf[:len(msgBuf)-4])
		if gotMessageCRC := messageCRC.Sum32(); gotMessageCRC != wantMessageCRC {
			return reportKiroStreamError(callback, fmt.Errorf(
				"invalid Kiro event stream message CRC: got %08x, want %08x",
				gotMessageCRC,
				wantMessageCRC,
			))
		}

		headers, err := extractEventStreamStringHeaders(msgBuf[0:headersLength])
		if err != nil {
			return reportKiroStreamError(callback, fmt.Errorf("invalid Kiro event stream headers: %w", err))
		}
		eventType := headers[":event-type"]
		payloadBytes := msgBuf[headersLength : len(msgBuf)-4]

		messageType := headers[":message-type"]
		if stopReason := kiroStopReasonFromEvent(headers, eventType); stopReason != "" {
			if callback.OnStopReason != nil {
				callback.OnStopReason(stopReason)
			}
			break
		}
		if messageType == "error" || messageType == "exception" {
			streamErr := newKiroEventStreamError(messageType, headers, payloadBytes)
			return reportKiroStreamError(callback, streamErr)
		}
		if messageType != "" && messageType != "event" {
			return reportKiroStreamError(callback, fmt.Errorf("unsupported Kiro event stream message type %q", messageType))
		}
		if eventType == "" {
			return reportKiroStreamError(callback, fmt.Errorf("Kiro event stream frame is missing :event-type"))
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
			return reportKiroStreamError(callback, fmt.Errorf("invalid %s payload: %w", eventType, err))
		}
		var trailing interface{}
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				err = fmt.Errorf("multiple JSON values")
			}
			return reportKiroStreamError(callback, fmt.Errorf("invalid %s payload: %w", eventType, err))
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens)

		// Dispatch by event type.
		switch eventType {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				if callback.OnText != nil {
					callback.OnText(content, false)
				}
			}
		case "reasoningContentEvent":
			reasoning := KiroReasoningContentEvent{
				Signature:       firstStringField(event, "signature"),
				RedactedContent: firstStringField(event, "redactedContent", "redacted_content"),
			}
			if text, ok := event["text"].(string); ok && text != "" {
				reasoning.Text = text
			}
			if reasoning.Text != "" || reasoning.Signature != "" || reasoning.RedactedContent != "" {
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
		case "meteringEvent":
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
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

	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}

	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
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
// Per Kiro's ListAvailableModels, the 1M-token context window applies to
// Claude 4.6 and newer (sonnet-4.6, opus-4.6, opus-4.7, opus-4.8, and future
// 4.x releases), while 4.5 and earlier (opus-4.5, sonnet-4.5, sonnet-4,
// haiku-4.5) use a 200K window. This value is used to convert the upstream
// contextUsagePercentage into an absolute input-token count that clients rely
// on to decide when to compact; an undersized window under-reports tokens and
// prevents clients from compacting in time.
func getContextWindowSize(model string) int {
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
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
			state.InputBuffer.Reset()
			state.InputBuffer.Write(data)
			a.totalInputBytes = nextTotal
		default:
			return fmt.Errorf("tool use %q input has unsupported type %T", resolvedID, input)
		}
	}

	if !isStop {
		return nil
	}

	toolUse, err := completeToolUse(state)
	if err != nil {
		return err
	}
	delete(a.states, resolvedID)
	a.totalInputBytes -= state.InputBuffer.Len()
	if callback != nil && callback.OnToolUse != nil {
		callback.OnToolUse(toolUse)
	}
	return nil
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
			state.ToolUseID = toolUseID
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
