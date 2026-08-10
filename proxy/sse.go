package proxy

import (
	"net/http"
	"strings"
	"unicode"
)

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

// splitBeforeThinkingStartTag keeps a complete opening tag at a frame boundary
// until the next byte arrives. That byte decides whether the tag is a protocol
// marker or a quoted/punctuation-delimited literal.
func splitBeforeThinkingStartTag(text string) (ready, pending string) {
	if strings.TrimSpace(text) == "" {
		return "", text
	}
	if strings.HasSuffix(text, thinkingOpenTag) {
		ready = text[:len(text)-len(thinkingOpenTag)]
		pending = thinkingOpenTag
	} else {
		ready, pending = splitBeforePartialTag(text, thinkingOpenTag)
	}
	if pending != "" && strings.TrimSpace(ready) == "" {
		return "", ready + pending
	}
	return ready, pending
}

// findRealThinkingStartTag ignores tags that are embedded in quoted strings or
// inline code. Kiro often mentions the literal protocol tags while explaining
// a tool call; treating those mentions as state transitions corrupts the SSE
// block order.
func findRealThinkingStartTag(text string) int {
	return findRealThinkingStartTagWithLookbehind(text, 0)
}

func findRealThinkingStartTagWithLookbehind(text string, lookbehind byte) int {
	const tag = thinkingOpenTag
	searchStart := 0
	for searchStart <= len(text)-len(tag) {
		relative := strings.Index(text[searchStart:], tag)
		if relative < 0 {
			return -1
		}
		pos := searchStart + relative
		if !thinkingTagQuotedWithLookbehind(text, pos, len(tag), lookbehind) {
			return pos
		}
		searchStart = pos + 1
	}
	return -1
}

// findRealThinkingEndTag ignores quoted closing tags and waits for at least
// one byte after a complete tag. The following byte may be ordinary answer
// text (some Kiro models omit the blank-line separator), while an exact tag at
// the end of a frame is retained by splitBeforeThinkingEndTag below.
func findRealThinkingEndTag(text string) int {
	return findRealThinkingEndTagWithLookbehind(text, 0)
}

func findRealThinkingEndTagWithLookbehind(text string, lookbehind byte) int {
	const tag = thinkingCloseTag
	searchStart := 0
	for searchStart <= len(text)-len(tag) {
		relative := strings.Index(text[searchStart:], tag)
		if relative < 0 {
			return -1
		}
		pos := searchStart + relative
		if thinkingTagQuotedWithLookbehind(text, pos, len(tag), lookbehind) {
			searchStart = pos + 1
			continue
		}
		after := text[pos+len(tag):]
		if len(after) == 0 {
			return -1
		}
		return pos
	}
	return -1
}

func splitBeforeThinkingEndTag(text string) (ready, pending string) {
	if strings.HasSuffix(text, thinkingCloseTag) {
		return text[:len(text)-len(thinkingCloseTag)], thinkingCloseTag
	}
	return splitBeforePartialTag(text, thinkingCloseTag)
}

// consumeThinkingClosingSeparator removes the protocol blank line after a real
// closing tag. A single newline is retained until the next frame so a split
// "\n\n" separator is not leaked as visible answer text.
func consumeThinkingClosingSeparator(text string, force bool) (rest string, wait bool) {
	switch {
	case strings.HasPrefix(text, "\r\n\r\n"):
		return strings.TrimLeftFunc(text[4:], unicode.IsSpace), false
	case strings.HasPrefix(text, "\n\n"):
		return strings.TrimLeftFunc(text[2:], unicode.IsSpace), false
	}
	if strings.TrimSpace(text) == "" {
		if force {
			// kiro.rs trims all formatting whitespace after a closing thinking tag
			// during final flush, including a separator that ends at EOF.
			return "", false
		}
		return text, true
	}
	return strings.TrimLeftFunc(text, unicode.IsSpace), false
}

func findThinkingEndTagAtBufferEnd(text string) int {
	return findThinkingEndTagAtBufferEndWithLookbehind(text, 0)
}

func findThinkingEndTagAtBufferEndWithLookbehind(text string, lookbehind byte) int {
	const tag = thinkingCloseTag
	searchStart := 0
	for searchStart <= len(text)-len(tag) {
		relative := strings.Index(text[searchStart:], tag)
		if relative < 0 {
			return -1
		}
		pos := searchStart + relative
		if !thinkingTagQuotedWithLookbehind(text, pos, len(tag), lookbehind) && strings.TrimSpace(text[pos+len(tag):]) == "" {
			return pos
		}
		searchStart = pos + 1
	}
	return -1
}

func thinkingTagQuoted(text string, pos, tagLen int) bool {
	return thinkingTagQuotedWithLookbehind(text, pos, tagLen, 0)
}

func thinkingTagQuotedWithLookbehind(text string, pos, tagLen int, lookbehind byte) bool {
	// Match kiro.rs' boundary rule: a tag adjacent to a quoting or
	// punctuation byte is prose/code, not a protocol delimiter. This keeps
	// literal examples such as `</thinking>` and "<thinking>" visible.
	if pos == 0 && strings.ContainsRune(thinkingTagBoundaryChars, rune(lookbehind)) {
		return true
	}
	if pos > 0 && strings.ContainsRune(thinkingTagBoundaryChars, rune(text[pos-1])) {
		return true
	}
	after := pos + tagLen
	return after < len(text) && strings.ContainsRune(thinkingTagBoundaryChars, rune(text[after]))
}

const thinkingTagBoundaryChars = "`\"'\\#!@$%^&*()-_=+[]{};:><,.?/"

// trimThinkingLeadingNewline removes the formatting newline emitted after an
// opening tag. The flag remains set for an empty delta so the behavior is
// stable when the tag and its first content arrive in different frames.
func trimThinkingLeadingNewline(text string, pending *bool) string {
	if pending == nil || !*pending || text == "" {
		return text
	}
	*pending = false
	if strings.HasPrefix(text, "\r\n") {
		return text[2:]
	}
	if strings.HasPrefix(text, "\n") {
		return text[1:]
	}
	return text
}
