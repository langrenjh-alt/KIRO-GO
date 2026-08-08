package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-go/config"
)

func TestKiroPayloadPreflightTrimsHaikuByTokenBudgetBelowRawLimit(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	t.Setenv("KIRO_INPUT_TOKEN_RESERVE", "8192")

	chunk := strings.Repeat("history context ", 270)
	messages := []ClaudeMessage{{Role: "user", Content: "oldest user turn"}}
	for i := 0; i < 80; i++ {
		messages = append(messages,
			ClaudeMessage{Role: "assistant", Content: "assistant old " + chunk},
			ClaudeMessage{Role: "user", Content: "user old " + chunk},
		)
	}
	messages = append(messages,
		ClaudeMessage{Role: "assistant", Content: "RECENT ASSISTANT"},
		ClaudeMessage{Role: "user", Content: "FINAL CURRENT REQUEST"},
	)
	payload := ClaudeToKiro(&ClaudeRequest{
		Model:    "claude-haiku-4.5",
		System:   "SYSTEM PRIMING MUST STAY",
		Messages: messages,
	}, false)

	before, err := measureKiroPayloadBreakdown(payload)
	if err != nil {
		t.Fatalf("measure before: %v", err)
	}
	tokenBudget := getContextWindowSize("claude-haiku-4.5") - defaultKiroInputTokenReserve
	if before.TotalBytes >= defaultKiroMaxRawBodyBytes {
		t.Fatalf("payload bytes=%d must stay below raw limit=%d for token-only case", before.TotalBytes, defaultKiroMaxRawBodyBytes)
	}
	if before.EstimatedInputTokens <= tokenBudget {
		t.Fatalf("estimated tokens=%d must exceed token budget=%d", before.EstimatedInputTokens, tokenBudget)
	}

	result, err := preflightKiroPayload(payload)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if result.DroppedHistoryEntries == 0 {
		t.Fatal("expected old history to be trimmed")
	}
	if result.After.TotalBytes > result.RawBodyLimitBytes || result.After.EstimatedInputTokens > result.InputTokenBudget {
		t.Fatalf("post-trim payload still exceeds limits: %+v", result)
	}
	if got := payload.ConversationState.CurrentMessage.UserInputMessage.Content; got != "FINAL CURRENT REQUEST" {
		t.Fatalf("current request changed: %q", got)
	}
	assertKiroSystemPriming(t, payload, "SYSTEM PRIMING MUST STAY")
	if !kiroHistoryContainsText(payload.ConversationState.History, "RECENT ASSISTANT") {
		t.Fatal("recent history was not retained")
	}
	if kiroHistoryContainsText(payload.ConversationState.History, "oldest user turn") {
		t.Fatal("oldest history was not removed")
	}
}

func TestKiroPayloadPreflightTrimsLargeContextModelByRawBodyLimit(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	t.Setenv("KIRO_INPUT_TOKEN_RESERVE", "8192")

	chunk := strings.Repeat("raw-body-history ", 350)
	messages := []OpenAIMessage{{Role: "system", Content: "RAW LIMIT SYSTEM"}, {Role: "user", Content: "start"}}
	for i := 0; i < 100; i++ {
		messages = append(messages,
			OpenAIMessage{Role: "assistant", Content: "old assistant " + chunk},
			OpenAIMessage{Role: "user", Content: "old user " + chunk},
		)
	}
	messages = append(messages,
		OpenAIMessage{Role: "assistant", Content: "RECENT RAW ASSISTANT"},
		OpenAIMessage{Role: "user", Content: "RAW CURRENT"},
	)
	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: messages}, false)

	before, err := measureKiroPayloadBreakdown(payload)
	if err != nil {
		t.Fatalf("measure before: %v", err)
	}
	if before.TotalBytes <= defaultKiroMaxRawBodyBytes {
		t.Fatalf("payload bytes=%d must exceed raw limit=%d", before.TotalBytes, defaultKiroMaxRawBodyBytes)
	}
	if before.EstimatedInputTokens >= getContextWindowSize("claude-opus-4.8")-defaultKiroInputTokenReserve {
		t.Fatalf("estimated tokens=%d unexpectedly hit 1M-model token budget", before.EstimatedInputTokens)
	}

	result, err := preflightKiroPayload(payload)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if result.DroppedHistoryEntries == 0 || result.After.TotalBytes > result.RawBodyLimitBytes {
		t.Fatalf("raw-body preflight did not trim correctly: %+v", result)
	}
	if got := payload.ConversationState.CurrentMessage.UserInputMessage.Content; got != "RAW CURRENT" {
		t.Fatalf("current request changed: %q", got)
	}
	if !kiroHistoryContainsText(payload.ConversationState.History, "RECENT RAW ASSISTANT") {
		t.Fatal("recent raw-limit history was not retained")
	}
}

