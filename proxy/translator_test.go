package proxy

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExtractOpenAIMessageTextStructured(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "alpha"},
		map[string]interface{}{"type": "input_text", "text": "beta"},
	}

	if got := extractOpenAIMessageText(content); got != "alphabeta" {
		t.Fatalf("expected concatenated structured text, got %q", got)
	}

	nested := map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "nested"}},
	}
	if got := extractOpenAIMessageText(nested); got != "nested" {
		t.Fatalf("expected nested content extraction, got %q", got)
	}
}

func TestOpenAIToKiroPreservesStructuredAssistantAndToolContent(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{
				Role: "system",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "system-a"},
					map[string]interface{}{"type": "text", "text": "system-b"},
				},
			},
			{Role: "user", Content: "first-question"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "assistant-structured"},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "tool-result-structured"},
				},
			},
		},
	}

	payload := OpenAIToKiro(req, false)

	// History starts with a priming pair.
	if len(payload.ConversationState.History) != 4 {
		t.Fatalf("expected 4 history items (2 priming + 2 conversation), got %d", len(payload.ConversationState.History))
	}

	// history[0]: priming user
	primingUser := payload.ConversationState.History[0].UserInputMessage
	if primingUser == nil {
		t.Fatalf("expected history[0] to be priming user message")
	}
	if !strings.Contains(primingUser.Content, "system-a") || !strings.Contains(primingUser.Content, "system-b") {
		t.Fatalf("expected priming user message to contain system prompt, got %q", primingUser.Content)
	}
	if strings.Contains(primingUser.Content, "first-question") {
		t.Fatalf("expected system prompt priming not to contain user question, got %q", primingUser.Content)
	}

	// history[1]: priming assistant
	primingAssistant := payload.ConversationState.History[1].AssistantResponseMessage
	if primingAssistant == nil {
		t.Fatalf("expected history[1] to be priming assistant message")
	}
	if primingAssistant.Content != "I will follow these instructions." {
		t.Fatalf("expected priming assistant ack, got %q", primingAssistant.Content)
	}

	// history[2]: first user turn
	firstConvUser := payload.ConversationState.History[2].UserInputMessage
	if firstConvUser == nil {
		t.Fatalf("expected history[2] to be first conversation user message")
	}
	if !strings.Contains(firstConvUser.Content, "first-question") {
		t.Fatalf("expected history[2] to contain first-question, got %q", firstConvUser.Content)
	}

	// history[3]: assistant reply
	historyAssistant := payload.ConversationState.History[3].AssistantResponseMessage
	if historyAssistant == nil {
		t.Fatalf("expected history[3] to be assistant message")
	}
	if historyAssistant.Content != "assistant-structured" {
		t.Fatalf("expected assistant structured content to be preserved, got %q", historyAssistant.Content)
	}

	// The tool result answers call_1, but the last history assistant has no
	// matching structured tool call (it is text-only), so the tool result is an
	// orphan. Kiro's upstream rejects structured tool results that do not answer
	// the immediately preceding assistant tool call, so it must be narrated into
	// the current message text rather than kept structured.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "tool-result-structured") {
		t.Fatalf("expected tool-result continuation content, got %q", cur.Content)
	}
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) != 0 {
		t.Fatalf("expected orphan tool result to be flattened into text, not kept structured")
	}
}

func TestOpenAIToKiroAssistantMapContentInHistory(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: map[string]interface{}{"type": "text", "text": "assistant-map"}},
			{Role: "user", Content: "u2"},
		},
	}

	payload := OpenAIToKiro(req, false)

	if len(payload.ConversationState.History) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(payload.ConversationState.History))
	}
	assistant := payload.ConversationState.History[1].AssistantResponseMessage
	if assistant == nil {
		t.Fatalf("expected second history entry to be assistant")
	}
	if assistant.Content != "assistant-map" {
		t.Fatalf("expected assistant map content preserved, got %q", assistant.Content)
	}
}

