package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

// This checker validates public event transitions and reconstructed data, not
// private scorer preferences. Ping and unknown future event types are allowed.
// https://platform.claude.com/docs/en/build-with-claude/streaming
type streamContractView struct {
	text       string
	stopReason string
	toolInput  []map[string]interface{}
}

func inspectAnthropicStreamContract(body string) (streamContractView, error) {
	var view streamContractView
	if !strings.HasSuffix(body, "\n\n") {
		return view, fmt.Errorf("missing final SSE separator")
	}
	started, terminal, stopped := false, false, false
	active, next := -1, 0
	blockType, args := "", ""
	lastOutput := float64(-1)
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(frame) == "" || strings.HasPrefix(frame, ":") {
			continue
		}
		if stopped {
			return view, fmt.Errorf("event after message_stop")
		}
		name, data := "", ""
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data += strings.TrimPrefix(line, "data: ")
			}
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil || event["type"] != name {
			return view, fmt.Errorf("invalid event JSON/type for %q", name)
		}
		// A buffered endpoint may send heartbeat pings before its buffered
		// message_start. Heartbeats are not assistant/message state changes.
		if name == "ping" {
			continue
		}
		if !started && name != "message_start" {
			return view, fmt.Errorf("first event is %q", name)
		}
		switch name {
		case "message_start":
			message, ok := event["message"].(map[string]interface{})
			if started || !ok || message["role"] != "assistant" || message["type"] != "message" {
				return view, fmt.Errorf("invalid/duplicate message_start")
			}
			for _, field := range []string{"id", "model"} {
				if value, ok := message[field].(string); !ok || value == "" {
					return view, fmt.Errorf("message missing %s", field)
				}
			}
			content, ok := message["content"].([]interface{})
			if !ok || len(content) != 0 || message["stop_reason"] != nil {
				return view, fmt.Errorf("nonempty initial content or terminal start")
			}
			usage, ok := message["usage"].(map[string]interface{})
			if !ok {
				return view, fmt.Errorf("missing start usage")
			}
			lastOutput, ok = usage["output_tokens"].(float64)
			if !ok || lastOutput < 0 {
				return view, fmt.Errorf("invalid start output usage")
			}
			started = true
		case "content_block_start":
			index, ok := event["index"].(float64)
			block, blockOK := event["content_block"].(map[string]interface{})
			if active != -1 || terminal || !ok || !blockOK || index != float64(next) {
				return view, fmt.Errorf("overlapping/noncontiguous content block")
			}
			active, next, args = next, next+1, ""
			blockType, _ = block["type"].(string)
			if blockType == "tool_use" || blockType == "server_tool_use" {
				for _, field := range []string{"id", "name"} {
					if value, ok := block[field].(string); !ok || value == "" {
						return view, fmt.Errorf("tool missing %s", field)
					}
				}
				if _, ok := block["input"].(map[string]interface{}); !ok {
					return view, fmt.Errorf("tool initial input is not object")
				}
			}
		case "content_block_delta":
			index, ok := event["index"].(float64)
			delta, deltaOK := event["delta"].(map[string]interface{})
			if active < 0 || !ok || !deltaOK || index != float64(active) {
				return view, fmt.Errorf("delta outside matching block")
			}
			switch delta["type"] {
			case "text_delta":
				if blockType != "text" {
					return view, fmt.Errorf("text delta in %s block", blockType)
				}
				text, ok := delta["text"].(string)
				if !ok {
					return view, fmt.Errorf("text delta is not string")
				}
				view.text += text
			case "input_json_delta":
				if blockType != "tool_use" && blockType != "server_tool_use" {
					return view, fmt.Errorf("tool delta in %s block", blockType)
				}
				partial, ok := delta["partial_json"].(string)
				if !ok {
					return view, fmt.Errorf("partial_json is not string")
				}
				args += partial
			}
		case "content_block_stop":
			if index, ok := event["index"].(float64); active < 0 || !ok || index != float64(active) {
				return view, fmt.Errorf("stop outside matching block")
			}
			if blockType == "tool_use" || blockType == "server_tool_use" {
				input := map[string]interface{}{}
				if args != "" {
					if err := json.Unmarshal([]byte(args), &input); err != nil || input == nil {
						return view, fmt.Errorf("incomplete/nonobject tool input")
					}
				}
				view.toolInput = append(view.toolInput, input)
			}
			active = -1
		case "message_delta":
			if active >= 0 {
				return view, fmt.Errorf("terminal before block closes")
			}
			delta, ok := event["delta"].(map[string]interface{})
			if !ok {
				return view, fmt.Errorf("missing message delta")
			}
			if reason, ok := delta["stop_reason"].(string); ok {
				view.stopReason = reason
			}
			if usage, ok := event["usage"].(map[string]interface{}); ok {
				if output, ok := usage["output_tokens"].(float64); ok {
					if output < lastOutput {
						return view, fmt.Errorf("cumulative output usage decreased")
					}
					lastOutput = output
				}
			}
			terminal = true
		case "message_stop":
			if active >= 0 || !terminal || view.stopReason == "" {
				return view, fmt.Errorf("stop before complete terminal delta")
			}
			stopped = true
		case "error":
			return view, fmt.Errorf("unexpected SSE error in successful fixture")
		}
	}
	if !stopped {
		return view, fmt.Errorf("missing message_stop")
	}
	return view, nil
}

func TestAnthropicStreamContractContentAndRetryBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		thinking bool
		omitted  bool
		text     string
		stop     string
		tools    int
		emit     func(*testing.T, *KiroPayload, *KiroStreamCallback) error
	}{
		{
			name: "plain text", text: "Bonjour. Привет. 你好。", stop: "end_turn",
			emit: func(_ *testing.T, _ *KiroPayload, cb *KiroStreamCallback) error {
				cb.OnText("Bonjour. Привет. 你好。", false)
				cb.OnComplete(10, 9)
				return nil
			},
		},
		{
			name: "native reasoning and signature", thinking: true, text: "Final answer.", stop: "end_turn",
			emit: func(_ *testing.T, _ *KiroPayload, cb *KiroStreamCallback) error {
				cb.OnReasoning(KiroReasoningContentEvent{Text: "Consider the problem.", Signature: "opaque-native-signature-fixture"})
				cb.OnText("Final answer.", false)
				cb.OnComplete(10, 9)
				return nil
			},
		},
		{
			name: "omitted reasoning", thinking: true, omitted: true, text: "Final answer.", stop: "end_turn",
			emit: func(_ *testing.T, _ *KiroPayload, cb *KiroStreamCallback) error {
				cb.OnReasoning(KiroReasoningContentEvent{Text: "Private reasoning must stay omitted.", Signature: "opaque-native-signature-fixture"})
				cb.OnText("Final answer.", false)
				cb.OnComplete(10, 9)
				return nil
			},
		},
		{
			name: "interleaved AWS tool fragments", stop: "tool_use", tools: 2,
			emit: func(t *testing.T, _ *KiroPayload, cb *KiroStreamCallback) error {
				frames := [][]byte{
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "toolu_first", "name": "lookup", "input": `{"q":`}),
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "toolu_second", "name": "lookup", "input": `{"q":"second"}`, "stop": true}),
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "toolu_first", "name": "lookup", "input": `"first"}`, "stop": true}),
				}
				return parseEventStream(bytes.NewReader(bytes.Join(frames, nil)), cb)
			},
		},
		{
			name: "split stop sequence", text: "α", stop: "stop_sequence",
			emit: func(_ *testing.T, payload *KiroPayload, cb *KiroStreamCallback) error {
				payload.StopSequences = []string{"<STOP>"}
				cb.OnText("α<ST", false)
				cb.OnText("OP>must not escape", false)
				cb.OnComplete(10, 9)
				return nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, payload *KiroPayload, cb *KiroStreamCallback) error {
				return test.emit(t, payload, cb)
			})
			payload := emptyVisibleOutputPayload()
			if test.stop == "stop_sequence" {
				payload.StopSequences = []string{"<STOP>"}
			}
			rec := httptest.NewRecorder()
			h.handleClaudeStream(rec, payload, "claude-opus-5-5", "claude-opus-5-5", test.thinking,
				claudeThinkingResponseOptions{Format: "thinking", OmitDisplay: test.omitted}, 10, nil, "")
			view, err := inspectAnthropicStreamContract(rec.Body.String())
			if rec.Code != http.StatusOK || err != nil || view.text != test.text || view.stopReason != test.stop || len(view.toolInput) != test.tools {
				t.Fatalf("status=%d contract=%v view=%+v body=%s", rec.Code, err, view, rec.Body.String())
			}
			if test.tools == 2 && (view.toolInput[0]["q"] != "first" || view.toolInput[1]["q"] != "second") {
				t.Fatalf("tool argument reconstruction=%#v", view.toolInput)
			}
			if test.omitted && strings.Contains(rec.Body.String(), "Private reasoning") {
				t.Fatal("omitted reasoning reached downstream")
			}
		})
	}
}

