package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// This fixture deliberately uses HTML metacharacters in a validation value,
// rather than removable descriptions. Preflight must measure the exact body
// encoder used by the HTTP request, not encoding/json's HTML-escaped variant.
func TestRequestCompatReviewMeasuresActualUpstreamEncoding(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	payload := reviewToolPayload(1)
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0].ToolSpecification.InputSchema.JSON = map[string]interface{}{
		"type": "string", "enum": []interface{}{strings.Repeat("<&>", 120_000)},
	}
	actual, err := marshalUpstreamJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	escaped, _ := json.Marshal(payload)
	if len(actual) >= defaultKiroMaxRawBodyBytes || len(escaped) <= defaultKiroMaxRawBodyBytes {
		t.Fatalf("invalid boundary fixture: actual=%d escaped=%d", len(actual), len(escaped))
	}
	result, err := preflightKiroPayload(payload)
	if err != nil {
		t.Fatalf("actual upstream body fits: %v", err)
	}
	if result.Before.TotalBytes != len(actual) || result.After.TotalBytes != len(actual) {
		t.Fatalf("preflight counted a different encoding: before=%d after=%d actual=%d", result.Before.TotalBytes, result.After.TotalBytes, len(actual))
	}
	after, _ := marshalUpstreamJSON(payload)
	if !bytes.Equal(actual, after) {
		t.Fatal("request fitting the actual wire limit was changed")
	}
}

// An independently constructed 249-tool request exercises property names that
// collide with annotation keywords and literal instance data containing those
// same names. All 249 tools and all validation/default data must survive.
func TestRequestCompatReview249ToolsPreservesConstraintsAndProtectedContext(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	payload := reviewToolPayload(249)
	sourceTools := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	sourceWire, _ := json.Marshal(sourceTools)
	protectedHistory, _ := json.Marshal(payload.ConversationState.History)
	resultsWire, _ := json.Marshal(payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults)
	before, _ := marshalUpstreamJSON(payload)
	if len(before) <= defaultKiroMaxRawBodyBytes {
		t.Fatalf("fixture does not exercise fixed tool pressure: %d bytes", len(before))
	}
	result, err := preflightKiroPayload(payload)
	if err != nil {
		t.Fatalf("249 tools with compressible annotations should fit: %v", err)
	}
	tools := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	if len(tools) != 249 {
		t.Fatalf("tools lost: %d", len(tools))
	}
	for i := range tools {
		want := sourceTools[i].ToolSpecification
		got := tools[i].ToolSpecification
		if got.Name != want.Name {
			t.Fatalf("tool %d renamed: %q -> %q", i, want.Name, got.Name)
		}
		if !utf8.ValidString(got.Description) {
			t.Fatalf("tool %d has a partial UTF-8 description", i)
		}
		wantValidation := reviewValidationFields(t, want.InputSchema.JSON)
		gotValidation := reviewValidationFields(t, got.InputSchema.JSON)
		if !bytes.Equal(wantValidation, gotValidation) {
			t.Fatalf("tool %d lost validation/default data:\nwant %s\ngot  %s", i, wantValidation, gotValidation)
		}
	}
	unchangedSource, _ := json.Marshal(sourceTools)
	if !bytes.Equal(sourceWire, unchangedSource) {
		t.Fatal("compaction mutated caller-owned tools or schema maps through an alias")
	}
	afterHistory, _ := json.Marshal(payload.ConversationState.History)
	if !bytes.Equal(protectedHistory, afterHistory) {
		t.Fatal("fixed-context compaction changed protected system/active tool history")
	}
	afterResults, _ := json.Marshal(payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults)
	if !bytes.Equal(resultsWire, afterResults) || payload.ConversationState.CurrentMessage.UserInputMessage.Content != "CURRENT REQUEST MUST STAY" {
		t.Fatal("compaction changed the current request or its tool result")
	}
	after, _ := marshalUpstreamJSON(payload)
	if len(after) > defaultKiroMaxRawBodyBytes || result.After.TotalBytes != len(after) {
		t.Fatalf("compaction size mismatch: actual=%d measured=%d limit=%d", len(after), result.After.TotalBytes, defaultKiroMaxRawBodyBytes)
	}
	if _, err := preflightKiroPayload(payload); err != nil {
		t.Fatalf("second preflight: %v", err)
	}
	second, _ := marshalUpstreamJSON(payload)
	if !bytes.Equal(after, second) {
		t.Fatal("successful compaction is not idempotent")
	}
	t.Logf("independent fixture tools=249 before_bytes=%d after_bytes=%d limit=%d protected_history=unchanged defaults_and_constraints=unchanged", len(before), len(after), defaultKiroMaxRawBodyBytes)
}

