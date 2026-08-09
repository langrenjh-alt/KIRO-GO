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
// After the fix, assistant prose must never contain narrated invocation syntax;
// real tool calls remain in Kiro's structured toolUses field.
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
	}

	rounds := collectNativeKiroToolRounds(t, payload)
	if len(rounds) != 8 {
		t.Fatalf("structured tool rounds=%d, want 8", len(rounds))
	}
	for i := 0; i < 8; i++ {
		marker := fmt.Sprintf("OUTPUT_%d", i)
		if got := kiroToolResultText(rounds[i].Results[0]); got != marker {
			t.Fatalf("tool output %d=%q, want %q", i, got, marker)
		}
	}
	if payload.HistoryMode != kiroHistoryModeNativeActive || payload.ConversationState.AgentTaskType != "vibe" {
		t.Fatalf("native agent protocol not selected: mode=%q task=%q", payload.HistoryMode, payload.ConversationState.AgentTaskType)
	}
}

func newPollToolCall(id, name, args string) ToolCall {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

type observedKiroToolRound struct {
	Uses    []KiroToolUse
	Results []KiroToolResult
}

func collectNativeKiroToolRounds(t *testing.T, payload *KiroPayload) []observedKiroToolRound {
	t.Helper()
	var rounds []observedKiroToolRound
	history := payload.ConversationState.History
	for i, message := range history {
		assistant := message.AssistantResponseMessage
		if assistant == nil || len(assistant.ToolUses) == 0 {
			continue
		}
		var results []KiroToolResult
		if i+1 < len(history) {
			next := history[i+1].UserInputMessage
			if next != nil && next.UserInputMessageContext != nil {
				results = next.UserInputMessageContext.ToolResults
			}
		} else if context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext; context != nil {
			results = context.ToolResults
		}
		if !toolResultsExactlyMatchUses(assistant.ToolUses, results) {
			t.Fatalf("history[%d] tool round is not paired: uses=%+v results=%+v", i, assistant.ToolUses, results)
		}
		rounds = append(rounds, observedKiroToolRound{Uses: assistant.ToolUses, Results: results})
	}
	return rounds
}

func kiroToolResultText(result KiroToolResult) string {
	parts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		parts = append(parts, content.Text)
	}
	return strings.Join(parts, "\n")
}

// Even byte-identical tool rounds are causal history and remain distinct.
func TestCollapsesConsecutiveIdenticalToolResults(t *testing.T) {
	msgs := []OpenAIMessage{{Role: "user", Content: "start"}}
	// Five exact replays of one tool cycle.
	for i := 0; i < 5; i++ {
		msgs = append(msgs,
			OpenAIMessage{Role: "assistant", Content: "", ToolCalls: []ToolCall{
				newPollToolCall("same_call", "exec_command", `{"cmd":"x"}`),
			}},
			OpenAIMessage{Role: "tool", ToolCallID: "same_call", Content: "SAME_ERROR_OUTPUT"},
		)
	}
	msgs = append(msgs, OpenAIMessage{Role: "user", Content: "final"})

	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	rounds := collectNativeKiroToolRounds(t, payload)
	if len(rounds) != 5 {
		t.Fatalf("identical tool rounds=%d, want 5", len(rounds))
	}
	for i, round := range rounds {
		if round.Uses[0].ToolUseID != "same_call" || kiroToolResultText(round.Results[0]) != "SAME_ERROR_OUTPUT" {
			t.Fatalf("round %d changed: %+v", i, round)
		}
	}
}

