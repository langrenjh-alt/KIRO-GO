package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestKiroRSThinkingTagScannerSkipsQuotedLiterals(t *testing.T) {
	text := "literal `<thinking>` and \"</thinking>\" <thinking>real"
	start := findRealThinkingStartTag(text)
	if start != strings.LastIndex(text, "<thinking>") {
		t.Fatalf("start=%d text=%q", start, text)
	}
	endText := "literal `</thinking>` </thinking>\n\nanswer"
	end := findRealThinkingEndTag(endText)
	want := strings.Index(endText, "</thinking>\n\n")
	if end != want {
		t.Fatalf("end=%d want=%d text=%q", end, want, endText)
	}
}

func TestKiroRSThinkingTagScannerSkipsPunctuationBoundaries(t *testing.T) {
	text := "(<thinking>) and </thinking>, then <thinking>real"
	start := findRealThinkingStartTag(text)
	wantStart := strings.LastIndex(text, "<thinking>")
	if start != wantStart {
		t.Fatalf("start=%d want=%d text=%q", start, wantStart, text)
	}
	endText := "(</thinking>) </thinking>\n\nanswer"
	wantEnd := strings.Index(endText, "</thinking>\n\n")
	if end := findRealThinkingEndTag(endText); end != wantEnd {
		t.Fatalf("end=%d want=%d text=%q", end, wantEnd, endText)
	}
}

func TestKiroRSThinkingTagScannerRetainsCrossFrameClosingTag(t *testing.T) {
	ready, pending := splitBeforeThinkingEndTag("reason</thinking>")
	if ready != "reason" || pending != thinkingCloseTag {
		t.Fatalf("ready=%q pending=%q", ready, pending)
	}
	if got := findRealThinkingEndTag(pending + "answer"); got != len(pending)-len(thinkingCloseTag) {
		t.Fatalf("cross-frame end=%d", got)
	}
}

func TestKiroRSThinkingTagScannerRetainsCrossFrameOpeningTag(t *testing.T) {
	ready, pending := splitBeforeThinkingStartTag("prefix<thinking>")
	if ready != "prefix" || pending != thinkingOpenTag {
		t.Fatalf("ready=%q pending=%q", ready, pending)
	}
	if got := findRealThinkingStartTag(pending + `"literal"</thinking>answer`); got != -1 {
		t.Fatalf("quoted cross-frame opening tag was treated as a delimiter at %d", got)
	}

	ready, pending = splitBeforeThinkingStartTag(`prefix "<think`)
	if ready != `prefix "` || pending != `<think` {
		t.Fatalf("left boundary was not retained: ready=%q pending=%q", ready, pending)
	}
	if got := findRealThinkingStartTagWithLookbehind(pending+"ing>literal", ready[len(ready)-1]); got != -1 {
		t.Fatalf("cross-frame left quote was lost at %d", got)
	}
	ready, pending = splitBeforeThinkingStartTag(`"`)
	if ready != `"` || pending != "" {
		t.Fatalf("standalone opening boundary was not retained: ready=%q pending=%q", ready, pending)
	}
	if got := findRealThinkingStartTagWithLookbehind(thinkingOpenTag+"literal", ready[len(ready)-1]); got != -1 {
		t.Fatalf("standalone opening quote was lost at %d", got)
	}
}

func TestKiroRSThinkingTagScannerRetainsCrossFrameClosingLookbehind(t *testing.T) {
	ready, pending := splitBeforeThinkingEndTag(`"`)
	if ready != `"` || pending != "" {
		t.Fatalf("standalone closing boundary was not retained: ready=%q pending=%q", ready, pending)
	}
	if got := findRealThinkingEndTagWithLookbehind(thinkingCloseTag+"literal", ready[len(ready)-1]); got != -1 {
		t.Fatalf("standalone closing quote was lost at %d", got)
	}
	if got := findRealThinkingEndTag("reason" + thinkingCloseTag + "answer"); got != len("reason") {
		t.Fatalf("unseparated closing tag compatibility=%d, want %d", got, len("reason"))
	}
}

