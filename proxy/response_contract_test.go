package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func newResponseContractHandler(t *testing.T, frames ...[]byte) *Handler {
	t.Helper()
	h, cleanup := setupResponsesTestHandler(t)
	t.Cleanup(cleanup)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, frame := range frames {
			if _, err := w.Write(frame); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(swapKiroEndpointsForTest(t, server))
	return h
}

func responseContractLengthFrame(t *testing.T) []byte {
	t.Helper()
	return awsEventStreamFrameWithHeaders(t, map[string]string{
		":message-type":   "exception",
		":exception-type": "ContentLengthExceededException",
	}, map[string]interface{}{"message": "maximum output reached"})
}

func TestResponseContractClaudeModelEcho(t *testing.T) {
	const requestedModel = "claude-sonnet-4-6"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h := newResponseContractHandler(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
				"content": "model echo",
			}))
			body := fmt.Sprintf(`{"model":%q,"max_tokens":64,"stream":%v,"messages":[{"role":"user","content":"test"}]}`, requestedModel, stream)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			rec := httptest.NewRecorder()

			h.handleClaudeMessagesInternal(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if !stream {
				var response ClaudeResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if response.Model != requestedModel {
					t.Fatalf("model = %q, want requested model %q", response.Model, requestedModel)
				}
				return
			}

			events := decodeResponseContractSSE(t, rec.Body.String())
			if len(events) == 0 || events[0].Name != "message_start" {
				t.Fatalf("first event = %#v", events)
			}
			message := responseContractMap(t, events[0].Data["message"], "message_start.message")
			if message["model"] != requestedModel {
				t.Fatalf("stream model = %#v, want %q", message["model"], requestedModel)
			}
			if _, exists := message["context_management"]; exists {
				t.Fatalf("message_start must not contain terminal context_management: %#v", message)
			}
		})
	}
}