func TestOpenAIToKiroAssistantToolCallsDoNotInjectPlaceholder(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find weather"},
			{
				Role:    "assistant",
				Content: nil,
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: "{}"},
				}},
			},
			{Role: "user", Content: "continue"},
		},
	}

	payload := OpenAIToKiro(req, false)

	// The mid-history assistant turn carried ONLY a tool call (no text) and is
	// not the active tool turn, so its structured toolUses are cleared. That
	// leaves it hollow, and a hollow assistant turn is dropped entirely rather
	// than backfilled with a "." placeholder (which the model would imitate).
	// No surviving turn may contain tool-invocation text or structured toolUses.
	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil {
			continue
		}
		if len(a.ToolUses) != 0 {
			t.Fatalf("history[%d] retains structured toolUses", i)
		}
		if strings.Contains(a.Content, "get_weather") || strings.Contains(a.Content, "[Called tool") {
			t.Fatalf("history[%d] assistant contains tool-invocation text: %q", i, a.Content)
		}
		if strings.TrimSpace(a.Content) == "." || strings.TrimSpace(a.Content) == "" {
			t.Fatalf("history[%d] is a hollow assistant turn that should have been dropped", i)
		}
	}
}

func TestOpenAIConversationIDStableFromAnchor(t *testing.T) {
	baseMessages := []OpenAIMessage{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "Build calculator"},
		{Role: "assistant", Content: "Sure"},
		{Role: "user", Content: "Continue"},
	}

	reqA := &OpenAIRequest{Model: "claude-sonnet-4.5", Messages: baseMessages}
	reqB := &OpenAIRequest{Model: "claude-sonnet-4.5", Messages: append(baseMessages, OpenAIMessage{Role: "assistant", Content: "Next step"})}

	payloadA := OpenAIToKiro(reqA, false)
	payloadB := OpenAIToKiro(reqB, false)

	if payloadA.ConversationState.ConversationID == "" || payloadB.ConversationState.ConversationID == "" {
		t.Fatalf("expected non-empty conversation IDs")
	}
	if payloadA.ConversationState.ConversationID != payloadB.ConversationState.ConversationID {
		t.Fatalf("expected stable conversation ID across turns, got %q vs %q", payloadA.ConversationState.ConversationID, payloadB.ConversationState.ConversationID)
	}
}

func TestClaudeConversationIDStableFromAnchor(t *testing.T) {
	reqA := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
		},
	}
	reqB := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "next"},
		},
	}

	payloadA := ClaudeToKiro(reqA, false)
	payloadB := ClaudeToKiro(reqB, false)

	if payloadA.ConversationState.ConversationID == "" || payloadB.ConversationState.ConversationID == "" {
		t.Fatalf("expected non-empty conversation IDs")
	}
	if payloadA.ConversationState.ConversationID != payloadB.ConversationState.ConversationID {
		t.Fatalf("expected stable conversation ID across turns, got %q vs %q", payloadA.ConversationState.ConversationID, payloadB.ConversationState.ConversationID)
	}
}

func TestOpenAIConversationIDRandomForSyntheticAnchor(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "assistant", Content: "prefill"},
		},
	}

	payloadA := OpenAIToKiro(req, false)
	payloadB := OpenAIToKiro(req, false)

	if payloadA.ConversationState.ConversationID == payloadB.ConversationState.ConversationID {
		t.Fatalf("expected synthetic anchor to generate non-deterministic conversation IDs")
	}
}

