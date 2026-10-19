package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
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
	Model                 string
	Content               string
	Reasoning             ClaudeReasoningResult
	ToolUses              []KiroToolUse
	InputTokens           int
	OutputTokens          int
	Credits               float64
	UpstreamStopReason    string
	ContextWindowExceeded bool
	ContextInputTokens    int
	RetryUsage            retryBillingUsage
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
	ctx context.Context,
	req *ClaudeRequest,
	responseModel string,
	thinking bool,
	thinkingOpts claudeThinkingResponseOptions,
	isolationSeed string,
	apiKeyID string,
	cacheTracker *promptCacheTracker,
	cachePolicy config.PromptCachePolicy,
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
	advisorPayload := bindKiroPayloadContext(ClaudeToKiro(&advisorReq, false), ctx)
	advisorEstimate := estimateClaudeRequestInputTokens(&advisorReq)
	advisorResult, err := h.callClaudeBuffered(advisorModel, advisorPayload, false, advisorEstimate)
	if err != nil {
		if isDownstreamRequestCanceled(advisorPayload, err) {
			return
		}
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
	if cacheTracker != nil {
		cacheProfile = cacheTracker.BuildClaudeProfileWithPolicy(executorEffectiveReq, executorEstimate, cachePolicy, isolationSeed)
		if cacheProfile != nil {
			cacheProfile.requestTiming = requestTimingFromContext(ctx)
		}
	}
	executorPayload := bindKiroPayloadContext(ClaudeToKiro(&executorReq, thinking), ctx)
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
				RetryUsage:   advisorResult.RetryUsage,
				QuotaOwners:  []quotaOwner{{AccountID: advisorResult.Account.ID, Model: advisorResult.Model, Input: int64(advisorResult.InputTokens), Output: int64(advisorResult.OutputTokens)}},
			},
		)
		return
	}
	executorResult, err := h.callClaudeBuffered(req.Model, executorPayload, thinking, executorEstimate)
	if err != nil {
		if isDownstreamRequestCanceled(executorPayload, err) {
			return
		}
		h.handleAdvisorFailure(w, req.Model, err)
		return
	}

	cacheUsage := promptCacheUsage{}
	if cacheTracker != nil && executorResult.Account != nil {
		cacheUsage = cacheTracker.Compute(executorResult.Account.ID, cacheProfile)
		cacheTracker.Update(executorResult.Account.ID, cacheProfile)
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
	usagePayload := contextUsagePayloadFromTokens(req.Model, executorResult.ContextInputTokens)
	publicInputTokens := claudePublicInputTokens(usagePayload, req.Model, totalInputTokens)
	responseCacheUsage := reanchorClaudeCacheUsage(cacheProfile, cacheUsage, totalInputTokens)
	responseCacheUsage = claudeTerminalCacheUsage(cacheProfile, usagePayload, req.Model, totalInputTokens, responseCacheUsage)
	var retryUsage retryBillingUsage
	retryUsage.merge(advisorResult.RetryUsage)
	retryUsage.merge(executorResult.RetryUsage)
	publicInputTokens, responseCacheUsage = retryUsage.apply(publicInputTokens, responseCacheUsage)
	totalOutputTokens := advisorResult.OutputTokens + estimateClaudeOutputTokensWithRedacted(
		finalContent, reasoning.Text, reasoning.Redacted, executorResult.ToolUses,
	)
	accountedOutputTokens := retryUsage.finalAttemptOutputTokens(totalOutputTokens)
	resp := KiroToClaudeResponseWithReasoning(
		finalContent, responseReasoning, includeEmptyThinkingBlock, executorResult.ToolUses,
		publicInputTokens, totalOutputTokens, responseModel,
	)
	resp.Content = append(serverBlocks, resp.Content...)
	resp.StopReason = resolveClaudeStopReason(
		len(executorResult.ToolUses), executorResult.ContextWindowExceeded, executorResult.UpstreamStopReason,
	)
	if matchedStopSequence != "" {
		resp.StopReason = "stop_sequence"
		resp.StopSequence = &matchedStopSequence
	}
	resp.Usage.InputTokens = billedClaudeInputTokens(publicInputTokens, responseCacheUsage)
	resp.Usage.OutputTokens = totalOutputTokens
	resp.Usage.CacheCreationInputTokens = responseCacheUsage.CacheCreationInputTokens
	resp.Usage.CacheReadInputTokens = responseCacheUsage.CacheReadInputTokens
	resp.Usage.KiroContextUsage = kiroContextUsageFromTokens(req.Model, executorResult.ContextInputTokens)
	resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
		Ephemeral5mInputTokens: responseCacheUsage.CacheCreation5mInputTokens,
		Ephemeral1hInputTokens: responseCacheUsage.CacheCreation1hInputTokens,
	}
	resp.Usage.OutputTokensDetails = claudeReasoningUsageDetails(reasoning, includeEmptyThinkingBlock)
	inflateClaudeUsage(&resp.Usage)
	stageAccountQuotaReceipt(ctx, responseModel, quotaUsageFromClaude(resp.Usage), quotaFinalSources(publicInputTokens, totalOutputTokens, responseCacheUsage, retryUsage,
		quotaOwner{AccountID: advisorResult.Account.ID, Model: advisorResult.Model, Input: int64(advisorResult.InputTokens), Output: int64(advisorResult.OutputTokens)},
		quotaOwner{AccountID: executorResult.Account.ID, Model: executorResult.Model, Input: int64(executorResult.InputTokens), Output: int64(executorResult.RetryUsage.finalAttemptOutputTokens(totalOutputTokens - advisorResult.OutputTokens))}))

	totalCredits := advisorResult.Credits + executorResult.Credits
	h.recordSuccessForApiKeyWithCache(apiKeyID, totalInputTokens, accountedOutputTokens, totalCredits, cacheUsage)
	retryUsage.recordForAPIKey(apiKeyID)
	accountID := ""
	if executorResult.Account != nil {
		accountID = executorResult.Account.ID
	}
	h.recordSuccessLog("claude", req.Model, accountID, totalInputTokens+accountedOutputTokens, totalCredits, time.Since(started).Milliseconds())

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.recordFailureWithDetails("claude", req.Model, accountID, err)
	} else {
		completeAccountQuotaReceipt(ctx)
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
	payload = cloneKiroPayload(payload)
	if h.pool == nil {
		return nil, errNoAdvisorAccounts
	}
	excluded := make(map[string]bool)
	retryCounts := make(map[string]int)
	retrySameID := ""
	var lastErr error
	preResponseRetryCount := 0
	var retryUsage retryBillingUsage

	attemptLimit := h.maxAccountAttempts()
	for attempt := 0; attempt < attemptLimit; attempt++ {
		attemptModel := accountModelForAttempt(model, payload)
		account, allFull := h.acquireAccountForModel(attemptModel, excluded, retrySameID, kiroPayloadContext(payload))
		retrySameID = ""
		if account == nil {
			if h.promoteOpus55EmptyToOpus5(payload, excluded) {
				retrySameID = ""
				attemptLimit++
				continue
			}
			if h.startExpiredAccountRefreshes(attemptModel, excluded) {
				lastErr = errAccountRefreshInProgress
			} else if err := h.unavailableAccountError(attemptModel, allFull); err != nil {
				lastErr = err
			}
			break
		}
		if err := h.ensureValidTokenForPayload(account, payload); err != nil {
			h.releaseAccount(account)
			if isDownstreamRequestCanceled(payload, err) {
				return nil, err
			}
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		result := &bufferedClaudeCall{Account: account, Model: attemptModel}
		var content strings.Builder
		var reasoning strings.Builder
		var suppressedReasoningObserved bool
		callback := &KiroStreamCallback{
			RequireClientOutput:      true,
			AllowReasoningOnlyOutput: thinking,
			OnText: func(text string, isThinking bool) {
				if isThinking {
					if thinking {
						reasoning.WriteString(text)
					} else if text != "" {
						suppressedReasoningObserved = true
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
				if event.Text != "" || event.Signature != "" || event.RedactedContent != "" {
					suppressedReasoningObserved = true
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
				contextInputTokens, exceeded := observeKiroContextUsage(payload, model, percent)
				result.ContextInputTokens = contextInputTokens
				if exceeded {
					result.ContextWindowExceeded = true
				}
			},
		}
		attemptStarted := time.Now()
		err := h.callKiroWithBearerRefresh(account, payload, callback)
		err = resolveKiroOutputTruncation(payload, err, result.UpstreamStopReason, content.String(), completedKiroToolNames(result.ToolUses))
		if err == nil {
			visibleContent, extractedReasoning := extractThinkingFromContent(content.String())
			visibleContent, markerSeen := filterAgentCompletionContent(payload, visibleContent, len(result.ToolUses))
			if completionErr := agentCompletionResponseError(
				payload,
				suppressedReasoningObserved || reasoning.Len() > 0 || result.Reasoning.Signature != "" || len(result.Reasoning.Redacted) > 0,
				visibleContent,
				markerSeen,
				completedKiroToolNames(result.ToolUses),
				result.UpstreamStopReason,
			); completionErr != nil {
				err = completionErr
				prepareReasoningOnlyRetry(payload, err)
			}
			if err == nil {
				clientVisibleThinking := thinking &&
					(strings.TrimSpace(reasoning.String()) != "" || strings.TrimSpace(extractedReasoning) != "" ||
						result.Reasoning.Signature != "" || len(result.Reasoning.Redacted) > 0)
				if !claudeAttemptHasClientOutput(strings.TrimSpace(visibleContent) != "" || clientVisibleThinking, len(result.ToolUses)) {
					err = newUpstreamEmptyResponseError()
					logger.Warnf("[KiroAPI] empty buffered Claude output account_id=%s; retrying before accepting an empty turn", account.ID)
				}
			}
		}
		if err != nil {
			h.releaseAccount(account)
			discarded := estimateClaudeOutputTokensWithRedacted(
				content.String(), reasoning.String(), result.Reasoning.Redacted, result.ToolUses,
			)
			discarded = discardedAttemptOutputTokens(discarded, result.OutputTokens)
			if discarded > 0 || result.InputTokens > 0 || result.Credits > 0 {
				retryUsage.add(h.settleDiscardedAttempt(
					"claude", model, account.ID, "",
					clampClaudeInputTokensToModelWindow(model, discardedAttemptInputTokens(estimatedInputTokens, result.InputTokens)), discarded, result.Credits,
					time.Since(attemptStarted).Milliseconds(),
				))
			}
			if isDownstreamRequestCanceled(payload, err) {
				return nil, err
			}
			lastErr = err
			if takeKiroOutputContinuationRetry(payload, err) {
				retryUsage.markLastContinuationAttemptOutput()
				retrySameID = account.ID
				attemptLimit++
				continue
			}
			if isKiroInputTooLongError(err) {
				if h.retryAgentRecoveryAfterInputTooLong("buffered Claude", payload, account, &retrySameID, err) {
					attemptLimit++
					continue
				}
				return nil, err
			}
			if isUpstreamIncompleteResponseError(err) && agentCompletionRetryAllowed(payload, err) {
				retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
				if retryErr != nil {
					return nil, retryErr
				}
				if retried {
					attemptLimit++
					retryNumber, retryLimit := kiroPreResponseRetryProgress(payload, preResponseRetryCount, err)
					logger.Warnf("[KiroAPI] retrying buffered Claude call after incomplete agent response account_id=%s retry=%d/%d", account.ID, retryNumber, retryLimit)
					continue
				}
			}
			if isUpstreamEmptyResponseError(err) {
				retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
				if retryErr != nil {
					return nil, retryErr
				}
				if retried {
					attemptLimit++
					logger.Warnf("[KiroAPI] retrying buffered Claude call after empty upstream output account_id=%s retry=%d/%d", account.ID, preResponseRetryCount, maxKiroTransportPreResponseRetries)
					continue
				}
				return nil, err
			}
			if _, protocolErr := kiroStreamProtocolErrorCode(err); protocolErr {
				return nil, err
			}
			if isRateLimitError(err) {
				decision := h.applyRateLimitPolicy(account, retryCounts, excluded, err)
				if decision.Return429 {
					return nil, err
				}
				retrySameID = decision.RetrySameID
				if !h.waitBeforeRateLimitRetry(payload, decision) {
					return nil, payload.requestContext.Err()
				}
				continue
			}
			if isKiroClientRequestError(err) {
				return nil, err
			}
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		h.releaseAccount(account)

		resultContent, extractedReasoning := extractThinkingFromContent(content.String())
		if payload.StructuredOutput {
			resultContent, extractedReasoning = extractStructuredOutputContent(content.String())
		}
		result.Content = resultContent
		if thinking && result.Reasoning.Text == "" && extractedReasoning != "" {
			result.Reasoning.Text = extractedReasoning
		}
		result.Content, _ = filterAgentCompletionContent(payload, result.Content, len(result.ToolUses))
		result.Content = prependKiroOutputContinuation(payload, result.Content)
		if nativeReasoning := reasoning.String(); nativeReasoning != "" || !thinking {
			result.Reasoning.Text = nativeReasoning
		}
		result.ContextInputTokens = currentKiroContextUsageTokens(payload, model)
		// Upstream token counters can use the same Kiro context domain as
		// contextUsagePercentage. Anchor public and internal accounting to the
		// request estimate; raw occupancy remains in ContextInputTokens.
		result.InputTokens = max(1, clampClaudeInputTokensToModelWindow(model, estimatedInputTokens))
		result.OutputTokens = estimateClaudeOutputTokensWithRedacted(
			result.Content, result.Reasoning.Text, result.Reasoning.Redacted, result.ToolUses,
		)
		result.RetryUsage = retryUsage
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, result.InputTokens+retryUsage.finalAttemptOutputTokens(result.OutputTokens), result.Credits)
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
	if isKiroClientRequestError(err) {
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
