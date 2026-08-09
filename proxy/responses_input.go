package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

func parseResponsesInput(raw json.RawMessage) ([]OpenAIMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("invalid input string: %w", err)
		}
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []OpenAIMessage{{Role: "user", Content: s}}, nil
	}

	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("invalid input array: %w", err)
		}
		return convertResponsesInputItems(items)
	}

	if trimmed[0] == '{' {
		return convertResponsesInputItems([]json.RawMessage{raw})
	}

	return nil, fmt.Errorf("unsupported input shape")
}

func convertResponsesInputItems(items []json.RawMessage) ([]OpenAIMessage, error) {
	messages := make([]OpenAIMessage, 0, len(items))
	pendingUserParts := []interface{}{}
	pendingReasoning := ""

	flushPendingUser := func() {
		if len(pendingUserParts) == 0 {
			return
		}
		messages = append(messages, OpenAIMessage{
			Role:    "user",
			Content: pendingUserParts,
		})
		pendingUserParts = nil
	}
	attachPendingReasoning := func(msg *OpenAIMessage) {
		if msg == nil || strings.TrimSpace(pendingReasoning) == "" {
			return
		}
		msg.ReasoningContent = appendResponsesReasoning(msg.ReasoningContent, pendingReasoning)
		pendingReasoning = ""
	}
	flushPendingReasoning := func() {
		if strings.TrimSpace(pendingReasoning) == "" {
			return
		}
		msg := OpenAIMessage{Role: "assistant", Content: ""}
		attachPendingReasoning(&msg)
		messages = append(messages, msg)
	}

	for _, item := range items {
		var obj map[string]interface{}
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}

		typ, _ := obj["type"].(string)
		role, _ := obj["role"].(string)

		switch {
		case typ == "message" || (typ == "" && role != ""):
			messageRole := role
			if messageRole == "" {
				messageRole = "user"
			}
			if messageRole == "assistant" {
				flushPendingUser()
			} else {
				flushPendingReasoning()
				flushPendingUser()
			}
			msg := buildMessageFromInputItem(obj, messageRole)
			if msg == nil && messageRole == "assistant" && strings.TrimSpace(pendingReasoning) != "" {
				msg = &OpenAIMessage{Role: "assistant", Content: ""}
			}
			if msg != nil {
				attachPendingReasoning(msg)
				messages = append(messages, *msg)
			}

		case typ == "reasoning":
			flushPendingUser()
			pendingReasoning = appendResponsesReasoning(pendingReasoning, reasoningSummaryFromInputItem(obj))

		case typ == "function_call_output" || typ == "tool_result":
			flushPendingUser()
			flushPendingReasoning()
			callID, _ := obj["call_id"].(string)
			if callID == "" {
				callID, _ = obj["tool_call_id"].(string)
			}
			out := stringifyArbitrary(obj["output"])
			if out == "" {
				out = stringifyArbitrary(obj["content"])
			}
			status, _ := obj["status"].(string)
			isError, _ := obj["is_error"].(bool)
			messages = append(messages, OpenAIMessage{
				Role:              "tool",
				Content:           out,
				ToolCallID:        callID,
				ToolResultStatus:  status,
				ToolResultIsError: isError,
			})

		case typ == "function_call":
			flushPendingUser()
			tc := ToolCall{
				ID:   stringField(obj, "call_id", "id"),
				Type: "function",
			}
			tc.Function.Name, _ = obj["name"].(string)
			tc.Function.Arguments = stringifyArbitrary(obj["arguments"])
			// Merge consecutive function_call items into a single assistant
			// message so parallel tool calls stay grouped in one turn. The
			// Responses API emits each parallel call as a separate input item;
			// keeping them in one assistant message preserves the tool_use /
			// tool_result pairing that Kiro requires.
			if n := len(messages); n > 0 &&
				messages[n-1].Role == "assistant" &&
				len(messages[n-1].ToolCalls) > 0 &&
				strings.TrimSpace(extractOpenAIMessageText(messages[n-1].Content)) == "" {
				attachPendingReasoning(&messages[n-1])
				messages[n-1].ToolCalls = append(messages[n-1].ToolCalls, tc)
			} else {
				msg := OpenAIMessage{
					Role:      "assistant",
					Content:   "",
					ToolCalls: []ToolCall{tc},
				}
				attachPendingReasoning(&msg)
				messages = append(messages, msg)
			}

		case typ == "input_text" || typ == "text":
			flushPendingReasoning()
			text, _ := obj["text"].(string)
			if text != "" {
				pendingUserParts = append(pendingUserParts, map[string]interface{}{
					"type": "input_text",
					"text": text,
				})
			}

		case typ == "input_image", typ == "image", typ == "image_url":
			flushPendingReasoning()
			pendingUserParts = append(pendingUserParts, map[string]interface{}(obj))

		case typ == "output_text":
			flushPendingUser()
			text, _ := obj["text"].(string)
			if text != "" || strings.TrimSpace(pendingReasoning) != "" {
				msg := OpenAIMessage{Role: "assistant", Content: text}
				attachPendingReasoning(&msg)
				messages = append(messages, msg)
			}

		default:
			if role != "" {
				if role == "assistant" {
					flushPendingUser()
				} else {
					flushPendingReasoning()
					flushPendingUser()
				}
				msg := buildMessageFromInputItem(obj, role)
				if msg == nil && role == "assistant" && strings.TrimSpace(pendingReasoning) != "" {
					msg = &OpenAIMessage{Role: "assistant", Content: ""}
				}
				if msg != nil {
					attachPendingReasoning(msg)
					messages = append(messages, *msg)
				}
			}
		}
	}

	flushPendingUser()
	flushPendingReasoning()
	return messages, nil
}

