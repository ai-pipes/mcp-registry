package httpx

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// DecodeJSON reads the request body into dst with encoding/json/v2: member names match the
// struct tags case-sensitively (like the TypeScript server: {"NAME":1} does not fill `name`),
// and unknown members are dropped silently. An empty or whitespace-only body counts as `{}`; a
// body of `null`, trailing data after the first value and duplicate member names are errors.
// Failures are DomainErrors: invalid_request "body is not valid JSON", invalid_request
// "<field>: invalid type" for a value of the wrong type (no value echoed), invalid_request "body
// must not be null", and limit_exceeded when BodyLimit cut the body (so WriteDomainError answers
// 413). A handler that reads the body in another way gets the same cut as an error wrapping
// *http.MaxBytesError and passes it to WriteDomainError.
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
	if string(raw) == "null" {
		return InvalidRequest("body must not be null")
	}
	if err := jsonv2.Unmarshal(raw, dst); err != nil {
		var sem *jsonv2.SemanticError
		if errors.As(err, &sem) && sem.JSONKind != 0 && sem.GoType != nil {
			if field := pointerPath(sem.JSONPointer); field != "" {
				return InvalidRequest(field + ": invalid type")
			}
			return InvalidRequest("invalid type")
		}
		return InvalidRequest("body is not valid JSON")
	}
	return nil
}

// pointerPath renders a JSON Pointer ("/a/b") as a dotted path ("a.b").
func pointerPath(p jsontext.Pointer) string {
	return strings.ReplaceAll(strings.TrimPrefix(string(p), "/"), "/", ".")
}
