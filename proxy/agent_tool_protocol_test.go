package proxy

import (
	"strings"
	"testing"
)

func TestClaudeCodingAgentGetsToolActionProtocol(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		System: `You are an interactive agent that helps users with software engineering tasks.
# Using your tools
You are Claude Code, Anthropic's official CLI for Claude.`,
		Messages: []ClaudeMessage{{Role: "user", Content: "inspect the repository"}},
		Tools: []ClaudeTool{{
			Name:        "Read",
			Description: "Read a file",
			InputSchema: map[string]interface{}{"type": "object"},
		}},
	}

	priming := structuredOutputPriming(t, ClaudeToKiro(req, false))
	if !strings.Contains(priming, agentToolProtocol) {
		t.Fatalf("coding-agent protocol did not survive Claude prompt conversion: %q", priming)
	}
}

func TestOpenAICodingToolSetGetsToolActionProtocol(t *testing.T) {
	req := &OpenAIRequest{
		Model:    "claude-sonnet-4.6",
		Messages: []OpenAIMessage{{Role: "user", Content: "fix the bug"}},
		Tools: []OpenAITool{
			openAITestTool("shell_command"),
			openAITestTool("apply_patch"),
		},
	}

	priming := structuredOutputPriming(t, OpenAIToKiro(req, false))
	if !strings.Contains(priming, agentToolProtocol) {
		t.Fatalf("OpenAI coding-agent protocol missing from system priming: %q", priming)
	}
	withoutProtocol := *req
	withoutProtocol.ToolChoice = "none"
	if estimateOpenAIRequestInputTokens(req) <= estimateOpenAIRequestInputTokens(&withoutProtocol) {
		t.Fatal("OpenAI token estimate did not include the coding-agent protocol")
	}
}

func TestToolActionProtocolStaysScoped(t *testing.T) {
	tests := []struct {
		name string
		req  *OpenAIRequest
	}{
		{
			name: "no tools",
			req: &OpenAIRequest{
				Messages: []OpenAIMessage{{Role: "system", Content: "You are Codex, a coding agent."}, {Role: "user", Content: "hello"}},
			},
		},
		{
			name: "ordinary simulated tool",
			req: &OpenAIRequest{
				Messages: []OpenAIMessage{
					{Role: "system", Content: "You are Codex, a coding agent."},
					{Role: "user", Content: "look this up"},
				},
				Tools: []OpenAITool{openAITestTool("lookup")},
			},
		},
		{
			name: "tool choice none",
			req: &OpenAIRequest{
				Messages:   []OpenAIMessage{{Role: "system", Content: "You are Codex, a coding agent."}, {Role: "user", Content: "inspect"}},
				Tools:      []OpenAITool{openAITestTool("shell_command")},
				ToolChoice: "none",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := OpenAIToKiro(test.req, false)
			for _, message := range payload.ConversationState.History {
				if message.UserInputMessage != nil && strings.Contains(message.UserInputMessage.Content, agentToolProtocol) {
					t.Fatalf("agent protocol leaked into an unrelated request: %q", message.UserInputMessage.Content)
				}
			}
		})
	}
}

func TestClaudeToolActionProtocolIsIncludedInTokenEstimate(t *testing.T) {
	withAgentTool := &ClaudeRequest{
		Messages: []ClaudeMessage{{Role: "user", Content: "inspect"}},
		Tools:    []ClaudeTool{{Name: "exec_command", InputSchema: map[string]interface{}{"type": "object"}}},
	}
	withoutProtocol := *withAgentTool
	withoutProtocol.ToolChoice = "none"

	if estimateClaudeRequestInputTokens(withAgentTool) <= estimateClaudeRequestInputTokens(&withoutProtocol) {
		t.Fatal("Claude token estimate did not include the coding-agent protocol")
	}
}

func openAITestTool(name string) OpenAITool {
	var tool OpenAITool
	tool.Type = "function"
	tool.Function.Name = name
	tool.Function.Description = "test tool"
	tool.Function.Parameters = map[string]interface{}{"type": "object"}
	return tool
}