func TestRequestCompatReviewOversizedConstraintStillRejectsWithoutMutation(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	payload := reviewToolPayload(1)
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0].ToolSpecification.InputSchema.JSON = map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{
			"description": map[string]interface{}{"const": strings.Repeat("cannot remove a constraint", 40_000)},
		}, "required": []string{"description"}, "additionalProperties": false,
	}
	before, _ := marshalUpstreamJSON(payload)
	_, err := preflightKiroPayload(payload)
	var local *KiroPayloadPreflightError
	if !errors.As(err, &local) || !isKiroClientRequestError(err) {
		t.Fatalf("error=%T %v, want non-retryable local request error", err, err)
	}
	after, _ := marshalUpstreamJSON(payload)
	if !bytes.Equal(before, after) {
		t.Fatal("an impossible request was partially rewritten despite rejection")
	}
	if strings.Contains(err.Error(), "cannot remove a constraint") {
		t.Fatal("numeric-only preflight diagnostics leaked request content")
	}
}

func TestRequestCompatReviewKeepsReferencedAnnotationSubschemas(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	for _, ref := range []string{"#/examples/0", "#/$defs/embedded/examples/0"} {
		t.Run(ref, func(t *testing.T) {
			payload := reviewToolPayload(249)
			example := []interface{}{map[string]interface{}{"type": "string", "enum": []string{"exact-value"}, "default": "exact-value"}}
			referencedSchema := map[string]interface{}{
				"$ref": ref, "description": "Follow the referenced schema, including its literal enum.",
				"examples": example,
				"$defs":    map[string]interface{}{"embedded": map[string]interface{}{"examples": example}},
			}
			payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0].ToolSpecification.InputSchema.JSON = referencedSchema
			if _, err := preflightKiroPayload(payload); err != nil {
				t.Fatalf("compaction should retain the referenced schema: %v", err)
			}
			after, ok := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
			if !ok || after["$ref"] != ref {
				t.Fatalf("reference changed: %#v", after)
			}
			target := after["examples"]
			if strings.Contains(ref, "$defs") {
				definitions, _ := after["$defs"].(map[string]interface{})
				embedded, _ := definitions["embedded"].(map[string]interface{})
				target = embedded["examples"]
			}
			want, _ := json.Marshal(example)
			got, _ := json.Marshal(target)
			if !bytes.Equal(want, got) {
				t.Fatalf("referenced annotation subtree was removed: ref=%s want=%s got=%s", ref, want, got)
			}
		})
	}
}

