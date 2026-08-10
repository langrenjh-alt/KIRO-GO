package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHasWebSearchToolFindsSupportedNativeToolInMixedList(t *testing.T) {
	for _, toolType := range []string{"web_search_20250305", "web_search_20260209", "web_search_20260318"} {
		req := &ClaudeRequest{Tools: []ClaudeTool{{Name: "web_search", Type: toolType}}}
		if !hasWebSearchTool(req) {
			t.Fatalf("expected %s to use the web search fast path", toolType)
		}
	}

	invalid := []*ClaudeRequest{
		nil,
		{},
		{Tools: []ClaudeTool{{Name: "web_search"}}},
		{Tools: []ClaudeTool{{Name: "web_search", Type: "web_search_20990101"}}},
		{Tools: []ClaudeTool{{Name: "other", Type: "web_search_20250305"}}},
	}
	for i, req := range invalid {
		if hasWebSearchTool(req) {
			t.Fatalf("case %d unexpectedly used the web search fast path", i)
		}
	}

	mixed := &ClaudeRequest{Tools: []ClaudeTool{
		{Name: "Read"},
		{Name: "web_search", Type: "web_search_20250305"},
		{Name: "Write"},
	}}
	if !hasWebSearchTool(mixed) {
		t.Fatal("supported web_search in a mixed tool list must use the fast path")
	}
}

func TestNativeWebSearchDeclarationDetectsMixedAndUnknownVersions(t *testing.T) {
	for _, req := range []*ClaudeRequest{
		{Tools: []ClaudeTool{{Name: "web_search", Type: "web_search_20990101"}}},
		{Tools: []ClaudeTool{
			{Name: "web_search", Type: "web_search_20250305"},
			{Name: "Read"},
		}},
	} {
		if !hasNativeWebSearchDeclaration(req) {
			t.Fatalf("native declaration was not detected: %#v", req.Tools)
		}
	}
}

func TestHandleClaudeMessagesRejectsUnsupportedWebSearchBeforeMCP(t *testing.T) {
	tests := []struct {
		name    string
		content string
		tools   []ClaudeTool
	}{
		{
			name:    "unknown version",
			content: webSearchQueryPrefix + "release notes",
			tools:   []ClaudeTool{{Name: "web_search", Type: "web_search_20990101"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := config.Init(t.TempDir() + "/config.json"); err != nil {
				t.Fatalf("config.Init: %v", err)
			}
			var calls int
			h := &Handler{webSearchMCPCaller: func(context.Context, *config.Account, string) (*webSearchResultSet, error) {
				calls++
				return testWebSearchResultSet(), nil
			}}
			body, err := json.Marshal(ClaudeRequest{
				Model:    "claude-sonnet-4-6",
				Messages: []ClaudeMessage{{Role: "user", Content: test.content}},
				Tools:    test.tools,
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			recorder := httptest.NewRecorder()
			h.handleClaudeMessagesInternal(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)))

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if calls != 0 {
				t.Fatalf("MCP calls = %d, want 0", calls)
			}
			if atomic.LoadInt64(&h.totalRequests) != 1 || atomic.LoadInt64(&h.failedRequests) != 1 {
				t.Fatalf("rejection was not recorded once: total=%d failed=%d", h.totalRequests, h.failedRequests)
			}
			if logs := h.getRequestLogs(); len(logs) != 1 || logs[0].Status != "error" {
				t.Fatalf("unexpected rejection logs: %#v", logs)
			}
		})
	}
}

func TestExtractSearchQueryUsesLatestUserContentArray(t *testing.T) {
	req := &ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "user", Content: "Perform a web search for the query: stale query"},
		{Role: "assistant", Content: "previous answer"},
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "image", "source": map[string]interface{}{}},
			map[string]interface{}{"type": "text", "text": "nested request metadata"},
			map[string]interface{}{"type": "text", "text": "Perform a web search for the query: current query"},
		}},
	}}

	query, err := extractSearchQuery(req)
	if err != nil {
		t.Fatalf("extractSearchQuery returned an error: %v", err)
	}
	if query != "current query" {
		t.Fatalf("query = %q, want current query", query)
	}
}

func TestExtractSearchQueryRejectsLatestUserWithoutText(t *testing.T) {
	req := &ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "user", Content: "old query"},
		{Role: "user", Content: []interface{}{map[string]interface{}{"type": "image"}}},
	}}
	if _, err := extractSearchQuery(req); err == nil {
		t.Fatal("expected an error for a latest user message without text")
	}
}

func TestExtractSearchQuerySkipsTrailingToolResultTurn(t *testing.T) {
	req := &ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "user", Content: "Perform a web search for the query: current query"},
		{Role: "assistant", Content: "searching"},
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "tool_result", "tool_use_id": "toolu_search"},
		}},
	}}
	query, err := extractSearchQuery(req)
	if err != nil || query != "current query" {
		t.Fatalf("query=%q err=%v, want current query", query, err)
	}
}

func TestExtractSearchQueryAcceptsGenericAnthropicPrompt(t *testing.T) {
	req := &ClaudeRequest{Messages: []ClaudeMessage{{
		Role:    "user",
		Content: "Research this topic and decide whether to search the web.",
	}}}
	query, err := extractSearchQuery(req)
	if err != nil || query != "Research this topic and decide whether to search the web." {
		t.Fatalf("generic native web-search query = %q, err=%v", query, err)
	}
}

