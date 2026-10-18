package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	webSearchQueryPrefix       = "Perform a web search for the query: "
	webSearchToolName          = "web_search"
	webSearchMCPBaseURL        = "https://q.us-east-1.amazonaws.com/mcp"
	webSearchMCPMaxResponse    = int64(4 << 20)
	webSearchMCPRetryMaxBytes  = 1536
	webSearchRPCAccountRetries = 2
	webSearchSummaryChunkRunes = 100
	webSearchMaxQueryRunes     = 2048
	// Kiro's web_search MCP tool rejects a longer query with JSON-RPC -32602
	// "Invalid tool parameters provided". Live check on 2026-09-26: 200
	// characters succeed and 201 fail.
	webSearchMaxUpstreamRunes = 200
	webSearchMaxDomains       = 20
	webSearchMaxDomainRunes   = 253
	webSearchMaxResults       = 20
	webSearchMaxTitleRunes    = 512
	webSearchMaxSnippetRunes  = 2048
	webSearchMaxURLBytes      = 4096
)

var supportedWebSearchToolTypes = map[string]struct{}{
	"web_search_20250305": {},
	"web_search_20260209": {},
	"web_search_20260318": {},
}

func isResponsesWebSearchTool(tool OpenAITool) bool {
	toolType := strings.ToLower(strings.TrimSpace(tool.Type))
	return toolType == "web_search" || strings.HasPrefix(toolType, "web_search_")
}

func findResponsesWebSearchTool(tools []OpenAITool) (*OpenAITool, bool) {
	for i := range tools {
		if isResponsesWebSearchTool(tools[i]) {
			return &tools[i], true
		}
	}
	return nil, false
}

