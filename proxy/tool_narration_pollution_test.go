package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// TestNoToolInvocationTextInAssistantHistory is a regression guard for the
// few-shot pollution bug: when historical tool calls were narrated as
// "[Called tool X with input ...]" inside assistant turns, the model learned to
// emit that literal text instead of issuing real structured tool calls.
//
// After the fix, assistant history turns must never contain tool-invocation
// syntax. Tool identity is attributed only on the user "Tool results" side.
func TestNoToolInvocationTextInAssistantHistory(t *testing.T) {
	// Build a long OpenAI conversation with many completed tool cycles.
	msgs := []OpenAIMessage{{Role: "user", Content: "start a multi-step task"}}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			OpenAIMessage{Role: "assistant", Content: "", ToolCalls: []ToolCall{
				newPollToolCall(fmt.Sprintf("call_%d", i), "exec_command", fmt.Sprintf(`{"cmd":"step %d"}`, i)),
			}},
			OpenAIMessage{Role: "tool", ToolCallID: fmt.Sprintf("call_%d", i), Content: fmt.Sprintf("OUTPUT_%d", i)},
			OpenAIMessage{Role: "user", Content: fmt.Sprintf("continue %d", i)},
		)
	}
	msgs = append(msgs, OpenAIMessage{Role: "user", Content: "summarize"})

	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil {
			continue
		}
		// No assistant turn may contain tool-invocation-looking text.
		for _, bad := range []string{"[Called tool", "Called tool ", "with input {"} {
			if strings.Contains(a.Content, bad) {
				t.Fatalf("history[%d] assistant content contains mimicable tool text %q: %q", i, bad, a.Content)
			}
		}
		// No assistant turn may carry structured tool calls (rejected upstream).
		if len(a.ToolUses) > 0 {
			t.Fatalf("history[%d] assistant retains %d structured toolUses", i, len(a.ToolUses))
		}
	}

	// Tool outputs must still be preserved (on the user side) for context.
	var allText strings.Builder
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil {
			allText.WriteString(h.UserInputMessage.Content)
			allText.WriteString("\n")
		}
	}
	combined := allText.String()
	for i := 0; i < 8; i++ {
		marker := fmt.Sprintf("OUTPUT_%d", i)
		if !strings.Contains(combined, marker) {
			t.Fatalf("tool output %q lost from history", marker)
		}
	}
	// Tool identity should be attributed inside a data-only result envelope.
	if !strings.Contains(combined, `<tool_result name="exec_command">`) {
		t.Fatalf("expected tool results attributed to exec_command on the user side")
	}
	if strings.Contains(combined, "Tool results:") {
		t.Fatalf("history retained the mimicable legacy tool-results carrier: %q", combined)
	}
}

func newPollToolCall(id, name, args string) ToolCall {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// TestCollapsesConsecutiveIdenticalToolResults covers a client retry loop that
// sends the same failing tool result many times. After hollow assistant turns
// are dropped, those identical user "Tool results" turns become adjacent
// duplicates; the proxy collapses each run to a single copy.
func TestCollapsesConsecutiveIdenticalToolResults(t *testing.T) {
	msgs := []OpenAIMessage{{Role: "user", Content: "start"}}
	// 5 identical failing cycles in a row (model retrying the same tool).
	for i := 0; i < 5; i++ {
		msgs = append(msgs,
			OpenAIMessage{Role: "assistant", Content: "", ToolCalls: []ToolCall{
				newPollToolCall(fmt.Sprintf("c%d", i), "exec_command", `{"cmd":"x"}`),
			}},
			OpenAIMessage{Role: "tool", ToolCallID: fmt.Sprintf("c%d", i), Content: "SAME_ERROR_OUTPUT"},
		)
	}
	msgs = append(msgs, OpenAIMessage{Role: "user", Content: "final"})

	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	var combined strings.Builder
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil {
			combined.WriteString(h.UserInputMessage.Content)
			combined.WriteByte('\n')
		}
	}
	count := strings.Count(combined.String(), "SAME_ERROR_OUTPUT")
	if count != 1 {
		t.Fatalf("expected 5 identical tool results collapsed to 1 occurrence, got %d in %q", count, combined.String())
	}
}

