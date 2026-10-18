package proxy

import (
	"bytes"
	"encoding/json"
)

// marshalUpstreamJSON encodes a Kiro request the way JSON.stringify does.
// encoding/json escapes <, > and & by default, which is a stable byte-level
// difference from the Node client the IDE uses. The trailing newline added by
// Encoder.Encode is removed so the body stays a single JSON value.
func marshalUpstreamJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	body := buf.Bytes()
	if len(body) > 0 && body[len(body)-1] == '\n' {
		body = body[:len(body)-1]
	}
	return body, nil
}