func TestResponseContractClaudeKeepAlivePrecedesDelayedContent(t *testing.T) {
	h := newResponseContractHandler(t)
	h.kiroCaller = func(_ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
		callback.OnKeepAlive()
		callback.OnText("continued", false)
		callback.OnComplete(4, 2)
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-6","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"test"}]}`,
	))
	rec := httptest.NewRecorder()
	h.handleClaudeMessagesInternal(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	events := decodeResponseContractSSE(t, rec.Body.String())
	if len(events) < 4 || events[0].Name != "message_start" || events[1].Name != "ping" {
		t.Fatalf("keepalive prelude=%#v body=%s", events, rec.Body.String())
	}
	pings := 0
	var text strings.Builder
	for _, event := range events {
		if event.Name == "ping" {
			pings++
		}
		if event.Name != "content_block_delta" {
			continue
		}
		delta := responseContractMap(t, event.Data["delta"], "content_block_delta.delta")
		if delta["type"] == "text_delta" {
			text.WriteString(fmt.Sprint(delta["text"]))
		}
	}
	if pings != 1 || text.String() != "continued" || events[len(events)-1].Name != "message_stop" {
		t.Fatalf("pings=%d text=%q terminal=%q body=%s", pings, text.String(), events[len(events)-1].Name, rec.Body.String())
	}
}

func TestResponseContractRequiredToolChoiceSuppressesNarrationAndExtraCalls(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h := newResponseContractHandler(t,
				awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
					"content": "I'll call the available tools now.",
				}),
				awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
					"toolUseId": "toolu_bdrk_first", "name": "first_tool", "input": `{"value":1}`, "stop": true,
				}),
				awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
					"toolUseId": "toolu_bdrk_second", "name": "second_tool", "input": `{"value":2}`, "stop": true,
				}),
			)
			body := fmt.Sprintf(`{
				"model":"claude-opus-4-8",
				"max_tokens":128,
				"stream":%v,
				"messages":[{"role":"user","content":"Call one tool."}],
				"tools":[
					{"name":"first_tool","description":"first","input_schema":{"type":"object"}},
					{"name":"second_tool","description":"second","input_schema":{"type":"object"}}
				],
				"tool_choice":{"type":"any"}
			}`, stream)
			rec := httptest.NewRecorder()
			h.handleClaudeMessagesInternal(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}

			if !stream {
				var response ClaudeResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Content) != 1 || response.Content[0].Type != "tool_use" || response.Content[0].Name != "first_tool" {
					t.Fatalf("content=%#v", response.Content)
				}
				if !validClaudeOpaqueID(response.Content[0].ID, "toolu_") || response.StopReason != "tool_use" {
					t.Fatalf("tool id=%q stop=%q", response.Content[0].ID, response.StopReason)
				}
				return
			}

			events := decodeResponseContractSSE(t, rec.Body.String())
			var toolBlocks int
			for _, event := range events {
				if event.Name != "content_block_start" {
					continue
				}
				block := responseContractMap(t, event.Data["content_block"], "content_block")
				if block["type"] == "text" {
					t.Fatalf("tool turn leaked narration: %s", rec.Body.String())
				}
				if block["type"] == "tool_use" {
					toolBlocks++
					if block["name"] != "first_tool" || !validClaudeOpaqueID(fmt.Sprint(block["id"]), "toolu_") {
						t.Fatalf("tool block=%#v", block)
					}
				}
			}
			if toolBlocks != 1 {
				t.Fatalf("tool blocks=%d body=%s", toolBlocks, rec.Body.String())
			}
		})
	}
}

func TestResponseContractChatModelEchoAndLengthTermination(t *testing.T) {
	const requestedModel = "claude-sonnet-4-6"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h := newResponseContractHandler(t,
				awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial"}),
				responseContractLengthFrame(t),
			)
			body := fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"test"}]}`, requestedModel, stream)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rec := httptest.NewRecorder()

			h.handleOpenAIChat(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if !stream {
				var response map[string]interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if response["model"] != requestedModel {
					t.Fatalf("model = %#v, want %q", response["model"], requestedModel)
				}
				choices := responseContractSlice(t, response["choices"], "choices")
				choice := responseContractMap(t, choices[0], "choices[0]")
				if choice["finish_reason"] != "length" {
					t.Fatalf("finish_reason = %#v, want length", choice["finish_reason"])
				}
				return
			}

			chunks, done := decodeOpenAIDataSSE(t, rec.Body.String())
			if !done {
				t.Fatalf("stream is missing [DONE]:\n%s", rec.Body.String())
			}
			var terminal map[string]interface{}
			for i, chunk := range chunks {
				if chunk["model"] != requestedModel {
					t.Fatalf("chunk %d model = %#v, want %q", i, chunk["model"], requestedModel)
				}
				choices := responseContractSlice(t, chunk["choices"], fmt.Sprintf("chunk[%d].choices", i))
				choice := responseContractMap(t, choices[0], fmt.Sprintf("chunk[%d].choices[0]", i))
				if choice["finish_reason"] != nil {
					terminal = choice
				}
			}
			if terminal == nil || terminal["finish_reason"] != "length" {
				t.Fatalf("terminal choice = %#v, want finish_reason=length", terminal)
			}
		})
	}
}

