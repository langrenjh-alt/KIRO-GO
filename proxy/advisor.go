package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
)

const (
	advisorToolType     = "advisor_20260301"
	advisorToolName     = "advisor"
	defaultAdvisorModel = "claude-opus-4.8"
)

const advisorReviewInstruction = `<advisor_role>
You are a senior strategy advisor reviewing the full conversation for an executor model. Identify the most important gaps, risks, and next action. Return concise, concrete guidance only. Do not perform client-side tool calls and do not speak as the executor.
</advisor_role>`

type kiroAPICaller func(*config.Account, *KiroPayload, *KiroStreamCallback) error

type bufferedClaudeCall struct {
	Account               *config.Account
	Content               string
	Reasoning             ClaudeReasoningResult
	ToolUses              []KiroToolUse
	InputTokens           int
	OutputTokens          int
	Credits               float64
	UpstreamStopReason    string
	ContextWindowExceeded bool
}

var errNoAdvisorAccounts = errors.New("no available accounts for advisor request")

func isNativeAdvisorTool(tool ClaudeTool) bool {
	return strings.TrimSpace(tool.Type) == advisorToolType && strings.TrimSpace(tool.Name) == advisorToolName
}

func findNativeAdvisorTool(req *ClaudeRequest) (*ClaudeTool, bool) {
	if req == nil {
		return nil, false
	}
	for i := range req.Tools {
		if isNativeAdvisorTool(req.Tools[i]) {
			return &req.Tools[i], true
		}
	}
	return nil, false
}

