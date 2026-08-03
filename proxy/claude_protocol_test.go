package proxy

import (
	"bytes"
	"compress/zlib"
	"encoding/ascii85"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeThinkingSignature(t *testing.T) {
	if got := claudeThinkingSignature(" real-signature "); got != "real-signature" {
		t.Fatalf("real signature = %q", got)
	}
	if got := claudeThinkingSignature(""); got != claudeThinkingSignaturePlaceholder {
		t.Fatalf("placeholder signature = %q", got)
	}
	if _, err := base64.StdEncoding.DecodeString(claudeThinkingSignaturePlaceholder); err == nil {
		t.Fatal("placeholder must not masquerade as a replayable Anthropic signature")
	}
}

func TestResolveClaudeThinkingSignaturePrefersNative(t *testing.T) {
	native := ClaudeReasoningResult{Text: "reasoning", Signature: " native-signature "}
	if got := resolveClaudeThinkingSignature(native, false); got != "native-signature" {
		t.Fatalf("native signature = %q", got)
	}
	fallback := resolveClaudeThinkingSignature(ClaudeReasoningResult{Text: "reasoning"}, false)
	if !strings.HasPrefix(fallback, "kiro-go-unverified.") || len(fallback) < 50 {
		t.Fatalf("fallback signature = %q", fallback)
	}
}

func TestNormalizeClaudeToolUseID(t *testing.T) {
	validToolID := "toolu_01JgjyB9BRR1X4FcMGMTb3v3"
	validServerID := "srvtoolu_01FtQZXasVcFZsdrrrU1eC9Q"
	for _, id := range []string{validToolID, validServerID} {
		if got := normalizeClaudeToolUseID(id); got != id {
			t.Fatalf("normalizeClaudeToolUseID(%q) = %q", id, got)
		}
	}
	for _, upstreamID := range []string{"", "toolu_native", "srvtoolu_1", "call_legacy", "toolu_bdrk_provider"} {
		got := normalizeClaudeToolUseID(upstreamID)
		if !validClaudeOpaqueID(got, "toolu_") || got == upstreamID {
			t.Fatalf("normalizeClaudeToolUseID(%q) = %q", upstreamID, got)
		}
	}
}

func TestClaudeOpaqueIDsMatchProtocolShape(t *testing.T) {
	if id := newClaudeMessageID(); !strings.HasPrefix(id, "msg_"+claudeMessageIDVersion) || len(id) != len("msg_")+claudeOpaqueIDSuffixLength {
		t.Fatalf("generated message id = %q", id)
	}
	if id := newClaudeContainerID(); !strings.HasPrefix(id, "container_"+claudeMessageIDVersion) || len(id) != len("container_")+claudeOpaqueIDSuffixLength {
		t.Fatalf("generated container id = %q", id)
	}
	if id := newClaudeServerToolUseID(); !validClaudeOpaqueID(id, "srvtoolu_") {
		t.Fatalf("generated server tool id = %q", id)
	}
}

func TestKiroToClaudeResponseNormalizesToolUseID(t *testing.T) {
	resp := KiroToClaudeResponse("", "", false, []KiroToolUse{{
		ToolUseID: "call_legacy",
		Name:      "get_weather",
		Input:     map[string]interface{}{"city": "Tokyo", "unit": "celsius"},
	}}, 10, 10, "claude-sonnet-4-6")
	if len(resp.Content) != 1 || !validClaudeOpaqueID(resp.Content[0].ID, "toolu_") || resp.Content[0].ID == "call_legacy" || resp.StopReason != "tool_use" {
		t.Fatalf("response tool contract = %#v, stop_reason=%q", resp.Content, resp.StopReason)
	}
}

func TestExtractClaudeDocumentText(t *testing.T) {
	raw := []byte("%PDF-1.7\nBT (MAGIC-7F3K-VERIFY-CLAUDE) Tj ET\n%%EOF")
	encoded := base64.StdEncoding.EncodeToString(raw)
	text := extractClaudeDocumentText(map[string]interface{}{
		"type": "document",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": "application/pdf",
			"data":       encoded,
		},
	})
	if !strings.Contains(text, "MAGIC-7F3K-VERIFY-CLAUDE") {
		t.Fatalf("document magic string missing: %q", text)
	}
}

