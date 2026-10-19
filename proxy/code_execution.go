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
	bashCodeExecutionToolName   = "bash_code_execution"
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
	generated.Messages = append([]ClaudeMessage(nil), req.Messages...)
	generated.Stream = false
	generated.Thinking = nil
	generated.StopSequences = nil
	name, inputField := codeExecutionToolName, "code"
	description := "Execute standalone Python code in an isolated environment without network access. Each invocation starts a fresh interpreter; do not rely on state, files, or variables from earlier invocations. Use the actual returned stdout, stderr, and exit status to answer the user. Do not claim that code ran when the tool returned an error."
	if tool, ok := findNativeCodeExecutionTool(req); ok && strings.TrimSpace(tool.Type) == codeExecutionToolTypeLatest {
		name, inputField = bashCodeExecutionToolName, "command"
		description = "Run a Bash command in an isolated Linux container without network access. Python 3 is available: execute Python using python3 and an inline script or a here-document in this same command. Each invocation starts a fresh container; files and state do not persist between invocations. There is no text-editor sub-tool, file-download service, or persistent REPL. Use the actual returned stdout, stderr, and exit status to answer; never claim code ran when execution returned an error."
		if choice, err := resolveToolChoice(req.ToolChoice); err == nil && choice.Mode == "specific" && choice.Name == codeExecutionToolName {
			// Only the internal generator uses the implementation sub-tool name.
			// Keep the client's declaration and original choice unchanged.
			mapped := map[string]interface{}{}
			encoded, _ := json.Marshal(req.ToolChoice)
			_ = json.Unmarshal(encoded, &mapped)
			mapped["type"], mapped["name"] = "tool", name
			delete(mapped, "function")
			generated.ToolChoice = mapped
		}
	}
	executionTool := ClaudeTool{
		Type:        "custom",
		Name:        name,
		Description: description,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				inputField: map[string]interface{}{"type": "string"},
			},
			"required":             []string{inputField},
			"additionalProperties": false,
		},
	}
	generated.Tools = make([]ClaudeTool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		if isNativeCodeExecutionTool(tool) {
			generated.Tools = append(generated.Tools, executionTool)
		} else {
			generated.Tools = append(generated.Tools, tool)
		}
	}
	return &generated
}

func codeExecutionBlocks(toolUse KiroToolUse) []ClaudeContentBlock {
	blocks := codeExecutionResultBlocks(toolUse, CodeExecutionResult{ReturnCode: -1, ErrorCode: "unavailable"})
	return append(blocks, ClaudeContentBlock{Type: "text", Text: codeExecutionUnavailableText})
}

