package proxy

import (
	"encoding/json"
	"math"
	"strings"
)

// omitImagePayloadForTokens removes image bytes before a token estimate.
// Base64 length is not a model token count; leaving it in the cache prefix
// reports a single picture as about a million tokens of cache creation.
// Callers that hash the prompt for a cache hit must keep the original value.
func omitImagePayloadForTokens(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		if isImageInputBlock(typed) {
			return map[string]interface{}{"type": "image"}
		}
		trimmed := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			trimmed[key] = omitImagePayloadForTokens(item)
		}
		return trimmed
	case []interface{}:
		trimmed := make([]interface{}, len(typed))
		for i, item := range typed {
			trimmed[i] = omitImagePayloadForTokens(item)
		}
		return trimmed
	case string:
		if isInlineImageDataURL(typed) {
			return ""
		}
		return typed
	default:
		return value
	}
}

func isImageInputBlock(block map[string]interface{}) bool {
	switch strings.ToLower(strings.TrimSpace(stringFromMap(block, "type"))) {
	case "image", "image_url", "input_image":
		return true
	}
	if _, ok := block["image_url"]; ok {
		return true
	}
	if _, ok := block["image_base64"]; ok {
		return true
	}
	source, _ := block["source"].(map[string]interface{})
	if source == nil {
		return false
	}
	media := stringFromMap(source, "media_type", "mediaType", "mime_type", "mimeType")
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(media)), "image/")
}

func stringFromMap(block map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if text, ok := block[key].(string); ok {
			return text
		}
	}
	return ""
}

func isInlineImageDataURL(value string) bool {
	trimmed := strings.TrimSpace(value)
	header, _, ok := strings.Cut(strings.ToLower(trimmed), ";base64,")
	return ok && strings.HasPrefix(header, "data:image/")
}

func estimateApproxTokens(text string) int {
	if text == "" {
		return 0
	}

	length := 0
	var regularAscii, digits, symbols, nonASCII int
	for _, r := range text {
		length++
		switch {
		case r >= 0x80:
			nonASCII++
		case r >= '0' && r <= '9':
			digits++
		case (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~'):
			symbols++
		default:
			regularAscii++
		}
	}
	if length == 0 {
		return 0
	}
	if length < 5 {
		return max(1, int(math.Ceil(float64(length)/3.0)))
	}

	estimated := int(math.Ceil(
		float64(regularAscii)/4.5 +
			float64(digits)/2.0 +
			float64(symbols)/1.5 +
			float64(nonASCII)/1.5,
	))

	if estimated < 1 {
		return 1
	}
	return estimated
}

func estimateClaudeRequestInputTokens(req *ClaudeRequest) int {
	if req == nil {
		return 0
	}

	total := estimateClaudeValueTokens(req.System)
	if choice, err := resolveToolChoice(req.ToolChoice); err == nil {
		total += estimateApproxTokens(toolChoiceInstruction(choice))
	}
	if req.OutputConfig != nil {
		total += estimateApproxTokens(claudeStructuredOutputInstruction(req.OutputConfig.Format))
	}
	total += estimateApproxTokens(stopSequencesInstruction(req.StopSequences))

	for _, msg := range req.Messages {
		total += estimateClaudeValueTokens(msg.Content)
	}

	for _, tool := range req.Tools {
		total += estimateApproxTokens(tool.Name)
		total += estimateApproxTokens(tool.Description)
		total += estimateJSONTokens(tool.InputSchema)
	}

	return total
}

func estimateClaudeOutputTokens(content, thinkingContent string, toolUses []KiroToolUse) int {
	return estimateClaudeOutputTokensWithRedacted(content, thinkingContent, nil, toolUses)
}

func estimateClaudeOutputTokensWithRedacted(content, thinkingContent string, redactedThinking []string, toolUses []KiroToolUse) int {
	total := estimateApproxTokens(content)
	total += estimateApproxTokens(thinkingContent)
	for _, redacted := range redactedThinking {
		total += estimateApproxTokens(redacted)
	}

	for _, tu := range toolUses {
		total += estimateApproxTokens(tu.Name)
		total += estimateJSONTokens(tu.Input)
	}

	return total
}

func estimateClaudeValueTokens(v interface{}) int {
	switch value := v.(type) {
	case nil:
		return 0
	case string:
		if isInlineImageDataURL(value) {
			return 0
		}
		return estimateApproxTokens(value)
	case []interface{}:
		total := 0
		for _, part := range value {
			total += estimateClaudeValueTokens(part)
		}
		return total
	case map[string]interface{}:
		typeName, _ := value["type"].(string)
		switch typeName {
		case "image", "image_url", "input_image":
			return 0
		case "text":
			if text, ok := value["text"].(string); ok {
				if isInlineImageDataURL(text) {
					return 0
				}
				return estimateApproxTokens(text)
			}
		case "thinking":
			if thinking, ok := value["thinking"].(string); ok {
				return estimateApproxTokens(thinking)
			}
		case "tool_use":
			total := 0
			if name, ok := value["name"].(string); ok {
				total += estimateApproxTokens(name)
			}
			if input, ok := value["input"]; ok {
				total += estimateJSONTokens(omitImagePayloadForTokens(input))
			}
			if total > 0 {
				return total
			}
		case "tool_result":
			if content, ok := value["content"]; ok {
				return estimateClaudeValueTokens(content)
			}
		}

		total := 0
		if text, ok := value["text"].(string); ok && !isInlineImageDataURL(text) {
			total += estimateApproxTokens(text)
		}
		if thinking, ok := value["thinking"].(string); ok {
			total += estimateApproxTokens(thinking)
		}
		if content, ok := value["content"]; ok {
			total += estimateClaudeValueTokens(content)
		}
		if total > 0 {
			return total
		}

		return estimateJSONTokens(omitImagePayloadForTokens(value))
	default:
		return estimateJSONTokens(omitImagePayloadForTokens(value))
	}
}

func estimateJSONTokens(v interface{}) int {
	if v == nil {
		return 0
	}

	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}

	return estimateApproxTokens(string(b))
}

