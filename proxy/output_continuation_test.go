package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestClaudeNonStreamMaxTokensContinuesFromNativeAssistantHistory(t *testing.T) {
	payload := completionProtocolTestPayload()
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, attempt *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		switch calls {
		case 1:
			callback.OnText("prefix-", false)
			callback.OnStopReason("MAX_TOKENS")
		case 2:
			if attempt.OutputContinuations != 1 {
				t.Fatalf("continuations=%d, want 1", attempt.OutputContinuations)
			}
			history := attempt.ConversationState.History
			if len(history) < 2 || history[len(history)-1].AssistantResponseMessage == nil ||
				history[len(history)-1].AssistantResponseMessage.Content != "prefix-" {
				t.Fatalf("partial output was not replayed as assistant history: %+v", history)
			}
			if !strings.Contains(attempt.ConversationState.CurrentMessage.UserInputMessage.Content, "<output_continuation>") {
				t.Fatalf("continuation instruction missing: %q", attempt.ConversationState.CurrentMessage.UserInputMessage.Content)
			}
			callback.OnText(attempt.AgentCompletionMarker+"suffix", false)
			callback.OnStopReason("END_TURN")
		default:
			t.Fatalf("unexpected call %d", calls)
		}
		callback.OnComplete(10, 3)
		return nil
	})
	recorder := httptest.NewRecorder()

	h.handleClaudeNonStream(recorder, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	body := recorder.Body.String()
	if calls != 2 || recorder.Code != 200 || !strings.Contains(body, "prefix-suffix") {
		t.Fatalf("calls=%d status=%d body=%s", calls, recorder.Code, body)
	}
	if strings.Contains(body, "<agent_final_") || strings.Contains(body, "prefix-prefix-") {
		t.Fatalf("continuation marker or duplicated prefix leaked: %s", body)
	}
	var response ClaudeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	if response.Usage.CacheReadInputTokens != 13 {
		t.Fatalf("continuation retry usage was not retained as input: %+v", response.Usage)
	}
}

func TestClaudeStreamMaxTokensContinuesWithoutDuplicatingPrefix(t *testing.T) {
	payload := completionProtocolTestPayload()
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, attempt *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			callback.OnText("stream-prefix-", false)
			callback.OnStopReason("MAX_TOKENS")
		} else if calls == 2 {
			callback.OnText(attempt.AgentCompletionMarker+"stream-suffix", false)
			callback.OnStopReason("END_TURN")
		} else {
			t.Fatalf("unexpected call %d", calls)
		}
		callback.OnComplete(10, 3)
		return nil
	})
	recorder := httptest.NewRecorder()

	h.handleClaudeStream(recorder, payload, "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	body := recorder.Body.String()
	if calls != 2 || recorder.Code != 200 || strings.Count(body, "stream-prefix-") != 1 ||
		strings.Count(body, "stream-suffix") != 1 || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("calls=%d status=%d body=%s", calls, recorder.Code, body)
	}
	if strings.Contains(body, "<agent_final_") || strings.Contains(body, "event: error") {
		t.Fatalf("continuation protocol leaked or failed: %s", body)
	}
	var terminalUsage map[string]interface{}
	for _, event := range decodeResponseContractSSE(t, body) {
		if event.Name == "message_delta" {
			terminalUsage, _ = event.Data["usage"].(map[string]interface{})
		}
	}
	if terminalUsage == nil {
		t.Fatalf("stream terminal usage missing: %s", body)
	}
	wantOutput := estimateClaudeOutputTokensWithRedacted("stream-prefix-stream-suffix", "", nil, nil)
	if got := int(terminalUsage["output_tokens"].(float64)); got != wantOutput {
		t.Fatalf("stream continuation output_tokens=%d, want visible output %d: usage=%#v", got, wantOutput, terminalUsage)
	}
}

func TestClaudeStreamTransportDisconnectContinuesWithoutDuplicatingPrefix(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, attempt *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		switch calls {
		case 1:
			callback.OnText("disconnect-prefix-", false)
			return io.ErrUnexpectedEOF
		case 2:
			if attempt.OutputContinuations != 1 {
				t.Fatalf("continuations=%d, want 1", attempt.OutputContinuations)
			}
			history := attempt.ConversationState.History
			if len(history) < 2 || history[len(history)-1].AssistantResponseMessage == nil ||
				history[len(history)-1].AssistantResponseMessage.Content != "disconnect-prefix-" {
				t.Fatalf("partial output was not replayed as assistant history: %+v", history)
			}
			if !strings.Contains(attempt.ConversationState.CurrentMessage.UserInputMessage.Content, "<output_continuation>") {
				t.Fatalf("continuation instruction missing: %q", attempt.ConversationState.CurrentMessage.UserInputMessage.Content)
			}
			callback.OnText("disconnect-suffix", false)
			callback.OnStopReason("END_TURN")
		default:
			t.Fatalf("unexpected call %d", calls)
		}
		callback.OnComplete(10, 3)
		return nil
	})
	recorder := httptest.NewRecorder()

	h.handleClaudeStream(recorder, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	body := recorder.Body.String()
	if calls != 2 || recorder.Code != 200 || strings.Count(body, "disconnect-prefix-") != 1 ||
		strings.Count(body, "disconnect-suffix") != 1 || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("calls=%d status=%d body=%s", calls, recorder.Code, body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("transport recovery surfaced an SSE error: %s", body)
	}
}

