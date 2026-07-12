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
		if tool.Function.Name == choice.Name {
			return []OpenAITool{tool}
		}
	}
	return nil
}
