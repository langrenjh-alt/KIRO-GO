package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeUsageDoesNotExposeKiroHiddenContext(t *testing.T) {
	h := newResponseContractHandler(t,
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 100}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}),
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-opus-5",
		"max_tokens":32,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	rec := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if response.Usage.InputTokens >= 100 {
		t.Fatalf("usage input_tokens=%d includes hidden Kiro context", response.Usage.InputTokens)
	}
	if got := rec.Header().Get("request-id"); !strings.HasPrefix(got, "req_") {
		t.Fatalf("request-id=%q, want Anthropic req_ prefix", got)
	}
	if got := rec.Header().Get("x-request-id"); got != "" {
		t.Fatalf("x-request-id=%q must be left to the outer gateway", got)
	}
	if response.Usage.OutputTokensDetails == nil || response.Usage.OutputTokensDetails.ThinkingTokens != 0 {
		t.Fatalf("plain response thinking usage = %#v", response.Usage.OutputTokensDetails)
	}
}
