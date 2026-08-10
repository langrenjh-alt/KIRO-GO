package proxy

import (
	"bytes"
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
	for name, usage := range map[string]map[string]interface{}{
		"message_start": startUsage,
		"message_delta": terminalUsage,
	} {
		total := int(usage["input_tokens"].(float64) +
			usage["cache_creation_input_tokens"].(float64) +
			usage["cache_read_input_tokens"].(float64))
		if total != 125_000 {
			t.Fatalf("%s input total=%d, want Kiro context occupancy 125000: %#v", name, total, usage)
		}
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

func TestPatchClaudeCodeUsageTotalsKeepsContextTotalAcrossTerminalUsage(t *testing.T) {
	body := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1,"cache_creation_input_tokens":30,"cache_read_input_tokens":20,"cache_creation":{"ephemeral_5m_input_tokens":18,"ephemeral_1h_input_tokens":12}}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":7,"output_tokens":4,"cache_creation_input_tokens":40,"cache_read_input_tokens":10,"kiro_context_usage":{"input_tokens":1000,"context_window":1000000,"percentage":0.1,"exceeded":false}}}` + "\n\n")

	patched := patchClaudeCodeUsageTotals(body)
	var startUsage, terminalUsage map[string]interface{}
	for _, event := range decodeResponseContractSSE(t, string(patched)) {
		switch event.Name {
		case "message_start":
			message := responseContractMap(t, event.Data["message"], "message_start.message")
			startUsage = responseContractMap(t, message["usage"], "message_start.message.usage")
		case "message_delta":
			terminalUsage = responseContractMap(t, event.Data["usage"], "message_delta.usage")
		}
	}
	if startUsage == nil || terminalUsage == nil {
		t.Fatalf("missing usage events: start=%#v terminal=%#v body=%s", startUsage, terminalUsage, patched)
	}

	for name, usage := range map[string]map[string]interface{}{
		"message_start": startUsage,
		"message_delta": terminalUsage,
	} {
		input := int(usage["input_tokens"].(float64))
		creation := int(usage["cache_creation_input_tokens"].(float64))
		read := int(usage["cache_read_input_tokens"].(float64))
		if input != 950 || creation != 40 || read != 10 {
			t.Fatalf("%s usage=%#v, want input/cache_creation/cache_read=950/40/10", name, usage)
		}
		if total := input + creation + read; total != 1000 {
			t.Fatalf("%s input total=%d, want Kiro context occupancy 1000", name, total)
		}
	}
	if got := int(startUsage["output_tokens"].(float64)); got != 1 {
		t.Fatalf("message_start output_tokens=%d, want preserved value 1", got)
	}
	cacheCreation := responseContractMap(t, startUsage["cache_creation"], "message_start.usage.cache_creation")
	fiveMinute := int(cacheCreation["ephemeral_5m_input_tokens"].(float64))
	oneHour := int(cacheCreation["ephemeral_1h_input_tokens"].(float64))
	if fiveMinute != 24 || oneHour != 16 || fiveMinute+oneHour != 40 {
		t.Fatalf("message_start cache_creation=%#v, want 24/16 split summing to 40", cacheCreation)
	}
	if got := int(terminalUsage["output_tokens"].(float64)); got != 4 {
		t.Fatalf("message_delta output_tokens=%d, want preserved value 4", got)
	}
	assertKiroContextUsageJSON(t, terminalUsage["kiro_context_usage"], 1000, 1_000_000, 0.1, false)
}

func TestPatchClaudeCodeUsageTotalsClampsCacheToContext(t *testing.T) {
	body := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0,"cache_creation_input_tokens":54,"cache_read_input_tokens":36,"cache_creation":{"ephemeral_5m_input_tokens":54,"ephemeral_1h_input_tokens":36}}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":90,"cache_read_input_tokens":60,"cache_creation":{"ephemeral_5m_input_tokens":54,"ephemeral_1h_input_tokens":36},"kiro_context_usage":{"input_tokens":100,"context_window":1000000,"percentage":0.01,"exceeded":false}}}` + "\n\n")

	patched := patchClaudeCodeUsageTotals(body)
	var startUsage, terminalUsage map[string]interface{}
	for _, event := range decodeResponseContractSSE(t, string(patched)) {
		switch event.Name {
		case "message_start":
			message := responseContractMap(t, event.Data["message"], "message_start.message")
			startUsage = responseContractMap(t, message["usage"], "message_start.message.usage")
		case "message_delta":
			terminalUsage = responseContractMap(t, event.Data["usage"], "message_delta.usage")
		}
	}
	if startUsage == nil || terminalUsage == nil {
		t.Fatalf("missing usage events: start=%#v terminal=%#v body=%s", startUsage, terminalUsage, patched)
	}
	for name, usage := range map[string]map[string]interface{}{
		"message_start": startUsage,
		"message_delta": terminalUsage,
	} {
		input := int(usage["input_tokens"].(float64))
		creation := int(usage["cache_creation_input_tokens"].(float64))
		read := int(usage["cache_read_input_tokens"].(float64))
		if input+creation+read != 100 {
			t.Fatalf("%s usage=%#v, total=%d, want 100", name, usage, input+creation+read)
		}
		if input != 0 || creation != 60 || read != 40 {
			t.Fatalf("%s usage=%#v, want input/cache_creation/cache_read=0/60/40", name, usage)
		}
	}
	cacheCreation := responseContractMap(t, startUsage["cache_creation"], "message_start.usage.cache_creation")
	if got := int(cacheCreation["ephemeral_5m_input_tokens"].(float64)); got != 36 {
		t.Fatalf("5m cache=%d, want 36", got)
	}
	if got := int(cacheCreation["ephemeral_1h_input_tokens"].(float64)); got != 24 {
		t.Fatalf("1h cache=%d, want 24", got)
	}
}

