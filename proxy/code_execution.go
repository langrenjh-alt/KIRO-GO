package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	codeExecutionToolType       = "code_execution_20250522"
	codeExecutionToolTypeLatest = "code_execution_20250825"
	codeExecutionToolName       = "code_execution"
)

const codeExecutionUnavailableText = "The code execution service is currently unavailable, so the requested code was not run."

func isNativeCodeExecutionTool(tool ClaudeTool) bool {
	toolType := strings.TrimSpace(tool.Type)
	return (toolType == codeExecutionToolType || toolType == codeExecutionToolTypeLatest) &&
		strings.TrimSpace(tool.Name) == codeExecutionToolName
}

func findNativeCodeExecutionTool(req *ClaudeRequest) (*ClaudeTool, bool) {
	if req == nil {
		return nil, false
	}
	for i := range req.Tools {
		if isNativeCodeExecutionTool(req.Tools[i]) {
			return &req.Tools[i], true
		}
	}
	return nil, false
}

func codeExecutionGeneratorRequest(req *ClaudeRequest) *ClaudeRequest {
	generated := *req
	generated.Stream = false
	generated.Thinking = nil
	generated.OutputConfig = nil
	generated.StopSequences = nil
	generated.Tools = []ClaudeTool{{
		Type:        "custom",
		Name:        codeExecutionToolName,
		Description: "Submit Python code needed to answer the user. Do not explain the code before calling this tool.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"code": map[string]interface{}{"type": "string"},
			},
			"required": []string{"code"},
		},
	}}
	generated.ToolChoice = map[string]interface{}{"type": "tool", "name": codeExecutionToolName}
	return &generated
}

func codeExecutionBlocks(toolUse KiroToolUse) []ClaudeContentBlock {
	toolUseID := newClaudeServerToolUseID()
	input := map[string]interface{}{}
	for key, value := range toolUse.Input {
		input[key] = value
	}
	return []ClaudeContentBlock{
		{Type: "server_tool_use", ID: toolUseID, Name: codeExecutionToolName, Input: input},
		{
			Type:      "code_execution_tool_result",
			ToolUseID: toolUseID,
			Content: map[string]interface{}{
				"type":       "code_execution_tool_result_error",
				"error_code": "too_many_requests",
			},
		},
		{Type: "text", Text: codeExecutionUnavailableText},
	}
}

func newCodeExecutionContainer() *ClaudeContainer {
	return &ClaudeContainer{
		ID:        newClaudeContainerID(),
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano),
	}
}