func TestExtractSearchQueryUsesLastPrefixAndTruncatesLongContext(t *testing.T) {
	want := "current query"
	req := &ClaudeRequest{Messages: []ClaudeMessage{{
		Role:    "user",
		Content: strings.Repeat("上下文", webSearchMaxQueryRunes) + "\n" + webSearchQueryPrefix + want,
	}}}
	query, err := extractSearchQuery(req)
	if err != nil || query != want {
		t.Fatalf("prefixed long query = %q, err=%v", query, err)
	}

	plainTail := strings.Repeat("q", webSearchMaxQueryRunes+10)
	query, err = extractSearchQuery(&ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: plainTail}}})
	if err != nil || utf8.RuneCountInString(query) != webSearchMaxQueryRunes || query != plainTail[10:] {
		t.Fatalf("plain long query length=%d err=%v", utf8.RuneCountInString(query), err)
	}
}

func TestCallMCPWebSearchParsesAllContentAndSendsKiroHeaders(t *testing.T) {
	var gotRequest webSearchMCPRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/mcp" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("TokenType"); got != "EXTERNAL_IDP" {
			t.Errorf("TokenType = %q", got)
		}
		if got := r.Header.Get("x-amzn-kiro-profile-arn"); got != "arn:aws:codewhisperer:eu-west-1:1:profile/test" {
			t.Errorf("profile ARN header = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"jsonrpc":"2.0",
			"id":"request-id",
			"result":{"isError":false,"content":[
				{"type":"image","text":"ignored"},
				{"type":"text","text":"not JSON"},
				{"type":"text","text":"{\"results\":[{\"title\":\"First\",\"url\":\"https://one.example\",\"snippet\":\"one\"}],\"totalResults\":2}"},
				{"type":"text","text":"{\"results\":[{\"title\":\"Second\",\"url\":\"https://two.example\"}]}"}
			]}
		}`)
	}))
	defer server.Close()

	account := &config.Account{
		AccessToken: "access-token",
		AuthMethod:  "external_idp",
		ProfileArn:  "arn:aws:codewhisperer:eu-west-1:1:profile/test",
	}
	result, err := callMCPWebSearchWithOptions(context.Background(), account, "golang news", webSearchMCPCallOptions{
		Endpoint: server.URL + "/mcp",
		Client:   server.Client(),
	})
	if err != nil {
		t.Fatalf("callMCPWebSearchWithOptions returned an error: %v", err)
	}
	if gotRequest.JSONRPC != "2.0" || gotRequest.Method != "tools/call" {
		t.Fatalf("unexpected JSON-RPC request: %#v", gotRequest)
	}
	if gotRequest.Params.Name != "web_search" || gotRequest.Params.Arguments.Query != "golang news" {
		t.Fatalf("unexpected MCP params: %#v", gotRequest.Params)
	}
	if !strings.HasPrefix(gotRequest.ID, "web_search_tooluse_") {
		t.Fatalf("unexpected MCP request ID: %q", gotRequest.ID)
	}
	if !strings.HasPrefix(result.ToolUseID, "srvtoolu_") {
		t.Fatalf("unexpected tool use ID: %q", result.ToolUseID)
	}
	if result.Query != "golang news" || len(result.Results) != 2 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.TotalResults == nil || *result.TotalResults != 2 {
		t.Fatalf("unexpected total results: %#v", result.TotalResults)
	}
}

func TestCallMCPWebSearchPropagatesHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "rate limited and potentially sensitive body", http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := callMCPWebSearchWithOptions(context.Background(), nil, "private query", webSearchMCPCallOptions{
		Endpoint: server.URL,
		Client:   server.Client(),
	})
	var upstreamErr *webSearchMCPError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error type = %T, want *webSearchMCPError", err)
	}
	if upstreamErr.StatusCode != http.StatusTooManyRequests || upstreamErr.RetryAfter != "30" {
		t.Fatalf("unexpected upstream error: %#v", upstreamErr)
	}
	if strings.Contains(err.Error(), "private query") || strings.Contains(err.Error(), "sensitive body") {
		t.Fatalf("error leaks request or response content: %v", err)
	}
}

func TestCallMCPWebSearchPreservesMonthlyQuotaReasonForClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"reason":"MONTHLY_REQUEST_COUNT"}}`)
	}))
	defer server.Close()

	_, err := callMCPWebSearchWithOptions(context.Background(), nil, "query", webSearchMCPCallOptions{
		Endpoint: server.URL,
		Client:   server.Client(),
	})
	if !isWebSearchMonthlyRequestCountError(err) {
		t.Fatalf("monthly quota response was not classified: %v", err)
	}
	var upstreamErr *webSearchMCPError
	if !errors.As(err, &upstreamErr) || !strings.Contains(upstreamErr.Body, "MONTHLY_REQUEST_COUNT") {
		t.Fatalf("upstream body was not preserved for internal classification: %#v", err)
	}
}