func TestKiroRSThinkingClosingSeparatorWaitsAcrossFrames(t *testing.T) {
	rest, wait := consumeThinkingClosingSeparator("\n", false)
	if rest != "\n" || !wait {
		t.Fatalf("first separator frame: rest=%q wait=%v", rest, wait)
	}
	rest, wait = consumeThinkingClosingSeparator(rest+"\nanswer", false)
	if rest != "answer" || wait {
		t.Fatalf("completed separator: rest=%q wait=%v", rest, wait)
	}
	rest, wait = consumeThinkingClosingSeparator("answer", false)
	if rest != "answer" || wait {
		t.Fatalf("unseparated answer: rest=%q wait=%v", rest, wait)
	}
	for _, input := range []string{"\nanswer", "\r\nanswer", "\t answer", "\n\n\t answer", "\r\n\r\n\t answer"} {
		rest, wait = consumeThinkingClosingSeparator(input, false)
		if rest != "answer" || wait {
			t.Errorf("format whitespace before answer %q: rest=%q wait=%v", input, rest, wait)
		}
	}

	for _, partial := range []string{"", "\n", "\r", "\r\n", "\r\n\r", " ", "\t"} {
		rest, wait = consumeThinkingClosingSeparator(partial, true)
		if rest != "" || wait {
			t.Errorf("forced partial separator %q: rest=%q wait=%v", partial, rest, wait)
		}
	}

	for _, complete := range []struct {
		input string
		want  string
	}{
		{input: "\n\nanswer", want: "answer"},
		{input: "\r\n\r\nanswer", want: "answer"},
	} {
		for split := 0; split <= len(complete.input)-len("answer"); split++ {
			prefix := complete.input[:split]
			rest, wait = consumeThinkingClosingSeparator(prefix, false)
			if wait {
				rest += complete.input[split:]
			} else {
				rest = complete.input
			}
			got, finalWait := consumeThinkingClosingSeparator(rest, true)
			if got != complete.want || finalWait {
				t.Errorf("separator %q split at %d: rest=%q wait=%v", complete.input, split, got, finalWait)
			}
		}
	}
}

func TestKiroRSStreamingAdaptersPreserveCrossFrameThinkingLiteral(t *testing.T) {
	chunkCases := []struct {
		name          string
		chunks        []string
		wantVisible   string
		wantReasoning string
	}{
		{
			name:        "right lookahead",
			chunks:      []string{thinkingOpenTag, `"literal"</thinking>answer`},
			wantVisible: `<thinking>"literal"</thinking>answer`,
		},
		{
			name:        "opening left lookbehind",
			chunks:      []string{`"`, thinkingOpenTag, "literal"},
			wantVisible: `"<thinking>literal`,
		},
		{
			name:          "closing left lookbehind",
			chunks:        []string{thinkingOpenTag + "reason", `"`, thinkingCloseTag, " prose"},
			wantReasoning: `reason"</thinking> prose`,
		},
		{
			name:          "protocol whitespace and split separator",
			chunks:        []string{"\n\n" + thinkingOpenTag, "\nreason" + thinkingCloseTag + "\n", "\nanswer"},
			wantVisible:   "answer",
			wantReasoning: "reason",
		},
		{
			name:          "partial LF separator at EOF",
			chunks:        []string{thinkingOpenTag + "reason" + thinkingCloseTag, "\n"},
			wantReasoning: "reason",
		},
		{
			name:          "partial CRLF separator at EOF",
			chunks:        []string{thinkingOpenTag + "reason" + thinkingCloseTag, "\r\n"},
			wantReasoning: "reason",
		},
		{
			name:          "partial CRLFCR separator at EOF",
			chunks:        []string{thinkingOpenTag + "reason" + thinkingCloseTag, "\r\n\r"},
			wantReasoning: "reason",
		},
		{
			name:          "format whitespace at EOF",
			chunks:        []string{thinkingOpenTag + "reason" + thinkingCloseTag, "\t "},
			wantReasoning: "reason",
		},
		{
			name:          "format whitespace before answer",
			chunks:        []string{thinkingOpenTag + "reason" + thinkingCloseTag, "\t ", "answer"},
			wantVisible:   "answer",
			wantReasoning: "reason",
		},
	}
	tests := []struct {
		name     string
		invoke   func(*Handler, *httptest.ResponseRecorder)
		visible  func(*testing.T, string) string
		terminal string
	}{
		{
			name: "Claude Messages",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", true, claudeThinkingResponseOptions{Format: "thinking"}, 5, nil, "")
			},
			visible:  claudeVisibleTextFromSSE,
			terminal: "event: message_stop",
		},
		{
			name: "Chat Completions",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleOpenAIStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", true, false, 5, "")
			},
			visible:  openAIVisibleTextFromSSE,
			terminal: "data: [DONE]",
		},
		{
			name: "Responses",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := &ResponsesRequest{Model: "claude-opus-5", Stream: true}
				h.handleResponsesStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", true, 5, "", "resp_thinking_literal", req, nil, false)
			},
			visible:  responsesVisibleTextFromSSE,
			terminal: `"type":"response.completed"`,
		},
	}

	for _, test := range tests {
		for _, chunkCase := range chunkCases {
			t.Run(test.name+"/"+chunkCase.name, func(t *testing.T) {
				full := strings.Join(chunkCase.chunks, "")
				visible, reasoning := extractThinkingFromContent(full)
				if visible != chunkCase.wantVisible || reasoning != chunkCase.wantReasoning {
					t.Fatalf("non-stream conversion diverged: visible=%q reasoning=%q", visible, reasoning)
				}

				h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
					for _, chunk := range chunkCase.chunks {
						callback.OnText(chunk, false)
					}
					callback.OnStopReason("END_TURN")
					callback.OnComplete(10, 4)
					return nil
				})
				rec := httptest.NewRecorder()
				test.invoke(h, rec)

				body := rec.Body.String()
				if got := test.visible(t, body); got != chunkCase.wantVisible || rec.Code != http.StatusOK || !strings.Contains(body, test.terminal) {
					t.Fatalf("visible=%q want=%q status=%d body=%s", got, chunkCase.wantVisible, rec.Code, body)
				}
				if chunkCase.wantReasoning != "" && !strings.Contains(body, "reason") {
					t.Fatalf("reasoning content was dropped: %s", body)
				}
			})
		}
	}
}

