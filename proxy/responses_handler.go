package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultResponsesModel = "claude-sonnet-4.5"

func (h *Handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	apiKeyID := apiKeyIDFromContext(r.Context())
	cachePolicy := config.ResolvePromptCachePolicyForAPIKey(apiKeyID)
	effectivePolicyID := cachePolicy.ID
	if effectivePolicyID == "" {
		effectivePolicyID = "global"
	}
	w.Header().Set("X-Kiro-Api-Key-Matched", strconv.FormatBool(apiKeyID != ""))
	w.Header().Set("X-Kiro-Prompt-Cache-Policy", effectivePolicyID)

	var req ResponsesRequest
	var requestBody []byte
	var err error
	if cachePolicy.CacheMode == config.CacheModeFunctionSimulation {
		requestBody, err = readRequestBodyWithLimit(w, r, claudeMaxRequestBodyBytes)
		if err == nil {
			err = json.Unmarshal(requestBody, &req)
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

	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultResponsesModel
	}
	if msg := validateResponsesTextConfig(req.Text); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}
	resolvedChoice, err := resolveToolChoice(req.ToolChoice)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	} else {
		names := make([]string, 0, len(req.Tools))
		for _, tool := range req.Tools {
			names = append(names, openAIToolName(tool))
		}
		if msg := validateResolvedToolChoice(resolvedChoice, names); msg != "" {
			h.sendOpenAIError(w, 400, "invalid_request_error", msg)
			return
		}
	}

	storedInputCopy := append(json.RawMessage(nil), req.Input...)

	storeResponse := true
	if req.Store != nil {
		storeResponse = *req.Store
	}

	var historyMessages []OpenAIMessage
	if req.PreviousResponseID != "" {
		prev, loadErr := loadResponseForOwner(req.PreviousResponseID, apiKeyID)
		if loadErr != nil {
			// Log the real reason server-side for ops, but return a single fixed
			// generic message to the client so a bad id never leaks the server's
			// filesystem path (the raw os.PathError) nor distinguishes missing vs
			// expired vs not-owner.
			logger.Warnf("[Responses] previous_response_id %q load failed: %v", req.PreviousResponseID, loadErr)
			h.sendOpenAIError(w, 404, "invalid_request_error", "previous_response_id not found")
			return
		}
		historyMessages = expandPreviousResponseHistory(prev)
	}

	inputMessages, err := parseResponsesInput(req.Input)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}

	finalMessages := make([]OpenAIMessage, 0, len(historyMessages)+len(inputMessages)+1)
	finalMessages = append(finalMessages, historyMessages...)
	if strings.TrimSpace(req.Instructions) != "" {
		// New instructions on this turn always take effect, even when
		// continuing from previous_response_id. Place them after the
		// expanded history so they apply to the current and future turns,
		// while ancestor instructions (re-emitted by expandPreviousResponseHistory)
		// stay in scope for the historical exchanges they shaped.
		finalMessages = append(finalMessages, OpenAIMessage{
			Role:    "system",
			Content: req.Instructions,
		})
	}
	finalMessages = append(finalMessages, inputMessages...)

	if len(finalMessages) == 0 {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one message")
		return
	}

	hasUser := false
	for _, m := range finalMessages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one user message")
		return
	}

	openaiReq := &OpenAIRequest{
		Model:           req.Model,
		Messages:        finalMessages,
		Stream:          req.Stream,
		Tools:           req.Tools,
		ToolChoice:      req.ToolChoice,
		ReasoningEffort: req.effectiveReasoningEffort(),
		ResponseFormat:  responsesStructuredOutputFormat(req.Text),
	}
	if req.Temperature != nil {
		openaiReq.Temperature = *req.Temperature
	}
	if req.MaxOutputTokens != nil {
		openaiReq.MaxTokens = *req.MaxOutputTokens
	}

	thinkingCfg := config.GetThinkingConfig()
	requestedModel := req.Model
	actualModel, thinking := resolveOpenAIThinkingMode(req.Model, openaiReq.ReasoningEffort, thinkingCfg.Suffix)
	openaiReq.Model = actualModel

	estimatedInputTokens := estimateOpenAIRequestInputTokens(openaiReq)
	h.prepareResponsesPromptCache(&req, openaiReq, r.Context(), thinking, apiKeyID, extractProvidedKey(r), estimatedInputTokens, cachePolicy, requestBody)
	respID := generateResponseID()
	selectedTools := filterOpenAIToolsForChoice(req.Tools, resolvedChoice)
	if webSearchTool, ok := findResponsesWebSearchTool(selectedTools); ok {
		h.handleResponsesWebSearch(
			w, r.Context(), &req, finalMessages, *webSearchTool, actualModel, requestedModel,
			estimatedInputTokens, apiKeyID, respID, storedInputCopy, storeResponse,
		)
		return
	}

	kiroPayload := bindKiroPayloadContext(OpenAIToKiro(openaiReq, thinking), r.Context())
	kiroPayload.requestID = respID
	if _, err := preflightKiroPayload(kiroPayload); err != nil {
		h.recordFailureWithDetails("responses", actualModel, "", err)
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if req.Stream {
		h.handleResponsesStream(w, kiroPayload, actualModel, requestedModel, thinking, estimatedInputTokens,
			apiKeyID, respID, &req, storedInputCopy, storeResponse)
		return
	}

	h.handleResponsesNonStream(w, kiroPayload, actualModel, requestedModel, thinking, estimatedInputTokens,
		apiKeyID, respID, &req, storedInputCopy, storeResponse)
}

