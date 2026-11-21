package domain

import (
	"errors"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// Error codes of the identity module (the TypeScript IdentityErrorCode).
const (
	CodeInvalidState   = "invalid_state"
	CodeNotAllowed     = "not_allowed"
	CodeDisabled       = "disabled"
	CodeInvalidToken   = "invalid_token"
	CodeRateLimited    = "rate_limited"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeInvalidInput   = "invalid_input"
	CodeInvalidLink    = "invalid_link"    // a login link is unknown, already used or expired
	CodeIDPUnavailable = "idp_unavailable" // the identity provider cannot be reached right now
)

// Error is a refusal of the identity module: a kernel domain error with an optional retry delay.
// It unwraps to its *kernel.DomainError, so kernel.AsDomainError finds it.
type Error struct {
	*kernel.DomainError
	// RetryAfterSec is the suggested wait in seconds; 0 means none.
	RetryAfterSec int
}

// Unwrap exposes the embedded domain error to errors.As and errors.Is.
func (e *Error) Unwrap() error { return e.DomainError }

// Err builds a refusal without a retry delay.
func Err(code, message string) *Error {
	return &Error{DomainError: kernel.NewDomainError(code, message)}
}

// ErrRetryAfter builds a refusal that tells the caller how long to wait.
func ErrRetryAfter(code, message string, retryAfterSec int) *Error {
	e := Err(code, message)
	e.RetryAfterSec = retryAfterSec
	return e
}

// AsError finds an identity *Error in err's chain.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