func claudeVisibleTextFromSSE(t *testing.T, body string) string {
	t.Helper()
	var visible strings.Builder
	for _, event := range decodeResponseContractSSE(t, body) {
		if event.Name != "content_block_delta" {
			continue
		}
		delta, _ := event.Data["delta"].(map[string]interface{})
		if delta["type"] == "text_delta" {
			text, _ := delta["text"].(string)
			visible.WriteString(text)
		}
	}
	return visible.String()
}

func openAIVisibleTextFromSSE(t *testing.T, body string) string {
	t.Helper()
	chunks, done := decodeOpenAIDataSSE(t, body)
	if !done {
		t.Fatalf("OpenAI stream did not terminate: %s", body)
	}
	var visible strings.Builder
	for _, chunk := range chunks {
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		delta, _ := choice["delta"].(map[string]interface{})
		text, _ := delta["content"].(string)
		visible.WriteString(text)
	}
	return visible.String()
}

func responsesVisibleTextFromSSE(t *testing.T, body string) string {
	t.Helper()
	var visible strings.Builder
	for _, event := range decodeResponseContractSSE(t, body) {
		if event.Name == "response.output_text.delta" {
			text, _ := event.Data["delta"].(string)
			visible.WriteString(text)
		}
	}
	return visible.String()
}

func TestKiroRSThinkingLeadingNewlineIsRemovedOnce(t *testing.T) {
	pending := true
	if got := trimThinkingLeadingNewline("\nreason", &pending); got != "reason" || pending {
		t.Fatalf("first delta=%q pending=%v", got, pending)
	}
	pending = true
	if got := trimThinkingLeadingNewline("reason", &pending); got != "reason" || pending {
		t.Fatalf("non-newline delta=%q pending=%v", got, pending)
	}
}

func TestExtractThinkingFromContentRejectsUnterminatedBlockAsVisibleText(t *testing.T) {
	visible, reasoning := extractThinkingFromContent("<thinking>private reasoning")
	if visible != "" || reasoning != "private reasoning" {
		t.Fatalf("visible=%q reasoning=%q", visible, reasoning)
	}
	visible, reasoning = extractThinkingFromContent("`<thinking>` is literal")
	if visible != "`<thinking>` is literal" || reasoning != "" {
		t.Fatalf("quoted visible=%q reasoning=%q", visible, reasoning)
	}
}

func TestResponsesOutputGuardRejectsUnterminatedThinkingWhenHidden(t *testing.T) {
	if responsesAttemptHasClientOutput("<thinking>private reasoning", "", nil, false) {
		t.Fatal("unterminated hidden thinking was accepted as client-visible output")
	}
	if !responsesAttemptHasClientOutput("<thinking>private reasoning", "", nil, true) {
		t.Fatal("requested thinking was rejected")
	}
}