func (h *Handler) handleResponsesNonStream(
	w http.ResponseWriter, payload *KiroPayload, model, responseModel string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	payload = cloneKiroPayload(payload)
	excluded := make(map[string]bool)
	retryCounts := make(map[string]int)
	retrySameID := ""
	var lastErr error
	reqStart := time.Now()
	preResponseRetryCount := 0
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
		req.promptCache.compute(account.ID)

		var content, reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var upstreamStopReason string
		var contextWindowExceeded bool
		var suppressedReasoningObserved bool

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					if !thinking {
						suppressedReasoningObserved = true
						return
					}
					reasoningContent += text
					return
				}
				content += text
			},
			OnReasoning: func(event KiroReasoningContentEvent) {
				if !thinking {
					if event.Text != "" || event.Signature != "" || event.RedactedContent != "" {
						suppressedReasoningObserved = true
					}
					return
				}
				reasoningContent += event.Text
			},
			OnToolUse:    func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
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
		err := call(account, payload, callback)
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
			discarded := estimateOpenAIOutputTokens(content, reasoningContent, toolUses)
			discarded = discardedAttemptOutputTokens(discarded, outputTokens)
			if discarded > 0 || inputTokens > 0 || credits > 0 {
				retryUsage.add(h.settleDiscardedAttempt(
					"responses", model, account.ID, apiKeyID,
					clampClaudeInputTokensToModelWindow(model, discardedAttemptInputTokens(estimatedInputTokens, inputTokens)), discarded, credits,
					time.Since(reqStart).Milliseconds(),
				))
			}
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			lastErr = err
			if isKiroInputTooLongError(err) {
				if h.retryAgentRecoveryAfterInputTooLong("Responses non-stream", payload, account, &retrySameID, err) {
					attemptLimit++
					continue
				}
				h.recordFailureWithDetails("responses", model, account.ID, err)
				h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			if payload.agentActionContinuationTimeout && isRetryableKiroStreamError(err) && !isRateLimitError(err) {
				retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
				if retryErr != nil {
					return
				}
				if retried {
					attemptLimit++
					retryNumber, retryLimit := kiroPreResponseRetryProgress(payload, preResponseRetryCount, err)
					logger.Warnf("[KiroAPI] retrying Responses non-stream after transient continuation error account_id=%s retry=%d/%d: %v", account.ID, retryNumber, retryLimit, err)
					continue
				}
			}
			if isUpstreamIncompleteResponseError(err) && agentCompletionRetryAllowed(payload, err) {
				retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
				if retryErr != nil {
					return
				}
				if retried {
					attemptLimit++
					retryNumber, retryLimit := kiroPreResponseRetryProgress(payload, preResponseRetryCount, err)
					logger.Warnf("[KiroAPI] retrying Responses non-stream after incomplete agent response account_id=%s retry=%d/%d", account.ID, retryNumber, retryLimit)
					continue
				}
			}
			if code, protocolErr := kiroStreamProtocolErrorCode(err); protocolErr {
				h.recordFailureWithDetails("responses", model, account.ID, err)
				h.sendOpenAIError(w, http.StatusBadGateway, code, err.Error())
				return
			}
			if isRateLimitError(err) {
				decision := h.applyRateLimitPolicy(account, retryCounts, excluded, err)
				if decision.Return429 {
					h.recordFailureWithDetails("responses", model, account.ID, err)
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
				h.recordFailureWithDetails("responses", model, account.ID, err)
				h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		finalContent, _ := extractThinkingFromContent(content)
		finalContent, _ = filterAgentCompletionContent(payload, finalContent, len(toolUses))
		if !thinking {
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

		cacheUsage := req.promptCache.complete(account.ID, inputTokens)
		h.recordSuccessForApiKeyWithCache(apiKeyID, inputTokens, outputTokens, credits, cacheUsage)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("responses", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		publicInputTokens := contextUsageBlendedInputTokens(payload, model, inputTokens)
		publicCacheUsage := contextUsageBlendedCacheUsage(payload, model, inputTokens, cacheUsage)
		publicInputTokens, publicCacheUsage = retryUsage.apply(publicInputTokens, publicCacheUsage)
		respObj := buildResponsesObjectWithReasoning(respID, responseModel, finalContent, reasoningContent, toolUses, publicInputTokens, outputTokens, req)
		applyResponsesPromptCacheUsage(respObj, publicCacheUsage)
		respObj.Usage.KiroContextUsage = kiroContextUsage(payload, model)
		applyResponsesTerminalStatus(respObj, upstreamStopReason, contextWindowExceeded)
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions
		respObj.OwnerKeyID = apiKeyID

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	if lastErr == nil {
		h.recordFailureWithDetails("responses", model, "", fmt.Errorf("No available accounts"))
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", "No available accounts")
		return
	}
	h.recordFailureWithDetails("responses", model, "", lastErr)
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

func buildResponsesObject(
	id, model, content string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest,
) *ResponsesObject {
	return buildResponsesObjectWithReasoning(id, model, content, "", toolUses, inputTokens, outputTokens, req)
}

func buildResponsesObjectWithReasoning(
	id, model, content, reasoning string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest,
) *ResponsesObject {
	output := make([]ResponseOutputItem, 0, 2+len(toolUses))

	if strings.TrimSpace(reasoning) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("rs"),
			Type:   "reasoning",
			Status: "completed",
			Summary: []ResponseSummaryPart{{
				Type: "summary_text",
				Text: reasoning,
			}},
		})
	}

	if strings.TrimSpace(content) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: content,
			}},
		})
	}

	for _, tu := range toolUses {
		args, _ := json.Marshal(tu.Input)
		output = append(output, ResponseOutputItem{
			ID:        generateOutputItemID("fc"),
			Type:      "function_call",
			Status:    "completed",
			CallID:    tu.ToolUseID,
			Name:      tu.Name,
			Arguments: string(args),
		})
	}

	if len(output) == 0 {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "",
			}},
		})
	}

	return &ResponsesObject{
		ID:        id,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     model,
		Output:    output,
		Usage: ResponsesUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			OutputTokensDetails: ResponsesOutputTokensDetails{
				ReasoningTokens: estimateApproxTokens(reasoning),
			},
			TotalTokens: inputTokens + outputTokens,
		},
		Reasoning:          req.responseReasoningConfig(),
		Text:               req.Text,
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
}

