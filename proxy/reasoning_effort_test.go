package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEffortFromBudgetTokensBoundaries(t *testing.T) {
	tests := []struct {
		tokens int
		want   string
	}{
		{tokens: 0, want: "low"},
		{tokens: 4_000, want: "low"},
		{tokens: 4_001, want: "medium"},
		{tokens: 16_000, want: "medium"},
		{tokens: 16_001, want: "high"},
		{tokens: 64_000, want: "high"},
		{tokens: 64_001, want: "xhigh"},
	}

	for _, tc := range tests {
		if got := effortFromBudgetTokens(tc.tokens); got != tc.want {
			t.Fatalf("effortFromBudgetTokens(%d) = %q, want %q", tc.tokens, got, tc.want)
		}
	}
}

func TestNormalizeEffortForModel(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		effort string
		want   string
	}{
		{name: "case and whitespace", model: "claude-opus-4.8", effort: "  MEDIUM  ", want: "medium"},
		{name: "x dash alias", model: "claude-opus-4.8", effort: "x-high", want: "xhigh"},
		{name: "x underscore alias", model: "claude-opus-4.7", effort: "x_high", want: "xhigh"},
		{name: "max remains max", model: "claude-sonnet-4.6", effort: "MAX", want: "max"},
		{name: "unknown falls back", model: "claude-opus-4.8", effort: "extreme", want: "high"},
		{name: "opus 4.6 xhigh downgrade", model: "claude-opus-4.6", effort: "xhigh", want: "high"},
		{name: "sonnet 4.6 xhigh downgrade", model: "claude-sonnet-4.6", effort: "xhigh", want: "high"},
		{name: "known 4.5 xhigh downgrade", model: "claude-haiku-4.5", effort: "xhigh", want: "high"},
		{name: "opus 4.7 preserves xhigh", model: "claude-opus-4.7", effort: "xhigh", want: "xhigh"},
		{name: "claude 5 preserves xhigh", model: "claude-fable-5", effort: "xhigh", want: "xhigh"},
		{name: "unknown future model preserves xhigh", model: "claude-unknown-9", effort: "xhigh", want: "xhigh"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeEffortForModel(tc.model, tc.effort); got != tc.want {
				t.Fatalf("normalizeEffortForModel(%q, %q) = %q, want %q", tc.model, tc.effort, got, tc.want)
			}
		})
	}
}

func TestModelSupportsNativeReasoningAllowlist(t *testing.T) {
	for _, model := range []string{
		"claude-opus-4.6",
		"claude-opus-4-7",
		"claude-opus-4.8",
		"claude-sonnet-4-6",
		"claude-fable-5",
		"claude-mythos-5.1",
		"claude-sonnet-5",
		"claude-opus-5",
		"claude-5",
	} {
		if !modelSupportsNativeReasoning(model) {
			t.Errorf("modelSupportsNativeReasoning(%q) = false, want true", model)
		}
	}

	for _, model := range []string{
		"claude-sonnet-4.8",
		"claude-sonnet-4.5",
		"claude-opus-4.5",
		"claude-haiku-4.5",
		"claude-sonnet-50",
		"not-claude-opus-5",
		"gpt-5",
	} {
		if modelSupportsNativeReasoning(model) {
			t.Errorf("modelSupportsNativeReasoning(%q) = true, want false", model)
		}
	}
}

