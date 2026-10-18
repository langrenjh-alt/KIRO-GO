package config

import "testing"

func TestCanonicalModelIDFoldsVersionSpellings(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-8":          "claude-opus-4.8",
		"claude-opus-4.8":          "claude-opus-4.8",
		"claude-opus-4-8-20260101": "claude-opus-4.8",
		"Claude-Sonnet-4-6":        "claude-sonnet-4.6",
		"gpt-5-6-sol":              "gpt-5.6-sol",
		"gpt-5.6-sol":              "gpt-5.6-sol",
		"minimax-m2-5":             "minimax-m2.5",
		"claude-sonnet-4-20250514": "claude-sonnet-4",
		"claude-sonnet-4":          "claude-sonnet-4",
		"qwen3-coder-next":         "qwen3-coder-next",
		"Claude-opus-5.5":          "claude-opus-5.5",
		"claude-opus-5-5":          "claude-opus-5.5",
		"claude-sonnet-5.5":        "claude-sonnet-5.5",
	}
	for input, want := range cases {
		if got := CanonicalModelID(input); got != want {
			t.Errorf("CanonicalModelID(%q)=%q, want %q", input, got, want)
		}
	}
	if alt := AlternateModelID("claude-opus-4.8"); alt != "claude-opus-4-8" {
		t.Fatalf("alternate of dotted opus=%q", alt)
	}
	if alt := AlternateModelID("claude-opus-4-8"); alt != "claude-opus-4.8" {
		t.Fatalf("alternate of dashed opus=%q", alt)
	}
	if alt := AlternateModelID("claude-opus-5.5"); alt != "claude-opus-5-5" {
		t.Fatalf("alternate of opus 5.5=%q", alt)
	}
	if alt := AlternateModelID("claude-sonnet-4"); alt != "" {
		t.Fatalf("bare family produced an alternate: %q", alt)
	}
}

func TestPublicModelIDUsesHyphens(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5.5":          "claude-opus-5-5",
		"claude-opus-5-5":          "claude-opus-5-5",
		"Claude-Opus-4.8":          "claude-opus-4-8",
		"claude-opus-4.8-20260101": "claude-opus-4-8",
		"claude-opus-4-8-20260101": "claude-opus-4-8",
		"gpt-5.6-sol":              "gpt-5-6-sol",
		"minimax-m2.5":             "minimax-m2-5",
		"claude-sonnet-4":          "claude-sonnet-4",
		"qwen3-coder-next":         "qwen3-coder-next",
		"auto":                     "auto",
	}
	for input, want := range cases {
		if got := PublicModelID(input); got != want {
			t.Errorf("PublicModelID(%q)=%q, want %q", input, got, want)
		}
	}
}
