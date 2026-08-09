package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	webassets "kiro-go/web"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	tokenRefreshSkewSeconds   int64 = 120
	claudeMaxRequestBodyBytes int64 = 32 << 20
)

// RequestLog stores details about a single API request (success or failure).
type RequestLog struct {
	Time      int64   `json:"time"`      // Unix timestamp
	Endpoint  string  `json:"endpoint"`  // claude/openai/responses
	Model     string  `json:"model"`     // Requested model
	AccountID string  `json:"accountId"` // Account used
	Status    string  `json:"status"`    // "success" or "error"
	Error     string  `json:"error"`     // Error message (empty on success)
	ErrorType string  `json:"errorType"` // Error category (empty on success)
	Tokens    int     `json:"tokens"`    // Total tokens (input+output, 0 on failure)
	Credits   float64 `json:"credits"`   // Credits consumed (0 on failure)
	Duration  int64   `json:"duration"`  // Request duration in ms
}

const requestLogsMaxSize = 500

// Handler HTTP 处理器
type Handler struct {
	pool               *pool.AccountPool
	webSearchMCPCaller webSearchMCPCaller
	kiroCaller         kiroAPICaller
	// 运行时统计 (使用原子操作)
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalTokens     int64
	totalCredits    float64 // float64 需要用锁保护
	creditsMu       sync.RWMutex
	statsMu         sync.Mutex
	startTime       int64
	stopRefresh     chan struct{}
	stopStatsSaver  chan struct{}
	closeOnce       sync.Once
	backgroundWG    sync.WaitGroup
	// 模型缓存
	cachedModels          []ModelInfo
	modelsCacheMu         sync.RWMutex
	modelsCacheTime       int64
	promptCache           *promptCacheTracker
	promptCacheConfigMu   sync.Mutex
	promptCachePoliciesMu sync.Mutex
	promptCachePolicies   map[string]*promptCacheTracker
	traffic               *TrafficTracker
	latency               *latencyTracker
	retryBudget           *retryBudget
	responseWritePolicy   responseWritePolicy
	tokenRefreshOnce      sync.Once
	tokenRefreshGate      chan struct{}
	// Request-triggered expired-token refresh is single-flight per account.
	expiredRefreshMu         sync.Mutex
	refreshingExpiredTokens  map[string]int64
	expiredRefreshRetryAfter map[string]int64
	// 请求日志 (环形缓冲区，包含成功和失败)
	requestLogs   []RequestLog
	requestLogsMu sync.RWMutex
}

type thinkingStreamSource int

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}
	if req.OutputConfig != nil {
		if msg := validateClaudeOutputFormat(req.OutputConfig.Format); msg != "" {
			return msg
		}
	}
	if msg := validateClaudeStopSequences(req.StopSequences); msg != "" {
		return msg
	}
	if choice, err := resolveToolChoice(req.ToolChoice); err != nil {
		return err.Error()
	} else {
		names := make([]string, 0, len(req.Tools))
		for _, tool := range req.Tools {
			names = append(names, tool.Name)
		}
		if msg := validateResolvedToolChoice(choice, names); msg != "" {
			return msg
		}
	}

	hasUserMessage := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}
		hasUserMessage = true
	}

	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user"
	}
	if !hasUserMessage {
		return "at least one user message is required"
	}
	return ""
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateOpenAIResponseFormat(req.ResponseFormat); msg != "" {
		return msg
	}
	if _, err := resolveOpenAIStopSequences(req.Stop); err != nil {
		return err.Error()
	}
	if choice, err := resolveToolChoice(req.ToolChoice); err != nil {
		return err.Error()
	} else {
		names := make([]string, 0, len(req.Tools))
		for _, tool := range req.Tools {
			names = append(names, openAIToolName(tool))
		}
		if msg := validateResolvedToolChoice(choice, names); msg != "" {
			return msg
		}
	}

	hasNonSystem := false
	hasUserMessage := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		hasUserMessage = true
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserMessage {
		return "at least one user message is required"
	}
	return ""
}

func NewHandler() *Handler {
	// 启动时应用代理配置
	applyProxyConfig(config.GetProxyURL())

	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	h := &Handler{
		pool:                     pool.GetPool(),
		totalRequests:            int64(totalReq),
		successRequests:          int64(successReq),
		failedRequests:           int64(failedReq),
		totalTokens:              int64(totalTokens),
		totalCredits:             totalCredits,
		startTime:                time.Now().Unix(),
		stopRefresh:              make(chan struct{}),
		stopStatsSaver:           make(chan struct{}),
		promptCache:              newPromptCacheTracker(defaultPromptCacheTTL),
		promptCachePolicies:      make(map[string]*promptCacheTracker),
		traffic:                  NewTrafficTracker(DefaultTrafficRetention),
		latency:                  &latencyTracker{},
		retryBudget:              newRetryBudgetFromEnv(),
		responseWritePolicy:      responseWritePolicyFromEnv(),
		refreshingExpiredTokens:  make(map[string]int64),
		expiredRefreshRetryAfter: make(map[string]int64),
	}
	h.promptCache.SetDefaultTTL(time.Duration(config.GetPromptCacheTTLSeconds()) * time.Second)
	cachePath := filepath.Join(config.GetConfigDir(), "prompt_cache.json")
	h.promptCache.Load(cachePath)
	h.promptCache.startSaveLoop(cachePath, 30*time.Second)
	h.preloadPromptCachePolicyTrackers()
	// 启动后台刷新
	go h.backgroundRefresh()
	// 启动后台统计保存 (每30秒保存一次)
	h.backgroundWG.Add(1)
	go func() {
		defer h.backgroundWG.Done()
		h.backgroundStatsSaver()
	}()
	// 清理过期的 stored responses（>30 天）
	go purgeExpiredResponses(responsesDefaultTTL)
	return h
}

// Close stops background work, waits for queued statistics writes, and flushes
// prompt-cache state to disk.
func (h *Handler) Close() {
	if h == nil {
		return
	}
	h.closeOnce.Do(func() {
		if h.stopRefresh != nil {
			close(h.stopRefresh)
		}
		if h.stopStatsSaver != nil {
			close(h.stopStatsSaver)
		}
	})
	h.backgroundWG.Wait()
	CloseKiroIdleConnections()
	if h.promptCache != nil {
		h.promptCache.Stop()
	}
	h.stopPromptCachePolicyTrackers()
	if h.pool != nil {
		h.pool.WaitForPendingStats()
	}
}

// backgroundRefresh 后台定时刷新账户信息
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(30 * time.Minute) // 每 30 分钟刷新一次
	defer ticker.Stop()

	// 启动时延迟 10 秒后执行一次，关闭时可立即中断。
	startupTimer := time.NewTimer(10 * time.Second)
	select {
	case <-startupTimer.C:
	case <-h.stopRefresh:
		if !startupTimer.Stop() {
			<-startupTimer.C
		}
		return
	}
	h.refreshModelsCache()
	h.refreshAllAccounts()

	for {
		select {
		case <-ticker.C:
			h.refreshModelsCache()
			h.refreshAllAccounts()
		case <-h.stopRefresh:
			return
		}
	}
}

// refreshAllAccounts 刷新所有账户信息
func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()
	for i := range accounts {
		account := &accounts[i]
		if !account.Enabled || account.AccessToken == "" {
			continue
		}

		// 刷新 token（去重 + 持久化由 refreshAccountToken 统一处理）
		if err := h.refreshAccountToken(account, false); err != nil {
			logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
			h.handleAccountFailure(account, err)
			continue
		}

		// 刷新账户信息
		info, err := RefreshAccountInfo(account)
		if err != nil {
			logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
			continue
		}

		config.UpdateAccountInfo(account.ID, *info)
		logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
	}
	h.pool.Reload()
}

// validateApiKey 验证 API Key（Bool 包装，旧签名仍被部分调用方使用）
func (h *Handler) validateApiKey(r *http.Request) bool {
	_, err := h.authenticate(r)
	return err == nil
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	started := time.Now()
	entry, err := h.authenticate(r)
	observeRequestStage(r.Context(), stageConfigLockWait, time.Since(started))
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendClaudeError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
	started := time.Now()
	entry, err := h.authenticate(r)
	observeRequestStage(r.Context(), stageConfigLockWait, time.Since(started))
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendOpenAIError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, r.RemoteAddr)

	// CORS - 完整的头部支持
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
	w.Header().Set("Access-Control-Expose-Headers", "request-id, x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens, x-kiro-api-key-matched, x-kiro-prompt-cache-policy")

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}
	if isGenerationAPIPath(path) {
		ctx, cancel := context.WithCancel(r.Context())
		ctx, timing := withRequestTiming(ctx, h.latency)
		r = r.WithContext(ctx)
		policy := h.responseWritePolicy
		if policy.writeTimeout <= 0 {
			policy = responseWritePolicyFromEnv()
		}
		wrapped, bounded := wrapBoundedResponseWriterWithPolicy(w, cancel, timing, policy)
		w = wrapped
		defer func() {
			bounded.finish()
			cancel()
		}()
	}

	// 路由
	switch {
	// API 端点（需要验证 API Key）
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleClaudeMessages(w, ar)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleCountTokens(w, ar)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIChat(w, ar)
	case path == "/v1/responses" || path == "/responses":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIResponses(w, ar)
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 管理端点
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case path == "/admin/index-legacy.html":
		http.Redirect(w, r, "/admin/", http.StatusPermanentRedirect)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

	// 健康检查
	case path == "/health" || path == "/":
		h.handleHealth(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

func isGenerationAPIPath(path string) bool {
	switch path {
	case "/v1/messages", "/messages", "/anthropic/v1/messages",
		"/v1/messages/count_tokens", "/messages/count_tokens",
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses":
		return true
	default:
		return false
	}
}

// handleHealth 健康检查（不暴露统计数据）
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	cacheStats, cachePolicyStats := h.promptCacheStatsSnapshot()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"refillEligible":  h.pool.RefillEligibleCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"cache":           cacheStats,
		"cachePolicies":   cachePolicyStats,
		"ttftStages":      h.latency.Snapshot(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// handleModels 模型列表
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	// 尝试用缓存的真实模型列表
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()
	if len(cached) == 0 {
		h.refreshModelsCache()
		h.modelsCacheMu.RLock()
		cached = h.cachedModels
		h.modelsCacheMu.RUnlock()
	}

	thinkingSuffix := config.GetThinkingConfig().Suffix

	models := buildAnthropicModelsResponse(cached, thinkingSuffix)
	if len(models) == 0 {
		models = fallbackAnthropicModels(thinkingSuffix)
	}

	// 添加别名模型
	models = append(models,
		buildModelInfo("auto", "kiro-proxy", true),
		buildModelInfo("gpt-4o", "kiro-proxy", true),
		buildModelInfo("gpt-4", "kiro-proxy", true),
	)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
	return
}

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	models := make([]map[string]interface{}, 0, len(cached)*2)
	if len(cached) > 0 {
		for _, m := range cached {
			supportsImage := modelSupportsImage(m.InputTypes)
			limits := lookupModelTokenLimits(m.ModelId)
			models = append(models, buildModelInfoWithTokenLimits(m.ModelId, "anthropic", supportsImage, limits))
			// 自动生成 thinking 变体
			models = append(models, buildModelInfoWithTokenLimits(m.ModelId+thinkingSuffix, "anthropic", supportsImage, limits))
		}
	}
	return models
}

func fallbackAnthropicModels(thinkingSuffix string) []map[string]interface{} {
	return []map[string]interface{}{
		buildModelInfo("claude-opus-4.8", "anthropic", true),
		buildThinkingModelInfo("claude-opus-4.8", thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.6", "anthropic", true),
		buildThinkingModelInfo("claude-sonnet-4.6", thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.6", "anthropic", true),
		buildThinkingModelInfo("claude-opus-4.6", thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.7", "anthropic", true),
		buildThinkingModelInfo("claude-opus-4.7", thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.5", "anthropic", true),
		buildThinkingModelInfo("claude-sonnet-4.5", thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4", "anthropic", true),
		buildThinkingModelInfo("claude-sonnet-4", thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-haiku-4.5", "anthropic", true),
		buildThinkingModelInfo("claude-haiku-4.5", thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.5", "anthropic", true),
		buildThinkingModelInfo("claude-opus-4.5", thinkingSuffix, "anthropic", true),
	}
}

func modelSupportsImage(inputTypes []string) bool {
	for _, t := range inputTypes {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
			return true
		}
	}
	return false
}

func buildModelInfo(id, ownedBy string, supportsImage bool) map[string]interface{} {
	return buildModelInfoWithTokenLimits(id, ownedBy, supportsImage, lookupModelTokenLimits(id))
}

func buildThinkingModelInfo(id, thinkingSuffix, ownedBy string, supportsImage bool) map[string]interface{} {
	return buildModelInfoWithTokenLimits(id+thinkingSuffix, ownedBy, supportsImage, lookupModelTokenLimits(id))
}

func buildModelInfoWithTokenLimits(id, ownedBy string, supportsImage bool, limits modelTokenLimit) map[string]interface{} {
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	result := map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
		"supports_image":   supportsImage,
		"input_modalities": modalities,
		"modalities":       modalitiesMap,
		"capabilities": map[string]bool{
			"vision":       supportsImage,
			"image":        supportsImage,
			"image_vision": supportsImage,
		},
		"info": map[string]interface{}{
			"meta": map[string]interface{}{
				"capabilities": map[string]bool{
					"vision":       supportsImage,
					"image_vision": supportsImage,
				},
			},
		},
	}
	if limits.MaxInputTokens > 0 {
		result["context_window"] = limits.MaxInputTokens
		result["max_input_tokens"] = limits.MaxInputTokens
	}
	if limits.MaxOutputTokens > 0 {
		result["max_output_tokens"] = limits.MaxOutputTokens
	}
	return result
}

// refreshModelsCache 从 Kiro API 拉取模型列表并缓存
func (h *Handler) refreshModelsCache() {
	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		return
	}

	aggregated := make([]ModelInfo, 0)
	for i := range accounts {
		account := &accounts[i]
		if err := h.ensureValidToken(account); err != nil {
			logger.Warnf("[ModelsCache] Skip %s token refresh failed: %v", account.Email, err)
			h.handleAccountFailure(account, err)
			continue
		}

		models, err := ListAvailableModels(account)
		if err != nil {
			logger.Warnf("[ModelsCache] Failed to refresh for %s: %v", account.Email, err)
			h.handleAccountFailure(account, err)
			continue
		}
		// 缓存每账号可用模型，用于路由时过滤
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(account.ID, modelIDs)
		aggregated = mergeUniqueModels(aggregated, models)
	}

	if len(aggregated) > 0 {
		h.modelsCacheMu.Lock()
		h.cachedModels = aggregated
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Cached %d models", len(aggregated))
	}
}

// fetchAndCacheAccountModels 为单个账号拉取并写入模型缓存。
// 同时更新 pool 的路由缓存与全局聚合模型列表。
func (h *Handler) fetchAndCacheAccountModels(account *config.Account) error {
	if err := h.ensureValidToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := ListAvailableModels(account)
	if err != nil {
		return err
	}
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(account.ID, modelIDs)

	// 合并到聚合缓存
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	logger.Infof("[ModelsCache] Refreshed %d models for account %s", len(models), account.Email)
	return nil
}

// apiRefreshAccountModels POST /admin/api/accounts/{id}/models/refresh
// 立即为指定账号拉取并更新模型路由缓存。
func (h *Handler) apiRefreshAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// 从 pool 取运行时最新 token（与 refreshModelsCache 逻辑一致）
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	if err := h.fetchAndCacheAccountModels(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(h.pool.GetModelList(id)),
	})
}

// apiRefreshAllAccountsModels POST /admin/api/accounts/models/refresh
// 直接复用 refreshModelsCache，为所有已启用账号刷新模型路由缓存。
func (h *Handler) apiRefreshAllAccountsModels(w http.ResponseWriter, r *http.Request) {
	h.refreshModelsCache()
	h.modelsCacheMu.RLock()
	cachedLen := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"refreshed": cachedLen,
		"failed":    0,
	})
}

func mergeUniqueModels(existing []ModelInfo, incoming []ModelInfo) []ModelInfo {
	if len(incoming) == 0 {
		return existing
	}

	indexByID := make(map[string]int, len(existing))
	merged := make([]ModelInfo, len(existing))
	copy(merged, existing)
	for i, model := range merged {
		indexByID[strings.ToLower(strings.TrimSpace(model.ModelId))] = i
	}

	for _, model := range incoming {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = mergeModelInfo(merged[idx], model)
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}

	return merged
}

func mergeModelInfo(base ModelInfo, extra ModelInfo) ModelInfo {
	if base.ModelName == "" {
		base.ModelName = extra.ModelName
	}
	if base.Description == "" {
		base.Description = extra.Description
	}
	if base.RateMultiplier == 0 {
		base.RateMultiplier = extra.RateMultiplier
	}
	if base.TokenLimits == nil {
		base.TokenLimits = extra.TokenLimits
	}
	base.InputTypes = mergeStringLists(base.InputTypes, extra.InputTypes)
	return base
}

func mergeStringLists(base []string, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	for _, item := range extra {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	return merged
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	setAnthropicRequestID(w)
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	var req ClaudeRequest
	err := decodeRequestJSONWithLimit(w, r, claudeMaxRequestBodyBytes, &req)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.sendClaudeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds 32 MiB")
			return
		}
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}
	if req.OutputConfig != nil {
		if msg := validateClaudeOutputFormat(req.OutputConfig.Format); msg != "" {
			h.sendClaudeError(w, 400, "invalid_request_error", msg)
			return
		}
	}
	if msg := validateClaudeStopSequences(req.StopSequences); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}
	if choice, choiceErr := resolveToolChoice(req.ToolChoice); choiceErr != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", choiceErr.Error())
		return
	} else {
		names := make([]string, 0, len(req.Tools))
		for _, tool := range req.Tools {
			names = append(names, tool.Name)
		}
		if msg := validateResolvedToolChoice(choice, names); msg != "" {
			h.sendClaudeError(w, 400, "invalid_request_error", msg)
			return
		}
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, _ := resolveClaudeRequestThinkingMode(&req, thinkingCfg.Suffix)
	req.Model = actualModel
	// /count_tokens reports only the request supplied by the client. The
	// thinking prompt is injected by the proxy later and must not inflate the
	// public count.
	estimatedTokens := estimateClaudeRequestInputTokens(&req)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}

