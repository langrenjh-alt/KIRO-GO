package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

type resolvedToolChoice struct {
	Mode string
	Name string
}

const agentToolProtocol = `<agent_tool_protocol>
If the task can be completed without a tool, answer the user directly. If you need to inspect, search, run, edit, or write anything before answering, emit the corresponding real structured tool call in this response. Any text preceding that tool call is limited to one short sentence naming the action you are about to take; do not state a finding, a diagnosis, a root cause, a conclusion, or a recommendation before the tool results that support it are present. Never end a response with only narration or a promise about an action you have not performed. Never invent a tool result.
</agent_tool_protocol>`

func resolveToolChoice(value interface{}) (resolvedToolChoice, error) {
	if value == nil {
		return resolvedToolChoice{Mode: "auto"}, nil
	}
	if raw, ok := value.(json.RawMessage); ok {
		if len(raw) == 0 {
			return resolvedToolChoice{Mode: "auto"}, nil
		}
		var decoded interface{}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return resolvedToolChoice{}, fmt.Errorf("tool_choice must be valid JSON")
		}
		return resolveToolChoice(decoded)
	}
	if mode, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(mode)) {
		case "", "auto":
			return resolvedToolChoice{Mode: "auto"}, nil
		case "none":
			return resolvedToolChoice{Mode: "none"}, nil
		case "required", "any":
			return resolvedToolChoice{Mode: "required"}, nil
		default:
			return resolvedToolChoice{}, fmt.Errorf("tool_choice must be one of: auto, none, required")
		}
	}

	choice, ok := value.(map[string]interface{})
	if !ok {
		return resolvedToolChoice{}, fmt.Errorf("tool_choice has an unsupported shape")
	}
	choiceType, _ := choice["type"].(string)
	switch strings.ToLower(strings.TrimSpace(choiceType)) {
	case "", "auto":
		return resolvedToolChoice{Mode: "auto"}, nil
	case "none":
		return resolvedToolChoice{Mode: "none"}, nil
	case "any", "required":
		return resolvedToolChoice{Mode: "required"}, nil
	case "tool":
		name, _ := choice["name"].(string)
		if strings.TrimSpace(name) == "" {
			return resolvedToolChoice{}, fmt.Errorf("tool_choice.name is required when type is tool")
		}
		return resolvedToolChoice{Mode: "specific", Name: name}, nil
	case "function":
		name, _ := choice["name"].(string)
		if nested, ok := choice["function"].(map[string]interface{}); ok {
			if nestedName, ok := nested["name"].(string); ok {
				name = nestedName
			}
		}
		if strings.TrimSpace(name) == "" {
			return resolvedToolChoice{}, fmt.Errorf("tool_choice function name is required")
		}
		return resolvedToolChoice{Mode: "specific", Name: name}, nil
	default:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(choiceType)), "web_search") {
			return resolvedToolChoice{Mode: "specific", Name: webSearchToolName}, nil
		}
		return resolvedToolChoice{}, fmt.Errorf("tool_choice.type must be one of: auto, none, any, required, tool, function")
	}
}

func validateResolvedToolChoice(choice resolvedToolChoice, names []string) string {
	if choice.Mode == "auto" || choice.Mode == "none" {
		return ""
	}
	if len(names) == 0 {
		return "tool_choice requires at least one tool"
	}
	if choice.Mode != "specific" {
		return ""
	}
	for _, name := range names {
		if name == choice.Name {
			return ""
		}
	}
	return fmt.Sprintf("tool_choice references unknown tool %q", choice.Name)
}

func toolChoiceInstruction(choice resolvedToolChoice) string {
	switch choice.Mode {
	case "required":
		return "<tool_choice>Call at least one available tool before answering.</tool_choice>"
	case "specific":
		name := shortenToolName(sanitizeToolName(choice.Name))
		encoded, _ := json.Marshal(name)
		return "<tool_choice>You must call the tool named " + string(encoded) + " before answering.</tool_choice>"
	default:
		return ""
	}
}

func claudeAgentToolProtocolInstruction(req *ClaudeRequest, choice resolvedToolChoice) string {
	if req == nil {
		return ""
	}

	toolNames := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	return codingAgentToolProtocolInstruction(
		choice,
		isCodingAgentSystemPrompt(extractSystemPrompt(req.System)),
		toolNames,
	)
}