func estimateOpenAIRequestInputTokens(req *OpenAIRequest) int {
	if req == nil {
		return 0
	}

	total := 0
	total += estimateApproxTokens(openAIStructuredOutputInstruction(req.ResponseFormat))
	if choice, err := resolveToolChoice(req.ToolChoice); err == nil {
		total += estimateApproxTokens(openAIToolChoiceInstruction(choice))
	}
	if stopSequences, err := resolveOpenAIStopSequences(req.Stop); err == nil {
		total += estimateApproxTokens(stopSequencesInstruction(stopSequences))
	}

	for _, msg := range req.Messages {
		total += estimateOpenAIContentTokens(msg.Content)
		total += estimateApproxTokens(msg.ToolCallID)
		for _, tc := range msg.ToolCalls {
			total += estimateApproxTokens(tc.Function.Name)
			total += estimateApproxTokens(tc.Function.Arguments)
		}
	}

	for _, tool := range req.Tools {
		total += estimateApproxTokens(tool.Function.Name)
		total += estimateApproxTokens(tool.Function.Description)
		total += estimateJSONTokens(tool.Function.Parameters)
	}

	return total
}

func estimateOpenAIContentTokens(content interface{}) int {
	switch value := content.(type) {
	case nil:
		return 0
	case string:
		if isInlineImageDataURL(value) {
			return 0
		}
		return estimateApproxTokens(value)
	default:
		// Strip image bytes before the text extractor. An image-only part has
		// no text, and the extractor otherwise JSON-encodes the whole part.
		text := extractOpenAIMessageText(omitImagePayloadForTokens(value))
		if text == "" || isInlineImageDataURL(text) {
			return 0
		}
		return estimateApproxTokens(text)
	}
}

func estimateOpenAIOutputTokens(content, reasoningContent string, toolUses []KiroToolUse) int {
	return estimateClaudeOutputTokens(content, reasoningContent, toolUses)
}