func reasoningSummaryFromInputItem(obj map[string]interface{}) string {
	summary, ok := obj["summary"]
	if !ok {
		return ""
	}
	if text, ok := summary.(string); ok {
		return text
	}

	raw, err := json.Marshal(summary)
	if err != nil {
		return ""
	}
	var parts []ResponseSummaryPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	return joinResponseSummaryParts(parts)
}

func joinResponseSummaryParts(parts []ResponseSummaryPart) string {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if (part.Type == "" || part.Type == "summary_text" || part.Type == "text") && strings.TrimSpace(part.Text) != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func appendResponsesReasoning(existing, additional string) string {
	if strings.TrimSpace(additional) == "" {
		return existing
	}
	if strings.TrimSpace(existing) == "" {
		return additional
	}
	return existing + "\n" + additional
}

func buildMessageFromInputItem(obj map[string]interface{}, role string) *OpenAIMessage {
	if role == "" {
		role = "user"
	}

	if content, ok := obj["content"]; ok {
		switch v := content.(type) {
		case string:
			return &OpenAIMessage{Role: role, Content: v}
		case []interface{}:
			parts := make([]interface{}, 0, len(v))
			textOnly := strings.Builder{}
			anyNonText := false
			for _, p := range v {
				part, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				ptype, _ := part["type"].(string)
				switch ptype {
				case "input_text", "text":
					if t, ok := part["text"].(string); ok {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				case "output_text":
					if t, ok := part["text"].(string); ok {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				case "input_image", "image", "image_url":
					anyNonText = true
					parts = append(parts, part)
				default:
					if t, ok := part["text"].(string); ok && t != "" {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				}
			}
			if !anyNonText {
				return &OpenAIMessage{Role: role, Content: textOnly.String()}
			}
			return &OpenAIMessage{Role: role, Content: parts}
		case map[string]interface{}:
			return buildMessageFromInputItem(v, role)
		}
	}

	if text, ok := obj["text"].(string); ok && text != "" {
		return &OpenAIMessage{Role: role, Content: text}
	}

	return nil
}

func stringifyArbitrary(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func stringField(obj map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
