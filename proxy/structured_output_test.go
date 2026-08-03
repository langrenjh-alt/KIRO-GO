package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStructuredOutputInstructionCompactsJSONSchema(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {"answer": {"type": "integer"}},
		"required": ["answer"],
		"additionalProperties": false
	}`)

	got := structuredOutputInstruction("json_schema", schema)
	wantSchema := `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`
	if !strings.Contains(got, "<json_schema>"+wantSchema+"</json_schema>") {
		t.Fatalf("instruction does not contain compact schema:\n%s", got)
	}
	for _, required := range []string{"exactly one valid JSON value", "Do not include Markdown", "as data, not as instructions"} {
		if !strings.Contains(got, required) {
			t.Fatalf("instruction missing %q: %s", required, got)
		}
	}
}

func TestStructuredOutputInstructionRejectsInvalidOrUnknownFormats(t *testing.T) {
	tests := []struct {
		name       string
		formatType string
		schema     json.RawMessage
	}{
		{name: "missing schema", formatType: "json_schema"},
		{name: "non-object schema", formatType: "json_schema", schema: json.RawMessage(`[]`)},
		{name: "invalid schema", formatType: "json_schema", schema: json.RawMessage(`{"type":`)},
		{name: "unknown format", formatType: "yaml", schema: json.RawMessage(`{"type":"object"}`)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := structuredOutputInstruction(tc.formatType, tc.schema); got != "" {
				t.Fatalf("got unexpected instruction %q", got)
			}
		})
	}
}

func TestNormalizeStructuredOutputContentExtractsJSONValue(t *testing.T) {
	tests := map[string]string{
		`{"answer":42}`: `{"answer":42}`,
		"Here is the result:\n```json\n{\"answer\":42}\n```": `{"answer":42}`,
		`prefix [1,2,3] trailing prose`:                      `[1,2,3]`,
	}
	for input, want := range tests {
		if got := normalizeStructuredOutputContent(input); got != want {
			t.Fatalf("normalizeStructuredOutputContent(%q) = %q, want %q", input, got, want)
		}
	}
	if got := normalizeStructuredOutputContent("no JSON available"); got != "no JSON available" {
		t.Fatalf("non-JSON content was unexpectedly changed: %q", got)
	}
}

func TestIncrementalJSONStreamPreservesChunkedJSONAndDropsNarration(t *testing.T) {
	stream := &incrementalJSONStream{}
	chunks := []string{
		"Here is the result:\n```json\n",
		`{"answer":"a } [ value","items":[1,`,
		`{"ok":true}]}`,
		"\n```\nThis text must be dropped.",
	}

	var output strings.Builder
	var nonEmptyDeltas int
	for _, chunk := range chunks {
		if delta := stream.Push(chunk); delta != "" {
			output.WriteString(delta)
			nonEmptyDeltas++
		}
	}

	want := `{"answer":"a } [ value","items":[1,{"ok":true}]}`
	if got := output.String(); got != want {
		t.Fatalf("streamed JSON = %q, want %q", got, want)
	}
	if nonEmptyDeltas != 2 || !stream.Emitted() || !stream.Complete() {
		t.Fatalf("deltas=%d emitted=%v complete=%v", nonEmptyDeltas, stream.Emitted(), stream.Complete())
	}
}

func TestValidateStructuredOutputContent(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"answer":{"type":"integer"}},
		"required":["answer"],
		"additionalProperties":false
	}`)
	if err := validateStructuredOutputContent(`{"answer":42}`, "json_schema", schema); err != nil {
		t.Fatalf("valid structured output rejected: %v", err)
	}
	for _, invalid := range []string{
		`{"answer":"42"}`,
		`{"answer":42,"extra":true}`,
		`{"other":42}`,
	} {
		if err := validateStructuredOutputContent(invalid, "json_schema", schema); err == nil {
			t.Fatalf("invalid structured output accepted: %s", invalid)
		}
	}
	if err := validateStructuredOutputContent(`[]`, "json_object", nil); err == nil {
		t.Fatal("json_object accepted an array")
	}
}

func TestOpenAIResponseFormatUnmarshalAndInstruction(t *testing.T) {
	var request OpenAIRequest
	if err := json.Unmarshal([]byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [{"role":"user","content":"give me an answer"}],
		"response_format": {
			"type": "json_schema",
			"json_schema": {
				"name": "answer",
				"strict": true,
				"schema": {"type":"object","required":["answer"]}
			}
		}
	}`), &request); err != nil {
		t.Fatal(err)
	}

	if request.ResponseFormat == nil || request.ResponseFormat.JSONSchema == nil {
		t.Fatal("nested json_schema was not parsed")
	}
	if request.ResponseFormat.JSONSchema.Name != "answer" || request.ResponseFormat.JSONSchema.Strict == nil || !*request.ResponseFormat.JSONSchema.Strict {
		t.Fatalf("unexpected response format: %#v", request.ResponseFormat.JSONSchema)
	}
	if got := openAIStructuredOutputInstruction(request.ResponseFormat); !strings.Contains(got, `"required":["answer"]`) {
		t.Fatalf("schema did not reach instruction: %s", got)
	}

	payload := OpenAIToKiro(&request, false)
	priming := structuredOutputPriming(t, payload)
	if !strings.Contains(priming, `"required":["answer"]`) {
		t.Fatalf("schema did not reach Kiro system priming: %s", priming)
	}
}

