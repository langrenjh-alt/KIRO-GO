package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

func responsesWebSearchClaudeTool(tool OpenAITool) ClaudeTool {
	return ClaudeTool{
		Type:           "web_search_20260209",
		Name:           webSearchToolName,
		AllowedDomains: append([]string(nil), tool.Filters.AllowedDomains...),
	}
}

func extractResponsesWebSearchQuery(messages []OpenAIMessage) (string, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			continue
		}
		if isPureWebSearchToolResultContent(messages[i].Content) {
			continue
		}
		query := normalizeWebSearchQuery(extractOpenAIMessageText(messages[i].Content))
		if query == "" {
			if isWebSearchContinuationContent(messages[i].Content) {
				continue
			}
			return "", errors.New("latest user message does not contain a web search query")
		}
		return query, nil
	}
	return "", errors.New("web search request does not contain a user message")
}

func (h *Handler) handleResponsesWebSearch(
	w http.ResponseWriter,
	ctx context.Context,
	req *ResponsesRequest,
	messages []OpenAIMessage,
	openAITool OpenAITool,
	model string,
	responseModel string,
	estimatedInputTokens int,
	apiKeyID string,
	respID string,
	storedInput json.RawMessage,
	storeResponse bool,
) {
	query, err := extractResponsesWebSearchQuery(messages)
	if err != nil {
		h.recordFailureWithDetails("responses", model, "", err)
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	tool := responsesWebSearchClaudeTool(openAITool)
	if err := validateWebSearchToolOptions(tool); err != nil {
		h.recordFailureWithDetails("responses", model, "", err)
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	upstreamQuery := buildWebSearchMCPQuery(query, tool)
	if utf8.RuneCountInString(upstreamQuery) > webSearchMaxUpstreamRunes {
		err := fmt.Errorf("web search query and domain filters exceed %d characters", webSearchMaxUpstreamRunes)
		h.recordFailureWithDetails("responses", model, "", err)
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if req.Stream {
		if _, ok := w.(http.Flusher); !ok {
			h.sendOpenAIError(w, http.StatusInternalServerError, "server_error", "Streaming not supported")
			return
		}
	}

	call := h.webSearchMCPCaller
	if call == nil {
		call = callMCPWebSearch
	}
	started := time.Now()
	execution, err := h.executeWebSearch(ctx, query, upstreamQuery, tool, call)
	if err != nil {
		accountID := ""
		if execution != nil {
			accountID = execution.LastAccountID
		}
		h.recordFailureWithDetails("responses", model, accountID, err)
		if isWebSearchContextError(ctx, err) {
			return
		}
		status, errorType, message, retryAfter := mapWebSearchError(err)
		if retryAfter = safeWebSearchRetryAfter(retryAfter); retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		h.sendOpenAIError(w, status, errorType, message)
		return
	}

	respObj, outputTokens := buildResponsesWebSearchObject(respID, responseModel, req, execution.Result, estimatedInputTokens)
	respObj.StoredInput = storedInput
	respObj.Instructions = req.Instructions
	respObj.OwnerKeyID = apiKeyID
	if storeResponse {
		if saveErr := saveResponse(respObj); saveErr != nil {
			logResponsesPersistFailure(respObj.ID, saveErr)
		}
	}

	if req.Stream {
		if err := writeResponsesWebSearchSSE(w, respObj); err != nil {
			h.pool.RecordSuccess(execution.Account.ID)
			h.recordFailureWithDetails("responses", model, execution.Account.ID, err)
			return
		}
		h.recordWebSearchSuccess("responses", model, apiKeyID, execution.Account, estimatedInputTokens, outputTokens, started)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(respObj); err != nil {
		h.pool.RecordSuccess(execution.Account.ID)
		h.recordFailureWithDetails("responses", model, execution.Account.ID, err)
		return
	}
	h.recordWebSearchSuccess("responses", model, apiKeyID, execution.Account, estimatedInputTokens, outputTokens, started)
}

func buildResponsesWebSearchObject(
	id string,
	model string,
	req *ResponsesRequest,
	result *webSearchResultSet,
	inputTokens int,
) (*ResponsesObject, int) {
	summary := generateWebSearchSummary(result)
	annotations := buildResponsesWebSearchAnnotations(summary, result.Results)
	outputTokens := estimateApproxTokens(summary)
	if raw, err := json.Marshal(annotations); err == nil {
		outputTokens += estimateApproxTokens(string(raw))
	}
	if outputTokens < 1 {
		outputTokens = 1
	}

	callItem := ResponseOutputItem{
		ID:     generateOutputItemID("ws"),
		Type:   "web_search_call",
		Status: "completed",
		Action: &ResponseWebSearchAction{Type: "search", Query: result.Query},
	}
	messageItem := ResponseOutputItem{
		ID:     generateOutputItemID("msg"),
		Type:   "message",
		Role:   "assistant",
		Status: "completed",
		Content: []ResponseContentPart{{
			Type:        "output_text",
			Text:        summary,
			Annotations: annotations,
		}},
	}

	return &ResponsesObject{
		ID:        id,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     model,
		Output:    []ResponseOutputItem{callItem, messageItem},
		Usage: ResponsesUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		},
		Reasoning:          req.responseReasoningConfig(),
		Text:               req.Text,
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}, outputTokens
}

func buildResponsesWebSearchAnnotations(summary string, results []webSearchResult) []ResponseAnnotation {
	annotations := make([]ResponseAnnotation, 0, len(results))
	searchFrom := 0
	for _, result := range results {
		if result.URL == "" || searchFrom > len(summary) {
			continue
		}
		relative := strings.Index(summary[searchFrom:], result.URL)
		if relative < 0 {
			continue
		}
		byteStart := searchFrom + relative
		byteEnd := byteStart + len(result.URL)
		annotations = append(annotations, ResponseAnnotation{
			Type:       "url_citation",
			StartIndex: utf8.RuneCountInString(summary[:byteStart]),
			EndIndex:   utf8.RuneCountInString(summary[:byteEnd]),
			URL:        result.URL,
			Title:      result.Title,
		})
		searchFrom = byteEnd
	}
	return annotations
}

func writeResponsesWebSearchSSE(w http.ResponseWriter, response *ResponsesObject) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming web search response is not supported by the response writer")
	}
	configureSSEHeaders(w)

	sequenceNumber := 0
	send := func(eventName string, payload map[string]interface{}) error {
		payload["sequence_number"] = sequenceNumber
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, data); err != nil {
			return err
		}
		flusher.Flush()
		sequenceNumber++
		return nil
	}

	initial := *response
	initial.Status = "in_progress"
	initial.Output = []ResponseOutputItem{}
	initial.Usage = ResponsesUsage{}
	if err := send("response.created", map[string]interface{}{"type": "response.created", "response": &initial}); err != nil {
		return err
	}
	if err := send("response.in_progress", map[string]interface{}{"type": "response.in_progress", "response": &initial}); err != nil {
		return err
	}

	callItem := response.Output[0]
	inProgressCall := callItem
	inProgressCall.Status = "in_progress"
	if err := send("response.output_item.added", map[string]interface{}{
		"type": "response.output_item.added", "output_index": 0, "item": inProgressCall,
	}); err != nil {
		return err
	}
	for _, eventName := range []string{
		"response.web_search_call.in_progress",
		"response.web_search_call.searching",
		"response.web_search_call.completed",
	} {
		if err := send(eventName, map[string]interface{}{
			"type": eventName, "item_id": callItem.ID, "output_index": 0,
		}); err != nil {
			return err
		}
	}
	if err := send("response.output_item.done", map[string]interface{}{
		"type": "response.output_item.done", "output_index": 0, "item": callItem,
	}); err != nil {
		return err
	}

	messageItem := response.Output[1]
	part := messageItem.Content[0]
	inProgressMessage := messageItem
	inProgressMessage.Status = "in_progress"
	inProgressMessage.Content = []ResponseContentPart{}
	if err := send("response.output_item.added", map[string]interface{}{
		"type": "response.output_item.added", "output_index": 1, "item": inProgressMessage,
	}); err != nil {
		return err
	}
	if err := send("response.content_part.added", map[string]interface{}{
		"type": "response.content_part.added", "item_id": messageItem.ID, "output_index": 1,
		"content_index": 0, "part": map[string]interface{}{
			"type": "output_text", "text": "", "annotations": []interface{}{},
		},
	}); err != nil {
		return err
	}
	for index, annotation := range part.Annotations {
		if err := send("response.output_text.annotation.added", map[string]interface{}{
			"type": "response.output_text.annotation.added", "item_id": messageItem.ID,
			"output_index": 1, "content_index": 0, "annotation_index": index, "annotation": annotation,
		}); err != nil {
			return err
		}
	}
	for _, chunk := range chunkWebSearchText(part.Text, webSearchSummaryChunkRunes) {
		if err := send("response.output_text.delta", map[string]interface{}{
			"type": "response.output_text.delta", "item_id": messageItem.ID,
			"output_index": 1, "content_index": 0, "delta": chunk,
		}); err != nil {
			return err
		}
	}
	if err := send("response.output_text.done", map[string]interface{}{
		"type": "response.output_text.done", "item_id": messageItem.ID,
		"output_index": 1, "content_index": 0, "text": part.Text,
	}); err != nil {
		return err
	}
	if err := send("response.content_part.done", map[string]interface{}{
		"type": "response.content_part.done", "item_id": messageItem.ID,
		"output_index": 1, "content_index": 0, "part": part,
	}); err != nil {
		return err
	}
	if err := send("response.output_item.done", map[string]interface{}{
		"type": "response.output_item.done", "output_index": 1, "item": messageItem,
	}); err != nil {
		return err
	}
	return send("response.completed", map[string]interface{}{"type": "response.completed", "response": response})
}