func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	setAnthropicRequestID(w)
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	apiKeyID := apiKeyIDFromContext(r.Context())
	cacheTracker, cachePolicy := h.promptCacheRuntimeForAPIKey(apiKeyID)
	effectivePolicyID := cachePolicy.ID
	if effectivePolicyID == "" {
		effectivePolicyID = "global"
	}
	w.Header().Set("X-Kiro-Api-Key-Matched", strconv.FormatBool(apiKeyID != ""))
	w.Header().Set("X-Kiro-Prompt-Cache-Policy", effectivePolicyID)
	var req ClaudeRequest
	var body []byte
	if cachePolicy.CacheMode == config.CacheModeFunctionSimulation {
		var err error
		body, err = readRequestBodyWithLimit(w, r, claudeMaxRequestBodyBytes)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				h.sendClaudeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds 32 MiB")
				return
			}
			h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
			return
		}
	} else if err := decodeRequestJSONWithLimit(w, r, claudeMaxRequestBodyBytes, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.sendClaudeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds 32 MiB")
			return
		}
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}
	// Claude Code normally uses the same /v1/messages URL as other Anthropic
	// clients. Detect its stable system-prompt markers after decoding so those
	// existing clients receive the context usage contract without reconfiguration.
	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	requestedModel := req.Model
	actualModel, thinking := resolveClaudeRequestThinkingMode(&req, thinkingCfg.Suffix)
	req.Model = actualModel
	req.responseModel = requestedModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	// Keep public usage anchored to the client request. effectiveReq is still
	// used below for prompt-cache fingerprinting so standard and thinking
	// requests cannot share an upstream cache entry.
	estimatedInputTokens := estimateClaudeRequestInputTokens(&req)
	toolChoice, _ := resolveToolChoice(req.ToolChoice)
	selectedReq := req
	selectedReq.Tools = filterClaudeToolsForChoice(req.Tools, toolChoice)
	if toolChoice.Mode == "specific" && toolChoice.Name == advisorToolName {
		for i := range selectedReq.Tools {
			if !isNativeAdvisorTool(selectedReq.Tools[i]) {
				continue
			}
			selectedReq.Tools[i].Type = "custom"
			if strings.TrimSpace(selectedReq.Tools[i].Description) == "" {
				selectedReq.Tools[i].Description = "Request an advisor review."
			}
			if selectedReq.Tools[i].InputSchema == nil {
				selectedReq.Tools[i].InputSchema = map[string]interface{}{"type": "object"}
			}
		}
	}
	isolationSeed := claudePromptCacheIsolationSeed(effectiveReq, apiKeyID, extractProvidedKey(r))
	requestBodyFingerprint := ""
	if cachePolicy.CacheMode == config.CacheModeFunctionSimulation {
		requestBodyFingerprint = functionCacheRequestBodyFingerprint(body)
	}
	buildRequestCacheProfile := func() *promptCacheProfile {
		if cacheTracker == nil {
			return nil
		}
		profile := cacheTracker.BuildClaudeProfileForRequestWithPolicy(
			effectiveReq,
			estimatedInputTokens,
			isolationSeed,
			requestBodyFingerprint,
			cachePolicy,
		)
		if profile != nil {
			profile.requestTiming = requestTimingFromContext(r.Context())
		}
		return profile
	}
	if hasWebSearchTool(&selectedReq) {
		h.handleWebSearchRequest(w, r.Context(), &selectedReq, estimatedInputTokens, apiKeyID, buildRequestCacheProfile())
		return
	}
	if _, ok := findNativeCodeExecutionTool(&selectedReq); ok {
		h.handleCodeExecutionRequest(w, r.Context(), &selectedReq, requestedModel, estimatedInputTokens, apiKeyID, buildRequestCacheProfile())
		return
	}
	if _, ok := findNativeAdvisorTool(&selectedReq); ok {
		h.handleAdvisorRequest(
			w, r.Context(), &selectedReq, requestedModel, thinking, thinkingResponseOpts, isolationSeed, apiKeyID,
			cacheTracker, cachePolicy,
		)
		return
	}
	if hasNativeWebSearchDeclaration(&selectedReq) {
		err := errors.New("native web_search must be a supported standalone Claude Code WebSearch request")
		h.recordFailureWithDetails("claude", req.Model, "", err)
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	cacheProfile := buildRequestCacheProfile()

	// 转换请求
	kiroPayload := bindKiroPayloadContext(ClaudeToKiro(&selectedReq, thinking), r.Context())
	kiroPayload.requestID = w.Header().Get("request-id")
	if _, err := preflightKiroPayload(kiroPayload); err != nil {
		h.recordFailureWithDetails("claude", req.Model, "", err)
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Stream or non-stream
	if req.Stream {
		h.handleClaudeStream(w, kiroPayload, req.Model, requestedModel, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID)
	} else {
		h.handleClaudeNonStream(w, kiroPayload, req.Model, requestedModel, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID)
	}
}

// setAnthropicRequestID provides the response correlation headers expected by
// Anthropic clients. Kiro's upstream request IDs are not stable across the
// adapter boundary, so generate one per downstream request.
func setAnthropicRequestID(w http.ResponseWriter) {
	if w == nil || w.Header().Get("request-id") != "" {
		return
	}
	id := "req_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	w.Header().Set("request-id", id)
}

// retryKiroPreResponse prepares a bounded retry before the upstream has
// produced an answer or tool call. Marker recovery has its own semantic budget;
// only ordinary transport failures consume retryCount. Tool JSON recovery is
// handled separately by the streaming handler and must not fall through here.
// Callers must increase their attempt budget when it succeeds.
func (h *Handler) retryKiroPreResponse(payload *KiroPayload, account *config.Account, retryCount *int, excluded map[string]bool, retrySameID *string, causes ...error) (bool, error) {
	if account == nil || retryCount == nil || retrySameID == nil {
		return false, nil
	}
	var cause error
	if len(causes) > 0 {
		cause = causes[0]
	}
	if isKiroToolJSONError(cause) {
		return false, nil
	}
	if isKiroRecoveryTimeoutError(cause) {
		return false, nil
	}
	if isUpstreamAgentMarkerMismatchError(cause) {
		if !agentCompletionRetryAllowed(payload, cause) {
			return false, nil
		}
		if !h.allowUpstreamRetry() {
			return false, nil
		}
		*retrySameID = account.ID
		return true, nil
	}
	if *retryCount >= maxKiroTransportPreResponseRetries {
		return false, nil
	}
	if !h.allowUpstreamRetry() {
		return false, nil
	}

	delay := 100 * time.Millisecond
	if !waitForRateLimitRetry(kiroPayloadContext(payload), rateLimitDecision{Delay: delay}) {
		return false, kiroPayloadContext(payload).Err()
	}
	*retryCount++
	*retrySameID = account.ID
	return true, nil
}

func (h *Handler) retryAgentRecoveryAfterInputTooLong(endpoint string, payload *KiroPayload, account *config.Account, retrySameID *string, err error) bool {
	if account == nil || retrySameID == nil || !isKiroInputTooLongError(err) ||
		!agentInputTooLongRecoveryEligible(payload) || !h.allowUpstreamRetry() {
		return false
	}
	previousPhase := kiroAgentRecoveryPhase(payload, err)
	if !prepareAgentInputTooLongRecovery(payload) {
		return false
	}
	*retrySameID = account.ID
	logger.Warnf(
		"[KiroAPI] retrying %s after recovery input-too-long request_id=%s account_id=%s previous_phase=%s phase=%s retry=%d/%d completions=%d actions=%d full_context=%d payload_bytes=%d estimated_tokens=%d: %v",
		endpoint, payload.requestID, account.ID, previousPhase, kiroAgentRecoveryPhase(payload, err),
		payload.AgentInputTooLongRecoveries, maxKiroAgentInputTooLongRecoveries,
		payload.AgentCompletionRetries, payload.AgentActionContinuations, payload.AgentFullContextRecoveries,
		payload.lastAttemptPayloadBytes, payload.lastAttemptEstimatedInputTokens, err,
	)
	return true
}

func kiroPreResponseRetryProgress(payload *KiroPayload, transportRetries int, cause error) (int, int) {
	if payload != nil && payload.agentInputTooLongRecoveryPending {
		return payload.AgentInputTooLongRecoveries, maxKiroAgentInputTooLongRecoveries
	}
	if payload != nil && payload.agentFullContextRecoveryPending {
		return payload.AgentFullContextRecoveries, maxKiroAgentFullContextRecoveries
	}
	if isUpstreamAgentActionOnlyError(cause) {
		continuations := 0
		if payload != nil {
			continuations = payload.AgentActionContinuations
		}
		return continuations, maxKiroAgentActionContinuations
	}
	if isUpstreamAgentMarkerMismatchError(cause) {
		semanticRetries := 0
		if payload != nil {
			semanticRetries = payload.AgentCompletionRetries
		}
		return semanticRetries, maxKiroAgentCompletionRetries
	}
	return transportRetries, maxKiroTransportPreResponseRetries
}

func kiroAgentRecoveryPhase(payload *KiroPayload, cause error) string {
	if payload != nil && payload.agentInputTooLongRecoveryPending {
		return "bounded_context_retry"
	}
	if payload != nil && payload.agentFullContextRecoveryPending {
		if payload.AgentRecoveryCompacted {
			return "bounded_context"
		}
		return "full_context"
	}
	if isUpstreamAgentActionOnlyError(cause) {
		if payload != nil && payload.AgentFullContextRecoveries > 0 {
			return "full_context_exhausted"
		}
		return "short_action"
	}
	if isUpstreamAgentMarkerMismatchError(cause) {
		return "marker"
	}
	return "transport"
}

func readRequestBodyWithLimit(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	started := time.Now()
	defer func() {
		observeRequestStage(r.Context(), stageBodyRead, time.Since(started))
	}()
	if r.ContentLength > maxBytes {
		return nil, &http.MaxBytesError{Limit: maxBytes}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return io.ReadAll(r.Body)
}

func claudePromptCacheIsolationSeed(req *ClaudeRequest, apiKeyID string, providedKeys ...string) string {
	apiKeyID = strings.TrimSpace(apiKeyID)
	keySeed := ""
	if apiKeyID != "" {
		keySeed = "key:" + apiKeyID
	} else if len(providedKeys) > 0 {
		providedKey := strings.TrimSpace(providedKeys[0])
		if providedKey != "" {
			digest := sha256.Sum256([]byte(providedKey))
			keySeed = fmt.Sprintf("provided-key:%x", digest[:])
		}
	}
	if req != nil && req.Metadata != nil {
		if _, sessionID, ok := strings.Cut(req.Metadata.UserID, "_session_"); ok {
			if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
				if keySeed != "" {
					return keySeed + "|session:" + sessionID
				}
				return "session:" + sessionID
			}
		}
	}
	return keySeed
}

// handleClaudeStream Claude 流式响应
func (h *Handler) handleClaudeStream(w http.ResponseWriter, payload *KiroPayload, model, responseModel string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	h.handleClaudeStreamWithPrelude(
		w, payload, model, responseModel, thinking, thinkingOpts, estimatedInputTokens, cacheProfile, apiKeyID, nil,
	)
}

func (h *Handler) handleClaudeStreamWithPrelude(w http.ResponseWriter, payload *KiroPayload, model, responseModel string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, prelude *claudeStreamPrelude) {
	payload = cloneKiroPayload(payload)
	configureSSEHeaders(w)

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	reqStart := time.Now()
	msgID := newClaudeMessageID()
	startInputTokens := estimatedInputTokens
	if prelude != nil {
		startInputTokens += prelude.InputTokens
	}
	startInputTokens = clampClaudeInputTokensToModelWindow(model, startInputTokens)
	excluded := make(map[string]bool)
	retryCounts := make(map[string]int)
	toolJSONRetryCounts := make(map[string]int)
	preResponseRetryCount := 0
	retrySameID := ""
	var lastErr error
	streamCommitted := false
	preludeSent := false
	pingSent := false
	preludeEmitted := false
	nextContentIndex := 0
	var messageStartUsage promptCacheUsage
	cacheTracker := promptCacheTrackerForProfile(h.promptCache, cacheProfile)
	cacheUsage := promptCacheUsage{}
	cacheComputed := false
	var retryUsage retryBillingUsage
	if prelude != nil {
		retryUsage.merge(prelude.RetryUsage)
	}

	ensureMessageStart := func() {
		if preludeSent {
			return
		}
		h.sendSSE(w, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         responseModel,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"stop_details":  nil,
				"usage":         buildClaudeUsageMap(startInputTokens, 1, messageStartUsage, true),
			},
		})
		preludeSent = true
		streamCommitted = true
	}
	sendInitialPing := func() {
		if pingSent {
			return
		}
		h.sendSSE(w, flusher, "ping", map[string]interface{}{"type": "ping"})
		pingSent = true
		streamCommitted = true
	}
	sendClaudeStreamFailure := func(status int, errorType, message string) {
		if !streamCommitted {
			h.sendClaudeError(w, status, errorType, message)
			return
		}
		h.sendSSE(w, flusher, "error", map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": errorType, "message": message},
		})
	}
	var onStreamReady func()
	if earlyStreamPreludeEnabled() {
		onStreamReady = func() {
			if preludeSent {
				return
			}
			ensureMessageStart()
			sendInitialPing()
			if timing := requestTimingFromContext(kiroPayloadContext(payload)); timing != nil {
				timing.markDownstreamPreludeFlushed(time.Now())
			}
		}
	}

	attemptLimit := h.maxAccountAttempts()
	for attempt := 0; attempt < attemptLimit; attempt++ {
		account, allFull := h.acquireAccountForModel(model, excluded, retrySameID, kiroPayloadContext(payload))
		retrySameID = ""
		if account == nil {
			if h.startExpiredAccountRefreshes(model, excluded) {
				lastErr = errAccountRefreshInProgress
			} else if allFull {
				lastErr = fmt.Errorf(allAccountsConcurrencyFullMessage)
			}
			break
		}
		if err := h.ensureValidTokenForPayload(account, payload); err != nil {
			h.releaseAccount(account)
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		if !cacheComputed {
			cacheUsage = cacheTracker.Compute(account.ID, cacheProfile)
			messageStartUsage = reanchorClaudeCacheUsage(cacheProfile, cacheUsage, startInputTokens)
			cacheComputed = true
		}

		var inputTokens, outputTokens int
		var credits float64
		var toolUses []KiroToolUse
		var rawContentBuilder strings.Builder
		var rawThinkingBuilder strings.Builder
		var redactedThinking []string
		var pendingThinkingSignature string
		var reasoningSignature string
		var contextWindowExceeded bool
		var upstreamStopReason string
		stopFilter := newStopSequenceFilter(payload.StopSequences)
		completionGate := newAgentCompletionGateForPayload(payload)
		suppressRecoveryOutput := payload.AgentCompletionRetries > 0
		activeBlockIndex := -1
		activeBlockType := ""
		thinkingBlockEmitted := false
		hiddenThinkingObserved := false
		suppressedReasoningObserved := false
		visibleOutputProduced := prelude != nil && len(prelude.Blocks) > 0
		toolBlockIndexes := make(map[string]int)
		toolClientIDs := make(map[string]string)
		ignoredToolUses := make(map[string]bool)
		activeToolUseID := ""
		var deferredToolOutput []func()
		var deferredCompletedToolUses []KiroToolUse
		upstreamOutputStarted := false

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
			}
			if activeBlockType == "thinking" {
				signature := resolveClaudeThinkingSignature(ClaudeReasoningResult{
					Text:      rawThinkingBuilder.String(),
					Signature: pendingThinkingSignature,
					Redacted:  redactedThinking,
				}, true, responseModel)
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "thinking_delta", "thinking": ""},
				})
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{
						"type":      "signature_delta",
						"signature": signature,
					},
				})
				pendingThinkingSignature = ""
			}
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": activeBlockIndex,
			})
			activeBlockIndex = -1
			activeBlockType = ""
		}
		emitPrelude := func() {
			if preludeEmitted || prelude == nil || len(prelude.Blocks) == 0 {
				return
			}
			ensureMessageStart()
			closeActiveBlock()
			for _, block := range prelude.Blocks {
				idx := nextContentIndex
				nextContentIndex++

				switch block.Type {
				case "server_tool_use", "tool_use":
					contentBlock := map[string]interface{}{
						"type": block.Type, "id": block.ID, "name": block.Name, "input": map[string]interface{}{},
					}
					if block.Type == "tool_use" {
						contentBlock["caller"] = map[string]interface{}{"type": "direct"}
					}
					h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
						"type": "content_block_start", "index": idx, "content_block": contentBlock,
					})
					sendInitialPing()
					inputJSON, _ := json.Marshal(block.Input)
					partialJSON := string(inputJSON)
					if block.Type == "server_tool_use" && (block.Input == nil || partialJSON == "{}" || partialJSON == "null") {
						partialJSON = ""
					}
					h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type": "content_block_delta", "index": idx,
						"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": partialJSON},
					})
				default:
					h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
						"type": "content_block_start", "index": idx, "content_block": block,
					})
					sendInitialPing()
				}
				h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type": "content_block_stop", "index": idx,
				})
			}
			preludeEmitted = true
		}

		startContentBlock := func(blockType string) {
			if activeBlockType == blockType {
				return
			}
			ensureMessageStart()
			emitPrelude()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			if blockType == "thinking" {
				thinkingBlockEmitted = true
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type":      "thinking",
						"thinking":  "",
						"signature": "",
					},
				})
			} else {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "text",
						"text": "",
					},
				})
			}

			activeBlockIndex = idx
			activeBlockType = blockType
			sendInitialPing()
		}
		flushPendingSignatureBlock := func() {
			if stopFilter.Matched() != "" || !thinking || thinkingFormat != "thinking" || pendingThinkingSignature == "" || activeBlockType == "thinking" {
				return
			}
			startContentBlock("thinking")
			closeActiveBlock()
		}

		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var reasoningPhaseSealed bool
		var thinkingStarted bool
		var eventThinkingOpen bool
		structuredOutput := payload.StructuredOutput
		structuredJSON := &incrementalJSONStream{}
		// Auto tool choice permits a normal text answer, so stream it immediately.
		// Required/specific choices must remain pure tool turns: suppress Kiro's
		// common "I will call..." narration until the toolUse event arrives.
		holdToolText := (payload.ToolChoiceMode == "required" || payload.ToolChoiceMode == "specific") &&
			payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext != nil &&
			len(payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools) > 0

		emitRedactedThinking := func(data string) {
			if suppressRecoveryOutput || stopFilter.Matched() != "" || !thinking || thinkingFormat != "thinking" || data == "" {
				return
			}
			ensureMessageStart()
			emitPrelude()
			flushPendingSignatureBlock()
			closeActiveBlock()
			eventThinkingOpen = false
			thinkingStarted = false

			idx := nextContentIndex
			nextContentIndex++
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]string{
					"type": "redacted_thinking",
					"data": data,
				},
			})
			sendInitialPing()
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": idx,
			})
			visibleOutputProduced = true
		}
		emitHiddenThinkingBlock := func() {
			if suppressRecoveryOutput || !hiddenThinkingObserved || thinkingBlockEmitted || stopFilter.Matched() != "" {
				return
			}
			startContentBlock("thinking")
			closeActiveBlock()
			hiddenThinkingObserved = false
		}

		emitTextDelta := func(text string) {
			if text == "" {
				return
			}
			emitHiddenThinkingBlock()
			startContentBlock("text")
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": activeBlockIndex,
				"delta": map[string]string{"type": "text_delta", "text": text},
			})
			visibleOutputProduced = true
		}

		sendText := func(text string, thinkingState int) {
			if thinkingState == 0 {
				text = completionGate.Push(text)
				if text == "" {
					return
				}
				if suppressRecoveryOutput && !completionGate.Marked() {
					return
				}
				text = stopFilter.Push(text)
				if text == "" {
					return
				}
				reasoningPhaseSealed = true
				emitTextDelta(text)
				return
			}
			if stopFilter.Matched() != "" {
				return
			}

			if !thinking {
				return
			}

			switch thinkingFormat {
			case "think":
				var outputText string
				switch thinkingState {
				case 1:
					outputText = "<think>" + text
				case 2:
					outputText = text
				case 3:
					outputText = text + "</think>"
				}
				if outputText == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": outputText},
				})
				visibleOutputProduced = true
			case "reasoning_content":
				if text == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
				visibleOutputProduced = true
			default:
				if thinkingOpts.OmitDisplay {
					if text != "" {
						hiddenThinkingObserved = true
					}
					return
				}
				if thinkingState == 3 && text == "" {
					if activeBlockType == "thinking" {
						closeActiveBlock()
					}
					return
				}
				if text != "" {
					startContentBlock("thinking")
					h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": text},
					})
					visibleOutputProduced = true
				}
				if thinkingState == 3 && activeBlockType == "thinking" {
					closeActiveBlock()
				}
			}
		}

		processClaudeText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				isThinking = false
			}

			if isThinking {
				if reasoningPhaseSealed {
					return
				}
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendText(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendText(text, 2)
				}
				return
			}

			if holdToolText {
				if forceFlush {
					textBuffer = ""
				}
				return
			}

			if eventThinkingOpen {
				sendText("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			if structuredOutput {
				// Start the protocol stream as soon as upstream produces visible
				// text, but only forward bytes from the first JSON object/array
				// through its matching close delimiter.
				ensureMessageStart()
				emitPrelude()
				sendInitialPing()
				sendText(structuredJSON.Push(text), 0)
				return
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, thinkingOpenTag)
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendText(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+len(thinkingOpenTag):]
						inThinkingBlock = true
						dropTagThinking = reasoningPhaseSealed || !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush {
						sendText(textBuffer, 0)
						textBuffer = ""
						break
					} else {
						ready, pending := splitBeforePartialTag(textBuffer, thinkingOpenTag)
						sendText(ready, 0)
						textBuffer = pending
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, thinkingCloseTag)
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+len(thinkingCloseTag):]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(textBuffer, 1)
									sendText("", 3)
								} else {
									sendText(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						ready, pending := splitBeforePartialTag(textBuffer, thinkingCloseTag)
						if ready != "" && !dropTagThinking {
							if !thinkingStarted {
								sendText(ready, 1)
								thinkingStarted = true
							} else {
								sendText(ready, 2)
							}
						}
						textBuffer = pending
						break
					}
				}
			}
		}

		beginToolUse := func(toolUseID, name string) {
			if _, exists := toolBlockIndexes[toolUseID]; exists || ignoredToolUses[toolUseID] {
				return
			}
			if forcedToolChoiceMode(payload) && !kiroPayloadAllowsToolName(payload, name) {
				ignoredToolUses[toolUseID] = true
				return
			}
			// Anthropic content blocks are strictly serial. Keep streaming the
			// active tool's arguments, but wait to open another block until this
			// tool has received its validated completion event.
			if activeToolUseID != "" {
				return
			}
			if payload.ToolChoiceMode == "required" && (len(toolUses) > 0 || len(toolBlockIndexes) > 0) {
				ignoredToolUses[toolUseID] = true
				return
			}
			clientID := normalizeClaudeToolUseID(toolUseID)
			toolClientIDs[toolUseID] = clientID
			flushPendingSignatureBlock()
			processClaudeText("", false, true)
			if narration := completionGate.CommitPendingForTool(); narration != "" {
				emitTextDelta(stopFilter.Push(narration))
			}
			emitTextDelta(stopFilter.Flush())
			if stopFilter.Matched() != "" {
				ignoredToolUses[toolUseID] = true
				return
			}
			reasoningPhaseSealed = true

			emitHiddenThinkingBlock()
			visibleOutputProduced = true
			ensureMessageStart()
			emitPrelude()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++
			toolBlockIndexes[toolUseID] = idx
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":   "tool_use",
					"id":     clientID,
					"name":   name,
					"input":  map[string]interface{}{},
					"caller": map[string]interface{}{"type": "direct"},
				},
			})
			activeToolUseID = toolUseID
			sendInitialPing()
		}
		emitToolUseInputDelta := func(toolUseID, delta string) {
			if delta == "" || ignoredToolUses[toolUseID] {
				return
			}
			idx, exists := toolBlockIndexes[toolUseID]
			if !exists {
				return
			}
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]interface{}{
					"type":         "input_json_delta",
					"partial_json": delta,
				},
			})
		}
		handleText := func(text string, isThinking bool) {
			if text == "" {
				return
			}
			if isThinking {
				if !thinking {
					suppressedReasoningObserved = true
					return
				}
				rawThinkingBuilder.WriteString(text)
				if suppressRecoveryOutput {
					return
				}
			} else {
				rawContentBuilder.WriteString(text)
			}
			if !isThinking {
				flushPendingSignatureBlock()
			}
			processClaudeText(text, isThinking, false)
		}
		handleReasoning := func(event KiroReasoningContentEvent) {
			if reasoningPhaseSealed {
				if event.Text != "" || event.Signature != "" || event.RedactedContent != "" {
					suppressedReasoningObserved = true
				}
				return
			}
			if !thinking {
				if event.Text != "" || event.Signature != "" || event.RedactedContent != "" {
					suppressedReasoningObserved = true
				}
				return
			}
			if thinkingFormat == "thinking" && event.Signature != "" {
				pendingThinkingSignature = event.Signature
				reasoningSignature = event.Signature
			}
			if event.Text != "" {
				rawThinkingBuilder.WriteString(event.Text)
				if !suppressRecoveryOutput {
					processClaudeText(event.Text, true, false)
				}
			}
			if event.RedactedContent != "" {
				redactedThinking = append(redactedThinking, event.RedactedContent)
			}
			emitRedactedThinking(event.RedactedContent)
		}
		flushDeferredToolOutput := func() {
			deferred := deferredToolOutput
			deferredToolOutput = nil
			for _, emit := range deferred {
				emit()
			}
		}
		var completeToolUse func(KiroToolUse)
		completeToolUse = func(tu KiroToolUse) {
			if ignoredToolUses[tu.ToolUseID] {
				return
			}
			rawToolUseID := tu.ToolUseID
			if activeToolUseID != "" && activeToolUseID != rawToolUseID {
				deferredCompletedToolUses = append(deferredCompletedToolUses, tu)
				return
			}
			_, streamed := toolBlockIndexes[rawToolUseID]
			if !streamed {
				beginToolUse(rawToolUseID, tu.Name)
				if ignoredToolUses[rawToolUseID] {
					return
				}
				inputJSON, _ := json.Marshal(tu.Input)
				emitToolUseInputDelta(rawToolUseID, string(inputJSON))
			}
			tu.ToolUseID = toolClientIDs[rawToolUseID]
			toolUses = append(toolUses, tu)
			idx := toolBlockIndexes[rawToolUseID]
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": idx,
			})
			if activeToolUseID == rawToolUseID {
				activeToolUseID = ""
			}

			pendingTools := deferredCompletedToolUses
			deferredCompletedToolUses = nil
			for _, pendingTool := range pendingTools {
				completeToolUse(pendingTool)
			}
			if activeToolUseID == "" {
				flushDeferredToolOutput()
			}
		}

		callback := &KiroStreamCallback{
			OnStreamReady: onStreamReady,
			OnKeepAlive: func() {
				ensureMessageStart()
				h.sendSSE(w, flusher, "ping", map[string]interface{}{"type": "ping"})
				pingSent = true
				streamCommitted = true
			},
			OnText: func(text string, isThinking bool) {
				if text != "" {
					upstreamOutputStarted = true
				}
				if activeToolUseID != "" {
					deferredText := text
					deferredThinking := isThinking
					deferredToolOutput = append(deferredToolOutput, func() {
						handleText(deferredText, deferredThinking)
					})
					return
				}
				handleText(text, isThinking)
			},
			OnReasoning: func(event KiroReasoningContentEvent) {
				if event.Text != "" || event.Signature != "" || event.RedactedContent != "" {
					upstreamOutputStarted = true
				}
				if activeToolUseID != "" {
					deferredEvent := event
					deferredToolOutput = append(deferredToolOutput, func() {
						handleReasoning(deferredEvent)
					})
					return
				}
				handleReasoning(event)
			},
			OnToolUseStart: func(toolUseID, name string) {
				upstreamOutputStarted = true
				beginToolUse(toolUseID, name)
			},
			OnToolUseInputDelta: func(toolUseID, delta string) {
				if delta != "" {
					upstreamOutputStarted = true
				}
				emitToolUseInputDelta(toolUseID, delta)
			},
			OnToolUse: func(toolUse KiroToolUse) {
				upstreamOutputStarted = true
				completeToolUse(toolUse)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				_, exceeded := observeKiroContextUsage(payload, model, pct)
				if exceeded {
					contextWindowExceeded = true
				}
			},
		}

		call := h.kiroCaller
		if call == nil {
			call = CallKiroAPI
		}
		err := call(account, payload, callback)
		err = resolveKiroOutputTruncation(payload, err, upstreamStopReason, rawContentBuilder.String(), completedKiroToolNames(toolUses))
		if err == nil {
			if completionErr := agentCompletionResponseError(
				payload,
				suppressedReasoningObserved || hiddenThinkingObserved || rawThinkingBuilder.Len() > 0 || pendingThinkingSignature != "" || len(redactedThinking) > 0,
				completionGate.VisibleText(),
				completionGate.Marked(),
				completedKiroToolNames(toolUses),
				upstreamStopReason,
			); completionErr != nil {
				err = completionErr
				prepareReasoningOnlyRetry(payload, err)
				if isUpstreamAgentActionOnlyError(err) {
					payload.agentActionContinuationTimeout = true
				}
			}
		}
		if shouldFinishStreamAfterAgentContinuation(payload, err, len(toolUses)) {
			logger.Warnf("[KiroAPI] finishing Claude stream after bounded agent continuation request_id=%s account_id=%s phase=%s completions=%d actions=%d full_context=%d: %v",
				msgID, account.ID, kiroAgentRecoveryPhase(payload, err), payload.AgentCompletionRetries,
				payload.AgentActionContinuations, payload.AgentFullContextRecoveries, err)
			err = nil
			upstreamStopReason = "end_turn"
		}
		h.releaseAccount(account)
		if err != nil {
			// Bill whatever this attempt already produced before deciding how to
			// recover. Every branch below either retries or returns, so a later
			// settlement would never see these tokens.
			discarded := estimateClaudeOutputTokensWithRedacted(
				rawContentBuilder.String(), rawThinkingBuilder.String(), redactedThinking, toolUses,
			)
			discarded = discardedAttemptOutputTokens(discarded, outputTokens)
			if discarded > 0 || inputTokens > 0 || credits > 0 {
				discardedInput := clampClaudeInputTokensToModelWindow(
					model, discardedAttemptInputTokens(estimatedInputTokens, inputTokens),
				)
				retryUsage.add(h.settleDiscardedAttempt(
					"claude", model, account.ID, apiKeyID,
					discardedInput, discarded, credits,
					time.Since(reqStart).Milliseconds(),
				))
			}
			if isUpstreamIncompleteResponseError(err) || isUpstreamOutputTruncatedError(err) {
				if eventThinkingOpen {
					sendText("", 3)
					eventThinkingOpen = false
					thinkingStarted = false
				}
				flushPendingSignatureBlock()
				closeActiveBlock()
			}
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			lastErr = err
			if takeKiroOutputContinuationRetry(payload, err) {
				retryUsage.markLastVisibleContinuation()
				retryUsage.markLastContinuationAttemptOutput()
				retrySameID = account.ID
				attemptLimit++
				continue
			}
			if isKiroInputTooLongError(err) {
				if h.retryAgentRecoveryAfterInputTooLong("Claude stream", payload, account, &retrySameID, err) {
					attemptLimit++
					continue
				}
				h.recordFailureWithDetails("claude", model, account.ID, err)
				sendClaudeStreamFailure(http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			if code, protocolErr := kiroStreamProtocolErrorCode(err); protocolErr {
				if !upstreamOutputStarted && isKiroToolJSONError(err) && toolJSONRetryCounts[account.ID] < maxKiroToolJSONRetries && h.allowUpstreamRetry() {
					toolJSONRetryCounts[account.ID]++
					attemptLimit++
					// Prefer a fresh account for malformed tool fragments. A Kiro
					// session can cache the truncated stream; only fall back to the
					// same account when it is the sole available account.
					if h.pool != nil && h.pool.Count() > 1 {
						excluded[account.ID] = true
					} else {
						retrySameID = account.ID
					}
					logger.Warnf("[KiroAPI] retrying Claude stream after truncated tool JSON account_id=%s retry=%d/%d: %v", account.ID, toolJSONRetryCounts[account.ID], maxKiroToolJSONRetries, err)
					continue
				}
				canRetry := !upstreamOutputStarted || isUpstreamIncompleteResponseError(err)
				if isUpstreamIncompleteResponseError(err) && !agentCompletionRetryAllowed(payload, err) {
					canRetry = false
				}
				if canRetry && isRetryableKiroStreamError(err) {
					retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
					if retryErr != nil {
						return
					}
					if retried {
						attemptLimit++
						retryNumber, retryLimit := kiroPreResponseRetryProgress(payload, preResponseRetryCount, err)
						logger.Warnf("[KiroAPI] retrying Claude stream after pre-response protocol error request_id=%s account_id=%s phase=%s code=%s retry=%d/%d completions=%d actions=%d full_context=%d: %v",
							msgID, account.ID, kiroAgentRecoveryPhase(payload, err), code, retryNumber, retryLimit,
							payload.AgentCompletionRetries, payload.AgentActionContinuations, payload.AgentFullContextRecoveries, err)
						continue
					}
				}
				h.recordFailureWithDetails("claude", model, account.ID, err)
				sendClaudeStreamFailure(http.StatusBadGateway, code, err.Error())
				return
			}
			if isRateLimitError(err) {
				decision := h.applyRateLimitPolicy(account, retryCounts, excluded, err)
				if decision.Return429 {
					h.recordFailureWithDetails("claude", model, account.ID, err)
					sendClaudeStreamFailure(http.StatusTooManyRequests, "rate_limit_error", err.Error())
					return
				}
				retrySameID = decision.RetrySameID
				if !upstreamOutputStarted {
					if !h.waitBeforeRateLimitRetry(payload, decision) {
						return
					}
					continue
				}
				h.recordFailureWithDetails("claude", model, account.ID, err)
				sendClaudeStreamFailure(http.StatusTooManyRequests, "rate_limit_error", err.Error())
				return
			}
			if isKiroClientRequestError(err) {
				h.recordFailureWithDetails("claude", model, account.ID, err)
				sendClaudeStreamFailure(http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			if !upstreamOutputStarted && isRetryableKiroStreamError(err) {
				retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
				if retryErr != nil {
					return
				}
				if retried {
					attemptLimit++
					retryNumber, retryLimit := kiroPreResponseRetryProgress(payload, preResponseRetryCount, err)
					logger.Warnf("[KiroAPI] retrying Claude stream after transient pre-response error account_id=%s retry=%d/%d: %v", account.ID, retryNumber, retryLimit, err)
					continue
				}
			}
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			if !upstreamOutputStarted {
				continue
			}
			h.recordFailureWithDetails("claude", model, account.ID, err)
			sendClaudeStreamFailure(http.StatusBadGateway, "api_error", err.Error())
			return
		}

		processClaudeText("", false, true)
		if terminalText := completionGate.CommitPendingForTerminal(); terminalText != "" {
			emitTextDelta(stopFilter.Push(terminalText))
		}
		emitTextDelta(stopFilter.Flush())
		if eventThinkingOpen {
			sendText("", 3)
		}

		if (structuredOutput || holdToolText) && strings.TrimSpace(rawContentBuilder.String()) != "" {
			visibleOutputProduced = true
		}
		if !visibleOutputProduced {
			// An empty successful upstream turn is still a valid Anthropic message.
			// Do not turn it into a gateway error or cool down a healthy account.
			logger.Warnf("[KiroAPI] empty upstream output endpoint=claude model=%s account_id=%s account=%s; returning an empty message", model, account.ID, accountEmailForLog(account))
		}

		flushPendingSignatureBlock()
		closeActiveBlock()

		// Keep internal accounting anchored to the client-visible request. Kiro's
		// context usage is exposed separately in terminal usage so clients can
		// compact against the context Kiro actually consumed.
		executorInputTokens := clampClaudeInputTokensToModelWindow(model, estimatedInputTokens)
		inputTokens = executorInputTokens
		if prelude != nil {
			inputTokens = clampClaudeInputTokensToModelWindow(model, estimatedInputTokens+prelude.InputTokens)
		}
		if inputTokens <= 0 {
			inputTokens = 1
		}
		if _, exceeded := currentKiroContextUsage(payload, model); exceeded {
			contextWindowExceeded = true
		}
		// Keep the cache split anchored to the client-visible request total so
		// hidden Kiro prompt tokens cannot inflate downstream billing metadata.
		cacheUsage = reanchorClaudeCacheUsage(cacheProfile, cacheUsage, minInt(executorInputTokens, inputTokens))
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		outputContent, _ = filterAgentCompletionContent(payload, outputContent, len(toolUses))
		if structuredOutput {
			outputContent = normalizeStructuredOutputContent(outputContent)
		}
		outputContent, matchedStopSequence := truncateAtStopSequence(outputContent, payload.StopSequences)
		if len(toolUses) > 0 && len(payload.StopSequences) == 0 &&
			(payload.ToolChoiceMode == "required" || payload.ToolChoiceMode == "specific") {
			outputContent = ""
		}
		thinkingOutput := rawThinkingBuilder.String()
		if thinking && thinkingOutput == "" && extractedReasoning != "" {
			thinkingOutput = extractedReasoning
		}
		if !thinking {
			thinkingOutput = ""
		}
		if ((structuredOutput && !structuredJSON.Emitted()) || (holdToolText && len(toolUses) == 0)) && outputContent != "" {
			ensureMessageStart()
			emitPrelude()
			emitTextDelta(outputContent)
			closeActiveBlock()
		}
		thinkingOnly := (thinkingBlockEmitted || len(redactedThinking) > 0) && outputContent == "" && len(toolUses) == 0 && matchedStopSequence == ""
		if thinkingOnly {
			if upstreamStopReason == "" {
				upstreamStopReason = "max_tokens"
			}
		}
		executorOutputTokens := estimateClaudeOutputTokensWithRedacted(outputContent, thinkingOutput, redactedThinking, toolUses)
		executorCredits := credits
		outputTokens = executorOutputTokens
		if prelude != nil {
			outputTokens += prelude.OutputTokens
			credits += prelude.Credits
		}

		h.recordSuccessForApiKeyWithCache(apiKeyID, inputTokens, outputTokens, credits, cacheUsage)
		if prelude != nil {
			prelude.RetryUsage.recordForAPIKey(apiKeyID)
		}
		h.pool.RecordSuccess(account.ID)
		// h.pool.RecordLatency(account.ID, float64(time.Since(reqStart).Milliseconds()))
		// ↑ dispatch-only latency EWMA (commit fbc99b1), absent on this cache-only
		//   branch off main. No-op here; stripped so handler compiles against main's
		//   pool. Re-added verbatim when dispatch lands.
		h.pool.UpdateStats(account.ID, executorInputTokens+executorOutputTokens, executorCredits)
		promptCacheTrackerForProfile(h.promptCache, cacheProfile).Update(account.ID, cacheProfile)
		h.recordSuccessLog("claude", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		stopReason := resolveClaudeStopReason(len(toolUses), contextWindowExceeded, upstreamStopReason)
		var stopSequence interface{}
		if matchedStopSequence != "" {
			stopReason = "stop_sequence"
			stopSequence = matchedStopSequence
		}

		ensureMessageStart()
		emitPrelude()
		sendInitialPing()
		publicInputTokens := claudePublicInputTokens(payload, model, inputTokens)
		terminalCacheUsage := claudeTerminalCacheUsage(cacheProfile, payload, model, inputTokens, cacheUsage)
		publicInputTokens, terminalCacheUsage = retryUsage.apply(publicInputTokens, terminalCacheUsage)
		publicOutputTokens := retryUsage.applyOutput(outputTokens)
		finalUsage := buildClaudeTerminalUsageMap(publicInputTokens, publicOutputTokens, terminalCacheUsage)
		addKiroContextUsage(finalUsage, payload, model)
		addClaudeReasoningUsageDetails(finalUsage, ClaudeReasoningResult{
			Text:      thinkingOutput,
			Signature: reasoningSignature,
			Redacted:  redactedThinking,
		}, thinking && thinkingBlockEmitted)
		h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   stopReason,
				"stop_sequence": stopSequence,
				"stop_details":  nil,
			},
			"usage": finalUsage,
			"context_management": map[string]interface{}{
				"applied_edits": []interface{}{},
			},
		})

		h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		return
	}

	if lastErr == nil {
		h.recordFailureWithDetails("claude", model, "", fmt.Errorf("No available accounts"))
		sendClaudeStreamFailure(http.StatusServiceUnavailable, "api_error", "No available accounts")
		return
	}

	h.recordFailureWithDetails("claude", model, "", lastErr)
	if isAccountRefreshInProgressError(lastErr) {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(expiredAccountRefreshWait.Seconds())))
		sendClaudeStreamFailure(accountRefreshInProgressStatus, "account_refreshing", accountRefreshInProgressMessage)
		return
	}
	if isAllAccountsConcurrencyFullError(lastErr) {
		sendClaudeStreamFailure(http.StatusServiceUnavailable, "overloaded_error", allAccountsConcurrencyFullMessage)
		return
	}
	if isRateLimitError(lastErr) {
		sendClaudeStreamFailure(http.StatusTooManyRequests, "rate_limit_error", lastErr.Error())
		return
	}
	sendClaudeStreamFailure(http.StatusInternalServerError, "api_error", lastErr.Error())
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}

