package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

var errMultipleRequestJSONValues = errors.New("request body must contain exactly one JSON value")

// decodeRequestJSONWithLimit decodes a single JSON value directly from the
// request stream. This avoids retaining both the complete raw body and the
// decoded object for endpoints that do not need byte-for-byte request data.
func decodeRequestJSONWithLimit(w http.ResponseWriter, r *http.Request, maxBytes int64, dst interface{}) error {
	started := time.Now()
	defer func() {
		observeRequestStage(r.Context(), stageBodyRead, time.Since(started))
	}()
	if r.ContentLength > maxBytes {
		return &http.MaxBytesError{Limit: maxBytes}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errMultipleRequestJSONValues
		}
		return err
	}
	return nil
}
