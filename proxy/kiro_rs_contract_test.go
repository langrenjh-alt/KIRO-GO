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
