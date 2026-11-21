package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The port to an upstream MCP server (Streamable HTTP), mirroring shared-kernel/upstream.ts.
// The implementation lives in internal/platform/upstream; catalog (synchronisation) and gateway
// (call_tool) use it.

// UpstreamTarget is where and how to reach an upstream for one operation. Headers already carry
// the credentials, so a target must never be logged or put into an error.
type UpstreamTarget struct {
	URL              string
	Headers          map[string]string
	Timeout          time.Duration // one deadline for the whole operation
	MaxResponseBytes int64         // all responses of the operation together
}

// UpstreamTool is a tool as upstream tools/list describes it. The schemas and annotations are
// the raw JSON as upstream sent them, so nothing is lost or invented: a nil field was absent,
// a non-nil one (even "null" or "{}") was present. InputSchema is always present.
type UpstreamTool struct {
	Name         string
	Title        *string // nil: absent
	Description  *string // nil: absent
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage // nil: absent
	Annotations  json.RawMessage // nil: absent
}

// UpstreamCallResult is the upstream tools/call result as raw JSON ({"content": [...],
// "structuredContent": ..., "isError": ...}); the gateway hands it to the agent unchanged.
type UpstreamCallResult = json.RawMessage

// UpstreamClient is the port. Each operation is its own MCP session: nothing, credentials
// included, is shared between operations.
type UpstreamClient interface {
	// ListTools returns every page of tools/list.
	ListTools(ctx context.Context, target UpstreamTarget) ([]UpstreamTool, error)
	// CallTool calls one tool; arguments is the raw JSON object (nil or empty: none).
	CallTool(ctx context.Context, target UpstreamTarget, name string, arguments json.RawMessage) (UpstreamCallResult, error)
}

// UpstreamErrorKind classifies a failure to use an upstream.
type UpstreamErrorKind string

// UpstreamUnauthorized: upstream answered HTTP 401. UpstreamTimeout: the operation's deadline
// passed. UpstreamTooLarge: the answers exceeded MaxResponseBytes. UpstreamTransport: network
// failure, any other non-2xx, a redirect, a malformed answer. UpstreamRejected: upstream answered
// the request with a JSON-RPC error (unknown tool, invalid params, an error of its handler), an
// answer rather than a transport failure.
const (
	UpstreamUnauthorized UpstreamErrorKind = "unauthorized"
	UpstreamTimeout      UpstreamErrorKind = "timeout"
	UpstreamTooLarge     UpstreamErrorKind = "too_large"
	UpstreamTransport    UpstreamErrorKind = "transport"
	UpstreamRejected     UpstreamErrorKind = "rejected"
)

// UpstreamRejection is the JSON-RPC error upstream answered with. Message is upstream's text:
// for the agent only, never for logs, audit or error strings.
type UpstreamRejection struct {
	Code    int
	Message string
}

// UpstreamError means an upstream could not be used. Error() is a fixed text per kind: it
// carries no upstream body, header, URL or credential, and not the rejection's message, which is
// reachable only through the Rejection field. Rejection is set for UpstreamRejected only.
type UpstreamError struct {
	Kind      UpstreamErrorKind
	Rejection *UpstreamRejection
}

// NewUpstreamError builds an error of a kind other than UpstreamRejected (see
// RejectedUpstreamError).
func NewUpstreamError(kind UpstreamErrorKind) *UpstreamError { return &UpstreamError{Kind: kind} }

// RejectedUpstreamError is the error for a JSON-RPC error answer.
func RejectedUpstreamError(code int, message string) *UpstreamError {
	return &UpstreamError{Kind: UpstreamRejected, Rejection: &UpstreamRejection{Code: code, Message: message}}
}

// Error is the same text for every error of a kind (the rejection adds only its numeric code).
func (e *UpstreamError) Error() string {
	switch e.Kind {
	case UpstreamUnauthorized:
		return "upstream answered 401"
	case UpstreamTimeout:
		return "upstream did not answer in time"
	case UpstreamTooLarge:
		return "upstream answer exceeds the size limit"
	case UpstreamRejected:
		if e.Rejection != nil {
			return fmt.Sprintf("upstream rejected the request (JSON-RPC %d)", e.Rejection.Code)
		}
		return "upstream rejected the request"
	default:
		return "upstream transport error"
	}
}

// Code is the kind, the short value-free code ErrorLabel appends.
func (e *UpstreamError) Code() string { return string(e.Kind) }

// AsUpstreamError finds an UpstreamError anywhere in err's wrap chain.
func AsUpstreamError(err error) (*UpstreamError, bool) {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}