func (h *Handler) handleCodeExecutionRequest(
	w http.ResponseWriter,
	ctx context.Context,
	req *ClaudeRequest,
	responseModel string,
	estimatedInputTokens int,
	apiKeyID string,
	cacheProfile *promptCacheProfile,
) {
	if req == nil {
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", "Code execution request is missing")
		return
	}
	started := time.Now()
	generatorReq := codeExecutionGeneratorRequest(req)
	payload := bindKiroPayloadContext(ClaudeToKiro(generatorReq, false), ctx)
	result, err := h.callClaudeBuffered(req.Model, payload, false, estimateClaudeRequestInputTokens(generatorReq))
	if err != nil {
		if isDownstreamRequestCanceled(payload, err) {
			return
		}
		h.handleAdvisorFailure(w, req.Model, err)
		return
	}
	if len(result.ToolUses) == 0 || result.ToolUses[0].Name != codeExecutionToolName {
		err := errors.New("code execution model did not produce a code tool call")
		h.recordFailureWithDetails("claude", req.Model, result.Account.ID, err)
		h.sendClaudeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	blocks := codeExecutionBlocks(result.ToolUses[0])
	container := newCodeExecutionContainer()
	inputTokens := max(1, estimatedInputTokens)
	outputTokens := estimateClaudeOutputTokensWithRedacted(codeExecutionUnavailableText, "", nil, nil)
	cacheTracker := promptCacheTrackerForProfile(h.promptCache, cacheProfile)
	cacheUsage := promptCacheUsage{}
	if result.Account != nil {
		cacheUsage = cacheTracker.Compute(result.Account.ID, cacheProfile)
		cacheUsage = reanchorClaudeCacheUsage(cacheProfile, cacheUsage, inputTokens)
		cacheTracker.Update(result.Account.ID, cacheProfile)
	}
	usagePayload := contextUsagePayloadFromTokens(req.Model, result.ContextInputTokens)
	publicInputTokens := claudePublicInputTokens(usagePayload, req.Model, inputTokens)
	responseCacheUsage := cacheUsage
	contextUsage := kiroContextUsage(payload, req.Model)
	// ContextInputTokens includes Kiro-owned instructions. Use it for the gradual
	// public usage lift after 28%, while retaining the exact value diagnostically.
	responseCacheUsage = claudeTerminalCacheUsage(cacheProfile, usagePayload, req.Model, inputTokens, cacheUsage)
	publicInputTokens, responseCacheUsage = result.RetryUsage.apply(publicInputTokens, responseCacheUsage)
	if result.ContextInputTokens > 0 {
		contextUsage = kiroContextUsageFromTokens(req.Model, result.ContextInputTokens)
	}
	h.recordSuccessForApiKeyWithCache(apiKeyID, inputTokens, outputTokens, result.Credits, cacheUsage)
	result.RetryUsage.recordForAPIKey(apiKeyID)
	accountID := ""
	if result.Account != nil {
		accountID = result.Account.ID
	}
	h.recordSuccessLog("claude", req.Model, accountID, inputTokens+outputTokens, result.Credits, time.Since(started).Milliseconds())

	if req.Stream {
		if err := writeCodeExecutionSSE(w, responseModel, blocks, container, publicInputTokens, outputTokens, responseCacheUsage, contextUsage); err != nil {
			h.recordFailureWithDetails("claude", req.Model, accountID, err)
		}
		return
	}

	response := &ClaudeResponse{
		ID:          newClaudeMessageID(),
		Type:        "message",
		Role:        "assistant",
		Content:     blocks,
		Container:   container,
		Model:       responseModel,
		StopReason:  "end_turn",
		StopDetails: nil,
		Usage: ClaudeUsage{
			InputTokens:              billedClaudeInputTokens(publicInputTokens, responseCacheUsage),
			OutputTokens:             outputTokens,
			OutputTokensDetails:      &ClaudeOutputTokensDetails{},
			CacheCreationInputTokens: responseCacheUsage.CacheCreationInputTokens,
			CacheReadInputTokens:     responseCacheUsage.CacheReadInputTokens,
			CacheCreation: &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: responseCacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: responseCacheUsage.CacheCreation1hInputTokens,
			},
			ServiceTier:      "standard",
			InferenceGeo:     "not_available",
			ServerToolUse:    &ClaudeServerToolUsage{},
			KiroContextUsage: contextUsage,
		},
		ContextManagement: ClaudeContextManagement{AppliedEdits: []interface{}{}},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.recordFailureWithDetails("claude", req.Model, accountID, err)
	}
}

func writeCodeExecutionSSE(
	w http.ResponseWriter,
	model string,
	blocks []ClaudeContentBlock,
	container *ClaudeContainer,
	inputTokens, outputTokens int,
	cacheUsage promptCacheUsage,
	contextUsage *KiroContextUsage,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming not supported")
	}
	configureSSEHeaders(w)
	send := func(event string, data interface{}) {
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
		flusher.Flush()
	}

	send("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"model": model, "id": newClaudeMessageID(), "type": "message", "role": "assistant",
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil, "stop_details": nil,
			"usage": buildClaudeUsageMap(inputTokens, 1, cacheUsage, true),
		},
	})
	serverUse := blocks[0]
	send("content_block_start", map[string]interface{}{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]interface{}{
			"type": serverUse.Type, "id": serverUse.ID, "name": serverUse.Name, "input": map[string]interface{}{},
		},
	})
	send("ping", map[string]interface{}{"type": "ping"})
	send("content_block_delta", map[string]interface{}{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": ""},
	})
	inputJSON, _ := json.Marshal(serverUse.Input)
	send("content_block_delta", map[string]interface{}{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(inputJSON)},
	})
	send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
	send("content_block_start", map[string]interface{}{
		"type": "content_block_start", "index": 1, "content_block": blocks[1],
	})
	send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 1})
	send("content_block_start", map[string]interface{}{
		"type": "content_block_start", "index": 2,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	send("content_block_delta", map[string]interface{}{
		"type": "content_block_delta", "index": 2,
		"delta": map[string]interface{}{"type": "text_delta", "text": blocks[2].Text},
	})
	send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 2})
	usage := buildClaudeTerminalUsageMap(inputTokens, outputTokens, cacheUsage)
	if contextUsage != nil {
		usage["kiro_context_usage"] = contextUsage
	}
	usage["server_tool_use"] = map[string]int{"web_search_requests": 0, "web_fetch_requests": 0}
	send("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason": "end_turn", "stop_sequence": nil, "stop_details": nil, "container": container,
		},
		"usage":              usage,
		"context_management": map[string]interface{}{"applied_edits": []interface{}{}},
	})
	send("message_stop", map[string]interface{}{"type": "message_stop"})
	return nil
}