func withoutNativeAdvisorTools(tools []ClaudeTool) []ClaudeTool {
	filtered := make([]ClaudeTool, 0, len(tools))
	for _, tool := range tools {
		if !isNativeAdvisorTool(tool) {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func (h *Handler) handleAdvisorRequest(
	w http.ResponseWriter,
	req *ClaudeRequest,
	responseModel string,
	thinking bool,
	thinkingOpts claudeThinkingResponseOptions,
	isolationSeed string,
	apiKeyID string,
) {
	if req == nil {
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", "Advisor request is missing")
		return
	}
	if req.Stream {
		if _, ok := w.(http.Flusher); !ok {
			h.sendClaudeError(w, http.StatusInternalServerError, "api_error", "Streaming not supported")
			return
		}
	}
	advisorTool, ok := findNativeAdvisorTool(req)
	if !ok {
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", "Advisor tool declaration is missing")
		return
	}

	started := time.Now()
	advisorModel := strings.TrimSpace(advisorTool.Model)
	if advisorModel == "" {
		advisorModel = defaultAdvisorModel
	}
	advisorModel, _ = ParseModelAndThinking(advisorModel, config.GetThinkingConfig().Suffix)

	advisorReq := *req
	advisorReq.Model = advisorModel
	advisorReq.responseModel = ""
	advisorReq.Tools = nil
	advisorReq.ToolChoice = nil
	advisorReq.Thinking = nil
	advisorReq.OutputConfig = nil
	advisorReq.StopSequences = nil
	advisorReq.System = appendClaudeSystemText(req.System, advisorReviewInstruction)
	if advisorTool.MaxTokens > 0 {
		advisorReq.MaxTokens = advisorTool.MaxTokens
	} else if advisorReq.MaxTokens <= 0 {
		advisorReq.MaxTokens = 4096
	}
	advisorPayload := ClaudeToKiro(&advisorReq, false)
	advisorEstimate := estimateClaudeRequestInputTokens(&advisorReq)
	advisorResult, err := h.callClaudeBuffered(advisorModel, advisorPayload, false, advisorEstimate)
	if err != nil {
		h.handleAdvisorFailure(w, req.Model, err)
		return
	}
	advice := strings.TrimSpace(advisorResult.Content)
	if advice == "" {
		advice = strings.TrimSpace(advisorResult.Reasoning.Text)
	}
	if advice == "" {
		err := errors.New("advisor model returned no guidance")
		h.recordFailureWithDetails("claude", req.Model, advisorResult.Account.ID, err)
		h.sendClaudeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	executorReq := *req
	executorReq.Tools = withoutNativeAdvisorTools(req.Tools)
	if choice, _ := resolveToolChoice(req.ToolChoice); choice.Mode == "specific" && choice.Name == advisorToolName {
		executorReq.ToolChoice = nil
	}
	executorReq.Messages = appendClaudeLatestUserText(
		req.Messages,
		"\n\n<advisor_result>\n"+advice+"\n</advisor_result>\nUse this guidance while completing the user's request.",
	)
	executorEffectiveReq := cloneClaudeRequestForThinking(&executorReq, thinking)
	executorEstimate := estimateClaudeRequestInputTokens(executorEffectiveReq)
	var cacheProfile *promptCacheProfile
	if h.promptCache != nil {
		cacheProfile = h.promptCache.BuildClaudeProfile(executorEffectiveReq, executorEstimate, isolationSeed)
	}
	executorPayload := ClaudeToKiro(&executorReq, thinking)
	toolUseID := newClaudeServerToolUseID()
	serverBlocks := []ClaudeContentBlock{
		{Type: "server_tool_use", ID: toolUseID, Name: advisorToolName, Input: map[string]interface{}{}},
		{
			Type:      "advisor_tool_result",
			ToolUseID: toolUseID,
			Content: map[string]interface{}{
				"type": "advisor_result",
				"text": advice,
			},
		},
	}
	if req.Stream {
		h.handleClaudeStreamWithPrelude(
			w, executorPayload, req.Model, responseModel, thinking, thinkingOpts, executorEstimate, cacheProfile, apiKeyID,
			&claudeStreamPrelude{
				Blocks:       serverBlocks,
				InputTokens:  advisorResult.InputTokens,
				OutputTokens: advisorResult.OutputTokens,
				Credits:      advisorResult.Credits,
			},
		)
		return
	}
	executorResult, err := h.callClaudeBuffered(req.Model, executorPayload, thinking, executorEstimate)
	if err != nil {
		h.handleAdvisorFailure(w, req.Model, err)
		return
	}

	cacheUsage := promptCacheUsage{}
	if h.promptCache != nil && executorResult.Account != nil {
		cacheUsage = h.promptCache.Compute(executorResult.Account.ID, cacheProfile)
		h.promptCache.Update(executorResult.Account.ID, cacheProfile)
	}

	finalContent, extractedReasoning := extractThinkingFromContent(executorResult.Content)
	finalContent, matchedStopSequence := truncateAtStopSequence(finalContent, executorPayload.StopSequences)
	reasoning := executorResult.Reasoning
	if thinking && reasoning.Text == "" && extractedReasoning != "" {
		reasoning.Text = extractedReasoning
	}
	if !thinking {
		reasoning = ClaudeReasoningResult{}
	}

	includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && reasoning.Text != ""
	responseReasoning := reasoning
	if includeEmptyThinkingBlock {
		responseReasoning.Text = ""
	}
	if thinking && responseReasoning.Text != "" {
		switch thinkingOpts.Format {
		case "think":
			finalContent = "<think>" + responseReasoning.Text + "</think>" + finalContent
			responseReasoning = ClaudeReasoningResult{}
		case "reasoning_content":
			finalContent = responseReasoning.Text + finalContent
			responseReasoning = ClaudeReasoningResult{}
		}
	}

	totalInputTokens := clampClaudeInputTokensToModelWindow(
		req.Model,
		advisorResult.InputTokens+executorResult.InputTokens,
	)
	cacheUsage = reanchorClaudeCacheUsage(
		cacheProfile,
		cacheUsage,
		minInt(executorResult.InputTokens, totalInputTokens),
	)
	totalOutputTokens := advisorResult.OutputTokens + estimateClaudeOutputTokensWithRedacted(
		finalContent, reasoning.Text, reasoning.Redacted, executorResult.ToolUses,
	)
	resp := KiroToClaudeResponseWithReasoning(
		finalContent, responseReasoning, includeEmptyThinkingBlock, executorResult.ToolUses,
		totalInputTokens, totalOutputTokens, responseModel,
	)
	resp.Content = append(serverBlocks, resp.Content...)
	resp.StopReason = resolveClaudeStopReason(
		len(executorResult.ToolUses), executorResult.ContextWindowExceeded, executorResult.UpstreamStopReason,
	)
	if matchedStopSequence != "" {
		resp.StopReason = "stop_sequence"
		resp.StopSequence = &matchedStopSequence
	}
	resp.Usage.InputTokens = billedClaudeInputTokens(totalInputTokens, cacheUsage)
	resp.Usage.OutputTokens = totalOutputTokens
	resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
	resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
	resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
		Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
		Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
	}
	resp.Usage.OutputTokensDetails = claudeReasoningUsageDetails(reasoning, includeEmptyThinkingBlock)

	totalCredits := advisorResult.Credits + executorResult.Credits
	h.recordSuccessForApiKeyWithCache(apiKeyID, totalInputTokens, totalOutputTokens, totalCredits, cacheUsage)
	accountID := ""
	if executorResult.Account != nil {
		accountID = executorResult.Account.ID
	}
	h.recordSuccessLog("claude", req.Model, accountID, totalInputTokens+totalOutputTokens, totalCredits, time.Since(started).Milliseconds())

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.recordFailureWithDetails("claude", req.Model, accountID, err)
	}
}

func appendClaudeSystemText(system interface{}, text string) interface{} {
	block := map[string]interface{}{"type": "text", "text": text}
	switch value := system.(type) {
	case nil:
		return []interface{}{block}
	case string:
		blocks := make([]interface{}, 0, 2)
		if value != "" {
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": value})
		}
		return append(blocks, block)
	case []interface{}:
		blocks := append([]interface{}(nil), value...)
		return append(blocks, block)
	case []string:
		blocks := make([]interface{}, 0, len(value)+1)
		for _, item := range value {
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": item})
		}
		return append(blocks, block)
	default:
		return []interface{}{block}
	}
}

func appendClaudeLatestUserText(messages []ClaudeMessage, text string) []ClaudeMessage {
	result := append([]ClaudeMessage(nil), messages...)
	for i := len(result) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(result[i].Role), "user") {
			continue
		}
		block := map[string]interface{}{"type": "text", "text": text}
		switch content := result[i].Content.(type) {
		case string:
			result[i].Content = []interface{}{
				map[string]interface{}{"type": "text", "text": content},
				block,
			}
		case []interface{}:
			blocks := append([]interface{}(nil), content...)
			result[i].Content = append(blocks, block)
		default:
			result[i].Content = []interface{}{block}
		}
		break
	}
	return result
}

