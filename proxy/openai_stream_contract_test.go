package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newOpenAIChatContractHandler(t *testing.T, frames ...[]byte) *Handler {
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

func TestOpenAIChatStreamContract(t *testing.T) {
	for _, includeUsage := range []bool{false, true} {
		t.Run(fmt.Sprintf("include_usage=%v", includeUsage), func(t *testing.T) {
			h := newOpenAIChatContractHandler(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
				"content":      "streamed answer",
				"inputTokens":  17,
				"outputTokens": 4,
			}))
			body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"test"}],"stream":true,"stream_options":{"include_usage":%v}}`, includeUsage)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rec := httptest.NewRecorder()

			h.handleOpenAIChat(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			chunks := decodeOpenAIChatContractStream(t, rec.Body.String())
			wantChunks := 3
			if includeUsage {
				wantChunks++
			}
			if len(chunks) != wantChunks {
				t.Fatalf("chunk count = %d, want %d; body:\n%s", len(chunks), wantChunks, rec.Body.String())
			}

			id, _ := chunks[0]["id"].(string)
			if !strings.HasPrefix(id, "chatcmpl-") {
				t.Fatalf("id = %#v, want chatcmpl- prefix", chunks[0]["id"])
			}
			created := chunks[0]["created"]
			for i, chunk := range chunks {
				if chunk["id"] != id || chunk["created"] != created || chunk["model"] != "claude-sonnet-4-6" {
					t.Fatalf("chunk %d identity fields differ: %#v", i, chunk)
				}
				if chunk["object"] != "chat.completion.chunk" {
					t.Fatalf("chunk %d object = %#v", i, chunk["object"])
				}
			}

			firstChoice := openAIChatContractChoice(t, chunks[0], "first chunk")
			firstDelta := openAIChatContractMap(t, firstChoice["delta"], "first delta")
			if firstDelta["role"] != "assistant" || firstDelta["content"] != "" || firstChoice["finish_reason"] != nil {
				t.Fatalf("first choice = %#v, want assistant role chunk", firstChoice)
			}

			contentChoice := openAIChatContractChoice(t, chunks[1], "content chunk")
			contentDelta := openAIChatContractMap(t, contentChoice["delta"], "content delta")
			if contentDelta["content"] != "streamed answer" || contentChoice["finish_reason"] != nil {
				t.Fatalf("content choice = %#v", contentChoice)
			}

			finishIndex := len(chunks) - 1
			if includeUsage {
				finishIndex--
			}
			finishChoice := openAIChatContractChoice(t, chunks[finishIndex], "finish chunk")
			finishDelta := openAIChatContractMap(t, finishChoice["delta"], "finish delta")
			if finishDelta["content"] != "" || finishChoice["finish_reason"] != "stop" {
				t.Fatalf("finish choice = %#v, want empty content and stop", finishChoice)
			}
			for i := 0; i <= finishIndex; i++ {
				if _, ok := chunks[i]["usage"]; ok {
					t.Fatalf("chunk %d unexpectedly contains usage: %#v", i, chunks[i])
				}
			}

			if !includeUsage {
				return
			}
			usageChunk := chunks[len(chunks)-1]
			choices, ok := usageChunk["choices"].([]interface{})
			if !ok || len(choices) != 0 {
				t.Fatalf("usage choices = %#v, want empty array", usageChunk["choices"])
			}
			usage := openAIChatContractMap(t, usageChunk["usage"], "usage")
			prompt, promptOK := usage["prompt_tokens"].(float64)
			completion, completionOK := usage["completion_tokens"].(float64)
			total, totalOK := usage["total_tokens"].(float64)
			if !promptOK || !completionOK || !totalOK || total != prompt+completion {
				t.Fatalf("usage = %#v, want internally consistent token counts", usage)
			}
		})
	}
}

func TestOpenAIChatSuppressesReasoningWhenThinkingIsDisabled(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h := newOpenAIChatContractHandler(t,
				awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "private reasoning"}),
				awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "final answer"}),
			)
			body := fmt.Sprintf(`{"model":"claude-opus-5","messages":[{"role":"user","content":"test"}],"stream":%v}`, stream)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rec := httptest.NewRecorder()

			h.handleOpenAIChat(rec, req)

			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "final answer") {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "private reasoning") {
				t.Fatalf("reasoning leaked into non-thinking response: %s", rec.Body.String())
			}
		})
	}
}

func TestOpenAIChatStreamFlushesShortDeltaBeforeUpstreamCompletes(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	t.Cleanup(cleanup)

	firstFrameSent := make(chan struct{})
	releaseUpstream := make(chan struct{})
	released := false
	release := func() {
		if !released {
			close(releaseUpstream)
			released = true
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "first",
		}))
		w.(http.Flusher).Flush()
		close(firstFrameSent)
		<-releaseUpstream
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content":      " second",
			"inputTokens":  5,
			"outputTokens": 2,
		}))
	}))
	defer upstream.Close()
	defer release()
	defer swapKiroEndpointsForTest(t, upstream)()

	downstream := httptest.NewServer(http.HandlerFunc(h.handleOpenAIChat))
	defer downstream.Close()

	type responseResult struct {
		response *http.Response
		err      error
	}
	responseCh := make(chan responseResult, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, downstream.URL+"/v1/chat/completions", strings.NewReader(
			`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"test"}],"stream":true}`,
		))
		if err != nil {
			responseCh <- responseResult{err: err}
			return
		}
		resp, err := http.DefaultClient.Do(req)
		responseCh <- responseResult{response: resp, err: err}
	}()

	select {
	case <-firstFrameSent:
	case <-time.After(time.Second):
		t.Fatal("upstream did not send its first frame")
	}

	var result responseResult
	select {
	case result = <-responseCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("client received no response before the upstream stream completed")
	}
	if result.err != nil {
		t.Fatalf("stream request failed: %v", result.err)
	}
	defer result.response.Body.Close()
	if got := result.response.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}
	if got := result.response.Header.Get("Cache-Control"); !strings.Contains(got, "no-transform") {
		t.Fatalf("Cache-Control = %q, want no-transform", got)
	}

	reader := bufio.NewReader(result.response.Body)
	firstEvent, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first SSE event: %v", err)
	}
	if !strings.Contains(firstEvent, `"role":"assistant"`) {
		t.Fatalf("first SSE event = %q, want assistant role", firstEvent)
	}

	release()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remaining stream: %v", err)
	}
	if body := firstEvent + string(rest); !strings.Contains(body, `"content":"first"`) || !strings.Contains(body, `"content":" second"`) {
		t.Fatalf("stream did not preserve both deltas: %s", body)
	}
}

func TestOpenAIChatNonStreamContract(t *testing.T) {
	h := newOpenAIChatContractHandler(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content":      "non-streamed answer",
		"inputTokens":  17,
		"outputTokens": 4,
	}))
	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"test"}],"stream_options":{"include_usage":true}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.handleOpenAIChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	id, _ := response["id"].(string)
	if !strings.HasPrefix(id, "chatcmpl-") || response["object"] != "chat.completion" || response["model"] != "claude-sonnet-4-6" {
		t.Fatalf("response envelope = %#v", response)
	}
	choice := openAIChatContractChoice(t, response, "non-stream response")
	message := openAIChatContractMap(t, choice["message"], "non-stream message")
	if message["role"] != "assistant" || message["content"] != "non-streamed answer" || choice["finish_reason"] != "stop" {
		t.Fatalf("non-stream choice = %#v", choice)
	}
	usage := openAIChatContractMap(t, response["usage"], "non-stream usage")
	if _, ok := usage["total_tokens"]; !ok {
		t.Fatalf("non-stream usage = %#v", usage)
	}
}

func decodeOpenAIChatContractStream(t *testing.T, body string) []map[string]interface{} {
	t.Helper()
	frames := strings.Split(strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n")), "\n\n")
	chunks := make([]map[string]interface{}, 0, len(frames)-1)
	for i, frame := range frames {
		if frame == "data: [DONE]" {
			if i != len(frames)-1 {
				t.Fatalf("[DONE] is not the final frame: %q", body)
			}
			return chunks
		}
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("invalid SSE frame %d: %q", i, frame)
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &chunk); err != nil {
			t.Fatalf("decode chunk %d: %v", i, err)
		}
		chunks = append(chunks, chunk)
	}
	t.Fatalf("stream is missing final [DONE]: %q", body)
	return nil
}

func openAIChatContractChoice(t *testing.T, envelope map[string]interface{}, label string) map[string]interface{} {
	t.Helper()
	choices, ok := envelope["choices"].([]interface{})
	if !ok || len(choices) != 1 {
		t.Fatalf("%s choices = %#v, want one choice", label, envelope["choices"])
	}
	return openAIChatContractMap(t, choices[0], label+" choice")
}

func openAIChatContractMap(t *testing.T, value interface{}, label string) map[string]interface{} {
	t.Helper()
	result, ok := value.(map[string]interface{})
	if !ok {
		t.Fatalf("%s = %#v, want object", label, value)
	}
	return result
}