func applyResponsesTerminalStatus(resp *ResponsesObject, upstreamStopReason string, contextWindowExceeded bool) bool {
	if resp == nil || (!isKiroLengthStopReason(upstreamStopReason) && !contextWindowExceeded) {
		return false
	}
	resp.Status = "incomplete"
	resp.IncompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
	markLastResponsesOutputIncomplete(resp.Output)
	return true
}

func markLastResponsesOutputIncomplete(output []ResponseOutputItem) {
	for index := len(output) - 1; index >= 0; index-- {
		if output[index].Type == "message" || output[index].Type == "reasoning" {
			output[index].Status = "incomplete"
			return
		}
	}
}

func (h *Handler) handleResponsesStream(
	w http.ResponseWriter, payload *KiroPayload, model, responseModel string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	payload = cloneKiroPayload(payload)
	beginExplicitVisibleResponseTracking(w)
	configureSSEHeaders(w)

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	sequenceNumber := 0
	sendEvent := func(eventName string, payload interface{}, visible bool) {
		if event, ok := payload.(map[string]interface{}); ok {
			event["sequence_number"] = sequenceNumber
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data)); err != nil {
			return
		}
		if visible {
			markVisibleResponseWritten(w)
		}
		flusher.Flush()
		sequenceNumber++
	}
	send := func(eventName string, payload interface{}) {
		sendEvent(eventName, payload, false)
	}
	sendVisible := func(eventName string, payload interface{}) {
		sendEvent(eventName, payload, true)
	}

	createdAt := time.Now().Unix()
	initial := &ResponsesObject{
		ID:                 respID,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              responseModel,
		Output:             []ResponseOutputItem{},
		Usage:              ResponsesUsage{},
		Reasoning:          req.responseReasoningConfig(),
		Text:               req.Text,
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
	send("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": initial,
	})

	excluded := make(map[string]bool)
	retryCounts := make(map[string]int)
	retrySameID := ""
	var lastErr error
	responseStarted := false
	responseInProgressSent := false
	reqStart := time.Now()
	preResponseRetryCount := 0
	var streamOutput []ResponseOutputItem
	var fullReasoningText strings.Builder
	outputIndex := 0
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
		req.promptCache.compute(account.ID)

		if !responseInProgressSent {
			send("response.in_progress", map[string]interface{}{
				"type":     "response.in_progress",
				"response": initial,
			})
			responseInProgressSent = true
		}

		var (
			fullText                    strings.Builder
			rawAssistantText            strings.Builder
			messageText                 strings.Builder
			reasoningText               strings.Builder
			textBuffer                  string
			inThinkingBlock             bool
			dropTagThinking             bool
			thinkingSource              thinkingStreamSource
			reasoningPhaseSealed        bool
			toolUses                    []KiroToolUse
			inputTokens                 int
			outputTokens                int
			credits                     float64
			upstreamStopReason          string
			contextWindowExceeded       bool
			suppressedReasoningObserved bool
		)
		completionGate := newAgentCompletionGateForPayload(payload)
		suppressRecoveryOutput := payload.AgentCompletionRetries > 0

		messageItemID := ""
		messageStarted := false
		reasoningItemID := ""
		reasoningStarted := false
		contentIndex := 0
		outputItemStatus := "completed"
		var closeReasoning func()
		type responseToolStream struct {
			itemID      string
			outputIndex int
		}
		toolStreams := make(map[string]*responseToolStream)
		ignoredToolUses := make(map[string]bool)

		closeMessage := func() {
			if !messageStarted {
				return
			}
			text := messageText.String()
			item := ResponseOutputItem{
				ID:     messageItemID,
				Type:   "message",
				Role:   "assistant",
				Status: outputItemStatus,
				Content: []ResponseContentPart{{
					Type: "output_text",
					Text: text,
				}},
			}
			send("response.output_text.done", map[string]interface{}{
				"type":          "response.output_text.done",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"text":          text,
			})
			send("response.content_part.done", map[string]interface{}{
				"type":          "response.content_part.done",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part":          item.Content[0],
			})
			send("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item":         item,
			})
			streamOutput = append(streamOutput, item)
			outputIndex++
			messageStarted = false
			messageItemID = ""
			messageText.Reset()
		}

		ensureMessageStarted := func() {
			if messageStarted {
				return
			}
			if closeReasoning != nil {
				closeReasoning()
			}
			messageItemID = generateOutputItemID("msg")
			messageStarted = true
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      messageItemID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []map[string]interface{}{},
				},
			})
			send("response.content_part.added", map[string]interface{}{
				"type":          "response.content_part.added",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": "",
				},
			})
		}

		ensureReasoningStarted := func() {
			if reasoningStarted {
				return
			}
			closeMessage()
			reasoningItemID = generateOutputItemID("rs")
			reasoningStarted = true
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      reasoningItemID,
					"type":    "reasoning",
					"status":  "in_progress",
					"summary": []interface{}{},
				},
			})
			send("response.reasoning_summary_part.added", map[string]interface{}{
				"type":          "response.reasoning_summary_part.added",
				"item_id":       reasoningItemID,
				"output_index":  outputIndex,
				"summary_index": 0,
				"part": map[string]interface{}{
					"type": "summary_text",
					"text": "",
				},
			})
		}

		closeReasoning = func() {
			if !reasoningStarted {
				return
			}
			text := reasoningText.String()
			item := ResponseOutputItem{
				ID:     reasoningItemID,
				Type:   "reasoning",
				Status: outputItemStatus,
				Summary: []ResponseSummaryPart{{
					Type: "summary_text",
					Text: text,
				}},
			}
			send("response.reasoning_summary_text.done", map[string]interface{}{
				"type":          "response.reasoning_summary_text.done",
				"item_id":       reasoningItemID,
				"output_index":  outputIndex,
				"summary_index": 0,
				"text":          text,
			})
			send("response.reasoning_summary_part.done", map[string]interface{}{
				"type":          "response.reasoning_summary_part.done",
				"item_id":       reasoningItemID,
				"output_index":  outputIndex,
				"summary_index": 0,
				"part":          item.Summary[0],
			})
			send("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item":         item,
			})
			streamOutput = append(streamOutput, item)
			outputIndex++
			reasoningStarted = false
			reasoningItemID = ""
			reasoningText.Reset()
		}

		emitReasoningDelta := func(text string) {
			if text == "" {
				return
			}
			reasoningText.WriteString(text)
			fullReasoningText.WriteString(text)
			if suppressRecoveryOutput {
				return
			}
			ensureReasoningStarted()
			sendVisible("response.reasoning_summary_text.delta", map[string]interface{}{
				"type":          "response.reasoning_summary_text.delta",
				"item_id":       reasoningItemID,
				"output_index":  outputIndex,
				"summary_index": 0,
				"delta":         text,
			})
			responseStarted = true
		}

		emitOutputText := func(text string) {
			if text == "" {
				return
			}
			reasoningPhaseSealed = true
			fullText.WriteString(text)
			ensureMessageStarted()
			messageText.WriteString(text)
			sendVisible("response.output_text.delta", map[string]interface{}{
				"type":          "response.output_text.delta",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"delta":         text,
			})
			responseStarted = true
		}

		processAssistantText := func(text string, forceFlush bool) {
			textBuffer += text
			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, thinkingOpenTag)
					if thinkingStart != -1 {
						emitOutputText(textBuffer[:thinkingStart])
						textBuffer = textBuffer[thinkingStart+len(thinkingOpenTag):]
						inThinkingBlock = true
						dropTagThinking = !thinking || reasoningPhaseSealed || !allowTagSource(&thinkingSource)
					} else if forceFlush {
						emitOutputText(textBuffer)
						textBuffer = ""
						break
					} else {
						ready, pending := splitBeforePartialTag(textBuffer, thinkingOpenTag)
						emitOutputText(ready)
						textBuffer = pending
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, thinkingCloseTag)
					if thinkingEnd != -1 {
						if !dropTagThinking {
							emitReasoningDelta(textBuffer[:thinkingEnd])
						}
						textBuffer = textBuffer[thinkingEnd+len(thinkingCloseTag):]
						inThinkingBlock = false
						dropTagThinking = false
					} else if forceFlush {
						if !dropTagThinking {
							emitReasoningDelta(textBuffer)
						}
						textBuffer = ""
						inThinkingBlock = false
						dropTagThinking = false
						break
					} else {
						ready, pending := splitBeforePartialTag(textBuffer, thinkingCloseTag)
						if !dropTagThinking {
							emitReasoningDelta(ready)
						}
						textBuffer = pending
						break
					}
				}
			}
		}

		handleNativeReasoning := func(text string) {
			if text == "" {
				return
			}
			if !thinking || reasoningPhaseSealed {
				suppressedReasoningObserved = true
				return
			}
			if !allowReasoningSource(&thinkingSource) {
				return
			}
			emitReasoningDelta(text)
		}

		beginToolUse := func(toolUseID, name string) {
			if _, exists := toolStreams[toolUseID]; exists || ignoredToolUses[toolUseID] {
				return
			}
			if forcedToolChoiceMode(payload) && !kiroPayloadAllowsToolName(payload, name) {
				ignoredToolUses[toolUseID] = true
				return
			}
			processAssistantText("", true)
			narration := completionGate.CommitPendingForTool()
			if narration != "" {
				processAssistantText(narration, true)
			}
			reasoningPhaseSealed = true
			closeReasoning()
			closeMessage()
			stream := &responseToolStream{
				itemID:      generateOutputItemID("fc"),
				outputIndex: outputIndex,
			}
			toolStreams[toolUseID] = stream
			outputIndex++
			sendVisible("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": stream.outputIndex,
				"item": map[string]interface{}{
					"id":        stream.itemID,
					"type":      "function_call",
					"status":    "in_progress",
					"call_id":   toolUseID,
					"name":      name,
					"arguments": "",
				},
			})
			responseStarted = true
		}
		emitToolUseInputDelta := func(toolUseID, delta string) {
			if delta == "" || ignoredToolUses[toolUseID] {
				return
			}
			stream := toolStreams[toolUseID]
			if stream == nil {
				return
			}
			sendVisible("response.function_call_arguments.delta", map[string]interface{}{
				"type":         "response.function_call_arguments.delta",
				"item_id":      stream.itemID,
				"output_index": stream.outputIndex,
				"delta":        delta,
			})
		}

		callback := &KiroStreamCallback{
			OnKeepAlive: func() {
				writeSSEKeepAlive(w, flusher)
			},
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					handleNativeReasoning(text)
					return
				}
				rawAssistantText.WriteString(text)
				text = completionGate.Push(text)
				if text == "" {
					return
				}
				if suppressRecoveryOutput && !completionGate.Marked() {
					return
				}
				processAssistantText(text, false)
			},
			OnReasoning: func(event KiroReasoningContentEvent) {
				handleNativeReasoning(event.Text)
			},
			OnToolUseStart:      beginToolUse,
			OnToolUseInputDelta: emitToolUseInputDelta,
			OnToolUse: func(tu KiroToolUse) {
				if ignoredToolUses[tu.ToolUseID] ||
					(forcedToolChoiceMode(payload) && !kiroPayloadAllowsToolName(payload, tu.Name)) {
					ignoredToolUses[tu.ToolUseID] = true
					return
				}
				stream := toolStreams[tu.ToolUseID]
				if stream == nil {
					beginToolUse(tu.ToolUseID, tu.Name)
					stream = toolStreams[tu.ToolUseID]
					args, _ := json.Marshal(tu.Input)
					emitToolUseInputDelta(tu.ToolUseID, string(args))
				}
				toolUses = append(toolUses, tu)
				args, _ := json.Marshal(tu.Input)
				toolItem := ResponseOutputItem{
					ID:        stream.itemID,
					Type:      "function_call",
					Status:    "completed",
					CallID:    tu.ToolUseID,
					Name:      tu.Name,
					Arguments: string(args),
				}
				send("response.function_call_arguments.done", map[string]interface{}{
					"type":         "response.function_call_arguments.done",
					"item_id":      stream.itemID,
					"output_index": stream.outputIndex,
					"arguments":    string(args),
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": stream.outputIndex,
					"item":         toolItem,
				})
				streamOutput = append(streamOutput, toolItem)
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
			OnCredits: func(c float64) { credits = c },
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
		if err == nil {
			processAssistantText("", true)
			if completionErr := agentCompletionResponseError(
				payload,
				suppressedReasoningObserved || fullReasoningText.Len() > 0,
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
			if err == nil {
				if terminalText := completionGate.CommitPendingForTerminal(); terminalText != "" {
					processAssistantText(terminalText, true)
				}
			}
		}
		if shouldFinishStreamAfterAgentContinuation(payload, err, len(toolUses)) {
			logger.Warnf("[KiroAPI] finishing Responses stream after bounded agent continuation request_id=%s account_id=%s phase=%s completions=%d actions=%d full_context=%d: %v",
				respID, account.ID, kiroAgentRecoveryPhase(payload, err), payload.AgentCompletionRetries,
				payload.AgentActionContinuations, payload.AgentFullContextRecoveries, err)
			err = nil
			upstreamStopReason = "end_turn"
		}
		h.releaseAccount(account)
		if err != nil {
			discarded := estimateOpenAIOutputTokens(fullText.String(), fullReasoningText.String(), toolUses)
			discarded = discardedAttemptOutputTokens(discarded, outputTokens)
			if discarded > 0 || inputTokens > 0 || credits > 0 {
				retryUsage.add(h.settleDiscardedAttempt(
					"responses", model, account.ID, apiKeyID,
					clampClaudeInputTokensToModelWindow(model, discardedAttemptInputTokens(estimatedInputTokens, inputTokens)), discarded, credits,
					time.Since(reqStart).Milliseconds(),
				))
			}
			if isUpstreamIncompleteResponseError(err) {
				closeReasoning()
				closeMessage()
			}
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			if isKiroInputTooLongError(err) {
				if h.retryAgentRecoveryAfterInputTooLong("Responses stream", payload, account, &retrySameID, err) {
					attemptLimit++
					continue
				}
				send("response.failed", map[string]interface{}{
					"type": "response.failed",
					"response": map[string]interface{}{
						"id":     respID,
						"status": "failed",
						"error":  map[string]string{"type": "invalid_request_error", "message": err.Error()},
					},
				})
				h.recordFailureWithDetails("responses", model, account.ID, err)
				return
			}
			if payload.agentActionContinuationTimeout && isRetryableKiroStreamError(err) && !isRateLimitError(err) {
				retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
				if retryErr != nil {
					return
				}
				if retried {
					attemptLimit++
					retryNumber, retryLimit := kiroPreResponseRetryProgress(payload, preResponseRetryCount, err)
					logger.Warnf("[KiroAPI] retrying Responses stream after transient continuation error request_id=%s account_id=%s phase=%s retry=%d/%d completions=%d actions=%d full_context=%d: %v",
						respID, account.ID, kiroAgentRecoveryPhase(payload, err), retryNumber, retryLimit,
						payload.AgentCompletionRetries, payload.AgentActionContinuations, payload.AgentFullContextRecoveries, err)
					continue
				}
			}
			if isUpstreamIncompleteResponseError(err) && agentCompletionRetryAllowed(payload, err) {
				retried, retryErr := h.retryKiroPreResponse(payload, account, &preResponseRetryCount, excluded, &retrySameID, err)
				if retryErr != nil {
					return
				}
				if retried {
					attemptLimit++
					retryNumber, retryLimit := kiroPreResponseRetryProgress(payload, preResponseRetryCount, err)
					logger.Warnf("[KiroAPI] retrying Responses stream after incomplete agent response request_id=%s account_id=%s phase=%s retry=%d/%d completions=%d actions=%d full_context=%d",
						respID, account.ID, kiroAgentRecoveryPhase(payload, err), retryNumber, retryLimit,
						payload.AgentCompletionRetries, payload.AgentActionContinuations, payload.AgentFullContextRecoveries)
					continue
				}
			}
			if code, protocolErr := kiroStreamProtocolErrorCode(err); protocolErr {
				send("response.failed", map[string]interface{}{
					"type": "response.failed",
					"response": map[string]interface{}{
						"id":     respID,
						"status": "failed",
						"error":  map[string]string{"type": code, "message": err.Error()},
					},
				})
				h.recordFailureWithDetails("responses", model, account.ID, err)
				return
			}
			if isRateLimitError(err) {
				lastErr = err
				decision := h.applyRateLimitPolicy(account, retryCounts, excluded, err)
				if decision.Return429 {
					send("response.failed", map[string]interface{}{
						"type": "response.failed",
						"response": map[string]interface{}{
							"id":     respID,
							"status": "failed",
							"error":  map[string]string{"type": "rate_limit_error", "message": err.Error()},
						},
					})
					h.recordFailureWithDetails("responses", model, account.ID, err)
					return
				}
				retrySameID = decision.RetrySameID
				if !responseStarted {
					if !h.waitBeforeRateLimitRetry(payload, decision) {
						return
					}
					continue
				}
				send("response.failed", map[string]interface{}{
					"type": "response.failed",
					"response": map[string]interface{}{
						"id":     respID,
						"status": "failed",
						"error":  map[string]string{"type": "rate_limit_error", "message": err.Error()},
					},
				})
				h.recordFailureWithDetails("responses", model, account.ID, err)
				return
			}
			if isKiroClientRequestError(err) {
				send("response.failed", map[string]interface{}{
					"type": "response.failed",
					"response": map[string]interface{}{
						"id":     respID,
						"status": "failed",
						"error":  map[string]string{"type": "invalid_request_error", "message": err.Error()},
					},
				})
				h.recordFailureWithDetails("responses", model, account.ID, err)
				return
			}
			if !responseStarted {
				lastErr = err
				excluded[account.ID] = true
				h.handleAccountFailure(account, err)
				continue
			}
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    "server_error",
						"message": err.Error(),
					},
				},
			})
			h.recordFailureWithDetails("responses", model, account.ID, err)
			return
		}

		finalContent, _ := extractThinkingFromContent(fullText.String())
		reasoning := fullReasoningText.String()
		if !thinking {
			reasoning = ""
		}
		if isKiroLengthStopReason(upstreamStopReason) || contextWindowExceeded {
			outputItemStatus = "incomplete"
		}

		closeReasoning()
		closeMessage()
		if len(streamOutput) == 0 {
			ensureMessageStarted()
			closeMessage()
		}

		if _, exceeded := currentKiroContextUsage(payload, model); exceeded {
			contextWindowExceeded = true
		}
		inputTokens = clampClaudeInputTokensToModelWindow(model, estimatedInputTokens)
		if inputTokens <= 0 {
			inputTokens = 1
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoning, toolUses)

		cacheUsage := req.promptCache.complete(account.ID, inputTokens)
		h.recordSuccessForApiKeyWithCache(apiKeyID, inputTokens, outputTokens, credits, cacheUsage)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("responses", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		publicInputTokens := contextUsageBlendedInputTokens(payload, model, inputTokens)
		publicCacheUsage := contextUsageBlendedCacheUsage(payload, model, inputTokens, cacheUsage)
		publicInputTokens, publicCacheUsage = retryUsage.apply(publicInputTokens, publicCacheUsage)
		respObj := buildResponsesObjectWithReasoning(respID, responseModel, finalContent, reasoning, toolUses, publicInputTokens, outputTokens, req)
		applyResponsesPromptCacheUsage(respObj, publicCacheUsage)
		respObj.Usage.KiroContextUsage = kiroContextUsage(payload, model)
		respObj.Output = streamOutput
		incomplete := applyResponsesTerminalStatus(respObj, upstreamStopReason, contextWindowExceeded)
		respObj.CreatedAt = createdAt
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions
		respObj.OwnerKeyID = apiKeyID

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		terminalEvent := "response.completed"
		if incomplete {
			terminalEvent = "response.incomplete"
		}
		send(terminalEvent, map[string]interface{}{
			"type":     terminalEvent,
			"response": respObj,
		})
		return
	}

	if lastErr == nil {
		h.recordFailureWithDetails("responses", model, "", fmt.Errorf("No available accounts"))
		send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error": map[string]string{
					"type":    "server_error",
					"message": "No available accounts",
				},
			},
		})
		return
	}
	h.recordFailureWithDetails("responses", model, "", lastErr)
	errType := "server_error"
	errMessage := lastErr.Error()
	if isAccountRefreshInProgressError(lastErr) {
		errType = "account_refreshing"
		errMessage = accountRefreshInProgressMessage
	} else if isAllAccountsConcurrencyFullError(lastErr) {
		errMessage = allAccountsConcurrencyFullMessage
	} else if isRateLimitError(lastErr) {
		errType = "rate_limit_error"
	}
	send("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"status": "failed",
			"error": map[string]string{
				"type":    errType,
				"message": errMessage,
			},
		},
	})
}