func TestClaudeToKiroReasoningEffort(t *testing.T) {
	base := func(model string) *ClaudeRequest {
		return &ClaudeRequest{
			Model:    model,
			Messages: []ClaudeMessage{{Role: "user", Content: "test"}},
		}
	}

	t.Run("explicit effort takes priority over budget", func(t *testing.T) {
		req := base("claude-opus-4.8")
		req.Thinking = &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 64_001}
		req.OutputConfig = &ClaudeOutputConfig{Effort: "low"}

		payload := ClaudeToKiro(req, true)
		assertPayloadEffort(t, payload, "low")
	})

	t.Run("budget derives effort", func(t *testing.T) {
		req := base("claude-opus-4.8")
		req.Thinking = &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 16_001}

		payload := ClaudeToKiro(req, true)
		assertPayloadEffort(t, payload, "high")
	})

	t.Run("adaptive opus 4.6 permits normalized effort", func(t *testing.T) {
		req := base("claude-opus-4-6")
		req.Thinking = &ClaudeThinkingConfig{Type: "adaptive"}
		req.OutputConfig = &ClaudeOutputConfig{Effort: "x_high"}

		payload := ClaudeToKiro(req, true)
		assertPayloadEffort(t, payload, "high")
	})

	t.Run("non adaptive opus 4.6 is suppressed", func(t *testing.T) {
		req := base("claude-opus-4.6")
		req.Thinking = &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 8_000}
		req.OutputConfig = &ClaudeOutputConfig{Effort: "medium"}

		payload := ClaudeToKiro(req, true)
		if payload.AdditionalModelRequestFields != nil {
			t.Fatalf("unexpected fields: %#v", payload.AdditionalModelRequestFields)
		}
	})

	t.Run("disabled thinking suppresses explicit effort", func(t *testing.T) {
		req := base("claude-opus-4.8")
		req.Thinking = &ClaudeThinkingConfig{Type: "disabled"}
		req.OutputConfig = &ClaudeOutputConfig{Effort: "max"}

		_, thinking := resolveClaudeRequestThinkingMode(req, "-thinking")
		if thinking {
			t.Fatal("explicit effort must not override thinking.type=disabled")
		}
		payload := ClaudeToKiro(req, thinking)
		if payload.AdditionalModelRequestFields != nil {
			t.Fatalf("unexpected fields: %#v", payload.AdditionalModelRequestFields)
		}
	})

	t.Run("unsupported model is suppressed", func(t *testing.T) {
		req := base("claude-sonnet-4.8")
		req.Thinking = &ClaudeThinkingConfig{Type: "adaptive"}
		req.OutputConfig = &ClaudeOutputConfig{Effort: "high"}

		payload := ClaudeToKiro(req, true)
		if payload.AdditionalModelRequestFields != nil {
			t.Fatalf("unexpected fields: %#v", payload.AdditionalModelRequestFields)
		}
	})

	t.Run("explicit effort requests reasoning without thinking object", func(t *testing.T) {
		req := base("claude-fable-5")
		req.OutputConfig = &ClaudeOutputConfig{Effort: "x-high"}

		model, thinking := resolveClaudeRequestThinkingMode(req, "-thinking")
		if model != "claude-fable-5" || !thinking {
			t.Fatalf("got model=%q thinking=%v", model, thinking)
		}
		payload := ClaudeToKiro(req, thinking)
		assertPayloadEffort(t, payload, "xhigh")
	})
}

func TestClaudeThinkingPromptMatchesRequestedMode(t *testing.T) {
	t.Run("adaptive uses normalized effort", func(t *testing.T) {
		req := &ClaudeRequest{
			Model:        "claude-opus-4.6",
			Thinking:     &ClaudeThinkingConfig{Type: "adaptive"},
			OutputConfig: &ClaudeOutputConfig{Effort: "x_high"},
		}
		got := claudeThinkingPrompt(req, true)
		want := "<thinking_mode>adaptive</thinking_mode>\n<thinking_effort>high</thinking_effort>"
		if got != want {
			t.Fatalf("adaptive prompt = %q, want %q", got, want)
		}
	})

	t.Run("enabled uses client budget", func(t *testing.T) {
		req := &ClaudeRequest{
			Model:    "claude-opus-4.8",
			Thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 16_001},
		}
		got := claudeThinkingPrompt(req, true)
		want := "<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>16001</max_thinking_length>"
		if got != want {
			t.Fatalf("enabled prompt = %q, want %q", got, want)
		}
	})

	t.Run("effort-only request uses adaptive prompt", func(t *testing.T) {
		req := &ClaudeRequest{
			Model:        "claude-opus-4.8",
			OutputConfig: &ClaudeOutputConfig{Effort: "medium"},
		}
		got := claudeThinkingPrompt(req, true)
		if !strings.Contains(got, "<thinking_mode>adaptive</thinking_mode>") || !strings.Contains(got, "<thinking_effort>medium</thinking_effort>") {
			t.Fatalf("effort-only prompt = %q", got)
		}
	})
}

func TestOpus46ThinkingSuffixSynthesizesAdaptiveMode(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-opus-4-6-thinking",
		Messages: []ClaudeMessage{{Role: "user", Content: "test"}},
	}
	model, thinking := resolveClaudeRequestThinkingMode(req, "-thinking")
	if model != "claude-opus-4.6" || !thinking {
		t.Fatalf("got model=%q thinking=%v", model, thinking)
	}
	if req.Thinking == nil || req.Thinking.Type != "adaptive" {
		t.Fatalf("effective thinking config = %#v", req.Thinking)
	}
	req.Model = model
	if prompt := claudeThinkingPrompt(req, thinking); !strings.Contains(prompt, "<thinking_mode>adaptive</thinking_mode>") {
		t.Fatalf("suffix prompt = %q", prompt)
	}
	assertPayloadEffort(t, ClaudeToKiro(req, thinking), "high")
}

