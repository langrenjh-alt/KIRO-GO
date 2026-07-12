package proxy

import (
	"encoding/json"
	"strings"
)

type ResponsesRequest struct {
	Model              string                    `json:"model"`
	Input              json.RawMessage           `json:"input"`
	Instructions       string                    `json:"instructions,omitempty"`
	Stream             bool                      `json:"stream,omitempty"`
	Tools              []OpenAITool              `json:"tools,omitempty"`
	ToolChoice         json.RawMessage           `json:"tool_choice,omitempty"`
	PreviousResponseID string                    `json:"previous_response_id,omitempty"`
	Store              *bool                     `json:"store,omitempty"`
	Temperature        *float64                  `json:"temperature,omitempty"`
	MaxOutputTokens    *int                      `json:"max_output_tokens,omitempty"`
	Metadata           map[string]string         `json:"metadata,omitempty"`
	Reasoning          *ResponsesReasoningConfig `json:"reasoning,omitempty"`
	ReasoningEffort    string                    `json:"reasoning_effort,omitempty"`
	Text               *ResponsesTextConfig      `json:"text,omitempty"`
}

type ResponsesReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

func (r *ResponsesRequest) effectiveReasoningEffort() string {
	if r == nil {
		return ""
	}
	if r.Reasoning != nil && strings.TrimSpace(r.Reasoning.Effort) != "" {
		return r.Reasoning.Effort
	}
	return r.ReasoningEffort
}

func (r *ResponsesRequest) responseReasoningConfig() *ResponsesReasoningConfig {
	if r == nil {
		return nil
	}
	if r.Reasoning != nil {
		result := *r.Reasoning
		if strings.TrimSpace(result.Effort) == "" {
			result.Effort = r.ReasoningEffort
		}
		return &result
	}
	if strings.TrimSpace(r.ReasoningEffort) == "" {
		return nil
	}
	return &ResponsesReasoningConfig{Effort: r.ReasoningEffort}
}

type ResponsesObject struct {
	ID                 string                      `json:"id"`
	Object             string                      `json:"object"`
	CreatedAt          int64                       `json:"created_at"`
	Status             string                      `json:"status"`
	Model              string                      `json:"model"`
	Output             []ResponseOutputItem        `json:"output"`
	Usage              ResponsesUsage              `json:"usage"`
	Reasoning          *ResponsesReasoningConfig   `json:"reasoning,omitempty"`
	Text               *ResponsesTextConfig        `json:"text,omitempty"`
	IncompleteDetails  *ResponsesIncompleteDetails `json:"incomplete_details,omitempty"`
	PreviousResponseID string                      `json:"previous_response_id,omitempty"`
	Metadata           map[string]string           `json:"metadata,omitempty"`
	Error              *ResponsesError             `json:"error,omitempty"`
	Instructions       string                      `json:"instructions,omitempty"`
	StoredInput        json.RawMessage             `json:"-"`
	StoredInstr        string                      `json:"-"`
	OwnerKeyID         string                      `json:"-"` // API key that created this response (not exposed in API)
	StoredAt           int64                       `json:"stored_at,omitempty"`
}

type ResponseOutputItem struct {
	ID        string                `json:"id"`
	Type      string                `json:"type"`
	Role      string                `json:"role,omitempty"`
	Status    string                `json:"status,omitempty"`
	Content   []ResponseContentPart `json:"content,omitempty"`
	CallID    string                `json:"call_id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Arguments string                `json:"arguments,omitempty"`
	Summary   []ResponseSummaryPart `json:"summary,omitempty"`
}

type ResponseSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponseContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ResponsesUsage struct {
	InputTokens         int                          `json:"input_tokens"`
	InputTokensDetails  ResponsesInputTokensDetails  `json:"input_tokens_details"`
	OutputTokens        int                          `json:"output_tokens"`
	OutputTokensDetails ResponsesOutputTokensDetails `json:"output_tokens_details"`
	TotalTokens         int                          `json:"total_tokens"`
}

type ResponsesInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type ResponsesOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type ResponsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

type ResponsesError struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}