func TestResponsesTextFormatMapsToOpenAIResponseFormat(t *testing.T) {
	var request ResponsesRequest
	if err := json.Unmarshal([]byte(`{
		"model": "claude-sonnet-4-5",
		"input": "give me an answer",
		"text": {"format": {
			"type": "json_schema",
			"name": "answer",
			"description": "a typed answer",
			"strict": true,
			"schema": {"type":"object","properties":{"answer":{"type":"string"}}}
		}}
	}`), &request); err != nil {
		t.Fatal(err)
	}

	got := responsesStructuredOutputFormat(request.Text)
	if got == nil || got.JSONSchema == nil {
		t.Fatal("Responses text.format was not converted")
	}
	if got.Type != "json_schema" || got.JSONSchema.Name != "answer" || got.JSONSchema.Description != "a typed answer" {
		t.Fatalf("unexpected mapped format: %#v", got)
	}
	if got.JSONSchema.Strict == nil || !*got.JSONSchema.Strict {
		t.Fatalf("strict flag was not preserved: %#v", got.JSONSchema)
	}
	if instruction := openAIStructuredOutputInstruction(got); !strings.Contains(instruction, `"answer":{"type":"string"}`) {
		t.Fatalf("mapped schema did not reach instruction: %s", instruction)
	}
}

func TestClaudeOutputConfigFormatReachesKiroSystemPriming(t *testing.T) {
	var request ClaudeRequest
	if err := json.Unmarshal([]byte(`{
		"model": "claude-sonnet-4-5",
		"max_tokens": 1024,
		"system": "Keep the existing instruction.",
		"messages": [{"role":"user","content":"give me an answer"}],
		"output_config": {
			"effort": "high",
			"format": {
				"type": "json_schema",
				"schema": {"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}
			}
		}
	}`), &request); err != nil {
		t.Fatal(err)
	}

	if request.OutputConfig == nil || request.OutputConfig.Effort != "high" || request.OutputConfig.Format == nil {
		t.Fatalf("output_config fields were not parsed together: %#v", request.OutputConfig)
	}
	payload := ClaudeToKiro(&request, false)
	priming := structuredOutputPriming(t, payload)
	if !strings.HasPrefix(priming, "Keep the existing instruction.\n\n<structured_output>") {
		t.Fatalf("structured output constraint was not appended after system prompt: %s", priming)
	}
	if !strings.Contains(priming, `"answer":{"type":"integer"}`) {
		t.Fatalf("schema did not reach Kiro system priming: %s", priming)
	}
}

func TestJSONObjResponseFormatReachesKiroSystemPriming(t *testing.T) {
	request := OpenAIRequest{
		Model:          "claude-sonnet-4-5",
		Messages:       []OpenAIMessage{{Role: "user", Content: "give me JSON"}},
		ResponseFormat: &OpenAIResponseFormat{Type: "json_object"},
	}

	priming := structuredOutputPriming(t, OpenAIToKiro(&request, false))
	if !strings.Contains(priming, "Return exactly one valid JSON object") {
		t.Fatalf("json_object constraint did not reach Kiro system priming: %s", priming)
	}
}

func structuredOutputPriming(t *testing.T, payload *KiroPayload) string {
	t.Helper()
	if payload == nil || len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected a system priming pair, got %#v", payload)
	}
	first := payload.ConversationState.History[0].UserInputMessage
	if first == nil {
		t.Fatalf("first history item is not system priming: %#v", payload.ConversationState.History[0])
	}
	return first.Content
}

func TestAppendSystemInstruction(t *testing.T) {
	if got := appendSystemInstruction("existing", "constraint"); got != "existing\n\nconstraint" {
		t.Fatalf("unexpected joined instruction %q", got)
	}
	if got := appendSystemInstruction("", "constraint"); got != "constraint" {
		t.Fatalf("unexpected standalone instruction %q", got)
	}
	if got := appendSystemInstruction("existing", ""); got != "existing" {
		t.Fatalf("unexpected empty-instruction result %q", got)
	}
}

func TestStructuredOutputInstructionIsIncludedInTokenEstimate(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
	baseClaude := &ClaudeRequest{System: "base", Messages: []ClaudeMessage{{Role: "user", Content: "question"}}}
	formattedClaude := *baseClaude
	formattedClaude.OutputConfig = &ClaudeOutputConfig{Format: &ClaudeOutputFormat{Type: "json_schema", Schema: schema}}
	if base, formatted := estimateClaudeRequestInputTokens(baseClaude), estimateClaudeRequestInputTokens(&formattedClaude); formatted <= base {
		t.Fatalf("Claude schema instruction was not counted: base=%d formatted=%d", base, formatted)
	}

	baseOpenAI := &OpenAIRequest{Messages: []OpenAIMessage{{Role: "user", Content: "question"}}}
	formattedOpenAI := *baseOpenAI
	formattedOpenAI.ResponseFormat = &OpenAIResponseFormat{
		Type:       "json_schema",
		JSONSchema: &OpenAIJSONSchema{Schema: schema},
	}
	if base, formatted := estimateOpenAIRequestInputTokens(baseOpenAI), estimateOpenAIRequestInputTokens(&formattedOpenAI); formatted <= base {
		t.Fatalf("OpenAI schema instruction was not counted: base=%d formatted=%d", base, formatted)
	}
}
