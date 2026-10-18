package proxy

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"strings"

	"kiro-go/logger"
)

const (
	kiroImageShrinkMaxSide      = 1600
	kiroImageShrinkMinSide      = 640
	kiroImageShrinkStartQuality = 75
	kiroImageShrinkMinQuality   = 40
	kiroImageShrinkMaxSteps     = 24
)

type kiroImageShrinkState struct {
	quality int
	maxSide int
	stuck   bool
}

// compressKiroPayloadImages re-encodes oversized images until the marshaled
// Kiro request fits the raw body limit. The limit applies to the whole
// request, so images are reduced only by the amount the other fields do not
// already consume. History text is left untouched.
func compressKiroPayloadImages(payload *KiroPayload, rawLimit int) (bool, error) {
	if payload == nil || rawLimit <= 0 || len(collectKiroImages(payload)) == 0 {
		return false, nil
	}
	before, err := measureKiroPayloadTotals(payload)
	if err != nil {
		return false, err
	}
	if before.TotalBytes <= rawLimit {
		return false, nil
	}

	states := make(map[*KiroImage]*kiroImageShrinkState)
	changed := false
	for step := 0; step < kiroImageShrinkMaxSteps; step++ {
		current, measureErr := measureKiroPayloadTotals(payload)
		if measureErr != nil {
			return changed, measureErr
		}
		if current.TotalBytes <= rawLimit {
			logger.Infof(
				"[KiroPreflight] compressed images request_id=%s images=%d before_bytes=%d after_bytes=%d raw_limit_bytes=%d",
				kiroPayloadRequestID(payload), len(collectKiroImages(payload)),
				before.TotalBytes, current.TotalBytes, rawLimit,
			)
			return true, nil
		}
		target := largestShrinkableKiroImage(payload, states)
		if target == nil {
			break
		}
		state := states[target]
		if state == nil {
			state = &kiroImageShrinkState{}
			states[target] = state
		}
		if !shrinkKiroImageOneStep(target, state) {
			state.stuck = true
			continue
		}
		changed = true
	}
	if changed {
		current, measureErr := measureKiroPayloadTotals(payload)
		if measureErr != nil {
			return true, measureErr
		}
		logger.Infof(
			"[KiroPreflight] compressed images request_id=%s images=%d before_bytes=%d after_bytes=%d raw_limit_bytes=%d",
			kiroPayloadRequestID(payload), len(collectKiroImages(payload)),
			before.TotalBytes, current.TotalBytes, rawLimit,
		)
	}
	return changed, nil
}

func collectKiroImages(payload *KiroPayload) []*KiroImage {
	if payload == nil {
		return nil
	}
	images := make([]*KiroImage, 0)
	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	for i := range current.Images {
		images = append(images, &current.Images[i])
	}
	for i := range payload.ConversationState.History {
		user := payload.ConversationState.History[i].UserInputMessage
		if user == nil {
			continue
		}
		for j := range user.Images {
			images = append(images, &user.Images[j])
		}
	}
	return images
}

func largestShrinkableKiroImage(payload *KiroPayload, states map[*KiroImage]*kiroImageShrinkState) *KiroImage {
	var best *KiroImage
	bestLen := 0
	for _, img := range collectKiroImages(payload) {
		if state := states[img]; state != nil && state.stuck {
			continue
		}
		size := len(img.Source.Bytes)
		if size > bestLen {
			best = img
			bestLen = size
		}
	}
	return best
}

func shrinkKiroImageOneStep(img *KiroImage, state *kiroImageShrinkState) bool {
	if img == nil || state == nil || state.stuck {
		return false
	}
	decoded, err := decodeKiroImageBase64(img.Source.Bytes)
	if err != nil {
		state.stuck = true
		return false
	}
	picture, _, err := image.Decode(bytes.NewReader(decoded))
	if err != nil {
		state.stuck = true
		return false
	}
	original := img.Source.Bytes
	for attempt := 0; attempt < 6; attempt++ {
		next, quality, ok := state.next(picture)
		if !ok {
			state.stuck = true
			return false
		}
		encoded, encodeErr := encodeJPEG(next, quality)
		if encodeErr != nil {
			state.stuck = true
			return false
		}
		compressed := base64.StdEncoding.EncodeToString(encoded)
		if len(compressed) < len(original) {
			img.Format = "jpeg"
			img.Source.Bytes = compressed
			return true
		}
	}
	state.stuck = true
	return false
}

func (s *kiroImageShrinkState) next(picture image.Image) (image.Image, int, bool) {
	if s.stuck {
		return nil, 0, false
	}
	bounds := picture.Bounds()
	longSide := bounds.Dx()
	if bounds.Dy() > longSide {
		longSide = bounds.Dy()
	}
	if s.quality == 0 {
		s.quality = kiroImageShrinkStartQuality
		s.maxSide = kiroImageShrinkMaxSide
		if longSide > s.maxSide {
			return fitImageLongSide(picture, s.maxSide), s.quality, true
		}
		return picture, s.quality, true
	}
	if s.quality > kiroImageShrinkMinQuality {
		s.quality -= 15
		if s.quality < kiroImageShrinkMinQuality {
			s.quality = kiroImageShrinkMinQuality
		}
		return fitImageLongSide(picture, s.maxSide), s.quality, true
	}
	if s.maxSide > kiroImageShrinkMinSide {
		s.maxSide = s.maxSide * 3 / 4
		if s.maxSide < kiroImageShrinkMinSide {
			s.maxSide = kiroImageShrinkMinSide
		}
		return fitImageLongSide(picture, s.maxSide), s.quality, true
	}
	return nil, 0, false
}

func fitImageLongSide(src image.Image, maxSide int) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	longSide := width
	if height > longSide {
		longSide = height
	}
	if longSide <= maxSide || maxSide < 1 || width <= 0 || height <= 0 {
		return src
	}
	newWidth, newHeight := maxSide, height*maxSide/longSide
	if height > width {
		newHeight = maxSide
		newWidth = width * maxSide / longSide
	}
	if newWidth < 1 {
		newWidth = 1
	}
	if newHeight < 1 {
		newHeight = 1
	}
	return scaleImageNearest(src, newWidth, newHeight)
}

func scaleImageNearest(src image.Image, width, height int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := src.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return dst
	}
	for y := 0; y < height; y++ {
		sy := bounds.Min.Y + y*srcH/height
		for x := 0; x < width; x++ {
			sx := bounds.Min.X + x*srcW/width
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

func encodeJPEG(picture image.Image, quality int) ([]byte, error) {
	if quality < 1 {
		quality = 1
	}
	if quality > 100 {
		quality = 100
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, picture, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeKiroImageBase64(data string) ([]byte, error) {
	data = strings.TrimSpace(data)
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	var lastErr error
	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(data)
		if err == nil {
			return decoded, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
