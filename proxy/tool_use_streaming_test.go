package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestParseEventStreamEmitsToolFragmentsBeforeValidatedCompletion(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_stream", "name": "shell_command", "input": `{"command":`,
		}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_stream", "name": "shell_command", "input": `"echo ok"}`, "stop": true,
		}),
	}, nil))

	var events []string
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUseStart: func(toolUseID, name string) {
			events = append(events, "start:"+toolUseID+":"+name)
		},
		OnToolUseInputDelta: func(toolUseID, delta string) {
			events = append(events, "delta:"+toolUseID+":"+delta)
		},
		OnToolUse: func(toolUse KiroToolUse) {
			events = append(events, "complete:"+toolUse.ToolUseID+":"+toolUse.Name)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"start:toolu_stream:shell_command",
		`delta:toolu_stream:{"command":`,
		`delta:toolu_stream:"echo ok"}`,
		"complete:toolu_stream:shell_command",
	}
	if len(events) != len(want) {
		t.Fatalf("events=%q, want %q", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("event[%d]=%q, want %q; all=%q", i, events[i], want[i], events)
		}
	}
}

func TestToolArgumentsReachStreamingClientsBeforeToolCompletion(t *testing.T) {
	tests := []struct {
		name            string
		deltaMarker     string
		reasoningMarker string
		textMarker      string
		toolMarker      string
		invoke          func(*Handler, *httptest.ResponseRecorder)
	}{
		{
			name:            "Claude Messages",
			deltaMarker:     `"type":"input_json_delta"`,
			reasoningMarker: "ORDER_REASONING",
			textMarker:      "ORDER_TEXT",
			toolMarker:      `"type":"tool_use"`,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				h.handleClaudeStream(rec, completionProtocolTestPayload(), "claude-opus-5", "claude-opus-5", true, claudeThinkingResponseOptions{Format: "thinking"}, 5, nil, "")
			},
		},
		{
			name:            "Chat Completions",
			deltaMarker:     `"tool_calls"`,
			reasoningMarker: `"reasoning_content":"ORDER_REASONING"`,
			textMarker:      "ORDER_TEXT",
			toolMarker:      `"tool_calls"`,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
					"model":"claude-opus-5",
					"messages":[{"role":"system","content":"You are Codex, a coding agent."},{"role":"user","content":"run the check"}],
					"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
					"reasoning_effort":"high",
					"stream":true
				}`))
				h.handleOpenAIChat(rec, req)
			},
		},
		{
			name:            "Responses",
			deltaMarker:     `"type":"response.function_call_arguments.delta"`,
			reasoningMarker: "ORDER_REASONING",
			textMarker:      "ORDER_TEXT",
			toolMarker:      `"type":"function_call"`,
			invoke: func(h *Handler, rec *httptest.ResponseRecorder) {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
					"model":"claude-opus-5",
					"instructions":"You are Codex, a coding agent.",
					"input":"run the check",
					"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
					"reasoning":{"effort":"high","summary":"auto"},
					"stream":true,
					"store":false
				}`))
				h.handleOpenAIResponses(rec, req)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var rec *httptest.ResponseRecorder
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
				if callback.OnToolUseStart == nil || callback.OnToolUseInputDelta == nil || callback.OnToolUse == nil {
					t.Fatal("streaming tool callbacks were not installed")
				}
				callback.OnReasoning(KiroReasoningContentEvent{Text: "ORDER_REASONING"})
				callback.OnText("ORDER_TEXT", false)
				callback.OnReasoning(KiroReasoningContentEvent{Text: "LATE_REASONING"})
				callback.OnToolUseStart("toolu_stream", "shell_command")
				beforeDeltas := strings.Count(rec.Body.String(), test.deltaMarker)
				callback.OnToolUseInputDelta("toolu_stream", `{"command":`)
				afterFirst := strings.Count(rec.Body.String(), test.deltaMarker)
				if afterFirst != beforeDeltas+1 {
					t.Fatalf("first tool fragment was buffered: before=%d after=%d body=%s", beforeDeltas, afterFirst, rec.Body.String())
				}
				callback.OnToolUseInputDelta("toolu_stream", `"echo ok"}`)
				afterSecond := strings.Count(rec.Body.String(), test.deltaMarker)
				if afterSecond != afterFirst+1 {
					t.Fatalf("second tool fragment was buffered: first=%d second=%d body=%s", afterFirst, afterSecond, rec.Body.String())
				}
				callback.OnToolUse(KiroToolUse{
					ToolUseID: "toolu_stream",
					Name:      "shell_command",
					Input:     map[string]interface{}{"command": "echo ok"},
				})
				callback.OnStopReason("TOOL_USE")
				callback.OnComplete(10, 4)
				return nil
			})
			rec = httptest.NewRecorder()

			test.invoke(h, rec)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			reasoningIndex := strings.Index(body, test.reasoningMarker)
			textIndex := strings.Index(body, test.textMarker)
			toolIndex := strings.Index(body, test.toolMarker)
			if reasoningIndex < 0 || textIndex <= reasoningIndex || toolIndex <= textIndex {
				t.Fatalf("stream order reasoning=%d text=%d tool=%d; body=%s", reasoningIndex, textIndex, toolIndex, body)
			}
			if strings.Contains(body, "LATE_REASONING") {
				t.Fatalf("late reasoning reopened a second streamed thinking block: %s", body)
			}
		})
	}
}