func TestKiroPayloadPreflightRejectsOversizedSchemaWithoutUpstreamCall(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	t.Setenv("KIRO_INPUT_TOKEN_RESERVE", "8192")

	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:profile/preflight"}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = "preflight-schema"
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content:                 "current request",
		ModelID:                 "claude-opus-4.8",
		Origin:                  "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{Tools: []KiroToolWrapper{{}}},
	}
	tool := &payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0]
	tool.ToolSpecification.Name = "oversized_schema"
	tool.ToolSpecification.Description = "schema preflight"
	tool.ToolSpecification.InputSchema.JSON = map[string]interface{}{
		"type":        "object",
		"description": strings.Repeat("s", 950_000),
	}

	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	defer func() { kiroEndpoints = oldEndpoints }()

	err := CallKiroAPI(&config.Account{
		ID:          "preflight",
		AccessToken: "token",
		ProfileArn:  payload.ProfileArn,
	}, payload, nil)
	var preflightErr *KiroPayloadPreflightError
	if !errors.As(err, &preflightErr) {
		t.Fatalf("error=%T %v, want KiroPayloadPreflightError", err, err)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream calls=%d, want 0", upstreamCalls.Load())
	}
	if !isKiroClientRequestError(err) {
		t.Fatalf("preflight error was not classified as a client request error: %v", err)
	}
	if preflightErr.Breakdown.SchemaBytes <= 900_000 || preflightErr.Breakdown.ToolCount != 1 || preflightErr.Breakdown.SchemaCount != 1 {
		t.Fatalf("schema breakdown=%+v", preflightErr.Breakdown)
	}
	schema, ok := tool.ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if !ok || len(schema["description"].(string)) != 950_000 {
		t.Fatal("schema was silently changed")
	}
}

func TestKiroPayloadPreflightPreservesActiveToolAdjacencyAndIsIdempotent(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	t.Setenv("KIRO_INPUT_TOKEN_RESERVE", "8192")

	payload := &KiroPayload{}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = "active-tool-preflight"
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "SYSTEM ACTIVE", ModelID: "claude-haiku-4.5", Origin: "AI_EDITOR"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
	}
	chunk := strings.Repeat("old tool history ", 320)
	for i := 0; i < 70; i++ {
		payload.ConversationState.History = append(payload.ConversationState.History,
			KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: "old user " + chunk, ModelID: "claude-haiku-4.5", Origin: "AI_EDITOR"}},
			KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "old assistant " + chunk}},
		)
	}
	payload.ConversationState.History = append(payload.ConversationState.History,
		KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: "RECENT USER EVIDENCE", ModelID: "claude-haiku-4.5", Origin: "AI_EDITOR"}},
		KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "RECENT ASSISTANT EVIDENCE"}},
		KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: "run active tool", ModelID: "claude-haiku-4.5", Origin: "AI_EDITOR"}},
		KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{ToolUses: []KiroToolUse{{
			ToolUseID: "tool-active", Name: "inspect", Input: map[string]interface{}{"path": "src"},
		}}}},
	)
	current := KiroUserInputMessage{
		Content: "CURRENT TOOL RESULT",
		ModelID: "claude-haiku-4.5",
		Origin:  "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{
			Tools: []KiroToolWrapper{{}},
			ToolResults: []KiroToolResult{{
				ToolUseID: "tool-active",
				Status:    "success",
				Content:   []KiroResultContent{{Text: "tool result"}},
			}},
		},
	}
	current.UserInputMessageContext.Tools[0].ToolSpecification.Name = "inspect"
	current.UserInputMessageContext.Tools[0].ToolSpecification.Description = "inspect a path"
	current.UserInputMessageContext.Tools[0].ToolSpecification.InputSchema.JSON = map[string]interface{}{"type": "object"}
	payload.ConversationState.CurrentMessage.UserInputMessage = current
	currentWire, _ := json.Marshal(current)

	result, err := preflightKiroPayload(payload)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if result.DroppedHistoryEntries == 0 {
		t.Fatal("expected old history to be dropped")
	}
	assertKiroSystemPriming(t, payload, "SYSTEM ACTIVE")
	if !kiroHistoryContainsText(payload.ConversationState.History, "RECENT USER EVIDENCE") ||
		!kiroHistoryContainsText(payload.ConversationState.History, "RECENT ASSISTANT EVIDENCE") {
		t.Fatal("recent history was not preserved")
	}
	last := payload.ConversationState.History[len(payload.ConversationState.History)-1].AssistantResponseMessage
	if last == nil || len(last.ToolUses) != 1 || last.ToolUses[0].ToolUseID != "tool-active" {
		t.Fatalf("active assistant tool turn was not preserved: %#v", last)
	}
	afterCurrentWire, _ := json.Marshal(payload.ConversationState.CurrentMessage.UserInputMessage)
	if string(afterCurrentWire) != string(currentWire) {
		t.Fatal("current message, tools, schema, or tool results changed")
	}

	firstWire, _ := json.Marshal(payload)
	second, err := preflightKiroPayload(payload)
	if err != nil {
		t.Fatalf("second preflight: %v", err)
	}
	secondWire, _ := json.Marshal(payload)
	if second.DroppedHistoryEntries != 0 || string(secondWire) != string(firstWire) {
		t.Fatal("preflight was not idempotent")
	}
}