// webSearchMCPError preserves the upstream status so the HTTP handler can map
// authentication, rate-limit, and provider failures without parsing strings.
type webSearchMCPError struct {
	StatusCode int
	RPCCode    *int
	RetryAfter string
	Body       string
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

const (
	jsonRPCParseError     = -32700
	jsonRPCInvalidRequest = -32600
	jsonRPCMethodNotFound = -32601
	jsonRPCInvalidParams  = -32602
)

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
	Unavailable  bool
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

func findNativeWebSearchTool(req *ClaudeRequest) (*ClaudeTool, bool) {
	if req == nil {
		return nil, false
	}
	for i := range req.Tools {
		if isNativeWebSearchTool(req.Tools[i]) {
			return &req.Tools[i], true
		}
	}
	return nil, false
}

// hasWebSearchTool identifies requests that contain a supported native search
// declaration. Claude Code may submit its built-in search alongside the rest
// of the session tool registry, so the declaration does not need to be the
// only tool in the request.
func hasWebSearchTool(req *ClaudeRequest) bool {
	_, ok := findNativeWebSearchTool(req)
	return ok
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

type webSearchExecution struct {
	Result        *webSearchResultSet
	Account       *config.Account
	LastAccountID string
	DegradedError error
}

func (h *Handler) handleWebSearchRequest(
	w http.ResponseWriter,
	ctx context.Context,
	req *ClaudeRequest,
	estimatedInputTokens int,
	apiKeyID string,
	cacheProfile *promptCacheProfile,
) {
	call := h.webSearchMCPCaller
	if call == nil {
		call = callMCPWebSearch
	}
	h.handleWebSearchRequestWithCallerAndCache(w, ctx, req, estimatedInputTokens, apiKeyID, cacheProfile, call)
}

func (h *Handler) handleWebSearchRequestWithCaller(
	w http.ResponseWriter,
	ctx context.Context,
	req *ClaudeRequest,
	estimatedInputTokens int,
	apiKeyID string,
	call webSearchMCPCaller,
) {
	h.handleWebSearchRequestWithCallerAndCache(w, ctx, req, estimatedInputTokens, apiKeyID, nil, call)
}

func (h *Handler) handleWebSearchRequestWithCallerAndCache(
	w http.ResponseWriter,
	ctx context.Context,
	req *ClaudeRequest,
	estimatedInputTokens int,
	apiKeyID string,
	cacheProfile *promptCacheProfile,
	call webSearchMCPCaller,
) {
	tool, ok := findNativeWebSearchTool(req)
	if !ok {
		err := errors.New("web search request does not contain a supported native tool")
		h.recordFailureWithDetails("claude", req.Model, "", err)
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	query, err := extractSearchQuery(req)
	if err != nil {
		h.recordFailureWithDetails("claude", req.Model, "", err)
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := validateWebSearchToolOptions(*tool); err != nil {
		h.recordFailureWithDetails("claude", req.Model, "", err)
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	upstreamQuery := buildWebSearchMCPQuery(query, *tool)
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
	execution, err := h.executeWebSearch(ctx, query, upstreamQuery, *tool, call)
	if err != nil {
		lastAccountID := ""
		if execution != nil {
			lastAccountID = execution.LastAccountID
		}
		h.recordFailureWithDetails("claude", req.Model, lastAccountID, err)
		if isWebSearchContextError(ctx, err) {
			return
		}

		status, errorType, message, retryAfter := mapWebSearchError(err)
		if retryAfter = safeWebSearchRetryAfter(retryAfter); retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		h.sendClaudeError(w, status, errorType, message)
		return
	}

	cacheTracker := promptCacheTrackerForProfile(h.promptCache, cacheProfile)
	cacheUsage := promptCacheUsage{}
	if execution.Account != nil {
		cacheUsage = cacheTracker.Compute(execution.Account.ID, cacheProfile)
		cacheUsage = reanchorClaudeCacheUsage(cacheProfile, cacheUsage, estimatedInputTokens)
	}
	outputTokens, err := writeWebSearchResponseWithCache(w, req, execution.Result, estimatedInputTokens, cacheUsage)
	if err != nil {
		if execution.Account != nil {
			h.pool.RecordSuccess(execution.Account.ID)
		}
		h.recordFailureWithDetails("claude", req.Model, execution.LastAccountID, err)
		return
	}
	if execution.DegradedError != nil {
		h.recordFailureWithDetails("claude", req.Model, execution.LastAccountID, execution.DegradedError)
		return
	}
	if execution.Account != nil {
		cacheTracker.Update(execution.Account.ID, cacheProfile)
	}

	h.recordWebSearchSuccess("claude", req.Model, apiKeyID, execution.Account, estimatedInputTokens, outputTokens, cacheUsage, requestStarted)
}

func (h *Handler) executeWebSearch(
	ctx context.Context,
	query string,
	upstreamQuery string,
	tool ClaudeTool,
	call webSearchMCPCaller,
) (*webSearchExecution, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if call == nil {
		call = callMCPWebSearch
	}

	execution := &webSearchExecution{}
	excluded := make(map[string]bool)
	retryCounts := make(map[string]int)
	forceRefreshed := make(map[string]bool)
	retrySameID := ""
	rpcRejections := 0
	var lastErr error

	for attempt := 0; attempt < h.maxAccountAttempts(); attempt++ {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		if h.pool == nil {
			lastErr = errNoWebSearchAccounts
			break
		}

		account, allFull := h.acquireAccountForModel("", excluded, retrySameID, ctx)
		retrySameID = ""
		if account == nil {
			// Preserve the useful JSON-RPC failure when no compatible alternate
			// account is available instead of replacing it with a generic pool error.
			if rpcRejections > 0 && lastErr != nil {
				break
			}
			if h.startExpiredAccountRefreshes("", excluded) {
				lastErr = errAccountRefreshInProgress
			} else if allFull {
				lastErr = fmt.Errorf(allAccountsConcurrencyFullMessage)
			}
			break
		}
		execution.LastAccountID = account.ID

		if err := h.ensureValidTokenContext(ctx, account); err != nil {
			h.releaseAccount(account)
			if ctx.Err() != nil {
				lastErr = ctx.Err()
				break
			}
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		if err := ctx.Err(); err != nil {
			h.releaseAccount(account)
			lastErr = err
			break
		}
		if err := ensureRestProfileArn(account); err != nil {
			h.releaseAccount(account)
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		if err := ctx.Err(); err != nil {
			h.releaseAccount(account)
			lastErr = err
			break
		}

		result, err := call(ctx, account, upstreamQuery)
		if err != nil && isWebSearchBearerTokenInvalidError(err) &&
			!account.IsApiKeyCredential() && !forceRefreshed[account.ID] {
			forceRefreshed[account.ID] = true
			if refreshErr := h.refreshAccountTokenContext(ctx, account, true); refreshErr == nil {
				result, err = call(ctx, account, upstreamQuery)
			}
		}
		h.releaseAccount(account)
		if err != nil {
			lastErr = err
			if isPermanentWebSearchRPCErrorCode(err) {
				rpcRejections++
				if rpcRejections < webSearchRPCAccountRetries {
					// Some profiles expose chat but not the regional MCP tool. Try one
					// alternate account without cooling down the account for normal chat.
					excluded[account.ID] = true
					continue
				}
				break
			}
			if isRateLimitError(err) {
				decision := h.applyRateLimitPolicy(account, retryCounts, excluded, err)
				if decision.Return429 {
					break
				}
				retrySameID = decision.RetrySameID
				if !waitForRateLimitRetry(ctx, decision) {
					lastErr = ctx.Err()
					break
				}
				continue
			}
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
		result.Results = filterWebSearchResults(result.Results, tool)
		execution.Result = result
		execution.Account = account
		return execution, nil
	}

	if lastErr == nil {
		lastErr = errNoWebSearchAccounts
	}
	if isPermanentWebSearchRPCErrorCode(lastErr) {
		// Kiro can reject WebSearch at the MCP layer for an otherwise valid chat
		// account or region. Preserve the native server-tool response contract so
		// the client can continue, while retaining the failure for metrics/logs.
		execution.Result = &webSearchResultSet{
			ToolUseID:   newClaudeServerToolUseID(),
			Query:       query,
			Unavailable: true,
		}
		execution.DegradedError = lastErr
		return execution, nil
	}
	return execution, lastErr
}

func (h *Handler) recordWebSearchSuccess(
	endpoint string,
	model string,
	apiKeyID string,
	account *config.Account,
	inputTokens int,
	outputTokens int,
	cacheUsage promptCacheUsage,
	started time.Time,
) {
	if account == nil {
		return
	}
	h.recordSuccessForApiKeyWithCache(apiKeyID, inputTokens, outputTokens, 0, cacheUsage)
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, 0)
	h.recordSuccessLog(endpoint, model, account.ID, inputTokens+outputTokens, 0, time.Since(started).Milliseconds())
}

func isWebSearchContextError(ctx context.Context, err error) bool {
	return (ctx != nil && ctx.Err() != nil) || errors.Is(err, context.Canceled)
}

func shouldRetryWebSearchFailure(err error) bool {
	if isWebSearchTimeout(err) {
		return false
	}
	var upstream *webSearchMCPError
	if !errors.As(err, &upstream) {
		return true
	}
	if isPermanentWebSearchRPCError(upstream.RPCCode) {
		return false
	}
	if upstream.StatusCode == 0 {
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

func isPermanentWebSearchRPCError(code *int) bool {
	if code == nil {
		return false
	}
	switch *code {
	case jsonRPCParseError, jsonRPCInvalidRequest, jsonRPCMethodNotFound, jsonRPCInvalidParams:
		return true
	default:
		return false
	}
}

func isPermanentWebSearchRPCErrorCode(err error) bool {
	var upstream *webSearchMCPError
	return errors.As(err, &upstream) && isPermanentWebSearchRPCError(upstream.RPCCode)
}

func isWebSearchTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isWebSearchBearerTokenInvalidError(err error) bool {
	var upstream *webSearchMCPError
	if !errors.As(err, &upstream) ||
		(upstream.StatusCode != http.StatusUnauthorized && upstream.StatusCode != http.StatusForbidden) {
		return false
	}
	return strings.Contains(upstream.Body, "The bearer token included in the request is invalid")
}

func isWebSearchMonthlyRequestCountError(err error) bool {
	var upstream *webSearchMCPError
	return errors.As(err, &upstream) &&
		upstream.StatusCode == http.StatusPaymentRequired &&
		isMonthlyRequestCountBody(upstream.Body)
}

func (h *Handler) handleWebSearchAccountFailure(account *config.Account, err error) {
	var upstream *webSearchMCPError
	if errors.As(err, &upstream) {
		switch upstream.StatusCode {
		case http.StatusPaymentRequired, http.StatusTooManyRequests:
			if isWebSearchMonthlyRequestCountError(err) {
				h.disableAccount(account, "DISABLED", "Monthly request quota exhausted (MONTHLY_REQUEST_COUNT)")
			}
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
	if isAccountRefreshInProgressError(err) {
		return accountRefreshInProgressStatus, "account_refreshing", accountRefreshInProgressMessage, fmt.Sprintf("%d", int(expiredAccountRefreshWait.Seconds()))
	}
	if isAllAccountsConcurrencyFullError(err) {
		return http.StatusServiceUnavailable, "overloaded_error", allAccountsConcurrencyFullMessage, ""
	}
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

		if isPureWebSearchToolResultContent(req.Messages[i].Content) {
			// A follow-up tool-result turn has no user query of its own. Keep
			// looking for the search request that produced it.
			continue
		}
		text := normalizeWebSearchQuery(extractWebSearchText(req.Messages[i].Content))
		if text == "" {
			if isWebSearchContinuationContent(req.Messages[i].Content) {
				// Tool-result turns can be appended after the user's search
				// request. They carry no query themselves, so continue to the
				// previous user turn instead of rejecting a valid search.
				continue
			}
			return "", fmt.Errorf("latest user message does not contain a web search query")
		}
		return text, nil
	}

	return "", fmt.Errorf("web search request does not contain a user message")
}

func isPureWebSearchToolResultContent(content interface{}) bool {
	switch value := content.(type) {
	case map[string]interface{}:
		typ, _ := value["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		return typ == "tool_result" || typ == "function_call_output" || typ == "function_call"
	case []interface{}:
		if len(value) == 0 {
			return false
		}
		for _, item := range value {
			if !isPureWebSearchToolResultContent(item) {
				return false
			}
		}
		return true
	case []ClaudeContentBlock:
		if len(value) == 0 {
			return false
		}
		for _, block := range value {
			if block.Type != "tool_result" && block.Type != "tool_use" {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func isWebSearchContinuationContent(content interface{}) bool {
	switch value := content.(type) {
	case []interface{}:
		for _, item := range value {
			if isWebSearchContinuationContent(item) {
				return true
			}
		}
	case []ClaudeContentBlock:
		for _, block := range value {
			if block.Type == "tool_result" || block.Type == "tool_use" {
				return true
			}
		}
	case map[string]interface{}:
		typ, _ := value["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "tool_result", "tool_use", "function_call_output", "function_call":
			return true
		}
	}
	return false
}

func normalizeWebSearchQuery(value string) string {
	value = strings.TrimSpace(value)
	if marker := strings.LastIndex(value, webSearchQueryPrefix); marker >= 0 {
		value = strings.TrimSpace(value[marker+len(webSearchQueryPrefix):])
	}
	runes := []rune(value)
	if len(runes) > webSearchMaxQueryRunes {
		// Long agent turns often contain context followed by the actual request.
		// Keep the tail instead of rejecting the whole request at the adapter.
		value = strings.TrimSpace(string(runes[len(runes)-webSearchMaxQueryRunes:]))
	}
	return value
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
	query = truncateWebSearchQuery(strings.TrimSpace(query), webSearchMaxUpstreamRunes)
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
	return fitWebSearchMCPQuery(strings.Join(parts, " "), query)
}

func fitWebSearchMCPQuery(candidate, fallback string) string {
	if utf8.RuneCountInString(candidate) <= webSearchMaxUpstreamRunes {
		return candidate
	}
	// Domain clauses are advisory because the returned URLs are filtered again
	// locally. Preserve the user's query when the combined MCP argument would
	// exceed Kiro's 200-character tool limit.
	return truncateWebSearchQuery(fallback, webSearchMaxUpstreamRunes)
}

func truncateWebSearchQuery(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return strings.TrimSpace(string(runes[len(runes)-maxRunes:]))
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
	toolUseID := newClaudeServerToolUseID()
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
	originalQuery := strings.TrimSpace(query)
	upstreamQuery := compactWebSearchQueryForMCPRetry(originalQuery)
	if upstreamQuery == "" {
		return nil, fmt.Errorf("web search query is empty")
	}
	result, err := callMCPWebSearchWithOptionsInternal(ctx, account, upstreamQuery, options, true)
	if result != nil {
		// Compaction applies only to the upstream query. The client-visible
		// server_tool_use must continue to echo the original query.
		result.Query = originalQuery
	}
	return result, err
}

func callMCPWebSearchWithOptionsInternal(
	ctx context.Context,
	account *config.Account,
	query string,
	options webSearchMCPCallOptions,
	allowQueryCompatibilityRetry bool,
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
		client = kiroUpstreamClient(account, true)
	}
	maxResponseBytes := options.MaxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = webSearchMCPMaxResponse
	}

	toolUseID, payload := newWebSearchMCPRequest(query)
	body, err := marshalUpstreamJSON(payload)
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
	applyKiroSDKCallHeaders(req, 1)
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
		errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, webSearchMCPRetryMaxBytes+1))
		if readErr != nil {
			return nil, &webSearchMCPError{
				StatusCode: resp.StatusCode,
				RetryAfter: strings.TrimSpace(resp.Header.Get("Retry-After")),
				Message:    "web search MCP error response could not be read",
				Err:        readErr,
			}
		}
		return nil, &webSearchMCPError{
			StatusCode: resp.StatusCode,
			RetryAfter: strings.TrimSpace(resp.Header.Get("Retry-After")),
			Body:       string(errorBody),
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
		if isWebSearchNoResultsRPC(mcpResponse.Error) {
			// Kiro reports an empty result set with JSON-RPC -32602. That is not
			// an invalid request; surfacing it as a rejection made every miss
			// look like a broken web search.
			return &webSearchResultSet{ToolUseID: toolUseID, Query: query}, nil
		}
		if allowQueryCompatibilityRetry && mcpResponse.Error.Code != nil && *mcpResponse.Error.Code == jsonRPCInvalidParams {
			compatQuery := compactWebSearchQueryForMCPRetry(query)
			if compatQuery != "" && compatQuery != query {
				// The response has already been consumed. Close it before issuing the
				// one bounded compatibility retry so the transport can reuse the socket.
				_ = resp.Body.Close()
				return callMCPWebSearchWithOptionsInternal(ctx, account, compatQuery, options, false)
			}
		}
		message := "web search MCP upstream returned a JSON-RPC error"
		if mcpResponse.Error.Code != nil {
			message = fmt.Sprintf("%s (JSON-RPC %d)", message, *mcpResponse.Error.Code)
		}
		if isPermanentWebSearchRPCError(mcpResponse.Error.Code) {
			message = "web search MCP upstream rejected the request"
			if mcpResponse.Error.Code != nil {
				message = fmt.Sprintf("%s (JSON-RPC %d)", message, *mcpResponse.Error.Code)
			}
		}
		return nil, &webSearchMCPError{
			StatusCode: http.StatusBadGateway,
			RPCCode:    mcpResponse.Error.Code,
			Message:    message,
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

func isWebSearchNoResultsRPC(rpcErr *webSearchMCPResponseError) bool {
	if rpcErr == nil || rpcErr.Code == nil || *rpcErr.Code != jsonRPCInvalidParams {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(rpcErr.Message))
	return strings.Contains(message, "no results")
}

func compactWebSearchQueryForMCPRetry(query string) string {
	var builder strings.Builder
	builder.Grow(minInt(len(query), webSearchMaxUpstreamRunes*utf8.UTFMax))
	lastWasSpace := false
	for _, char := range strings.TrimSpace(query) {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			if !lastWasSpace {
				builder.WriteByte(' ')
				lastWasSpace = true
			}
			continue
		}
		builder.WriteRune(char)
		lastWasSpace = false
	}
	return truncateWebSearchQuery(strings.TrimSpace(builder.String()), webSearchMaxUpstreamRunes)
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
	return writeWebSearchResponseWithCache(w, req, result, estimatedInputTokens, promptCacheUsage{})
}

func writeWebSearchResponseWithCache(
	w http.ResponseWriter,
	req *ClaudeRequest,
	result *webSearchResultSet,
	estimatedInputTokens int,
	cacheUsage promptCacheUsage,
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
	responseModel := req.Model
	if strings.TrimSpace(req.responseModel) != "" {
		responseModel = req.responseModel
	}

	if !req.Stream {
		body := map[string]interface{}{
			"id":            newWebSearchMessageID(),
			"type":          "message",
			"role":          "assistant",
			"content":       content,
			"model":         responseModel,
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"stop_details":  nil,
			"usage":         buildWebSearchUsage(inputTokens, outputTokens, cacheUsage),
			"context_management": map[string]interface{}{
				"applied_edits": []interface{}{},
			},
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
	streamBody, err := renderWebSearchSSEWithCache(responseModel, result, content, summary, inputTokens, outputTokens, cacheUsage)
	if err != nil {
		return 0, err
	}

	configureSSEHeaders(w)
	if _, err := w.Write(streamBody); err != nil {
		return 0, err
	}
	flusher.Flush()
	return outputTokens, nil
}

func buildWebSearchContent(result *webSearchResultSet) ([]interface{}, string, int) {
	summary := generateWebSearchSummary(result)
	sessionID := uuid.NewString()
	searchResults := buildWebSearchResultContentForSession(result.Results, sessionID)
	citations := buildWebSearchCitationsForSession(result.Results, sessionID)
	content := []interface{}{
		map[string]interface{}{
			"id":    result.ToolUseID,
			"type":  "server_tool_use",
			"name":  webSearchToolName,
			"input": map[string]interface{}{"query": result.Query},
		},
		map[string]interface{}{
			"type":        "web_search_tool_result",
			"tool_use_id": result.ToolUseID,
			"content":     searchResults,
			"caller":      map[string]interface{}{"type": "direct"},
		},
		map[string]interface{}{"type": "text", "text": summary, "citations": citations},
	}

	resultJSON, _ := json.Marshal(searchResults)
	citationJSON, _ := json.Marshal(citations)
	outputTokens := estimateApproxTokens(summary) + estimateApproxTokens(string(resultJSON)) + estimateApproxTokens(string(citationJSON))
	if outputTokens < 1 {
		outputTokens = 1
	}
	return content, summary, outputTokens
}

func buildWebSearchResultContent(results []webSearchResult) []map[string]interface{} {
	return buildWebSearchResultContentForSession(results, uuid.NewString())
}

func buildWebSearchResultContentForSession(results []webSearchResult, sessionID string) []map[string]interface{} {
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
			"encrypted_content": opaqueWebSearchValue(claudeProtocolEnvelopeWebSearchContent, sessionID, result.URL, result.Title, snippet),
			"page_age":          pageAge,
		})
	}
	return content
}

func buildWebSearchCitations(results []webSearchResult) []map[string]interface{} {
	return buildWebSearchCitationsForSession(results, uuid.NewString())
}

func buildWebSearchCitationsForSession(results []webSearchResult, sessionID string) []map[string]interface{} {
	citations := make([]map[string]interface{}, 0, len(results))
	for _, result := range results {
		citedText := ""
		if result.Snippet != nil {
			citedText = truncateWebSearchSnippet(strings.TrimSpace(*result.Snippet), 500)
		}
		citations = append(citations, map[string]interface{}{
			"type":            "web_search_result_location",
			"url":             result.URL,
			"title":           result.Title,
			"encrypted_index": opaqueWebSearchValue(claudeProtocolEnvelopeWebSearchIndex, sessionID, result.URL, result.Title),
			"cited_text":      citedText,
		})
	}
	return citations
}

func opaqueWebSearchValue(kind uint64, sessionID string, parts ...string) string {
	material := strings.Join(parts, "\x00")
	if protocolCamouflageEnabled() {
		return buildClaudeWebSearchEnvelope(sessionID, kind, material)
	}
	digest := sha256.Sum256([]byte(material))
	return base64.RawStdEncoding.EncodeToString(digest[:])
}

func generateWebSearchSummary(result *webSearchResultSet) string {
	var summary strings.Builder
	if result.Unavailable {
		fmt.Fprintf(&summary, "Web search for %q is temporarily unavailable. No search results were returned.", result.Query)
		return summary.String()
	}
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

func buildWebSearchUsage(inputTokens, outputTokens int, cacheUsage promptCacheUsage) map[string]interface{} {
	usage := buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, true)
	usage["output_tokens_details"] = map[string]int{"thinking_tokens": 0}
	usage["server_tool_use"] = map[string]interface{}{
		"web_search_requests": 1,
		"web_fetch_requests":  0,
	}
	return usage
}

func renderWebSearchSSE(
	model string,
	result *webSearchResultSet,
	content []interface{},
	summary string,
	inputTokens, outputTokens int,
) ([]byte, error) {
	return renderWebSearchSSEWithCache(model, result, content, summary, inputTokens, outputTokens, promptCacheUsage{})
}

func renderWebSearchSSEWithCache(
	model string,
	result *webSearchResultSet,
	content []interface{},
	summary string,
	inputTokens, outputTokens int,
	cacheUsage promptCacheUsage,
) ([]byte, error) {
	var body bytes.Buffer
	messageStartUsage := buildWebSearchUsage(inputTokens, 0, cacheUsage)
	delete(messageStartUsage, "server_tool_use")
	delete(messageStartUsage, "output_tokens_details")
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
			"stop_details":  nil,
			"usage":         messageStartUsage,
		},
	}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"id":    result.ToolUseID,
			"type":  "server_tool_use",
			"name":  webSearchToolName,
			"input": map[string]interface{}{},
		},
	}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "ping", map[string]interface{}{"type": "ping"}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{
			"type":         "input_json_delta",
			"partial_json": "",
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
		"index": 0,
		"delta": map[string]interface{}{
			"type":         "input_json_delta",
			"partial_json": string(inputJSON),
		},
	}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": 0,
	}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         1,
		"content_block": content[1],
	}); err != nil {
		return nil, err
	}
	if err := appendWebSearchSSEEvent(&body, "content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": 1,
	}); err != nil {
		return nil, err
	}
	summaryBlock, _ := content[2].(map[string]interface{})
	citations, _ := summaryBlock["citations"].([]map[string]interface{})
	if err := appendWebSearchCitedTextBlock(&body, 2, chunkWebSearchText(summary, webSearchSummaryChunkRunes), citations); err != nil {
		return nil, err
	}
	terminalUsage := buildClaudeTerminalUsageMap(inputTokens, outputTokens, cacheUsage)
	terminalUsage["server_tool_use"] = map[string]interface{}{
		"web_search_requests": 1,
		"web_fetch_requests":  0,
	}
	if err := appendWebSearchSSEEvent(&body, "message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"stop_details":  nil,
		},
		"context_management": map[string]interface{}{
			"applied_edits": []interface{}{},
		},
		"usage": terminalUsage,
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

func appendWebSearchCitedTextBlock(body *bytes.Buffer, index int, chunks []string, citations []map[string]interface{}) error {
	if err := appendWebSearchSSEEvent(body, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]interface{}{
			"type":      "text",
			"text":      "",
			"citations": []interface{}{},
		},
	}); err != nil {
		return err
	}
	for _, citation := range citations {
		if err := appendWebSearchSSEEvent(body, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]interface{}{
				"type":     "citations_delta",
				"citation": citation,
			},
		}); err != nil {
			return err
		}
	}
	for _, chunk := range chunks {
		if err := appendWebSearchSSEEvent(body, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]interface{}{"type": "text_delta", "text": chunk},
		}); err != nil {
			return err
		}
	}
	return appendWebSearchSSEEvent(body, "content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": index,
	})
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
	return newClaudeMessageID()
}
