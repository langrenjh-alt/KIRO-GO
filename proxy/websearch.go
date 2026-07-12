package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	webSearchQueryPrefix       = "Perform a web search for the query: "
	webSearchToolName          = "web_search"
	webSearchMCPBaseURL        = "https://q.us-east-1.amazonaws.com/mcp"
	webSearchMCPMaxResponse    = int64(4 << 20)
	webSearchSummaryChunkRunes = 100
	webSearchMaxQueryRunes     = 2048
	webSearchMaxUpstreamRunes  = 4096
	webSearchMaxDomains        = 20
	webSearchMaxDomainRunes    = 253
	webSearchMaxResults        = 20
	webSearchMaxTitleRunes     = 512
	webSearchMaxSnippetRunes   = 2048
	webSearchMaxURLBytes       = 4096
)

var supportedWebSearchToolTypes = map[string]struct{}{
	"web_search_20250305": {},
	"web_search_20260209": {},
	"web_search_20260318": {},
}

// webSearchMCPError preserves the upstream status so the HTTP handler can map
// authentication, rate-limit, and provider failures without parsing strings.
type webSearchMCPError struct {
	StatusCode int
	RPCCode    *int
	RetryAfter string
	Message    string
	Err        error
}

func (e *webSearchMCPError) Error() string {
	if e == nil {
		return "web search MCP error"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.StatusCode == http.StatusPaymentRequired {
		return "web search MCP quota exceeded (HTTP 402)"
	}
	return "web search MCP request failed"
}

func (e *webSearchMCPError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type webSearchMCPRequest struct {
	JSONRPC string                    `json:"jsonrpc"`
	ID      string                    `json:"id"`
	Method  string                    `json:"method"`
	Params  webSearchMCPRequestParams `json:"params"`
}

type webSearchMCPRequestParams struct {
	Name      string                       `json:"name"`
	Arguments webSearchMCPRequestArguments `json:"arguments"`
}

type webSearchMCPRequestArguments struct {
	Query string `json:"query"`
}

type webSearchMCPResponse struct {
	JSONRPC string                      `json:"jsonrpc"`
	ID      json.RawMessage             `json:"id"`
	Result  *webSearchMCPResponseResult `json:"result"`
	Error   *webSearchMCPResponseError  `json:"error"`
}

type webSearchMCPResponseError struct {
	Code    *int   `json:"code"`
	Message string `json:"message"`
}

type webSearchMCPResponseResult struct {
	Content []webSearchMCPContent `json:"content"`
	IsError bool                  `json:"isError"`
}

type webSearchMCPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type webSearchResultSet struct {
	ToolUseID    string
	Query        string
	Results      []webSearchResult
	TotalResults *int
}

type webSearchResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Snippet       *string `json:"snippet,omitempty"`
	PublishedDate *int64  `json:"publishedDate,omitempty"`
	ID            *string `json:"id,omitempty"`
	Domain        *string `json:"domain,omitempty"`
}

type webSearchMCPCallOptions struct {
	Endpoint         string
	Client           *http.Client
	MaxResponseBytes int64
}

func isNativeWebSearchTool(tool ClaudeTool) bool {
	if strings.TrimSpace(tool.Name) != webSearchToolName {
		return false
	}
	_, ok := supportedWebSearchToolTypes[strings.TrimSpace(tool.Type)]
	return ok
}

func isNativeWebSearchDeclaration(tool ClaudeTool) bool {
	return strings.TrimSpace(tool.Name) == webSearchToolName &&
		strings.HasPrefix(strings.TrimSpace(tool.Type), "web_search_")
}

// hasWebSearchTool identifies the dedicated fast path. Mixed tool requests are
// intentionally left to the normal Messages flow.
func hasWebSearchTool(req *ClaudeRequest) bool {
	return req != nil && len(req.Tools) == 1 && isNativeWebSearchTool(req.Tools[0])
}

func hasNativeWebSearchDeclaration(req *ClaudeRequest) bool {
	if req == nil {
		return false
	}
	for _, tool := range req.Tools {
		if isNativeWebSearchDeclaration(tool) {
			return true
		}
	}
	return false
}

var errNoWebSearchAccounts = errors.New("no available accounts for web search")

type webSearchMCPCaller func(context.Context, *config.Account, string) (*webSearchResultSet, error)

func (h *Handler) handleWebSearchRequest(
	w http.ResponseWriter,
	ctx context.Context,
	req *ClaudeRequest,
	estimatedInputTokens int,
	apiKeyID string,
) {
	call := h.webSearchMCPCaller
	if call == nil {
		call = callMCPWebSearch
	}
	h.handleWebSearchRequestWithCaller(w, ctx, req, estimatedInputTokens, apiKeyID, call)
}

func (h *Handler) handleWebSearchRequestWithCaller(
	w http.ResponseWriter,
	ctx context.Context,
	req *ClaudeRequest,
	estimatedInputTokens int,
	apiKeyID string,
	call webSearchMCPCaller,
) {
	query, err := extractSearchQuery(req)
	if err != nil {
		h.recordFailureWithDetails("claude", req.Model, "", err)
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := validateWebSearchToolOptions(req.Tools[0]); err != nil {
		h.recordFailureWithDetails("claude", req.Model, "", err)
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	upstreamQuery := buildWebSearchMCPQuery(query, req.Tools[0])
	if utf8.RuneCountInString(upstreamQuery) > webSearchMaxUpstreamRunes {
		err := fmt.Errorf("web search query and domain filters exceed %d characters", webSearchMaxUpstreamRunes)
		h.recordFailureWithDetails("claude", req.Model, "", err)
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if req.Stream {
		if _, ok := w.(http.Flusher); !ok {
			err := errors.New("streaming web search is not supported by the response writer")
			h.recordFailureWithDetails("claude", req.Model, "", err)
			h.sendClaudeError(w, http.StatusInternalServerError, "api_error", "Streaming not supported")
			return
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if call == nil {
		call = callMCPWebSearch
	}

	requestStarted := time.Now()
	excluded := make(map[string]bool)
	var lastErr error
	lastAccountID := ""

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		if h.pool == nil {
			lastErr = errNoWebSearchAccounts
			break
		}

		account := h.pool.GetNextExcluding(excluded)
		if account == nil {
			break
		}
		lastAccountID = account.ID

		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		if err := ensureRestProfileArn(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}

		result, err := call(ctx, account, upstreamQuery)
		if err != nil {
			lastErr = err
			if isWebSearchContextError(ctx, err) || !shouldRetryWebSearchFailure(err) {
				break
			}
			excluded[account.ID] = true
			h.handleWebSearchAccountFailure(account, err)
			continue
		}
		if result == nil {
			lastErr = newInvalidWebSearchMCPResultError("web search MCP returned no result")
			excluded[account.ID] = true
			h.handleWebSearchAccountFailure(account, lastErr)
			continue
		}
		result.Query = query
		result.Results = filterWebSearchResults(result.Results, req.Tools[0])

		outputTokens, err := writeWebSearchResponse(w, req, result, estimatedInputTokens)
		if err != nil {
			h.pool.RecordSuccess(account.ID)
			h.recordFailureWithDetails("claude", req.Model, account.ID, err)
			return
		}

		h.recordSuccessForApiKey(apiKeyID, estimatedInputTokens, outputTokens, 0)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, estimatedInputTokens+outputTokens, 0)
		h.recordSuccessLog("claude", req.Model, account.ID, estimatedInputTokens+outputTokens, 0, time.Since(requestStarted).Milliseconds())
		return
	}

	if lastErr == nil {
		lastErr = errNoWebSearchAccounts
	}
	h.recordFailureWithDetails("claude", req.Model, lastAccountID, lastErr)
	if isWebSearchContextError(ctx, lastErr) {
		return
	}

	status, errorType, message, retryAfter := mapWebSearchError(lastErr)
	if retryAfter = safeWebSearchRetryAfter(retryAfter); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	h.sendClaudeError(w, status, errorType, message)
}

func isWebSearchContextError(ctx context.Context, err error) bool {
	return (ctx != nil && ctx.Err() != nil) || errors.Is(err, context.Canceled)
}

func shouldRetryWebSearchFailure(err error) bool {
	if isWebSearchTimeout(err) {
		return false
	}
	var upstream *webSearchMCPError
	if !errors.As(err, &upstream) || upstream.StatusCode == 0 {
		return true
	}
	if upstream.StatusCode >= 400 && upstream.StatusCode < 500 {
		switch upstream.StatusCode {
		case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
			http.StatusRequestTimeout, http.StatusTooManyRequests:
			return true
		default:
			return false
		}
	}
	return true
}

func isWebSearchTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (h *Handler) handleWebSearchAccountFailure(account *config.Account, err error) {
	var upstream *webSearchMCPError
	if errors.As(err, &upstream) {
		switch upstream.StatusCode {
		case http.StatusPaymentRequired, http.StatusTooManyRequests:
			h.pool.RecordError(account.ID, true)
			return
		case http.StatusForbidden:
			// MCP 403 can mean WebSearch entitlement, profile, or regional policy.
			// It is not sufficient evidence that the account token is invalid.
			h.pool.RecordError(account.ID, false)
			return
		}
	}
	h.handleAccountFailure(account, err)
}

func mapWebSearchError(err error) (int, string, string, string) {
	if errors.Is(err, errNoWebSearchAccounts) {
		return http.StatusServiceUnavailable, "api_error", "No available accounts", ""
	}

	var upstream *webSearchMCPError
	if !errors.As(err, &upstream) {
		return http.StatusBadGateway, "api_error", "Web search upstream request failed", ""
	}

	switch upstream.StatusCode {
	case http.StatusBadRequest:
		return http.StatusBadRequest, "invalid_request_error", "Web search request was rejected by the upstream service", ""
	case http.StatusPaymentRequired, http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "rate_limit_error", "Web search rate limit exceeded", upstream.RetryAfter
	case http.StatusUnauthorized, http.StatusForbidden:
		return http.StatusBadGateway, "api_error", "Web search upstream authentication failed", ""
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return http.StatusGatewayTimeout, "api_error", "Web search upstream request timed out", ""
	default:
		return http.StatusBadGateway, "api_error", "Web search upstream request failed", ""
	}
}

func safeWebSearchRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

// extractSearchQuery reads only the latest user turn. Claude Code may include
// earlier turns in the nested request, so using messages[0] can search for a
// stale query.
func extractSearchQuery(req *ClaudeRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("web search request is nil")
	}

	for i := len(req.Messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(req.Messages[i].Role), "user") {
			continue
		}

		text := strings.TrimSpace(extractWebSearchText(req.Messages[i].Content))
		if !strings.HasPrefix(text, webSearchQueryPrefix) {
			return "", fmt.Errorf("native web search is supported only for Claude Code WebSearch requests")
		}
		text = strings.TrimSpace(strings.TrimPrefix(text, webSearchQueryPrefix))
		if text == "" {
			return "", fmt.Errorf("latest user message does not contain a web search query")
		}
		if len(text) > webSearchMaxQueryRunes*utf8.UTFMax || utf8.RuneCountInString(text) > webSearchMaxQueryRunes {
			return "", fmt.Errorf("web search query exceeds %d characters", webSearchMaxQueryRunes)
		}
		return text, nil
	}

	return "", fmt.Errorf("web search request does not contain a user message")
}

func extractWebSearchText(content interface{}) string {
	switch value := content.(type) {
	case string:
		return value
	case []interface{}:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			if blockType != "text" && blockType != "input_text" {
				continue
			}
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		return selectWebSearchText(parts)
	case []ClaudeContentBlock:
		parts := make([]string, 0, len(value))
		for _, block := range value {
			if (block.Type == "text" || block.Type == "input_text") && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		return selectWebSearchText(parts)
	case json.RawMessage:
		var decoded interface{}
		if json.Unmarshal(value, &decoded) == nil {
			return extractWebSearchText(decoded)
		}
	}
	return ""
}

func selectWebSearchText(parts []string) string {
	for _, part := range parts {
		if strings.HasPrefix(strings.TrimSpace(part), webSearchQueryPrefix) {
			return part
		}
	}
	return strings.Join(parts, "\n")
}

func validateWebSearchToolOptions(tool ClaudeTool) error {
	if len(tool.AllowedDomains) > webSearchMaxDomains || len(tool.BlockedDomains) > webSearchMaxDomains {
		return fmt.Errorf("web search domain filters support at most %d entries", webSearchMaxDomains)
	}
	for _, domains := range [][]string{tool.AllowedDomains, tool.BlockedDomains} {
		for _, domain := range domains {
			domain = strings.TrimSpace(domain)
			if len(domain) > webSearchMaxDomainRunes*utf8.UTFMax || utf8.RuneCountInString(domain) > webSearchMaxDomainRunes || normalizeWebSearchDomain(domain) == "" {
				return fmt.Errorf("web search contains an invalid domain filter")
			}
		}
	}
	return nil
}

func buildWebSearchMCPQuery(query string, tool ClaudeTool) string {
	allowed := normalizeWebSearchDomains(tool.AllowedDomains)
	blocked := normalizeWebSearchDomains(tool.BlockedDomains)
	parts := make([]string, 0, 2+len(blocked))
	if len(allowed) > 0 {
		sites := make([]string, 0, len(allowed))
		for _, domain := range allowed {
			sites = append(sites, "site:"+domain)
		}
		parts = append(parts, "("+strings.Join(sites, " OR ")+")")
	}
	parts = append(parts, query)
	for _, domain := range blocked {
		parts = append(parts, "-site:"+domain)
	}
	return strings.Join(parts, " ")
}

func filterWebSearchResults(results []webSearchResult, tool ClaudeTool) []webSearchResult {
	allowed := normalizeWebSearchDomains(tool.AllowedDomains)
	blocked := normalizeWebSearchDomains(tool.BlockedDomains)
	filtered := make([]webSearchResult, 0, minInt(len(results), webSearchMaxResults))
	for _, result := range results {
		if len(filtered) >= webSearchMaxResults {
			break
		}
		result.URL = strings.TrimSpace(result.URL)
		if result.URL == "" || len(result.URL) > webSearchMaxURLBytes {
			continue
		}
		parsed, err := url.Parse(result.URL)
		if err != nil {
			continue
		}
		scheme := strings.ToLower(parsed.Scheme)
		host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
		if (scheme != "http" && scheme != "https") || parsed.User != nil || host == "" || webSearchHostMatchesAny(host, blocked) {
			continue
		}
		if len(allowed) > 0 && !webSearchHostMatchesAny(host, allowed) {
			continue
		}
		result.Title = truncateWebSearchSnippet(strings.TrimSpace(result.Title), webSearchMaxTitleRunes)
		if result.Title == "" {
			result.Title = host
		}
		if result.Snippet != nil {
			snippet := truncateWebSearchSnippet(strings.TrimSpace(*result.Snippet), webSearchMaxSnippetRunes)
			result.Snippet = &snippet
		}
		filtered = append(filtered, result)
	}
	return filtered
}

func normalizeWebSearchDomains(domains []string) []string {
	normalized := make([]string, 0, len(domains))
	for _, domain := range domains {
		if host := normalizeWebSearchDomain(domain); host != "" {
			normalized = append(normalized, host)
		}
	}
	return normalized
}

func normalizeWebSearchDomain(domain string) string {
	value := strings.TrimSpace(domain)
	if value == "" {
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "//" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	host = strings.TrimPrefix(strings.TrimPrefix(host, "*."), ".")
	if host == "" || len(host) > webSearchMaxDomainRunes*utf8.UTFMax || utf8.RuneCountInString(host) > webSearchMaxDomainRunes {
		return ""
	}
	for _, char := range host {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '.' {
			continue
		}
		return ""
	}
	return host
}

func webSearchHostMatchesAny(host string, domains []string) bool {
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func newWebSearchMCPRequest(query string) (string, webSearchMCPRequest) {
	randomID := strings.ReplaceAll(uuid.NewString(), "-", "")
	toolUseID := "srvtoolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	requestID := fmt.Sprintf(
		"web_search_tooluse_%s_%d_%s",
		randomID[:22],
		time.Now().UnixMilli(),
		randomID[24:],
	)

	return toolUseID, webSearchMCPRequest{
		JSONRPC: "2.0",
		ID:      requestID,
		Method:  "tools/call",
		Params: webSearchMCPRequestParams{
			Name:      webSearchToolName,
			Arguments: webSearchMCPRequestArguments{Query: query},
		},
	}
}

func callMCPWebSearch(ctx context.Context, account *config.Account, query string) (*webSearchResultSet, error) {
	return callMCPWebSearchWithOptions(ctx, account, query, webSearchMCPCallOptions{})
}

func callMCPWebSearchWithOptions(
	ctx context.Context,
	account *config.Account,
	query string,
	options webSearchMCPCallOptions,
) (*webSearchResultSet, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("web search query is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	endpoint := strings.TrimSpace(options.Endpoint)
	if endpoint == "" {
		endpoint = regionalizeURL(webSearchMCPBaseURL, account)
	}
	client := options.Client
	if client == nil {
		client = GetRestClientForProxy(ResolveAccountProxyURL(account))
	}
	maxResponseBytes := options.MaxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = webSearchMCPMaxResponse
	}

	toolUseID, payload := newWebSearchMCPRequest(query)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode web search MCP request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create web search MCP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	applyKiroBaseHeaders(req, account, buildStreamingHeaderValues(account, req.URL.Host))
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.NewString())
	if account != nil && strings.TrimSpace(account.ProfileArn) != "" {
		req.Header.Set("x-amzn-kiro-profile-arn", strings.TrimSpace(account.ProfileArn))
	}

	resp, err := client.Do(req)
	if err != nil {
		upstreamErr := &webSearchMCPError{Err: err}
		if isWebSearchTimeout(err) {
			upstreamErr.StatusCode = http.StatusGatewayTimeout
			upstreamErr.Message = "web search MCP request timed out"
		}
		return nil, upstreamErr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		message := fmt.Sprintf("web search MCP upstream returned HTTP %d", resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusPaymentRequired:
			message = "web search MCP quota exceeded (HTTP 402)"
		case http.StatusForbidden:
			message = "web search MCP upstream denied web search access"
		}
		return nil, &webSearchMCPError{
			StatusCode: resp.StatusCode,
			RetryAfter: strings.TrimSpace(resp.Header.Get("Retry-After")),
			Message:    message,
		}
	}

	responseBody, err := readWebSearchMCPBody(resp.Body, maxResponseBytes)
	if err != nil {
		return nil, &webSearchMCPError{
			StatusCode: http.StatusBadGateway,
			Message:    "web search MCP response could not be read",
			Err:        err,
		}
	}
	var mcpResponse webSearchMCPResponse
	if err := json.Unmarshal(responseBody, &mcpResponse); err != nil {
		return nil, &webSearchMCPError{
			StatusCode: http.StatusBadGateway,
			Message:    "web search MCP upstream returned invalid JSON",
			Err:        err,
		}
	}
	if mcpResponse.Error != nil {
		return nil, &webSearchMCPError{
			StatusCode: http.StatusBadGateway,
			RPCCode:    mcpResponse.Error.Code,
			Message:    "web search MCP upstream returned a JSON-RPC error",
		}
	}

	result, err := parseWebSearchMCPResult(mcpResponse.Result)
	if err != nil {
		return nil, err
	}
	result.ToolUseID = toolUseID
	result.Query = query
	return result, nil
}

func readWebSearchMCPBody(reader io.Reader, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(reader, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("web search MCP response exceeds %d bytes", maxBytes)
	}
	return body, nil
}

func parseWebSearchMCPResult(result *webSearchMCPResponseResult) (*webSearchResultSet, error) {
	if result == nil {
		return nil, newInvalidWebSearchMCPResultError("web search MCP response has no result")
	}
	if result.IsError {
		return nil, newInvalidWebSearchMCPResultError("web search MCP tool returned an error")
	}

	parsedAny := false
	combined := &webSearchResultSet{}
	for _, block := range result.Content {
		if block.Type != "text" || strings.TrimSpace(block.Text) == "" {
			continue
		}

		var envelope struct {
			Results      json.RawMessage `json:"results"`
			TotalResults *int            `json:"totalResults"`
			Error        string          `json:"error"`
		}
		if err := json.Unmarshal([]byte(block.Text), &envelope); err != nil || len(envelope.Results) == 0 {
			continue
		}
		if strings.TrimSpace(envelope.Error) != "" {
			return nil, newInvalidWebSearchMCPResultError("web search MCP result reported an error")
		}

		parsedAny = true
		if !bytes.Equal(bytes.TrimSpace(envelope.Results), []byte("null")) {
			var results []webSearchResult
			if err := json.Unmarshal(envelope.Results, &results); err != nil {
				return nil, newInvalidWebSearchMCPResultError("web search MCP result contains invalid results")
			}
			combined.Results = append(combined.Results, results...)
		}
		if envelope.TotalResults != nil {
			combined.TotalResults = envelope.TotalResults
		}
	}

	if !parsedAny {
		return nil, newInvalidWebSearchMCPResultError("web search MCP response has no parseable search content")
	}
	return combined, nil
}

func newInvalidWebSearchMCPResultError(message string) error {
	return &webSearchMCPError{StatusCode: http.StatusBadGateway, Message: message}
}

func writeWebSearchResponse(
	w http.ResponseWriter,
	req *ClaudeRequest,
	result *webSearchResultSet,
	estimatedInputTokens int,
) (int, error) {
	if w == nil {
		return 0, fmt.Errorf("web search response writer is nil")
	}
	if req == nil || result == nil {
		return 0, fmt.Errorf("web search response is missing request or result")
	}
	if strings.TrimSpace(result.Query) == "" || strings.TrimSpace(result.ToolUseID) == "" {
		return 0, fmt.Errorf("web search response is missing query or tool use ID")
	}

	inputTokens := estimatedInputTokens
	if inputTokens < 1 {
		inputTokens = 1
	}
	content, summary, outputTokens := buildWebSearchContent(result)

	if !req.Stream {
		body := map[string]interface{}{
			"id":            newWebSearchMessageID(),
			"type":          "message",
			"role":          "assistant",
			"content":       content,
			"model":         req.Model,
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         buildWebSearchUsage(inputTokens, outputTokens),
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode web search response: %w", err)
		}
		encoded = append(encoded, '\n')
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if _, err := w.Write(encoded); err != nil {
			return 0, err
		}
		return outputTokens, nil
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		return 0, fmt.Errorf("streaming web search response is not supported by the response writer")
	}
	streamBody, err := renderWebSearchSSE(req.Model, result, content, summary, inputTokens, outputTokens)
	if err != nil {
		return 0, err
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if _, err := w.Write(streamBody); err != nil {
		return 0, err
	}
	flusher.Flush()
	return outputTokens, nil
}

func buildWebSearchContent(result *webSearchResultSet) ([]interface{}, string, int) {
	decision := fmt.Sprintf("I'll search for %q.", result.Query)
	summary := generateWebSearchSummary(result)
	searchResults := buildWebSearchResultContent(result.Results)
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": decision},
		map[string]interface{}{
			"id":     result.ToolUseID,
			"type":   "server_tool_use",
			"name":   webSearchToolName,
			"input":  map[string]interface{}{"query": result.Query},
			"caller": map[string]interface{}{"type": "direct"},
		},
		map[string]interface{}{
			"type":        "web_search_tool_result",
			"tool_use_id": result.ToolUseID,
			"content":     searchResults,
			"caller":      map[string]interface{}{"type": "direct"},
		},
		map[string]interface{}{"type": "text", "text": summary},
	}

	resultJSON, _ := json.Marshal(searchResults)
	outputTokens := estimateApproxTokens(decision) + estimateApproxTokens(summary) + estimateApproxTokens(string(resultJSON))
	if outputTokens < 1 {
		outputTokens = 1
	}
	return content, summary, outputTokens
}

func buildWebSearchResultContent(results []webSearchResult) []map[string]interface{} {
	content := make([]map[string]interface{}, 0, len(results))
	for _, result := range results {
		snippet := ""
		if result.Snippet != nil {
			snippet = *result.Snippet
		}
		var pageAge interface{}
		if result.PublishedDate != nil {
			pageAge = time.UnixMilli(*result.PublishedDate).UTC().Format("January 2, 2006")
		}
		content = append(content, map[string]interface{}{
			"type":              "web_search_result",
			"title":             result.Title,
			"url":               result.URL,
			"encrypted_content": snippet,
			"page_age":          pageAge,
		})
	}
	return content
}

func generateWebSearchSummary(result *webSearchResultSet) string {
	var summary strings.Builder
	fmt.Fprintf(&summary, "Here are the search results for %q:\n\n", result.Query)
	if len(result.Results) == 0 {
		summary.WriteString("No results found.\n")
	} else {
		for i, searchResult := range result.Results {
			fmt.Fprintf(&summary, "%d. **%s**\n", i+1, searchResult.Title)
			if searchResult.Snippet != nil && strings.TrimSpace(*searchResult.Snippet) != "" {
				fmt.Fprintf(&summary, "   %s\n", truncateWebSearchSnippet(*searchResult.Snippet, 200))
			}
			fmt.Fprintf(&summary, "   Source: %s\n\n", searchResult.URL)
		}
	}
	summary.WriteString("\nPlease note that these are web search results and may not be fully accurate or up-to-date.")
	return summary.String()
}

func truncateWebSearchSnippet(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return value
	}
	count := 0
	for index := range value {
		if count == maxRunes {
			return value[:index] + "..."
		}
		count++
	}
	return value
}

func buildWebSearchUsage(inputTokens, outputTokens int) map[string]interface{} {
	return map[string]interface{}{
		"input_tokens":                inputTokens,
		"output_tokens":               outputTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
		"server_tool_use": map[string]interface{}{
			"web_search_requests": 1,
			"web_fetch_requests":  0,
		},
	}
}

func renderWebSearchSSE(
	model string,
	result *webSearchResultSet,
	content []interface{},
	summary string,
	inputTokens, outputTokens int,
) ([]byte, error) {
	var body bytes.Buffer
	messageStartUsage := buildWebSearchUsage(inputTokens, 0)
	delete(messageStartUsage, "server_tool_use")
	if err := appendWebSearchSSEEvent(&body, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            newWebSearchMessageID(),
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         messageStartUsage,
		},
	}); err != nil {
		return nil, err
	}

	decision, _ := content[0].(map[string]interface{})["text"].(string)
	if err := appendWebSearchTextBlock(&body, 0, []string{decision}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]interface{}{
			"id":     result.ToolUseID,
			"type":   "server_tool_use",
			"name":   webSearchToolName,
			"input":  map[string]interface{}{},
			"caller": map[string]interface{}{"type": "direct"},
		},
	}); err != nil {
		return nil, err
	}
	inputJSON, err := json.Marshal(map[string]interface{}{"query": result.Query})
	if err != nil {
		return nil, fmt.Errorf("encode web search tool input: %w", err)
	}
	if err := appendWebSearchSSEEvent(&body, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 1,
		"delta": map[string]interface{}{
			"type":         "input_json_delta",
			"partial_json": string(inputJSON),
		},
	}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": 1,
	}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         2,
		"content_block": content[2],
	}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": 2,
	}); err != nil {
		return nil, err
	}
	if err := appendWebSearchTextBlock(&body, 3, chunkWebSearchText(summary, webSearchSummaryChunkRunes)); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn"},
		"usage": map[string]interface{}{
			"output_tokens": outputTokens,
			"server_tool_use": map[string]interface{}{
				"web_search_requests": 1,
				"web_fetch_requests":  0,
			},
		},
	}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "message_stop", map[string]interface{}{
		"type": "message_stop",
	}); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

