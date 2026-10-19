package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestKiroToolSchemaCompactionPreservesValidationAndLiteralData(t *testing.T) {
	literal := map[string]interface{}{
		"description": "application data, not documentation",
		"title":       "a title property", "examples": []interface{}{"literal"}, "$comment": "literal comment",
	}
	schema := map[string]interface{}{
		"type": "object", "description": strings.Repeat("root guidance ", 100), "title": "Root", "$comment": "docs",
		"properties": map[string]interface{}{
			"description": map[string]interface{}{"type": "string", "description": "A real field", "minLength": 2},
			"title":       map[string]interface{}{"type": "string", "const": "must stay"},
			"examples":    map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			"payload":     map[string]interface{}{"type": "object", "default": literal, "enum": []interface{}{literal}, "const": literal},
			"count":       map[string]interface{}{"type": "integer", "minimum": json.Number("9007199254740993"), "default": json.Number("9007199254740995")},
		},
		"required": []interface{}{"description", "title", "examples"}, "additionalProperties": false,
		"$schema": "http://json-schema.org/draft-07/schema#", "$id": "urn:test", "$ref": "#/$defs/withRules",
		"$defs":             map[string]interface{}{"withRules": map[string]interface{}{"type": "object", "description": strings.Repeat("definition ", 100), "title": "Definition"}},
		"definitions":       map[string]interface{}{"legacy": map[string]interface{}{"type": "string", "pattern": "^valid$", "title": "legacy"}},
		"patternProperties": map[string]interface{}{"^x-": map[string]interface{}{"type": "number", "maximum": 5, "description": strings.Repeat("pattern ", 100)}},
		"allOf":             []interface{}{map[string]interface{}{"properties": map[string]interface{}{"mode": map[string]interface{}{"enum": []interface{}{"one", "two"}, "title": "Mode"}}}},
		"if":                map[string]interface{}{"required": []interface{}{"payload"}, "description": strings.Repeat("condition ", 100)},
		"then":              map[string]interface{}{"required": []interface{}{"count"}},
		"else":              map[string]interface{}{"not": map[string]interface{}{"required": []interface{}{"count"}}},
		"dependencies":      map[string]interface{}{"description": []interface{}{"title", "examples"}, "payload": map[string]interface{}{"minProperties": 2, "title": "Dep"}},
		"dependentSchemas":  map[string]interface{}{"title": map[string]interface{}{"minProperties": 1, "title": "Dependent"}},
		"x-custom-data":     literal,
	}
	payload := kiroCompactionFixture([]KiroToolWrapper{{}})
	tool := &payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0]
	tool.ToolSpecification.Name = "literal_guard"
	tool.ToolSpecification.Description = strings.Repeat("Tool guidance 中文。 ", 80)
	tool.ToolSpecification.InputSchema.JSON = schema
	original, _ := marshalUpstreamJSON(payload)
	candidate, stats, err := compactKiroToolContext(payload, 256)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ToolContextBytesSaved <= 0 || stats.CompactedToolDescriptions != 1 || stats.CompactedSchemaDescriptions < 3 || stats.RemovedSchemaAnnotations < 5 {
		t.Fatalf("unexpected compaction stats: %+v", stats)
	}
	compacted := candidate.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0].ToolSpecification
	if compacted.Name != "literal_guard" || !strings.Contains(compacted.Description, kiroShortenedDescriptionNote) || !utf8.ValidString(compacted.Description) || len(compacted.Description) > 256 {
		t.Fatalf("invalid compacted tool: %#v", compacted)
	}
	if !reflect.DeepEqual(kiroValidationProjection(schema), kiroValidationProjection(compacted.InputSchema.JSON)) {
		t.Fatal("validation schema or literal application data changed")
	}
	unchanged, _ := marshalUpstreamJSON(payload)
	if !bytes.Equal(original, unchanged) {
		t.Fatal("source payload/schema alias changed")
	}
	first, _ := marshalUpstreamJSON(candidate)
	second, _, err := compactKiroToolContext(candidate, 256)
	if err != nil {
		t.Fatal(err)
	}
	secondWire, _ := marshalUpstreamJSON(second)
	if !bytes.Equal(first, secondWire) {
		t.Fatal("schema compaction is not idempotent")
	}
}