func TestExtractClaudeDocumentTextFromReportLabStyleStream(t *testing.T) {
	const magic = "MAGIC-7F3K-VERIFY-CLAUDE-RELAY-DETECTOR"
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write([]byte("BT /F1 14 Tf (Magic identifier: " + magic + ") Tj ET"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	encoded85 := make([]byte, ascii85.MaxEncodedLen(compressed.Len()))
	n := ascii85.Encode(encoded85, compressed.Bytes())
	pdf := append([]byte("%PDF-1.4\n<< /Filter [ /ASCII85Decode /FlateDecode ] >>\nstream\n"), encoded85[:n]...)
	pdf = append(pdf, []byte("~>\nendstream\n%%EOF")...)

	text := extractClaudeDocumentText(map[string]interface{}{
		"source": map[string]interface{}{
			"media_type": "application/pdf",
			"data":       base64.StdEncoding.EncodeToString(pdf),
		},
	})
	if !strings.Contains(text, magic) {
		t.Fatalf("compressed document magic string missing: %q", text)
	}
}

func TestResolveClaudeStopReason(t *testing.T) {
	tests := []struct {
		name            string
		toolUseCount    int
		contextExceeded bool
		upstream        string
		want            string
	}{
		{name: "end turn", want: "end_turn"},
		{name: "max tokens", upstream: "max_tokens", want: "max_tokens"},
		{name: "context window", contextExceeded: true, upstream: "max_tokens", want: "model_context_window_exceeded"},
		{name: "upstream stop overrides tool", toolUseCount: 1, upstream: "max_tokens", want: "max_tokens"},
		{name: "tool use", toolUseCount: 1, want: "tool_use"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveClaudeStopReason(tc.toolUseCount, tc.contextExceeded, tc.upstream)
			if got != tc.want {
				t.Fatalf("stop reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClaudeOutputTokenEstimateCountsRedactedThinking(t *testing.T) {
	base := estimateClaudeOutputTokensWithRedacted("answer", "", nil, nil)
	withRedacted := estimateClaudeOutputTokensWithRedacted("answer", "", []string{"encrypted reasoning payload"}, nil)
	if withRedacted <= base {
		t.Fatalf("redacted thinking was not counted: base=%d with=%d", base, withRedacted)
	}
}

func TestClaudeOpaqueIDsUseNativeVersionedShape(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		id     string
	}{
		{prefix: "toolu_", id: normalizeClaudeToolUseID("legacy-call")},
		{prefix: "srvtoolu_", id: newClaudeServerToolUseID()},
	} {
		if !validClaudeOpaqueID(tc.id, tc.prefix) {
			t.Fatalf("ID %q does not match native %s shape", tc.id, tc.prefix)
		}
		if !strings.HasPrefix(strings.TrimPrefix(tc.id, tc.prefix), claudeOpaqueIDVersion) {
			t.Fatalf("ID %q is missing native version prefix %q", tc.id, claudeOpaqueIDVersion)
		}
	}
	if id := newClaudeMessageID(); !strings.HasPrefix(id, "msg_"+claudeMessageIDVersion) {
		t.Fatalf("message ID %q is missing native version prefix %q", id, claudeMessageIDVersion)
	}
	if validClaudeOpaqueID("msg_zz1234567890123456789012", "msg_") {
		t.Fatal("accepted an unversioned message ID")
	}
}

func TestClaudeStreamTagThinkingOnlyDoesNotInjectFallbackText(t *testing.T) {
	h := newResponseContractHandler(t, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "<thinking>private reasoning</thinking>",
	}))
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "test",
		ModelID: "claude-sonnet-4.6",
		Origin:  "AI_EDITOR",
	}
	rec := httptest.NewRecorder()

	h.handleClaudeStream(
		rec,
		payload,
		"claude-sonnet-4.6",
		"claude-sonnet-4.6",
		true,
		claudeThinkingResponseOptions{Format: "thinking"},
		1,
		nil,
		"",
	)

	events := decodeResponseContractSSE(t, rec.Body.String())
	var visibleText, stopReason string
	for _, event := range events {
		switch event.Name {
		case "content_block_delta":
			delta, _ := event.Data["delta"].(map[string]interface{})
			if delta["type"] == "text_delta" {
				visibleText += delta["text"].(string)
			}
		case "message_delta":
			delta, _ := event.Data["delta"].(map[string]interface{})
			stopReason, _ = delta["stop_reason"].(string)
		}
	}
	if visibleText != "" || stopReason != "max_tokens" {
		t.Fatalf("visible text=%q stop_reason=%q\n%s", visibleText, stopReason, rec.Body.String())
	}
}