func TestClaudeNonStreamTransportDisconnectContinuesWithoutDuplicatingPrefix(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, attempt *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			callback.OnText("nonstream-prefix-", false)
			return io.ErrUnexpectedEOF
		}
		if attempt.OutputContinuations != 1 {
			t.Fatalf("continuations=%d, want 1", attempt.OutputContinuations)
		}
		callback.OnText("nonstream-suffix", false)
		callback.OnStopReason("END_TURN")
		callback.OnComplete(10, 3)
		return nil
	})
	recorder := httptest.NewRecorder()

	h.handleClaudeNonStream(recorder, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	body := recorder.Body.String()
	if calls != 2 || recorder.Code != 200 || strings.Count(body, "nonstream-prefix-") != 1 ||
		strings.Count(body, "nonstream-suffix") != 1 || strings.Contains(body, "api_error") {
		t.Fatalf("calls=%d status=%d body=%s", calls, recorder.Code, body)
	}
}

func TestClaudeStreamHTTP500AfterVisiblePrefixContinues(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, attempt *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			callback.OnText("http-prefix-", false)
			return &KiroHTTPError{StatusCode: http.StatusInternalServerError, Endpoint: "test", Body: "temporary"}
		}
		if attempt.OutputContinuations != 1 {
			t.Fatalf("continuations=%d, want 1", attempt.OutputContinuations)
		}
		callback.OnText("http-suffix", false)
		callback.OnStopReason("END_TURN")
		callback.OnComplete(10, 3)
		return nil
	})
	recorder := httptest.NewRecorder()

	h.handleClaudeStream(recorder, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	body := recorder.Body.String()
	if calls != 2 || recorder.Code != http.StatusOK || strings.Count(body, "http-prefix-") != 1 ||
		strings.Count(body, "http-suffix") != 1 || strings.Contains(body, "event: error") {
		t.Fatalf("calls=%d status=%d body=%s", calls, recorder.Code, body)
	}
}

func TestClaudeStreamTransportContinuationIsBounded(t *testing.T) {
	var calls int
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		callback.OnText("partial-"+string(rune('0'+calls)), false)
		return io.ErrUnexpectedEOF
	})
	recorder := httptest.NewRecorder()

	h.handleClaudeStream(recorder, emptyVisibleOutputPayload(), "claude-opus-5", "claude-opus-5", false, claudeThinkingResponseOptions{}, 5, nil, "")

	body := recorder.Body.String()
	if calls != maxKiroOutputContinuations+1 {
		t.Fatalf("calls=%d, want bounded %d", calls, maxKiroOutputContinuations+1)
	}
	if !strings.Contains(body, "event: error") || strings.Contains(body, "event: message_stop") {
		t.Fatalf("bounded transport failure did not retain the terminal SSE error: %s", body)
	}
}

func TestOutputContinuationOnlyAcceptsTransientMidStreamFailures(t *testing.T) {
	if !isKiroOutputContinuationError(errors.New("read Kiro event stream prelude: stream error: INTERNAL_ERROR; received from peer")) {
		t.Fatal("HTTP/2 peer reset was not classified as resumable")
	}
	if isKiroOutputContinuationError(newUpstreamAgentMarkerMismatchError()) {
		t.Fatal("agent completion mismatch must use semantic recovery")
	}
	if isKiroOutputContinuationError(&KiroStreamProtocolError{Code: "upstream_tool_json_error", Err: errors.New("unexpected EOF")}) {
		t.Fatal("partial tool JSON must not be replayed as assistant text")
	}
}

func TestMaxTokensContinuationIsBoundedAndRejectsToolTerminals(t *testing.T) {
	payload := completionProtocolTestPayload()
	for i := 0; i < maxKiroOutputContinuations; i++ {
		payload.outputContinuationAttemptText = "segment"
		err := resolveKiroOutputTruncation(payload, newUpstreamOutputTruncatedError(), "max_tokens", "", nil)
		if !isUpstreamOutputTruncatedError(err) || !takeKiroOutputContinuationRetry(payload, err) {
			t.Fatalf("continuation %d was not prepared: %v", i+1, err)
		}
	}
	payload.outputContinuationAttemptText = "final truncated segment"
	err := resolveKiroOutputTruncation(payload, newUpstreamOutputTruncatedError(), "max_tokens", "", nil)
	if !isUpstreamAgentMarkerMismatchError(err) || payload.OutputContinuations != maxKiroOutputContinuations {
		t.Fatalf("bounded continuation did not fail closed: continuations=%d err=%v", payload.OutputContinuations, err)
	}

	toolPayload := completionProtocolTestPayload()
	toolPayload.outputContinuationAttemptText = "partial"
	err = resolveKiroOutputTruncation(toolPayload, newUpstreamOutputTruncatedError(), "max_tokens", "", []string{"shell_command"})
	if !isUpstreamAgentMarkerMismatchError(err) || toolPayload.OutputContinuations != 0 {
		t.Fatalf("MAX_TOKENS tool terminal was continued: continuations=%d err=%v", toolPayload.OutputContinuations, err)
	}
}

func TestContinuationRetrySeparatesReplayedPrefixFromFinalAttemptUsage(t *testing.T) {
	var usage retryBillingUsage
	usage.add(newDiscardedAttemptUsage(10, 7, 0))
	usage.markLastContinuationAttemptOutput()
	if got := usage.finalAttemptOutputTokens(12); got != 5 {
		t.Fatalf("final attempt output=%d, want suffix-only 5", got)
	}
	if got := usage.applyOutput(5); got != 5 {
		t.Fatalf("non-stream output reintroduced replayed prefix: %d", got)
	}
	usage.markLastVisibleContinuation()
	if got := usage.applyOutput(5); got != 12 {
		t.Fatalf("stream output=%d, want visible prefix plus suffix 12", got)
	}
}
