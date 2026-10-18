package proxy

import (
	"strings"
	"testing"
	"time"
)

func TestImagePayloadNotCountedInCache(t *testing.T) {
	text := strings.Repeat("explain the attached screenshot in detail. ", 200)
	image := strings.Repeat("abcDEF0123+/", 40000)
	source := map[string]interface{}{
		"type":       "base64",
		"media_type": "image/png",
		"data":       image,
	}
	withImage := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{
			Role: "user",
			Content: []interface{}{
				map[string]interface{}{"type": "text", "text": text},
				map[string]interface{}{"type": "image", "source": source},
			},
		}},
	}
	textOnly := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{Role: "user", Content: text}},
	}

	withTokens := estimateClaudeRequestInputTokens(withImage)
	textTokens := estimateClaudeRequestInputTokens(textOnly)
	if withTokens != textTokens {
		t.Fatalf("image payload changed input tokens: with=%d text=%d", withTokens, textTokens)
	}
	if got := source["data"]; got != image {
		t.Fatalf("token estimate mutated image bytes")
	}
	if withTokens >= 20000 {
		t.Fatalf("input tokens=%d, image base64 is still being counted", withTokens)
	}

	tracker := newPromptCacheTracker(time.Hour)
	imageProfile := tracker.BuildClaudeProfile(withImage, withTokens, "image-session")
	textProfile := tracker.BuildClaudeProfile(textOnly, textTokens, "image-session")
	if imageProfile == nil || textProfile == nil || len(imageProfile.Breakpoints) == 0 || len(textProfile.Breakpoints) == 0 {
		t.Fatal("expected cache profiles for the text and image requests")
	}
	imageBreak := imageProfile.Breakpoints[len(imageProfile.Breakpoints)-1]
	textBreak := textProfile.Breakpoints[len(textProfile.Breakpoints)-1]
	if imageBreak.Fingerprint == textBreak.Fingerprint {
		t.Fatal("image bytes were removed from the cache fingerprint")
	}
	if imageBreak.CumulativeTokens > textBreak.CumulativeTokens+64 {
		t.Fatalf("cache tokens with image=%d text=%d", imageBreak.CumulativeTokens, textBreak.CumulativeTokens)
	}

	first := tracker.Compute("acct", imageProfile)
	if first.CacheCreationInputTokens <= 0 || first.CacheCreationInputTokens > textBreak.CumulativeTokens+64 {
		t.Fatalf("cache creation=%d, want the text prefix only", first.CacheCreationInputTokens)
	}
	tracker.Update("acct", imageProfile)
	second := tracker.Compute("acct", imageProfile)
	if second.CacheCreationInputTokens != 0 || second.CacheReadInputTokens <= 0 || second.CacheReadInputTokens > first.CacheCreationInputTokens {
		t.Fatalf("same image should read the text prefix, got %+v after creation %+v", second, first)
	}

	other := *withImage
	other.Messages = []ClaudeMessage{{
		Role: "user",
		Content: []interface{}{
			map[string]interface{}{"type": "text", "text": text},
			map[string]interface{}{"type": "image", "source": map[string]interface{}{
				"type": "base64", "media_type": "image/png", "data": image + "zz",
			}},
		},
	}}
	otherProfile := tracker.BuildClaudeProfile(&other, withTokens, "image-session")
	if otherProfile.Breakpoints[len(otherProfile.Breakpoints)-1].Fingerprint == imageBreak.Fingerprint {
		t.Fatal("a different image reused the previous cache fingerprint")
	}
	miss := tracker.Compute("acct", otherProfile)
	if miss.CacheReadInputTokens != 0 || miss.CacheCreationInputTokens > textBreak.CumulativeTokens+64 {
		t.Fatalf("different image cache usage=%+v", miss)
	}

	toolResult := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{
			Role: "user",
			Content: []interface{}{map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": "tool_1",
				"content": []interface{}{map[string]interface{}{
					"type":   "image",
					"source": map[string]interface{}{"type": "base64", "media_type": "image/jpeg", "data": image},
				}},
			}},
		}},
	}
	if got := estimateClaudeRequestInputTokens(toolResult); got > 32 {
		t.Fatalf("tool result image tokens=%d", got)
	}

	openAI := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{{
			Role: "user",
			Content: []interface{}{map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]interface{}{"url": "data:image/png;base64," + image},
			}},
		}},
	}
	if got := estimateOpenAIRequestInputTokens(openAI); got > 32 {
		t.Fatalf("openai image tokens=%d", got)
	}
}