func TestCallMCPWebSearchRejectsProviderErrorsAndInvalidSuccess(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "json rpc error",
			body: `{"jsonrpc":"2.0","id":"x","error":{"code":-32000,"message":"provider unavailable"}}`,
		},
		{
			name: "tool error",
			body: `{"jsonrpc":"2.0","id":"x","result":{"isError":true,"content":[{"type":"text","text":"search failed"}]}}`,
		},
		{
			name: "missing content",
			body: `{"jsonrpc":"2.0","id":"x","result":{"isError":false,"content":[]}}`,
		},
		{
			name: "unparseable content",
			body: `{"jsonrpc":"2.0","id":"x","result":{"isError":false,"content":[{"type":"text","text":"not JSON"}]}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			_, err := callMCPWebSearchWithOptions(context.Background(), nil, "query", webSearchMCPCallOptions{
				Endpoint: server.URL,
				Client:   server.Client(),
			})
			var upstreamErr *webSearchMCPError
			if !errors.As(err, &upstreamErr) {
				t.Fatalf("error = %v (%T), want *webSearchMCPError", err, err)
			}
			if upstreamErr.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", upstreamErr.StatusCode)
			}
		})
	}
}

func TestCallMCPWebSearchClassifiesJSONRPCFailures(t *testing.T) {
	tests := []struct {
		name      string
		code      int
		retryable bool
	}{
		{name: "invalid params", code: jsonRPCInvalidParams, retryable: false},
		{name: "method not found", code: jsonRPCMethodNotFound, retryable: false},
		{name: "provider transient", code: -32000, retryable: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"x","error":{"code":%d,"message":"sensitive provider detail"}}`, test.code)
			}))
			defer server.Close()

			_, err := callMCPWebSearchWithOptions(context.Background(), nil, "private query", webSearchMCPCallOptions{
				Endpoint: server.URL,
				Client:   server.Client(),
			})
			var upstreamErr *webSearchMCPError
			if !errors.As(err, &upstreamErr) || upstreamErr.RPCCode == nil || *upstreamErr.RPCCode != test.code {
				t.Fatalf("unexpected error: %#v", err)
			}
			if got := shouldRetryWebSearchFailure(err); got != test.retryable {
				t.Fatalf("retryable = %v, want %v", got, test.retryable)
			}
			if strings.Contains(err.Error(), "private query") || strings.Contains(err.Error(), "sensitive provider detail") {
				t.Fatalf("error leaks request or provider content: %v", err)
			}
		})
	}
}

func TestCallMCPWebSearchRetriesInvalidParamsWithCompactedQuery(t *testing.T) {
	originalQuery := strings.Repeat("\u6d4b", 1800) + "\x00\n latest release"
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request webSearchMCPRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if calls == 1 {
			if request.Params.Arguments.Query != strings.TrimSpace(originalQuery) {
				t.Fatalf("first query was changed before rejection")
			}
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"x","error":{"code":-32602,"message":"sensitive invalid params detail"}}`)
			return
		}
		if got := len(request.Params.Arguments.Query); got > webSearchMCPRetryMaxBytes {
			t.Fatalf("compatibility query bytes = %d, want <= %d", got, webSearchMCPRetryMaxBytes)
		}
		if strings.ContainsRune(request.Params.Arguments.Query, '\x00') {
			t.Fatal("compatibility query retained a control character")
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"x","result":{"isError":false,"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Release\",\"url\":\"https://example.com/release\"}]}"}]}}`)
	}))
	defer server.Close()

	result, err := callMCPWebSearchWithOptions(context.Background(), nil, originalQuery, webSearchMCPCallOptions{
		Endpoint: server.URL,
		Client:   server.Client(),
	})
	if err != nil {
		t.Fatalf("callMCPWebSearchWithOptions returned an error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("MCP calls = %d, want 2", calls)
	}
	if result.Query != strings.TrimSpace(originalQuery) {
		t.Fatal("client-visible query did not preserve the original value")
	}
}

func TestCallMCPWebSearchLimitsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 65))
	}))
	defer server.Close()

	_, err := callMCPWebSearchWithOptions(context.Background(), nil, "query", webSearchMCPCallOptions{
		Endpoint:         server.URL,
		Client:           server.Client(),
		MaxResponseBytes: 64,
	})
	var upstreamErr *webSearchMCPError
	if !errors.As(err, &upstreamErr) || upstreamErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("error = %#v, want a 502 webSearchMCPError", err)
	}
}

func TestCallMCPWebSearchMapsClientTimeout(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}

	_, err := callMCPWebSearchWithOptions(context.Background(), nil, "query", webSearchMCPCallOptions{
		Endpoint: "https://q.us-east-1.amazonaws.com/mcp",
		Client:   client,
	})
	var upstreamErr *webSearchMCPError
	if !errors.As(err, &upstreamErr) || upstreamErr.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("error = %#v, want a 504 webSearchMCPError", err)
	}
	if shouldRetryWebSearchFailure(err) {
		t.Fatal("client timeout must not retry across every account")
	}
}

