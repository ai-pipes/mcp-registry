package domain

import "github.com/ai-pipes/mcp-registry/internal/kernel"

// Error codes of the catalog module (the TypeScript CatalogErrorCode). The domain itself returns
// the invalid_* codes and not_found; the others are used by the application layer.
const (
	CodeInvalidSlug    = "invalid_slug"
	CodeInvalidToolRef = "invalid_tool_ref"
	CodeInvalidAuth    = "invalid_auth"
	CodeInvalidAccess  = "invalid_access"
	CodeInvalidServer  = "invalid_server"
	CodeServerExists   = "server_exists"
	// CodeServerConflict: the server was changed by someone else between reading and writing it.
	CodeServerConflict  = "server_conflict"
	CodeForbidden       = "forbidden"
	CodeNotFound        = "not_found"
	CodeNeedsConnection = "needs_connection"
	CodeSyncFailed      = "sync_failed"
)

// newError is the catalog's refusal: a kernel.DomainError (kernel.AsDomainError finds it).
func newError(code, message string) error { return kernel.NewDomainError(code, message) }
