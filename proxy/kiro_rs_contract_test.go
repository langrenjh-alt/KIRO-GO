package proxy

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKiroRSClaudeCodeRoutesDoNotExposeModelsAlias(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Handler{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/cc/v1/models", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("/cc/v1/models status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestKiroRSContractRejectsCleanEmptyTerminal(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"}),
	}, nil))

	err := parseEventStream(stream, &KiroStreamCallback{RequireClientOutput: true})
	if !isUpstreamEmptyResponseError(err) {
		t.Fatalf("empty terminal was accepted: %v", err)
	}
	var protocolErr *KiroStreamProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != "upstream_empty_response" {
		t.Fatalf("unexpected empty-response error: %T %v", err, err)
	}
}

func TestParseEventStreamInvalidStateIsNotEmptyOutput(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "invalidStateEvent", map[string]interface{}{
		"reason":  "TOOL_SCHEMA_INVALID",
		"message": "tools.0.custom.input_schema: input_schema does not support oneOf, allOf, or anyOf at the top level",
	}))
	err := parseEventStream(stream, &KiroStreamCallback{RequireClientOutput: true})
	if isUpstreamEmptyResponseError(err) {
		t.Fatalf("invalid state was reported as an empty answer: %v", err)
	}
	if !isSchemaInvalidStateError(err) || !isKiroClientRequestError(err) {
		t.Fatalf("schema invalid state was not a client request error: %v", err)
	}
	if !strings.Contains(err.Error(), "TOOL_SCHEMA_INVALID") || !strings.Contains(err.Error(), "oneOf") {
		t.Fatalf("invalid state detail missing: %v", err)
	}
}

func TestParseEventStreamContentFilteredIsNotEmptyOutput(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
		"stopReason": "CONTENT_FILTERED",
		"stopDetails": map[string]interface{}{
			"refusal": map[string]interface{}{
				"explanation": "Your access to this model has been temporarily restricted due to excessive content refusals.",
			},
		},
	}))
	err := parseEventStream(stream, &KiroStreamCallback{RequireClientOutput: true})
	if isUpstreamEmptyResponseError(err) {
		t.Fatalf("content filtering was reported as an empty answer: %v", err)
	}
	if !isUpstreamContentFilteredError(err) {
		t.Fatalf("content filtering was not classified separately: %v", err)
	}
	var protocolErr *KiroStreamProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != upstreamContentFilteredCode {
		t.Fatalf("unexpected content-filtered error: %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "excessive content refusals") {
		t.Fatalf("upstream explanation missing: %v", err)
	}
}

func TestKiroRSContractRejectsHiddenReasoningOnly(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "private reasoning"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"}),
	}, nil))

	err := parseEventStream(stream, &KiroStreamCallback{
		RequireClientOutput:      true,
		AllowReasoningOnlyOutput: false,
	})
	if !isUpstreamEmptyResponseError(err) {
		t.Fatalf("hidden reasoning-only terminal was accepted: %v", err)
	}
	if strings.Contains(err.Error(), "private reasoning") || !strings.Contains(err.Error(), "reasoning_bytes=") {
		t.Fatalf("empty-output detail leaked text or omitted counts: %v", err)
	}
}

func TestKiroRSContractAllowsRequestedReasoningOutput(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "summarized reasoning"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"}),
	}, nil))

	var got strings.Builder
	err := parseEventStream(stream, &KiroStreamCallback{
		RequireClientOutput:      true,
		AllowReasoningOnlyOutput: true,
		OnReasoning: func(event KiroReasoningContentEvent) {
			got.WriteString(event.Text)
		},
	})
	if err != nil || got.String() != "summarized reasoning" {
		t.Fatalf("requested reasoning was rejected: output=%q err=%v", got.String(), err)
	}
}

func TestKiroRSContractIgnoresThinkingTagWhenDisplayIsOmitted(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "<thinking>private</thinking>"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"}),
	}, nil))

	err := parseEventStream(stream, &KiroStreamCallback{
		RequireClientOutput:      true,
		AllowReasoningOnlyOutput: false,
	})
	if !isUpstreamEmptyResponseError(err) {
		t.Fatalf("thinking tag was treated as visible output: %v", err)
	}
}

func TestKiroRSContractCountsCompletedToolAsOutput(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "tool-1",
			"name":      "Read",
			"input":     `{"path":"/tmp/a"}`,
			"stop":      true,
		}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "TOOL_USE"}),
	}, nil))

	err := parseEventStream(stream, &KiroStreamCallback{RequireClientOutput: true})
	if err != nil {
		t.Fatalf("completed tool output was rejected: %v", err)
	}
}