func TestOpenAIToKiroReasoningEffort(t *testing.T) {
	req := &OpenAIRequest{
		Model:           "claude-opus-4-7",
		Messages:        []OpenAIMessage{{Role: "user", Content: "test"}},
		ReasoningEffort: "x-high",
	}
	model, thinking := resolveOpenAIThinkingMode(req.Model, req.ReasoningEffort, "-thinking")
	if model != "claude-opus-4.7" || !thinking {
		t.Fatalf("got model=%q thinking=%v", model, thinking)
	}
	req.Model = model
	assertPayloadEffort(t, OpenAIToKiro(req, thinking), "xhigh")

	req.Model = "claude-opus-4.6"
	if fields := OpenAIToKiro(req, true).AdditionalModelRequestFields; fields != nil {
		t.Fatalf("OpenAI cannot safely select Opus 4.6 adaptive mode, got %#v", fields)
	}

	req.Model = "claude-sonnet-4.8"
	if fields := OpenAIToKiro(req, true).AdditionalModelRequestFields; fields != nil {
		t.Fatalf("unsupported model received fields: %#v", fields)
	}
}

func TestOpenAIReasoningEffortNoneAndMinimal(t *testing.T) {
	model, thinking := resolveOpenAIThinkingMode("claude-opus-4.8-thinking", "none", "-thinking")
	if model != "claude-opus-4.8" || thinking {
		t.Fatalf("none must disable thinking, got model=%q thinking=%v", model, thinking)
	}
	noneReq := &OpenAIRequest{
		Model:           model,
		Messages:        []OpenAIMessage{{Role: "user", Content: "test"}},
		ReasoningEffort: "none",
	}
	if fields := OpenAIToKiro(noneReq, thinking).AdditionalModelRequestFields; fields != nil {
		t.Fatalf("none must not reach Kiro reasoning fields: %#v", fields)
	}

	minimalReq := &OpenAIRequest{
		Model:           "claude-opus-4.8",
		Messages:        []OpenAIMessage{{Role: "user", Content: "test"}},
		ReasoningEffort: "minimal",
	}
	_, thinking = resolveOpenAIThinkingMode(minimalReq.Model, minimalReq.ReasoningEffort, "-thinking")
	if !thinking {
		t.Fatal("minimal must enable the nearest supported reasoning tier")
	}
	assertPayloadEffort(t, OpenAIToKiro(minimalReq, thinking), "low")
}

func TestResponsesReasoningEffortNestedPrecedesTopLevelAlias(t *testing.T) {
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(`{
		"model":"claude-opus-4.8",
		"input":"test",
		"reasoning":{"effort":"medium"},
		"reasoning_effort":"low"
	}`), &req); err != nil {
		t.Fatal(err)
	}

	if got := req.effectiveReasoningEffort(); got != "medium" {
		t.Fatalf("effectiveReasoningEffort() = %q, want medium", got)
	}
	req.Reasoning.Effort = "   "
	if got := req.effectiveReasoningEffort(); got != "low" {
		t.Fatalf("blank nested effort should fall back to alias, got %q", got)
	}
}

func TestKiroReasoningEffortWireFormat(t *testing.T) {
	payload := &KiroPayload{
		AdditionalModelRequestFields: newReasoningFields("claude-opus-4.8", "high"),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	var wire map[string]interface{}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	outer, ok := wire["additionalModelRequestFields"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing camelCase outer field in %s", raw)
	}
	if _, exists := outer["outputConfig"]; exists {
		t.Fatalf("unexpected camelCase inner field in %s", raw)
	}
	output, ok := outer["output_config"].(map[string]interface{})
	if !ok || output["effort"] != "high" {
		t.Fatalf("unexpected output_config in %s", raw)
	}
}

func TestReasoningEffortRequestFieldsUnmarshal(t *testing.T) {
	var claudeReq ClaudeRequest
	if err := json.Unmarshal([]byte(`{
		"model":"claude-opus-4.8",
		"messages":[],
		"max_tokens":1024,
		"output_config":{"effort":"max"}
	}`), &claudeReq); err != nil {
		t.Fatal(err)
	}
	if claudeReq.OutputConfig == nil || claudeReq.OutputConfig.Effort != "max" {
		t.Fatalf("Claude output_config.effort was not parsed: %#v", claudeReq.OutputConfig)
	}

	var openAIReq OpenAIRequest
	if err := json.Unmarshal([]byte(`{
		"model":"claude-opus-4.8",
		"messages":[],
		"reasoning_effort":"high"
	}`), &openAIReq); err != nil {
		t.Fatal(err)
	}
	if openAIReq.ReasoningEffort != "high" {
		t.Fatalf("OpenAI reasoning_effort was not parsed: %q", openAIReq.ReasoningEffort)
	}
}

func assertPayloadEffort(t *testing.T, payload *KiroPayload, want string) {
	t.Helper()
	if payload == nil || payload.AdditionalModelRequestFields == nil || payload.AdditionalModelRequestFields.OutputConfig == nil {
		t.Fatalf("missing additionalModelRequestFields.output_config: %#v", payload)
	}
	if got := payload.AdditionalModelRequestFields.OutputConfig.Effort; got != want {
		t.Fatalf("effort = %q, want %q", got, want)
	}
}