func TestHTTPProtocolsMapFixedPayloadPreflightFailureTo400BeforeStreaming(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	t.Setenv("KIRO_INPUT_TOKEN_RESERVE", "8192")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	huge := strings.Repeat("z", 950_000)
	schema := map[string]interface{}{"type": "object", "description": huge}

	tests := []struct {
		name   string
		path   string
		body   map[string]interface{}
		handle func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{
			name: "claude messages",
			path: "/v1/messages",
			body: map[string]interface{}{
				"model": "claude-opus-4.8", "max_tokens": 64, "stream": true,
				"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}},
				"tools":    []interface{}{map[string]interface{}{"name": "oversized_schema", "input_schema": schema}},
			},
			handle: func(h *Handler, w http.ResponseWriter, r *http.Request) { h.handleClaudeMessagesInternal(w, r) },
		},
		{
			name: "openai chat",
			path: "/v1/chat/completions",
			body: map[string]interface{}{
				"model": "claude-opus-4.8", "stream": true,
				"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}},
				"tools":    []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "oversized_schema", "parameters": schema}}},
			},
			handle: func(h *Handler, w http.ResponseWriter, r *http.Request) { h.handleOpenAIChat(w, r) },
		},
		{
			name: "openai responses",
			path: "/v1/responses",
			body: map[string]interface{}{
				"model": "claude-opus-4.8", "stream": true, "input": "hello",
				"tools": []interface{}{map[string]interface{}{"type": "function", "name": "oversized_schema", "parameters": schema}},
			},
			handle: func(h *Handler, w http.ResponseWriter, r *http.Request) { h.handleOpenAIResponses(w, r) },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.body)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(string(body)))
			test.handle(&Handler{}, recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			responseBody := recorder.Body.String()
			if !strings.Contains(responseBody, "invalid_request_error") || !strings.Contains(responseBody, "schema_bytes=") {
				t.Fatalf("unexpected error body: %s", responseBody)
			}
			if strings.Contains(responseBody, strings.Repeat("z", 32)) {
				t.Fatal("error response leaked schema content")
			}
			if strings.Contains(responseBody, "event:") {
				t.Fatalf("stream was committed before preflight failure: %s", responseBody)
			}
		})
	}
}

func assertKiroSystemPriming(t *testing.T, payload *KiroPayload, want string) {
	t.Helper()
	history := payload.ConversationState.History
	if len(history) < 2 || history[0].UserInputMessage == nil || history[1].AssistantResponseMessage == nil {
		t.Fatalf("system priming pair missing: %#v", history)
	}
	if !strings.Contains(history[0].UserInputMessage.Content, want) {
		t.Fatalf("system priming changed: %q", history[0].UserInputMessage.Content)
	}
}

func kiroHistoryContainsText(history []KiroHistoryMessage, text string) bool {
	for _, message := range history {
		if message.UserInputMessage != nil && strings.Contains(message.UserInputMessage.Content, text) {
			return true
		}
		if message.AssistantResponseMessage != nil && strings.Contains(message.AssistantResponseMessage.Content, text) {
			return true
		}
	}
	return false
}
