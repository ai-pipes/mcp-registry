// Package connections is the public surface of the connections module: a person's credentials for
// the servers that take them (OAuth with PKCE, personal access tokens). The catalog's
// synchronisation, the gateway, the MCP layer and the CLI use the module only through this
// package; its REST routes are in the http subpackage.
package connections

import (
	"context"
	"errors"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
)

// The types other modules and the composition exchange with the module. They are aliases, so a
// value of one is a value of the module's own type.
type (
	// Principal is who is acting: the identity module's principal.
	Principal = app.Principal
	// Deps is what the use cases need: stores, the OAuth client, the directory of servers (over the
	// catalog's root package), the secret box, the clock, the audit log and plain values. The
	// composition fills it; New adds the single flight.
	Deps = app.Deps
	// ConnectionStore persists connections (the Postgres implementation is in infra).
	ConnectionStore = app.ConnectionStore
	// PendingConnects keeps started connect attempts.
	PendingConnects = app.PendingConnects
	// ServerDirectory is read access to the catalog's servers.
	ServerDirectory = app.ServerDirectory
	// OAuthClient is Authorization Code + PKCE against a provider.
	OAuthClient = app.OAuthClient
	// Pkce makes the random tokens and the challenge of the flow.
	Pkce = app.Pkce
	// ServerConfig is an active server's configuration as far as personal credentials care.
	ServerConfig = app.ServerConfig
	// PersonalServer is a server that takes a person's own credentials.
	PersonalServer = app.PersonalServer
	// TokenSet is what a provider gave back (prints redacted).
	TokenSet = app.TokenSet
	// ClientSecret is a server's OAuth client secret (prints redacted).
	ClientSecret = app.ClientSecret
	// AuthorizeInput is what AuthorizeURL needs besides the OAuth configuration.
	AuthorizeInput = app.AuthorizeInput
	// ExchangeInput is the code exchange of the callback.
	ExchangeInput = app.ExchangeInput
	// TokenEndpointError says the token endpoint answered with an error.
	TokenEndpointError = app.TokenEndpointError
	// ConnectionKey names a stored connection.
	ConnectionKey = app.ConnectionKey
	// LockedFunc is what runs while a connection is held.
	LockedFunc = app.LockedFunc
	// CredentialResult is a person's upstream secret for a server, or why there is none. It prints
	// redacted on every path: never log or marshal its Secret on purpose.
	CredentialResult = app.CredentialResult
	// CredentialFailure is why there is no usable personal secret.
	CredentialFailure = app.CredentialFailure
	// ResolveQuery asks for a person's secret for a server.
	ResolveQuery = app.ResolveQuery
	// MarkExpiredCommand names a connection upstream rejected.
	MarkExpiredCommand = app.MarkExpiredCommand
	// ConnectionSummary is one server of a person's list (TypeScript ConnectionSummary).
	ConnectionSummary = app.ConnectionSummary
	// SummaryStatus is the listed state of a connection.
	SummaryStatus = app.SummaryStatus
	// ResealSummary is what a key rotation did to people's connections.
	ResealSummary = app.ResealSummary
)

// The failures of ResolveCredential and the listed statuses.
const (
	// FailureNeedsConnection: never connected, or the server is gone or disabled.
	FailureNeedsConnection = app.FailureNeedsConnection
	// FailureExpired: connected but expired; the person connects again.
	FailureExpired = app.FailureExpired
	// FailureUnavailable: the OAuth provider could not be reached to refresh; try later.
	FailureUnavailable = app.FailureUnavailable

	SummaryReady   = app.SummaryReady
	SummaryExpired = app.SummaryExpired
	SummaryMissing = app.SummaryMissing
)

// ServerCallTarget is the catalog's description of a server to call.
type ServerCallTarget = app.ServerCallTarget

// ServerConfigOf is the configuration of a catalog server as the module fingerprints it: a caller
// that is about to send a resolved secret to a server sets ResolveQuery.Expect to it, built from the
// catalog target it read. It is the one mapping from a target to the fingerprint input.
func ServerConfigOf(t ServerCallTarget) ServerConfig { return app.ServerConfigOf(t) }

// NewClientSecret is a client secret (a public client is the zero ClientSecret).
func NewClientSecret(v string) ClientSecret { return app.NewClientSecret(v) }

// Service is the module's surface for the other modules, the CLI and the scheduler. It holds the
// dependencies privately and exactly ONE single flight, shared by every caller of
// ResolveCredential, so that one person's renewals for one server are joined in this process
// whoever asks (the gateway, the catalog's synchronisation). Build it once with New and share it;
// it is safe for concurrent use as far as its ports are.
type Service struct{ d app.Deps }

// New builds the service over the dependencies. Every field of Deps but Flights must be set: a
// missing one is an error naming it, so a half-wired composition fails at start and not on the
// first request. Deps.Flights is ignored: New creates the service's own single flight.
func New(d Deps) (*Service, error) {
	for _, c := range []struct {
		name string
		nil  bool
	}{
		{"Connections", d.Connections == nil},
		{"Pending", d.Pending == nil},
		{"Servers", d.Servers == nil},
		{"OAuth", d.OAuth == nil},
		{"Pkce", d.Pkce == nil},
		{"Box", d.Box == nil},
		{"Clock", d.Clock == nil},
		{"Audit", d.Audit == nil},
		{"RedirectURI", d.RedirectURI == ""},
	} {
		if c.nil {
			return nil, errors.New("connections: Deps." + c.name + " is not set")
		}
	}
	d.Flights = app.NewSingleFlight[app.CredentialResult]()
	return &Service{d: d}, nil
}

// ResolveCredential is the person's upstream secret for a server: the OAuth access token or the
// PAT, raw, refreshed ahead of expiry or when ForceRefresh is set (the gateway sets it after an
// upstream 401). A secret issued for another configuration of the server is never handed out: the
// connection expires instead. The answers a caller acts on are CredentialFailure values with a nil
// error (needs_connection, expired, unavailable); the error is for unexpected failures. A caller
// whose ctx ends while it waits for a renewal returns ctx.Err() and the renewal goes on without it.
func (s *Service) ResolveCredential(ctx context.Context, q ResolveQuery) (CredentialResult, error) {
	return app.ResolveCredential(ctx, s.d, q)
}

// MarkExpired turns a ready connection expired because upstream rejected its credential (the
// gateway decides when: a PAT at once, an OAuth token whose just-refreshed replacement was
// rejected too). An unknown or already expired connection is left alone.
func (s *Service) MarkExpired(ctx context.Context, c MarkExpiredCommand) error {
	return app.MarkExpired(ctx, s.d, c)
}

// ListConnections is every active oauth_user and pat_user server the person may use, ordered by
// slug, with the state of their connection (ready, expired, missing). The MCP layer's list_servers
// maps it to the agent's view (TypeScript AgentConnection, which lives in gateway/domain).
func (s *Service) ListConnections(ctx context.Context, p Principal) ([]ConnectionSummary, error) {
	return app.ListConnections(ctx, s.d, app.ListQuery{Principal: p})
}

// ResealConnections re-seals every stored token from one key to the other (the CLI's
// rotate-secrets-key). No audit record: the caller records the rotation.
func (s *Service) ResealConnections(ctx context.Context, from, to kernel.SecretBox) (ResealSummary, error) {
	return app.ResealConnections(ctx, s.d.Connections, from, to)
}