func TestResponseContractResponsesModelEchoAndLengthTermination(t *testing.T) {
	const requestedModel = "claude-sonnet-4-6"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h := newResponseContractHandler(t,
				awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial"}),
				responseContractLengthFrame(t),
			)
			body := fmt.Sprintf(`{"model":%q,"input":"test","stream":%v,"store":false}`, requestedModel, stream)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			rec := httptest.NewRecorder()

			h.handleOpenAIResponses(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if !stream {
				var response ResponsesObject
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if response.Model != requestedModel {
					t.Fatalf("model = %q, want requested model %q", response.Model, requestedModel)
				}
				if response.Status != "incomplete" || response.IncompleteDetails == nil || response.IncompleteDetails.Reason != "max_output_tokens" {
					t.Fatalf("terminal status = %q, details = %#v", response.Status, response.IncompleteDetails)
				}
				if len(response.Output) == 0 || response.Output[len(response.Output)-1].Status != "incomplete" {
					t.Fatalf("terminal output = %#v, want incomplete final item", response.Output)
				}
				return
			}

			events := decodeResponseContractSSE(t, rec.Body.String())
			seenEnvelope := map[string]bool{}
			seenIncompleteItem := false
			for _, event := range events {
				if event.Name == "response.output_item.done" {
					item := responseContractMap(t, event.Data["item"], "output_item.done.item")
					if item["status"] == "incomplete" {
						seenIncompleteItem = true
					}
				}
				if event.Name != "response.created" && event.Name != "response.in_progress" && event.Name != "response.incomplete" {
					continue
				}
				response := responseContractMap(t, event.Data["response"], event.Name+".response")
				if response["model"] != requestedModel {
					t.Fatalf("%s model = %#v, want %q", event.Name, response["model"], requestedModel)
				}
				seenEnvelope[event.Name] = true
				if event.Name == "response.incomplete" {
					if response["status"] != "incomplete" {
						t.Fatalf("terminal response status = %#v", response["status"])
					}
					details := responseContractMap(t, response["incomplete_details"], "incomplete_details")
					if details["reason"] != "max_output_tokens" {
						t.Fatalf("incomplete reason = %#v", details["reason"])
					}
				}
			}
			for _, name := range []string{"response.created", "response.in_progress", "response.incomplete"} {
				if !seenEnvelope[name] {
					t.Fatalf("missing %s event in stream:\n%s", name, rec.Body.String())
				}
			}
			if !seenIncompleteItem {
				t.Fatalf("stream is missing incomplete output item:\n%s", rec.Body.String())
			}
			for _, event := range events {
				if event.Name == "response.completed" {
					t.Fatalf("length-limited stream emitted response.completed: %s", rec.Body.String())
				}
			}
		})
	}
}