func appendWebSearchTextBlock(body *bytes.Buffer, index int, chunks []string) error {
	if err := appendWebSearchSSEEvent(body, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]interface{}{
			"type": "text",
			"text": "",
		},
	}); err != nil {
		return err
	}
	for _, chunk := range chunks {
		if err := appendWebSearchSSEEvent(body, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]interface{}{
				"type": "text_delta",
				"text": chunk,
			},
		}); err != nil {
			return err
		}
	}
	return appendWebSearchSSEEvent(body, "content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": index,
	})
}

func appendWebSearchSSEEvent(body *bytes.Buffer, event string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode web search SSE event %s: %w", event, err)
	}
	fmt.Fprintf(body, "event: %s\ndata: %s\n\n", event, data)
	return nil
}

func chunkWebSearchText(value string, maxRunes int) []string {
	if maxRunes <= 0 {
		return []string{value}
	}
	runes := []rune(value)
	if len(runes) == 0 {
		return []string{""}
	}
	chunks := make([]string, 0, (len(runes)+maxRunes-1)/maxRunes)
	for start := 0; start < len(runes); start += maxRunes {
		end := start + maxRunes
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks
}

func newWebSearchMessageID() string {
	raw := strings.ReplaceAll(uuid.NewString(), "-", "")
	return "msg_" + raw[:24]
}
