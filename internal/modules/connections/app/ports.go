package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

// Principal is who is acting: the identity module's principal.
type Principal = identity.Principal

const redacted = "[redacted]"

// TokenSet is what the OAuth provider gave back: raw tokens, so it prints redacted on every path.
// RefreshToken is empty when the provider sent none; ExpiresAt is nil when it did not say.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
}

// String is "[redacted]".
func (TokenSet) String() string { return redacted }

// GoString is "[redacted]".
func (TokenSet) GoString() string { return redacted }

// Format prints "[redacted]" for every verb.
func (TokenSet) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue is the slog form.
func (TokenSet) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON writes "[redacted]".
func (TokenSet) MarshalJSON() ([]byte, error) { return redactedJSON, nil }

// MarshalText writes "[redacted]".
func (TokenSet) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// redactedJSON is the JSON form of every value that carries a secret.
var redactedJSON = []byte(`"` + redacted + `"`)

// ClientSecret is the OAuth client secret of a server. The zero value is a public client (none).
// It prints redacted on every path; read it with Value.
type ClientSecret struct {
	value string
	set   bool
}

// NewClientSecret is a client secret (a public client is the zero value, not an empty secret).
func NewClientSecret(v string) ClientSecret { return ClientSecret{value: v, set: true} }

// Value is the secret and whether there is one.
func (s ClientSecret) Value() (string, bool) { return s.value, s.set }

// String is "[redacted]".
func (ClientSecret) String() string { return redacted }

// GoString is "[redacted]".
func (ClientSecret) GoString() string { return redacted }

// Format prints "[redacted]" for every verb.
func (ClientSecret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue is the slog form.
func (ClientSecret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON writes "[redacted]".
func (ClientSecret) MarshalJSON() ([]byte, error) { return redactedJSON, nil }

// MarshalText writes "[redacted]".
func (ClientSecret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// AuthorizeInput is what AuthorizeURL needs besides the server's OAuth configuration. State is a
// secret (it is the one-time proof of the callback), so it prints redacted.
type AuthorizeInput struct {
	RedirectURI   string
	State         string
	CodeChallenge string
}

// Format prints the redirect URI only.
func (a AuthorizeInput) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "AuthorizeInput{redirectURI:%s}", a.RedirectURI)
}

// LogValue is the slog form: the redirect URI only.
func (a AuthorizeInput) LogValue() slog.Value {
	return slog.GroupValue(slog.String("redirectURI", a.RedirectURI))
}

// MarshalJSON writes "[redacted]".
func (AuthorizeInput) MarshalJSON() ([]byte, error) { return redactedJSON, nil }

// MarshalText writes "[redacted]".
func (AuthorizeInput) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// ExchangeInput is the code exchange of the callback. Code and CodeVerifier are secrets, so it
// prints redacted. RedirectURI is exactly the one sent with the authorize request.
type ExchangeInput struct {
	Code         string
	CodeVerifier string
	RedirectURI  string
}

// Format prints the redirect URI only.
func (e ExchangeInput) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "ExchangeInput{redirectURI:%s}", e.RedirectURI)
}

// LogValue is the slog form: the redirect URI only.
func (e ExchangeInput) LogValue() slog.Value {
	return slog.GroupValue(slog.String("redirectURI", e.RedirectURI))
}

// MarshalJSON writes "[redacted]".
func (ExchangeInput) MarshalJSON() ([]byte, error) { return redactedJSON, nil }

// MarshalText writes "[redacted]".
func (ExchangeInput) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// TokenEndpointError says the token endpoint ANSWERED a request with an error: its HTTP status and
// the OAuth `error` code ("" when the answer had none). OAuthClient returns it (possibly wrapped:
// callers use errors.As) for such answers; any other error from Refresh means there was no usable
// answer (timeout, network, a malformed or 200-with-bad-body answer). The distinction decides
// whether a failed refresh ends the grant: domain.IsRefreshRejected.
type TokenEndpointError struct {
	Status int
	Code   string
}

