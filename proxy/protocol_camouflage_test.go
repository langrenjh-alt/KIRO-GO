package proxy

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCamouflageClaudeThinkingSignatureRebuildsModelMetadata(t *testing.T) {
	source := buildClaudeThinkingSignatureEnvelope("claude-quince", []byte("fixture"))
	got := camouflageClaudeThinkingSignature(source, "claude-opus-4-8")
	if got == source {
		t.Fatal("signature envelope was not rebuilt")
	}

	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if !isClaudeProtocolEnvelope(raw, claudeProtocolEnvelopeSignature) {
		t.Fatal("signature is not a purpose=1 protocol envelope")
	}
	metadata := mustProtocolEnvelopeMetadata(t, raw)
	if model := protoBytesField(metadata, 6); string(model) != "claude-opus-4-8" {
		t.Fatalf("signature model = %q", model)
	}
	if purpose := protoBytesField(metadata, 8); string(purpose) != "thinking" {
		t.Fatalf("signature purpose = %q", purpose)
	}
	requestID := string(protoBytesField(metadata, 11))
	if _, err := uuid.Parse(requestID); err != nil {
		t.Fatalf("signature request id = %q: %v", requestID, err)
	}
}

func TestCamouflageClaudeThinkingSignaturePreservesUnknownValues(t *testing.T) {
	for _, value := range []string{"native-signature", base64.StdEncoding.EncodeToString([]byte("not protobuf"))} {
		if got := camouflageClaudeThinkingSignature(value, "claude-opus-4-8"); got != value {
			t.Fatalf("unknown signature %q changed to %q", value, got)
		}
	}
}

func TestClaudeWebSearchEnvelopesShareSessionAndUseDistinctPurposes(t *testing.T) {
	sessionID := uuid.NewString()
	content := buildClaudeWebSearchEnvelope(sessionID, claudeProtocolEnvelopeWebSearchContent, "url\x00title\x00snippet")
	index := buildClaudeWebSearchEnvelope(sessionID, claudeProtocolEnvelopeWebSearchIndex, "url\x00title")

	contentRaw := decodeProtocolEnvelope(t, content, claudeProtocolEnvelopeWebSearchContent)
	indexRaw := decodeProtocolEnvelope(t, index, claudeProtocolEnvelopeWebSearchIndex)
	for name, raw := range map[string][]byte{"content": contentRaw, "index": indexRaw} {
		metadata := mustProtocolEnvelopeMetadata(t, raw)
		if got := string(protoBytesField(metadata, 4)); got != sessionID {
			t.Fatalf("%s session id = %q, want %q", name, got, sessionID)
		}
	}

	contentPayload := protoBytesField(protoBytesField(contentRaw, 2), 5)
	indexPayload := protoBytesField(protoBytesField(indexRaw, 2), 5)
	if len(contentPayload) < 2400 || len(indexPayload) != 20 {
		t.Fatalf("payload lengths content=%d index=%d", len(contentPayload), len(indexPayload))
	}
	if len(content) < 3000 || len(index) < 180 {
		t.Fatalf("base64 envelope lengths content=%d index=%d", len(content), len(index))
	}
}

func TestBuildWebSearchContentUsesSharedProtocolMetadata(t *testing.T) {
	content, _, _ := buildWebSearchContent(testWebSearchResultSet())
	toolResult := content[1].(map[string]interface{})
	result := toolResult["content"].([]map[string]interface{})[0]
	text := content[2].(map[string]interface{})
	citation := text["citations"].([]map[string]interface{})[0]

	contentRaw := decodeProtocolEnvelope(t, result["encrypted_content"].(string), claudeProtocolEnvelopeWebSearchContent)
	indexRaw := decodeProtocolEnvelope(t, citation["encrypted_index"].(string), claudeProtocolEnvelopeWebSearchIndex)
	contentSession := string(protoBytesField(mustProtocolEnvelopeMetadata(t, contentRaw), 4))
	indexSession := string(protoBytesField(mustProtocolEnvelopeMetadata(t, indexRaw), 4))
	if contentSession == "" || contentSession != indexSession {
		t.Fatalf("web search session ids content=%q index=%q", contentSession, indexSession)
	}
	if strings.Contains(result["encrypted_content"].(string), *testWebSearchResultSet().Results[0].Snippet) {
		t.Fatal("encrypted_content exposed the source snippet")
	}
}

func decodeProtocolEnvelope(t *testing.T, encoded string, purpose uint64) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode protocol envelope: %v", err)
	}
	if !isClaudeProtocolEnvelope(raw, purpose) {
		t.Fatalf("not a purpose=%d protocol envelope", purpose)
	}
	return raw
}

func mustProtocolEnvelopeMetadata(t *testing.T, raw []byte) []byte {
	t.Helper()
	inner := protoBytesField(raw, 2)
	metadata := protoBytesField(inner, 1)
	if len(metadata) == 0 {
		t.Fatal("protocol envelope metadata is missing")
	}
	return metadata
}

func protoBytesField(raw []byte, number uint64) []byte {
	fields, ok := parseProtoFields(raw)
	if !ok {
		return nil
	}
	for _, field := range fields {
		if field.Number == number && field.Wire == 2 {
			return field.Bytes
		}
	}
	return nil
}