func writeSSEKeepAlive(w http.ResponseWriter, flusher http.Flusher) {
	fmt.Fprint(w, ": keep-alive\n\n")
	flusher.Flush()
}

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// saveStats 保存统计到配置文件
func (h *Handler) saveStats() {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	config.UpdateRuntimeStats(
		int(atomic.LoadInt64(&h.totalRequests)),
		int(atomic.LoadInt64(&h.successRequests)),
		int(atomic.LoadInt64(&h.failedRequests)),
		int(atomic.LoadInt64(&h.totalTokens)),
		h.getCredits(),
		h.pool.SnapshotAccountStats(),
	)
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
func (h *Handler) recordSuccessForApiKey(apiKeyID string, inputTokens, outputTokens int, credits float64) {
	h.recordSuccessForApiKeyWithCache(apiKeyID, inputTokens, outputTokens, credits, promptCacheUsage{})
}

// estimateOpenAIDiscardedOutput mirrors the success path's arithmetic, which
// counts each tool call's name and arguments directly instead of going through
// KiroToolUse.
func estimateOpenAIDiscardedOutput(content, reasoning string, toolCalls []ToolCall) int {
	total := estimateApproxTokens(content) + estimateApproxTokens(reasoning)
	for _, tc := range toolCalls {
		total += estimateApproxTokens(tc.Function.Name)
		total += estimateApproxTokens(tc.Function.Arguments)
	}
	return total
}

// settleDiscardedAttempt bills an attempt whose output never reached the client:
// a structured-output validation retry, or an agent turn that ended without a
// marker or tool call. Kiro has already produced these tokens upstream, so
// skipping them understates usage and lets a reasoning loop burn quota that no
// counter or quota gate can observe. It settles inside the attempt's own loop
// iteration against the account that produced it. The returned usage is also
// carried into a later successful response as cache-read input.
func (h *Handler) settleDiscardedAttempt(endpoint, model, accountID, apiKeyID string, inputTokens, outputTokens int, credits float64, elapsedMs int64) discardedAttemptUsage {
	attempt := newDiscardedAttemptUsage(inputTokens, outputTokens, credits)
	h.recordSuccessForApiKey(apiKeyID, attempt.InputTokens, attempt.OutputTokens, attempt.Credits)
	if h.pool != nil {
		h.pool.UpdateStats(accountID, attempt.totalTokens(), attempt.Credits)
	}
	h.recordSuccessLog(endpoint, model, accountID, attempt.totalTokens(), attempt.Credits, elapsedMs)
	return attempt
}

func (h *Handler) recordSuccessForApiKeyWithCache(apiKeyID string, inputTokens, outputTokens int, credits float64, cacheUsage promptCacheUsage) {
	h.statsMu.Lock()
	h.recordSuccess(inputTokens, outputTokens, credits)
	if h.traffic != nil {
		h.traffic.Record(TrafficObservation{
			InputTokens:         int64(billedClaudeInputTokens(inputTokens, cacheUsage)),
			OutputTokens:        int64(outputTokens),
			CacheCreationTokens: int64(cacheUsage.CacheCreationInputTokens),
			CacheReadTokens:     int64(cacheUsage.CacheReadInputTokens),
		})
	}
	h.statsMu.Unlock()

	if apiKeyID != "" {
		if err := config.RecordApiKeyUsage(apiKeyID, int64(inputTokens+outputTokens), credits); err != nil {
			logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
		}
	}
}

func reanchorClaudeCacheUsage(profile *promptCacheProfile, usage promptCacheUsage, finalInputTokens int) promptCacheUsage {
	if profile == nil || finalInputTokens <= 0 {
		return usage
	}
	return clampCacheReadToModelWindow(
		usage.splitAgainstTotal(profile.TotalInputTokens, finalInputTokens),
		profile.Model,
	)
}

func clampClaudeInputTokensToModelWindow(model string, inputTokens int) int {
	limit := getContextWindowSize(model)
	if limit > 0 && inputTokens > limit {
		return limit
	}
	return inputTokens
}

// recordFailureWithDetails records a failure and stores it in the request logs.
func (h *Handler) recordFailureWithDetails(endpoint, model, accountID string, err error) {
	h.statsMu.Lock()
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)
	if h.traffic != nil {
		h.traffic.Record(TrafficObservation{})
	}
	h.statsMu.Unlock()

	if err == nil {
		return
	}

	errMsg := err.Error()
	errType := classifyError(errMsg)

	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		Status:    "error",
		Error:     errMsg,
		ErrorType: errType,
	}

	h.appendRequestLog(entry)
}