func TestDoesNotCollapseSameToolOutputWithDifferentIDs(t *testing.T) {
	msgs := []OpenAIMessage{
		{Role: "user", Content: "start"},
		{Role: "assistant", ToolCalls: []ToolCall{newPollToolCall("call_a", "exec_command", `{"cmd":"x"}`)}},
		{Role: "tool", ToolCallID: "call_a", Content: "SAME_OUTPUT_DIFFERENT_ID"},
		{Role: "assistant", ToolCalls: []ToolCall{newPollToolCall("call_b", "exec_command", `{"cmd":"x"}`)}},
		{Role: "tool", ToolCallID: "call_b", Content: "SAME_OUTPUT_DIFFERENT_ID"},
		{Role: "user", Content: "final"},
	}

	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: msgs}, false)
	rounds := collectNativeKiroToolRounds(t, payload)
	if len(rounds) != 2 {
		t.Fatalf("tool rounds=%d, want 2", len(rounds))
	}
	for i, id := range []string{"call_a", "call_b"} {
		if rounds[i].Uses[0].ToolUseID != id || rounds[i].Results[0].ToolUseID != id ||
			kiroToolResultText(rounds[i].Results[0]) != "SAME_OUTPUT_DIFFERENT_ID" {
			t.Fatalf("round %d lost ID/output: %+v", i, rounds[i])
		}
	}
}

func TestDoesNotCollapseSameToolIDWithDifferentInputs(t *testing.T) {
	msgs := []OpenAIMessage{
		{Role: "user", Content: "start"},
		{Role: "assistant", ToolCalls: []ToolCall{newPollToolCall("reused_id", "exec_command", `{"cmd":"first"}`)}},
		{Role: "tool", ToolCallID: "reused_id", Content: "SAME_OUTPUT_DIFFERENT_INPUT"},
		{Role: "assistant", ToolCalls: []ToolCall{newPollToolCall("reused_id", "exec_command", `{"cmd":"second"}`)}},
		{Role: "tool", ToolCallID: "reused_id", Content: "SAME_OUTPUT_DIFFERENT_INPUT"},
		{Role: "user", Content: "final"},
	}

	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: msgs}, false)
	rounds := collectNativeKiroToolRounds(t, payload)
	if len(rounds) != 2 {
		t.Fatalf("tool rounds=%d, want 2", len(rounds))
	}
	for i, input := range []string{"first", "second"} {
		if rounds[i].Uses[0].Input["cmd"] != input ||
			kiroToolResultText(rounds[i].Results[0]) != "SAME_OUTPUT_DIFFERENT_INPUT" {
			t.Fatalf("round %d lost input/output: %+v", i, rounds[i])
		}
	}
}

func TestDoesNotCollapseSameToolCycleWithDifferentAssistantReasoning(t *testing.T) {
	toolUse := func(reasoning string) OpenAIMessage {
		return OpenAIMessage{
			Role:             "assistant",
			ReasoningContent: reasoning,
			ToolCalls: []ToolCall{
				newPollToolCall("reused_id", "exec_command", `{"cmd":"x"}`),
			},
		}
	}
	req := &OpenAIRequest{
		Model: "claude-opus-4.8",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "start"},
			toolUse("first rationale"),
			{Role: "tool", ToolCallID: "reused_id", Content: "SAME_OUTPUT_DIFFERENT_REASONING"},
			toolUse("second rationale"),
			{Role: "tool", ToolCallID: "reused_id", Content: "SAME_OUTPUT_DIFFERENT_REASONING"},
			{Role: "user", Content: "final"},
		},
	}

	payload := OpenAIToKiro(req, false)
	rounds := collectNativeKiroToolRounds(t, payload)
	if len(rounds) != 2 {
		t.Fatalf("tool rounds=%d, want 2", len(rounds))
	}
	for i, reasoning := range []string{"first rationale", "second rationale"} {
		if kiroToolResultText(rounds[i].Results[0]) != "SAME_OUTPUT_DIFFERENT_REASONING" {
			t.Fatalf("round %d lost result: %+v", i, rounds[i])
		}
		found := false
		for _, history := range payload.ConversationState.History {
			if assistant := history.AssistantResponseMessage; assistant != nil &&
				len(assistant.ToolUses) > 0 && assistant.ToolUses[0].ToolUseID == "reused_id" &&
				strings.Contains(assistant.Content, reasoning) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("assistant reasoning %q was lost", reasoning)
		}
	}
}

