package proxy

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// A shared fixture is compiled unchanged against the production baseline and
// release source so the two reported failures have observable before/after gates.
func TestRequestCompatReleaseProbe(t *testing.T) {
	mode := os.Getenv("REQUEST_COMPAT_PROBE_MODE")
	if mode == "" {
		mode = "MODIFIED"
	}
	wantFixed := mode == "MODIFIED"
	displayErr := validateClaudeThinkingConfig(&ClaudeThinkingConfig{Type: "adaptive", Display: "updates"}, 4096)
	displayAccepted := displayErr == ""
	if displayAccepted != wantFixed {
		t.Fatalf("display acceptance=%v expected=%v error=%q", displayAccepted, wantFixed, displayErr)
	}
	if wantFixed && !resolveClaudeThinkingResponseOptions(&ClaudeThinkingConfig{Type: "adaptive", Display: "updates"}, "thinking").OmitDisplay {
		t.Fatal("updates must hide reasoning content")
	}

	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	t.Setenv("KIRO_INPUT_TOKEN_RESERVE", "8192")
	payload := &KiroPayload{}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = "release-compat-fixture"
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "retain system instructions", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
	}
	for i := 0; i < 155; i++ {
		payload.ConversationState.History = append(payload.ConversationState.History,
			KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: strings.Repeat("old context ", 1700), ModelID: "claude-opus-4.8", Origin: "AI_EDITOR"}},
			KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: strings.Repeat("old context ", 1700)}},
		)
	}
	tools := make([]KiroToolWrapper, 249)
	for i := range tools {
		tools[i].ToolSpecification.Name = fmt.Sprintf("fixture_tool_%03d", i)
		tools[i].ToolSpecification.Description = strings.Repeat("Tool operation guidance. ", 31)
		tools[i].ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{
			"type": "object", "description": strings.Repeat("Retain schema guidance. ", 110),
			"properties": map[string]interface{}{
				"description": map[string]interface{}{"type": "string", "description": strings.Repeat("Property hint. ", 20)},
				"mode":        map[string]interface{}{"type": "string", "enum": []interface{}{"keep", "skip"}, "default": "keep"},
			}, "required": []interface{}{"mode"}, "additionalProperties": false,
		}}
	}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "retain current question", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR", UserInputMessageContext: &UserInputMessageContext{Tools: tools},
	}
	before, err := measureKiroPayloadBreakdown(payload)
	if err != nil {
		t.Fatal(err)
	}
	if before.TotalBytes < 7_000_000 {
		t.Fatalf("fixture too small: %d", before.TotalBytes)
	}
	_, err = preflightKiroPayload(payload)
	payloadAccepted := err == nil
	if payloadAccepted != wantFixed {
		t.Fatalf("payload acceptance=%v expected=%v err=%v", payloadAccepted, wantFixed, err)
	}
	actual := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	if len(actual) != 249 {
		t.Fatalf("tools=%d", len(actual))
	}
	for i, tool := range actual {
		if tool.ToolSpecification.Name != fmt.Sprintf("fixture_tool_%03d", i) {
			t.Fatal("tool name changed")
		}
		schema := tool.ToolSpecification.InputSchema.JSON.(map[string]interface{})
		properties := schema["properties"].(map[string]interface{})
		if properties["description"].(map[string]interface{})["type"] != "string" {
			t.Fatal("named property removed")
		}
		param := properties["mode"].(map[string]interface{})
		if param["default"] != "keep" || len(param["enum"].([]interface{})) != 2 || len(schema["required"].([]interface{})) != 1 || schema["additionalProperties"] != false {
			t.Fatal("validation semantics changed")
		}
	}
	if payload.ConversationState.CurrentMessage.UserInputMessage.Content != "retain current question" {
		t.Fatal("current request changed")
	}
	fmt.Printf("%s: display_updates=%v payload_249=%v tools=249 validation_preserved=true exit=0\n", mode, displayAccepted, payloadAccepted)
}
