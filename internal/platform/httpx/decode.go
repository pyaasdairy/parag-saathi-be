package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// maxBodyBytes caps request bodies at 1 MiB — pours, readings and admin
// payloads are all tiny; photo evidence goes to the object store, not the API.
const maxBodyBytes = 1 << 20

// DecodeJSON strictly decodes the request body into dst. It enforces the
// size cap and exactly one JSON value.
func DecodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return BadRequest("BODY_TOO_LARGE", fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes))
		case errors.Is(err, io.EOF):
			return BadRequest("EMPTY_BODY", "request body is required")
		default:
			return BadRequest("MALFORMED_JSON", "request body is not valid JSON: "+err.Error())
		}
	}
	if dec.More() {
		return BadRequest("MALFORMED_JSON", "request body must contain a single JSON value")
	}
	return nil
}

// Page holds validated pagination parameters.
type Page struct {
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
}

// ParsePage reads ?limit= and ?offset= with sane bounds (default 50, max 200).
func ParsePage(r *http.Request) Page {
	p := Page{Limit: 50, Offset: 0}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			p.Limit = min(n, 200)
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			p.Offset = n
		}
	}
	return p
}
