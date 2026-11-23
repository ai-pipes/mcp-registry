package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

// Principal is who is acting: the identity module's principal. The catalog reads only its
// UserID, Role and Groups.
type Principal = identity.Principal

// ServerListing is a server with the number of its tools that are visible (not hidden, not removed).
type ServerListing struct {
	Server    *domain.Server
	ToolCount int
}

// ServerRepository stores servers. Every operation that changes a server and produces events
// takes the events with it and writes both in ONE transaction: the use case cannot save the
// aggregate and forget (or double-write) its events. Edits are written only at the version the
// aggregate was read at.
type ServerRepository interface {
	// FindBySlug returns the server as stored (nil, nil when there is none). Reading takes no lock.
	FindBySlug(ctx context.Context, slug string) (*domain.Server, error)
	// List returns every server ordered by slug, with its visible tool count from the same query.
	List(ctx context.Context) ([]ServerListing, error)
	// Create inserts a new server (version 0) and appends the events to the outbox, in one
	// transaction. A taken slug is the domain error server_exists.
	Create(ctx context.Context, server *domain.Server, events []kernel.Event) error
	// Save writes an edit and appends the events to the outbox, in one transaction, only if the
	// stored version is still the one the aggregate was read at (and bumps it); otherwise nothing
	// is written and the error is the domain's server_conflict (not_found when the server is
	// gone). The secret is written only when the aggregate changed it (Server.SecretChanged).
	// Synchronisation state is never written by Save.
	Save(ctx context.Context, server *domain.Server, events []kernel.Event) error
	// SaveSyncState writes only the synchronisation state, atomically, by the rule of
	// Server.ApplySync: an attempt that started before the stored last_sync_at changes nothing,
	// and sync_user_id changes only when the record names SyncedBy. not_found for an unknown server.
	SaveSyncState(ctx context.Context, slug string, record domain.SyncRecord) error
	// ReplaceSealedSecret swaps the sealed secret only while it still equals old
	// (compare-and-set), touching nothing else: no updated_at, no version, no events. false when
	// it changed meanwhile or the server is gone.
	ReplaceSealedSecret(ctx context.Context, slug string, old, next []byte) (replaced bool, err error)
}

// SnapshotResult says what ApplySnapshot did; AddedRefs is a subset of ChangedRefs (tools that
// were absent or removed before).
type SnapshotResult struct {
	ChangedRefs []string
	AddedRefs   []string
	RemovedRefs []string
}

// ToolRepository stores tool snapshots.
type ToolRepository interface {
	// Find returns the stored tool, removed ones included (nil, nil when there is none).
	Find(ctx context.Context, server, name string) (*domain.Tool, error)
	// ListByServer returns every stored tool of the server, removed ones included, ordered by name.
	ListByServer(ctx context.Context, server string) ([]*domain.Tool, error)
	// Save persists the tool's hidden flag and appends the events (the tool's own, drained by the
	// use case) to the outbox in one transaction. not_found when the tool is gone.
	Save(ctx context.Context, tool *domain.Tool, events []kernel.Event) error
	// ApplySnapshot, in ONE transaction: locks the server row (not_found when unknown), reads
	// its stored tools, reconciles them with the snapshot (domain.ReconcileTools), writes the
	// result and appends the reconciliation's events to the outbox. The events are produced
	// under the lock, from what is stored at that moment, so the use case never sees them.
	ApplySnapshot(ctx context.Context, server string, snapshot []domain.ToolSnapshot, now time.Time) (SnapshotResult, error)
}

// PersonalCredential is the answer of SyncCredentials.PersonalSecret: the person's own upstream
// secret (an OAuth access token or a PAT, raw), or why there is none (Failure set, Secret empty).
type PersonalCredential struct {
	Secret  string
	Failure domain.PersonalCredentialFailure
}

// SyncCredentials is what a synchronisation of an oauth_user / pat_user server needs from other
// modules: whether the credentials owner is still an enabled admin (identity) and that person's
// own upstream secret (connections). The composition implements it over their public APIs with
// no rules of its own: whose connection may serve and what a failure means for the
// synchronisation are the catalog's (domain.MayLend, domain.FailureOfCredential). The catalog
// builds the header itself (domain.HeaderFor). An unexpected failure is the error return.
type SyncCredentials interface {
	IsActiveAdmin(ctx context.Context, userID string) (bool, error)
	PersonalSecret(ctx context.Context, server ServerCallTarget, userID string) (PersonalCredential, error)
}

// Logger receives the unexpected errors the module absorbs instead of returning. Fields carry no
// secrets and no error messages (use kernel.ErrorLabel). *slog.Logger satisfies it.
type Logger interface {
	Error(msg string, args ...any)
}

// Deps is everything the use cases need: ports and plain values, never the configuration.
type Deps struct {
	Servers ServerRepository
	Tools   ToolRepository
	Box     kernel.SecretBox
	Clock   kernel.Clock
	Audit   kernel.AuditLog

	// Synchronisation only.
	Upstream    kernel.UpstreamClient
	Credentials SyncCredentials
	// MaxResponseBytes: upstream answers above this size fail a synchronisation
	// (UPSTREAM_MAX_RESPONSE_BYTES).
	MaxResponseBytes int64
	// Logger may be nil.
	Logger Logger
}

// ServerView is the admin's view of a server. It holds SecretSet only: never the sealed bytes,
// never the opened secret, and no version.
type ServerView struct {
	Slug          string
	Name          string
	Description   string
	URL           string
	Auth          domain.AuthMode
	SecretSet     bool
	Access        domain.Access
	TimeoutMs     int
	DailyLimit    *int
	Status        domain.ServerStatus
	LastSyncAt    *time.Time
	LastSyncError *string
	ToolCount     int
}

// MemberServerView is what a member sees of a server they may use: no URL, no auth config, no
// sync state.
type MemberServerView struct {
	Slug        string
	Name        string
	Description string
	ToolCount   int
	AuthKind    domain.AuthKind
}

// ToolView is a stored tool as the gateway and the admin screens see it.
type ToolView struct {
	Server       string
	Name         string
	Ref          string
	Title        *string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage // nil: absent
	Annotations  domain.ToolAnnotations
	Hidden       bool
}

// ServerCallTarget is what the gateway needs to call an upstream. No secret: it is read on demand
// with GetServerSecretFor, which hands it out only while it belongs to this configuration.
type ServerCallTarget struct {
	Slug string
	// Name is the display name, for messages to the agent.
	Name       string
	URL        string
	Auth       domain.AuthMode
	TimeoutMs  int
	DailyLimit *int
}

// CatalogTool is a tool as a member sees it when browsing.
type CatalogTool struct {
	Ref         string
	Title       *string
	Description string
	Annotations domain.ToolAnnotations
}

// CatalogEntry is a server a person may use with its visible tools.
type CatalogEntry struct {
	Slug        string
	Name        string
	Description string
	Tools       []CatalogTool
}

// IndexedTool is one visible tool as the search index needs it.
type IndexedTool struct {
	Ref         string
	Name        string
	Title       *string
	Description string
	InputSchema json.RawMessage
}

// ServerIndexSource is a server and its searchable tools; Tools is empty for a disabled server.
type ServerIndexSource struct {
	Slug        string
	Name        string
	Description string
	Tools       []IndexedTool
}