func openAIAgentToolProtocolInstruction(req *OpenAIRequest, choice resolvedToolChoice) string {
	if req == nil {
		return ""
	}

	var systemPrompt string
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			systemPrompt = appendSystemInstruction(systemPrompt, extractOpenAIMessageText(msg.Content))
		}
	}

	toolNames := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		toolNames = append(toolNames, openAIToolName(tool))
	}
	return codingAgentToolProtocolInstruction(choice, isCodingAgentSystemPrompt(systemPrompt), toolNames)
}

func codingAgentToolProtocolInstruction(choice resolvedToolChoice, recognizedHarness bool, toolNames []string) string {
	if choice.Mode != "auto" || len(toolNames) == 0 {
		return ""
	}

	matchedTools := 0
	hasExecutionOrMutation := false
	for _, name := range toolNames {
		capability := codingAgentToolCapability(name)
		if capability == 0 {
			continue
		}
		matchedTools++
		if capability&(codingAgentToolExecute|codingAgentToolMutate) != 0 {
			hasExecutionOrMutation = true
		}
	}

	// A known coding harness needs only one recognizable agent tool. For an
	// unknown client, require either a shell/edit tool or multiple inspection
	// tools so ordinary single-tool API requests keep their original prompt.
	if matchedTools == 0 || (!recognizedHarness && !hasExecutionOrMutation && matchedTools < 2) {
		return ""
	}
	return agentToolProtocol
}

func codingAgentCompletionProtocolEnabled(choice resolvedToolChoice, recognizedHarness bool, toolNames []string) bool {
	if choice.Mode != "auto" || !recognizedHarness {
		return false
	}
	for _, name := range toolNames {
		if codingAgentToolCapability(name) != 0 {
			return true
		}
	}
	return false
}

const (
	codingAgentToolInspect = 1 << iota
	codingAgentToolExecute
	codingAgentToolMutate
)

func codingAgentToolCapability(name string) int {
	normalized := normalizeAgentToolName(name)
	if normalized == "" {
		return 0
	}
	parts := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(name)), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	lastPart := ""
	if len(parts) > 0 {
		lastPart = normalizeAgentToolName(parts[len(parts)-1])
	}

	for _, exact := range []string{"read", "glob", "grep", "search"} {
		if normalized == exact || lastPart == exact {
			return codingAgentToolInspect
		}
	}
	for _, suffix := range []string{
		"readfile", "readfiles", "searchfiles",
		"findfiles", "listfiles", "listdirectory", "listdir", "viewfile",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return codingAgentToolInspect
		}
	}
	for _, exact := range []string{"bash", "shell", "exec", "terminal"} {
		if normalized == exact || lastPart == exact {
			return codingAgentToolExecute
		}
	}
	for _, suffix := range []string{
		"shellcommand", "execcommand", "executecommand", "runcommand",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return codingAgentToolExecute
		}
	}
	for _, exact := range []string{"write", "edit", "patch"} {
		if normalized == exact || lastPart == exact {
			return codingAgentToolMutate
		}
	}
	for _, suffix := range []string{
		"writefile", "editfile", "applypatch", "replacefile", "createfile",
		"notebookedit", "strreplaceeditor",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return codingAgentToolMutate
		}
	}
	return 0
}

func normalizeAgentToolName(name string) string {
	var normalized strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			normalized.WriteRune(r)
		}
	}
	return normalized.String()
}

func isCodingAgentSystemPrompt(prompt string) bool {
	if isClaudeCodeSystemPrompt(prompt) {
		return true
	}

	lower := strings.ToLower(prompt)
	markers := []string{
		"you are codex, a coding agent",
		"you are a coding agent",
		"you are an ai coding agent",
		"you are a software engineering agent",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func filterClaudeToolsForChoice(tools []ClaudeTool, choice resolvedToolChoice) []ClaudeTool {
	if choice.Mode == "none" {
		return nil
	}
	if choice.Mode != "specific" {
		return tools
	}
	for _, tool := range tools {
		if tool.Name == choice.Name {
			return []ClaudeTool{tool}
		}
	}
	return nil
}

func filterOpenAIToolsForChoice(tools []OpenAITool, choice resolvedToolChoice) []OpenAITool {
	if choice.Mode == "none" {
		return nil
	}
	if choice.Mode != "specific" {
		return tools
	}
	for _, tool := range tools {
		if openAIToolName(tool) == choice.Name {
			return []OpenAITool{tool}
		}
	}
	return nil
}

func openAIToolName(tool OpenAITool) string {
	if name := strings.TrimSpace(tool.Function.Name); name != "" {
		return name
	}
	if isResponsesWebSearchTool(tool) {
		return webSearchToolName
	}
	return ""
}