// recordSuccessLog records a successful request in the request logs.
func (h *Handler) recordSuccessLog(endpoint, model, accountID string, tokens int, credits float64, durationMs int64) {
	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		Status:    "success",
		Tokens:    tokens,
		Credits:   credits,
		Duration:  durationMs,
	}

	h.appendRequestLog(entry)
}

func (h *Handler) appendRequestLog(entry RequestLog) {
	h.requestLogsMu.Lock()
	if h.requestLogs == nil {
		h.requestLogs = make([]RequestLog, 0, requestLogsMaxSize)
	}
	if len(h.requestLogs) >= requestLogsMaxSize {
		h.requestLogs = h.requestLogs[1:]
	}
	h.requestLogs = append(h.requestLogs, entry)
	h.requestLogsMu.Unlock()
}

// classifyError categorizes an error message into a type for display.
func classifyError(msg string) string {
	switch {
	case isQuotaErrorMessage(msg):
		return "quota"
	case isOverageErrorMessage(msg):
		return "overage"
	case isSuspensionErrorMessage(msg):
		return "suspended"
	case isAuthErrorMessage(msg):
		return "auth"
	case isProfileUnavailableErrorMessage(msg):
		return "profile"
	case strings.Contains(strings.ToLower(msg), "web search mcp"):
		return "websearch"
	case strings.Contains(strings.ToLower(msg), "upstream did not complete the agent turn"):
		return "agent"
	default:
		return "unknown"
	}
}

// getRequestLogs returns a copy of request logs (newest first).
func (h *Handler) getRequestLogs() []RequestLog {
	h.requestLogsMu.RLock()
	defer h.requestLogsMu.RUnlock()
	if len(h.requestLogs) == 0 {
		return []RequestLog{}
	}
	result := make([]RequestLog, len(h.requestLogs))
	for i, e := range h.requestLogs {
		result[len(h.requestLogs)-1-i] = e
	}
	return result
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, payload *KiroPayload, model, responseModel string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	payload = cloneKiroPayload(payload)
	excluded := make(map[string]bool)
	retryCounts := make(map[string]int)
	retrySameID := ""
	var lastErr error
	reqStart := time.Now()
	structuredRetries := 0
	preResponseRetryCount := 0
	cacheTracker := promptCacheTrackerForProfile(h.promptCache, cacheProfile)
	cacheUsage := promptCacheUsage{}
	cacheComputed := false
	var retryUsage retryBillingUsage

	attemptLimit := h.maxAccountAttempts()
	for attempt := 0; attempt < attemptLimit; attempt++ {
		account, allFull := h.acquireAccountForModel(model, excluded, retrySameID, kiroPayloadContext(payload))
		retrySameID = ""
		if account == nil {
			if h.startExpiredAccountRefreshes(model, excluded) {
				lastErr = errAccountRefreshInProgress
			} else if allFull {
				lastErr = fmt.Errorf(allAccountsConcurrencyFullMessage)
			}
			break
		}
		if err := h.ensureValidTokenForPayload(account, payload); err != nil {
			h.releaseAccount(account)
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		if !cacheComputed {
			cacheUsage = cacheTracker.Compute(account.ID, cacheProfile)
			cacheComputed = true
		}

		var content string
		var thinkingContent string
		var thinkingSignature string
		var redactedThinking []string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var contextWindowExceeded bool
		var upstreamStopReason string
		var suppressedReasoningObserved bool
		var matchedStopSequence string
		updateStopSequenceState := func() {
			if matchedStopSequence != "" {
				return
			}
			visibleContent, _ := extractThinkingFromContent(content)
			_, matchedStopSequence = truncateAtStopSequence(visibleContent, payload.StopSequences)
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					if !thinking {
						suppressedReasoningObserved = true
						return
					}
					thinkingContent += text
					return
				}
				content += text
				updateStopSequenceState()
			},
			OnReasoning: func(event KiroReasoningContentEvent) {
				if !thinking {
					if event.Text != "" || event.Signature != "" || event.RedactedContent != "" {
						suppressedReasoningObserved = true
					}
					return
				}
				thinkingContent += event.Text
				if event.Signature != "" {
					thinkingSignature = event.Signature
				}
				if event.RedactedContent != "" {
					redactedThinking = append(redactedThinking, event.RedactedContent)
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				tu.ToolUseID = normalizeClaudeToolUseID(tu.ToolUseID)
				if matchedStopSequence == "" && (payload.ToolChoiceMode != "required" || len(toolUses) == 0) {
					toolUses = append(toolUses, tu)
				}
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				_, exceeded := observeKiroContextUsage(payload, model, pct)
				if exceeded {
					contextWindowExceeded = true
				}
			},
		}

		call := h.kiroCaller
		if call == nil {
			call = CallKiroAPI
		}
		err := call(account, payload, callback)
		err = resolveKiroOutputTruncation(payload, err, upstreamStopReason, content, completedKiroToolNames(toolUses))
		if err == nil {
			visibleContent, _ := extractThinkingFromContent(content)
			visibleContent, markerSeen := filterAgentCompletionContent(payload, visibleContent, len(toolUses))
			if completionErr := agentCompletionResponseError(
				payload,
				suppressedReasoningObserved || thinkingContent != "" || thinkingSignature != "" || len(redactedThinking) > 0,
				visibleContent,
				markerSeen,
				completedKiroToolNames(toolUses),
				upstreamStopReason,
			); completionErr != nil {
				err = completionErr
				prepareReasoningOnlyRetry(payload, err)
			}
		}
		h.releaseAccount(account)
		if err != nil {
			discarded := estimateClaudeOutputTokensWithRedacted(
				content, thinkingContent, redactedThinking, toolUses,
			)
			discarded = discardedAttemptOutputTokens(discarded, outputTokens)
			if discarded > 0 || inputTokens > 0 || credits > 0 {
				retryUsage.add(h.settleDiscardedAttempt(
					"claude", model, account.ID, apiKeyID,
					clampClaudeInputTokensToModelWindow(model, discardedAttemptInputTokens(estimatedInputTokens, inputTokens)), discarded, credits,
					time.Since(reqStart).Milliseconds(),
				))
			}
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			lastErr = err
			if takeKiroOutputContinuationRetry(payload, err) {
				retryUsage.markLastContinuationAttemptOutput()
				retrySameID = account.ID
				attemptLimit++
				continue
			}
			if isKiroInputTooLongError(err) {
				if h.retryAgentRecoveryAfterInputTooLong("Claude non-stream", payload, account, &retrySameID, err) {
					attemptLimit++
					continue
				}
				h.recordFailureWithDetails("claude", model, account.ID, err)
				h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			if !isKiroToolJSONError(err) && isRetryableKiroStreamError(err) &&
				(!isUpstreamIncompleteResponseError(err) || agentCompletionRetryAllowed(payload, err)) {
				retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
				if retryErr != nil {
					return
				}
				if retried {
					attemptLimit++
					retryNumber, retryLimit := kiroPreResponseRetryProgress(payload, preResponseRetryCount, err)
					logger.Warnf("[KiroAPI] retrying Claude non-stream after transient pre-response error account_id=%s retry=%d/%d: %v", account.ID, retryNumber, retryLimit, err)
					continue
				}
			}
			if code, protocolErr := kiroStreamProtocolErrorCode(err); protocolErr {
				h.recordFailureWithDetails("claude", model, account.ID, err)
				h.sendClaudeError(w, http.StatusBadGateway, code, err.Error())
				return
			}
			if isRateLimitError(err) {
				decision := h.applyRateLimitPolicy(account, retryCounts, excluded, err)
				if decision.Return429 {
					h.recordFailureWithDetails("claude", model, account.ID, err)
					h.sendClaudeError(w, http.StatusTooManyRequests, "rate_limit_error", err.Error())
					return
				}
				retrySameID = decision.RetrySameID
				if !h.waitBeforeRateLimitRetry(payload, decision) {
					return
				}
				continue
			}
			if isKiroClientRequestError(err) {
				h.recordFailureWithDetails("claude", model, account.ID, err)
				h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		thinkingFormat := thinkingOpts.Format
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		finalContent, _ = filterAgentCompletionContent(payload, finalContent, len(toolUses))
		finalContent = prependKiroOutputContinuation(payload, finalContent)
		if payload.StructuredOutput {
			finalContent = normalizeStructuredOutputContent(finalContent)
			if validationErr := validateStructuredOutputContent(
				finalContent, payload.StructuredOutputType, payload.StructuredOutputSchema,
			); validationErr != nil && structuredRetries == 0 {
				structuredRetries++
				// Settle the discarded attempt against the account that produced it,
				// in this iteration. Deferring it to the successful attempt made the
				// final turn report tokens it never generated, and lost the usage
				// entirely whenever the retry chain ended in an error response.
				discardedInput := clampClaudeInputTokensToModelWindow(
					model, discardedAttemptInputTokens(estimatedInputTokens, inputTokens),
				)
				if discardedInput <= 0 {
					discardedInput = 1
				}
				discardedOutput := estimateClaudeOutputTokensWithRedacted(
					finalContent, thinkingContent, redactedThinking, toolUses,
				)
				discardedOutput = discardedAttemptOutputTokens(discardedOutput, outputTokens)
				retryUsage.add(h.settleDiscardedAttempt(
					"claude", model, account.ID, apiKeyID,
					discardedInput, discardedOutput, credits,
					time.Since(reqStart).Milliseconds(),
				))
				retrySameID = account.ID
				logger.Warnf(
					"[KiroAPI] structured output validation failed endpoint=claude model=%s account_id=%s; retrying once: %v",
					model, account.ID, validationErr,
				)
				continue
			}
		}
		finalContent, finalMatchedStopSequence := truncateAtStopSequence(finalContent, payload.StopSequences)
		if matchedStopSequence == "" {
			matchedStopSequence = finalMatchedStopSequence
		}
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
			thinkingSignature = ""
			redactedThinking = nil
		}
		if len(toolUses) > 0 && payload.AgentCompletionRetries > 0 {
			rawThinkingContent = ""
			thinkingSignature = ""
			redactedThinking = nil
		}
		// Anthropic accepts an optional text block before tool_use, but many
		// clients (and the native tool contract) expect a pure tool turn. Kiro
		// commonly narrates its intent before emitting the structured call; drop
		// that narration from non-stream responses so the tool call is unambiguous.
		if len(toolUses) > 0 && len(payload.StopSequences) == 0 {
			finalContent = ""
		}
		if finalContent == "" && len(toolUses) == 0 && (rawThinkingContent != "" || thinkingSignature != "" || len(redactedThinking) > 0) && upstreamStopReason == "" {
			upstreamStopReason = "max_tokens"
		}

		// Internal accounting remains based on the client-visible request.
		inputTokens = clampClaudeInputTokensToModelWindow(model, estimatedInputTokens)
		if inputTokens <= 0 {
			inputTokens = 1
		}
		if _, exceeded := currentKiroContextUsage(payload, model); exceeded {
			contextWindowExceeded = true
		}
		// Keep the cache split anchored to the client-visible request total so
		// hidden Kiro prompt tokens cannot inflate downstream billing metadata.
		cacheUsage = reanchorClaudeCacheUsage(cacheProfile, cacheUsage, inputTokens)
		outputTokens = estimateClaudeOutputTokensWithRedacted(finalContent, rawThinkingContent, redactedThinking, toolUses)

		// Each attempt settles in its own iteration, so this reports only what this
		// attempt generated. Client-visible usage stays aligned with the response.
		attemptOutputTokens := retryUsage.finalAttemptOutputTokens(outputTokens)
		h.recordSuccessForApiKeyWithCache(apiKeyID, inputTokens, attemptOutputTokens, credits, cacheUsage)
		h.pool.RecordSuccess(account.ID)
		// h.pool.RecordLatency(account.ID, float64(time.Since(reqStart).Milliseconds()))
		// ↑ dispatch-only latency EWMA (commit fbc99b1), absent on this cache-only
		//   branch off main. No-op here; stripped so handler compiles against main's
		//   pool. Re-added verbatim when dispatch lands.
		h.pool.UpdateStats(account.ID, inputTokens+attemptOutputTokens, credits)
		promptCacheTrackerForProfile(h.promptCache, cacheProfile).Update(account.ID, cacheProfile)
		h.recordSuccessLog("claude", model, account.ID, inputTokens+attemptOutputTokens, credits, time.Since(reqStart).Milliseconds())

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}

		if thinking && responseThinkingContent != "" {
			switch thinkingFormat {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			default:
			}
		}

		reasoningResult := ClaudeReasoningResult{Text: responseThinkingContent}
		if thinkingFormat == "thinking" {
			reasoningResult.Signature = thinkingSignature
			reasoningResult.Redacted = redactedThinking
		}
		publicInputTokens := claudePublicInputTokens(payload, model, inputTokens)
		responseCacheUsage := claudeTerminalCacheUsage(cacheProfile, payload, model, inputTokens, cacheUsage)
		publicInputTokens, responseCacheUsage = retryUsage.apply(publicInputTokens, responseCacheUsage)
		resp := KiroToClaudeResponseWithReasoning(finalContent, reasoningResult, includeEmptyThinkingBlock, toolUses, publicInputTokens, outputTokens, responseModel)
		if thinking {
			resp.Usage.OutputTokensDetails = claudeReasoningUsageDetails(ClaudeReasoningResult{
				Text:      rawThinkingContent,
				Signature: thinkingSignature,
				Redacted:  redactedThinking,
			}, includeEmptyThinkingBlock)
		}
		resp.StopReason = resolveClaudeStopReason(len(toolUses), contextWindowExceeded, upstreamStopReason)
		if matchedStopSequence != "" {
			resp.StopReason = "stop_sequence"
			resp.StopSequence = &matchedStopSequence
		}
		resp.Usage.InputTokens = billedClaudeInputTokens(publicInputTokens, responseCacheUsage)
		resp.Usage.CacheCreationInputTokens = responseCacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = responseCacheUsage.CacheReadInputTokens
		resp.Usage.KiroContextUsage = kiroContextUsage(payload, model)
		if cacheProfile != nil {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: responseCacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: responseCacheUsage.CacheCreation1hInputTokens,
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.recordFailureWithDetails("claude", model, "", fmt.Errorf("No available accounts"))
		h.sendClaudeError(w, http.StatusServiceUnavailable, "api_error", "No available accounts")
		return
	}

	h.recordFailureWithDetails("claude", model, "", lastErr)
	if isAccountRefreshInProgressError(lastErr) {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(expiredAccountRefreshWait.Seconds())))
		h.sendClaudeError(w, accountRefreshInProgressStatus, "account_refreshing", accountRefreshInProgressMessage)
		return
	}
	if isAllAccountsConcurrencyFullError(lastErr) {
		h.sendClaudeError(w, http.StatusServiceUnavailable, "overloaded_error", allAccountsConcurrencyFullMessage)
		return
	}
	if isRateLimitError(lastErr) {
		h.sendClaudeError(w, http.StatusTooManyRequests, "rate_limit_error", lastErr.Error())
		return
	}
	h.sendClaudeError(w, http.StatusInternalServerError, "api_error", lastErr.Error())
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	apiKeyID := apiKeyIDFromContext(r.Context())
	cacheTracker, cachePolicy := h.promptCacheRuntimeForAPIKey(apiKeyID)
	effectivePolicyID := cachePolicy.ID
	if effectivePolicyID == "" {
		effectivePolicyID = "global"
	}
	w.Header().Set("X-Kiro-Api-Key-Matched", strconv.FormatBool(apiKeyID != ""))
	w.Header().Set("X-Kiro-Prompt-Cache-Policy", effectivePolicyID)

	var req OpenAIRequest
	var body []byte
	var err error
	if cachePolicy.CacheMode == config.CacheModeFunctionSimulation {
		body, err = readRequestBodyWithLimit(w, r, claudeMaxRequestBodyBytes)
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
	} else {
		err = decodeRequestJSONWithLimit(w, r, claudeMaxRequestBodyBytes, &req)
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.sendOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds 32 MiB")
			return
		}
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	requestedModel := req.Model
	actualModel, thinking := resolveOpenAIThinkingMode(req.Model, req.ReasoningEffort, thinkingCfg.Suffix)
	req.Model = actualModel
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)

	kiroPayload := bindKiroPayloadContext(OpenAIToKiro(&req, thinking), r.Context())
	if _, err := preflightKiroPayload(kiroPayload); err != nil {
		h.recordFailureWithDetails("openai", req.Model, "", err)
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	requestBodyFingerprint := ""
	if cachePolicy.CacheMode == config.CacheModeFunctionSimulation {
		requestBodyFingerprint = functionCacheRequestBodyFingerprint(body)
	}
	var cacheProfile *promptCacheProfile
	if cacheTracker != nil {
		cacheReq := openAIRequestForPromptCache(&req, thinking)
		isolationSeed := claudePromptCacheIsolationSeed(nil, apiKeyID, extractProvidedKey(r))
		cacheProfile = cacheTracker.BuildClaudeProfileForRequestWithPolicy(
			cacheReq, estimatedInputTokens, isolationSeed, requestBodyFingerprint, cachePolicy,
		)
		if cacheProfile != nil {
			cacheProfile.requestTiming = requestTimingFromContext(r.Context())
		}
	}

	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		h.handleOpenAIStreamWithCache(w, kiroPayload, req.Model, requestedModel, thinking, includeUsage, estimatedInputTokens, cacheProfile, apiKeyID)
	} else {
		h.handleOpenAINonStreamWithCache(w, kiroPayload, req.Model, requestedModel, thinking, estimatedInputTokens, cacheProfile, apiKeyID)
	}
}