func TestClaudeToKiroDropsLeadingAssistantHistory(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: "prefill"},
			{Role: "user", Content: "real user message"},
		},
	}

	payload := ClaudeToKiro(req, false)

	if len(payload.ConversationState.History) != 0 {
		t.Fatalf("expected leading assistant-only history to be dropped, got %d entries", len(payload.ConversationState.History))
	}

	if strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "Begin conversation") {
		t.Fatalf("unexpected synthetic Begin conversation injection in current content: %q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
}

func TestKiroToClaudeResponseCanEmitEmptyThinkingBlock(t *testing.T) {
	resp := KiroToClaudeResponse("final answer", "", true, nil, 10, 20, "claude-sonnet-4.6")

	if len(resp.Content) != 2 {
		t.Fatalf("expected empty thinking block plus text block, got %d blocks", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" {
		t.Fatalf("expected first block to be thinking, got %#v", resp.Content[0])
	}
	if resp.Content[0].Thinking != "" {
		t.Fatalf("expected omitted thinking block to have empty content, got %#v", resp.Content[0].Thinking)
	}
	if resp.Content[0].Signature != claudeThinkingSignaturePlaceholder {
		t.Fatalf("expected a non-empty fallback signature, got %#v", resp.Content[0].Signature)
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "final answer" {
		t.Fatalf("expected text block to be preserved, got %#v", resp.Content[1])
	}
}

func TestKiroToClaudeResponsePreservesNativeReasoningAndProtocolEnvelope(t *testing.T) {
	resp := KiroToClaudeResponseWithReasoning(
		"final answer",
		ClaudeReasoningResult{
			Text:      "native thinking",
			Signature: "real-signature",
			Redacted:  []string{"encrypted-thinking"},
		},
		false,
		[]KiroToolUse{{ToolUseID: "toolu_1", Name: "lookup", Input: map[string]interface{}{"q": "x"}}},
		10,
		20,
		"claude-sonnet-4-6",
	)

	if len(resp.Content) != 4 {
		t.Fatalf("content block count = %d, want 4: %#v", len(resp.Content), resp.Content)
	}
	if block := resp.Content[0]; block.Type != "thinking" || block.Thinking != "native thinking" || block.Signature != "real-signature" {
		t.Fatalf("thinking block = %#v", block)
	}
	if block := resp.Content[1]; block.Type != "redacted_thinking" || block.Data != "encrypted-thinking" {
		t.Fatalf("redacted block = %#v", block)
	}
	if block := resp.Content[2]; block.Type != "text" || block.Text != "final answer" {
		t.Fatalf("text block = %#v", block)
	}
	if block := resp.Content[3]; block.Type != "tool_use" || block.Caller == nil || block.Caller.Type != "direct" {
		t.Fatalf("tool block = %#v", block)
	}
	if resp.StopReason != "tool_use" || resp.StopSequence != nil || resp.StopDetails != nil {
		t.Fatalf("stop fields = reason:%q sequence:%v details:%v", resp.StopReason, resp.StopSequence, resp.StopDetails)
	}
	if len(resp.ContextManagement.AppliedEdits) != 0 {
		t.Fatalf("context management = %#v", resp.ContextManagement)
	}
	if resp.Usage.CacheCreation == nil || resp.Usage.ServiceTier != "standard" || resp.Usage.InferenceGeo != "not_available" {
		t.Fatalf("usage envelope = %#v", resp.Usage)
	}
	wantThinkingTokens := estimateClaudeOutputTokensWithRedacted("", "native thinking", []string{"encrypted-thinking"}, nil)
	if resp.Usage.OutputTokensDetails == nil || resp.Usage.OutputTokensDetails.ThinkingTokens != wantThinkingTokens {
		t.Fatalf("thinking usage = %#v, want %d", resp.Usage.OutputTokensDetails, wantThinkingTokens)
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"stop_sequence":null`,
		`"stop_details":null`,
		`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}`,
		`"context_management":{"applied_edits":[]}`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("response JSON is missing %s: %s", field, raw)
		}
	}
}

func TestKiroToOpenAIResponsePreservesReasoningTextAndToolCall(t *testing.T) {
	toolUses := []KiroToolUse{{
		ToolUseID: "call_order",
		Name:      "shell_command",
		Input:     map[string]interface{}{"command": "rg -n TODO ."},
	}}
	tests := []struct {
		format           string
		wantContent      string
		wantReasoningKey bool
	}{
		{format: "reasoning_content", wantContent: "Inspecting the repository.", wantReasoningKey: true},
		{format: "thinking", wantContent: "<thinking>Need to inspect first.</thinking>Inspecting the repository."},
		{format: "think", wantContent: "<think>Need to inspect first.</think>Inspecting the repository."},
	}

	for _, test := range tests {
		t.Run(test.format, func(t *testing.T) {
			resp := KiroToOpenAIResponseWithReasoning(
				"Inspecting the repository.",
				"Need to inspect first.",
				toolUses,
				10,
				5,
				"claude-opus-5",
				test.format,
			)
			choice := resp["choices"].([]map[string]interface{})[0]
			message := choice["message"].(map[string]interface{})
			if choice["finish_reason"] != "tool_calls" || message["content"] != test.wantContent {
				t.Fatalf("choice=%#v", choice)
			}
			if _, ok := message["tool_calls"].([]map[string]interface{}); !ok {
				t.Fatalf("tool calls missing: %#v", message)
			}
			reasoning, hasReasoning := message["reasoning_content"]
			if hasReasoning != test.wantReasoningKey || (hasReasoning && reasoning != "Need to inspect first.") {
				t.Fatalf("reasoning format mismatch: %#v", message)
			}
		})
	}

	plain := KiroToOpenAIResponse("Inspecting the repository.", toolUses, 10, 5, "claude-opus-5")
	if plain.Choices[0].Message.Content != "Inspecting the repository." || len(plain.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("plain OpenAI response lost text or tool call: %#v", plain.Choices[0].Message)
	}
}

func TestKiroToClaudeResponsePreservesSignatureOnlyThinking(t *testing.T) {
	resp := KiroToClaudeResponseWithReasoning(
		"answer",
		ClaudeReasoningResult{Signature: "real-signature"},
		false,
		nil,
		1,
		1,
		"claude-opus-4.8",
	)
	if len(resp.Content) != 2 {
		t.Fatalf("content = %#v", resp.Content)
	}
	if block := resp.Content[0]; block.Type != "thinking" || block.Thinking != "" || block.Signature != "real-signature" {
		t.Fatalf("signature-only thinking block = %#v", block)
	}
	if resp.Usage.OutputTokensDetails == nil {
		t.Fatal("signature-only response is missing thinking token details")
	}

	plain := KiroToClaudeResponse("answer", "", false, nil, 1, 1, "claude-opus-4.8")
	if plain.Usage.OutputTokensDetails == nil || plain.Usage.OutputTokensDetails.ThinkingTokens != 0 {
		t.Fatalf("plain response thinking details = %#v", plain.Usage.OutputTokensDetails)
	}
}

func TestClaudeAssistantThinkingRoundTripsIntoKiroHistory(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "thinking", "thinking": "prior reasoning", "signature": "opaque"},
				map[string]interface{}{"type": "text", "text": "prior answer"},
			}},
			{Role: "user", Content: "continue"},
		},
	}
	payload := ClaudeToKiro(req, true)
	var assistantContent string
	for _, message := range payload.ConversationState.History {
		if message.AssistantResponseMessage != nil {
			assistantContent = message.AssistantResponseMessage.Content
		}
	}
	if !strings.Contains(assistantContent, "<thinking>prior reasoning</thinking>") || !strings.Contains(assistantContent, "prior answer") {
		t.Fatalf("assistant history = %q", assistantContent)
	}
}

func TestToolResultsContinuationUsesNativeKiroContext(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find data"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "fetch", Arguments: "{}"},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "result-1"},
		},
	}

	payload := OpenAIToKiro(req, false)
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if current.Content != "" || current.UserInputMessageContext == nil ||
		len(current.UserInputMessageContext.ToolResults) != 1 ||
		kiroToolResultText(current.UserInputMessageContext.ToolResults[0]) != "result-1" {
		t.Fatalf("native continuation changed: %+v", current)
	}
}

func TestEnsureObjectSchemaMatchesKiroRSNormalization(t *testing.T) {
	input := map[string]interface{}{
		"type":                 "object",
		"required":             []interface{}{"path", 42, nil},
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":                 "string",
				"required":             nil,
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
			"options": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"force": map[string]interface{}{"type": "boolean"},
				},
			},
		},
		"anyOf": []interface{}{
			map[string]interface{}{
				"type":                 "object",
				"required":             []interface{}{"force", false},
				"additionalProperties": false,
			},
		},
	}

	got := ensureObjectSchema(input).(map[string]interface{})
	if got["required"] == nil {
		t.Fatalf("required was omitted: %#v", got)
	}
	if required, ok := got["required"].([]interface{}); !ok || len(required) != 1 || required[0] != "path" {
		t.Fatalf("top-level required=%#v, want [path]", got["required"])
	}
	if got["additionalProperties"] != false {
		t.Fatalf("top-level additionalProperties=%#v, want false", got["additionalProperties"])
	}
	nested := got["properties"].(map[string]interface{})["path"].(map[string]interface{})
	if nested["required"] != nil || nested["additionalProperties"] == nil {
		t.Fatalf("nested schema was unexpectedly normalized: %#v", nested)
	}
	options := got["properties"].(map[string]interface{})["options"].(map[string]interface{})
	if options["additionalProperties"] != false {
		t.Fatalf("nested additionalProperties was not preserved: %#v", options)
	}
	anyOf := got["anyOf"].([]interface{})[0].(map[string]interface{})
	if _, exists := anyOf["required"]; !exists {
		t.Fatalf("nested required was unexpectedly removed: %#v", anyOf)
	}
	if _, stillPresent := input["additionalProperties"]; !stillPresent {
		t.Fatalf("expected sanitizer not to mutate caller schema")
	}
}

func TestConvertOpenAIToolsSanitizesSchemaAndDescription(t *testing.T) {
	var tool OpenAITool
	tool.Type = "function"
	tool.Function.Name = "read_file"
	tool.Function.Parameters = map[string]interface{}{
		"type":                 "object",
		"required":             []string{},
		"additionalProperties": false,
	}

	tools := convertOpenAITools([]OpenAITool{tool})
	if len(tools) != 1 {
		t.Fatalf("expected one converted tool, got %d", len(tools))
	}
	if strings.TrimSpace(tools[0].ToolSpecification.Description) == "" {
		t.Fatalf("expected fallback tool description")
	}
	schema := tools[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if schema["additionalProperties"] != false {
		t.Fatalf("expected OpenAI tool schema to preserve additionalProperties, got %#v", schema)
	}
	if required, ok := schema["required"].([]interface{}); !ok || len(required) != 0 {
		t.Fatalf("expected empty required array, got %#v", schema["required"])
	}
}

func TestParseModelAndThinking(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantModel    string
		wantThinking bool
	}{
		// Format normalization: dash → dot for new versions without code changes.
		{"new opus dash form", "claude-opus-4-8", "claude-opus-4.8", false},
		{"new opus dot form", "claude-opus-4.8", "claude-opus-4.8", false},
		{"existing opus dash form", "claude-opus-4-7", "claude-opus-4.7", false},
		{"existing opus dot form", "claude-opus-4.7", "claude-opus-4.7", false},
		{"sonnet dash form", "claude-sonnet-4-6", "claude-sonnet-4.6", false},
		{"sonnet dot form", "claude-sonnet-4.6", "claude-sonnet-4.6", false},
		{"haiku dash form", "claude-haiku-4-5", "claude-haiku-4.5", false},
		{"haiku dot form", "claude-haiku-4.5", "claude-haiku-4.5", false},
		{"future major bump", "claude-sonnet-5-0", "claude-sonnet-5.0", false},

		// Bare family name passes through (no minor to normalize).
		{"bare sonnet 4", "claude-sonnet-4", "claude-sonnet-4", false},

		// Dated snapshot must hit the alias before the regex rewrites it.
		{"dated sonnet snapshot", "claude-sonnet-4-20250514", "claude-sonnet-4", false},

		// Cross-family legacy IDs.
		{"claude 3.5 sonnet", "claude-3-5-sonnet", "claude-sonnet-4.5", false},
		{"claude 3 opus", "claude-3-opus", "claude-sonnet-4.5", false},
		{"claude 3 sonnet", "claude-3-sonnet", "claude-sonnet-4", false},
		{"claude 3 haiku", "claude-3-haiku", "claude-haiku-4.5", false},

		// Non-Anthropic fallbacks.
		{"gpt-4-turbo", "gpt-4-turbo", "claude-sonnet-4.5", false},
		{"gpt-4o", "gpt-4o", "claude-sonnet-4.5", false},
		{"gpt-4", "gpt-4", "claude-sonnet-4.5", false},
		{"gpt-3.5-turbo", "gpt-3.5-turbo", "claude-sonnet-4.5", false},

		// Thinking suffix is stripped before mapping.
		{"thinking suffix on dash form", "claude-opus-4-8-thinking", "claude-opus-4.8", true},
		{"thinking suffix on dot form", "claude-sonnet-4.5-thinking", "claude-sonnet-4.5", true},
		{"thinking suffix on legacy alias", "claude-3-5-sonnet-thinking", "claude-sonnet-4.5", true},

		// Unknown models pass through unchanged.
		{"unknown model", "some-other-model", "some-other-model", false},
		{"misspelled claude family", "claude-opux-4-8", "claude-opux-4-8", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotThinking := ParseModelAndThinking(tc.input, "-thinking")
			if gotModel != tc.wantModel {
				t.Errorf("model: got %q, want %q", gotModel, tc.wantModel)
			}
			if gotThinking != tc.wantThinking {
				t.Errorf("thinking: got %v, want %v", gotThinking, tc.wantThinking)
			}
		})
	}
}

func TestParseModelAndThinkingDoesNotRewriteDatedSnapshotMinor(t *testing.T) {
	// Guards the \b boundary in claudeVersionPattern: without it, the regex would
	// rewrite "claude-sonnet-4-20250514" to "claude-sonnet-4.20250514" before the
	// alias table could redirect it.
	got, _ := ParseModelAndThinking("claude-sonnet-4-20250514", "-thinking")
	if got != "claude-sonnet-4" {
		t.Fatalf("dated snapshot must alias to claude-sonnet-4, got %q", got)
	}
	if strings.Contains(got, ".") {
		t.Fatalf("dated snapshot must not be rewritten with a dot, got %q", got)
	}
}

func TestClaudeToolResultErrorStatusMapping(t *testing.T) {
	tests := []struct {
		name            string
		isErrorFragment string
		wantStatus      string
	}{
		{name: "error", isErrorFragment: `,"is_error":true`, wantStatus: "error"},
		{name: "explicit success", isErrorFragment: `,"is_error":false`, wantStatus: "success"},
		{name: "default success", wantStatus: "success"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := `{
				"model":"claude-opus-4.8",
				"tools":[{"name":"Write","description":"write a file","input_schema":{"type":"object"}}],
				"messages":[
					{"role":"user","content":"write the file"},
					{"role":"assistant","content":[{"type":"tool_use","id":"tool_write","name":"Write","input":{"file_path":"a.js"}}]},
					{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_write","content":"Write failed"` + test.isErrorFragment + `}]}
				]
			}`

			var req ClaudeRequest
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				t.Fatalf("unmarshal Claude request: %v", err)
			}
			payload := ClaudeToKiro(&req, false)
			current := payload.ConversationState.CurrentMessage.UserInputMessage
			if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 1 {
				t.Fatalf("expected one active structured tool result: %#v", current.UserInputMessageContext)
			}
			result := current.UserInputMessageContext.ToolResults[0]
			if result.Status != test.wantStatus {
				t.Fatalf("tool result status=%q, want %q", result.Status, test.wantStatus)
			}
			if result.ToolUseID != "tool_write" || len(result.Content) != 1 || result.Content[0].Text != "Write failed" {
				t.Fatalf("tool result identity or body was lost: %#v", result)
			}

			wire, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal Kiro payload: %v", err)
			}
			if !strings.Contains(string(wire), `"status":"`+test.wantStatus+`"`) {
				t.Fatalf("Kiro wire payload omitted status %q: %s", test.wantStatus, wire)
			}
		})
	}
}

func TestClaudeToolResultErrorStatusSurvivesHistoryConversion(t *testing.T) {
	t.Run("historical result", func(t *testing.T) {
		req := &ClaudeRequest{
			Model: "claude-opus-4.8",
			Tools: []ClaudeTool{{Name: "Write", Description: "write a file", InputSchema: map[string]interface{}{"type": "object"}}},
			Messages: []ClaudeMessage{
				{Role: "user", Content: "write the file"},
				{Role: "assistant", Content: []interface{}{
					map[string]interface{}{"type": "tool_use", "id": "tool_write", "name": "Write", "input": map[string]interface{}{"file_path": "a.js"}},
				}},
				{Role: "user", Content: []interface{}{
					map[string]interface{}{"type": "tool_result", "tool_use_id": "tool_write", "content": "Write failed", "is_error": true},
				}},
				{Role: "user", Content: "continue with a corrected write"},
			},
		}

		payload := ClaudeToKiro(req, false)
		rounds := collectNativeKiroToolRounds(t, payload)
		if len(rounds) != 1 || rounds[0].Results[0].Status != "error" ||
			kiroToolResultText(rounds[0].Results[0]) != "Write failed" {
			t.Fatalf("native history lost tool failure status or body: %+v", rounds)
		}
	})

	t.Run("orphaned current result", func(t *testing.T) {
		req := &ClaudeRequest{
			Model: "claude-opus-4.8",
			Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "tool_orphan", "content": "permission denied", "is_error": true},
			}}},
		}

		content := ClaudeToKiro(req, false).ConversationState.CurrentMessage.UserInputMessage.Content
		if !strings.Contains(content, `<tool_result status="error">`) || !strings.Contains(content, "permission denied") {
			t.Fatalf("flattened orphan lost tool failure status or body: %q", content)
		}
	})
}

func TestClaudeParallelToolResultStatusesRemainIndependent(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Tools: []ClaudeTool{
			{Name: "Read", Description: "read a file", InputSchema: map[string]interface{}{"type": "object"}},
			{Name: "Write", Description: "write a file", InputSchema: map[string]interface{}{"type": "object"}},
		},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "update the files"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "tool_read", "name": "Read", "input": map[string]interface{}{"file_path": "a.js"}},
				map[string]interface{}{"type": "tool_use", "id": "tool_write", "name": "Write", "input": map[string]interface{}{"file_path": "b.js"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "tool_read", "content": "read ok"},
				map[string]interface{}{"type": "tool_result", "tool_use_id": "tool_write", "content": "permission denied", "is_error": true},
			}},
		},
	}

	current := ClaudeToKiro(req, false).ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 2 {
		t.Fatalf("expected two active structured results: %#v", current.UserInputMessageContext)
	}
	results := make(map[string]KiroToolResult, 2)
	for _, result := range current.UserInputMessageContext.ToolResults {
		results[result.ToolUseID] = result
	}
	if results["tool_read"].Status != "success" || results["tool_read"].Content[0].Text != "read ok" {
		t.Fatalf("successful result changed: %#v", results["tool_read"])
	}
	if results["tool_write"].Status != "error" || results["tool_write"].Content[0].Text != "permission denied" {
		t.Fatalf("failed result changed or crossed calls: %#v", results["tool_write"])
	}
}

func TestOpenAIToolResultExplicitFailureStatusesRemainIndependent(t *testing.T) {
	raw := `{
		"model":"claude-opus-4.8",
		"tools":[{"type":"function","function":{"name":"shell_command","parameters":{"type":"object"}}}],
		"messages":[
			{"role":"user","content":"run checks"},
			{"role":"assistant","tool_calls":[
				{"id":"call_ok","type":"function","function":{"name":"shell_command","arguments":"{}"}},
				{"id":"call_error","type":"function","function":{"name":"shell_command","arguments":"{}"}},
				{"id":"call_failed","type":"function","function":{"name":"shell_command","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_ok","content":"ok"},
			{"role":"tool","tool_call_id":"call_error","content":"permission denied","is_error":true},
			{"role":"tool","tool_call_id":"call_failed","content":"exit 1","status":"failed"}
		]
	}`
	var req OpenAIRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal OpenAI request: %v", err)
	}
	current := OpenAIToKiro(&req, false).ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 3 {
		t.Fatalf("expected three active tool results: %#v", current.UserInputMessageContext)
	}
	results := make(map[string]KiroToolResult, 3)
	for _, result := range current.UserInputMessageContext.ToolResults {
		results[result.ToolUseID] = result
	}
	if results["call_ok"].Status != "success" || results["call_ok"].Content[0].Text != "ok" {
		t.Fatalf("successful result changed: %#v", results["call_ok"])
	}
	for _, id := range []string{"call_error", "call_failed"} {
		if results[id].Status != "error" {
			t.Fatalf("explicit failure %s was not preserved: %#v", id, results[id])
		}
	}
}

func TestBuildToolResultsContinuationPreservesEmptyAndLongErrors(t *testing.T) {
	empty := buildToolResultsContinuation([]KiroToolResult{{
		ToolUseID: "tool_empty",
		Status:    "error",
	}})
	if !strings.Contains(empty, `<tool_result status="error">`) ||
		!strings.Contains(empty, "(no output)") ||
		!strings.Contains(empty, "</tool_result>") {
		t.Fatalf("empty error result lost its failure envelope: %q", empty)
	}

	long := buildToolResultsContinuation([]KiroToolResult{{
		ToolUseID: "tool_long",
		Status:    "error",
		Content:   []KiroResultContent{{Text: strings.Repeat("界", 3000)}},
	}})
	if len(long) > 4000 {
		t.Fatalf("bounded tool result length=%d, want <= 4000", len(long))
	}
	if !utf8.ValidString(long) || !strings.Contains(long, `<tool_result status="error">`) ||
		strings.Count(long, "</tool_result>") != 1 ||
		!strings.HasSuffix(long, toolResultsContinuationSuffix) {
		t.Fatalf("long error result was truncated into a malformed envelope: %q", long)
	}
}

func TestClaudeToolResultImageAttachedToCurrentMessage(t *testing.T) {
	const imgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Tools: []ClaudeTool{{Name: "read", Description: "read a file", InputSchema: map[string]interface{}{"type": "object"}}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "read this image"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "tool_use", "id": "tool_1", "name": "read", "input": map[string]interface{}{"path": "a.png"}},
				},
			},
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tool_1",
						"content": []interface{}{
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       imgData,
								},
							},
						},
					},
				},
			},
		},
	}

	payload := ClaudeToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected tool_result image attached to current message, got %d images", len(cur.Images))
	}
	if cur.Images[0].Format != "png" || cur.Images[0].Source.Bytes != imgData {
		t.Fatalf("unexpected image payload: %+v", cur.Images[0])
	}
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected one tool result preserved")
	}
	if strings.TrimSpace(cur.UserInputMessageContext.ToolResults[0].Content[0].Text) == "" {
		t.Fatalf("expected non-empty placeholder text for image-only tool result")
	}
}

func TestClaudeToolResultMixedTextAndImage(t *testing.T) {
	const imgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tool_2",
						"content": []interface{}{
							map[string]interface{}{"type": "text", "text": "here is the screenshot"},
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       imgData,
								},
							},
						},
					},
				},
			},
		},
	}

	payload := ClaudeToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected one image extracted, got %d", len(cur.Images))
	}
	// The tool_result is orphaned: it is the only message, with no preceding
	// assistant tool_use to answer. It therefore cannot stay structured (upstream
	// rejects a dangling tool_result), so its text must be folded into the message
	// content rather than dropped — alongside the extracted image.
	if !strings.Contains(cur.Content, "here is the screenshot") {
		t.Fatalf("expected orphaned tool_result text folded into content, got %q", cur.Content)
	}
}

func TestOpenAIToolResultMixedTextAndImageOrphaned(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "look at the file"},
			{
				Role:       "tool",
				ToolCallID: "call_orph",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "here is the screenshot"},
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": dataURL}},
				},
			},
		},
	}

	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected one image extracted, got %d", len(cur.Images))
	}
	// The trailing tool message is orphaned (no matching assistant tool_call in
	// history), so its text must be folded into the message content alongside the
	// extracted image rather than dropped. Mirrors TestClaudeToolResultMixedTextAndImage.
	if !strings.Contains(cur.Content, "here is the screenshot") {
		t.Fatalf("expected orphaned tool_result text folded into content, got %q", cur.Content)
	}
}

func TestOpenAIToolResultImageAttachedToCurrentMessage(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "look at the file"},
			{
				Role:       "tool",
				ToolCallID: "call_img",
				Content: []interface{}{
					map[string]interface{}{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": dataURL},
					},
				},
			},
		},
	}

	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected tool image attached to current message, got %d", len(cur.Images))
	}
	if cur.Images[0].Format != "png" {
		t.Fatalf("expected png format, got %q", cur.Images[0].Format)
	}
}

func TestOpenAIToolResultImageCarriedWhenFollowedByUser(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "look at the file"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID:   "call_img",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: "read", Arguments: `{"path":"a.png"}`},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_img",
				Content: []interface{}{
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": dataURL}},
				},
			},
			{Role: "user", Content: "what do you see?"},
		},
	}

	payload := OpenAIToKiro(req, false)

	// The flushed tool-result entry keeps its image: sanitizeKiroHistory narrates
	// the structured ToolResults into text and clears UserInputMessageContext, but
	// leaves the attached Images in place. So count the image on the history
	// entry directly (the "look at the file" user turn carries none).
	var histImages int
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil {
			histImages += len(h.UserInputMessage.Images)
		}
	}
	if histImages != 1 {
		t.Fatalf("expected tool image carried on the flushed tool-result history entry, got %d", histImages)
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 0 {
		t.Fatalf("tool image should not leak into a later user message, got %d on current", len(cur.Images))
	}
}
