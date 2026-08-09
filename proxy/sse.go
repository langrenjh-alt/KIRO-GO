package proxy

import "net/http"

const (
	thinkingOpenTag  = "<thinking>"
	thinkingCloseTag = "</thinking>"
)

type claudeStreamPrelude struct {
	Blocks       []ClaudeContentBlock
	InputTokens  int
	OutputTokens int
	Credits      float64
	RetryUsage   retryBillingUsage
}

// configureSSEHeaders prevents HTTP intermediaries from buffering or
// transforming event-stream responses.
func configureSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

// splitBeforePartialTag returns text that can be emitted immediately while
// retaining only a suffix that could become tag when the next chunk arrives.
func splitBeforePartialTag(text, tag string) (ready, pending string) {
	maxPrefix := len(tag) - 1
	if len(text) < maxPrefix {
		maxPrefix = len(text)
	}
	for size := maxPrefix; size > 0; size-- {
		if text[len(text)-size:] == tag[:size] {
			return text[:len(text)-size], text[len(text)-size:]
		}
	}
	return text, ""
}
