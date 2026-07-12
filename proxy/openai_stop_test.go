package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveOpenAIStopSequences(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value interface{}
		want  []string
	}{
		{name: "single", value: "<STOP>", want: []string{"<STOP>"}},
		{name: "array", value: []interface{}{"one", "two"}, want: []string{"one", "two"}},
		{name: "raw", value: json.RawMessage(`["one","two"]`), want: []string{"one", "two"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOpenAIStopSequences(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("stop sequences = %#v, want %#v", got, tc.want)
			}
		})
	}

	for _, value := range []interface{}{
		"",
		[]interface{}{"ok", 1.0},
		[]interface{}{"one", "two", "three", "four", "five"},
	} {
		if _, err := resolveOpenAIStopSequences(value); err == nil {
			t.Fatalf("invalid stop value accepted: %#v", value)
		}
	}
}

func TestOpenAIStopValidationAtHTTPEntrypoint(t *testing.T) {
	h := &Handler{}
	for _, body := range []string{
		`{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"test"}],"stop":""}`,
		`{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"test"}],"stop":["1","2","3","4","5"]}`,
		`{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"test"}],"stop":42}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.handleOpenAIChat(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "stop") {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func TestOpenAIStopSequencesReachKiroPayloadAndEstimator(t *testing.T) {
	req := &OpenAIRequest{
		Model:    "claude-sonnet-4.6",
		Messages: []OpenAIMessage{{Role: "user", Content: "test"}},
		Stop:     []string{"<STOP>", "END"},
	}
	withoutStop := *req
	withoutStop.Stop = nil
	payload := OpenAIToKiro(req, false)
	if len(payload.StopSequences) != 2 || payload.StopSequences[0] != "<STOP>" {
		t.Fatalf("payload stop sequences = %#v", payload.StopSequences)
	}
	if priming := structuredOutputPriming(t, payload); !strings.Contains(priming, `["<STOP>","END"]`) {
		t.Fatalf("stop instruction missing from priming: %q", priming)
	}
	if estimateOpenAIRequestInputTokens(req) <= estimateOpenAIRequestInputTokens(&withoutStop) {
		t.Fatal("stop instruction was not included in the input token estimate")
	}
}

func TestOpenAIHandlersApplyStopAndPreserveEarlierTools(t *testing.T) {
	frames := [][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "call_kept", "name": "kept_tool", "input": `{}`, "stop": true,
		}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "before<ST"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "OP>after"}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "call_dropped", "name": "dropped_tool", "input": `{}`, "stop": true,
		}),
	}
	h := newOpenAIChatContractHandler(t, frames...)

	nonStreamReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"test"}],"stop":"<STOP>"}`,
	))
	nonStreamRec := httptest.NewRecorder()
	h.handleOpenAIChat(nonStreamRec, nonStreamReq)
	if nonStreamRec.Code != http.StatusOK {
		t.Fatalf("non-stream status=%d body=%s", nonStreamRec.Code, nonStreamRec.Body.String())
	}
	var nonStream map[string]interface{}
	if err := json.Unmarshal(nonStreamRec.Body.Bytes(), &nonStream); err != nil {
		t.Fatal(err)
	}
	choice := openAIChatContractChoice(t, nonStream, "non-stream")
	message := openAIChatContractMap(t, choice["message"], "non-stream message")
	toolCalls, _ := message["tool_calls"].([]interface{})
	if choice["finish_reason"] != "stop" || len(toolCalls) != 1 {
		t.Fatalf("non-stream choice = %#v", choice)
	}
	kept := openAIChatContractMap(t, toolCalls[0], "kept tool")
	function := openAIChatContractMap(t, kept["function"], "kept function")
	if function["name"] != "kept_tool" {
		t.Fatalf("non-stream tools = %#v", toolCalls)
	}

	streamReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"test"}],"stream":true,"stop":["<STOP>"]}`,
	))
	streamRec := httptest.NewRecorder()
	h.handleOpenAIChat(streamRec, streamReq)
	if streamRec.Code != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", streamRec.Code, streamRec.Body.String())
	}
	chunks := decodeOpenAIChatContractStream(t, streamRec.Body.String())
	var text string
	var tools []string
	var finishReason string
	for _, chunk := range chunks {
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		streamChoice := openAIChatContractMap(t, choices[0], "stream choice")
		delta := openAIChatContractMap(t, streamChoice["delta"], "stream delta")
		if content, ok := delta["content"].(string); ok {
			text += content
		}
		if rawTools, ok := delta["tool_calls"].([]interface{}); ok {
			for _, rawTool := range rawTools {
				tool := openAIChatContractMap(t, rawTool, "stream tool")
				function := openAIChatContractMap(t, tool["function"], "stream function")
				tools = append(tools, function["name"].(string))
			}
		}
		if reason, ok := streamChoice["finish_reason"].(string); ok {
			finishReason = reason
		}
	}
	if text != "before" || len(tools) != 1 || tools[0] != "kept_tool" || finishReason != "stop" {
		t.Fatalf("stream text=%q tools=%#v finish=%q\n%s", text, tools, finishReason, streamRec.Body.String())
	}
	if strings.Contains(streamRec.Body.String(), "after") || strings.Contains(streamRec.Body.String(), "dropped_tool") {
		t.Fatalf("stream leaked content after stop: %s", streamRec.Body.String())
	}
}

func TestOpenAINonStreamTruncatesAtStop(t *testing.T) {
	h := newOpenAIChatContractHandler(t,
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "before<ST"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "OP>after"}),
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"test"}],"stop":"<STOP>"}`,
	))
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	choice := openAIChatContractChoice(t, response, "non-stream stop")
	message := openAIChatContractMap(t, choice["message"], "non-stream stop message")
	if message["content"] != "before" || choice["finish_reason"] != "stop" {
		t.Fatalf("choice = %#v", choice)
	}
}
