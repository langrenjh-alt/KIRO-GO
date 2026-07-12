package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveToolChoiceShapes(t *testing.T) {
	tests := []struct {
		name     string
		value    interface{}
		wantMode string
		wantName string
	}{
		{name: "Claude any", value: map[string]interface{}{"type": "any"}, wantMode: "required"},
		{name: "Claude tool", value: map[string]interface{}{"type": "tool", "name": "lookup"}, wantMode: "specific", wantName: "lookup"},
		{name: "OpenAI required", value: "required", wantMode: "required"},
		{name: "OpenAI function", value: map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "lookup"}}, wantMode: "specific", wantName: "lookup"},
		{name: "Responses function", value: json.RawMessage(`{"type":"function","name":"lookup"}`), wantMode: "specific", wantName: "lookup"},
		{name: "none", value: "none", wantMode: "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveToolChoice(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if got.Mode != tc.wantMode || got.Name != tc.wantName {
				t.Fatalf("choice = %#v", got)
			}
		})
	}
}

func TestToolChoiceFiltersAndInstruction(t *testing.T) {
	choice := resolvedToolChoice{Mode: "specific", Name: "lookup"}
	tools := []ClaudeTool{{Name: "other"}, {Name: "lookup"}}
	filtered := filterClaudeToolsForChoice(tools, choice)
	if len(filtered) != 1 || filtered[0].Name != "lookup" {
		t.Fatalf("filtered tools = %#v", filtered)
	}
	if instruction := toolChoiceInstruction(choice); !strings.Contains(instruction, `"lookup"`) {
		t.Fatalf("instruction = %q", instruction)
	}
}

func TestToolChoiceReachesKiroPayload(t *testing.T) {
	claudeTools := []ClaudeTool{
		{Name: "other", Description: "other", InputSchema: map[string]interface{}{"type": "object"}},
		{Name: "lookup", Description: "lookup", InputSchema: map[string]interface{}{"type": "object"}},
	}
	claudeReq := &ClaudeRequest{
		Model:      "claude-sonnet-4.6",
		Messages:   []ClaudeMessage{{Role: "user", Content: "test"}},
		Tools:      claudeTools,
		ToolChoice: map[string]interface{}{"type": "tool", "name": "lookup"},
	}
	payload := ClaudeToKiro(claudeReq, false)
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.Tools) != 1 || context.Tools[0].ToolSpecification.Name != "lookup" {
		t.Fatalf("Claude forced tools = %#v", context)
	}
	if priming := structuredOutputPriming(t, payload); !strings.Contains(priming, `"lookup"`) {
		t.Fatalf("Claude tool choice priming = %q", priming)
	}

	var openAITool OpenAITool
	openAITool.Type = "function"
	openAITool.Function.Name = "lookup"
	openAITool.Function.Parameters = map[string]interface{}{"type": "object"}
	openAIReq := &OpenAIRequest{
		Model:      "claude-sonnet-4.6",
		Messages:   []OpenAIMessage{{Role: "user", Content: "test"}},
		Tools:      []OpenAITool{openAITool},
		ToolChoice: "none",
	}
	openAIPayload := OpenAIToKiro(openAIReq, false)
	if context := openAIPayload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext; context != nil && len(context.Tools) != 0 {
		t.Fatalf("none tool choice leaked tools: %#v", context.Tools)
	}
}
