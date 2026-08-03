package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestThinkingSourceReasoningFirst(t *testing.T) {
	var source thinkingStreamSource

	if !allowReasoningSource(&source) {
		t.Fatalf("expected reasoning source to be accepted first")
	}
	if source != thinkingSourceReasoningEvent {
		t.Fatalf("expected source to be reasoning, got %v", source)
	}
	if allowTagSource(&source) {
		t.Fatalf("expected tag source to be rejected after reasoning source selected")
	}
}

func TestClaudeMessagesRejectsOversizedBody(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.ContentLength = claudeMaxRequestBodyBytes + 1
	rec := httptest.NewRecorder()

	h.handleClaudeMessagesInternal(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request_too_large") {
		t.Fatalf("unexpected error body: %s", rec.Body.String())
	}
}

func TestStructuredOutputValidationAtHTTPEntrypoints(t *testing.T) {
	h := &Handler{}

	claudeBody := `{"model":"claude-sonnet-4.6","max_tokens":1024,"messages":[{"role":"user","content":"test"}],"output_config":{"format":{"type":"json_schema","schema":[]}}}`
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(claudeBody))
		rec := httptest.NewRecorder()
		if path == "/v1/messages/count_tokens" {
			h.handleCountTokens(rec, req)
		} else {
			h.handleClaudeMessagesInternal(rec, req)
		}
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "format.schema") {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}

	chatBody := `{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"test"}],"response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "json_schema.name") {
		t.Fatalf("chat status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadRequestBodyLimitWithoutContentLength(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("0123456789"))
	req.ContentLength = -1
	_, err := readRequestBodyWithLimit(httptest.NewRecorder(), req, 8)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) || tooLarge.Limit != 8 {
		t.Fatalf("error = %#v, want MaxBytesError with limit 8", err)
	}
}

func TestRequestValidationAllowsEmptyUserTurns(t *testing.T) {
	claudeReq := &ClaudeRequest{
		Model:     "claude-sonnet-4.6",
		MaxTokens: 1024,
		Messages:  []ClaudeMessage{{Role: "user", Content: ""}},
	}
	if msg := validateClaudeRequestShape(claudeReq); msg != "" {
		t.Fatalf("Claude empty user turn rejected: %s", msg)
	}
	payload := ClaudeToKiro(claudeReq, false)
	if strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.Content) == "" {
		t.Fatal("Claude conversion did not synthesize fallback user content")
	}

	openAIReq := &OpenAIRequest{
		Model:    "claude-sonnet-4.6",
		Messages: []OpenAIMessage{{Role: "user", Content: ""}},
	}
	if msg := validateOpenAIRequestShape(openAIReq); msg != "" {
		t.Fatalf("OpenAI empty user turn rejected: %s", msg)
	}
	openAIPayload := OpenAIToKiro(openAIReq, false)
	if strings.TrimSpace(openAIPayload.ConversationState.CurrentMessage.UserInputMessage.Content) == "" {
		t.Fatal("OpenAI conversion did not synthesize fallback user content")
	}
}

func TestClaudeThinkingBudgetMayMeetOrExceedMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		name         string
		maxTokens    int
		budgetTokens int
	}{
		{name: "equal", maxTokens: 4096, budgetTokens: 4096},
		{name: "greater", maxTokens: 4096, budgetTokens: 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &ClaudeRequest{
				Model:     "claude-opus-4.8",
				MaxTokens: tc.maxTokens,
				Thinking: &ClaudeThinkingConfig{
					Type:         "enabled",
					BudgetTokens: tc.budgetTokens,
				},
				Messages: []ClaudeMessage{{Role: "user", Content: "test"}},
			}
			if msg := validateClaudeRequestShape(req); msg != "" {
				t.Fatalf("budget_tokens=%d max_tokens=%d rejected: %s", tc.budgetTokens, tc.maxTokens, msg)
			}
		})
	}
}

func TestClaudeThinkingBudgetAcceptsPositiveProtocolBoundaryValues(t *testing.T) {
	for _, budgetTokens := range []int{1, 100, 1023} {
		req := &ClaudeRequest{
			Model:     "claude-opus-4.8",
			MaxTokens: 2048,
			Thinking: &ClaudeThinkingConfig{
				Type:         "enabled",
				BudgetTokens: budgetTokens,
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "test"}},
		}
		if msg := validateClaudeRequestShape(req); msg != "" {
			t.Fatalf("budget_tokens=%d rejected: %s", budgetTokens, msg)
		}
	}
}

func TestClaudeNonStreamRetriesNextAccountAfterPreResponseFailure(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	if err := config.AddAccount(config.Account{
		ID:          "first",
		Enabled:     true,
		AccessToken: "token-first",
		ProfileArn:  "arn:aws:codewhisperer:profile/first",
	}); err != nil {
		t.Fatalf("add first account: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "second",
		Enabled:     true,
		AccessToken: "token-second",
		ProfileArn:  "arn:aws:codewhisperer:profile/second",
	}); err != nil {
		t.Fatalf("add second account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	requestTokens := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		requestTokens = append(requestTokens, token)
		// Fail the first attempted account (whichever it is) so the handler
		// is forced to add it to `excluded` and retry the other one.
		if len(requestTokens) == 1 {
			http.Error(w, "temporary upstream failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "retried successfully",
		}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    server.URL,
		Origin: "AI_EDITOR",
		Name:   "test",
	}}
	defer func() { kiroEndpoints = oldEndpoints }()

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingStats)
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, payload, "claude-sonnet-4.5", "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry to succeed, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(requestTokens) != 2 {
		t.Fatalf("expected two account attempts, got %v", requestTokens)
	}
	if requestTokens[0] == requestTokens[1] {
		t.Fatalf("expected first account to be excluded before retry, got %v", requestTokens)
	}
	tokenSet := map[string]bool{requestTokens[0]: true, requestTokens[1]: true}
	if !tokenSet["token-first"] || !tokenSet["token-second"] {
		t.Fatalf("expected both accounts to be tried, got %v", requestTokens)
	}

	var resp ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "retried successfully" {
		t.Fatalf("expected retried response content, got %#v", resp.Content)
	}
}

func TestClaudeNonStreamDoesNotRotateAccountsForMalformedToolJSON(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, account := range []config.Account{
		{
			ID:          "protocol-first",
			Enabled:     true,
			AccessToken: "token-protocol-first",
			ProfileArn:  "arn:aws:codewhisperer:profile/protocol-first",
		},
		{
			ID:          "protocol-second",
			Enabled:     true,
			AccessToken: "token-protocol-second",
			ProfileArn:  "arn:aws:codewhisperer:profile/protocol-second",
		},
	} {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("add account %s: %v", account.ID, err)
		}
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_bad",
			"name":      "broken",
			"input":     `{"unterminated":`,
			"stop":      true,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingStats)
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, payload, "claude-sonnet-4.5", "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "")

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_tool_json_error") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("upstream requests=%d, want 1 without account rotation", got)
	}
	for _, account := range config.GetAccounts() {
		if !account.Enabled {
			t.Fatalf("protocol error disabled healthy account %q", account.ID)
		}
	}
}

func TestClaudeStreamNativeReasoningProtocol(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "stream-protocol",
		Enabled:     true,
		AccessToken: "token-stream-protocol",
		ProfileArn:  "arn:aws:codewhisperer:profile/stream-protocol",
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
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
			"signature":       "real-signature",
			"redactedContent": "encrypted-thinking",
		}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "final answer"}),
		awsEventStreamFrameWithHeaders(t, map[string]string{
			":message-type":   "exception",
			":exception-type": "ContentLengthExceededException",
		}, map[string]interface{}{"message": "maximum output reached"}),
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
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.6",
		Origin:  "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeStream(
		rec,
		payload,
		"claude-sonnet-4.6",
		"claude-sonnet-4-6-thinking",
		true,
		claudeThinkingResponseOptions{Format: "thinking"},
		10,
		nil,
		"",
	)

	events := decodeWebSearchSSE(t, rec.Body.String())
	positions := map[string]int{}
	thinkingIndex := -1
	var messageDelta map[string]interface{}
	for i, event := range events {
		switch event.Name {
		case "message_start":
			message := event.Data["message"].(map[string]interface{})
			if message["model"] != "claude-sonnet-4-6-thinking" {
				t.Fatalf("response model = %#v", message["model"])
			}
			usage := message["usage"].(map[string]interface{})
			if usage["output_tokens"] != float64(1) || usage["service_tier"] != "standard" {
				t.Fatalf("message_start usage = %#v", usage)
			}
		case "ping":
			positions["ping"] = i
		case "content_block_start":
			block := event.Data["content_block"].(map[string]interface{})
			switch block["type"] {
			case "thinking":
				thinkingIndex = int(event.Data["index"].(float64))
				positions["thinking_start"] = i
			case "redacted_thinking":
				if block["data"] != "encrypted-thinking" {
					t.Fatalf("redacted data = %#v", block["data"])
				}
				positions["redacted_start"] = i
			case "text":
				positions["text_start"] = i
			}
		case "content_block_delta":
			delta := event.Data["delta"].(map[string]interface{})
			if delta["type"] == "signature_delta" {
				if delta["signature"] != "real-signature" {
					t.Fatalf("signature delta = %#v", delta)
				}
				positions["signature"] = i
			}
		case "content_block_stop":
			if thinkingIndex >= 0 && int(event.Data["index"].(float64)) == thinkingIndex {
				positions["thinking_stop"] = i
			}
		case "message_delta":
			messageDelta = event.Data
		}
	}

	for _, name := range []string{"ping", "thinking_start", "signature", "thinking_stop", "redacted_start", "text_start"} {
		if _, ok := positions[name]; !ok {
			t.Fatalf("missing %s event in %s", name, rec.Body.String())
		}
	}
	if !(positions["thinking_start"] < positions["ping"] &&
		positions["ping"] < positions["signature"] &&
		positions["signature"] < positions["thinking_stop"] &&
		positions["thinking_stop"] < positions["redacted_start"] &&
		positions["redacted_start"] < positions["text_start"]) {
		t.Fatalf("invalid reasoning block order: %#v", positions)
	}
	if messageDelta == nil {
		t.Fatal("missing message_delta")
	}
	delta := messageDelta["delta"].(map[string]interface{})
	if delta["stop_reason"] != "max_tokens" {
		t.Fatalf("stop reason = %#v", delta["stop_reason"])
	}
	if value, exists := delta["stop_sequence"]; !exists || value != nil {
		t.Fatalf("stop_sequence = %#v, exists=%v", value, exists)
	}
	if value, exists := delta["stop_details"]; !exists || value != nil {
		t.Fatalf("stop_details = %#v, exists=%v", value, exists)
	}
	usage := messageDelta["usage"].(map[string]interface{})
	details, ok := usage["output_tokens_details"].(map[string]interface{})
	if !ok {
		t.Fatalf("message_delta usage is missing thinking details: %#v", usage)
	}
	wantThinkingTokens := estimateClaudeOutputTokensWithRedacted("", "", []string{"encrypted-thinking"}, nil)
	if details["thinking_tokens"] != float64(wantThinkingTokens) {
		t.Fatalf("thinking_tokens = %#v, want %d", details["thinking_tokens"], wantThinkingTokens)
	}
}

func TestThinkingSourceTagFirst(t *testing.T) {
	var source thinkingStreamSource

	if !allowTagSource(&source) {
		t.Fatalf("expected tag source to be accepted first")
	}
	if source != thinkingSourceTagBlock {
		t.Fatalf("expected source to be tag, got %v", source)
	}
	if allowReasoningSource(&source) {
		t.Fatalf("expected reasoning source to be rejected after tag source selected")
	}
}

func TestThinkingSourceSameSourceRemainsAllowed(t *testing.T) {
	var source thinkingStreamSource

	if !allowTagSource(&source) {
		t.Fatalf("expected initial tag source selection to succeed")
	}
	if !allowTagSource(&source) {
		t.Fatalf("expected repeated tag source selection to stay allowed")
	}

	source = thinkingSourceUnknown
	if !allowReasoningSource(&source) {
		t.Fatalf("expected initial reasoning source selection to succeed")
	}
	if !allowReasoningSource(&source) {
		t.Fatalf("expected repeated reasoning source selection to stay allowed")
	}
}

func TestValidateOpenAIRequestShapeRejectsAssistantPrefill(t *testing.T) {
	req := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "prefill"},
		},
	}

	if msg := validateOpenAIRequestShape(req); msg == "" {
		t.Fatalf("expected assistant-prefill final message to be rejected")
	}
}

func TestValidateOpenAIRequestShapeAllowsToolResultFinalTurn(t *testing.T) {
	req := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find weather"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: "{}"},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
		},
	}

	if msg := validateOpenAIRequestShape(req); msg != "" {
		t.Fatalf("expected tool-result final turn to be valid, got %q", msg)
	}
}

func TestValidateClaudeRequestShapeRejectsAssistantPrefill(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "prefill"},
		},
	}

	if msg := validateClaudeRequestShape(req); msg == "" {
		t.Fatalf("expected assistant-prefill final message to be rejected")
	}
}

func TestResolveClaudeThinkingModeHonorsRequestThinking(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		thinking     *ClaudeThinkingConfig
		wantModel    string
		wantThinking bool
	}{
		{
			name:         "adaptive request enables thinking",
			model:        "claude-sonnet-4.6",
			thinking:     &ClaudeThinkingConfig{Type: "adaptive"},
			wantModel:    "claude-sonnet-4.6",
			wantThinking: true,
		},
		{
			name:         "enabled request enables thinking",
			model:        "claude-opus-4.5",
			thinking:     &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			wantModel:    "claude-opus-4.5",
			wantThinking: true,
		},
		{
			name:         "disabled request keeps thinking off",
			model:        "claude-opus-4.7",
			thinking:     &ClaudeThinkingConfig{Type: "disabled"},
			wantModel:    "claude-opus-4.7",
			wantThinking: false,
		},
		{
			name:         "suffix remains supported when thinking is disabled",
			model:        "claude-sonnet-4.5-thinking",
			thinking:     &ClaudeThinkingConfig{Type: "disabled"},
			wantModel:    "claude-sonnet-4.5",
			wantThinking: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotThinking := resolveClaudeThinkingMode(tc.model, tc.thinking, "-thinking")
			if gotModel != tc.wantModel {
				t.Fatalf("expected model %q, got %q", tc.wantModel, gotModel)
			}
			if gotThinking != tc.wantThinking {
				t.Fatalf("expected thinking=%v, got %v", tc.wantThinking, gotThinking)
			}
		})
	}
}

func TestCloneClaudeRequestForThinkingInjectsPromptWithoutMutatingOriginal(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-sonnet-4.6",
		System: "Follow the user instructions.",
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	blocks, ok := cloned.System.([]interface{})
	if !ok {
		t.Fatalf("expected cloned system prompt to be structured blocks, got %T", cloned.System)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks after prepend, got %d", len(blocks))
	}
	gotPrompt := extractSystemPrompt(cloned.System)
	expected := ThinkingModePrompt + "\n\nFollow the user instructions."
	if gotPrompt != expected {
		t.Fatalf("expected injected system prompt %q, got %q", expected, gotPrompt)
	}
	if original, ok := req.System.(string); !ok || original != "Follow the user instructions." {
		t.Fatalf("expected original request system prompt to stay unchanged, got %#v", req.System)
	}
}

func TestCloneClaudeRequestForThinkingPreservesStructuredSystemBlocks(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": "cached system",
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
					"ttl":  "5m",
				},
			},
		},
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	blocks, ok := cloned.System.([]interface{})
	if !ok {
		t.Fatalf("expected structured system blocks, got %T", cloned.System)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks after prepend, got %d", len(blocks))
	}
	first, ok := blocks[0].(map[string]interface{})
	if !ok || first["text"] != ThinkingModePrompt+"\n" {
		t.Fatalf("expected first block to be thinking prompt, got %#v", blocks[0])
	}
	second, ok := blocks[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected original system block to remain a map, got %T", blocks[1])
	}
	cacheControl, ok := second["cache_control"].(map[string]interface{})
	if !ok || cacheControl["type"] != "ephemeral" {
		t.Fatalf("expected original cache_control to be preserved, got %#v", second["cache_control"])
	}
}

func TestThinkingPromptAffectsClaudeTokenEstimate(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.6",
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}

	baseTokens := estimateClaudeRequestInputTokens(req)
	thinkingTokens := estimateClaudeRequestInputTokens(cloneClaudeRequestForThinking(req, true))

	if thinkingTokens <= baseTokens {
		t.Fatalf("expected thinking tokens (%d) to exceed base tokens (%d)", thinkingTokens, baseTokens)
	}
}

func TestValidateClaudeThinkingConfig(t *testing.T) {
	tests := []struct {
		name        string
		thinking    *ClaudeThinkingConfig
		maxTokens   int
		expectError bool
	}{
		{
			name:        "adaptive is valid",
			thinking:    &ClaudeThinkingConfig{Type: "adaptive"},
			maxTokens:   4096,
			expectError: false,
		},
		{
			name:        "enabled requires budget",
			thinking:    &ClaudeThinkingConfig{Type: "enabled"},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "enabled accepts a positive budget below legacy minimum",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 100},
			maxTokens:   4096,
			expectError: false,
		},
		{
			name:        "enabled rejects max tokens zero",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			maxTokens:   0,
			expectError: true,
		},
		{
			name:        "enabled budget may equal max tokens for proxy compatibility",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 4096},
			maxTokens:   4096,
			expectError: false,
		},
		{
			name:        "enabled budget may exceed max tokens for proxy compatibility",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 8192},
			maxTokens:   4096,
			expectError: false,
		},
		{
			name:        "disabled rejects display",
			thinking:    &ClaudeThinkingConfig{Type: "disabled", Display: "summarized"},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "missing type is rejected",
			thinking:    &ClaudeThinkingConfig{},
			maxTokens:   4096,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errMsg := validateClaudeThinkingConfig(tc.thinking, tc.maxTokens)
			if tc.expectError && errMsg == "" {
				t.Fatalf("expected validation error")
			}
			if !tc.expectError && errMsg != "" {
				t.Fatalf("expected thinking config to be valid, got %q", errMsg)
			}
		})
	}
}

func TestResolveClaudeThinkingResponseOptions(t *testing.T) {
	tests := []struct {
		name       string
		thinking   *ClaudeThinkingConfig
		defaultFmt string
		wantFmt    string
		wantOmit   bool
	}{
		{
			name:       "default config is preserved when display unset",
			thinking:   &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			defaultFmt: "think",
			wantFmt:    "think",
			wantOmit:   false,
		},
		{
			name:       "summarized forces official thinking blocks",
			thinking:   &ClaudeThinkingConfig{Type: "adaptive", Display: "summarized"},
			defaultFmt: "reasoning_content",
			wantFmt:    "thinking",
			wantOmit:   false,
		},
		{
			name:       "omitted forces official thinking blocks and hides content",
			thinking:   &ClaudeThinkingConfig{Type: "adaptive", Display: "omitted"},
			defaultFmt: "think",
			wantFmt:    "thinking",
			wantOmit:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := resolveClaudeThinkingResponseOptions(tc.thinking, tc.defaultFmt)
			if opts.Format != tc.wantFmt {
				t.Fatalf("expected format %q, got %q", tc.wantFmt, opts.Format)
			}
			if opts.OmitDisplay != tc.wantOmit {
				t.Fatalf("expected omitDisplay=%v, got %v", tc.wantOmit, opts.OmitDisplay)
			}
		})
	}
}

func TestMergeUniqueModelsPreservesUnionAcrossAccounts(t *testing.T) {
	base := []ModelInfo{
		{ModelId: "claude-sonnet-4.5", InputTypes: []string{"TEXT"}},
	}
	incoming := []ModelInfo{
		{ModelId: "claude-sonnet-4.5", InputTypes: []string{"image"}},
		{ModelId: "claude-opus-4-7", InputTypes: []string{"text"}},
	}

	merged := mergeUniqueModels(base, incoming)
	if len(merged) != 2 {
		t.Fatalf("expected 2 unique models, got %d", len(merged))
	}
	if !modelSupportsImage(merged[0].InputTypes) {
		t.Fatalf("expected merged input types to preserve image capability, got %#v", merged[0].InputTypes)
	}
	if merged[1].ModelId != "claude-opus-4-7" {
		t.Fatalf("expected second model to be claude-opus-4-7, got %q", merged[1].ModelId)
	}
}

func TestBuildAnthropicModelsResponseGeneratesThinkingVariants(t *testing.T) {
	models := buildAnthropicModelsResponse([]ModelInfo{{
		ModelId:    "claude-sonnet-4.5",
		InputTypes: []string{"text", "image"},
	}}, "-thinking")

	if len(models) != 2 {
		t.Fatalf("expected base model and thinking variant, got %d", len(models))
	}
	if models[0]["id"] != "claude-sonnet-4.5" {
		t.Fatalf("unexpected base model id: %#v", models[0]["id"])
	}
	if models[1]["id"] != "claude-sonnet-4.5-thinking" {
		t.Fatalf("unexpected thinking model id: %#v", models[1]["id"])
	}
	if supportsImage, ok := models[0]["supports_image"].(bool); !ok || !supportsImage {
		t.Fatalf("expected image capability to be preserved, got %#v", models[0]["supports_image"])
	}
}

// TestStatsIncludesCacheMetrics verifies /v1/stats surfaces a "cache" object
// populated from the prompt-cache tracker's atomic counters (hits/misses).
func TestStatsIncludesCacheMetrics(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()

	tr := newPromptCacheTracker(defaultPromptCacheTTL)
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6",
		TotalInputTokens: 2000,
		Breakpoints:      []promptCacheBreakpoint{{Fingerprint: [32]byte{9}, CumulativeTokens: 2000, TTL: time.Hour}},
	}
	tr.Compute("acct", profile) // miss (empty cache)
	tr.Update("acct", profile)
	tr.Compute("acct", profile) // hit

	h := &Handler{
		pool:        p,
		promptCache: tr,
		startTime:   time.Now().Unix(),
	}

	rec := httptest.NewRecorder()
	h.handleStats(rec, httptest.NewRequest(http.MethodGet, "/v1/stats", nil))

	var got map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	cache, ok := got["cache"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a cache object in stats, got %#v", got)
	}
	if cache["hits"].(float64) != 1 {
		t.Fatalf("expected 1 hit, got %v", cache["hits"])
	}
	if cache["misses"].(float64) != 1 {
		t.Fatalf("expected 1 miss, got %v", cache["misses"])
	}
}

func TestAdminSettingsPromptCacheMaxRatioHotReload(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdatePromptCacheMaxRatio(0.85); err != nil {
		t.Fatalf("set initial cache ratio: %v", err)
	}

	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	profile := &promptCacheProfile{
		Model:            "claude-sonnet-4-6",
		TotalInputTokens: 2000,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      [32]byte{11},
			CumulativeTokens: 2000,
			TTL:              time.Hour,
		}},
	}
	tracker.Update("acct", profile)
	if got := tracker.Compute("acct", profile).CacheReadInputTokens; got != 1700 {
		t.Fatalf("expected initial 0.85 ratio to cap cache reads at 1700, got %d", got)
	}

	h := &Handler{}
	updateRec := httptest.NewRecorder()
	updateReq := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(`{"promptCacheMaxRatio":0.95}`))
	h.apiUpdateSettings(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}
	if got := config.GetPromptCacheMaxRatio(); got != 0.95 {
		t.Fatalf("expected in-memory ratio 0.95, got %v", got)
	}
	if got := tracker.Compute("acct", profile).CacheReadInputTokens; got != 1900 {
		t.Fatalf("expected hot-reloaded 0.95 ratio to cap cache reads at 1900, got %d", got)
	}

	getRec := httptest.NewRecorder()
	h.apiGetSettings(getRec, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))
	var settings map[string]interface{}
	if err := json.NewDecoder(getRec.Body).Decode(&settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if got := settings["promptCacheMaxRatio"]; got != 0.95 {
		t.Fatalf("expected settings API ratio 0.95, got %#v", got)
	}

	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if got := config.GetPromptCacheMaxRatio(); got != 0.95 {
		t.Fatalf("expected persisted ratio 0.95 after reload, got %v", got)
	}
}

func TestAdminSettingsRejectsInvalidPromptCacheMaxRatio(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdatePromptCacheMaxRatio(0.85); err != nil {
		t.Fatalf("set initial cache ratio: %v", err)
	}

	h := &Handler{}
	for _, body := range []string{
		`{"promptCacheMaxRatio":0}`,
		`{"promptCacheMaxRatio":0.001}`,
		`{"promptCacheMaxRatio":-0.1}`,
		`{"promptCacheMaxRatio":1.01}`,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
		h.apiUpdateSettings(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
		if got := config.GetPromptCacheMaxRatio(); got != 0.85 {
			t.Fatalf("body=%s changed ratio to %v", body, got)
		}
	}
}

func TestClaudePromptCacheIsolationSeed(t *testing.T) {
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(`{"metadata":{"user_id":"user_hash_account__session_session-123"}}`), &req); err != nil {
		t.Fatalf("decode Claude metadata: %v", err)
	}

	if got := claudePromptCacheIsolationSeed(&req, "client-key-id"); got != "key:client-key-id|session:session-123" {
		t.Fatalf("session seed = %q", got)
	}
	if first, second := claudePromptCacheIsolationSeed(&req, "key-a"), claudePromptCacheIsolationSeed(&req, "key-b"); first == second {
		t.Fatalf("different client keys shared a session seed: %q", first)
	}
	if got := claudePromptCacheIsolationSeed(&req, ""); got != "session:session-123" {
		t.Fatalf("session-only seed = %q", got)
	}
	req.Metadata.UserID = "user-without-session"
	if got := claudePromptCacheIsolationSeed(&req, "client-key-id"); got != "key:client-key-id" {
		t.Fatalf("key fallback seed = %q", got)
	}
	req.Metadata.UserID = "trailing_session_"
	if got := claudePromptCacheIsolationSeed(&req, ""); got != "" {
		t.Fatalf("shared key without session must disable cache, got %q", got)
	}

	providedSeed := claudePromptCacheIsolationSeed(&req, "", "client-secret")
	if providedSeed == "" || !strings.HasPrefix(providedSeed, "provided-key:") {
		t.Fatalf("provided key seed = %q", providedSeed)
	}
	if strings.Contains(providedSeed, "client-secret") {
		t.Fatal("provided key was retained in the isolation seed")
	}
	if repeat := claudePromptCacheIsolationSeed(&req, "", "client-secret"); repeat != providedSeed {
		t.Fatalf("provided key seed drifted: first=%q repeat=%q", providedSeed, repeat)
	}
	if other := claudePromptCacheIsolationSeed(&req, "", "other-secret"); other == providedSeed {
		t.Fatal("different provided keys shared an isolation seed")
	}
}

func TestPromptCacheProvidedHeaderKeyWorksWithoutAuthContext(t *testing.T) {
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	claudeReq := &ClaudeRequest{
		Model: "claude-sonnet-4-6",
		System: []interface{}{map[string]interface{}{
			"type":          "text",
			"text":          strings.Repeat("stable client-key prefix ", 300),
			"cache_control": map[string]interface{}{"type": "ephemeral"},
		}},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}

	headerReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	headerReq.Header.Set("X-Api-Key", "open-client-key")
	seed := claudePromptCacheIsolationSeed(claudeReq, "", extractProvidedKey(headerReq))
	profile := tracker.BuildClaudeProfile(claudeReq, estimateClaudeRequestInputTokens(claudeReq), seed)
	if profile == nil {
		t.Fatal("provided request header should build a cache profile without an auth context")
	}
	if cold := tracker.Compute("account", profile); cold.CacheReadInputTokens != 0 {
		t.Fatalf("cold request cache_read=%d, want 0", cold.CacheReadInputTokens)
	}
	tracker.Update("account", profile)
	if hot := tracker.Compute("account", profile); hot.CacheReadInputTokens == 0 {
		t.Fatal("repeated request header key should hit the stored prefix")
	}

	otherReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	otherReq.Header.Set("Authorization", "Bearer other-client-key")
	otherSeed := claudePromptCacheIsolationSeed(claudeReq, "", extractProvidedKey(otherReq))
	otherProfile := tracker.BuildClaudeProfile(claudeReq, estimateClaudeRequestInputTokens(claudeReq), otherSeed)
	if other := tracker.Compute("account", otherProfile); other.CacheReadInputTokens != 0 {
		t.Fatalf("different request header key cache_read=%d, want 0", other.CacheReadInputTokens)
	}
}

func TestReanchorClaudeCacheUsageUsesFinalUpstreamInput(t *testing.T) {
	profile := &promptCacheProfile{Model: "claude-opus-5", TotalInputTokens: 2000}
	estimated := promptCacheUsage{
		CacheCreationInputTokens:   500,
		CacheReadInputTokens:       1000,
		CacheCreation5mInputTokens: 500,
	}

	got := reanchorClaudeCacheUsage(profile, estimated, 1000)
	if got.CacheCreationInputTokens != 250 || got.CacheReadInputTokens != 500 {
		t.Fatalf("reanchored usage = %+v, want creation=250 read=500", got)
	}
	if total := billedClaudeInputTokens(1000, got) + got.CacheCreationInputTokens + got.CacheReadInputTokens; total != 1000 {
		t.Fatalf("reanchored token split total = %d, want 1000", total)
	}
}

func TestClaudeUsageTotalDoesNotExceedModelContextWindow(t *testing.T) {
	const estimatedTotal = 1_029_554
	profile := &promptCacheProfile{Model: "claude-opus-5", TotalInputTokens: estimatedTotal}
	estimated := promptCacheUsage{
		CacheCreationInputTokens:   595,
		CacheReadInputTokens:       926_600,
		CacheCreation5mInputTokens: 595,
	}

	inputTokens := clampClaudeInputTokensToModelWindow(profile.Model, estimatedTotal)
	usage := reanchorClaudeCacheUsage(profile, estimated, inputTokens)
	total := billedClaudeInputTokens(inputTokens, usage) + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	if total != 1_000_000 {
		t.Fatalf("reported input total=%d, want 1000000; usage=%+v", total, usage)
	}
	if inputTokens != 1_000_000 {
		t.Fatalf("input token basis=%d, want 1000000", inputTokens)
	}
}

func TestNoAvailableAccountsAreRecordedInTraffic(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	tracker := NewTrafficTracker(time.Hour)
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTrackerWithCapacity(time.Hour, 16),
		traffic:     tracker,
	}

	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, &KiroPayload{}, "claude-sonnet-4.6", "claude-sonnet-4.6", false, claudeThinkingResponseOptions{}, 100, nil, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := tracker.Query(time.Hour).Totals.Requests; got != 1 {
		t.Fatalf("traffic requests=%d, want 1", got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failed requests=%d, want 1", got)
	}
}

func TestHandlerCloseWaitsForPromptCacheFlush(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := NewHandler()
	h.promptCache.mu.Lock()
	h.promptCache.putLocked([32]byte{42}, time.Now().Add(time.Hour), time.Hour)
	h.promptCache.dirty = true
	h.promptCache.mu.Unlock()

	done := make(chan struct{})
	go func() {
		h.Close()
		h.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Handler.Close did not finish promptly")
	}

	loaded := newPromptCacheTrackerWithCapacity(time.Hour, 16)
	loaded.Load(filepath.Join(filepath.Dir(configPath), "prompt_cache.json"))
	if got := loaded.Stats().Entries; got != 1 {
		t.Fatalf("flushed prompt cache entries=%d, want 1", got)
	}
}

func TestAdminSettingsPromptCacheMaxEntriesHotReload(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdatePromptCacheMaxEntries(300); err != nil {
		t.Fatalf("set initial capacity: %v", err)
	}

	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 300)
	now := time.Now()
	tracker.mu.Lock()
	for i := 0; i < 300; i++ {
		fingerprint := [32]byte{byte(i >> 8), byte(i)}
		tracker.putLocked(fingerprint, now.Add(time.Hour), time.Hour)
	}
	tracker.mu.Unlock()

	h := &Handler{promptCache: tracker}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(`{"promptCacheMaxEntries":256}`))
	h.apiUpdateSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := config.GetPromptCacheMaxEntries(); got != 256 {
		t.Fatalf("configured capacity = %d, want 256", got)
	}
	stats := tracker.Stats()
	if stats.Capacity != 256 || stats.Entries != 256 || stats.Evictions != 44 {
		t.Fatalf("live tracker did not shrink immediately: %+v", stats)
	}

	getRec := httptest.NewRecorder()
	h.apiGetSettings(getRec, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))
	var settings map[string]interface{}
	if err := json.NewDecoder(getRec.Body).Decode(&settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if got := settings["promptCacheMaxEntries"]; got != float64(256) {
		t.Fatalf("settings capacity = %#v, want 256", got)
	}

	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if got := config.GetPromptCacheMaxEntries(); got != 256 {
		t.Fatalf("persisted capacity = %d, want 256", got)
	}
}

func TestAdminPromptCacheCapacityConcurrentUpdatesStayInSync(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	tracker := newPromptCacheTrackerWithCapacity(time.Hour, 4096)
	h := &Handler{promptCache: tracker}
	capacities := []int{256, 512, 1024, 2048, 4096, 8192}

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		capacity := capacities[i%len(capacities)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			body := fmt.Sprintf(`{"promptCacheMaxEntries":%d}`, capacity)
			h.apiUpdateSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Errorf("capacity=%d status=%d body=%s", capacity, rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	if configured, live := config.GetPromptCacheMaxEntries(), tracker.Stats().Capacity; live != configured {
		t.Fatalf("persisted capacity=%d live capacity=%d", configured, live)
	}
}

func TestAdminSettingsRejectsInvalidPromptCacheMaxEntries(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdatePromptCacheMaxEntries(4096); err != nil {
		t.Fatalf("set initial capacity: %v", err)
	}

	h := &Handler{promptCache: newPromptCacheTrackerWithCapacity(time.Hour, 4096)}
	for _, body := range []string{
		`{"promptCacheMaxEntries":255}`,
		`{"promptCacheMaxEntries":1048577}`,
		`{"promptCacheMaxEntries":1.5}`,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
		h.apiUpdateSettings(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
		if got := config.GetPromptCacheMaxEntries(); got != 4096 {
			t.Fatalf("body=%s changed capacity to %d", body, got)
		}
	}
}

func TestAdminSettingsPromptCacheTTLHotReloadAndPersistence(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	tracker := newPromptCacheTracker(time.Hour)
	h := &Handler{promptCache: tracker}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(`{"promptCacheTTLSeconds":3600}`))
	h.apiUpdateSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := config.GetPromptCacheTTLSeconds(); got != config.ExtendedPromptCacheTTLSeconds {
		t.Fatalf("configured TTL=%d, want 3600", got)
	}
	if got := tracker.DefaultTTL(); got != time.Hour {
		t.Fatalf("live tracker TTL=%v, want 1h", got)
	}

	getRec := httptest.NewRecorder()
	h.apiGetSettings(getRec, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))
	var settings map[string]interface{}
	if err := json.NewDecoder(getRec.Body).Decode(&settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if got := settings["promptCacheTTLSeconds"]; got != float64(config.ExtendedPromptCacheTTLSeconds) {
		t.Fatalf("settings TTL=%#v, want 3600", got)
	}

	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if got := config.GetPromptCacheTTLSeconds(); got != config.ExtendedPromptCacheTTLSeconds {
		t.Fatalf("persisted TTL=%d, want 3600", got)
	}
}

func TestAdminSettingsRejectsInvalidPromptCacheTTL(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	tracker := newPromptCacheTracker(time.Hour)
	h := &Handler{promptCache: tracker}
	for _, body := range []string{
		`{"promptCacheTTLSeconds":0}`,
		`{"promptCacheTTLSeconds":299}`,
		`{"promptCacheTTLSeconds":301}`,
		`{"promptCacheTTLSeconds":3599}`,
		`{"promptCacheTTLSeconds":3601}`,
		`{"promptCacheTTLSeconds":300.5}`,
		`{"promptCacheTTLSeconds":"300"}`,
	} {
		rec := httptest.NewRecorder()
		h.apiUpdateSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
		if got := config.GetPromptCacheTTLSeconds(); got != config.DefaultPromptCacheTTLSeconds {
			t.Fatalf("body=%s changed configured TTL to %d", body, got)
		}
		if got := tracker.DefaultTTL(); got != defaultPromptCacheTTL {
			t.Fatalf("body=%s changed live TTL to %v", body, got)
		}
	}
}

func TestAdminSettingsPromptCacheTTLSaveFailureDoesNotHotReload(t *testing.T) {
	dir := t.TempDir()
	if err := config.Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	tracker := newPromptCacheTracker(time.Hour)
	h := &Handler{promptCache: tracker}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove config directory: %v", err)
	}

	rec := httptest.NewRecorder()
	h.apiUpdateSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(`{"promptCacheTTLSeconds":3600}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", rec.Code, rec.Body.String())
	}
	if got := config.GetPromptCacheTTLSeconds(); got != config.DefaultPromptCacheTTLSeconds {
		t.Fatalf("failed save changed configured TTL to %d", got)
	}
	if got := tracker.DefaultTTL(); got != defaultPromptCacheTTL {
		t.Fatalf("failed save changed live tracker TTL to %v", got)
	}
}

