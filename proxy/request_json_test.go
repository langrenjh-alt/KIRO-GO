package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeRequestJSONWithLimit(t *testing.T) {
	type payload struct {
		Value string `json:"value"`
	}

	t.Run("valid", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("  {\"value\":\"ok\"}\n"))
		var got payload
		if err := decodeRequestJSONWithLimit(httptest.NewRecorder(), req, 128, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Value != "ok" {
			t.Fatalf("value = %q", got.Value)
		}
	})

	t.Run("multiple values", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{} {}"))
		var got payload
		if err := decodeRequestJSONWithLimit(httptest.NewRecorder(), req, 128, &got); !errors.Is(err, errMultipleRequestJSONValues) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("declared oversized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
		req.ContentLength = 129
		var got payload
		err := decodeRequestJSONWithLimit(httptest.NewRecorder(), req, 128, &got)
		var tooLarge *http.MaxBytesError
		if !errors.As(err, &tooLarge) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("streamed oversized", func(t *testing.T) {
		body := "{\"value\":\"" + strings.Repeat("x", 256) + "\"}"
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.ContentLength = -1
		var got payload
		err := decodeRequestJSONWithLimit(httptest.NewRecorder(), req, 128, &got)
		var tooLarge *http.MaxBytesError
		if !errors.As(err, &tooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestJSONEndpointsRejectDeclaredOversizedBodies(t *testing.T) {
	h := &Handler{}
	tests := []struct {
		name   string
		path   string
		handle func(http.ResponseWriter, *http.Request)
	}{
		{name: "claude messages", path: "/v1/messages", handle: h.handleClaudeMessagesInternal},
		{name: "claude count tokens", path: "/v1/messages/count_tokens", handle: h.handleCountTokens},
		{name: "openai chat", path: "/v1/chat/completions", handle: h.handleOpenAIChat},
		{name: "openai responses", path: "/v1/responses", handle: h.handleOpenAIResponses},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}"))
			req.ContentLength = claudeMaxRequestBodyBytes + 1
			recorder := httptest.NewRecorder()
			tc.handle(recorder, req)
			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
