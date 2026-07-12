package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStopSequenceFilterHandlesChunkBoundaries(t *testing.T) {
	filter := newStopSequenceFilter([]string{"<STOP>", "unused"})
	if got := filter.Push("before<ST"); got != "before" {
		t.Fatalf("first delta = %q, want %q", got, "before")
	}
	if got := filter.Push("OP>after"); got != "" {
		t.Fatalf("second delta leaked stop content: %q", got)
	}
	if got := filter.Flush(); got != "" {
		t.Fatalf("flush leaked content after stop: %q", got)
	}
	if got := filter.Matched(); got != "<STOP>" {
		t.Fatalf("matched sequence = %q", got)
	}
}

func TestStopSequenceFilterFlushesIncompletePrefix(t *testing.T) {
	filter := newStopSequenceFilter([]string{"<STOP>"})
	if got := filter.Push("answer<ST"); got != "answer" {
		t.Fatalf("delta = %q", got)
	}
	if got := filter.Flush(); got != "<ST" {
		t.Fatalf("flush = %q, want incomplete prefix", got)
	}
}

func TestClaudeStopSequencesReachKiroPayload(t *testing.T) {
	req := &ClaudeRequest{
		Model:         "claude-sonnet-4.6",
		Messages:      []ClaudeMessage{{Role: "user", Content: "test"}},
		StopSequences: []string{"<STOP>", "\nHuman:"},
	}
	payload := ClaudeToKiro(req, false)
	if len(payload.StopSequences) != 2 || payload.StopSequences[0] != "<STOP>" {
		t.Fatalf("internal stop sequences = %#v", payload.StopSequences)
	}
	if priming := structuredOutputPriming(t, payload); !strings.Contains(priming, `["<STOP>","\nHuman:"]`) {
		t.Fatalf("stop sequences missing from Kiro priming: %q", priming)
	}
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var wireObject map[string]interface{}
	if err := json.Unmarshal(wire, &wireObject); err != nil {
		t.Fatal(err)
	}
	if _, exists := wireObject["StopSequences"]; exists {
		t.Fatalf("adapter-only StopSequences field leaked into Kiro wire payload: %s", wire)
	}
	if _, exists := wireObject["stop_sequences"]; exists {
		t.Fatalf("adapter-only stop_sequences field leaked into Kiro wire payload: %s", wire)
	}
}

func TestCountTokensValidatesToolChoiceAndStopSequences(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{}
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown forced tool",
			body: `{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"test"}],"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"missing"}}`,
			want: "unknown tool",
		},
		{
			name: "empty stop sequence",
			body: `{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"test"}],"stop_sequences":[""]}`,
			want: "empty strings",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.handleCountTokens(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCountTokensIncludesRequiredToolChoiceInstruction(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{}
	base := `{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"test"}],"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object"}}]%s}`
	count := func(suffix string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(strings.Replace(base, "%s", suffix, 1)))
		rec := httptest.NewRecorder()
		h.handleCountTokens(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var response struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.InputTokens
	}

	automatic := count("")
	required := count(`,"tool_choice":{"type":"any"}`)
	if required <= automatic {
		t.Fatalf("required tool choice was not counted: auto=%d required=%d", automatic, required)
	}
}

func TestClaudeHandlersApplyStopSequenceMetadata(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "stop-sequence",
		Enabled:     true,
		AccessToken: "token-stop-sequence",
		ProfileArn:  "arn:aws:codewhisperer:profile/stop-sequence",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	frames := [][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_kept", "name": "kept_tool", "input": `{}`, "stop": true,
		}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "before<ST"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "OP>after"}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_dropped", "name": "dropped_tool", "input": `{}`, "stop": true,
		}),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, frame := range frames {
			_, _ = w.Write(frame)
		}
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	defer func() { kiroEndpoints = oldEndpoints }()
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingStats)
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	newPayload := func() *KiroPayload {
		payload := &KiroPayload{StopSequences: []string{"<STOP>"}}
		payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
			Content: "hello",
			ModelID: "claude-sonnet-4.6",
			Origin:  "AI_EDITOR",
		}
		return payload
	}

	nonStream := httptest.NewRecorder()
	h.handleClaudeNonStream(nonStream, newPayload(), "claude-sonnet-4.6", "claude-sonnet-4.6", false, claudeThinkingResponseOptions{}, 1, nil, "")
	if nonStream.Code != http.StatusOK {
		t.Fatalf("non-stream status=%d body=%s", nonStream.Code, nonStream.Body.String())
	}
	var response ClaudeResponse
	if err := json.Unmarshal(nonStream.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 2 || response.Content[0].Type != "text" || response.Content[0].Text != "before" ||
		response.Content[1].Type != "tool_use" || response.Content[1].Name != "kept_tool" {
		t.Fatalf("non-stream content = %#v", response.Content)
	}
	if response.StopReason != "stop_sequence" || response.StopSequence == nil || *response.StopSequence != "<STOP>" {
		t.Fatalf("non-stream stop metadata = reason:%q sequence:%v", response.StopReason, response.StopSequence)
	}

	stream := httptest.NewRecorder()
	h.handleClaudeStream(stream, newPayload(), "claude-sonnet-4.6", "claude-sonnet-4.6", false, claudeThinkingResponseOptions{}, 1, nil, "")
	events := decodeWebSearchSSE(t, stream.Body.String())
	var streamedText string
	var streamStopReason string
	var streamStopSequence interface{}
	var streamedTools []string
	for _, event := range events {
		switch event.Name {
		case "content_block_start":
			block, _ := event.Data["content_block"].(map[string]interface{})
			if block["type"] == "tool_use" {
				streamedTools = append(streamedTools, block["name"].(string))
			}
		case "content_block_delta":
			delta, _ := event.Data["delta"].(map[string]interface{})
			if delta["type"] == "text_delta" {
				streamedText += delta["text"].(string)
			}
		case "message_delta":
			delta := event.Data["delta"].(map[string]interface{})
			streamStopReason, _ = delta["stop_reason"].(string)
			streamStopSequence = delta["stop_sequence"]
		}
	}
	if streamedText != "before" {
		t.Fatalf("streamed text = %q", streamedText)
	}
	if len(streamedTools) != 1 || streamedTools[0] != "kept_tool" {
		t.Fatalf("streamed tools = %#v", streamedTools)
	}
	if streamStopReason != "stop_sequence" || streamStopSequence != "<STOP>" {
		t.Fatalf("stream stop metadata = reason:%q sequence:%#v", streamStopReason, streamStopSequence)
	}
	if strings.Contains(stream.Body.String(), "<STOP>") || strings.Contains(stream.Body.String(), "after") {
		t.Fatalf("stream leaked stopped content: %s", stream.Body.String())
	}
}