// OpenAI Chat prompt-cache plumbing.
// openAIRequestForPromptCache maps the prompt-bearing OpenAI fields to the
// cache tracker's canonical Claude shape. Tool call/result blocks retain their
// linkage so volatile call IDs are normalized by the existing cache logic.
func openAIRequestForPromptCache(req *OpenAIRequest, thinking bool) *ClaudeRequest {
	if req == nil {
		return nil
	}
	cacheReq := &ClaudeRequest{
		Model:      req.Model,
		ToolChoice: req.ToolChoice,
		Messages:   make([]ClaudeMessage, 0, len(req.Messages)),
	}

	var systemPrompt strings.Builder
	for _, message := range req.Messages {
		role := strings.TrimSpace(message.Role)
		if role == "system" {
			if text := extractOpenAIMessageText(message.Content); text != "" {
				systemPrompt.WriteString(text)
				systemPrompt.WriteByte('\n')
			}
			continue
		}

		cacheRole := role
		content := message.Content
		switch role {
		case "assistant":
			blocks := openAIContentBlocksForPromptCache(content)
			for _, call := range message.ToolCalls {
				var input interface{}
				if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
					input = call.Function.Arguments
				}
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    call.ID,
					"name":  call.Function.Name,
					"input": input,
				})
			}
			content = blocks
		case "tool":
			cacheRole = "user"
			content = []interface{}{map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": message.ToolCallID,
				"content":     message.Content,
			}}
		}
		cacheReq.Messages = append(cacheReq.Messages, ClaudeMessage{Role: cacheRole, Content: content})
	}
	if systemPrompt.Len() > 0 {
		cacheReq.System = systemPrompt.String()
	}

	choice, _ := resolveToolChoice(req.ToolChoice)
	kiroTools, _ := convertOpenAIToolsWithNameMap(filterOpenAIToolsForChoice(req.Tools, choice))
	cacheReq.Tools = make([]ClaudeTool, 0, len(kiroTools))
	for _, tool := range kiroTools {
		cacheReq.Tools = append(cacheReq.Tools, ClaudeTool{
			Name:        tool.ToolSpecification.Name,
			Description: tool.ToolSpecification.Description,
			InputSchema: tool.ToolSpecification.InputSchema.JSON,
		})
	}
	return cloneClaudeRequestForThinking(cacheReq, thinking)
}

func openAIContentBlocksForPromptCache(content interface{}) []interface{} {
	switch value := content.(type) {
	case nil:
		return nil
	case []interface{}:
		return append([]interface{}(nil), value...)
	default:
		return []interface{}{map[string]interface{}{"type": "text", "text": value}}
	}
}

type openAIPromptCacheState struct {
	tracker  *promptCacheTracker
	profile  *promptCacheProfile
	usage    promptCacheUsage
	computed bool
	updated  bool
}

func newOpenAIPromptCacheState(fallback *promptCacheTracker, profile *promptCacheProfile) *openAIPromptCacheState {
	return &openAIPromptCacheState{
		tracker: promptCacheTrackerForProfile(fallback, profile),
		profile: profile,
	}
}

func (s *openAIPromptCacheState) compute(accountID string) {
	if s == nil || s.computed {
		return
	}
	s.computed = true
	if s.tracker != nil && s.profile != nil {
		s.usage = s.tracker.Compute(accountID, s.profile)
	}
}

func (s *openAIPromptCacheState) complete(accountID string, inputTokens int) promptCacheUsage {
	if s == nil {
		return promptCacheUsage{}
	}
	s.compute(accountID)
	usage := reanchorClaudeCacheUsage(s.profile, s.usage, inputTokens)
	if !s.updated && s.tracker != nil && s.profile != nil {
		s.tracker.Update(accountID, s.profile)
		s.updated = true
	}
	return usage
}

func buildOpenAIUsageMap(inputTokens, outputTokens int, cacheUsage promptCacheUsage) map[string]interface{} {
	cachedTokens := cacheUsage.CacheReadInputTokens
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > inputTokens {
		cachedTokens = inputTokens
	}
	return map[string]interface{}{
		"prompt_tokens":     inputTokens,
		"completion_tokens": outputTokens,
		"total_tokens":      inputTokens + outputTokens,
		"prompt_tokens_details": map[string]int{
			"cached_tokens": cachedTokens,
		},
	}
}

// handleOpenAIStream preserves the direct-call contract used by focused tests.
func (h *Handler) handleOpenAIStream(w http.ResponseWriter, payload *KiroPayload, model, responseModel string, thinking, includeUsage bool, estimatedInputTokens int, apiKeyID string) {
	h.handleOpenAIStreamWithCache(w, payload, model, responseModel, thinking, includeUsage, estimatedInputTokens, nil, apiKeyID)
}

func (h *Handler) handleOpenAIStreamWithCache(w http.ResponseWriter, payload *KiroPayload, model, responseModel string, thinking, includeUsage bool, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	payload = cloneKiroPayload(payload)
	configureSSEHeaders(w)

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	created := time.Now().Unix()
	excluded := make(map[string]bool)
	retryCounts := make(map[string]int)
	retrySameID := ""
	var lastErr error
	reqStart := time.Now()
	preResponseRetryCount := 0
	streamCommitted := false
	preludeSent := false
	earlyPreludeEnabled := earlyStreamPreludeEnabled()
	cacheState := newOpenAIPromptCacheState(h.promptCache, cacheProfile)
	var retryUsage retryBillingUsage
	finishWithStreamError := func(errType string, err error) {
		message := "upstream stream failed"
		if err != nil {
			message = err.Error()
		}
		data, _ := json.Marshal(map[string]interface{}{
			"error": map[string]string{"type": errType, "message": message},
		})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
		flusher.Flush()
		streamCommitted = true
	}

	attemptLimit := h.maxAccountAttempts()
	for attempt := 0; attempt < attemptLimit; attempt++ {
		account, allFull := h.acquireAccountForModel(model, excluded, retrySameID, kiroPayloadContext(payload))
		retrySameID = ""
		if account == nil {
			if h.startExpiredAccountRefreshes(model, excluded) {
				lastErr = errAccountRefreshInProgress
			} else if allFull {
				lastErr = fmt.Errorf(allAccountsConcurrencyFullMessage)
			}
			break
		}
		if err := h.ensureValidTokenForPayload(account, payload); err != nil {
			h.releaseAccount(account)
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var toolCalls []ToolCall
		var toolCallIndex int
		var inputTokens, outputTokens int
		var credits float64
		var rawContentBuilder strings.Builder
		var rawReasoningBuilder strings.Builder
		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var reasoningPhaseSealed bool
		var thinkingStarted bool
		var eventThinkingOpen bool
		var upstreamStopReason string
		var contextWindowExceeded bool
		var suppressedReasoningObserved bool
		upstreamOutputStarted := false
		stopFilter := newStopSequenceFilter(payload.StopSequences)
		completionGate := newAgentCompletionGateForPayload(payload)
		suppressRecoveryOutput := payload.AgentCompletionRetries > 0
		bypassStopFilter := false
		toolCallIndexes := make(map[string]int)
		toolCallNames := make(map[string]string)
		ignoredToolUses := make(map[string]bool)

		writeChunk := func(choices []map[string]interface{}, usage map[string]interface{}) {
			chunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   responseModel,
				"choices": choices,
			}
			if usage != nil {
				chunk["usage"] = usage
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			streamCommitted = true
		}

		ensureAssistantRole := func() {
			if preludeSent {
				return
			}
			writeChunk([]map[string]interface{}{{
				"index": 0,
				"delta": map[string]string{
					"role":    "assistant",
					"content": "",
				},
				"finish_reason": nil,
			}}, nil)
			preludeSent = true
		}
		var onStreamReady func()
		if earlyPreludeEnabled {
			onStreamReady = func() {
				if preludeSent {
					return
				}
				ensureAssistantRole()
				if timing := requestTimingFromContext(kiroPayloadContext(payload)); timing != nil {
					timing.markDownstreamPreludeFlushed(time.Now())
				}
			}
		}

		sendChunk := func(content string, thinkingState int) {
			if content == "" && thinkingState == 2 {
				return
			}
			if thinkingState == 0 && !bypassStopFilter {
				content = completionGate.Push(content)
				if content == "" {
					return
				}
				if suppressRecoveryOutput && !completionGate.Marked() {
					return
				}
				content = stopFilter.Push(content)
			} else if thinkingState > 0 && stopFilter.Matched() != "" {
				return
			}

			var chunk map[string]interface{}

			if thinkingState > 0 {
				if !thinking {
					return
				}
				switch thinkingFormat {
				case "thinking":
					var text string
					switch thinkingState {
					case 1:
						text = "<thinking>" + content
					case 2:
						text = content
					case 3:
						text = content + "</thinking>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   responseModel,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				case "think":
					var text string
					switch thinkingState {
					case 1:
						text = "<think>" + content
					case 2:
						text = content
					case 3:
						text = content + "</think>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   responseModel,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				default:
					if content == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   responseModel,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"reasoning_content": content},
							"finish_reason": nil,
						}},
					}
				}
			} else {
				if content == "" {
					return
				}
				reasoningPhaseSealed = true
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   responseModel,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": content},
						"finish_reason": nil,
					}},
				}
			}
			ensureAssistantRole()
			chunk["created"] = created
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
		}
		flushPendingStopText := func() {
			pending := stopFilter.Flush()
			if pending == "" {
				return
			}
			bypassStopFilter = true
			sendChunk(pending, 0)
			bypassStopFilter = false
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				isThinking = false
			}

			if isThinking {
				if reasoningPhaseSealed {
					return
				}
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendChunk(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendChunk(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, thinkingOpenTag)
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendChunk(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+len(thinkingOpenTag):]
						inThinkingBlock = true
						dropTagThinking = reasoningPhaseSealed || !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush {
						sendChunk(textBuffer, 0)
						textBuffer = ""
						break
					} else {
						ready, pending := splitBeforePartialTag(textBuffer, thinkingOpenTag)
						sendChunk(ready, 0)
						textBuffer = pending
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, thinkingCloseTag)
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendChunk(content, 1)
								sendChunk("", 3)
							} else {
								sendChunk(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+len(thinkingCloseTag):]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendChunk(textBuffer, 1)
									sendChunk("", 3)
								} else {
									sendChunk(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						ready, pending := splitBeforePartialTag(textBuffer, thinkingCloseTag)
						if ready != "" && !dropTagThinking {
							if !thinkingStarted {
								sendChunk(ready, 1)
								thinkingStarted = true
							} else {
								sendChunk(ready, 2)
							}
						}
						textBuffer = pending
						break
					}
				}
			}
		}

		beginToolCall := func(toolUseID, name string) {
			if _, exists := toolCallIndexes[toolUseID]; exists || ignoredToolUses[toolUseID] {
				return
			}
			if forcedToolChoiceMode(payload) && !kiroPayloadAllowsToolName(payload, name) {
				ignoredToolUses[toolUseID] = true
				return
			}
			processText("", false, true)
			if narration := completionGate.CommitPendingForTool(); narration != "" {
				filtered := stopFilter.Push(narration)
				if filtered != "" {
					bypassStopFilter = true
					sendChunk(filtered, 0)
					bypassStopFilter = false
				}
			}
			flushPendingStopText()
			if stopFilter.Matched() != "" {
				return
			}
			reasoningPhaseSealed = true

			index := toolCallIndex
			toolCallIndex++
			toolCallIndexes[toolUseID] = index
			toolCallNames[toolUseID] = name
			ensureAssistantRole()
			writeChunk([]map[string]interface{}{{
				"index": 0,
				"delta": map[string]interface{}{
					"tool_calls": []map[string]interface{}{{
						"index": index,
						"id":    toolUseID,
						"type":  "function",
						"function": map[string]string{
							"name":      name,
							"arguments": "",
						},
					}},
				},
				"finish_reason": nil,
			}}, nil)
		}
		emitToolCallDelta := func(toolUseID, delta string) {
			if delta == "" || ignoredToolUses[toolUseID] {
				return
			}
			index, exists := toolCallIndexes[toolUseID]
			if !exists {
				return
			}
			writeChunk([]map[string]interface{}{{
				"index": 0,
				"delta": map[string]interface{}{
					"tool_calls": []map[string]interface{}{{
						"index": index,
						"id":    toolUseID,
						"type":  "function",
						"function": map[string]string{
							"name":      toolCallNames[toolUseID],
							"arguments": delta,
						},
					}},
				},
				"finish_reason": nil,
			}}, nil)
		}

		callback := &KiroStreamCallback{
			OnStreamReady: onStreamReady,
			OnKeepAlive: func() {
				writeSSEKeepAlive(w, flusher)
				streamCommitted = true
			},
			OnText: func(text string, isThinking bool) {
				if text != "" {
					upstreamOutputStarted = true
				}
				if text == "" || stopFilter.Matched() != "" {
					return
				}
				if isThinking {
					if !thinking {
						suppressedReasoningObserved = true
						return
					}
					rawReasoningBuilder.WriteString(text)
					if suppressRecoveryOutput {
						return
					}
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnReasoning: func(event KiroReasoningContentEvent) {
				if event.Text != "" || event.Signature != "" || event.RedactedContent != "" {
					upstreamOutputStarted = true
				}
				if event.Text == "" || stopFilter.Matched() != "" {
					return
				}
				if !thinking {
					suppressedReasoningObserved = true
					return
				}
				rawReasoningBuilder.WriteString(event.Text)
				if !suppressRecoveryOutput {
					processText(event.Text, true, false)
				}
			},
			OnToolUseStart: func(toolUseID, name string) {
				upstreamOutputStarted = true
				beginToolCall(toolUseID, name)
			},
			OnToolUseInputDelta: func(toolUseID, delta string) {
				if delta != "" {
					upstreamOutputStarted = true
				}
				emitToolCallDelta(toolUseID, delta)
			},
			OnToolUse: func(tu KiroToolUse) {
				upstreamOutputStarted = true
				if ignoredToolUses[tu.ToolUseID] ||
					(forcedToolChoiceMode(payload) && !kiroPayloadAllowsToolName(payload, tu.Name)) {
					ignoredToolUses[tu.ToolUseID] = true
					return
				}
				_, streamed := toolCallIndexes[tu.ToolUseID]
				if !streamed {
					beginToolCall(tu.ToolUseID, tu.Name)
					if _, exists := toolCallIndexes[tu.ToolUseID]; !exists {
						return
					}
					args, _ := json.Marshal(tu.Input)
					emitToolCallDelta(tu.ToolUseID, string(args))
				}
				args, _ := json.Marshal(tu.Input)
				tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
				tc.Function.Name = tu.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				_, exceeded := observeKiroContextUsage(payload, model, pct)
				if exceeded {
					contextWindowExceeded = true
				}
			},
		}

		call := h.kiroCaller
		if call == nil {
			call = CallKiroAPI
		}
		cacheState.compute(account.ID)
		err := call(account, payload, callback)
		err = resolveKiroOutputTruncation(payload, err, upstreamStopReason, rawContentBuilder.String(), completedKiroToolNamesFromOpenAIToolCalls(toolCalls))
		if err == nil {
			if completionErr := agentCompletionResponseError(
				payload,
				suppressedReasoningObserved || rawReasoningBuilder.Len() > 0,
				completionGate.VisibleText(),
				completionGate.Marked(),
				completedOpenAIToolNames(toolCalls),
				upstreamStopReason,
			); completionErr != nil {
				err = completionErr
				prepareReasoningOnlyRetry(payload, err)
				if isUpstreamAgentActionOnlyError(err) {
					payload.agentActionContinuationTimeout = true
				}
			}
		}
		if shouldFinishStreamAfterAgentContinuation(payload, err, len(toolCalls)) {
			logger.Warnf("[KiroAPI] finishing OpenAI stream after bounded agent continuation request_id=%s account_id=%s phase=%s completions=%d actions=%d full_context=%d: %v",
				chatID, account.ID, kiroAgentRecoveryPhase(payload, err), payload.AgentCompletionRetries,
				payload.AgentActionContinuations, payload.AgentFullContextRecoveries, err)
			err = nil
			upstreamStopReason = "end_turn"
		}
		h.releaseAccount(account)
		if err != nil {
			// Bill whatever this attempt already produced before deciding how to
			// recover. Every branch below either retries or returns, so a later
			// settlement would never see these tokens.
			discarded := estimateOpenAIDiscardedOutput(
				rawContentBuilder.String(), rawReasoningBuilder.String(), toolCalls,
			)
			discarded = discardedAttemptOutputTokens(discarded, outputTokens)
			if discarded > 0 || inputTokens > 0 || credits > 0 {
				retryUsage.add(h.settleDiscardedAttempt(
					"openai", model, account.ID, apiKeyID,
					clampClaudeInputTokensToModelWindow(model, discardedAttemptInputTokens(estimatedInputTokens, inputTokens)), discarded, credits,
					time.Since(reqStart).Milliseconds(),
				))
			}
			if isUpstreamIncompleteResponseError(err) && eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			lastErr = err
			if takeKiroOutputContinuationRetry(payload, err) {
				retryUsage.markLastVisibleContinuation()
				retryUsage.markLastContinuationAttemptOutput()
				retrySameID = account.ID
				attemptLimit++
				continue
			}
			if isKiroInputTooLongError(err) {
				if h.retryAgentRecoveryAfterInputTooLong("OpenAI stream", payload, account, &retrySameID, err) {
					attemptLimit++
					continue
				}
				h.recordFailureWithDetails("openai", model, account.ID, err)
				if !streamCommitted {
					h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				} else {
					finishWithStreamError("invalid_request_error", err)
				}
				return
			}
			if (!upstreamOutputStarted || isUpstreamIncompleteResponseError(err)) && isRetryableKiroStreamError(err) &&
				(!isUpstreamIncompleteResponseError(err) || agentCompletionRetryAllowed(payload, err)) {
				retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
				if retryErr != nil {
					return
				}
				if retried {
					attemptLimit++
					retryNumber, retryLimit := kiroPreResponseRetryProgress(payload, preResponseRetryCount, err)
					logger.Warnf("[KiroAPI] retrying OpenAI stream after transient pre-response error request_id=%s account_id=%s phase=%s retry=%d/%d completions=%d actions=%d full_context=%d: %v",
						chatID, account.ID, kiroAgentRecoveryPhase(payload, err), retryNumber, retryLimit,
						payload.AgentCompletionRetries, payload.AgentActionContinuations, payload.AgentFullContextRecoveries, err)
					continue
				}
			}
			if code, protocolErr := kiroStreamProtocolErrorCode(err); protocolErr {
				h.recordFailureWithDetails("openai", model, account.ID, err)
				if !streamCommitted {
					h.sendOpenAIError(w, http.StatusBadGateway, code, err.Error())
				} else {
					finishWithStreamError(code, err)
				}
				return
			}
			if isRateLimitError(err) {
				lastErr = err
				decision := h.applyRateLimitPolicy(account, retryCounts, excluded, err)
				if decision.Return429 {
					h.recordFailureWithDetails("openai", model, account.ID, err)
					if !streamCommitted {
						h.sendOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", err.Error())
					} else {
						finishWithStreamError("rate_limit_error", err)
					}
					return
				}
				retrySameID = decision.RetrySameID
				if !upstreamOutputStarted {
					if !h.waitBeforeRateLimitRetry(payload, decision) {
						return
					}
					continue
				}
				h.recordFailureWithDetails("openai", model, account.ID, err)
				finishWithStreamError("rate_limit_error", err)
				return
			}
			if isKiroClientRequestError(err) {
				h.recordFailureWithDetails("openai", model, account.ID, err)
				if !streamCommitted {
					h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				} else {
					finishWithStreamError("invalid_request_error", err)
				}
				return
			}
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			if !upstreamOutputStarted {
				continue
			}
			h.recordFailureWithDetails("openai", model, account.ID, err)
			finishWithStreamError("server_error", err)
			return
		}

		processText("", false, true)
		if terminalText := completionGate.CommitPendingForTerminal(); terminalText != "" {
			filtered := stopFilter.Push(terminalText)
			if filtered != "" {
				bypassStopFilter = true
				sendChunk(filtered, 0)
				bypassStopFilter = false
			}
		}
		flushPendingStopText()
		if eventThinkingOpen {
			sendChunk("", 3)
		}

		if _, exceeded := currentKiroContextUsage(payload, model); exceeded {
			contextWindowExceeded = true
		}
		inputTokens = clampClaudeInputTokensToModelWindow(model, estimatedInputTokens)
		if inputTokens <= 0 {
			inputTokens = 1
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		outputContent, _ = filterAgentCompletionContent(payload, outputContent, len(toolCalls))
		outputContent, matchedStopSequence := truncateAtStopSequence(outputContent, payload.StopSequences)
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			outputTokens += estimateApproxTokens(tc.Function.Name)
			outputTokens += estimateApproxTokens(tc.Function.Arguments)
		}

		cacheUsage := cacheState.complete(account.ID, inputTokens)
		h.recordSuccessForApiKeyWithCache(apiKeyID, inputTokens, outputTokens, credits, cacheUsage)
		h.pool.RecordSuccess(account.ID)
		// h.pool.RecordLatency(account.ID, float64(time.Since(reqStart).Milliseconds()))
		// ↑ dispatch-only latency EWMA (commit fbc99b1), absent on this cache-only
		//   branch off main. No-op here; stripped so handler compiles against main's
		//   pool. Re-added verbatim when dispatch lands.
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("openai", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		finishReason := "stop"
		if matchedStopSequence != "" {
			finishReason = "stop"
		} else if isKiroLengthStopReason(upstreamStopReason) || contextWindowExceeded {
			finishReason = "length"
		} else if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}

		ensureAssistantRole()
		writeChunk([]map[string]interface{}{{
			"index":         0,
			"delta":         map[string]string{"content": ""},
			"finish_reason": finishReason,
		}}, nil)
		if includeUsage {
			publicInputTokens := contextUsageBlendedInputTokens(payload, model, inputTokens)
			publicCacheUsage := contextUsageBlendedCacheUsage(payload, model, inputTokens, cacheUsage)
			publicInputTokens, publicCacheUsage = retryUsage.apply(publicInputTokens, publicCacheUsage)
			publicOutputTokens := retryUsage.applyOutput(outputTokens)
			terminalUsage := buildOpenAIUsageMap(publicInputTokens, publicOutputTokens, publicCacheUsage)
			addKiroContextUsage(terminalUsage, payload, model)
			writeChunk([]map[string]interface{}{}, terminalUsage)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("No available accounts")
		h.recordFailureWithDetails("openai", model, "", lastErr)
		if streamCommitted {
			finishWithStreamError("server_error", lastErr)
		} else {
			h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", lastErr.Error())
		}
		return
	}

	h.recordFailureWithDetails("openai", model, "", lastErr)
	if isAccountRefreshInProgressError(lastErr) {
		if streamCommitted {
			finishWithStreamError("account_refreshing", lastErr)
			return
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(expiredAccountRefreshWait.Seconds())))
		h.sendOpenAIError(w, accountRefreshInProgressStatus, "account_refreshing", accountRefreshInProgressMessage)
		return
	}
	if isAllAccountsConcurrencyFullError(lastErr) {
		if streamCommitted {
			finishWithStreamError("server_error", lastErr)
		} else {
			h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", allAccountsConcurrencyFullMessage)
		}
		return
	}
	if isRateLimitError(lastErr) {
		if streamCommitted {
			finishWithStreamError("rate_limit_error", lastErr)
		} else {
			h.sendOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", lastErr.Error())
		}
		return
	}
	if streamCommitted {
		finishWithStreamError("server_error", lastErr)
	} else {
		h.sendOpenAIError(w, http.StatusInternalServerError, "server_error", lastErr.Error())
	}
}

