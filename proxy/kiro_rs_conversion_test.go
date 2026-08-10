package proxy

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestKiroRSSchemaNormalizationContract(t *testing.T) {
	for name, input := range map[string]interface{}{
		"non-object": "invalid",
		"invalid fields": map[string]interface{}{
			"type":                 nil,
			"properties":           nil,
			"required":             []interface{}{"first", 7, "second", false},
			"additionalProperties": "invalid",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := ensureObjectSchema(input).(map[string]interface{})
			if got["type"] != "object" || !reflect.DeepEqual(got["properties"], map[string]interface{}{}) {
				t.Fatalf("object defaults=%#v", got)
			}
			if got["additionalProperties"] != true {
				t.Fatalf("additionalProperties=%#v, want true", got["additionalProperties"])
			}
			wantRequired := []interface{}{}
			if name == "invalid fields" {
				wantRequired = []interface{}{"first", "second"}
			}
			if !reflect.DeepEqual(got["required"], wantRequired) {
				t.Fatalf("required=%#v, want %#v", got["required"], wantRequired)
			}
		})
	}

	schemaValue := map[string]interface{}{"type": "string"}
	got := ensureObjectSchema(map[string]interface{}{
		"additionalProperties": schemaValue,
	}).(map[string]interface{})
	if !reflect.DeepEqual(got["additionalProperties"], schemaValue) {
		t.Fatalf("schema-valued additionalProperties was changed: %#v", got)
	}
}

func TestKiroRSTextBlocksUseNewlineSeparators(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "first"},
		map[string]interface{}{"type": "text", "text": "second"},
	}
	text, _, _ := extractClaudeUserContent(content)
	if text != "first\nsecond" {
		t.Fatalf("user text blocks=%q, want newline-separated content", text)
	}
	toolText, _ := extractToolResultContent(content)
	if toolText != "first\nsecond" {
		t.Fatalf("tool-result text blocks=%q, want newline-separated content", toolText)
	}
}

func TestKiroRSShortenToolNameIsDeterministicAndCollisionResistant(t *testing.T) {
	nameA := "mcp__workspace_server_with_a_very_long_namespace__read_file_with_a_very_long_suffix_a"
	nameB := "mcp__workspace_server_with_a_very_long_namespace__read_file_with_a_very_long_suffix_b"
	shortA := shortenToolName(nameA)
	shortB := shortenToolName(nameB)
	if shortA != shortenToolName(nameA) {
		t.Fatalf("shortened tool name is not deterministic: %q vs %q", shortA, shortenToolName(nameA))
	}
	if shortA == shortB {
		t.Fatalf("distinct long tool names collided: %q", shortA)
	}
	if len([]byte(shortA)) > 63 || len([]byte(shortB)) > 63 {
		t.Fatalf("shortened name exceeds Kiro limit: %d, %d", len([]byte(shortA)), len([]byte(shortB)))
	}
	if !strings.Contains(shortA, "_") || !strings.Contains(shortB, "_") {
		t.Fatalf("shortened names lost digest separator: %q %q", shortA, shortB)
	}
}

func TestKiroRSShortenToolNamePreservesUTF8(t *testing.T) {
	name := strings.Repeat("工具", 40) + "_read"
	short := shortenToolName(name)
	if !utf8.ValidString(short) {
		t.Fatalf("shortened tool name is invalid UTF-8: %q", short)
	}
	if len([]byte(short)) > 63 {
		t.Fatalf("shortened Unicode name exceeds Kiro limit: %d", len([]byte(short)))
	}
}
