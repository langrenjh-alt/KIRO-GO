package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// A tool turn is not the final structured answer. Likewise a refusal or an
// explicitly truncated response must keep its native termination semantics,
// rather than being retried until it looks like a successful JSON answer.
func claudeStructuredOutputRequiresValidation(payload *KiroPayload, toolCount int, stopReason string, contextExceeded bool, matchedStop string) bool {
	if payload == nil || !payload.StructuredOutput || toolCount > 0 || contextExceeded || matchedStop != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(normalizeKiroStopReason(stopReason))) {
	case "max_tokens", "model_context_window_exceeded", "refusal", "pause_turn", "stop_sequence":
		return false
	}
	return true
}

func prepareClaudeStructuredOutputRetry(payload *KiroPayload) {
	if payload == nil {
		return
	}
	// This is a bounded format correction, not fabricated output or a new user
	// task. The original schema, current request, tools and history stay intact.
	payload.ConversationState.CurrentMessage.UserInputMessage.Content += "\n\n<structured_output_retry>" +
		"The previous attempt did not match the requested output format. Produce the answer to the original request as exactly one JSON value satisfying the supplied JSON Schema. " +
		"Do not add Markdown fences or explanatory text, and do not replace missing information with invented facts. If a tool is needed, make its real tool call first." +
		"</structured_output_retry>"
}

// Structured output is generated and checked before assistant content is
// committed to the stream. An invalid attempt can then be retried without
// leaking bad JSON, partial tool calls or discarded reasoning into the result.
// Ordinary unstructured streams retain their existing incremental behavior.
func (h *Handler) handleClaudeStructuredStream(w http.ResponseWriter, payload *KiroPayload, model, responseModel string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, prelude *claudeStreamPrelude) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, http.StatusInternalServerError, "api_error", "Streaming not supported")
		return
	}
	h.handleClaudeBufferedResponse(w, payload, model, responseModel, thinking, thinkingOpts, estimatedInputTokens, cacheProfile, apiKeyID, prelude, func(response *ClaudeResponse) {
		if kiroPayloadContext(payload).Err() != nil {
			return
		}
		configureSSEHeaders(w)
		send := func(event string, value interface{}) {
			if kiroPayloadContext(payload).Err() == nil {
				h.sendSSE(w, flusher, event, value)
			}
		}
		startUsage := response.Usage
		startUsage.OutputTokens = 1
		if _, deferInflation := w.(*claudeCodeBufferedResponseWriter); !deferInflation {
			startUsage.OutputTokens = inflateBillingTokens(startUsage.OutputTokens)
		}
		startUsage.OutputTokensDetails = nil
		send("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id": response.ID, "type": "message", "role": "assistant",
				"model": response.Model, "content": []interface{}{},
				"stop_reason": nil, "stop_sequence": nil, "stop_details": nil,
				"usage": startUsage,
			},
		})
		send("ping", map[string]interface{}{"type": "ping"})
		for index, block := range response.Content {
			start := interface{}(block)
			switch block.Type {
			case "text":
				start = map[string]interface{}{"type": "text", "text": ""}
			case "thinking":
				start = map[string]interface{}{"type": "thinking", "thinking": ""}
			case "tool_use":
				start = map[string]interface{}{
					"type": "tool_use", "id": block.ID, "name": block.Name,
					"input": map[string]interface{}{}, "caller": block.Caller,
				}
			}
			send("content_block_start", map[string]interface{}{"type": "content_block_start", "index": index, "content_block": start})
			sendDelta := func(delta map[string]interface{}) {
				send("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": index, "delta": delta})
			}
			switch block.Type {
			case "text":
				if block.Text != "" {
					sendDelta(map[string]interface{}{"type": "text_delta", "text": block.Text})
				}
			case "thinking":
				if block.Thinking != "" {
					sendDelta(map[string]interface{}{"type": "thinking_delta", "thinking": block.Thinking})
				}
				if block.Signature != "" {
					sendDelta(map[string]interface{}{"type": "signature_delta", "signature": block.Signature})
				}
			case "tool_use":
				input, _ := json.Marshal(block.Input)
				sendDelta(map[string]interface{}{"type": "input_json_delta", "partial_json": string(input)})
			}
			send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": index})
		}
		send("message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": response.StopReason, "stop_sequence": response.StopSequence, "stop_details": response.StopDetails,
			},
			"usage": response.Usage, "context_management": response.ContextManagement,
		})
		send("message_stop", map[string]interface{}{"type": "message_stop"})
		if kiroPayloadContext(payload).Err() == nil {
			completeAccountQuotaReceipt(kiroPayloadContext(payload))
		}
	})
}