// handleOpenAINonStream OpenAI 非流式响应
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, payload *KiroPayload, model, responseModel string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	h.handleOpenAINonStreamWithCache(w, payload, model, responseModel, thinking, estimatedInputTokens, nil, apiKeyID)
}

func (h *Handler) handleOpenAINonStreamWithCache(w http.ResponseWriter, payload *KiroPayload, model, responseModel string, thinking bool, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	payload = cloneKiroPayload(payload)
	excluded := make(map[string]bool)
	retryCounts := make(map[string]int)
	retrySameID := ""
	var lastErr error
	reqStart := time.Now()
	preResponseRetryCount := 0
	cacheState := newOpenAIPromptCacheState(h.promptCache, cacheProfile)
	var retryUsage retryBillingUsage

	attemptLimit := h.maxAccountAttempts()
	for attempt := 0; attempt < attemptLimit; attempt++ {
		account, allFull := h.acquireAccountForModel(model, excluded, retrySameID, kiroPayloadContext(payload))
		retrySameID = ""
		if account == nil {
			if h.startExpiredAccountRefreshes(model, excluded) {
				lastErr = errAccountRefreshInProgress
			} else if allFull {
				lastErr = fmt.Errorf(allAccountsConcurrencyFullMessage)
			}
			break
		}
		if err := h.ensureValidTokenForPayload(account, payload); err != nil {
			h.releaseAccount(account)
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var content string
		var reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var upstreamStopReason string
		var contextWindowExceeded bool
		var suppressedReasoningObserved bool
		var matchedStopSequence string
		updateStopSequenceState := func() {
			if matchedStopSequence != "" {
				return
			}
			visibleContent, _ := extractThinkingFromContent(content)
			_, matchedStopSequence = truncateAtStopSequence(visibleContent, payload.StopSequences)
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if matchedStopSequence != "" {
					return
				}
				if isThinking {
					if !thinking {
						suppressedReasoningObserved = true
						return
					}
					reasoningContent += text
					return
				}
				content += text
				updateStopSequenceState()
			},
			OnToolUse: func(tu KiroToolUse) {
				if matchedStopSequence == "" {
					toolUses = append(toolUses, tu)
				}
			},
			OnComplete:   func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			OnCredits:    func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				_, exceeded := observeKiroContextUsage(payload, model, pct)
				if exceeded {
					contextWindowExceeded = true
				}
			},
		}

		call := h.kiroCaller
		if call == nil {
			call = CallKiroAPI
		}
		cacheState.compute(account.ID)
		err := call(account, payload, callback)
		err = resolveKiroOutputTruncation(payload, err, upstreamStopReason, content, completedKiroToolNames(toolUses))
		if err == nil {
			visibleContent, _ := extractThinkingFromContent(content)
			visibleContent, markerSeen := filterAgentCompletionContent(payload, visibleContent, len(toolUses))
			if completionErr := agentCompletionResponseError(
				payload,
				suppressedReasoningObserved || reasoningContent != "",
				visibleContent,
				markerSeen,
				completedKiroToolNames(toolUses),
				upstreamStopReason,
			); completionErr != nil {
				err = completionErr
				prepareReasoningOnlyRetry(payload, err)
			}
		}
		h.releaseAccount(account)
		if err != nil {
			// Bill whatever this attempt already produced before deciding how to
			// recover. Every branch below either retries or returns, so a later
			// settlement would never see these tokens.
			discarded := estimateOpenAIOutputTokens(content, reasoningContent, toolUses)
			discarded = discardedAttemptOutputTokens(discarded, outputTokens)
			if discarded > 0 || inputTokens > 0 || credits > 0 {
				retryUsage.add(h.settleDiscardedAttempt(
					"openai", model, account.ID, apiKeyID,
					clampClaudeInputTokensToModelWindow(model, discardedAttemptInputTokens(estimatedInputTokens, inputTokens)), discarded, credits,
					time.Since(reqStart).Milliseconds(),
				))
			}
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			lastErr = err
			if takeKiroOutputContinuationRetry(payload, err) {
				retryUsage.markLastContinuationAttemptOutput()
				retrySameID = account.ID
				attemptLimit++
				continue
			}
			if isKiroInputTooLongError(err) {
				if h.retryAgentRecoveryAfterInputTooLong("OpenAI non-stream", payload, account, &retrySameID, err) {
					attemptLimit++
					continue
				}
				h.recordFailureWithDetails("openai", model, account.ID, err)
				h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			if !isKiroToolJSONError(err) && isRetryableKiroStreamError(err) &&
				(!isUpstreamIncompleteResponseError(err) || agentCompletionRetryAllowed(payload, err)) {
				retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
				if retryErr != nil {
					return
				}
				if retried {
					attemptLimit++
					retryNumber, retryLimit := kiroPreResponseRetryProgress(payload, preResponseRetryCount, err)
					logger.Warnf("[KiroAPI] retrying OpenAI non-stream after transient pre-response error account_id=%s retry=%d/%d: %v", account.ID, retryNumber, retryLimit, err)
					continue
				}
			}
			if code, protocolErr := kiroStreamProtocolErrorCode(err); protocolErr {
				h.recordFailureWithDetails("openai", model, account.ID, err)
				h.sendOpenAIError(w, http.StatusBadGateway, code, err.Error())
				return
			}
			if isRateLimitError(err) {
				decision := h.applyRateLimitPolicy(account, retryCounts, excluded, err)
				if decision.Return429 {
					h.recordFailureWithDetails("openai", model, account.ID, err)
					h.sendOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", err.Error())
					return
				}
				retrySameID = decision.RetrySameID
				if !h.waitBeforeRateLimitRetry(payload, decision) {
					return
				}
				continue
			}
			if isKiroClientRequestError(err) {
				h.recordFailureWithDetails("openai", model, account.ID, err)
				h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		finalContent, _ = filterAgentCompletionContent(payload, finalContent, len(toolUses))
		finalContent = prependKiroOutputContinuation(payload, finalContent)
		finalContent, finalMatchedStopSequence := truncateAtStopSequence(finalContent, payload.StopSequences)
		if matchedStopSequence == "" {
			matchedStopSequence = finalMatchedStopSequence
		}
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}
		if len(toolUses) > 0 && payload.AgentCompletionRetries > 0 {
			reasoningContent = ""
		}

		if _, exceeded := currentKiroContextUsage(payload, model); exceeded {
			contextWindowExceeded = true
		}
		inputTokens = clampClaudeInputTokensToModelWindow(model, estimatedInputTokens)
		if inputTokens <= 0 {
			inputTokens = 1
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		cacheUsage := cacheState.complete(account.ID, inputTokens)
		attemptOutputTokens := retryUsage.finalAttemptOutputTokens(outputTokens)
		h.recordSuccessForApiKeyWithCache(apiKeyID, inputTokens, attemptOutputTokens, credits, cacheUsage)
		h.pool.RecordSuccess(account.ID)
		// h.pool.RecordLatency(account.ID, float64(time.Since(reqStart).Milliseconds()))
		// ↑ dispatch-only latency EWMA (commit fbc99b1), absent on this cache-only
		//   branch off main. No-op here; stripped so handler compiles against main's
		//   pool. Re-added verbatim when dispatch lands.
		h.pool.UpdateStats(account.ID, inputTokens+attemptOutputTokens, credits)
		h.recordSuccessLog("openai", model, account.ID, inputTokens+attemptOutputTokens, credits, time.Since(reqStart).Milliseconds())

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		publicInputTokens := contextUsageBlendedInputTokens(payload, model, inputTokens)
		publicCacheUsage := contextUsageBlendedCacheUsage(payload, model, inputTokens, cacheUsage)
		publicInputTokens, publicCacheUsage = retryUsage.apply(publicInputTokens, publicCacheUsage)
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, publicInputTokens, outputTokens, responseModel, thinkingFormat)
		usage := buildOpenAIUsageMap(publicInputTokens, outputTokens, publicCacheUsage)
		addKiroContextUsage(usage, payload, model)
		resp["usage"] = usage
		if matchedStopSequence != "" {
			if choices, ok := resp["choices"].([]map[string]interface{}); ok && len(choices) > 0 {
				choices[0]["finish_reason"] = "stop"
			}
		} else if isKiroLengthStopReason(upstreamStopReason) || contextWindowExceeded {
			if choices, ok := resp["choices"].([]map[string]interface{}); ok && len(choices) > 0 {
				choices[0]["finish_reason"] = "length"
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.recordFailureWithDetails("openai", model, "", fmt.Errorf("No available accounts"))
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", "No available accounts")
		return
	}

	h.recordFailureWithDetails("openai", model, "", lastErr)
	if isAccountRefreshInProgressError(lastErr) {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(expiredAccountRefreshWait.Seconds())))
		h.sendOpenAIError(w, accountRefreshInProgressStatus, "account_refreshing", accountRefreshInProgressMessage)
		return
	}
	if isAllAccountsConcurrencyFullError(lastErr) {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", allAccountsConcurrencyFullMessage)
		return
	}
	if isRateLimitError(lastErr) {
		h.sendOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", lastErr.Error())
		return
	}
	h.sendOpenAIError(w, http.StatusInternalServerError, "server_error", lastErr.Error())
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

// refreshAccountToken is the single locked entry point for refreshing an
// account's access token. Every caller — the background refresh sweep, the
// per-request ensureValidToken, and the admin refresh endpoints — must funnel
// through here so concurrent refreshes of the same account deduplicate rather
// than racing the IdP. IdPs that rotate refresh tokens (one-time-use — e.g.
// external_idp / Azure AD; see the comment in auth/oidc.go refreshExternalIdpToken)
// reject a refresh that reuses an already-consumed token with invalid_grant, and
// that flows into handleAccountFailure → disableAccount("BANNED").
//
// force bypasses the not-near-expiry fast path for explicit admin "refresh now"
// actions and the 401/403 retry path.
func (h *Handler) refreshAccountToken(account *config.Account, force bool) error {
	return h.refreshAccountTokenContext(context.Background(), account, force)
}

func (h *Handler) refreshAccountTokenContext(ctx context.Context, account *config.Account, force bool) error {
	if account == nil {
		return fmt.Errorf("account is nil")
	}
	// API-key credentials are long-lived bearer values. They have no OAuth
	// refresh material, including when an admin explicitly requests a refresh.
	if account.IsApiKeyCredential() {
		return nil
	}
	if !force && (account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds) {
		return nil
	}

	h.tokenRefreshOnce.Do(func() {
		h.tokenRefreshGate = make(chan struct{}, 1)
		h.tokenRefreshGate <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.tokenRefreshGate:
	}
	defer func() { h.tokenRefreshGate <- struct{}{} }()

	// Another goroutine may have refreshed this account while we waited on the
	// lock. Adopt the freshest persisted token before refreshing so we never
	// reuse an already-rotated (one-time-use) refresh token.
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
		if !force && (account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds) {
			return nil
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshTokenContext(ctx, account)
	if err != nil {
		return err
	}

	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}
	config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt)
	return nil
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	return h.refreshAccountToken(account, false)
}

func (h *Handler) ensureValidTokenForPayload(account *config.Account, payload *KiroPayload) error {
	ctx := context.Background()
	if payload != nil && payload.requestContext != nil {
		ctx = payload.requestContext
	}
	return h.refreshAccountTokenContext(ctx, account, false)
}

func (h *Handler) ensureValidTokenContext(ctx context.Context, account *config.Account) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return h.refreshAccountTokenContext(ctx, account, false)
}

// ==================== 管理 API ====================

func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// 验证密码
	password := r.Header.Get("X-Admin-Password")
	if password == "" {
		cookie, _ := r.Cookie("admin_password")
		if cookie != nil {
			password = cookie.Value
		}
	}

	expected := config.GetPassword()
	if expected != "" && subtle.ConstantTimeCompare([]byte(password), []byte(expected)) != 1 {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case path == "/accounts" && r.Method == "GET":
		h.apiGetAccounts(w, r)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	// models/refresh 必须在通用 /refresh 前匹配，否则会被误拦截
	case path == "/accounts/models/refresh" && r.Method == "POST":
		h.apiRefreshAllAccountsModels(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/refresh")
		h.apiRefreshAccountModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models")
		h.apiGetAccountModels(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiSetAccountOverage(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiGetAccountOverage(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/full") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/full")
		h.apiGetAccountFull(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && r.Method == "DELETE":
		h.apiDeleteAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "PUT":
		h.apiUpdateAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case path == "/auth/iam-sso/start" && r.Method == "POST":
		h.apiStartIamSso(w, r)
	case path == "/auth/iam-sso/complete" && r.Method == "POST":
		h.apiCompleteIamSso(w, r)
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/kiro-sso/start" && r.Method == "POST":
		h.apiStartKiroSso(w, r)
	case path == "/auth/kiro-sso/poll" && r.Method == "POST":
		h.apiPollKiroSso(w, r)
	case path == "/auth/kiro-sso/cancel" && r.Method == "POST":
		h.apiCancelKiroSso(w, r)
	case path == "/auth/kiro-sso/callback" && r.Method == "POST":
		h.apiCompleteKiroSso(w, r)
	case path == "/auth/sso-token" && r.Method == "POST":
		h.apiImportSsoToken(w, r)
	case path == "/auth/credentials" && r.Method == "POST":
		h.apiImportCredentials(w, r)
	case path == "/auth/apikeys-batch" && r.Method == "POST":
		h.apiImportApiKeys(w, r)
	case path == "/status" && r.Method == "GET":
		h.apiGetStatus(w, r)
	case path == "/settings" && r.Method == "GET":
		h.apiGetSettings(w, r)
	case path == "/settings" && r.Method == "POST":
		h.apiUpdateSettings(w, r)
	case path == "/stats" && r.Method == "GET":
		h.apiGetStats(w, r)
	case path == "/stats/reset" && r.Method == "POST":
		h.apiResetStats(w, r)
	case path == "/traffic" && r.Method == "GET":
		h.apiGetTraffic(w, r)
	case path == "/logs" && r.Method == "GET":
		h.apiGetLogs(w, r)
	case path == "/logs" && r.Method == "DELETE":
		h.apiClearLogs(w, r)
	case path == "/generate-machine-id" && r.Method == "GET":
		h.apiGenerateMachineId(w, r)
	case path == "/thinking" && r.Method == "GET":
		h.apiGetThinkingConfig(w, r)
	case path == "/thinking" && r.Method == "POST":
		h.apiUpdateThinkingConfig(w, r)
	case path == "/endpoint" && r.Method == "GET":
		h.apiGetEndpointConfig(w, r)
	case path == "/endpoint" && r.Method == "POST":
		h.apiUpdateEndpointConfig(w, r)
	case path == "/proxy" && r.Method == "GET":
		h.apiGetProxy(w, r)
	case path == "/proxy" && r.Method == "POST":
		h.apiUpdateProxy(w, r)
	case path == "/prompt-filter" && r.Method == "GET":
		h.apiGetPromptFilter(w, r)
	case path == "/prompt-filter" && r.Method == "POST":
		h.apiUpdatePromptFilter(w, r)
	case path == "/version" && r.Method == "GET":
		h.apiGetVersion(w, r)
	case path == "/export" && r.Method == "POST":
		h.apiExportAccounts(w, r)
	case path == "/prompt-cache-policies" && r.Method == "GET":
		h.apiListPromptCachePolicies(w, r)
	case path == "/prompt-cache-policies" && r.Method == "POST":
		h.apiCreatePromptCachePolicy(w, r)
	case strings.HasPrefix(path, "/prompt-cache-policies/") && r.Method == "GET":
		h.apiGetPromptCachePolicy(w, r, strings.TrimPrefix(path, "/prompt-cache-policies/"))
	case strings.HasPrefix(path, "/prompt-cache-policies/") && r.Method == "PUT":
		h.apiUpdatePromptCachePolicy(w, r, strings.TrimPrefix(path, "/prompt-cache-policies/"))
	case strings.HasPrefix(path, "/prompt-cache-policies/") && r.Method == "DELETE":
		h.apiDeletePromptCachePolicy(w, r, strings.TrimPrefix(path, "/prompt-cache-policies/"))
	case path == "/api-keys" && r.Method == "GET":
		h.apiListApiKeys(w, r)
	case path == "/api-keys" && r.Method == "POST":
		h.apiCreateApiKey(w, r)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-usage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-usage")
		h.apiResetApiKeyUsage(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "GET":
		h.apiGetApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "PUT":
		h.apiUpdateApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "DELETE":
		h.apiDeleteApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()
	runtimeStates := h.pool.RuntimeStates()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// 隐藏敏感信息
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// 获取运行时统计
		stats := statsMap[a.ID]

		runtime, inPool := runtimeStates[a.ID]
		runtimeAvailable := inPool && runtime.Available
		runtimeReason := runtime.Reason
		if !inPool {
			runtimeReason = "not_in_runtime_pool"
		}
		result[i] = map[string]interface{}{
			"id":                    a.ID,
			"email":                 a.Email,
			"userId":                a.UserId,
			"nickname":              a.Nickname,
			"authMethod":            a.AuthMethod,
			"provider":              a.Provider,
			"region":                a.Region,
			"enabled":               a.Enabled,
			"banStatus":             a.BanStatus,
			"banReason":             a.BanReason,
			"banTime":               a.BanTime,
			"expiresAt":             a.ExpiresAt,
			"hasToken":              a.AccessToken != "",
			"machineId":             a.MachineId,
			"weight":                a.Weight,
			"accountLevel":          config.NormalizeAccountLevel(a.AccountLevel),
			"effectiveAccountLevel": config.EffectiveAccountLevel(a),
			"maxConcurrency":        a.MaxConcurrency,
			"inFlight":              h.pool.InFlight(a.ID),
			"runtimeAvailable":      runtimeAvailable,
			"runtimeReason":         runtimeReason,
			"overageStatus":         a.OverageStatus,
			"overageCapability":     a.OverageCapability,
			"overageCap":            a.OverageCap,
			"overageRate":           a.OverageRate,
			"currentOverages":       a.CurrentOverages,
			"overageCheckedAt":      a.OverageCheckedAt,
			"proxyURL":              a.ProxyURL,
			"subscriptionType":      a.SubscriptionType,
			"subscriptionTitle":     a.SubscriptionTitle,
			"daysRemaining":         a.DaysRemaining,
			"usageCurrent":          a.UsageCurrent,
			"usageLimit":            a.UsageLimit,
			"usagePercent":          a.UsagePercent,
			"nextResetDate":         a.NextResetDate,
			"lastRefresh":           a.LastRefresh,
			"trialUsageCurrent":     a.TrialUsageCurrent,
			"trialUsageLimit":       a.TrialUsageLimit,
			"trialUsagePercent":     a.TrialUsagePercent,
			"trialStatus":           a.TrialStatus,
			"trialExpiresAt":        a.TrialExpiresAt,
			"requestCount":          stats.RequestCount,
			"errorCount":            stats.ErrorCount,
			"totalTokens":           stats.TotalTokens,
			"totalCredits":          stats.TotalCredits,
			"lastUsed":              stats.LastUsed,
		}
	}
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}
	if account.Region == "" {
		account.Region = "us-east-1"
	}
	account.AccountLevel = config.NormalizeAccountLevel(account.AccountLevel)
	if account.MaxConcurrency < 0 || account.MaxConcurrency > 10000 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "maxConcurrency must be between 0 and 10000"})
		return
	}

	// api_key: use the Kiro API key directly as the bearer (tokentype: API_KEY).
	// Mirror it into AccessToken for pool/dispatch/metrics compatibility, zero
	// ExpiresAt + RefreshToken so no refresh path ever fires.
	if account.IsApiKeyCredential() {
		if account.KiroApiKey == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required for api_key accounts"})
			return
		}
		account.AuthMethod = "api_key"
		account.AccessToken = account.KiroApiKey
		account.RefreshToken = ""
		account.ExpiresAt = 0
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token，立即拉取并缓存模型列表
	if account.Enabled && (account.AccessToken != "" || account.IsApiKeyCredential()) {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", accountEmailForLog(&acc), err)
			}
		}(account)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 获取现有账号
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 只更新传入的字段
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		existing.Weight = int(v)
	}
	if v, ok := updates["accountLevel"].(string); ok {
		existing.AccountLevel = config.NormalizeAccountLevel(v)
	}
	if v, ok := updates["maxConcurrency"].(float64); ok {
		if v < 0 || v > 10000 || v != float64(int(v)) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "maxConcurrency must be an integer between 0 and 10000"})
			return
		}
		existing.MaxConcurrency = int(v)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		existing.ProxyURL = v
	}

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表
	if !oldEnabled && existing.Enabled && existing.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for re-enabled account %s: %v", accountEmailForLog(&acc), err)
			}
		}(*existing)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetAccountOverage 拉取并返回单个账号的上游 Overages 状态。