func codeExecutionResultBlocks(toolUse KiroToolUse, result CodeExecutionResult) []ClaudeContentBlock {
	name := codeExecutionToolName
	if toolUse.Name == bashCodeExecutionToolName {
		name = bashCodeExecutionToolName
	}
	toolUseID := newClaudeServerToolUseID()
	input := map[string]interface{}{}
	for key, value := range toolUse.Input {
		input[key] = value
	}
	var content interface{} = map[string]interface{}{
		"type": name + "_result", "stdout": result.Stdout,
		"stderr": result.Stderr, "return_code": result.ReturnCode,
		"content": []interface{}{},
	}
	if result.ErrorCode != "" {
		code := publicCodeExecutionError(result.ErrorCode)
		if name == bashCodeExecutionToolName && (result.ErrorCode == "output_limit_exceeded" || result.ErrorCode == "output_file_too_large") {
			code = "output_file_too_large"
		}
		content = map[string]interface{}{"type": name + "_tool_result_error", "error_code": code}
	}
	return []ClaudeContentBlock{
		{Type: "server_tool_use", ID: toolUseID, Name: name, Input: input},
		{
			Type:      name + "_tool_result",
			ToolUseID: toolUseID,
			Content:   content,
		},
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
	nativeName, inputField, executionMode := codeExecutionToolName, "code", "python"
	if tool, ok := findNativeCodeExecutionTool(req); ok && strings.TrimSpace(tool.Type) == codeExecutionToolTypeLatest {
		nativeName, inputField, executionMode = bashCodeExecutionToolName, "command", "bash"
	}
	executor := h.codeExecutor
	var executorConfigErr error
	if executor == nil {
		executor, executorConfigErr = configuredCodeExecutor()
	}
	var blocks []ClaudeContentBlock
	var payload *KiroPayload
	var result *bufferedClaudeCall
	var retryUsage retryBillingUsage
	var credits float64
	var container *ClaudeContainer
	type pendingRound struct {
		result     *bufferedClaudeCall
		settled    bool
		blockStart int
		discarded  bool
	}
	var successfulRounds []*pendingRound
	defer func() {
		// callClaudeBuffered has already recorded these rounds in account-pool
		// stats, but it deliberately does not know the downstream API key. A
		// cancellation or later failure must not erase their consumed usage.
		for _, round := range successfulRounds {
			if !round.settled {
				h.settleCodeExecutionRound(req.Model, apiKeyID, round.result, time.Since(started).Milliseconds())
				round.settled = true
			}
		}
	}()
	executions := 0
	structuredRetries := 0
	stopReason := "end_turn"
	// Three execution rounds, one final answer and at most one format-only
	// correction. The correction cannot grant another execution allowance.
	for round := 0; round < maxCodeExecutionRounds+2; round++ {
		payload = bindKiroPayloadContext(ClaudeToKiro(generatorReq, false), ctx)
		if structuredRetries > 0 {
			prepareClaudeStructuredOutputRetry(payload)
		}
		if _, err := preflightKiroPayload(payload); err != nil {
			h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		var err error
		result, err = h.callClaudeBuffered(req.Model, payload, false, estimateClaudeRequestInputTokens(generatorReq))
		if err != nil {
			if isDownstreamRequestCanceled(payload, err) {
				return
			}
			h.handleAdvisorFailure(w, req.Model, err)
			return
		}
		credits += result.Credits
		retryUsage.merge(result.RetryUsage)
		pending := &pendingRound{result: result, blockStart: len(blocks)}
		successfulRounds = append(successfulRounds, pending)
		if ctx.Err() != nil {
			return
		}
		if payload.ToolChoiceMode == "none" && len(result.ToolUses) > 0 {
			err := errors.New("upstream returned a tool call while tools were disabled")
			h.recordFailureWithDetails("claude", req.Model, "", err)
			h.sendClaudeError(w, http.StatusBadGateway, "upstream_tool_choice_error", err.Error())
			return
		}
		var nativeTools, clientTools []KiroToolUse
		for _, toolUse := range result.ToolUses {
			if toolUse.Name == nativeName {
				nativeTools = append(nativeTools, toolUse)
			} else {
				clientTools = append(clientTools, toolUse)
			}
		}
		if len(nativeTools) == 0 {
			validationPayload := payload
			if req.OutputConfig != nil && req.OutputConfig.Format != nil {
				// The final answer contract belongs to the original request,
				// even when auto choice never needed an execution round.
				validation := *payload
				validation.StructuredOutput = true
				validation.StructuredOutputType = req.OutputConfig.Format.Type
				validation.StructuredOutputSchema = req.OutputConfig.Format.Schema
				validationPayload = &validation
			}
			text, _ := extractThinkingFromContent(result.Content)
			if claudeStructuredOutputRequiresValidation(validationPayload, len(clientTools), result.UpstreamStopReason, result.ContextWindowExceeded, "") {
				text, _ = extractStructuredOutputContent(result.Content)
				text = normalizeStructuredOutputContent(text)
				if err := validateStructuredOutputContent(text, validationPayload.StructuredOutputType, validationPayload.StructuredOutputSchema); err != nil {
					// This invalid final answer is a consumed model round, not a
					// Python execution. Settle it once without re-recording the pool.
					attempt := h.settleCodeExecutionRound(req.Model, apiKeyID, result, time.Since(started).Milliseconds())
					pending.settled = true
					pending.discarded = true
					credits -= result.Credits
					retryUsage.add(attempt)
					if structuredRetries >= 1 {
						failure := fmt.Errorf("upstream structured output did not match the requested format after one validation retry: %w", err)
						h.recordFailureWithDetails("claude", req.Model, "", failure)
						h.sendClaudeError(w, http.StatusBadGateway, "upstream_structured_output_error", failure.Error())
						return
					}
					structuredRetries++
					generatorReq.Tools = nil
					generatorReq.ToolChoice = map[string]interface{}{"type": "none"}
					generatorReq.OutputConfig = req.OutputConfig
					continue
				}
			}
			if text != "" {
				blocks = append(blocks, ClaudeContentBlock{Type: "text", Text: text})
			}
			for _, toolUse := range clientTools {
				blocks = append(blocks, ClaudeContentBlock{Type: "tool_use", ID: normalizeClaudeToolUseID(toolUse.ToolUseID), Name: toolUse.Name, Input: toolUse.Input, Caller: &ClaudeCaller{Type: "direct"}})
			}
			stopReason = resolveClaudeStopReason(len(clientTools), result.ContextWindowExceeded, result.UpstreamStopReason)
			if validationPayload.StructuredOutput && (strings.EqualFold(result.UpstreamStopReason, "refusal") || strings.EqualFold(result.UpstreamStopReason, "pause_turn")) {
				stopReason = strings.ToLower(result.UpstreamStopReason)
			}
			break
		}
		if container == nil {
			container = newCodeExecutionContainer()
		}
		if executor == nil || executorConfigErr != nil {
			// Retain the explicit unavailable response when no isolated worker is
			// configured; never substitute model-generated answers for execution.
			blocks = append(blocks, codeExecutionBlocks(nativeTools[0])...)
			break
		}
		var assistantBlocks, resultBlocks []interface{}
		if result.Content != "" {
			assistantBlocks = append(assistantBlocks, map[string]interface{}{"type": "text", "text": result.Content})
		}
		executionFailed := false
		for _, toolUse := range nativeTools {
			toolID := normalizeClaudeToolUseID(toolUse.ToolUseID)
			assistantBlocks = append(assistantBlocks, map[string]interface{}{"type": "tool_use", "id": toolID, "name": nativeName, "input": toolUse.Input})
			execution := CodeExecutionResult{ReturnCode: -1}
			code, isString := toolUse.Input[inputField].(string)
			switch {
			case !isString || len(code) == 0 || len(code) > maxCodeExecutionCodeBytes:
				execution.ErrorCode = "invalid_tool_input"
			case executions >= maxCodeExecutionRounds:
				execution.ErrorCode = "too_many_requests"
			default:
				executions++
				var executeErr error
				if executionMode == "bash" {
					if modern, ok := executor.(modeCodeExecutor); ok {
						execution, executeErr = modern.ExecuteMode(ctx, executionMode, code)
					} else {
						execution.ErrorCode = "unavailable"
					}
				} else {
					execution, executeErr = executor.Execute(ctx, code)
				}
				if ctx.Err() != nil {
					return
				}
				if executeErr != nil && execution.ErrorCode == "" {
					execution.ErrorCode = "unavailable"
				}
			}
			executionFailed = executionFailed || execution.ErrorCode != "" || execution.ReturnCode != 0
			blocks = append(blocks, codeExecutionResultBlocks(toolUse, execution)...)
			encoded, _ := json.Marshal(execution)
			resultBlocks = append(resultBlocks, map[string]interface{}{
				"type": "tool_result", "tool_use_id": toolID, "content": string(encoded), "is_error": execution.ErrorCode != "" || execution.ReturnCode != 0,
			})
		}
		if len(clientTools) > 0 {
			for _, toolUse := range clientTools {
				blocks = append(blocks, ClaudeContentBlock{Type: "tool_use", ID: normalizeClaudeToolUseID(toolUse.ToolUseID), Name: toolUse.Name, Input: toolUse.Input, Caller: &ClaudeCaller{Type: "direct"}})
			}
			stopReason = "tool_use"
			break
		}
		generatorReq.Messages = append(generatorReq.Messages,
			ClaudeMessage{Role: "assistant", Content: assistantBlocks},
			ClaudeMessage{Role: "user", Content: resultBlocks},
		)
		generatorReq.ToolChoice = map[string]interface{}{"type": "auto"}
		generatorReq.OutputConfig = req.OutputConfig
		if executionFailed || executions >= maxCodeExecutionRounds {
			generatorReq.Tools = nil
			generatorReq.ToolChoice = map[string]interface{}{"type": "none"}
			generatorReq.OutputConfig = req.OutputConfig
		}
	}
	if result == nil {
		h.sendClaudeError(w, http.StatusBadGateway, "api_error", "code execution response is missing")
		return
	}
	inputTokens := max(1, estimatedInputTokens)
	outputTokens := estimateCodeExecutionOutputTokens(blocks)
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
	publicInputTokens, responseCacheUsage = retryUsage.apply(publicInputTokens, responseCacheUsage)
	if result.ContextInputTokens > 0 {
		contextUsage = kiroContextUsageFromTokens(req.Model, result.ContextInputTokens)
	}
	h.recordSuccessForApiKeyWithCache(apiKeyID, inputTokens, outputTokens, credits, cacheUsage)
	for _, round := range successfulRounds {
		if !round.settled {
			round.result.RetryUsage.recordForAPIKey(apiKeyID)
			round.settled = true
		}
	}
	accountID := ""
	if result.Account != nil {
		accountID = result.Account.ID
	}
	h.recordSuccessLog("claude", req.Model, accountID, inputTokens+outputTokens, credits, time.Since(started).Milliseconds())
	var quotaOwners []quotaOwner
	for i, round := range successfulRounds {
		if round.discarded {
			continue
		}
		end := len(blocks)
		if i+1 < len(successfulRounds) {
			end = successfulRounds[i+1].blockStart
		}
		visibleOutput := 0
		if end > round.blockStart {
			visibleOutput = estimateCodeExecutionOutputTokens(blocks[round.blockStart:end])
		}
		owner := quotaOwner{Model: round.result.Model, Input: int64(round.result.InputTokens), Output: int64(visibleOutput)}
		if round.result.Account != nil {
			owner.AccountID = round.result.Account.ID
		}
		quotaOwners = append(quotaOwners, owner)
	}
	stageAccountQuotaReceipt(ctx, responseModel, quotaRawUsage(publicInputTokens, outputTokens, responseCacheUsage), quotaFinalSources(publicInputTokens, outputTokens, responseCacheUsage, retryUsage, quotaOwners...))

	if req.Stream {
		if err := writeCodeExecutionSSEWithStop(w, responseModel, blocks, container, stopReason, publicInputTokens, outputTokens, responseCacheUsage, contextUsage, ctx); err != nil {
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
		StopReason:  stopReason,
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
	inflateClaudeUsage(&response.Usage)
	updateAccountQuotaClaudeUsage(ctx, response.Usage)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.recordFailureWithDetails("claude", req.Model, accountID, err)
	} else {
		completeAccountQuotaReceipt(ctx)
	}
}

func (h *Handler) settleCodeExecutionRound(model, apiKeyID string, result *bufferedClaudeCall, elapsedMs int64) discardedAttemptUsage {
	usage := newDiscardedAttemptUsage(result.InputTokens, result.RetryUsage.finalAttemptOutputTokens(result.OutputTokens), result.Credits)
	h.recordSuccessForApiKey(apiKeyID, usage.InputTokens, usage.OutputTokens, usage.Credits)
	result.RetryUsage.recordForAPIKey(apiKeyID)
	accountID := ""
	if result.Account != nil {
		accountID = result.Account.ID
	}
	usage.AccountID, usage.Model = accountID, result.Model
	h.recordSuccessLog("claude", model, accountID, usage.totalTokens(), usage.Credits, elapsedMs)
	return usage
}

func estimateCodeExecutionOutputTokens(blocks []ClaudeContentBlock) int {
	var text strings.Builder
	var toolUses []KiroToolUse
	for _, block := range blocks {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "server_tool_use", "tool_use":
			input, _ := block.Input.(map[string]interface{})
			toolUses = append(toolUses, KiroToolUse{Name: block.Name, Input: input})
		}
	}
	// Worker stdout and protocol wrappers are not generated-model tokens.
	return max(1, estimateClaudeOutputTokensWithRedacted(text.String(), "", nil, toolUses))
}

func writeCodeExecutionSSE(
	w http.ResponseWriter,
	model string,
	blocks []ClaudeContentBlock,
	container *ClaudeContainer,
	inputTokens, outputTokens int,
	cacheUsage promptCacheUsage,
	contextUsage *KiroContextUsage,
	receiptContexts ...context.Context,
) error {
	return writeCodeExecutionSSEWithStop(w, model, blocks, container, "end_turn", inputTokens, outputTokens, cacheUsage, contextUsage, receiptContexts...)
}

func writeCodeExecutionSSEWithStop(
	w http.ResponseWriter,
	model string,
	blocks []ClaudeContentBlock,
	container *ClaudeContainer,
	stopReason string,
	inputTokens, outputTokens int,
	cacheUsage promptCacheUsage,
	contextUsage *KiroContextUsage,
	receiptContexts ...context.Context,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming not supported")
	}
	configureSSEHeaders(w)
	var writeErr error
	send := func(event string, data interface{}) {
		if writeErr != nil {
			return
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			writeErr = err
			return
		}
		_, writeErr = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
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
	for index, block := range blocks {
		switch block.Type {
		case "server_tool_use", "tool_use":
			start := map[string]interface{}{"type": block.Type, "id": block.ID, "name": block.Name, "input": map[string]interface{}{}}
			if block.Caller != nil {
				start["caller"] = block.Caller
			}
			send("content_block_start", map[string]interface{}{"type": "content_block_start", "index": index, "content_block": start})
			if index == 0 {
				send("ping", map[string]interface{}{"type": "ping"})
			}
			send("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": index,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": ""},
			})
			inputJSON, _ := json.Marshal(block.Input)
			send("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": index,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(inputJSON)},
			})
		case "text":
			send("content_block_start", map[string]interface{}{
				"type": "content_block_start", "index": index,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			send("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": index,
				"delta": map[string]interface{}{"type": "text_delta", "text": block.Text},
			})
		default:
			send("content_block_start", map[string]interface{}{"type": "content_block_start", "index": index, "content_block": block})
		}
		send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": index})
	}
	usage := buildClaudeTerminalUsageMap(inputTokens, outputTokens, cacheUsage)
	if len(receiptContexts) > 0 {
		updateAccountQuotaClaudeMap(receiptContexts[0], usage, cacheUsage)
	}
	if contextUsage != nil {
		usage["kiro_context_usage"] = contextUsage
	}
	usage["server_tool_use"] = map[string]int{"web_search_requests": 0, "web_fetch_requests": 0}
	send("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason": stopReason, "stop_sequence": nil, "stop_details": nil, "container": container,
		},
		"usage":              usage,
		"context_management": map[string]interface{}{"applied_edits": []interface{}{}},
	})
	send("message_stop", map[string]interface{}{"type": "message_stop"})
	if writeErr == nil && len(receiptContexts) > 0 {
		completeAccountQuotaReceipt(receiptContexts[0])
	}
	return writeErr
}
