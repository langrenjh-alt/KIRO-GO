package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

const defaultResponsesModel = "claude-sonnet-4.5"

func (h *Handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
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
	apiKeyID := apiKeyIDFromContext(r.Context())
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
	respID := generateResponseID()
	selectedTools := filterOpenAIToolsForChoice(req.Tools, resolvedChoice)
	if webSearchTool, ok := findResponsesWebSearchTool(selectedTools); ok {
		h.handleResponsesWebSearch(
			w, r.Context(), &req, finalMessages, *webSearchTool, actualModel, requestedModel,
			estimatedInputTokens, apiKeyID, respID, storedInputCopy, storeResponse,
		)
		return
	}

	kiroPayload := OpenAIToKiro(openaiReq, thinking)

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
	excluded := make(map[string]bool)
	var lastErr error
	reqStart := time.Now()

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var content, reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamStopReason string
		var contextWindowExceeded bool

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					if thinking {
						reasoningContent += text
					} else {
						content += text
					}
				} else {
					content += text
				}
			},
			OnToolUse:    func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete:   func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			OnCredits:    func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
				if pct >= 100 {
					contextWindowExceeded = true
				}
			},
		}

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			if code, protocolErr := kiroStreamProtocolErrorCode(err); protocolErr {
				h.recordFailureWithDetails("responses", model, account.ID, err)
				h.sendOpenAIError(w, http.StatusBadGateway, code, err.Error())
				return
			}
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		finalContent, _ := extractThinkingFromContent(content)
		if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("responses", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		respObj := buildResponsesObjectWithReasoning(respID, responseModel, finalContent, reasoningContent, toolUses, inputTokens, outputTokens, req)
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
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}
	h.recordFailureWithDetails("responses", model, "", lastErr)
	h.sendOpenAIError(w, 500, "server_error", lastErr.Error())
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
	if resp == nil || (strings.TrimSpace(upstreamStopReason) == "" && !contextWindowExceeded) {
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
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	sequenceNumber := 0
	send := func(eventName string, payload interface{}) {
		if event, ok := payload.(map[string]interface{}); ok {
			event["sequence_number"] = sequenceNumber
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data))
		flusher.Flush()
		sequenceNumber++
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
	var lastErr error
	responseStarted := false
	reqStart := time.Now()

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		send("response.in_progress", map[string]interface{}{
			"type":     "response.in_progress",
			"response": initial,
		})

		var (
			fullText              strings.Builder
			messageText           strings.Builder
			reasoningText         strings.Builder
			fullReasoningText     strings.Builder
			toolUses              []KiroToolUse
			streamOutput          []ResponseOutputItem
			inputTokens           int
			outputTokens          int
			credits               float64
			realInputTokens       int
			upstreamStopReason    string
			contextWindowExceeded bool
		)

		messageItemID := ""
		messageStarted := false
		reasoningItemID := ""
		reasoningStarted := false
		outputIndex := 0
		contentIndex := 0
		outputItemStatus := "completed"
		var closeReasoning func()

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

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking && thinking {
					ensureReasoningStarted()
					reasoningText.WriteString(text)
					fullReasoningText.WriteString(text)
					send("response.reasoning_summary_text.delta", map[string]interface{}{
						"type":          "response.reasoning_summary_text.delta",
						"item_id":       reasoningItemID,
						"output_index":  outputIndex,
						"summary_index": 0,
						"delta":         text,
					})
					responseStarted = true
					return
				}
				fullText.WriteString(text)
				ensureMessageStarted()
				messageText.WriteString(text)
				send("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       messageItemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"delta":         text,
				})
				responseStarted = true
			},
			OnToolUse: func(tu KiroToolUse) {
				closeReasoning()
				closeMessage()

				toolUses = append(toolUses, tu)
				args, _ := json.Marshal(tu.Input)
				fcID := generateOutputItemID("fc")
				toolItem := ResponseOutputItem{
					ID:        fcID,
					Type:      "function_call",
					Status:    "completed",
					CallID:    tu.ToolUseID,
					Name:      tu.Name,
					Arguments: string(args),
				}
				send("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "in_progress",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": "",
					},
				})
				send("response.function_call_arguments.delta", map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"item_id":      fcID,
					"output_index": outputIndex,
					"delta":        string(args),
				})
				send("response.function_call_arguments.done", map[string]interface{}{
					"type":         "response.function_call_arguments.done",
					"item_id":      fcID,
					"output_index": outputIndex,
					"arguments":    string(args),
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item":         toolItem,
				})
				streamOutput = append(streamOutput, toolItem)
				outputIndex++
				responseStarted = true
			},
			OnComplete:   func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			OnCredits:    func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
				if pct >= 100 {
					contextWindowExceeded = true
				}
			},
		}

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
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
		if strings.TrimSpace(upstreamStopReason) != "" || contextWindowExceeded {
			outputItemStatus = "incomplete"
		}

		closeReasoning()
		closeMessage()
		if len(streamOutput) == 0 {
			ensureMessageStarted()
			closeMessage()
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoning, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("responses", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		respObj := buildResponsesObjectWithReasoning(respID, responseModel, finalContent, reasoning, toolUses, inputTokens, outputTokens, req)
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
	send("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"status": "failed",
			"error": map[string]string{
				"type":    "server_error",
				"message": lastErr.Error(),
			},
		},
	})
}