func TestWriteWebSearchResponseNonStream(t *testing.T) {
	result := testWebSearchResultSet()
	recorder := httptest.NewRecorder()
	outputTokens, err := writeWebSearchResponse(recorder, &ClaudeRequest{
		Model: "claude-sonnet-4-6",
	}, result, 17)
	if err != nil {
		t.Fatalf("writeWebSearchResponse returned an error: %v", err)
	}
	if outputTokens <= 0 {
		t.Fatalf("output tokens = %d", outputTokens)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["type"] != "message" || response["stop_reason"] != "end_turn" {
		t.Fatalf("unexpected response envelope: %#v", response)
	}
	blocks, ok := response["content"].([]interface{})
	if !ok || len(blocks) != 3 {
		t.Fatalf("content = %#v", response["content"])
	}
	serverUse := blocks[0].(map[string]interface{})
	if serverUse["type"] != "server_tool_use" || serverUse["id"] != result.ToolUseID {
		t.Fatalf("server tool block = %#v", serverUse)
	}
	if _, exists := serverUse["caller"]; exists {
		t.Fatalf("server tool block must not contain caller: %#v", serverUse)
	}
	toolResult := blocks[1].(map[string]interface{})
	if toolResult["type"] != "web_search_tool_result" || toolResult["tool_use_id"] != result.ToolUseID {
		t.Fatalf("tool result block = %#v", toolResult)
	}
	caller := toolResult["caller"].(map[string]interface{})
	if caller["type"] != "direct" {
		t.Fatalf("tool result caller = %#v", caller)
	}
	searchResult := toolResult["content"].([]interface{})[0].(map[string]interface{})
	if searchResult["encrypted_content"] == *result.Results[0].Snippet || strings.TrimSpace(searchResult["encrypted_content"].(string)) == "" {
		t.Fatalf("encrypted_content must be opaque: %#v", searchResult)
	}
	if !strings.Contains(blocks[2].(map[string]interface{})["text"].(string), "https://example.com/article") {
		t.Fatalf("summary does not contain source URL: %#v", blocks[2])
	}
	citations := blocks[2].(map[string]interface{})["citations"].([]interface{})
	if len(citations) == 0 || citations[0].(map[string]interface{})["type"] != "web_search_result_location" {
		t.Fatalf("citations = %#v", citations)
	}
	usage := response["usage"].(map[string]interface{})
	serverUsage := usage["server_tool_use"].(map[string]interface{})
	if serverUsage["web_search_requests"] != float64(1) {
		t.Fatalf("usage = %#v", usage)
	}
	if serverUsage["web_fetch_requests"] != float64(0) {
		t.Fatalf("usage is missing zero web_fetch requests: %#v", usage)
	}
}

func TestWriteWebSearchResponseStream(t *testing.T) {
	result := testWebSearchResultSet()
	recorder := httptest.NewRecorder()
	_, err := writeWebSearchResponse(recorder, &ClaudeRequest{
		Model:  "claude-sonnet-4-6",
		Stream: true,
	}, result, 17)
	if err != nil {
		t.Fatalf("writeWebSearchResponse returned an error: %v", err)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}

	body := recorder.Body.String()
	for _, expected := range []string{
		"event: message_start",
		`"type":"server_tool_use"`,
		`"type":"input_json_delta"`,
		`"type":"web_search_tool_result"`,
		`"type":"citations_delta"`,
		`"tool_use_id":"srvtoolu_test"`,
		"https://example.com/article",
		`"web_search_requests":1`,
		"event: message_stop",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("SSE response is missing %q:\n%s", expected, body)
		}
	}

	events := decodeWebSearchSSE(t, body)
	var sawEmptyServerInput, sawEmptyInputDelta, sawQueryDelta bool
	positions := map[string]int{}
	for eventIndex, event := range events {
		index, _ := event.Data["index"].(float64)
		if event.Name == "content_block_start" && int(index) == 0 {
			block, _ := event.Data["content_block"].(map[string]interface{})
			input, _ := block["input"].(map[string]interface{})
			sawEmptyServerInput = block["type"] == "server_tool_use" && len(input) == 0
			positions["server_start"] = eventIndex
		}
		if event.Name == "ping" {
			positions["ping"] = eventIndex
		}
		if event.Name == "content_block_delta" && int(index) == 0 {
			delta, _ := event.Data["delta"].(map[string]interface{})
			partial, _ := delta["partial_json"].(string)
			if delta["type"] == "input_json_delta" && partial == "" {
				sawEmptyInputDelta = true
			}
			var input map[string]interface{}
			if delta["type"] == "input_json_delta" && json.Unmarshal([]byte(partial), &input) == nil {
				sawQueryDelta = input["query"] == result.Query
			}
		}
	}
	if !sawEmptyServerInput || !sawEmptyInputDelta || !sawQueryDelta {
		t.Fatalf("invalid streamed server tool input: empty_start=%v empty_delta=%v query_delta=%v", sawEmptyServerInput, sawEmptyInputDelta, sawQueryDelta)
	}
	if positions["server_start"] >= positions["ping"] {
		t.Fatalf("ping must follow server tool start: %#v", positions)
	}
}