func TestAdminTrafficUsesMutuallyExclusiveTokenSeries(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	now := time.Date(2026, 7, 11, 12, 0, 30, 0, time.UTC)
	tracker := newTrafficTracker(time.Hour, func() time.Time { return now })
	h := &Handler{traffic: tracker}

	h.recordSuccessForApiKey("", 100, 20, 0)
	h.recordSuccessForApiKeyWithCache("", 200, 30, 0, promptCacheUsage{
		CacheCreationInputTokens: 40,
		CacheReadInputTokens:     120,
	})
	h.recordFailureWithDetails("claude", "model", "account", newAuthError(500, "api_error", "boom"))

	rec := httptest.NewRecorder()
	h.apiGetTraffic(rec, httptest.NewRequest(http.MethodGet, "/admin/api/traffic?range=1h", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("traffic status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Range  string          `json:"range"`
		Totals TrafficCounters `json:"totals"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode traffic: %v", err)
	}
	want := TrafficCounters{
		Requests:            3,
		InputTokens:         140,
		OutputTokens:        50,
		CacheCreationTokens: 40,
		CacheReadTokens:     120,
	}
	if response.Range != "1h" || response.Totals != want {
		t.Fatalf("traffic response range=%q totals=%+v, want %+v", response.Range, response.Totals, want)
	}

	bad := httptest.NewRecorder()
	h.apiGetTraffic(bad, httptest.NewRequest(http.MethodGet, "/admin/api/traffic?range=7d", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid range status=%d body=%s", bad.Code, bad.Body.String())
	}

	reset := httptest.NewRecorder()
	h.apiResetStats(reset, httptest.NewRequest(http.MethodPost, "/admin/api/stats/reset", nil))
	if got := tracker.Query(time.Hour).Totals; got != (TrafficCounters{}) {
		t.Fatalf("traffic was not reset: %+v", got)
	}
}

func TestStatsResetIsAtomicWithTrafficRecording(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	tracker := NewTrafficTracker(time.Hour)
	h := &Handler{traffic: tracker}

	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				h.recordSuccessForApiKey("", 1, 0, 0)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			h.apiResetStats(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/admin/api/stats/reset", nil))
		}
	}()
	wg.Wait()

	globalRequests := uint64(atomic.LoadInt64(&h.totalRequests))
	trafficRequests := tracker.Query(time.Hour).Totals.Requests
	if globalRequests != trafficRequests {
		t.Fatalf("global requests=%d traffic requests=%d", globalRequests, trafficRequests)
	}
}

// TestApiAddAccountApiKeyBranch verifies POST /admin/api/accounts with an
// api_key account normalizes AuthMethod, zeroes ExpiresAt, mirrors AccessToken,
// and skips the OAuth refresh requirement.
func TestApiAddAccountApiKeyBranch(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Stub the REST store so the background model-cache fetch fails fast without
	// real network egress.
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("test: no network")
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	h := &Handler{pool: accountpool.GetPool()}
	body := `{"kiroApiKey":"key-123","authMethod":"api_key","region":"eu-central-1","enabled":true}`
	req := httptest.NewRequest("POST", "/admin/api/accounts", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiAddAccount(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accs))
	}
	got := accs[0]
	if got.AuthMethod != "api_key" {
		t.Fatalf("AuthMethod: want api_key, got %q", got.AuthMethod)
	}
	if got.AccessToken != "key-123" {
		t.Fatalf("AccessToken mirror: want key-123, got %q", got.AccessToken)
	}
	if got.ExpiresAt != 0 {
		t.Fatalf("ExpiresAt: want 0, got %d", got.ExpiresAt)
	}
	if got.KiroApiKey != "key-123" {
		t.Fatalf("KiroApiKey: want key-123, got %q", got.KiroApiKey)
	}
	if got.RefreshToken != "" {
		t.Fatalf("RefreshToken: want empty, got %q", got.RefreshToken)
	}
}

// TestApiAddAccountApiKeyRequiresKey verifies a missing key on an api_key
// request is rejected with 400.
func TestApiAddAccountApiKeyRequiresKey(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	body := `{"authMethod":"api_key"}`
	req := httptest.NewRequest("POST", "/admin/api/accounts", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiAddAccount(rec, req)
	if rec.Code != 400 {
		t.Fatalf("expected 400 for api_key without key, got %d body=%s", rec.Code, rec.Body.String())
	}
}
