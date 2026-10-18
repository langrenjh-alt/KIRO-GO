package proxy

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"math/rand"
	"testing"
)

func TestPreflightCompressesImagesToRawBodyLimit(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	t.Setenv("KIRO_INPUT_TOKEN_RESERVE", "8192")

	first := noisyPNGBase64(t, 340, 340)
	second := noisyPNGBase64(t, 340, 340)
	payload := &KiroPayload{}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = "image-preflight"
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "KEEP THIS HISTORY", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "recent answer"}},
	}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "describe the screenshots",
		ModelID: "claude-opus-4.8",
		Origin:  "AI_EDITOR",
		Images: []KiroImage{
			{Format: "png", Source: struct {
				Bytes string `json:"bytes"`
			}{Bytes: first}},
			{Format: "png", Source: struct {
				Bytes string `json:"bytes"`
			}{Bytes: second}},
		},
	}

	before, err := measureKiroPayloadTotals(payload)
	if err != nil {
		t.Fatalf("measure before: %v", err)
	}
	if before.TotalBytes <= 921600 {
		t.Fatalf("fixture bytes=%d, want above the 900 KiB raw limit", before.TotalBytes)
	}
	if before.EstimatedInputTokens > getContextWindowSize("claude-opus-4.8")-defaultKiroInputTokenReserve {
		t.Fatalf("fixture tokens=%d unexpectedly exceed the model budget", before.EstimatedInputTokens)
	}

	result, err := preflightKiroPayload(payload)
	if err != nil {
		t.Fatalf("preflight rejected a request that image compression can fit: %v", err)
	}
	if result.After.TotalBytes > result.RawBodyLimitBytes {
		t.Fatalf("compressed payload bytes=%d limit=%d", result.After.TotalBytes, result.RawBodyLimitBytes)
	}
	if result.DroppedHistoryEntries != 0 {
		t.Fatalf("history was trimmed although images were the overflow: dropped=%d", result.DroppedHistoryEntries)
	}
	if !kiroHistoryContainsText(payload.ConversationState.History, "KEEP THIS HISTORY") {
		t.Fatal("history was removed while compressing images")
	}
	images := payload.ConversationState.CurrentMessage.UserInputMessage.Images
	if len(images) != 2 {
		t.Fatalf("images=%d, want 2", len(images))
	}
	compressed := 0
	for _, img := range images {
		if img.Format == "jpeg" && len(img.Source.Bytes) < len(first) {
			compressed++
		}
	}
	if compressed == 0 {
		t.Fatal("no image was compressed below its original PNG")
	}
	if payload.ConversationState.CurrentMessage.UserInputMessage.Content != "describe the screenshots" {
		t.Fatal("current text changed while compressing images")
	}
}

func TestPreflightLeavesFittingImagesUnchanged(t *testing.T) {
	t.Setenv("KIRO_MAX_RAW_BODY_BYTES", "921600")
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "small",
		ModelID: "claude-opus-4.8",
		Origin:  "AI_EDITOR",
		Images: []KiroImage{{
			Format: "png",
			Source: struct {
				Bytes string `json:"bytes"`
			}{Bytes: noisyPNGBase64(t, 8, 8)},
		}},
	}
	original := payload.ConversationState.CurrentMessage.UserInputMessage.Images[0].Source.Bytes
	if _, err := preflightKiroPayload(payload); err != nil {
		t.Fatalf("small image was rejected: %v", err)
	}
	if got := payload.ConversationState.CurrentMessage.UserInputMessage.Images[0].Source.Bytes; got != original {
		t.Fatal("image under the raw limit was recompressed")
	}
}

func noisyPNGBase64(t *testing.T, width, height int) string {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	rng := rand.New(rand.NewSource(1))
	rng.Read(picture.Pix)
	var buf bytes.Buffer
	if err := png.Encode(&buf, picture); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
