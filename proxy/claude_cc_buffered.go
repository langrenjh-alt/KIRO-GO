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
		body = patchClaudeCodeMessageStartUsage(body)
	}
	if !buffered.committed {
		w.WriteHeader(status)
	}
	_, _ = w.Write(body)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func patchClaudeCodeMessageStartUsage(body []byte) []byte {
	contextInputTokens, ok := claudeCodeContextInputTokens(body)
	if !ok {
		return body
	}

	blocks := bytes.SplitAfter(body, []byte("\n\n"))
	for index, block := range blocks {
		if !bytes.HasPrefix(block, []byte("event: message_start\n")) {
			continue
		}
		dataStart := bytes.Index(block, []byte("data: "))
		if dataStart < 0 {
			continue
		}
		dataStart += len("data: ")
		dataEnd := bytes.IndexByte(block[dataStart:], '\n')
		if dataEnd < 0 {
			dataEnd = len(block) - dataStart
		}

		var event map[string]interface{}
		if err := json.Unmarshal(block[dataStart:dataStart+dataEnd], &event); err != nil {
			continue
		}
		message, ok := event["message"].(map[string]interface{})
		if !ok {
			continue
		}
		usage, ok := message["usage"].(map[string]interface{})
		if !ok {
			continue
		}
		usage["input_tokens"] = contextInputTokens
		encoded, err := json.Marshal(event)
		if err != nil {
			continue
		}

		patched := make([]byte, 0, len(block)+len(encoded)-dataEnd)
		patched = append(patched, block[:dataStart]...)
		patched = append(patched, encoded...)
		patched = append(patched, block[dataStart+dataEnd:]...)
		blocks[index] = patched
		break
	}
	return bytes.Join(blocks, nil)
}

func claudeCodeContextInputTokens(body []byte) (int, bool) {
	for _, block := range bytes.Split(body, []byte("\n\n")) {
		if !bytes.HasPrefix(block, []byte("event: message_delta\n")) {
			continue
		}
		dataStart := bytes.Index(block, []byte("data: "))
		if dataStart < 0 {
			continue
		}
		dataStart += len("data: ")
		dataEnd := bytes.IndexByte(block[dataStart:], '\n')
		if dataEnd < 0 {
			dataEnd = len(block) - dataStart
		}

		var event struct {
			Usage struct {
				KiroContextUsage *KiroContextUsage `json:"kiro_context_usage"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(block[dataStart:dataStart+dataEnd], &event); err == nil &&
			event.Usage.KiroContextUsage != nil {
			return event.Usage.KiroContextUsage.InputTokens, true
		}
	}
	return 0, false
}
