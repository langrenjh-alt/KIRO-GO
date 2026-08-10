package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// claudeCodeBufferedResponseWriter keeps /cc/v1/messages SSE bytes private
// until the upstream turn has completed. This mirrors kiro.rs' buffered Claude
// Code endpoint while leaving the ordinary /v1/messages stream unchanged.
type claudeCodeBufferedResponseWriter struct {
	destination http.ResponseWriter
	body        bytes.Buffer
	status      int
	committed   bool
}

func (w *claudeCodeBufferedResponseWriter) Header() http.Header {
	return w.destination.Header()
}

func (w *claudeCodeBufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *claudeCodeBufferedResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *claudeCodeBufferedResponseWriter) Flush() {
	// Rust's buffered stream still emits periodic keep-alives while it waits
	// for the upstream body. Forward only those frames; assistant data remains
	// buffered until the complete turn is ready.
	for {
		body := w.body.Bytes()
		var keepAlive []byte
		switch {
		case bytes.HasSuffix(body, []byte(": keep-alive\n\n")):
			keepAlive = []byte(": keep-alive\n\n")
		case bytes.HasSuffix(body, []byte("\n\n")):
			end := len(body) - len("\n\n")
			start := bytes.LastIndex(body[:end], []byte("\n\n"))
			if start < 0 {
				start = 0
			} else {
				start += len("\n\n")
			}
			if bytes.HasPrefix(body[start:end], []byte("event: ping\n")) {
				keepAlive = append([]byte(nil), body[start:]...)
			}
		}
		if len(keepAlive) == 0 {
			return
		}
		w.body.Truncate(len(body) - len(keepAlive))
		if w.status == 0 {
			w.status = http.StatusOK
		}
		_, _ = w.destination.Write(keepAlive)
		w.committed = true
		if flusher, ok := w.destination.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func (h *Handler) handleClaudeCodeBufferedStream(
	w http.ResponseWriter,
	payload *KiroPayload,
	model, responseModel string,
	thinking bool,
	thinkingOpts claudeThinkingResponseOptions,
	estimatedInputTokens int,
	cacheProfile *promptCacheProfile,
	apiKeyID string,
) {
	buffered := &claudeCodeBufferedResponseWriter{destination: w}
	h.handleClaudeStream(
		buffered, payload, model, responseModel, thinking, thinkingOpts,
		estimatedInputTokens, cacheProfile, apiKeyID,
	)

	status := buffered.status
	if status == 0 {
		status = http.StatusOK
	}
	body := buffered.body.Bytes()
	if status == http.StatusOK {
		body = patchClaudeCodeUsageTotals(body)
	}
	if !buffered.committed {
		w.WriteHeader(status)
	}
	_, _ = w.Write(body)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func patchClaudeCodeUsageTotals(body []byte) []byte {
	usagePatch, ok := claudeCodeTerminalUsagePatch(body)
	if !ok {
		return body
	}

	blocks := bytes.SplitAfter(body, []byte("\n\n"))
	for index, block := range blocks {
		if !bytes.HasPrefix(block, []byte("event: message_start\n")) &&
			!bytes.HasPrefix(block, []byte("event: message_delta\n")) {
			continue
		}
		patched, ok := patchClaudeCodeUsageBlock(block, usagePatch)
		if !ok {
			continue
		}
		blocks[index] = patched
	}
	return bytes.Join(blocks, nil)
}

type claudeCodeUsagePatch struct {
	inputTokens         int
	cacheCreationTokens int
	cacheReadTokens     int
}

// Claude Code treats message_delta usage as authoritative over message_start
// and calculates context input as input + cache_creation + cache_read. Patch the
// same standard tuple into both events so the terminal event cannot undo the
// buffered context correction and cached tokens are not counted twice.
func claudeCodeTerminalUsagePatch(body []byte) (claudeCodeUsagePatch, bool) {
	for _, block := range bytes.Split(body, []byte("\n\n")) {
		if !bytes.HasPrefix(block, []byte("event: message_delta\n")) {
			continue
		}
		event, _, _, ok := decodeClaudeCodeSSEBlock(block)
		if !ok {
			continue
		}
		usage, ok := event["usage"].(map[string]interface{})
		if !ok {
			continue
		}
		contextUsage, ok := usage["kiro_context_usage"].(map[string]interface{})
		if !ok {
			continue
		}
		contextInputTokens := claudeCodeUsageInt(contextUsage["input_tokens"])
		if contextInputTokens <= 0 {
			continue
		}

		cacheCreationTokens := claudeCodeUsageInt(usage["cache_creation_input_tokens"])
		cacheReadTokens := claudeCodeUsageInt(usage["cache_read_input_tokens"])
		cacheTotal := cacheCreationTokens + cacheReadTokens
		if cacheTotal > contextInputTokens {
			cacheReadTokens = int(float64(contextInputTokens)*float64(cacheReadTokens)/float64(cacheTotal) + 0.5)
			if cacheReadTokens > contextInputTokens {
				cacheReadTokens = contextInputTokens
			}
			cacheCreationTokens = contextInputTokens - cacheReadTokens
		}
		return claudeCodeUsagePatch{
			inputTokens:         contextInputTokens - cacheCreationTokens - cacheReadTokens,
			cacheCreationTokens: cacheCreationTokens,
			cacheReadTokens:     cacheReadTokens,
		}, true
	}
	return claudeCodeUsagePatch{}, false
}

func patchClaudeCodeUsageBlock(block []byte, patch claudeCodeUsagePatch) ([]byte, bool) {
	event, dataStart, dataEnd, ok := decodeClaudeCodeSSEBlock(block)
	if !ok {
		return block, false
	}

	var usage map[string]interface{}
	switch event["type"] {
	case "message_start":
		message, ok := event["message"].(map[string]interface{})
		if !ok {
			return block, false
		}
		usage, ok = message["usage"].(map[string]interface{})
		if !ok {
			return block, false
		}
	case "message_delta":
		usage, ok = event["usage"].(map[string]interface{})
		if !ok {
			return block, false
		}
		// Only the terminal delta carries Kiro's authoritative context event.
		// Leave any future intermediate usage deltas untouched; Claude Code
		// consumes the last usage-bearing delta as the completed turn total.
		if _, ok := usage["kiro_context_usage"]; !ok {
			return block, false
		}
	default:
		return block, false
	}

	usage["input_tokens"] = patch.inputTokens
	if _, exists := usage["cache_creation_input_tokens"]; exists || patch.cacheCreationTokens > 0 {
		usage["cache_creation_input_tokens"] = patch.cacheCreationTokens
	}
	if _, exists := usage["cache_read_input_tokens"]; exists || patch.cacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = patch.cacheReadTokens
	}
	if cacheCreation, ok := usage["cache_creation"].(map[string]interface{}); ok {
		fiveMinute := claudeCodeUsageInt(cacheCreation["ephemeral_5m_input_tokens"])
		oneHour := claudeCodeUsageInt(cacheCreation["ephemeral_1h_input_tokens"])
		fiveMinute, oneHour = scaleClaudeCodeCacheCreation(fiveMinute, oneHour, patch.cacheCreationTokens)
		cacheCreation["ephemeral_5m_input_tokens"] = fiveMinute
		cacheCreation["ephemeral_1h_input_tokens"] = oneHour
	}

	encoded, err := json.Marshal(event)
	if err != nil {
		return block, false
	}
	patched := make([]byte, 0, len(block)+len(encoded)-(dataEnd-dataStart))
	patched = append(patched, block[:dataStart]...)
	patched = append(patched, encoded...)
	patched = append(patched, block[dataEnd:]...)
	return patched, true
}

func decodeClaudeCodeSSEBlock(block []byte) (map[string]interface{}, int, int, bool) {
	dataStart := bytes.Index(block, []byte("data: "))
	if dataStart < 0 {
		return nil, 0, 0, false
	}
	dataStart += len("data: ")
	dataLength := bytes.IndexByte(block[dataStart:], '\n')
	if dataLength < 0 {
		dataLength = len(block) - dataStart
	}
	dataEnd := dataStart + dataLength

	var event map[string]interface{}
	if err := json.Unmarshal(block[dataStart:dataEnd], &event); err != nil {
		return nil, 0, 0, false
	}
	return event, dataStart, dataEnd, true
}

func claudeCodeUsageInt(value interface{}) int {
	number, ok := value.(float64)
	if !ok || number <= 0 {
		return 0
	}
	return int(number)
}

func scaleClaudeCodeCacheCreation(fiveMinute, oneHour, total int) (int, int) {
	if total <= 0 {
		return 0, 0
	}
	previousTotal := fiveMinute + oneHour
	if previousTotal <= 0 {
		return total, 0
	}
	scaledOneHour := int(float64(total)*float64(oneHour)/float64(previousTotal) + 0.5)
	if scaledOneHour > total {
		scaledOneHour = total
	}
	return total - scaledOneHour, scaledOneHour
}