// Error is `token endpoint answered HTTP <status>` with ` (<code>)` when there is a code.
func (e *TokenEndpointError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("token endpoint answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("token endpoint answered HTTP %d (%s)", e.Status, e.Code)
}

// OAuthClient is Authorization Code + PKCE (S256) against the upstream's OAuth provider. The
// implementation owns the 10 s timeout of each request to the provider (a refresh runs under the
// row lock, so it bounds other replicas' wait) and never lets a token, code, state, verifier or
// the client secret into an error text.
type OAuthClient interface {
	// AuthorizeURL is the provider URL to send the person to. It fails for a configuration it
	// cannot use (the use case drops the cause: an error here must not carry the state).
	AuthorizeURL(cfg catalog.OAuthClientConfig, in AuthorizeInput) (string, error)
	// ExchangeCode swaps the code for tokens. Any error means the provider did not authorise.
	ExchangeCode(ctx context.Context, cfg catalog.OAuthClientConfig, secret ClientSecret, in ExchangeInput) (TokenSet, error)
	// Refresh returns *TokenEndpointError for an error answer, any other error for no answer.
	Refresh(ctx context.Context, cfg catalog.OAuthClientConfig, secret ClientSecret, refreshToken string) (TokenSet, error)
	// Revoke is RFC 7009; it is only called when the connection may be revoked at the server's
	// revoke URL. An error is ignored by the caller.
	Revoke(ctx context.Context, cfg catalog.OAuthClientConfig, secret ClientSecret, token string) error
}

// ServerConfig is an active server's configuration as far as personal credentials care. No secret.
// OAuth is set exactly for oauth_user servers.
type ServerConfig struct {
	Slug     string
	URL      string
	AuthKind catalog.AuthKind
	OAuth    *catalog.OAuthClientConfig
}

// PersonalServer is a server that takes a person's own credentials.
type PersonalServer struct {
	Slug     string
	Name     string
	AuthKind catalog.AuthKind // AuthOAuthUser or AuthPATUser
	Config   ServerConfig
}

// ServerDirectory is read access to the catalog, implemented over its root package
// (catalog.Service.ListServersFor, GetServerForUpstream, GetServerSecret).
type ServerDirectory interface {
	// PersonalServersFor is the active oauth_user and pat_user servers the person may use,
	// ordered by slug (an admin may use any active one).
	PersonalServersFor(ctx context.Context, p Principal) ([]PersonalServer, error)
	// Config is the configuration of an active server; nil when unknown or disabled.
	Config(ctx context.Context, slug string) (*ServerConfig, error)
	// ClientSecret is the server's OAuth client secret, opened; the zero value when unset (any
	// server status). An error means no key opens it. It never leaves the module.
	ClientSecret(ctx context.Context, slug string) (ClientSecret, error)
}

// ConnectionKey names a stored connection.
type ConnectionKey struct {
	UserID string
	Server string
}

// LockedFunc is what runs while a connection is held. current is the row as read UNDER the lock
// (nil when there is none). The returned connection, when not nil, is written in the same
// transaction before the lock is released (it must have the locked key); nil writes nothing. An
// error rolls back and is returned by WithLock.
//
// The callback receives only the row and the context: under the lock the use cases touch nothing
// but that row and the provider's HTTP request (the lock holds a database connection, and asking
// the pool for a second one under it can starve the pool). The callbacks in this package are built
// from values read before the lock and report what they decided through their closure; the audit
// record is written after WithLock returns.
type LockedFunc func(ctx context.Context, current *domain.Connection) (write *domain.Connection, err error)

// ConnectionStore persists connections. Tokens arrive and leave sealed (the aggregate holds no
// plain text).
type ConnectionStore interface {
	// Get returns the connection (nil, nil when there is none). It takes no lock.
	Get(ctx context.Context, userID, server string) (*domain.Connection, error)
	// ListForUser returns the person's connections ordered by server.
	ListForUser(ctx context.Context, userID string) ([]*domain.Connection, error)
	// WithLock runs fn while holding the (userID, server) connection exclusively across processes,
	// not only in this one (Postgres: one transaction, `select ... for update`; a missing row locks
	// nothing but inserts of the same key still serialise). See LockedFunc.
	WithLock(ctx context.Context, userID, server string, fn LockedFunc) error
	// Delete removes the connection and returns what was there (nil, nil when nothing was).
	Delete(ctx context.Context, userID, server string) (*domain.Connection, error)
	// ListKeys is every stored connection's key, ordered by user then server (SECRETS_KEY rotation).
	ListKeys(ctx context.Context) ([]ConnectionKey, error)
}

// PendingConnects keeps started connect attempts. An attempt is one-time and belongs to its starter.
type PendingConnects interface {
	// Insert stores a started attempt.
	Insert(ctx context.Context, p domain.PendingConnect) error
	// Consume ATOMICALLY removes and returns the attempt with this state that userID started
	// (Postgres: `delete ... where state = $1 and user_id = $2 returning *`), so of two concurrent
	// callbacks with one state exactly one gets it. Nothing is removed, and nil is returned, when
	// there is no such attempt or it belongs to another person: the starter can still complete it
	// (the decided deviation oauth-state-kept). An expired attempt of userID is removed and
	// returned; the use case then refuses it.
	Consume(ctx context.Context, state, userID string) (*domain.PendingConnect, error)
}

// Pkce makes the random tokens and the challenge of Authorization Code + PKCE.
type Pkce interface {
	// RandomToken is a random URL-safe string (43 characters of base64url, no padding): state and
	// code verifier.
	RandomToken() (string, error)
	// Challenge is BASE64URL(SHA-256(verifier)) (RFC 7636 S256).
	Challenge(verifier string) string
}

// Deps is what the use cases need. The composition fills it.
type Deps struct {
	Connections ConnectionStore
	Pending     PendingConnects
	Servers     ServerDirectory
	OAuth       OAuthClient
	Pkce        Pkce
	Box         kernel.SecretBox
	// Flights joins concurrent renewals of one (person, server) in this process. Build it once
	// with NewSingleFlight and share it between all uses of the Deps.
	Flights *SingleFlight[CredentialResult]
	Clock   kernel.Clock
	Audit   kernel.AuditLog
	// RedirectURI is <PUBLIC_URL>/api/connections/callback.
	RedirectURI string
}
