package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestResponsesWebSearchExecutesMCPWithMixedTools(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var calls int
	h.webSearchMCPCaller = func(_ context.Context, _ *config.Account, query string) (*webSearchResultSet, error) {
		calls++
		if query != "(site:example.com) current Go release" {
			t.Fatalf("query = %q", query)
		}
		return testWebSearchResultSet(), nil
	}

	body := `{
		"model":"claude-sonnet-4.6",
		"input":"current Go release",
		"tools":[
			{"type":"function","name":"read_file","parameters":{"type":"object"}},
			{"type":"web_search_preview","search_context_size":"medium","filters":{"allowed_domains":["example.com"]}}
		],
		"store":false
	}`
	recorder := httptest.NewRecorder()
	h.handleOpenAIResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls != 1 {
		t.Fatalf("MCP calls = %d, want 1", calls)
	}

	var response ResponsesObject
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Output) != 2 || response.Output[0].Type != "web_search_call" {
		t.Fatalf("output = %#v", response.Output)
	}
	call := response.Output[0]
	if call.Status != "completed" || call.Action == nil || call.Action.Type != "search" || call.Action.Query != "current Go release" {
		t.Fatalf("web search call = %#v", call)
	}
	message := response.Output[1]
	if message.Type != "message" || len(message.Content) != 1 || len(message.Content[0].Annotations) == 0 {
		t.Fatalf("search message = %#v", message)
	}
	annotation := message.Content[0].Annotations[0]
	if annotation.Type != "url_citation" || annotation.URL == "" || annotation.EndIndex <= annotation.StartIndex {
		t.Fatalf("annotation = %#v", annotation)
	}
}

func TestResponsesWebSearchToolChoiceSelectsBuiltInTool(t *testing.T) {
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(`{
		"input":"latest release",
		"tools":[{"type":"web_search_preview"}],
		"tool_choice":{"type":"web_search_preview"}
	}`), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	choice, err := resolveToolChoice(req.ToolChoice)
	if err != nil {
		t.Fatalf("resolve tool choice: %v", err)
	}
	selected := filterOpenAIToolsForChoice(req.Tools, choice)
	if choice.Mode != "specific" || choice.Name != webSearchToolName || len(selected) != 1 || !isResponsesWebSearchTool(selected[0]) {
		t.Fatalf("choice=%#v selected=%#v", choice, selected)
	}
}

func TestResponsesWebSearchStreamEmitsExecutionEvents(t *testing.T) {
	result := testWebSearchResultSet()
	response, _ := buildResponsesWebSearchObject(
		"resp_search", "claude-sonnet-4.6", &ResponsesRequest{}, result, 12,
	)
	recorder := httptest.NewRecorder()
	if err := writeResponsesWebSearchSSE(recorder, response); err != nil {
		t.Fatalf("writeResponsesWebSearchSSE: %v", err)
	}

	body := recorder.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.web_search_call.searching",
		"event: response.web_search_call.completed",
		"event: response.output_text.annotation.added",
		"event: response.output_text.delta",
		"event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream is missing %q:\n%s", want, body)
		}
	}
}