func TestPatchClaudeCodeUsageTotalsLeavesIntermediateDeltaUntouched(t *testing.T) {
	body := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":null},"usage":{"input_tokens":11,"output_tokens":1}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"kiro_context_usage":{"input_tokens":1000,"context_window":1000000,"percentage":0.1,"exceeded":false}}}` + "\n\n")

	patched := patchClaudeCodeUsageTotals(body)
	events := decodeResponseContractSSE(t, string(patched))
	var intermediate, terminal map[string]interface{}
	for _, event := range events {
		if event.Name != "message_delta" {
			continue
		}
		usage := responseContractMap(t, event.Data["usage"], "message_delta.usage")
		if intermediate == nil {
			intermediate = usage
		} else {
			terminal = usage
		}
	}
	if intermediate == nil || terminal == nil {
		t.Fatalf("missing deltas: intermediate=%#v terminal=%#v body=%s", intermediate, terminal, patched)
	}
	if got := int(intermediate["input_tokens"].(float64)); got != 11 {
		t.Fatalf("intermediate input=%d, want unchanged 11", got)
	}
	if got := int(terminal["input_tokens"].(float64)); got != 993 {
		t.Fatalf("terminal input=%d, want 993", got)
	}
}

func TestPatchClaudeCodeUsageTotalsLeavesMalformedOrContextlessBodyUntouched(t *testing.T) {
	for name, body := range map[string][]byte{
		"malformed":   []byte("event: message_delta\n" + `data: {"type":"message_delta","usage":` + "\n\n"),
		"contextless": []byte("event: message_delta\n" + `data: {"type":"message_delta","usage":{"input_tokens":12,"output_tokens":2}}` + "\n\n"),
	} {
		t.Run(name, func(t *testing.T) {
			got := patchClaudeCodeUsageTotals(body)
			if !bytes.Equal(got, body) {
				t.Fatalf("body changed: got=%q want=%q", got, body)
			}
		})
	}
}
