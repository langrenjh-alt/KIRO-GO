package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestHiddenReasoningOnlyResponseClassification(t *testing.T) {
	tests := []struct {
		name              string
		thinking          bool
		reasoningObserved bool
		visibleContent    string
		toolCount         int
		stopReason        string
		want              bool
	}{
		{name: "hidden reasoning only", reasoningObserved: true, want: true},
		{name: "explicit thinking", thinking: true, reasoningObserved: true},
		{name: "no reasoning", reasoningObserved: false},
		{name: "final answer", reasoningObserved: true, visibleContent: "answer"},
		{name: "tool call", reasoningObserved: true, toolCount: 1},
		{name: "terminal stop", reasoningObserved: true, stopReason: "max_tokens"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := hiddenReasoningOnlyResponse(
				test.thinking,
				test.reasoningObserved,
				test.visibleContent,
				test.toolCount,
				test.stopReason,
			)
			if got != test.want {
				t.Fatalf("hiddenReasoningOnlyResponse()=%v, want %v", got, test.want)
			}
		})
	}

	err := newUpstreamIncompleteResponseError()
	if !isUpstreamIncompleteResponseError(err) || !isRetryableKiroStreamError(err) {
		t.Fatalf("incomplete response error must be recognized and retryable: %v", err)
	}
}

func TestReasoningOnlyRetriesAcrossPublicResponseFormats(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(*Handler, *httptest.ResponseRecorder)
	}{
		{
			name: "Claude stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
		},
		{
			name: "Claude non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeNonStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")
			},
		},
		{
			name: "Chat Completions stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
					`{"model":"claude-opus-5","messages":[{"role":"user","content":"test"}],"stream":true}`,
				))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name: "Chat Completions non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
					`{"model":"claude-opus-5","messages":[{"role":"user","content":"test"}]}`,
				))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name: "Responses stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
					`{"model":"claude-opus-5","input":"test","stream":true,"store":false}`,
				))
				h.handleOpenAIResponses(rec, req)
			},
		},
		{
			name: "Responses non-stream",
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
					`{"model":"claude-opus-5","input":"test","store":false}`,
				))
				h.handleOpenAIResponses(rec, req)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				calls++
				reasoning := fmt.Sprintf("private reasoning %d", calls)
				if callback.OnReasoning != nil {
					callback.OnReasoning(KiroReasoningContentEvent{Text: reasoning})
				} else {
					callback.OnText(reasoning, true)
				}
				if calls == 2 {
					callback.OnText("final answer", false)
				}
				callback.OnComplete(10, 4)
				return nil
			})
			rec := httptest.NewRecorder()

			test.invoke(h, rec)

			if calls != 2 {
				t.Fatalf("upstream calls=%d, want 2", calls)
			}
			body := rec.Body.String()
			if rec.Code != http.StatusOK || !strings.Contains(body, "final answer") {
				t.Fatalf("status=%d body=%s", rec.Code, body)
			}
			if strings.Contains(body, "private reasoning") || strings.Contains(body, upstreamIncompleteResponseCode) {
				t.Fatalf("discarded reasoning attempt leaked into recovered response: %s", body)
			}
			if test.name == "Responses stream" && strings.Count(body, "event: response.in_progress") != 1 {
				t.Fatalf("Responses retry emitted duplicate in-progress events: %s", body)
			}
		})
	}
}

func TestClaudeStreamReasoningOnlyRetryExhaustionReturnsError(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnReasoning(KiroReasoningContentEvent{Text: "private reasoning"})
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	if calls != maxKiroPreResponseRetries+1 {
		t.Fatalf("upstream calls=%d, want %d", calls, maxKiroPreResponseRetries+1)
	}
	body := rec.Body.String()
	if rec.Code != http.StatusBadGateway || !strings.Contains(body, upstreamIncompleteResponseCode) {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	if strings.Contains(body, "private reasoning") || strings.Contains(body, "message_stop") {
		t.Fatalf("reasoning-only exhaustion was exposed as a normal completion: %s", body)
	}
}

func TestBufferedClaudeCallRetriesReasoningOnlyResponse(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnReasoning(KiroReasoningContentEvent{Text: "private reasoning"})
		if calls == 2 {
			callback.OnText("final answer", false)
		}
		callback.OnComplete(10, 4)
		return nil
	})

	result, err := h.callClaudeBuffered("claude-opus-5", emptyVisibleOutputPayload(), false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.Content != "final answer" || result.Reasoning.Text != "" {
		t.Fatalf("calls=%d content=%q reasoning=%q", calls, result.Content, result.Reasoning.Text)
	}
}