func TestClaudeContinuationSerializesInterleavedToolAndReasoningBlocks(t *testing.T) {
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		calls++
		if calls == 1 {
			callback.OnReasoning(KiroReasoningContentEvent{Text: "first-pass reasoning"})
			callback.OnText("I'll inspect the implementation now.", false)
			callback.OnComplete(10, 4)
			return nil
		}

		callback.OnToolUseStart("toolu_stream", "shell_command")
		callback.OnReasoning(KiroReasoningContentEvent{Text: "recovery reasoning"})
		callback.OnToolUseInputDelta("toolu_stream", `{"command":"echo ok"}`)
		callback.OnToolUse(KiroToolUse{
			ToolUseID: "toolu_stream",
			Name:      "shell_command",
			Input:     map[string]interface{}{"command": "echo ok"},
		})
		callback.OnStopReason("TOOL_USE")
		callback.OnComplete(10, 4)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(
		rec,
		completionProtocolTestPayload(),
		"claude-opus-5",
		"claude-opus-5",
		true,
		claudeThinkingResponseOptions{Format: "thinking"},
		5,
		nil,
		"",
	)

	body := rec.Body.String()
	if calls != 2 || rec.Code != http.StatusOK {
		t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, body)
	}
	if !strings.Contains(body, "first-pass reasoning") || strings.Contains(body, "recovery reasoning") {
		t.Fatalf("original reasoning was lost or recovery reasoning leaked: %s", body)
	}
	if !strings.Contains(body, `"type":"input_json_delta"`) {
		t.Fatalf("streamed tool arguments were not forwarded: %s", body)
	}
	assertClaudeContentBlocksSerialized(t, body)
}

func TestClaudeSerializesParallelToolFragmentsCompletedOutOfOrder(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnToolUseStart("toolu_first", "shell_command")
		callback.OnToolUseInputDelta("toolu_first", `{"command":"first"}`)
		callback.OnToolUseStart("toolu_second", "Read")
		callback.OnToolUseInputDelta("toolu_second", `{"file_path":"second.go"}`)
		callback.OnReasoning(KiroReasoningContentEvent{Text: "reasoning after parallel tools"})
		callback.OnToolUse(KiroToolUse{
			ToolUseID: "toolu_second",
			Name:      "Read",
			Input:     map[string]interface{}{"file_path": "second.go"},
		})
		callback.OnToolUse(KiroToolUse{
			ToolUseID: "toolu_first",
			Name:      "shell_command",
			Input:     map[string]interface{}{"command": "first"},
		})
		callback.OnStopReason("TOOL_USE")
		callback.OnComplete(10, 6)
		return nil
	})
	rec := httptest.NewRecorder()

	h.handleClaudeStream(
		rec,
		completionProtocolTestPayload(),
		"claude-opus-5",
		"claude-opus-5",
		true,
		claudeThinkingResponseOptions{Format: "thinking"},
		5,
		nil,
		"",
	)

	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Count(body, `"type":"tool_use"`) != 2 {
		t.Fatalf("parallel tool calls were not both forwarded: status=%d body=%s", rec.Code, body)
	}
	if !strings.Contains(body, `\"file_path\":\"second.go\"`) || strings.Contains(body, "reasoning after parallel tools") {
		t.Fatalf("deferred tool input was lost or late reasoning leaked: %s", body)
	}
	assertClaudeContentBlocksSerialized(t, body)
}

func assertClaudeContentBlocksSerialized(t *testing.T, body string) {
	t.Helper()
	activeIndex := -1
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			continue
		}
		switch event.Type {
		case "content_block_start":
			if activeIndex >= 0 {
				t.Fatalf("content block %d started while block %d was still open: %s", event.Index, activeIndex, body)
			}
			activeIndex = event.Index
		case "content_block_delta":
			if activeIndex != event.Index {
				t.Fatalf("content block delta index=%d while active index=%d: %s", event.Index, activeIndex, body)
			}
		case "content_block_stop":
			if activeIndex != event.Index {
				t.Fatalf("content block stop index=%d while active index=%d: %s", event.Index, activeIndex, body)
			}
			activeIndex = -1
		}
	}
	if activeIndex >= 0 {
		t.Fatalf("content block %d remained open: %s", activeIndex, body)
	}
}