func TestKiroPayloadPreflightCompacts249ToolsAfterSevenMBHistory(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	t.Setenv("KIRO_INPUT_TOKEN_RESERVE", "8192")
	tools := make([]KiroToolWrapper, 249)
	for i := range tools {
		tools[i].ToolSpecification.Name = fmt.Sprintf("mcp_tool_%03d", i)
		tools[i].ToolSpecification.Description = strings.Repeat("D", 690)
		tools[i].ToolSpecification.InputSchema.JSON = map[string]interface{}{
			"type": "object", "description": strings.Repeat("S", 2300),
			"properties": map[string]interface{}{"mode": map[string]interface{}{
				"type": "string", "description": strings.Repeat("P", 500), "enum": []interface{}{"summary", "full"}, "default": "summary",
			}},
			"required": []interface{}{"mode"}, "additionalProperties": false,
			"$defs": map[string]interface{}{"source": map[string]interface{}{"type": "integer", "minimum": 0}},
		}
	}
	payload := kiroCompactionFixture(tools)
	history := payload.ConversationState.History[:2]
	for i := 0; i < 155; i++ {
		history = append(history,
			KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: strings.Repeat("H", 19800), ModelID: "claude-opus-4.8", Origin: "AI_EDITOR"}},
			KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: strings.Repeat("A", 19800)}},
		)
	}
	history = append(history, KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: "run active tool", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR"}})
	history = append(history, payload.ConversationState.History[2])
	payload.ConversationState.History = history
	originalSchemas, _ := marshalUpstreamJSON(tools)
	originalCurrent := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	originalResults, _ := marshalUpstreamJSON(payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults)
	before, _ := measureKiroPayloadBreakdown(payload)
	minimal, dropped, fits, err := fitKiroPayloadByHistory(payload, 921600, 991808)
	if err != nil {
		t.Fatal(err)
	}
	minimalBefore, _ := measureKiroPayloadBreakdown(minimal)
	if before.TotalBytes < 7_000_000 || minimalBefore.TotalBytes < 950_000 || minimalBefore.TotalBytes > 1_000_000 || fits || dropped != 311 {
		t.Fatalf("fixture does not reproduce user case: original=%+v minimal=%+v dropped=%d fits=%t", before, minimalBefore, dropped, fits)
	}
	result, err := preflightKiroPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.After.TotalBytes > 921600 || result.After.EstimatedInputTokens > 991808 || result.After.ToolCount != 249 || result.After.SchemaCount != 249 || result.ToolContextBytesSaved < 40_000 {
		t.Fatalf("compaction failed limits/tools invariant: %+v", result)
	}
	if result.CompactedSchemaDescriptions != 249 || result.CompactedToolDescriptions != 0 {
		t.Fatalf("unnecessary or missing description changes: %+v", result)
	}
	assertKiroSystemPriming(t, payload, "SYSTEM MUST STAY")
	last := payload.ConversationState.History[len(payload.ConversationState.History)-1].AssistantResponseMessage
	if last == nil || len(last.ToolUses) != 1 || last.ToolUses[0].ToolUseID != "active-tool" {
		t.Fatal("active tool call not preserved adjacent to result")
	}
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	afterResults, _ := marshalUpstreamJSON(current.UserInputMessageContext.ToolResults)
	if current.Content != originalCurrent || !bytes.Equal(originalResults, afterResults) {
		t.Fatal("current user request or tool result changed")
	}
	for i, tool := range current.UserInputMessageContext.Tools {
		if tool.ToolSpecification.Name != tools[i].ToolSpecification.Name || !reflect.DeepEqual(kiroValidationProjection(tools[i].ToolSpecification.InputSchema.JSON), kiroValidationProjection(tool.ToolSpecification.InputSchema.JSON)) {
			t.Fatalf("tool %d changed name or validation schema", i)
		}
	}
	unchanged, _ := marshalUpstreamJSON(tools)
	if !bytes.Equal(originalSchemas, unchanged) {
		t.Fatal("shared source tool schemas were mutated")
	}
	wire, _ := marshalUpstreamJSON(payload)
	second, err := preflightKiroPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	secondWire, _ := marshalUpstreamJSON(payload)
	if !bytes.Equal(wire, secondWire) || second.DroppedHistoryEntries != 0 || second.ToolContextBytesSaved != 0 {
		t.Fatal("preflight changed an already fitted payload")
	}
	t.Logf("original_bytes=%d minimal_before_bytes=%d final_bytes=%d tools=%d schemas=%d dropped_history=%d tool_context_bytes_saved=%d", before.TotalBytes, minimalBefore.TotalBytes, result.After.TotalBytes, result.After.ToolCount, result.After.SchemaCount, result.DroppedHistoryEntries, result.ToolContextBytesSaved)
}

