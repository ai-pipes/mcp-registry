package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// DecodeJSON reads the request body into dst. An empty or whitespace-only body counts as `{}`;
// unknown fields are dropped silently; anything after the first JSON value is an error. Failures
// are DomainErrors: invalid_request "body is not valid JSON", invalid_request "<field>: invalid
// type" for a value of the wrong type, and limit_exceeded when BodyLimit cut the body (so
// WriteDomainError answers 413). A handler that reads the body in another way gets the same cut
// as an error wrapping *http.MaxBytesError and passes it to WriteDomainError.
func DecodeJSON(r *http.Request, dst any) error {
	var raw []byte
	if r.Body != nil {
		var err error
		raw, err = io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return kernel.NewDomainError("limit_exceeded", errBodyTooLarge)
			}
			return InvalidRequest("body could not be read")
		}
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			if typeErr.Field != "" {
				return InvalidRequest(typeErr.Field + ": invalid type")
			}
			return InvalidRequest("invalid type")
		}
		return InvalidRequest("body is not valid JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return InvalidRequest("body is not valid JSON")
	}
	return nil
}