// 同步把结果写回 config.json 缓存，确保 UI 与持久化一致。
func (h *Handler) apiGetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := FetchOverageStatus(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist GET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiSetAccountOverage 翻转单个账号的上游 Overages 开关，并刷新缓存。
// Body: {"enabled": true|false}
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := SetOverageStatus(account, body.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist SET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiBatchAccounts 批量操作账号（启用/禁用/刷新）
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				// 记录本次从禁用→启用、且有 token 的账号
				if enabled && !a.Enabled && a.AccessToken != "" {
					toRefreshModels = append(toRefreshModels, a)
				}
				a.Enabled = enabled
				if enabled && a.BanStatus != "" && a.BanStatus != "ACTIVE" {
					a.BanStatus = "ACTIVE"
					a.BanReason = ""
					a.BanTime = 0
				}
				config.UpdateAccount(a.ID, a)
			}
		}
		h.pool.Reload()
		// 为本次新启用的账号异步拉取模型缓存
		for _, acc := range toRefreshModels {
			go func(a config.Account) {
				a.Enabled = true
				if err := h.fetchAndCacheAccountModels(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", a.Email, err)
				}
			}(acc)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "refresh":
		successCount := 0
		failCount := 0
		for _, id := range req.IDs {
			accounts := config.GetAccounts()
			var account *config.Account
			for i := range accounts {
				if accounts[i].ID == id {
					account = &accounts[i]
					break
				}
			}
			if account == nil {
				failCount++
				continue
			}
			// 刷新 token（去重 + 持久化由 refreshAccountToken 统一处理）
			if account.RefreshToken != "" {
				if err := h.refreshAccountToken(account, true); err != nil {
					logger.Warnf("[ApiBatch] Token refresh failed for %s: %v", account.Email, err)
				}
			}
			// 刷新账户信息
			info, err := RefreshAccountInfo(account)
			if err != nil {
				failCount++
				continue
			}
			config.UpdateAccountInfo(id, *info)
			successCount++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"refreshed": successCount,
			"failed":    failCount,
		})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// apiStartKiroSso starts the Kiro hosted-portal sign-in (Enterprise SSO — Microsoft 365 /
// Entra ID, plus Google/GitHub). It binds the loopback callback listener and returns the
// sign-in URL the operator opens in a browser ON THE SAME HOST as the proxy (the OAuth
// redirect targets 127.0.0.1:3128). The browser is driven through the enterprise external-IdP
// leg automatically; the front end polls /auth/kiro-sso/poll until completion.
func (h *Handler) apiStartKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	// Region is optional (defaults to us-east-1 in StartKiroSsoLogin), so a decode
	// error (including an empty body) is intentionally tolerated — mirrors
	// apiStartBuilderIdLogin.
	json.NewDecoder(r.Body).Decode(&req)

	session, signInURL, err := auth.StartKiroSsoLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": session.ID,
		"signInUrl": signInURL,
		"interval":  2,
	})
}

// apiCancelKiroSso tears down an in-flight hosted-portal sign-in (operator closed or
// cancelled the modal), freeing the loopback callback port immediately instead of
// waiting for the deadline.
func (h *Handler) apiCancelKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.SessionID != "" {
		auth.CancelKiroSsoLogin(req.SessionID)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// apiCompleteKiroSso lets an operator paste the OAuth callback URL for remote
// deployments where localhost isn't the proxy host. The callback URL is fed
// through the session's state machine; if a redirect is returned (enterprise
// SSO leg-1) the front end should open it in a browser.
func (h *Handler) apiCompleteKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"session_id"`
		CallbackURL string `json:"callback_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.SessionID == "" || req.CallbackURL == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "session_id and callback_url are required"})
		return
	}

	redirectURL, err := auth.FeedCallbackURL(req.SessionID, req.CallbackURL)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if redirectURL != "" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":        "redirect",
			"authorize_url": redirectURL,
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "accepted",
	})
}

// apiPollKiroSso reports the hosted-portal sign-in status. While the user is signing in it
// returns completed=false; once the listener captures the authorization code it exchanges it,
// persists the account (AuthMethod "external_idp" for an Azure tenant, "social" otherwise), and
// returns completed=true. The profileArn is resolved lazily on first use (the EXTERNAL_IDP
// token type header is now sent on CodeWhisperer calls), so it is not required here.
func (h *Handler) apiPollKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	result, status, err := auth.PollKiroSsoAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    "pending",
		})
		return
	}

	// 授权完成，创建账号
	account := config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         result.Email,
		AccessToken:   result.AccessToken,
		RefreshToken:  result.RefreshToken,
		ClientID:      result.ClientID,
		AuthMethod:    result.AuthMethod,
		Provider:      result.Provider,
		Region:        result.Region,
		ProfileArn:    result.ProfileArn,
		TokenEndpoint: result.TokenEndpoint,
		IssuerURL:     result.IssuerURL,
		Scopes:        result.Scopes,
		ExpiresAt:     time.Now().Unix() + int64(result.ExpiresIn),
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":         account.ID,
			"email":      account.Email,
			"authMethod": account.AuthMethod,
		},
	})
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
		email, _, _ := auth.GetUserInfo(accessToken)

		// 创建账号
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

type credentialImportRequest struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	AuthMethod   string `json:"authMethod"`
	Provider     string `json:"provider"`
	Region       string `json:"region"`
	// external_idp (enterprise SSO / Azure AD) refresh material.
	TokenEndpoint string `json:"tokenEndpoint"`
	IssuerURL     string `json:"issuerUrl"`
	Scopes        string `json:"scopes"`
	// api_key credential: Kiro API key used directly as bearer (no OAuth refresh).
	KiroApiKey string `json:"kiroApiKey"`
	AuthRegion string `json:"authRegion"`
	ApiRegion  string `json:"apiRegion"`
	// Optional identity preservation when pasting a full account record.
	ID         string `json:"id"`
	Email      string `json:"email"`
	Nickname   string `json:"nickname"`
	ProfileArn string `json:"profileArn"`
	// userId (account-level in Kiro Account Manager exports) embeds the Azure
	// tenant, from which tokenEndpoint/issuerUrl/scopes are derived when missing.
	UserID string `json:"userId"`
}