func TestRequestCompatReviewCompactedCandidateMayFitWithoutHistoryMarker(t *testing.T) {
	payload := reviewToolPayload(1)
	payload.ConversationState.History = []KiroHistoryMessage{{UserInputMessage: &KiroUserInputMessage{Content: "old", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR"}}}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	context.ToolResults = nil
	context.Tools[0].ToolSpecification.Description = strings.Repeat("x\r\n", 20)
	context.Tools[0].ToolSpecification.InputSchema.JSON = map[string]interface{}{"type": "object"}
	before, _ := marshalUpstreamJSON(payload)
	// CRLF normalization alone saves 40 encoded bytes. A truncation marker
	// plus acknowledgement is larger than the complete original tiny history.
	limit := len(before) - 10
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", fmt.Sprint(limit))
	result, err := preflightKiroPayload(payload)
	if err != nil {
		t.Fatalf("compacted candidate fits with its complete history: %v", err)
	}
	if result.DroppedHistoryEntries != 0 || len(payload.ConversationState.History) != 1 || payload.ConversationState.History[0].UserInputMessage.Content != "old" {
		t.Fatal("preflight replaced tiny fitting history with a larger truncation marker")
	}
	after, _ := marshalUpstreamJSON(payload)
	if len(after) > limit {
		t.Fatalf("body=%d limit=%d", len(after), limit)
	}
}

func reviewToolPayload(count int) *KiroPayload {
	payload := &KiroPayload{ProfileArn: "arn:review:request-compat"}
	payload.ConversationState.ConversationID = "independent-request-compat-review"
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "SYSTEM INSTRUCTIONS MUST STAY", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{ToolUses: []KiroToolUse{{ToolUseID: "review-active", Name: "fixture_000", Input: map[string]interface{}{"title": "read"}}}}},
	}
	context := &UserInputMessageContext{Tools: make([]KiroToolWrapper, count), ToolResults: []KiroToolResult{{ToolUseID: "review-active", Content: []KiroResultContent{{Text: "ACTIVE TOOL RESULT MUST STAY"}}, Status: "success"}}}
	for i := range context.Tools {
		spec := &context.Tools[i].ToolSpecification
		spec.Name = fmt.Sprintf("fixture_%03d", i)
		spec.Description = strings.Repeat("Read repository data and return precise evidence. 保留路径和范围。\n", 17)
		spec.InputSchema.JSON = map[string]interface{}{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"type":    "object", "description": strings.Repeat("These argument details explain how to select repository records while retaining user supplied defaults and validation rules.\n", 26),
			"title": "Detailed repository query", "$comment": "Presentation metadata may be omitted under pressure.",
			"properties": map[string]interface{}{
				"description": map[string]interface{}{"$ref": "#/$defs/literal", "description": "This is a real input property, not the parent schema annotation."},
				"title":       map[string]interface{}{"type": "string", "enum": []string{"read", "list"}, "default": "read"},
				"examples":    map[string]interface{}{"type": "integer", "minimum": json.Number("9007199254740993"), "maximum": json.Number("9007199254740995"), "default": json.Number("9007199254740993")},
				"default":     map[string]interface{}{"type": "boolean", "const": true, "default": true},
			},
			"required": []string{"description", "title", "examples", "default"}, "additionalProperties": false,
			"$defs": map[string]interface{}{"literal": map[string]interface{}{"type": "object", "const": map[string]interface{}{"title": "KEEP TITLE", "description": "KEEP DESCRIPTION", "examples": []string{"KEEP EXAMPLE"}, "$comment": "KEEP COMMENT"}, "default": map[string]interface{}{"title": "KEEP DEFAULT TITLE", "description": "KEEP DEFAULT DESCRIPTION"}}},
		}
	}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{Content: "CURRENT REQUEST MUST STAY", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR", UserInputMessageContext: context}
	return payload
}

// Explicit fixture paths avoid reimplementing the production schema walker.
// Only descriptions at the two known schema-annotation locations and the
// known presentation keywords can differ; literal instance data stays exact.
func reviewValidationFields(t *testing.T, value interface{}) []byte {
	t.Helper()
	wire, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.UseNumber()
	var object map[string]interface{}
	if err := decoder.Decode(&object); err != nil {
		t.Fatal(err)
	}
	delete(object, "description")
	delete(object, "title")
	delete(object, "$comment")
	if properties, ok := object["properties"].(map[string]interface{}); ok {
		if descriptionProperty, ok := properties["description"].(map[string]interface{}); ok {
			delete(descriptionProperty, "description")
		}
	}
	wire, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}