func TestAnthropicStreamContractPreOutputRetryHasOneEnvelope(t *testing.T) {
	t.Setenv("KIRO_EARLY_STREAM_PRELUDE", "true")
	calls := 0
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
		calls++
		cb.OnStreamReady()
		if calls == 1 {
			return &KiroStreamProtocolError{Code: "upstream_tool_json_error", Err: errors.New("truncated arguments before output")}
		}
		cb.OnText("Recovered intact.", false)
		cb.OnComplete(10, 5)
		return nil
	})
	rec := httptest.NewRecorder()
	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5-5", "claude-opus-5-5", false, claudeThinkingResponseOptions{}, 10, nil, "")
	view, err := inspectAnthropicStreamContract(rec.Body.String())
	if err != nil || calls != 2 || view.text != "Recovered intact." || strings.Count(rec.Body.String(), "event: message_start\n") != 1 {
		t.Fatalf("calls=%d contract=%v body=%s", calls, err, rec.Body.String())
	}
}

func TestAnthropicStreamContractFailureNeverAdvertisesSuccess(t *testing.T) {
	h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
		cb.OnText("Partial output.", false)
		return &KiroStreamProtocolError{Code: "upstream_event_error", Err: errors.New("broken event frame")}
	})
	rec := httptest.NewRecorder()
	h.handleClaudeStream(rec, emptyVisibleOutputPayload(), "claude-opus-5-5", "claude-opus-5-5", false, claudeThinkingResponseOptions{}, 10, nil, "")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "event: error\n") || strings.Contains(body, "event: message_stop\n") || strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("failed stream fabricated successful completion: status=%d body=%s", rec.Code, body)
	}
	if _, err := inspectAnthropicStreamContract(body); err == nil {
		t.Fatal("contract checker accepted errored/incomplete stream")
	}
}

func TestAnthropicStreamContractPreservesTerminalReason(t *testing.T) {
	for _, route := range []string{"/v1/messages", "/cc/v1/messages"} {
		for _, reason := range []string{"refusal", "pause_turn", " REFUSAL ", "PAUSE_TURN"} {
			t.Run(route+"/"+reason, func(t *testing.T) {
				calls := 0
				h := newEmptyVisibleOutputTestHandler(t, 1, func(_ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
					calls++
					cb.OnText("Real upstream terminal content.", false)
					cb.OnStopReason(reason)
					cb.OnComplete(10, 8)
					return nil
				})
				req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Read the request."}]}`))
				rec := httptest.NewRecorder()
				h.handleClaudeMessagesInternal(rec, req)
				view, err := inspectAnthropicStreamContract(rec.Body.String())
				if rec.Code != http.StatusOK || err != nil {
					t.Fatalf("status=%d protocol=%v body=%s", rec.Code, err, rec.Body.String())
				}
				want := strings.ToLower(strings.TrimSpace(reason))
				if calls != 1 || view.text != "Real upstream terminal content." || view.stopReason != want {
					t.Fatalf("calls=%d text=%q stop_reason=%q want=%q", calls, view.text, view.stopReason, want)
				}
			})
		}
	}
}