// TestMergesUserTurnsAfterToolHistoryScrub ensures that removing historical
// assistant toolUses does not leave an invalid run of consecutive user turns.
// The tool output remains in the merged user content, while the assistant-side
// invocation syntax stays absent so the model cannot imitate it.
func TestMergesUserTurnsAfterToolHistoryScrub(t *testing.T) {
	msgs := []OpenAIMessage{{Role: "user", Content: "start"}}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			OpenAIMessage{Role: "assistant", ToolCalls: []ToolCall{
				newPollToolCall(id, "exec_command", fmt.Sprintf(`{"cmd":"step %d"}`, i)),
			}},
			OpenAIMessage{Role: "tool", ToolCallID: id, Content: fmt.Sprintf("OUTPUT_%d", i)},
			OpenAIMessage{Role: "user", Content: fmt.Sprintf("continue %d", i)},
		)
	}
	msgs = append(msgs, OpenAIMessage{Role: "user", Content: "final"})

	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: msgs}, false)
	history := payload.ConversationState.History
	for i := 1; i < len(history); i++ {
		if history[i-1].UserInputMessage != nil && history[i].UserInputMessage != nil {
			t.Fatalf("history contains consecutive user turns at %d/%d: %#v / %#v", i-1, i, history[i-1], history[i])
		}
	}

	var merged strings.Builder
	for _, h := range history {
		if h.UserInputMessage != nil {
			merged.WriteString(h.UserInputMessage.Content)
			merged.WriteByte('\n')
		}
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(merged.String(), fmt.Sprintf("OUTPUT_%d", i)) {
			t.Fatalf("merged history lost OUTPUT_%d: %q", i, merged.String())
		}
	}
	for _, h := range history {
		if a := h.AssistantResponseMessage; a != nil && len(a.ToolUses) > 0 {
			t.Fatalf("historical assistant retained structured toolUses: %#v", a.ToolUses)
		}
	}
}

func TestClaudeMergesUserTurnsAfterToolHistoryScrub(t *testing.T) {
	msgs := []ClaudeMessage{{Role: "user", Content: "start"}}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("toolu_%d", i)
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: []interface{}{
				map[string]interface{}{
					"type": "tool_use", "id": id, "name": "exec_command",
					"input": map[string]interface{}{"cmd": fmt.Sprintf("step %d", i)},
				},
			}},
			ClaudeMessage{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type": "tool_result", "tool_use_id": id,
					"content": fmt.Sprintf("CLAUDE_OUTPUT_%d", i),
				},
			}},
			ClaudeMessage{Role: "user", Content: fmt.Sprintf("continue %d", i)},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "final"})

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)
	history := payload.ConversationState.History
	var merged strings.Builder
	for i, h := range history {
		if i > 0 && history[i-1].UserInputMessage != nil && h.UserInputMessage != nil {
			t.Fatalf("history contains consecutive user turns at %d/%d", i-1, i)
		}
		if h.UserInputMessage != nil {
			merged.WriteString(h.UserInputMessage.Content)
			merged.WriteByte('\n')
		}
		if a := h.AssistantResponseMessage; a != nil && len(a.ToolUses) > 0 {
			t.Fatalf("historical assistant retained structured toolUses: %#v", a.ToolUses)
		}
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(merged.String(), fmt.Sprintf("CLAUDE_OUTPUT_%d", i)) {
			t.Fatalf("merged Claude history lost output %d: %q", i, merged.String())
		}
	}
}

// TestDropsDotPollutedAssistantTurns covers the second-order pollution: after
// stripping "[Called tool ...]" from assistant turns that held only that text,
// the turns become empty and must NOT be backfilled with ".". A history full of
// "." assistant turns trains the model to reply ".". Such hollow turns are
// dropped instead.
func TestDropsDotPollutedAssistantTurns(t *testing.T) {
	msgs := []ClaudeMessage{{Role: "user", Content: "start"}}
	for i := 0; i < 6; i++ {
		// Assistant turn that is pure replayed tool-call text (becomes empty after scrub).
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "[Called tool exec_command with input {\"cmd\":\"x\"}]"},
			ClaudeMessage{Role: "user", Content: "continue"},
		)
		// Also a turn that is already a bare "." (client-replayed prior placeholder).
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "."},
			ClaudeMessage{Role: "user", Content: "go on"},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "final question"})

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil {
			continue
		}
		c := strings.TrimSpace(a.Content)
		if c == "." || c == "" {
			t.Fatalf("history[%d] is a hollow/dot assistant turn that should have been dropped", i)
		}
		if strings.Contains(a.Content, "[Called tool") {
			t.Fatalf("history[%d] still contains replayed tool-call text", i)
		}
	}
}