func decodeCredentialImportRequest(r io.Reader) (credentialImportRequest, error) {
	var raw json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return credentialImportRequest{}, err
	}
	var req credentialImportRequest
	if len(raw) > 0 && raw[0] == '[' {
		var entries []struct {
			credentialImportRequest
			Type         string `json:"type"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			AuthMethod   string `json:"auth_method"`
			KiroApiKey   string `json:"kiro_api_key"`
			AuthRegion   string `json:"auth_region"`
			ApiRegion    string `json:"api_region"`
			ProfileArn   string `json:"profile_arn"`
			StartURL     string `json:"start_url"`
			UserID       string `json:"user_id"`
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return credentialImportRequest{}, err
		}
		for _, entry := range entries {
			if entry.Type != "" && !strings.EqualFold(entry.Type, "kiro") {
				continue
			}
			req = entry.credentialImportRequest
			if req.AccessToken == "" {
				req.AccessToken = entry.AccessToken
			}
			if req.RefreshToken == "" {
				req.RefreshToken = entry.RefreshToken
			}
			if req.ClientID == "" {
				req.ClientID = entry.ClientID
			}
			if req.ClientSecret == "" {
				req.ClientSecret = entry.ClientSecret
			}
			if req.AuthMethod == "" {
				req.AuthMethod = entry.AuthMethod
			}
			if req.KiroApiKey == "" {
				req.KiroApiKey = entry.KiroApiKey
			}
			if req.AuthRegion == "" {
				req.AuthRegion = entry.AuthRegion
			}
			if req.ApiRegion == "" {
				req.ApiRegion = entry.ApiRegion
			}
			if req.ProfileArn == "" {
				req.ProfileArn = entry.ProfileArn
			}
			if req.UserID == "" {
				req.UserID = entry.UserID
			}
			return req, nil
		}
		return credentialImportRequest{}, fmt.Errorf("no kiro credential")
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return credentialImportRequest{}, err
	}
	return req, nil
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	// Cap the body: accessToken becomes attacker-influenced input that is base64- and
	// JSON-decoded twice (issuerFromAccessTokenJWT / ExpFromAccessTokenJWT). Without a
	// limit an oversized token is a memory-amplification DoS. Mirrors the io.LimitReader
	// guard on outbound IdP responses in auth/kiro_sso.go's oidcDiscover.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	req, err := decodeCredentialImportRequest(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// api_key: import a Kiro API key used directly as bearer (no OAuth refresh,
	// no profile ARN). Mirror the key into AccessToken for pool/dispatch/metrics
	// compatibility. Best-effort RefreshAccountInfo populates email/usage/credit.
	if req.KiroApiKey != "" || strings.EqualFold(req.AuthMethod, "api_key") || strings.EqualFold(req.AuthMethod, "apikey") {
		if req.KiroApiKey == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required for api_key accounts"})
			return
		}
		if req.Region == "" {
			req.Region = "us-east-1"
		}
		id := req.ID
		if id == "" || config.AccountIDExists(id) {
			id = auth.GenerateAccountID()
		}
		account := config.Account{
			ID:          id,
			Email:       req.Email,
			Nickname:    req.Nickname,
			AuthMethod:  "api_key",
			KiroApiKey:  req.KiroApiKey,
			AccessToken: req.KiroApiKey,
			Region:      req.Region,
			AuthRegion:  req.AuthRegion,
			ApiRegion:   req.ApiRegion,
			ExpiresAt:   0,
			Enabled:     true,
			MachineId:   config.GenerateMachineId(),
		}
		if err := config.AddAccount(account); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		h.pool.Reload()
		// Best-effort: populate email/usage/credit from upstream. RefreshAccountInfo
		// uses KiroApiKey (mirrored into AccessToken) as the bearer via
		// applyKiroBaseHeaders; profile-ARN resolution is skipped for api_key.
		go func(acc config.Account) {
			if _, infoErr := RefreshAccountInfo(&acc); infoErr != nil {
				logger.Warnf("[Import] RefreshAccountInfo failed for api_key account %s: %v", accountEmailForLog(&acc), infoErr)
			}
		}(account)
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for api_key account %s: %v", accountEmailForLog(&acc), err)
			}
		}(account)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"account": map[string]interface{}{"id": account.ID, "email": account.Email},
		})
		return
	}

	if req.RefreshToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken is required"})
		return
	}

	// 设置默认值
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	// 标准化 authMethod。external_idp 必须先于 clientId+clientSecret→idc 的推断被识别
	//（external_idp 带 clientId 但没有 clientSecret），否则会被误判成 social 而 refresh 到错误端点。
	req.AuthMethod = normalizeImportAuthMethod(req.AuthMethod, req.ClientID, req.ClientSecret, req.TokenEndpoint)

	// Resolve Azure endpoints from userId (Kiro export, account level) or the
	// accessToken JWT issuer (bare blobs: clientId + token only). A derivation that
	// also clears the allow-list is itself proof the credential is external_idp —
	// IdC/social access tokens are not microsoftonline JWTs, so a bare IdC blob (its
	// iss is an AWS host) won't clear the list and won't be misclassified.
	derivedTE, derivedIss, derivedSc := auth.DeriveExternalIdpEndpoints(req.UserID, req.ClientID, req.AccessToken)
	if derivedTE != "" && auth.ValidateExternalIdpEndpoint(derivedTE) == nil && req.AuthMethod != "external_idp" {
		req.AuthMethod = "external_idp"
	}

	// external_idp 的 tokenEndpoint 是用户可填的新信任边界：必须经 allow-list 校验，
	// 否则一份不信任的 credential JSON 可指向内网/攻击者主机，导致 refresh token 被外泄。
	if req.AuthMethod == "external_idp" {
		// Kiro Account Manager exports and bare blobs omit tokenEndpoint/issuerUrl/
		// scopes; fill them from the derived (userId or accessToken-JWT) tenant.
		if req.TokenEndpoint == "" {
			req.TokenEndpoint = derivedTE
		}
		if req.IssuerURL == "" {
			req.IssuerURL = derivedIss
		}
		if req.Scopes == "" {
			req.Scopes = derivedSc
		}
		if req.ClientID == "" || req.TokenEndpoint == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "external_idp requires clientId and tokenEndpoint (or userId/accessToken to derive it)"})
			return
		}
		if err := auth.ValidateExternalIdpEndpoint(req.TokenEndpoint); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "external IdP endpoint rejected: " + err.Error()})
			return
		}
		if req.IssuerURL != "" {
			if err := auth.ValidateExternalIdpEndpoint(req.IssuerURL); err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "external IdP issuer rejected: " + err.Error()})
				return
			}
		}
	}

	// Resolve the access token to persist. For external_idp we prefer TRUST-ON-IMPORT:
	// when the pasted JSON carries an Azure AD access token (a JWT with a real exp),
	// persist it directly WITHOUT a live refresh round-trip. The JSON can then be
	// imported repeatedly / into multiple instances without each import consuming
	// (rotating) the refresh token, and without requiring egress to Microsoft at
	// import time. The runtime background refresh (backgroundRefresh /
	// ensureValidToken) renews it later when the account is actually used. Falls
	// back to refresh-at-import for idc/social and for external_idp credentials
	// carrying only a refreshToken (so the regression gate — reject when refresh
	// fails — still holds there).
	var (
		accessToken string
		expiresAt   int64
		profileArn  string
	)
	email := req.Email
	if req.AuthMethod == "external_idp" && req.AccessToken != "" {
		if exp := auth.ExpFromAccessTokenJWT(req.AccessToken); exp > 0 {
			accessToken = req.AccessToken
			expiresAt = exp
			profileArn = req.ProfileArn
		}
	}
	if accessToken == "" {
		tempAccount := &config.Account{
			RefreshToken:  req.RefreshToken,
			ClientID:      req.ClientID,
			ClientSecret:  req.ClientSecret,
			AuthMethod:    req.AuthMethod,
			Region:        req.Region,
			TokenEndpoint: req.TokenEndpoint,
			Scopes:        req.Scopes,
		}
		a, newRT, ea, newPA, err := auth.RefreshToken(tempAccount)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
		accessToken = a
		expiresAt = ea
		profileArn = newPA
		if newRT != "" {
			req.RefreshToken = newRT
		}
		if fetchedEmail, _, _ := auth.GetUserInfo(accessToken); fetchedEmail != "" {
			email = fetchedEmail
		}
	}
	if profileArn == "" {
		profileArn = req.ProfileArn // external_idp refresh returns no profileArn
	}

	// 创建账号
	provider := req.Provider
	if provider == "" && req.AuthMethod == "external_idp" {
		provider = "AzureAD"
	}
	// Re-importing a backup must never create a duplicate entry. If an account
	// with the same (resolved) refresh token already exists, update it in place
	// (reusing its id) instead of minting a fresh id and leaving two live
	// accounts sharing the token — which would interleave refresh/rotation
	// conflicts. Mirrors the bulk import path's dedup (config.AddAccounts).
	existingID := ""
	if req.RefreshToken != "" {
		existingID = config.FindAccountIDByRefreshToken(req.RefreshToken)
	}
	id := req.ID
	if existingID != "" {
		id = existingID
	} else if id == "" || config.AccountIDExists(id) {
		id = auth.GenerateAccountID()
	}
	account := config.Account{
		ID:            id,
		Email:         email,
		AccessToken:   accessToken,
		RefreshToken:  req.RefreshToken,
		ClientID:      req.ClientID,
		ClientSecret:  req.ClientSecret,
		AuthMethod:    req.AuthMethod,
		Provider:      provider,
		Region:        req.Region,
		ExpiresAt:     expiresAt,
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
		ProfileArn:    profileArn,
		TokenEndpoint: req.TokenEndpoint,
		IssuerURL:     req.IssuerURL,
		Scopes:        req.Scopes,
	}

	if existingID != "" {
		// Update the existing entry in place rather than creating a duplicate.
		if err := config.UpdateAccount(id, account); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	} else {
		if err := config.AddAccount(account); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// externalIdpAuthMethodAliases are lower-cased authMethod values (or Kiro Account
// Manager provider labels) that mean "external IdP / enterprise SSO" and must
// normalize to "external_idp".
var externalIdpAuthMethodAliases = map[string]bool{
	"external_idp": true,
	"azuread":      true,
	"azure":        true,
	"entra":        true,
	"entra-id":     true,
	"entra_id":     true,
	"microsoft":    true,
	"m365":         true,
	"office365":    true,
	"external":     true,
}

// normalizeImportAuthMethod maps a pasted credential JSON's authMethod (plus its
// clientId/clientSecret/tokenEndpoint) onto one of the three canonical methods
// ("external_idp" | "idc" | "social"). external_idp MUST be detected before the
// clientId+clientSecret→idc inference, because external_idp accounts carry clientId
// but NO clientSecret, so the old default branch misclassified them as "social" and
// refresh hit the wrong endpoint.
//
// It preserves the pre-existing idc/social heuristics:
//   - empty authMethod + clientId present             -> idc
//   - empty authMethod, no clientId                   -> social
//   - "enterprise" (Kiro Account Manager IdC label)   -> idc
//   - unrecognized non-empty + clientId+clientSecret  -> idc, else social
func normalizeImportAuthMethod(authMethod, clientID, clientSecret, tokenEndpoint string) string {
	am := strings.ToLower(strings.TrimSpace(authMethod))
	switch {
	case externalIdpAuthMethodAliases[am]:
		return "external_idp"
	case tokenEndpoint != "": // infer when not declared explicitly
		return "external_idp"
	case am == "social" || am == "google" || am == "github":
		return "social"
	case am == "idc" || am == "builderid" || am == "enterprise":
		return "idc"
	}
	if am == "" {
		if clientID != "" {
			return "idc"
		}
		return "social"
	}
	if clientID != "" && clientSecret != "" {
		return "idc"
	}
	return "social"
}

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"refillEligible":  h.pool.RefillEligibleCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":                           config.GetApiKey(),
		"requireApiKey":                    config.IsApiKeyRequired(),
		"port":                             config.GetPort(),
		"host":                             config.GetHost(),
		"allowOverUsage":                   config.GetAllowOverUsage(),
		"accountConcurrencyRefreshSeconds": config.GetAccountConcurrencyRefreshSeconds(),
		"promptCacheMaxRatio":              config.GetPromptCacheMaxRatio(),
		"promptCacheMaxEntries":            config.GetPromptCacheMaxEntries(),
		"promptCacheTTLSeconds":            config.GetPromptCacheTTLSeconds(),
		"rateLimit":                        config.GetRateLimitConfig(),
		"scheduling":                       config.GetSchedulingConfig(),
		"accountPriority":                  config.GetAccountPriorityConfig(),
		"cacheMode":                        config.GetCacheMode(),
		"functionCache":                    config.GetFunctionCacheConfig(),
		"reasoningCap":                     config.GetReasoningCapConfig(),
		"reasoningCapEnvFallbackMode":      reasoningCapModeFromEnvFallback(),
		"agentCompletionMarkerEnabled":     config.GetAgentCompletionMarkerEnabled(),
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey                           *string                       `json:"apiKey,omitempty"`
		RequireApiKey                    *bool                         `json:"requireApiKey,omitempty"`
		Password                         string                        `json:"password,omitempty"`
		AllowOverUsage                   *bool                         `json:"allowOverUsage,omitempty"`
		AccountConcurrencyRefreshSeconds *int                          `json:"accountConcurrencyRefreshSeconds,omitempty"`
		PromptCacheMaxRatio              *float64                      `json:"promptCacheMaxRatio,omitempty"`
		PromptCacheMaxEntries            *int                          `json:"promptCacheMaxEntries,omitempty"`
		PromptCacheTTLSeconds            *int                          `json:"promptCacheTTLSeconds,omitempty"`
		RateLimit                        *config.RateLimitConfig       `json:"rateLimit,omitempty"`
		Scheduling                       *config.SchedulingConfig      `json:"scheduling,omitempty"`
		AccountPriority                  *config.AccountPriorityConfig `json:"accountPriority,omitempty"`
		CacheMode                        *string                       `json:"cacheMode,omitempty"`
		FunctionCache                    *config.FunctionCacheConfig   `json:"functionCache,omitempty"`
		ReasoningCap                     *config.ReasoningCapConfig    `json:"reasoningCap,omitempty"`
		AgentCompletionMarkerEnabled     *bool                         `json:"agentCompletionMarkerEnabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.PromptCacheMaxRatio != nil && (*req.PromptCacheMaxRatio < config.MinPromptCacheRatio || *req.PromptCacheMaxRatio > config.MaxPromptCacheRatio) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("promptCacheMaxRatio must be between %.2f and %.2f", config.MinPromptCacheRatio, config.MaxPromptCacheRatio)})
		return
	}
	if req.PromptCacheMaxEntries != nil && (*req.PromptCacheMaxEntries < config.MinPromptCacheEntries || *req.PromptCacheMaxEntries > config.MaxPromptCacheEntries) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("promptCacheMaxEntries must be between %d and %d", config.MinPromptCacheEntries, config.MaxPromptCacheEntries)})
		return
	}
	if req.PromptCacheTTLSeconds != nil && !config.IsValidPromptCacheTTLSeconds(*req.PromptCacheTTLSeconds) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("promptCacheTTLSeconds must be either %d or %d", config.DefaultPromptCacheTTLSeconds, config.ExtendedPromptCacheTTLSeconds)})
		return
	}
	if req.CacheMode != nil && *req.CacheMode != config.CacheModeNative && *req.CacheMode != config.CacheModeFunctionSimulation {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "cacheMode must be native or function_simulation"})
		return
	}
	if req.ReasoningCap != nil {
		if msg := validateReasoningCapConfig(*req.ReasoningCap); msg != "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
	}
	if req.AccountConcurrencyRefreshSeconds != nil && (*req.AccountConcurrencyRefreshSeconds < config.MinAccountConcurrencyRefreshSeconds || *req.AccountConcurrencyRefreshSeconds > config.MaxAccountConcurrencyRefreshSeconds) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("accountConcurrencyRefreshSeconds must be between %d and %d", config.MinAccountConcurrencyRefreshSeconds, config.MaxAccountConcurrencyRefreshSeconds)})
		return
	}

	if req.ApiKey != nil || req.RequireApiKey != nil || req.Password != "" {
		if err := config.UpdateSettingsPatch(req.ApiKey, req.RequireApiKey, req.Password); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// 更新超额使用设置
	if req.AllowOverUsage != nil {
		if err := config.UpdateAllowOverUsage(*req.AllowOverUsage); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// Rebuild the pool so over-quota accounts are re-included or dropped immediately.
		h.pool.Reload()
	}
	if req.AccountConcurrencyRefreshSeconds != nil {
		if err := config.UpdateAccountConcurrencyRefreshSeconds(*req.AccountConcurrencyRefreshSeconds); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	if req.PromptCacheMaxRatio != nil || req.PromptCacheMaxEntries != nil || req.PromptCacheTTLSeconds != nil {
		h.promptCacheConfigMu.Lock()
		defer h.promptCacheConfigMu.Unlock()
		if err := config.UpdatePromptCacheSettings(req.PromptCacheMaxRatio, req.PromptCacheMaxEntries, req.PromptCacheTTLSeconds); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if req.PromptCacheMaxEntries != nil && h.promptCache != nil {
			h.promptCache.SetMaxEntries(config.GetPromptCacheMaxEntries())
		}
		if req.PromptCacheTTLSeconds != nil && h.promptCache != nil {
			h.promptCache.SetDefaultTTL(time.Duration(config.GetPromptCacheTTLSeconds()) * time.Second)
		}
	}
	if req.RateLimit != nil || req.Scheduling != nil || req.AccountPriority != nil {
		if err := config.UpdateDispatchPolicies(req.RateLimit, req.Scheduling, req.AccountPriority); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	if req.ReasoningCap != nil {
		if err := config.UpdateReasoningCapConfig(*req.ReasoningCap); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	if req.AgentCompletionMarkerEnabled != nil {
		if err := config.UpdateAgentCompletionMarkerEnabled(*req.AgentCompletionMarkerEnabled); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	if req.CacheMode != nil || req.FunctionCache != nil {
		if err := config.UpdateCachePolicy(req.CacheMode, req.FunctionCache); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	if err := config.UpdateStats(0, 0, 0, 0, 0); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	atomic.StoreInt64(&h.totalRequests, 0)
	atomic.StoreInt64(&h.successRequests, 0)
	atomic.StoreInt64(&h.failedRequests, 0)
	atomic.StoreInt64(&h.totalTokens, 0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	if h.traffic != nil {
		h.traffic.Reset()
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetTraffic(w http.ResponseWriter, r *http.Request) {
	rangeName := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("range")))
	if rangeName == "" {
		rangeName = "24h"
	}

	var window time.Duration
	switch rangeName {
	case "1h":
		window = time.Hour
	case "6h":
		window = 6 * time.Hour
	case "24h":
		window = 24 * time.Hour
	default:
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "range must be one of: 1h, 6h, 24h"})
		return
	}

	snapshot := h.traffic.Query(window)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"range":         rangeName,
		"startTime":     snapshot.StartTime,
		"endTime":       snapshot.EndTime,
		"bucketSeconds": snapshot.BucketSeconds,
		"totals":        snapshot.Totals,
		"buckets":       snapshot.Buckets,
	})
}

func (h *Handler) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs": h.getRequestLogs(),
	})
}

func (h *Handler) apiClearLogs(w http.ResponseWriter, r *http.Request) {
	h.requestLogsMu.Lock()
	h.requestLogs = h.requestLogs[:0]
	h.requestLogsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if err := h.ensureValidTokenContext(r.Context(), account); err != nil {
		if r.Context().Err() != nil {
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	leased, allFull := h.pool.AcquireForTest(account)
	if leased == nil {
		if allFull {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": allAccountsConcurrencyFullMessage})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account is not currently available"})
		return
	}
	account = leased
	defer h.releaseAccount(account)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := bindKiroPayloadContext(OpenAIToKiro(openaiReq, thinking), r.Context())

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnError:        func(err error) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}

	err := CallKiroAPI(account, kiroPayload, callback)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
	})
}

// apiRefreshAccount 刷新账户信息（使用量、订阅等）
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// RefreshAccountInfo persists ban, quota, and subscription changes to
	// config.json. Keep the in-memory dispatch pool in the same generation for
	// every success and early-error return from this endpoint.
	if h.pool != nil {
		defer h.pool.Reload()
	}

	// 先尝试刷新 token（不管是否过期，确保 token 有效）
	refreshTokenIfNeeded := func() error {
		if account.RefreshToken == "" {
			return nil
		}
		return h.refreshAccountToken(account, true)
	}

	// 检查 token 是否快过期，先刷新
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := refreshTokenIfNeeded(); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}

	// 获取账户信息
	info, err := RefreshAccountInfo(account)
	if err != nil {
		// 检查是否为封禁相关错误
		errMsg := err.Error()
		if isSuspensionErrorMessage(errMsg) {
			// 封禁状态已在 RefreshAccountInfo 中处理，静默返回成功
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Account status updated",
			})
			return
		}

		// 如果是 403/401，说明 token 无效，尝试刷新后重试
		if isAuthErrorMessage(errMsg) {
			if refreshErr := refreshTokenIfNeeded(); refreshErr == nil {
				// 重试
				info, err = RefreshAccountInfo(account)
				if err != nil {
					// 重试后仍然失败，检查是否为封禁状态
					if isSuspensionErrorMessage(err.Error()) {
						json.NewEncoder(w).Encode(map[string]interface{}{
							"success": true,
							"message": "Account status updated",
						})
						return
					}
				}
			}
		}

		// 其他错误才显示错误信息
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// 保存到配置
	if err := config.UpdateAccountInfo(id, *info); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"info":    info,
	})
}

// apiGetAccountFull 获取单个账号的完整信息（包含敏感字段）
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 查找指定账号
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 获取运行时统计
	var stats config.Account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
	}

	// 返回完整账号信息（包含敏感字段）
	result := map[string]interface{}{
		"id":                account.ID,
		"email":             account.Email,
		"userId":            account.UserId,
		"nickname":          account.Nickname,
		"accessToken":       account.AccessToken,
		"refreshToken":      account.RefreshToken,
		"clientId":          account.ClientID,
		"clientSecret":      account.ClientSecret,
		"authMethod":        account.AuthMethod,
		"provider":          account.Provider,
		"region":            account.Region,
		"expiresAt":         account.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"overageStatus":     account.OverageStatus,
		"overageCapability": account.OverageCapability,
		"overageCap":        account.OverageCap,
		"overageRate":       account.OverageRate,
		"currentOverages":   account.CurrentOverages,
		"overageCheckedAt":  account.OverageCheckedAt,
		"proxyURL":          account.ProxyURL,
		"enabled":           account.Enabled,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      account.UsageCurrent,
		"usageLimit":        account.UsageLimit,
		"usagePercent":      account.UsagePercent,
		"nextResetDate":     account.NextResetDate,
		"lastRefresh":       account.LastRefresh,
		"trialUsageCurrent": account.TrialUsageCurrent,
		"trialUsageLimit":   account.TrialUsageLimit,
		"trialUsagePercent": account.TrialUsagePercent,
		"trialStatus":       account.TrialStatus,
		"trialExpiresAt":    account.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
	}

	json.NewEncoder(w).Encode(result)
}

// apiGetAccountModels 获取账户可用模型
func (h *Handler) apiGetAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	models, err := ListAvailableModels(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 同步更新路由缓存
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(id, modelIDs)
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// apiGetAccountModelsCached 返回账号已缓存的模型列表（不实时拉取）
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	models := h.pool.GetModelList(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// ==================== 静态文件服务 ====================

var embeddedAdminFileServer = http.FileServer(http.FS(webassets.Assets))
var embeddedAdminStaticServer = http.StripPrefix("/admin", embeddedAdminFileServer)

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	request := r.Clone(r.Context())
	request.URL.Path = "/"
	embeddedAdminFileServer.ServeHTTP(w, request)
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	assetPath := strings.TrimPrefix(r.URL.Path, "/admin/")
	info, err := fs.Stat(webassets.Assets, assetPath)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	embeddedAdminStaticServer.ServeHTTP(w, r)
}

// apiGetThinkingConfig 获取 thinking 配置
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":       cfg.Suffix,
		"openaiFormat": cfg.OpenAIFormat,
		"claudeFormat": cfg.ClaudeFormat,
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix       string `json:"suffix"`
		OpenAIFormat string `json:"openaiFormat"`
		ClaudeFormat string `json:"claudeFormat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证格式
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig 获取端点配置
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
	})
}

// apiUpdateEndpointConfig 更新端点配置
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string `json:"preferredEndpoint"`
		EndpointFallback  *bool  `json:"endpointFallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdatePreferredEndpoint(req.PreferredEndpoint); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.EndpointFallback != nil {
		config.UpdateEndpointFallback(*req.EndpointFallback)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

// apiGetProxy 获取当前代理配置
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"proxyURL": config.GetProxyURL(),
	})
}

// apiUpdateProxy 更新代理配置并立即生效
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxyURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证代理 URL 格式（非空时）
	if req.ProxyURL != "" {
		if !strings.HasPrefix(req.ProxyURL, "http://") &&
			!strings.HasPrefix(req.ProxyURL, "https://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5h://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "proxyURL must start with http://, https://, socks5://, or socks5h://"})
			return
		}
	}

	if err := config.UpdateProxySettings(req.ProxyURL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 立即应用新的代理配置
	applyProxyConfig(req.ProxyURL)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"` // 为空则导出全部
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空或解析失败，导出全部
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		AccessToken  string `json:"accessToken"`
		CsrfToken    string `json:"csrfToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId,omitempty"`
		ClientSecret string `json:"clientSecret,omitempty"`
		Region       string `json:"region,omitempty"`
		ExpiresAt    int64  `json:"expiresAt"`
		AuthMethod   string `json:"authMethod,omitempty"`
		Provider     string `json:"provider,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:        a.ID,
			Email:     a.Email,
			Nickname:  a.Nickname,
			Idp:       idp,
			UserId:    a.UserId,
			MachineId: a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:  a.AccessToken,
				CsrfToken:    "",
				RefreshToken: a.RefreshToken,
				ClientID:     a.ClientID,
				ClientSecret: a.ClientSecret,
				Region:       a.Region,
				ExpiresAt:    a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod:   authMethod,
				Provider:     a.Provider,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
