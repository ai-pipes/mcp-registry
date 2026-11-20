package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

const errBodyTooLarge = "request body is too large"

// StatusFor is the HTTP status of an error code: the table of the reference server
// (apps/server/src/platform/http/errors.ts), plus internal -> 500. Any other invalid_* is 422,
// everything else 400.
func StatusFor(code string) int {
	switch code {
	case "not_found":
		return http.StatusNotFound
	case "invalid_token", "unauthenticated":
		return http.StatusUnauthorized
	case "forbidden", "csrf":
		return http.StatusForbidden
	case "limit_exceeded":
		return http.StatusRequestEntityTooLarge
	case "rate_limited":
		return http.StatusTooManyRequests
	case "server_exists", "server_conflict":
		return http.StatusConflict
	case "sync_failed":
		return http.StatusBadGateway
	case "internal":
		return http.StatusInternalServerError
	}
	if strings.HasPrefix(code, "invalid_") {
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadRequest
}

// WriteError writes {"error":{"code","message"}} with the status for code.
func WriteError(w http.ResponseWriter, code, message string) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
	body := bytes.TrimRight(buf.Bytes(), "\n")
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	w.WriteHeader(StatusFor(code))
	_, _ = w.Write(body)
}

// WriteDomainError answers a DomainError with its code and message. A body cut by BodyLimit
// (*http.MaxBytesError anywhere in the chain) is the same 413 limit_exceeded answer. Anything
// else is a 500 "internal" / "Internal error" and a log record without the error's text (it may
// quote user input or secrets), written to slog's default logger, which the composition root sets.
func WriteDomainError(w http.ResponseWriter, err error) {
	if de, ok := kernel.AsDomainError(err); ok {
		WriteError(w, de.Code, de.Message)
		return
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		WriteError(w, "limit_exceeded", errBodyTooLarge)
		return
	}
	slog.Error("unhandled error", logging.ErrAttr(err))
	WriteError(w, "internal", "Internal error")
}

// InvalidRequest is the error of a malformed request (code invalid_request, status 400):
// the one query and body handlers of modules return for input they cannot use.
func InvalidRequest(message string) error {
	return kernel.NewDomainError("invalid_request", message)
}
