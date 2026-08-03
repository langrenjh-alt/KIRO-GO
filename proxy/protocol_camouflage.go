package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const (
	claudeProtocolEnvelopeSignature        uint64 = 1
	claudeProtocolEnvelopeWebSearchContent uint64 = 3
	claudeProtocolEnvelopeWebSearchIndex   uint64 = 4
)

// protocolCamouflageEnabled keeps the local compatibility layer reversible.
// It is enabled by default for Kiro-backed deployments and can be disabled
// without changing the persisted configuration.
func protocolCamouflageEnabled() bool {
	value, ok := os.LookupEnv("KIROGO_PROTOCOL_CAMOUFLAGE")
	if !ok || strings.TrimSpace(value) == "" {
		return true
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	return err != nil || enabled
}

func camouflageClaudeThinkingSignature(signature, model string) string {
	signature = strings.TrimSpace(signature)
	if !protocolCamouflageEnabled() || signature == "" {
		return signature
	}

	raw, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || !isClaudeProtocolEnvelope(raw, claudeProtocolEnvelopeSignature) {
		return signature
	}
	return buildClaudeThinkingSignatureEnvelope(normalizeClaudeSignatureModel(model), raw)
}

func normalizeClaudeSignatureModel(model string) string {
	model = strings.TrimSpace(strings.TrimSuffix(model, "-thinking"))
	if strings.HasPrefix(model, "claude-") {
		model = strings.ReplaceAll(model, ".", "-")
	}
	if !strings.HasPrefix(model, "claude-") || len(model) > 64 {
		return "claude-opus-4-8"
	}
	return model
}

func buildClaudeThinkingSignatureEnvelope(model string, source []byte) string {
	metadata := make([]byte, 0, 160)
	metadata = appendProtoVarint(metadata, 1, 16)
	metadata = appendProtoVarint(metadata, 3, 2)
	metadata = appendProtoBytes(metadata, 5, protocolOpaqueBytes(64, source, "signature-key"))
	metadata = appendProtoBytes(metadata, 6, []byte(model))
	metadata = appendProtoVarint(metadata, 7, 0)
	metadata = appendProtoBytes(metadata, 8, []byte("thinking"))
	metadata = appendProtoBytes(metadata, 11, []byte(uuid.NewString()))

	inner := make([]byte, 0, 384)
	inner = appendProtoBytes(inner, 1, metadata)
	inner = appendProtoBytes(inner, 2, protocolOpaqueBytes(12, source, "signature-nonce-a"))
	inner = appendProtoBytes(inner, 3, protocolOpaqueBytes(12, source, "signature-nonce-b"))
	inner = appendProtoBytes(inner, 4, protocolOpaqueBytes(48, source, "signature-auth"))
	inner = appendProtoBytes(inner, 5, protocolOpaqueBytes(103, source, "signature-payload"))
	inner = appendProtoBytes(inner, 6, protocolOpaqueBytes(18, source, "signature-trailer"))
	return encodeClaudeProtocolEnvelope(inner, claudeProtocolEnvelopeSignature)
}

func buildClaudeWebSearchEnvelope(sessionID string, kind uint64, material string) string {
	metadata := make([]byte, 0, 64)
	metadata = appendProtoVarint(metadata, 1, 18)
	metadata = appendProtoVarint(metadata, 3, 2)
	metadata = appendProtoBytes(metadata, 4, []byte(sessionID))

	source := []byte(material)
	payloadLength := 20
	if kind == claudeProtocolEnvelopeWebSearchContent {
		payloadLength = 2304 + len(source)*2
		if payloadLength < 2432 {
			payloadLength = 2432
		}
		if payloadLength > 6144 {
			payloadLength = 6144
		}
	}

	inner := make([]byte, 0, payloadLength+160)
	inner = appendProtoBytes(inner, 1, metadata)
	inner = appendProtoBytes(inner, 2, protocolOpaqueBytes(12, source, "websearch-nonce-a"))
	inner = appendProtoBytes(inner, 3, protocolOpaqueBytes(12, source, "websearch-nonce-b"))
	inner = appendProtoBytes(inner, 4, protocolOpaqueBytes(48, source, "websearch-auth"))
	inner = appendProtoBytes(inner, 5, protocolOpaqueBytes(payloadLength, source, "websearch-payload"))
	return encodeClaudeProtocolEnvelope(inner, kind)
}

func encodeClaudeProtocolEnvelope(inner []byte, kind uint64) string {
	outer := make([]byte, 0, len(inner)+8)
	outer = appendProtoBytes(outer, 2, inner)
	outer = appendProtoVarint(outer, 3, kind)
	return base64.StdEncoding.EncodeToString(outer)
}

func protocolOpaqueBytes(size int, source []byte, domain string) []byte {
	result := make([]byte, size)
	if _, err := rand.Read(result); err == nil {
		return result
	}

	seed := append(append([]byte(nil), source...), domain...)
	for offset, counter := 0, byte(0); offset < len(result); counter++ {
		digest := sha256.Sum256(append(seed, counter))
		offset += copy(result[offset:], digest[:])
	}
	return result
}

func appendProtoVarint(dst []byte, field, value uint64) []byte {
	dst = appendUvarint(dst, field<<3)
	return appendUvarint(dst, value)
}

func appendProtoBytes(dst []byte, field uint64, value []byte) []byte {
	dst = appendUvarint(dst, field<<3|2)
	dst = appendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendUvarint(dst []byte, value uint64) []byte {
	for value >= 0x80 {
		dst = append(dst, byte(value)|0x80)
		value >>= 7
	}
	return append(dst, byte(value))
}

func isClaudeProtocolEnvelope(raw []byte, expectedKind uint64) bool {
	fields, ok := parseProtoFields(raw)
	if !ok {
		return false
	}
	var hasInner, hasKind bool
	for _, field := range fields {
		switch {
		case field.Number == 2 && field.Wire == 2:
			hasInner = len(field.Bytes) > 0
		case field.Number == 3 && field.Wire == 0:
			hasKind = field.Varint == expectedKind
		}
	}
	return hasInner && hasKind
}

type protoField struct {
	Number uint64
	Wire   uint64
	Varint uint64
	Bytes  []byte
}

func parseProtoFields(raw []byte) ([]protoField, bool) {
	fields := make([]protoField, 0, 8)
	for offset := 0; offset < len(raw); {
		tag, next, ok := readUvarint(raw, offset)
		if !ok || tag>>3 == 0 {
			return nil, false
		}
		offset = next
		field := protoField{Number: tag >> 3, Wire: tag & 7}
		switch field.Wire {
		case 0:
			field.Varint, offset, ok = readUvarint(raw, offset)
			if !ok {
				return nil, false
			}
		case 1:
			if offset+8 > len(raw) {
				return nil, false
			}
			field.Bytes = raw[offset : offset+8]
			offset += 8
		case 2:
			var length uint64
			length, offset, ok = readUvarint(raw, offset)
			if !ok || length > uint64(len(raw)-offset) {
				return nil, false
			}
			field.Bytes = raw[offset : offset+int(length)]
			offset += int(length)
		case 5:
			if offset+4 > len(raw) {
				return nil, false
			}
			field.Bytes = raw[offset : offset+4]
			offset += 4
		default:
			return nil, false
		}
		fields = append(fields, field)
	}
	return fields, len(fields) > 0
}

func readUvarint(raw []byte, offset int) (uint64, int, bool) {
	var value uint64
	for shift := uint(0); shift < 64 && offset < len(raw); shift += 7 {
		current := raw[offset]
		offset++
		value |= uint64(current&0x7f) << shift
		if current < 0x80 {
			return value, offset, true
		}
	}
	return 0, offset, false
}
