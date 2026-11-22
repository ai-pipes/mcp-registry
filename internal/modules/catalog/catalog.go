package catalog

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// The types other modules exchange with the catalog. They are aliases, so a value of one is a
// value of the module's own type.
type (
	// Principal is who is acting: the identity module's principal.
	Principal = app.Principal
	// Deps is what the use cases need: repositories, the secret box, the clock, the audit log, the
	// upstream client, the credentials port and plain values. The composition fills it.
	Deps = app.Deps
	// ServerView is the admin's view of a server (SecretSet only, never a secret).
	ServerView = app.ServerView
	// MemberServerView is a server as a member sees it: no URL, no auth settings.
	MemberServerView = app.MemberServerView
	// ToolView is a stored tool with its schemas.
	ToolView = app.ToolView
	// ServerCallTarget is what the gateway needs to call a server (no secret: GetServerSecret).
	ServerCallTarget = app.ServerCallTarget
	// Entry is a server a person may browse, with its visible tools (TypeScript CatalogEntry).
	Entry = app.CatalogEntry
	// EntryTool is a tool of an Entry (TypeScript CatalogTool).
	EntryTool = app.CatalogTool
	// IndexedTool is a tool as the search index needs it.
	IndexedTool = app.IndexedTool
	// ServerIndexSource is a server and its searchable tools.
	ServerIndexSource = app.ServerIndexSource
	// SyncCredentials is the port the connections module implements for the synchronisation of
	// a server that authenticates per person.
	SyncCredentials = app.SyncCredentials
	// PersonalCredential is the answer of SyncCredentials.PersonalSecret.
	PersonalCredential = app.PersonalCredential
	// PersonalCredentialFailure is why a personal credential could not be had.
	PersonalCredentialFailure = domain.PersonalCredentialFailure
	// SyncCommand names one server to synchronise; a nil Principal is the scheduler or the CLI
	// without --as.
	SyncCommand = app.SyncCommand
	// SyncCounts is what a synchronisation changed.
	SyncCounts = app.SyncCounts
	// SyncOutcome is the result of one server in SyncAllServers.
	SyncOutcome = app.SyncOutcome
	// ResealSummary is what a key rotation did to the server secrets.
	ResealSummary = app.ResealSummary
	// CreateServerCommand creates (and registers) a server.
	CreateServerCommand = app.CreateServerCommand
	// ServerInput is a server as the REST contract and the CLI's server file carry it.
	ServerInput = app.ServerInput
	// ServerFields are the editable fields of a server.
	ServerFields = app.ServerFields
	// Issue is one breach of the REST contract: the dotted path and the reason.
	Issue = app.Issue
	// AuthMode is how the gateway authenticates to a server's upstream.
	AuthMode = domain.AuthMode
	// AuthKind is the kind of an AuthMode.
	AuthKind = domain.AuthKind
	// OAuthClientConfig is the OAuth client of an oauth_user server (no client secret).
	OAuthClientConfig = domain.OAuthClientConfig
	// AuthorizeParam is one extra query parameter of the authorize request.
	AuthorizeParam = domain.AuthorizeParam
	// Access says who may use a server.
	Access = domain.Access
	// AccessKind is all or groups.
	AccessKind = domain.AccessKind
	// CredentialSource says where the upstream credential of an auth mode comes from.
	CredentialSource = domain.CredentialSource
	// ServerStatus is active or disabled.
	ServerStatus = domain.ServerStatus
	// ToolAnnotations are the four boolean MCP hints of a tool.
	ToolAnnotations = domain.ToolAnnotations
)

// The kinds of AuthMode, the reasons a personal credential is missing and the statuses.
const (
	AuthNone         = domain.AuthNone
	AuthSharedSecret = domain.AuthSharedSecret
	AuthOAuthUser    = domain.AuthOAuthUser
	AuthPATUser      = domain.AuthPATUser

	CredentialNeedsConnection = domain.CredentialNeedsConnection
	CredentialExpired         = domain.CredentialExpired
	CredentialUnavailable     = domain.CredentialUnavailable

	AccessAll    = domain.AccessAll
	AccessGroups = domain.AccessGroups

	SourceNone         = domain.SourceNone
	SourceServerSecret = domain.SourceServerSecret
	SourcePersonal     = domain.SourcePersonal

	StatusActive   = domain.StatusActive
	StatusDisabled = domain.StatusDisabled
)

// The error codes the module answers with (kernel.DomainError.Code).
const (
	CodeNotFound        = domain.CodeNotFound
	CodeInvalidToolRef  = domain.CodeInvalidToolRef
	CodeForbidden       = domain.CodeForbidden
	CodeNeedsConnection = domain.CodeNeedsConnection
	CodeSyncFailed      = domain.CodeSyncFailed
	CodeInvalidRequest  = app.CodeInvalidRequest
)

// HeaderFor is the header that authenticates to the upstream for a secret (an access token, a
// PAT or the server's own secret): the one place a header is built. Empty for AuthNone.
func HeaderFor(m AuthMode, secret string) kernel.UpstreamHeaders { return domain.HeaderFor(m, secret) }

// ParseToolRef splits "<server>.<tool>" at the first dot; invalid_tool_ref when it does not look
// like that.
func ParseToolRef(ref string) (server, tool string, err error) { return domain.ParseToolRef(ref) }

// FormatToolRef is the inverse of ParseToolRef.
func FormatToolRef(server, tool string) string { return domain.FormatToolRef(server, tool) }