func (h *Handler) callClaudeBuffered(
	model string,
	payload *KiroPayload,
	thinking bool,
	estimatedInputTokens int,
) (*bufferedClaudeCall, error) {
	if h.pool == nil {
		return nil, errNoAdvisorAccounts
	}
	call := h.kiroCaller
	if call == nil {
		call = CallKiroAPI
	}
	excluded := make(map[string]bool)
	retryCounts := make(map[string]int)
	retrySameID := ""
	var lastErr error

	for attempt := 0; attempt < h.maxAccountAttempts(); attempt++ {
		account, allFull := h.acquireAccountForModel(model, excluded, retrySameID)
		retrySameID = ""
		if account == nil {
			if h.startExpiredAccountRefreshes(model, excluded) {
				lastErr = errAccountRefreshInProgress
			} else if allFull {
				lastErr = fmt.Errorf(allAccountsConcurrencyFullMessage)
			}
			break
		}
		if err := h.ensureValidToken(account); err != nil {
			h.releaseAccount(account)
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		result := &bufferedClaudeCall{Account: account}
		var content strings.Builder
		var reasoning strings.Builder
		var realInputTokens int
		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					if thinking {
						reasoning.WriteString(text)
					}
					return
				}
				content.WriteString(text)
			},
			OnReasoning: func(event KiroReasoningContentEvent) {
				if thinking {
					reasoning.WriteString(event.Text)
					if event.Signature != "" {
						result.Reasoning.Signature = event.Signature
					}
					if event.RedactedContent != "" {
						result.Reasoning.Redacted = append(result.Reasoning.Redacted, event.RedactedContent)
					}
					return
				}
			},
			OnToolUse: func(toolUse KiroToolUse) {
				result.ToolUses = append(result.ToolUses, toolUse)
			},
			OnComplete: func(inputTokens, outputTokens int) {
				result.InputTokens = inputTokens
				result.OutputTokens = outputTokens
			},
			OnStopReason: func(reason string) {
				result.UpstreamStopReason = reason
			},
			OnCredits: func(credits float64) {
				result.Credits = credits
			},
			OnContextUsage: func(percent float64) {
				realInputTokens = int(percent * float64(getContextWindowSize(model)) / 100.0)
				if percent >= 100 {
					result.ContextWindowExceeded = true
				}
			},
		}
		if err := call(account, payload, callback); err != nil {
			h.releaseAccount(account)
			lastErr = err
			if isKiroInputTooLongError(err) {
				return nil, err
			}
			if _, protocolErr := kiroStreamProtocolErrorCode(err); protocolErr {
				return nil, err
			}
			if isRateLimitError(err) {
				decision := h.applyRateLimitPolicy(account, retryCounts, excluded)
				if decision.Return429 {
					return nil, err
				}
				retrySameID = decision.RetrySameID
				h.sleepBeforeRateLimitRetry(decision)
				continue
			}
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		h.releaseAccount(account)

		result.Content = content.String()
		result.Reasoning.Text = reasoning.String()
		if realInputTokens > 0 {
			result.InputTokens = realInputTokens
		} else if result.InputTokens <= 0 {
			result.InputTokens = max(1, clampClaudeInputTokensToModelWindow(model, estimatedInputTokens))
		}
		result.OutputTokens = estimateClaudeOutputTokensWithRedacted(
			result.Content, result.Reasoning.Text, result.Reasoning.Redacted, result.ToolUses,
		)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, result.InputTokens+result.OutputTokens, result.Credits)
		return result, nil
	}
	if lastErr == nil {
		lastErr = errNoAdvisorAccounts
	}
	return nil, lastErr
}

func (h *Handler) handleAdvisorFailure(w http.ResponseWriter, model string, err error) {
	h.recordFailureWithDetails("claude", model, "", err)
	if isKiroInputTooLongError(err) {
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if code, protocolErr := kiroStreamProtocolErrorCode(err); protocolErr {
		h.sendClaudeError(w, http.StatusBadGateway, code, err.Error())
		return
	}
	if errors.Is(err, errNoAdvisorAccounts) {
		h.sendClaudeError(w, http.StatusServiceUnavailable, "api_error", "No available accounts")
		return
	}
	if isAccountRefreshInProgressError(err) {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(expiredAccountRefreshWait.Seconds())))
		h.sendClaudeError(w, accountRefreshInProgressStatus, "account_refreshing", accountRefreshInProgressMessage)
		return
	}
	if isAllAccountsConcurrencyFullError(err) {
		h.sendClaudeError(w, http.StatusServiceUnavailable, "overloaded_error", allAccountsConcurrencyFullMessage)
		return
	}
	if isRateLimitError(err) {
		h.sendClaudeError(w, http.StatusTooManyRequests, "rate_limit_error", err.Error())
		return
	}
	h.sendClaudeError(w, http.StatusBadGateway, "api_error", err.Error())
}