// A separate user continuation is merged into the preceding structured result
// turn without flattening the call/result pair.
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

	rounds := collectNativeKiroToolRounds(t, payload)
	if len(rounds) != 3 {
		t.Fatalf("structured rounds=%d, want 3", len(rounds))
	}
	for i, round := range rounds {
		if round.Uses[0].ToolUseID != fmt.Sprintf("call_%d", i) ||
			kiroToolResultText(round.Results[0]) != fmt.Sprintf("OUTPUT_%d", i) {
			t.Fatalf("round %d changed: %+v", i, round)
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
	for i, h := range history {
		if i > 0 && history[i-1].UserInputMessage != nil && h.UserInputMessage != nil {
			t.Fatalf("history contains consecutive user turns at %d/%d", i-1, i)
		}
	}
	rounds := collectNativeKiroToolRounds(t, payload)
	if len(rounds) != 3 {
		t.Fatalf("structured rounds=%d, want 3", len(rounds))
	}
	for i, round := range rounds {
		if round.Uses[0].ToolUseID != fmt.Sprintf("toolu_%d", i) ||
			kiroToolResultText(round.Results[0]) != fmt.Sprintf("CLAUDE_OUTPUT_%d", i) {
			t.Fatalf("round %d changed: %+v", i, round)
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

func TestStripPollutedToolCallTextPreservesLiteralInvokeXML(t *testing.T) {
	input := "  Protocol example:\n\n\n<invoke name=\"Edit\"><parameter name=\"file_path\">main.go</parameter></invoke>\nKeep this example.  \n"
	if got := stripPollutedToolCallText(input); got != input {
		t.Fatalf("literal invoke XML was rewritten:\nwant: %q\n got: %q", input, got)
	}
}

func TestStripPollutedToolCallTextPreservesUnclosedInvokeXML(t *testing.T) {
	input := "\nPartial response:\n<thinking>retain this reasoning</thinking>\n<invoke name=\"Edit\"><parameter name=\"file_path\">main.go\n"
	if got := stripPollutedToolCallText(input); got != input {
		t.Fatalf("unfinished invoke XML was rewritten:\nwant: %q\n got: %q", input, got)
	}
}

func TestHistoricalCmdUsesNeutralUserEnvelope(t *testing.T) {
	msgs := []OpenAIMessage{
		{Role: "user", Content: "run it"},
		{Role: "assistant", Content: "Preparing the command.", ToolCalls: []ToolCall{
			newPollToolCall("cmd_call", "exec_command", `{"cmd":"go test ./proxy"}`),
		}},
		{Role: "tool", ToolCallID: "cmd_call", Content: "CMD_OUTPUT"},
		{Role: "user", Content: "continue"},
	}
	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	seenOriginalAssistant := false
	for i, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			for _, fabricated := range []string{"I already inspected", "I already ran", "I performed"} {
				if strings.Contains(a.Content, fabricated) {
					t.Fatalf("history[%d] contains fabricated assistant narration %q: %q", i, fabricated, a.Content)
				}
			}
			if a.Content == "Preparing the command." {
				seenOriginalAssistant = true
			}
		}
	}
	if !seenOriginalAssistant {
		t.Fatal("original assistant content was dropped or rewritten")
	}
	rounds := collectNativeKiroToolRounds(t, payload)
	if len(rounds) != 1 {
		t.Fatalf("structured rounds=%d, want 1", len(rounds))
	}
	round := rounds[0]
	if round.Uses[0].ToolUseID != "cmd_call" || round.Uses[0].Name != "execCommand" ||
		round.Uses[0].Input["cmd"] != "go test ./proxy" || kiroToolResultText(round.Results[0]) != "CMD_OUTPUT" {
		t.Fatalf("structured command exchange changed: %+v", round)
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