func TestResponseContractResponsesReasoningSSEOrderAndIDs(t *testing.T) {
	h := newResponseContractHandler(t,
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "first thought"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": " second thought"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "final answer"}),
	)
	body := `{"model":"claude-opus-4.8","input":"test","stream":true,"store":false,"reasoning":{"effort":"medium","summary":"auto"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := decodeResponseContractSSE(t, rec.Body.String())
	gotOrder := make([]string, len(events))
	for i, event := range events {
		sequence, ok := event.Data["sequence_number"].(float64)
		if !ok || int(sequence) != i {
			t.Fatalf("event %d (%s) sequence_number = %#v", i, event.Name, event.Data["sequence_number"])
		}
		gotOrder[i] = event.Name
	}
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("event order mismatch:\n got: %v\nwant: %v\nbody:\n%s", gotOrder, wantOrder, rec.Body.String())
	}

	reasoningAdded := responseContractMap(t, events[2].Data["item"], "reasoning item added")
	reasoningID, _ := reasoningAdded["id"].(string)
	if !strings.HasPrefix(reasoningID, "rs_") {
		t.Fatalf("reasoning item id = %q", reasoningID)
	}
	for _, index := range []int{3, 4, 5, 6, 7} {
		if events[index].Data["item_id"] != reasoningID {
			t.Fatalf("event %s item_id = %#v, want %q", events[index].Name, events[index].Data["item_id"], reasoningID)
		}
	}
	if events[4].Data["delta"] != "first thought" || events[5].Data["delta"] != " second thought" {
		t.Fatalf("reasoning deltas = %#v, %#v", events[4].Data["delta"], events[5].Data["delta"])
	}
	if events[6].Data["text"] != "first thought second thought" {
		t.Fatalf("reasoning done text = %#v", events[6].Data["text"])
	}
	reasoningDone := responseContractMap(t, events[8].Data["item"], "reasoning item done")
	if reasoningDone["id"] != reasoningID || reasoningDone["status"] != "completed" {
		t.Fatalf("reasoning done item = %#v", reasoningDone)
	}

	messageAdded := responseContractMap(t, events[9].Data["item"], "message item added")
	messageID, _ := messageAdded["id"].(string)
	if !strings.HasPrefix(messageID, "msg_") {
		t.Fatalf("message item id = %q", messageID)
	}
	for _, index := range []int{10, 11, 12, 13} {
		if events[index].Data["item_id"] != messageID {
			t.Fatalf("event %s item_id = %#v, want %q", events[index].Name, events[index].Data["item_id"], messageID)
		}
	}
	messageDone := responseContractMap(t, events[14].Data["item"], "message item done")
	if messageDone["id"] != messageID || messageDone["status"] != "completed" {
		t.Fatalf("message done item = %#v", messageDone)
	}

	terminal := responseContractMap(t, events[15].Data["response"], "completed response")
	if terminal["model"] != "claude-opus-4.8" || terminal["status"] != "completed" {
		t.Fatalf("terminal response = %#v", terminal)
	}
	output := responseContractSlice(t, terminal["output"], "completed response output")
	if len(output) != 2 {
		t.Fatalf("terminal output = %#v", output)
	}
	if responseContractMap(t, output[0], "output[0]")["id"] != reasoningID || responseContractMap(t, output[1], "output[1]")["id"] != messageID {
		t.Fatalf("terminal output IDs do not match streamed IDs: %#v", output)
	}
}

func TestResponseContractWebSearchModelEchoAndCitationSSE(t *testing.T) {
	const requestedModel = "claude-sonnet-4-6"
	result := testWebSearchResultSet()

	nonStreamReq := &ClaudeRequest{Model: "claude-sonnet-4.6"}
	nonStreamReq.responseModel = requestedModel
	nonStreamRec := httptest.NewRecorder()
	if _, err := writeWebSearchResponse(nonStreamRec, nonStreamReq, result, 17); err != nil {
		t.Fatalf("write non-stream response: %v", err)
	}
	var nonStream map[string]interface{}
	if err := json.Unmarshal(nonStreamRec.Body.Bytes(), &nonStream); err != nil {
		t.Fatalf("decode non-stream response: %v", err)
	}
	if nonStream["model"] != requestedModel {
		t.Fatalf("non-stream model = %#v, want %q", nonStream["model"], requestedModel)
	}

	streamReq := &ClaudeRequest{Model: "claude-sonnet-4.6", Stream: true}
	streamReq.responseModel = requestedModel
	streamRec := httptest.NewRecorder()
	if _, err := writeWebSearchResponse(streamRec, streamReq, result, 17); err != nil {
		t.Fatalf("write stream response: %v", err)
	}
	events := decodeWebSearchSSE(t, streamRec.Body.String())
	if len(events) == 0 || events[0].Name != "message_start" {
		t.Fatalf("first event = %#v", events)
	}
	message := responseContractMap(t, events[0].Data["message"], "message_start.message")
	if message["model"] != requestedModel {
		t.Fatalf("stream model = %#v, want %q", message["model"], requestedModel)
	}
	if _, exists := message["context_management"]; exists {
		t.Fatalf("web search message_start must not contain terminal context_management: %#v", message)
	}

	_, summary, _ := buildWebSearchContent(result)
	wantOrder := []string{
		"message_start",
		"content_block_start:0",
		"ping",
		"content_block_delta:0:input_json_delta",
		"content_block_delta:0:input_json_delta",
		"content_block_stop:0",
		"content_block_start:1",
		"content_block_stop:1",
		"content_block_start:2",
	}
	for range buildWebSearchCitations(result.Results) {
		wantOrder = append(wantOrder, "content_block_delta:2:citations_delta")
	}
	for range chunkWebSearchText(summary, webSearchSummaryChunkRunes) {
		wantOrder = append(wantOrder, "content_block_delta:2:text_delta")
	}
	wantOrder = append(wantOrder,
		"content_block_stop:2",
		"message_delta",
		"message_stop",
	)
	gotOrder := make([]string, len(events))
	for i, event := range events {
		gotOrder[i] = describeResponseContractWebSearchEvent(event)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("citation SSE order mismatch:\n got: %v\nwant: %v\nbody:\n%s", gotOrder, wantOrder, streamRec.Body.String())
	}

	var citation map[string]interface{}
	for _, event := range events {
		if describeResponseContractWebSearchEvent(event) != "content_block_delta:2:citations_delta" {
			continue
		}
		delta := responseContractMap(t, event.Data["delta"], "citation delta")
		citation = responseContractMap(t, delta["citation"], "citation")
		break
	}
	if citation == nil {
		t.Fatal("missing citation delta")
	}
	if citation["type"] != "web_search_result_location" || citation["url"] != result.Results[0].URL || citation["title"] != result.Results[0].Title {
		t.Fatalf("citation identity fields = %#v", citation)
	}
	if strings.TrimSpace(fmt.Sprint(citation["cited_text"])) == "" {
		t.Fatalf("citation cited_text = %#v", citation["cited_text"])
	}
	encryptedIndex := fmt.Sprint(citation["encrypted_index"])
	if encryptedIndex == "" || encryptedIndex == *result.Results[0].Snippet {
		t.Fatalf("citation encrypted_index must be opaque: %#v", citation)
	}
	terminal := events[len(events)-2].Data
	contextManagement := responseContractMap(t, terminal["context_management"], "message_delta.context_management")
	if _, ok := contextManagement["applied_edits"].([]interface{}); !ok {
		t.Fatalf("message_delta applied_edits missing: %#v", terminal)
	}
}

func decodeOpenAIDataSSE(t *testing.T, body string) ([]map[string]interface{}, bool) {
	t.Helper()
	body = strings.ReplaceAll(body, "\r\n", "\n")
	frames := strings.Split(strings.TrimSpace(body), "\n\n")
	chunks := make([]map[string]interface{}, 0, len(frames))
	done := false
	for _, frame := range frames {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				done = true
				continue
			}
			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				t.Fatalf("decode OpenAI SSE chunk: %v, data=%s", err, data)
			}
			chunks = append(chunks, chunk)
		}
	}
	return chunks, done
}

func decodeResponseContractSSE(t *testing.T, body string) []decodedWebSearchSSEEvent {
	t.Helper()
	body = strings.ReplaceAll(body, "\r\n", "\n")
	events := make([]decodedWebSearchSSEEvent, 0)
	for _, frame := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var event decodedWebSearchSSEEvent
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data := strings.TrimPrefix(line, "data: ")
				if err := json.Unmarshal([]byte(data), &event.Data); err != nil {
					t.Fatalf("decode Responses SSE data: %v, data=%s", err, data)
				}
			}
		}
		if event.Name != "" && event.Data != nil {
			events = append(events, event)
		}
	}
	return events
}

func responseContractMap(t *testing.T, value interface{}, label string) map[string]interface{} {
	t.Helper()
	result, ok := value.(map[string]interface{})
	if !ok {
		t.Fatalf("%s = %#v, want object", label, value)
	}
	return result
}

func responseContractSlice(t *testing.T, value interface{}, label string) []interface{} {
	t.Helper()
	result, ok := value.([]interface{})
	if !ok || len(result) == 0 {
		t.Fatalf("%s = %#v, want non-empty array", label, value)
	}
	return result
}

func describeResponseContractWebSearchEvent(event decodedWebSearchSSEEvent) string {
	description := event.Name
	index, hasIndex := event.Data["index"].(float64)
	if hasIndex {
		description += fmt.Sprintf(":%d", int(index))
	}
	if delta, ok := event.Data["delta"].(map[string]interface{}); ok {
		if deltaType, ok := delta["type"].(string); ok {
			description += ":" + deltaType
		}
	}
	return description
}