func TestWriteWebSearchResponseEmptyResultsStillCountsSearch(t *testing.T) {
	result := &webSearchResultSet{ToolUseID: "srvtoolu_empty", Query: "no matches"}
	recorder := httptest.NewRecorder()
	if _, err := writeWebSearchResponse(recorder, &ClaudeRequest{Model: "model"}, result, 5); err != nil {
		t.Fatalf("writeWebSearchResponse: %v", err)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	blocks := response["content"].([]interface{})
	toolResult := blocks[1].(map[string]interface{})
	content := toolResult["content"].([]interface{})
	if len(content) != 0 || toolResult["tool_use_id"] != result.ToolUseID {
		t.Fatalf("unexpected empty result block: %#v", toolResult)
	}
	serverUsage := response["usage"].(map[string]interface{})["server_tool_use"].(map[string]interface{})
	if serverUsage["web_search_requests"] != float64(1) {
		t.Fatalf("empty result must still count one completed search: %#v", serverUsage)
	}
}

func TestFilterWebSearchResultsEnforcesDomainBoundaries(t *testing.T) {
	results := []webSearchResult{
		{Title: "root", URL: "https://example.com/root"},
		{Title: "subdomain", URL: "https://news.example.com/story"},
		{Title: "blocked", URL: "https://blocked.example.com/story"},
		{Title: "suffix attack", URL: "https://example.com.evil.test/story"},
		{Title: "other", URL: "https://other.test/story"},
		{Title: "invalid", URL: "not-a-url"},
	}
	filtered := filterWebSearchResults(results, ClaudeTool{
		AllowedDomains: []string{"HTTPS://EXAMPLE.COM./allowed"},
		BlockedDomains: []string{"*.blocked.example.com"},
	})
	if len(filtered) != 2 || filtered[0].Title != "root" || filtered[1].Title != "subdomain" {
		t.Fatalf("unexpected filtered results: %#v", filtered)
	}
}

func TestBuildWebSearchMCPQueryAppliesDomainConstraints(t *testing.T) {
	tool := ClaudeTool{
		AllowedDomains: []string{"docs.example.com", "news.example.com"},
		BlockedDomains: []string{"archive.example.com"},
	}
	query := buildWebSearchMCPQuery("release notes", tool)
	for _, want := range []string{
		"(site:docs.example.com OR site:news.example.com)",
		"release notes",
		"-site:archive.example.com",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("constrained query %q does not contain %q", query, want)
		}
	}
}

func TestCombinedWebSearchQueryHasOutboundLimit(t *testing.T) {
	domain := strings.Repeat("a", 240) + ".example.com"
	tool := ClaudeTool{
		AllowedDomains: make([]string, webSearchMaxDomains),
		BlockedDomains: make([]string, webSearchMaxDomains),
	}
	for i := range tool.AllowedDomains {
		tool.AllowedDomains[i] = domain
		tool.BlockedDomains[i] = domain
	}
	if err := validateWebSearchToolOptions(tool); err != nil {
		t.Fatalf("individual domain filters should be valid: %v", err)
	}
	query := buildWebSearchMCPQuery(strings.Repeat("q", webSearchMaxQueryRunes), tool)
	if got := utf8.RuneCountInString(query); got > webSearchMaxUpstreamRunes {
		t.Fatalf("outbound query length = %d, want <= %d", got, webSearchMaxUpstreamRunes)
	}
	if strings.Contains(query, "site:") {
		t.Fatalf("oversized domain clauses must be dropped before MCP call: %q", query)
	}
	if query != strings.Repeat("q", webSearchMaxQueryRunes) {
		t.Fatalf("user query was not preserved: length=%d", utf8.RuneCountInString(query))
	}
}

func TestFilterWebSearchResultsRejectsUnsafeURLsAndCapsOutput(t *testing.T) {
	results := []webSearchResult{
		{Title: "script", URL: "javascript:alert(1)"},
		{Title: "file", URL: "file:///etc/passwd"},
		{Title: "credentials", URL: "https://user:pass@example.com/private"},
	}
	for i := 0; i < webSearchMaxResults+5; i++ {
		results = append(results, webSearchResult{Title: "safe", URL: fmt.Sprintf("https://example.com/%d", i)})
	}

	filtered := filterWebSearchResults(results, ClaudeTool{})
	if len(filtered) != webSearchMaxResults {
		t.Fatalf("filtered result count = %d, want %d", len(filtered), webSearchMaxResults)
	}
	for _, result := range filtered {
		if !strings.HasPrefix(result.URL, "https://example.com/") {
			t.Fatalf("unsafe result survived filtering: %#v", result)
		}
	}
}

func TestWriteWebSearchResponseChecksFlusherBeforeWriting(t *testing.T) {
	writer := &nonFlushingResponseWriter{header: make(http.Header)}
	_, err := writeWebSearchResponse(writer, &ClaudeRequest{Model: "model", Stream: true}, testWebSearchResultSet(), 1)
	if err == nil {
		t.Fatal("expected an error for a non-flushing response writer")
	}
	if writer.body.Len() != 0 {
		t.Fatalf("wrote a partial response before checking Flusher: %q", writer.body.String())
	}
}

