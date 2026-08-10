package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeCodeStreamBuffersAndPatchesMessageStartUsage(t *testing.T) {
	recorder := &flushSnapshotResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
	streamedBeforeContext := false
	pingBeforeContext := false
	h := newResponseContractHandler(t)
	h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnText("visible only after completion", false)
		streamedBeforeContext = strings.Contains(recorder.Body.String(), "visible only after completion")
		pingBeforeContext = strings.Contains(recorder.Body.String(), "event: ping")
		callback.OnContextUsage(12.5)
		callback.OnComplete(999_999, 4)
		return nil
	}

	body := `{
		"model":"claude-opus-5",
		"max_tokens":32,
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/cc/v1/messages", strings.NewReader(body))
	h.handleClaudeMessagesInternal(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if streamedBeforeContext {
		t.Fatal("/cc stream exposed assistant output before contextUsageEvent")
	}
	if !pingBeforeContext {
		t.Fatal("/cc stream did not forward the initial keep-alive before upstream completion")
	}
	if len(recorder.snapshots) != 2 {
		t.Fatalf("flushes=%d, want initial ping plus one buffered flush", len(recorder.snapshots))
	}
	if !strings.Contains(recorder.snapshots[0], "event: ping") || strings.Contains(recorder.snapshots[0], "event: message_start") {
		t.Fatalf("initial snapshot was not ping-only: %s", recorder.snapshots[0])
	}

	var startUsage, terminalUsage map[string]interface{}
	for _, event := range decodeResponseContractSSE(t, recorder.Body.String()) {
		switch event.Name {
		case "message_start":
			message := responseContractMap(t, event.Data["message"], "message_start.message")
			startUsage = responseContractMap(t, message["usage"], "message_start.message.usage")
		case "message_delta":
			terminalUsage = responseContractMap(t, event.Data["usage"], "message_delta.usage")
		}
	}
	if startUsage == nil || terminalUsage == nil {
		t.Fatalf("missing usage events: start=%#v terminal=%#v body=%s", startUsage, terminalUsage, recorder.Body.String())
	}
	if got := int(startUsage["input_tokens"].(float64)); got != 125_000 {
		t.Fatalf("message_start input_tokens=%d, want Kiro context occupancy 125000", got)
	}
	assertKiroContextUsageJSON(t, terminalUsage["kiro_context_usage"], 125_000, 1_000_000, 12.5, false)
}

func TestClaudeCodeStreamKeepsEstimatedUsageWithoutContextEvent(t *testing.T) {
	h := newResponseContractHandler(t)
	h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnText("done", false)
		callback.OnComplete(999_999, 1)
		return nil
	}

	body := `{"model":"claude-opus-5","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	var request ClaudeRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("decode request fixture: %v", err)
	}
	want := estimateClaudeRequestInputTokens(&request)
	recorder := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(
		recorder,
		httptest.NewRequest(http.MethodPost, "/cc/v1/messages", strings.NewReader(body)),
	)

	for _, event := range decodeResponseContractSSE(t, recorder.Body.String()) {
		if event.Name != "message_start" {
			continue
		}
		message := responseContractMap(t, event.Data["message"], "message_start.message")
		usage := responseContractMap(t, message["usage"], "message_start.message.usage")
		if got := int(usage["input_tokens"].(float64)); got != want {
			t.Fatalf("message_start input_tokens=%d, want estimate %d without contextUsageEvent", got, want)
		}
		return
	}
	t.Fatalf("message_start missing: %s", recorder.Body.String())
}
