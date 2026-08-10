package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestClaudeToKiroPreservesOversizedHistory verifies that translation itself is
// lossless. The transport preflight applies model/raw limits immediately before
// an upstream attempt, after every recovery mutation.
func TestClaudeToKiroPreservesOversizedHistory(t *testing.T) {
	// ~2KB chunk repeated across many turns to exceed the former 900 KiB cap.
	big := strings.Repeat("lorem ipsum dolor sit amet ", 80) // ~2.1KB

	msgs := []ClaudeMessage{
		{Role: "user", Content: "start the long task"},
	}
	for i := 0; i < 800; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "step result: " + big},
			ClaudeMessage{Role: "user", Content: "next: " + big},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL: summarize everything above"})

	req := &ClaudeRequest{
		Model:    "claude-opus-4.8",
		System:   "You are a helpful assistant.",
		Messages: msgs,
	}

	payload := ClaudeToKiro(req, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if len(raw) <= 900*1024 {
		t.Fatalf("test payload size %d did not exceed the former 900 KiB cap", len(raw))
	}

	// The current message must be preserved.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "FINAL: summarize everything above") {
		t.Fatalf("current message lost after truncation, got %q", cur.Content[:min(80, len(cur.Content))])
	}

	// kiro.rs closes a trailing historical user turn with the same neutral
	// assistant acknowledgement it sends before the current message.
	if got, want := len(payload.ConversationState.History), len(msgs)+2; got != want {
		t.Fatalf("history entries=%d, want %d; input was dropped", got, want)
	}
	foundFirstUser := false
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && h.UserInputMessage.Content == "start the long task" {
			foundFirstUser = true
			break
		}
	}
	if !foundFirstUser {
		t.Fatal("oldest user turn was not preserved")
	}

	// System priming should still be at the front.
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected priming retained, history too short")
	}
	primingUser := payload.ConversationState.History[0].UserInputMessage
	if primingUser == nil || !strings.Contains(primingUser.Content, "helpful assistant") {
		t.Fatalf("expected system priming retained at front")
	}
}

func TestClaudeToKiroPreservesSmallPayload(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-opus-4.8",
		System: "You are helpful.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "how are you?"},
		},
	}
	payload := ClaudeToKiro(req, false)
	if got, want := len(payload.ConversationState.History), 4; got != want {
		t.Fatalf("history entries=%d, want %d", got, want)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
