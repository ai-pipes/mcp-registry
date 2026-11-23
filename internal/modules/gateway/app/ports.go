package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/modules/search"
)

// Principal is who is calling: the identity module's principal.
type Principal = identity.Principal

// ToolCatalog is what the gateway reads of the catalog. The composition implements it over the
// catalog's root package.
type ToolCatalog interface {
	// ToolFor is the tool the person may use now. ok is false for an unknown, hidden or removed
	// tool, a malformed ref, and a server that is unknown, disabled or closed to the person; the
	// error is for unexpected failures.
	ToolFor(ctx context.Context, p Principal, ref string) (tool catalog.ToolView, ok bool, err error)
	// ServerFor is the active server's call target, nil when unknown or disabled.
	ServerFor(ctx context.Context, slug string) (*catalog.ServerCallTarget, error)
	// ServerSecret is the server's decrypted shared secret for the target the caller read
	// (catalog.Service.GetServerSecretFor): SecretFound only while the secret belongs to that
	// target's configuration (an empty secret that is set is returned as such), SecretNotSet when
	// none is set, SecretMoved when the server no longer sends its secret where the target would.
	ServerSecret(ctx context.Context, target catalog.ServerCallTarget) (secret string, found catalog.SecretLookup, err error)
}

// PersonalCredentials is the person's own secret for a server. *connections.Service implements
// it. Resolving takes the target the caller read: the secret is handed out only while the server
// is still configured so (otherwise the failure is unavailable).
type PersonalCredentials interface {
	ResolveForSending(ctx context.Context, userID string, target catalog.ServerCallTarget, forceRefresh bool) (connections.CredentialResult, error)
	MarkExpired(ctx context.Context, c connections.MarkExpiredCommand) error
}

// ArgsCheck is the verdict of an ArgsValidator: OK, or the JSON pointer of the first offence and
// the validator's own message.
type ArgsCheck struct {
	OK      bool
	Path    string
	Message string
}

// ArgsValidator checks a call's arguments against a tool's input schema. It is advisory (the
// upstream validates too): a schema it cannot use is not enforced. ref only names the tool in
// diagnostics; it never fails (a problem with a schema is its own concern).
type ArgsValidator interface {
	Validate(ref string, schema json.RawMessage, args json.RawMessage) ArgsCheck
}

// SecretComparer compares two secrets in constant time (crypto/subtle in infra: crypto is not
// allowed in this layer).
type SecretComparer interface {
	Equal(a, b string) bool
}

// CallLogEntry is one row of the call log: who, with which token, what, when, how long, how it
// ended. Never arguments or results.
type CallLogEntry struct {
	UserID string
	// TokenID is empty for a session (NULL in the log).
	TokenID    string
	Server     string
	Tool       string
	StartedAt  time.Time
	DurationMs int64
	Outcome    domain.CallOutcome
}

// CallRecord is a stored entry with its id (a decimal string; the log orders by its number).
type CallRecord struct {
	ID string
	CallLogEntry
}

// CallLogFilter selects entries, newest first. Nil fields are not filtered. Before alone is
// "strictly older"; with BeforeID the pair (Before, BeforeID) is the exclusive upper cursor, so
// rows sharing a start time are neither skipped nor repeated.
type CallLogFilter struct {
	UserID   *string
	Server   *string
	Outcome  *domain.CallOutcome
	Before   *time.Time
	BeforeID *string
	Limit    int
}

// CallLog is the call log store.
type CallLog interface {
	Record(ctx context.Context, e CallLogEntry) error
	List(ctx context.Context, f CallLogFilter) ([]CallRecord, error)
	// DeleteOlderThan removes entries that started before t and returns how many.
	DeleteOlderThan(ctx context.Context, t time.Time) (int64, error)
}

// QuotaRequest is one unit against a person's daily limit for a server.
type QuotaRequest struct {
	UserID string
	Server string
	// Day is the UTC day, YYYY-MM-DD.
	Day   string
	Limit int
}

// Quota is the daily counter store.
type Quota interface {
	// TryConsume counts one call and reports whether it fits the limit (one atomic statement; a
	// refused call counts nothing).
	TryConsume(ctx context.Context, r QuotaRequest) (ok bool, err error)
	// DeleteDaysBefore removes the counters of days before day (YYYY-MM-DD) and returns how many.
	DeleteDaysBefore(ctx context.Context, day string) (int64, error)
}

// Logger receives the failures the use cases absorb. Fields carry no secrets, no arguments and no
// error messages (kernel.ErrorLabel). *slog.Logger satisfies it.
type Logger interface {
	Error(msg string, args ...any)
}

// Deps is what the use cases need: ports and plain values, never the configuration.
type Deps struct {
	Catalog     ToolCatalog
	Credentials PersonalCredentials
	Validator   ArgsValidator
	CallLog     CallLog
	Quota       Quota
	Upstream    kernel.UpstreamClient
	Clock       kernel.Clock
	Logger      Logger
	Secrets     SecretComparer

	// PublicURL is PUBLIC_URL; trailing slashes are removed where it is put into a text.
	PublicURL string
	// MaxResponseBytes is UPSTREAM_MAX_RESPONSE_BYTES.
	MaxResponseBytes int64
	// BaseContext ends when the process shuts down: calls in flight are cancelled then (and write
	// a transport_error row). A call is detached from its request's context, not from this one.
	// Nil means never.
	BaseContext context.Context
}

// PersonCatalog is what the meta-tools read of the catalog for a person: the servers the person may
// use (with their visible tool counts) and a tool the person may use now. *catalog.Service
// implements it; GetToolFor's DomainError codes not_found and invalid_tool_ref mean "not offered".
type PersonCatalog interface {
	ListServersFor(ctx context.Context, p Principal) ([]catalog.MemberServerView, error)
	GetToolFor(ctx context.Context, p Principal, ref string) (catalog.ToolView, error)
}

// PersonConnections is the person's connections to the servers that take personal credentials.
// *connections.Service implements it.
type PersonConnections interface {
	ListConnections(ctx context.Context, p Principal) ([]connections.ConnectionSummary, error)
}

// ToolSearch is the search over the tool index. *search.Service implements it.
type ToolSearch interface {
	SearchTools(ctx context.Context, query string, limit int, servers []string) ([]search.ToolHit, error)
}

// MetaDeps is what list_servers, search_tools and describe_tool need: the other modules' public
// services and PUBLIC_URL (for the connections link).
type MetaDeps struct {
	Catalog     PersonCatalog
	Connections PersonConnections
	Search      ToolSearch
	// PublicURL is PUBLIC_URL (trailing slashes are removed in the link).
	PublicURL string
}
