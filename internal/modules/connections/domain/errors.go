package domain

import (
	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// Error codes of the connections module (the TypeScript ConnectionErrorCode).
const (
	// CodeNotFound: no such server, not active, or the person may not use it.
	CodeNotFound = "not_found"
	// CodeInvalidAuthKind: the server does not take this kind of personal credential.
	CodeInvalidAuthKind = "invalid_auth_kind"
	// CodeInvalidState: the connect attempt is unknown, used, expired or someone else's.
	CodeInvalidState = "invalid_state"
	// CodeExchangeFailed: the OAuth provider refused to exchange the code.
	CodeExchangeFailed = "exchange_failed"
	// CodeInvalidPAT: the pasted token is empty, too long or not a single line.
	CodeInvalidPAT = "invalid_pat"
)

// What the server was expected to use, for ErrInvalidAuthKind's message.
const (
	WhatOAuth = "OAuth"
	WhatPAT   = "personal access tokens"
)

// ErrNotFound is `server "<slug>" not found`.
func ErrNotFound(slug string) error {
	return kernel.NewDomainError(CodeNotFound, `server "`+slug+`" not found`)
}

// ErrInvalidAuthKind is `server "<slug>" does not use <what>` (WhatOAuth or WhatPAT).
func ErrInvalidAuthKind(slug, what string) error {
	return kernel.NewDomainError(CodeInvalidAuthKind, `server "`+slug+`" does not use `+what)
}

// ErrInvalidState is the refusal of an unknown, used, expired or foreign connect attempt.
func ErrInvalidState() error {
	return kernel.NewDomainError(CodeInvalidState, "connect attempt is unknown or expired")
}

// ErrExchangeFailed is the refusal of the code exchange.
func ErrExchangeFailed() error {
	return kernel.NewDomainError(CodeExchangeFailed, "the provider rejected the authorization")
}

func errInvalidPAT() error {
	return kernel.NewDomainError(CodeInvalidPAT, "the token must be a non-empty single line")
}