// TestScrubsClientReplayedToolCallText covers the recovery path: a polluted
// client stored the model's "[Called tool ...]" text output as assistant
// history and replays it. The proxy must strip that text from assistant turns
// so the pattern is not reinforced.
func TestScrubsClientReplayedToolCallText(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "do the task"},
			// Assistant text the client captured from the model's polluted output.
			{Role: "assistant", Content: "Let me check.\n\n[Called tool exec_command with input {\"cmd\":\"pwd\"}]"},
			{Role: "user", Content: "continue"},
			{Role: "assistant", Content: "[Called tool exec_command with input {\"cmd\":\"ls\"}]"},
			{Role: "user", Content: "continue"},
		},
	}

	payload := ClaudeToKiro(req, false)

	for i, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			if strings.Contains(a.Content, "[Called tool") {
				t.Fatalf("history[%d] still contains replayed tool-call text: %q", i, a.Content)
			}
		}
	}

	// The natural prose around the stripped marker must be preserved.
	var combined strings.Builder
	for _, h := range payload.ConversationState.History {
		if h.AssistantResponseMessage != nil {
			combined.WriteString(h.AssistantResponseMessage.Content)
			combined.WriteString("\n")
		}
	}
	if !strings.Contains(combined.String(), "Let me check.") {
		t.Fatalf("expected surrounding assistant prose to survive scrubbing, got:\n%s", combined.String())
	}
}

func TestScrubsClientReplayedRoleToolTranscript(t *testing.T) {
	polluted := "Now let me check misc.js against the CSS.\n" +
		"user Tool results:\n" +
		"[Bash] CSS 缺失 js\\core\\ui.js:\ncls\nCSS 缺失 js\\apps\\misc.js:\nmsg-bubble msg-day\n" +
		"assistant\n" +
		"As expected - the Messages classes need styles. Let me add them."

	cleaned := stripPollutedToolCallText(polluted)
	for _, want := range []string{
		"Now let me check misc.js against the CSS.",
		"As expected - the Messages classes need styles. Let me add them.",
	} {
		if !strings.Contains(cleaned, want) {
			t.Fatalf("surrounding narration %q was lost: %q", want, cleaned)
		}
	}
	for _, leaked := range []string{"user Tool results:", "[Bash]", "CSS 缺失", "\nassistant\n"} {
		if strings.Contains(cleaned, leaked) {
			t.Fatalf("pseudo tool transcript leaked %q: %q", leaked, cleaned)
		}
	}
}

func TestAgentCompletionToolCommitScrubsPseudoTranscript(t *testing.T) {
	marker := "<agent_final_0123456789abcdef>"
	gate := newAgentCompletionGate(marker)
	polluted := "Now let me inspect the stylesheet.\n" +
		"user Tool results:\n[Bash] copied tool output\nassistant\n" +
		"I found the missing styles. Let me edit them."
	streamed := gate.Push(polluted)
	if !strings.Contains(streamed, "Now let me inspect the stylesheet.") ||
		!strings.Contains(streamed, "I found the missing styles. Let me edit them.") {
		t.Fatalf("ordinary pre-tool narration did not stream: %q", streamed)
	}
	if strings.Contains(streamed, "Tool results:") || strings.Contains(streamed, "[Bash]") {
		t.Fatalf("pseudo tool transcript leaked into the streamed narration: %q", streamed)
	}
	committed := gate.CommitPendingForTool()
	if committed != "" {
		t.Fatalf("already streamed narration was replayed before the tool stream: %q", committed)
	}
	if !strings.Contains(gate.VisibleText(), "Tool results:") {
		t.Fatalf("terminal evidence lost the original unmarked response: %q", gate.VisibleText())
	}
}

func TestAgentCompletionFilterOnlyScrubsWhenToolCallExists(t *testing.T) {
	marker := "<agent_final_0123456789abcdef>"
	payload := &KiroPayload{AgentCompletionMarker: marker}
	sample := "A transcript example:\nuser Tool results:\nexample output\nassistant\nend of example"

	final, marked := filterAgentCompletionContent(payload, marker+"\n"+sample, 0)
	if !marked || final != sample {
		t.Fatalf("tool-free final answer was unexpectedly rewritten: marked=%v content=%q", marked, final)
	}

	toolNarration, _ := filterAgentCompletionContent(payload, sample, 1)
	if strings.Contains(toolNarration, "Tool results:") || strings.Contains(toolNarration, "example output") {
		t.Fatalf("non-stream tool response retained pseudo transcript: %q", toolNarration)
	}
	if !strings.Contains(toolNarration, "A transcript example:") || !strings.Contains(toolNarration, "end of example") {
		t.Fatalf("non-stream tool response lost surrounding narration: %q", toolNarration)
	}
}