func TestHandleClaudeMessagesRoutesWebSearchAndRecordsUsage(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "websearch-success-account",
		Enabled:     true,
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:1:profile/websearch-success",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	apiKey, err := config.AddApiKey(config.ApiKeyEntry{
		Name:    "websearch-test",
		Key:     "sk-websearch-test",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("add API key: %v", err)
	}

	pool := accountpool.GetPool()
	pool.Reload()
	t.Cleanup(pool.WaitForPendingStats)
	tracker := NewTrafficTracker(time.Hour)
	var calls int
	h := &Handler{
		pool:    pool,
		traffic: tracker,
		webSearchMCPCaller: func(_ context.Context, account *config.Account, query string) (*webSearchResultSet, error) {
			calls++
			if account.ID != "websearch-success-account" {
				t.Fatalf("unexpected account: %#v", account)
			}
			if query != "solid state drive news" {
				t.Fatalf("query = %q", query)
			}
			result := testWebSearchResultSet()
			result.Query = query
			return result, nil
		},
	}

	body := `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"Perform a web search for the query: solid state drive news"}],
		"tools":[
			{"name":"Read","description":"Read a file","input_schema":{"type":"object"}},
			{"type":"web_search_20250305","name":"web_search","max_uses":8}
		]
	}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request = withApiKeyContext(request, &apiKey)
	recorder := httptest.NewRecorder()

	// promptCache is deliberately nil: the native WebSearch branch must run
	// before the normal prompt-cache and ClaudeToKiro path.
	h.handleClaudeMessagesInternal(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls != 1 {
		t.Fatalf("MCP calls = %d, want 1", calls)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	usage := response["usage"].(map[string]interface{})
	inputTokens := int(usage["input_tokens"].(float64))
	outputTokens := int(usage["output_tokens"].(float64))
	totalTokens := inputTokens + outputTokens
	if inputTokens <= 0 || outputTokens <= 0 {
		t.Fatalf("unexpected usage: %#v", usage)
	}

	if got := atomic.LoadInt64(&h.totalRequests); got != 1 {
		t.Fatalf("total requests = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&h.successRequests); got != 1 {
		t.Fatalf("success requests = %d, want 1", got)
	}
	traffic := tracker.Query(time.Hour).Totals
	if traffic.Requests != 1 || traffic.InputTokens != uint64(inputTokens) || traffic.OutputTokens != uint64(outputTokens) {
		t.Fatalf("unexpected traffic totals: %#v", traffic)
	}
	storedKey := config.GetApiKeyEntry(apiKey.ID)
	if storedKey == nil || storedKey.RequestsCount != 1 || storedKey.TokensUsed != int64(totalTokens) {
		t.Fatalf("unexpected API key usage: %#v", storedKey)
	}
	storedAccount := pool.GetByID("websearch-success-account")
	if storedAccount == nil || storedAccount.RequestCount != 1 || storedAccount.TotalTokens != totalTokens {
		t.Fatalf("unexpected account stats: %#v", storedAccount)
	}
	logs := h.getRequestLogs()
	if len(logs) != 1 || logs[0].Status != "success" || logs[0].Tokens != totalTokens {
		t.Fatalf("unexpected request logs: %#v", logs)
	}
}

func TestExecuteWebSearchRetriesRPCRejectionOnAlternateAccount(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, id := range []string{"websearch-rpc-account-a", "websearch-rpc-account-b"} {
		if err := config.AddAccount(config.Account{
			ID:          id,
			Enabled:     true,
			AccessToken: "access-token",
			ProfileArn:  "arn:aws:codewhisperer:us-east-1:1:profile/" + id,
		}); err != nil {
			t.Fatalf("add account %s: %v", id, err)
		}
	}

	pool := accountpool.GetPool()
	pool.Reload()
	h := &Handler{pool: pool}
	var calls int
	var firstAccountID string
	execution, err := h.executeWebSearch(
		context.Background(),
		"release notes",
		"release notes",
		ClaudeTool{},
		func(_ context.Context, account *config.Account, query string) (*webSearchResultSet, error) {
			calls++
			if calls == 1 {
				firstAccountID = account.ID
				code := jsonRPCInvalidParams
				return nil, &webSearchMCPError{
					StatusCode: http.StatusBadGateway,
					RPCCode:    &code,
					Message:    "web search MCP upstream rejected the request",
				}
			}
			result := testWebSearchResultSet()
			result.Query = query
			return result, nil
		},
	)
	if err != nil {
		t.Fatalf("executeWebSearch returned an error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("MCP calls = %d, want 2", calls)
	}
	if execution == nil || execution.Account == nil || execution.Account.ID == firstAccountID {
		t.Fatalf("alternate account was not used: first=%q execution=%#v", firstAccountID, execution)
	}
	if got := pool.AvailableCount(); got != 2 {
		t.Fatalf("RPC rejection cooled down a chat-capable account: available=%d", got)
	}
}

func TestHandleWebSearchRPCRejectionReturnsStructuredUnavailableResult(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "websearch-rpc-rejected",
		Enabled:     true,
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:1:profile/websearch-rpc-rejected",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	pool := accountpool.GetPool()
	pool.Reload()
	h := &Handler{pool: pool, traffic: NewTrafficTracker(time.Hour)}
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []ClaudeMessage{{Role: "user", Content: webSearchQueryPrefix + "release notes"}},
		Tools:    []ClaudeTool{{Type: "web_search_20250305", Name: "web_search"}},
	}

	var calls int
	recorder := httptest.NewRecorder()
	h.handleWebSearchRequestWithCaller(recorder, context.Background(), req, 10, "",
		func(context.Context, *config.Account, string) (*webSearchResultSet, error) {
			calls++
			code := jsonRPCInvalidParams
			return nil, &webSearchMCPError{
				StatusCode: http.StatusBadGateway,
				RPCCode:    &code,
				Message:    "web search MCP upstream rejected the request (JSON-RPC -32602)",
			}
		})

	if recorder.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	blocks, _ := response["content"].([]interface{})
	if len(blocks) != 3 {
		t.Fatalf("structured fallback content=%#v", response["content"])
	}
	toolResult := blocks[1].(map[string]interface{})
	results, ok := toolResult["content"].([]interface{})
	if !ok || len(results) != 0 || toolResult["type"] != "web_search_tool_result" {
		t.Fatalf("fallback tool result=%#v", toolResult)
	}
	summary := blocks[2].(map[string]interface{})["text"].(string)
	if !strings.Contains(summary, "temporarily unavailable") || strings.Contains(summary, "No results found") {
		t.Fatalf("fallback summary=%q", summary)
	}
	logs := h.getRequestLogs()
	if len(logs) != 1 || logs[0].Status != "error" || logs[0].ErrorType != "websearch" {
		t.Fatalf("fallback logs=%#v", logs)
	}
}

func TestHandleWebSearchFailureReturnsRateLimitAndRecordsOnce(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "websearch-rate-limit-account",
		Enabled:     true,
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:1:profile/websearch-rate-limit",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	apiKey, err := config.AddApiKey(config.ApiKeyEntry{
		Name:    "websearch-rate-limit",
		Key:     "sk-websearch-rate-limit",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("add API key: %v", err)
	}

	pool := accountpool.GetPool()
	pool.Reload()
	tracker := NewTrafficTracker(time.Hour)
	h := &Handler{pool: pool, traffic: tracker}
	req := &ClaudeRequest{
		Model: "claude-sonnet-4-6",
		Messages: []ClaudeMessage{{
			Role:    "user",
			Content: "Perform a web search for the query: rate limited query",
		}},
		Tools: []ClaudeTool{{Type: "web_search_20250305", Name: "web_search"}},
	}

	var calls int
	recorder := httptest.NewRecorder()
	h.handleWebSearchRequestWithCaller(
		recorder,
		context.Background(),
		req,
		20,
		apiKey.ID,
		func(context.Context, *config.Account, string) (*webSearchResultSet, error) {
			calls++
			return nil, &webSearchMCPError{
				StatusCode: http.StatusTooManyRequests,
				RetryAfter: "30",
				Message:    "web search MCP upstream returned HTTP 429",
			}
		},
	)

	if calls != 1 {
		t.Fatalf("MCP calls = %d, want 1", calls)
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if retryAfter := recorder.Header().Get("Retry-After"); retryAfter != "30" {
		t.Fatalf("Retry-After = %q", retryAfter)
	}
	if got := atomic.LoadInt64(&h.totalRequests); got != 1 {
		t.Fatalf("total requests = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failed requests = %d, want 1", got)
	}
	if got := tracker.Query(time.Hour).Totals.Requests; got != 1 {
		t.Fatalf("traffic requests = %d, want 1", got)
	}
	storedKey := config.GetApiKeyEntry(apiKey.ID)
	if storedKey == nil || storedKey.RequestsCount != 0 || storedKey.TokensUsed != 0 {
		t.Fatalf("failed search should not consume API key usage: %#v", storedKey)
	}
	logs := h.getRequestLogs()
	if len(logs) != 1 || logs[0].Status != "error" || logs[0].ErrorType != "quota" {
		t.Fatalf("unexpected request logs: %#v", logs)
	}
}

func TestHandleWebSearchForbiddenDoesNotDisableChatAccount(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const accountID = "websearch-forbidden-account"
	if err := config.AddAccount(config.Account{
		ID:          accountID,
		Enabled:     true,
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:1:profile/websearch-forbidden",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	pool := accountpool.GetPool()
	pool.Reload()
	h := &Handler{pool: pool, traffic: NewTrafficTracker(time.Hour)}
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []ClaudeMessage{{Role: "user", Content: "Perform a web search for the query: restricted"}},
		Tools:    []ClaudeTool{{Type: "web_search_20250305", Name: "web_search"}},
	}

	recorder := httptest.NewRecorder()
	h.handleWebSearchRequestWithCaller(recorder, context.Background(), req, 10, "",
		func(context.Context, *config.Account, string) (*webSearchResultSet, error) {
			return nil, &webSearchMCPError{
				StatusCode: http.StatusForbidden,
				Message:    "web search MCP upstream denied web search access",
			}
		})

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	found := false
	for _, account := range config.GetAccounts() {
		if account.ID == accountID && !account.Enabled {
			t.Fatalf("WebSearch entitlement 403 disabled a chat-capable account: %#v", account)
		}
		if account.ID == accountID {
			found = true
		}
	}
	if !found {
		t.Fatal("WebSearch entitlement 403 removed the account")
	}
}

func TestHandleWebSearchPaymentRequiredStartsQuotaCooldown(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const accountID = "websearch-payment-account"
	if err := config.AddAccount(config.Account{
		ID:          accountID,
		Enabled:     true,
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:1:profile/websearch-payment",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	pool := accountpool.GetPool()
	pool.Reload()
	h := &Handler{pool: pool}
	account := pool.GetByID(accountID)
	if account == nil {
		t.Fatal("payment account not found")
	}

	h.handleWebSearchAccountFailure(account, &webSearchMCPError{StatusCode: http.StatusPaymentRequired})
	if got := pool.AvailableCount(); got != 0 {
		t.Fatalf("available accounts after HTTP 402 = %d, want 0 during quota cooldown", got)
	}
	if got := classifyError((&webSearchMCPError{StatusCode: http.StatusPaymentRequired}).Error()); got != "quota" {
		t.Fatalf("HTTP 402 log classification = %q, want quota", got)
	}
}

func TestWebSearchBearerInvalidForcesOneOAuthRefresh(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const accountID = "websearch-bearer-refresh-account"
	if err := config.AddAccount(config.Account{
		ID:           accountID,
		Enabled:      true,
		AccessToken:  "stale-access-token",
		RefreshToken: "refresh-token",
		AuthMethod:   "idc",
		ProfileArn:   "arn:aws:codewhisperer:us-east-1:1:profile/websearch-refresh",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	var calls, refreshes int
	h := &Handler{
		pool: p,
		tokenRefresher: func(_ context.Context, account *config.Account, _ bool) error {
			refreshes++
			account.AccessToken = "fresh-access-token"
			return nil
		},
	}
	tool := ClaudeTool{Type: "web_search_20250305", Name: "web_search"}
	execution, err := h.executeWebSearch(context.Background(), "query", "query", tool,
		func(_ context.Context, account *config.Account, _ string) (*webSearchResultSet, error) {
			calls++
			if account.AccessToken != "fresh-access-token" {
				return nil, &webSearchMCPError{
					StatusCode: http.StatusUnauthorized,
					Body:       `{"message":"The bearer token included in the request is invalid."}`,
					Message:    "web search MCP upstream returned HTTP 401",
				}
			}
			return testWebSearchResultSet(), nil
		})
	if err != nil {
		t.Fatalf("executeWebSearch returned an error: %v", err)
	}
	if execution == nil || execution.Account == nil || execution.Account.ID != accountID {
		t.Fatalf("unexpected execution: %#v", execution)
	}
	if calls != 2 || refreshes != 1 {
		t.Fatalf("calls=%d refreshes=%d, want one forced refresh and two MCP calls", calls, refreshes)
	}
}

func TestWebSearchMonthlyRequestCountDisablesAccount(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const accountID = "websearch-monthly-quota-account"
	if err := config.AddAccount(config.Account{
		ID:          accountID,
		Enabled:     true,
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:1:profile/websearch-quota",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	account := p.GetByID(accountID)
	if account == nil {
		t.Fatal("quota account not found")
	}
	h.handleWebSearchAccountFailure(account, &webSearchMCPError{
		StatusCode: http.StatusPaymentRequired,
		Body:       `{"reason":"MONTHLY_REQUEST_COUNT"}`,
		Message:    "web search MCP quota exceeded (HTTP 402)",
	})
	var disabled *config.Account
	for _, stored := range config.GetAccounts() {
		if stored.ID == accountID {
			copy := stored
			disabled = &copy
			break
		}
	}
	if disabled == nil || disabled.Enabled || !strings.Contains(disabled.BanReason, "MONTHLY_REQUEST_COUNT") {
		t.Fatalf("monthly quota did not disable account: %#v", disabled)
	}
	if got := p.AvailableCount(); got != 0 {
		t.Fatalf("available accounts after monthly quota = %d, want 0", got)
	}
}

func testWebSearchResultSet() *webSearchResultSet {
	snippet := "An example search result."
	published := int64(1_735_689_600_000)
	return &webSearchResultSet{
		ToolUseID: "srvtoolu_test",
		Query:     "example query",
		Results: []webSearchResult{{
			Title:         "Example",
			URL:           "https://example.com/article",
			Snippet:       &snippet,
			PublishedDate: &published,
		}},
	}
}

type nonFlushingResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *nonFlushingResponseWriter) Header() http.Header { return w.header }
func (w *nonFlushingResponseWriter) Write(body []byte) (int, error) {
	return w.body.Write(body)
}
func (w *nonFlushingResponseWriter) WriteHeader(statusCode int) { w.status = statusCode }

type decodedWebSearchSSEEvent struct {
	Name string
	Data map[string]interface{}
}

func decodeWebSearchSSE(t *testing.T, body string) []decodedWebSearchSSEEvent {
	t.Helper()
	events := make([]decodedWebSearchSSEEvent, 0)
	for _, frame := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var event decodedWebSearchSSEEvent
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event.Data); err != nil {
					t.Fatalf("decode SSE data: %v", err)
				}
			}
		}
		if event.Name != "" && event.Data != nil {
			events = append(events, event)
		}
	}
	return events
}