func TestKiroPayloadPreflightDoesNotCompactFittingToolDocumentation(t *testing.T) {
	payload := kiroCompactionFixture([]KiroToolWrapper{{}})
	tool := &payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0]
	tool.ToolSpecification.Name = "small"
	tool.ToolSpecification.Description = strings.Repeat("do not shorten ", 350)
	tool.ToolSpecification.InputSchema.JSON = map[string]interface{}{"type": "object", "title": "Keep", "description": strings.Repeat("full description ", 350), "examples": []interface{}{map[string]interface{}{}}}
	before, _ := marshalUpstreamJSON(payload)
	if _, err := preflightKiroPayload(payload); err != nil {
		t.Fatal(err)
	}
	after, _ := marshalUpstreamJSON(payload)
	if !bytes.Equal(before, after) {
		t.Fatal("fitting request documentation was needlessly modified")
	}
}

func TestKiroToolSchemaCompactionPreservesExternalReferenceTargets(t *testing.T) {
	for _, reference := range []string{"urn:kiro:external", "other.json", "other.json#/examples/0"} {
		t.Run(reference, func(t *testing.T) {
			tools := make([]KiroToolWrapper, 2)
			tools[0].ToolSpecification.Name = "source"
			tools[0].ToolSpecification.Description = strings.Repeat("source guidance ", 100)
			tools[0].ToolSpecification.InputSchema.JSON = map[string]interface{}{"$ref": reference, "description": strings.Repeat("schema guidance ", 100)}
			tools[1].ToolSpecification.Name = "target"
			tools[1].ToolSpecification.InputSchema.JSON = map[string]interface{}{
				"title": "Keep target annotations", "description": strings.Repeat("target documentation ", 100),
				"examples": []interface{}{map[string]interface{}{"$id": reference, "type": "string", "enum": []interface{}{"valid"}}},
			}
			candidate, stats, err := compactKiroToolContext(kiroCompactionFixture(tools), 256)
			if err != nil {
				t.Fatal(err)
			}
			if stats.CompactedToolDescriptions != 1 || stats.CompactedSchemaDescriptions != 0 || stats.RemovedSchemaAnnotations != 0 {
				t.Fatalf("external ref annotation targets not protected: %+v", stats)
			}
			for i, tool := range candidate.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools {
				before, _ := marshalUpstreamJSON(tools[i].ToolSpecification.InputSchema.JSON)
				after, _ := marshalUpstreamJSON(tool.ToolSpecification.InputSchema.JSON)
				if !bytes.Equal(before, after) {
					t.Fatalf("schema %d changed external reference/target: %s => %s", i, before, after)
				}
			}
		})
	}
}

func kiroCompactionFixture(tools []KiroToolWrapper) *KiroPayload {
	payload := &KiroPayload{}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = "schema-compaction"
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "SYSTEM MUST STAY", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{ToolUses: []KiroToolUse{{ToolUseID: "active-tool", Name: "inspect", Input: map[string]interface{}{"mode": "summary"}}}}},
	}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "CURRENT REQUEST MUST STAY", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{
			Tools: tools, ToolResults: []KiroToolResult{{ToolUseID: "active-tool", Status: "success", Content: []KiroResultContent{{Text: "RESULT MUST STAY"}}}},
		},
	}
	return payload
}

// Produce a canonical validation-only schema while treating arbitrary literal
// application data as opaque. Reuse the keyword-aware walker with cap=0 after
// removing only description annotations at known schema nodes.
func kiroValidationProjection(schema interface{}) string {
	wire, _ := marshalUpstreamJSON(schema)
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.UseNumber()
	var clone interface{}
	_ = decoder.Decode(&clone)
	stripKiroValidationDescriptions(clone)
	canonical, _ := marshalUpstreamJSON(clone)
	return string(canonical)
}

func stripKiroValidationDescriptions(value interface{}) {
	schema, ok := value.(map[string]interface{})
	if !ok {
		return
	}
	delete(schema, "description")
	compactKiroSchemaAnnotations(schema, 0, &kiroToolContextCompaction{})
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies"} {
		if entries, ok := schema[keyword].(map[string]interface{}); ok {
			for _, child := range entries {
				stripKiroValidationDescriptions(child)
			}
		}
	}
	for _, keyword := range []string{"additionalProperties", "unevaluatedProperties", "additionalItems", "unevaluatedItems", "contains", "propertyNames", "not", "if", "then", "else", "contentSchema"} {
		stripKiroValidationDescriptions(schema[keyword])
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems", "items"} {
		if children, ok := schema[keyword].([]interface{}); ok {
			for _, child := range children {
				stripKiroValidationDescriptions(child)
			}
		} else {
			stripKiroValidationDescriptions(schema[keyword])
		}
	}
}