// The outbox events of the catalog, delivered to the search module. Payloads travel as JSON, so a
// handler reads them with ParseToolsEvent and ParseServerEvent.
const (
	// EventToolsChanged: tools were added, changed, revived, hidden or unhidden. Payload {server, refs}.
	EventToolsChanged = domain.EventToolsChanged
	// EventToolsRemoved: upstream no longer offers these tools. Payload {server, refs}.
	EventToolsRemoved = domain.EventToolsRemoved
	// EventServerChanged: the name, description, access or status of a server changed, or it was
	// created. Payload {server}.
	EventServerChanged = domain.EventServerChanged
)

// ToolsEvent is the payload of EventToolsChanged and EventToolsRemoved.
type ToolsEvent struct {
	Server string
	Refs   []string
}

// ParseToolsEvent reads the payload of a tools_changed or tools_removed event as it comes out of
// the outbox (refs may be []string or []any). ok is false for a payload of another shape.
func ParseToolsEvent(payload map[string]any) (ev ToolsEvent, ok bool) {
	server, ok := payload["server"].(string)
	if !ok {
		return ToolsEvent{}, false
	}
	ev = ToolsEvent{Server: server, Refs: []string{}}
	switch refs := payload["refs"].(type) {
	case []string:
		ev.Refs = append(ev.Refs, refs...)
	case []any:
		for _, r := range refs {
			s, isString := r.(string)
			if !isString {
				return ToolsEvent{}, false
			}
			ev.Refs = append(ev.Refs, s)
		}
	default:
		return ToolsEvent{}, false
	}
	return ev, true
}

// ParseServerEvent reads the payload of a server_changed event: the slug.
func ParseServerEvent(payload map[string]any) (server string, ok bool) {
	server, ok = payload["server"].(string)
	return server, ok
}

// Service is the catalog's surface for the other modules, the CLI and the scheduler. It holds the
// module's dependencies privately, since callers cannot name the application layer. Build it once
// with New and share it; it is safe for concurrent use as far as its ports are.
type Service struct{ d app.Deps }

// New builds the service over the module's dependencies. Deps.Credentials must not be nil.
func New(d Deps) *Service { return &Service{d: d} }

// ListServersFor is the active servers the person may use (the connections module's directory,
// the gateway's list_servers).
func (s *Service) ListServersFor(ctx context.Context, p Principal) ([]MemberServerView, error) {
	return app.ListServersFor(ctx, s.d, p)
}

// GetServerForUpstream is the call parameters of an active server; nil when unknown or disabled.
func (s *Service) GetServerForUpstream(ctx context.Context, slug string) (*ServerCallTarget, error) {
	return app.GetServerForUpstream(ctx, s.d, slug)
}

// GetServerSecret is the opened secret of a server (the shared key, or the OAuth client secret);
// ok is false when there is none. A secret no key opens is an error.
func (s *Service) GetServerSecret(ctx context.Context, slug string) (secret string, ok bool, err error) {
	return app.GetServerSecret(ctx, s.d, slug)
}

// GetToolFor is a tool the person may use right now; one not_found for everything else, a
// malformed ref is invalid_tool_ref.
func (s *Service) GetToolFor(ctx context.Context, p Principal, ref string) (ToolView, error) {
	return app.GetToolFor(ctx, s.d, p, ref)
}

// ListCatalogFor is what a person may browse.
func (s *Service) ListCatalogFor(ctx context.Context, p Principal) ([]Entry, error) {
	return app.ListCatalogFor(ctx, s.d, p)
}

// ListToolsForIndex is what the search module indexes for a server; nil when unknown, no tools
// when disabled.
func (s *Service) ListToolsForIndex(ctx context.Context, slug string) (*ServerIndexSource, error) {
	return app.ListToolsForIndex(ctx, s.d, slug)
}

// ListServerSlugs is the slug of every server, disabled ones included.
func (s *Service) ListServerSlugs(ctx context.Context) ([]string, error) {
	return app.ListServerSlugs(ctx, s.d)
}

// ErrFirstSyncInterrupted is returned by RegisterServer, with the created server, when the context ends during the first sync.
var ErrFirstSyncInterrupted = app.ErrFirstSyncInterrupted

// SyncServer synchronises one server with its upstream: counts, or a DomainError (needs_connection,
// sync_failed, not_found, forbidden...).
func (s *Service) SyncServer(ctx context.Context, cmd SyncCommand) (SyncCounts, error) {
	return app.SyncServer(ctx, s.d, cmd)
}

// SyncAllServers synchronises every active server in slug order, as the scheduler does.
func (s *Service) SyncAllServers(ctx context.Context) ([]SyncOutcome, error) {
	return app.SyncAllServers(ctx, s.d)
}

// RegisterServer creates a server and synchronises it at once; a failed synchronisation is
// recorded in the returned view's LastSyncError and is not an error. A context that ends during
// the first sync returns the created server's view with ErrFirstSyncInterrupted.
func (s *Service) RegisterServer(ctx context.Context, cmd CreateServerCommand) (ServerView, error) {
	return app.RegisterServer(ctx, s.d, cmd)
}

// ResealServerSecrets re-seals every stored server secret from one key to the other (the CLI's
// rotate-secrets-key).
func (s *Service) ResealServerSecrets(ctx context.Context, from, to kernel.SecretBox) (ResealSummary, error) {
	return app.ResealServerSecrets(ctx, s.d.Servers, from, to)
}

// ValidateServerInput lists every breach of the contract of a server file (the CLI's
// create-server) in schema order; the missing-field problems come from the shape step of the
// caller.
func ValidateServerInput(in ServerInput) []Issue { return app.ValidateServerInput(in) }

// EventTypes lists every outbox event type the catalog emits (domain.EventTypes). The composition
// checks that the outbox handler table has a handler for each of them (an event of an unhandled
// type is marked processed by the worker and lost). The catalog's repositories refuse an event
// whose type is not in the list, so it cannot go stale unnoticed.
func EventTypes() []string { return domain.EventTypes() }
