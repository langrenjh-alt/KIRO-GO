package proxy

import (
	"net/http/httptest"
	"testing"
)

func TestClaudeThinkingSignature(t *testing.T) {
	if got := claudeThinkingSignature(" real-signature "); got != "real-signature" {
		t.Fatalf("real signature = %q", got)
	}
	if got := claudeThinkingSignature(""); got != claudeThinkingSignaturePlaceholder {
		t.Fatalf("placeholder signature = %q", got)
	}
}

func TestResolveClaudeStopReason(t *testing.T) {
	tests := []struct {
		name            string
		toolUseCount    int
		contextExceeded bool
		upstream        string
		want            string
	}{
		{name: "end turn", want: "end_turn"},
		{name: "max tokens", upstream: "max_tokens", want: "max_tokens"},
		{name: "context window", contextExceeded: true, upstream: "max_tokens", want: "model_context_window_exceeded"},
		{name: "upstream stop overrides tool", toolUseCount: 1, upstream: "max_tokens", want: "max_tokens"},
		{name: "tool use", toolUseCount: 1, want: "tool_use"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveClaudeStopReason(tc.toolUseCount, tc.contextExceeded, tc.upstream)
			if got != tc.want {
				t.Fatalf("stop reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClaudeOutputTokenEstimateCountsRedactedThinking(t *testing.T) {
	base := estimateClaudeOutputTokensWithRedacted("answer", "", nil, nil)
	withRedacted := estimateClaudeOutputTokensWithRedacted("answer", "", []string{"encrypted reasoning payload"}, nil)
	if withRedacted <= base {
		t.Fatalf("redacted thinking was not counted: base=%d with=%d", base, withRedacted)
	}
}

func TestClaudeStreamTagThinkingOnlyEmitsFallbackText(t *testing.T) {
	h := newResponseContractHandler(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "<thinking>private reasoning</thinking>",
	}))
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "test",
		ModelID: "claude-sonnet-4.6",
		Origin:  "AI_EDITOR",
	}
	rec := httptest.NewRecorder()

	h.handleClaudeStream(
		rec,
		payload,
		"claude-sonnet-4.6",
		"claude-sonnet-4.6",
		true,
		claudeThinkingResponseOptions{Format: "thinking"},
		1,
		nil,
		"",
	)

	events := decodeResponseContractSSE(t, rec.Body.String())
	var visibleText, stopReason string
	for _, event := range events {
		switch event.Name {
		case "content_block_delta":
			delta, _ := event.Data["delta"].(map[string]interface{})
			if delta["type"] == "text_delta" {
				visibleText += delta["text"].(string)
			}
		case "message_delta":
			delta, _ := event.Data["delta"].(map[string]interface{})
			stopReason, _ = delta["stop_reason"].(string)
		}
	}
	if visibleText != " " || stopReason != "max_tokens" {
		t.Fatalf("visible text=%q stop_reason=%q\n%s", visibleText, stopReason, rec.Body.String())
	}
}
