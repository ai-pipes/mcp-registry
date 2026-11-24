package app

import (
	"context"
	"net/url"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

// Lookups return (nil, nil) when nothing matches; an error means the store failed.

// OIDCAuthorizationInput is what the provider needs to build an authorization URL (Code + PKCE S256).
// State and nonce are secrets of the attempt (kernel.Secret); the challenge is public.
type OIDCAuthorizationInput struct {
	State         kernel.Secret
	CodeChallenge string
	Nonce         kernel.Secret
}

// OIDCExchangeInput is a callback's code with the secrets of the attempt it belongs to, each a
// kernel.Secret.
type OIDCExchangeInput struct {
	Code         kernel.Secret
	CodeVerifier kernel.Secret
	Nonce        kernel.Secret
	// Iss is the iss parameter of the callback when the provider sent one (RFC 9207); nil otherwise.
	Iss *string
}

// OIDCProvider talks to the identity provider: Authorization Code + PKCE. Both methods return a
// *domain.Error with code idp_unavailable while the provider cannot be reached; the server itself,
// /mcp and token auth never wait for it. Any other error means the exchange was rejected.
type OIDCProvider interface {
	AuthorizationURL(ctx context.Context, in OIDCAuthorizationInput) (*url.URL, error)
	// Exchange trades the code for tokens, checks the ID token (signature, nonce) and returns the claims.
	Exchange(ctx context.Context, in OIDCExchangeInput) (domain.OIDCClaims, error)
}

// RateLimiter counts failures per key (a client IP).
type RateLimiter interface {
	// Hit counts one failure and says whether the key is still allowed; retryAfterSec is the
	// remaining lock in seconds when it is not.
	Hit(key string) (allowed bool, retryAfterSec int)
	// Check does not count: is the key currently locked out?
	Check(key string) (allowed bool, retryAfterSec int)
}

// Hasher is SHA-256 behind a port (crypto lives in infrastructure).
type Hasher interface {
	// Hex is the lower-case hex SHA-256 of a UTF-8 string.
	Hex(s string) string
	// Base64URL is the unpadded base64url SHA-256 of a UTF-8 string (the PKCE S256 challenge).
	Base64URL(s string) string
	// Equal compares two strings in constant time.
	Equal(a, b string) bool
}

// Random is the source of secrets.
type Random interface {
	// Base64URL is nBytes random bytes as an unpadded base64url string.
	Base64URL(nBytes int) (string, error)
}

// TokenRecord is a stored API token; only its hash is kept.
type TokenRecord struct {
	ID         string
	UserID     string
	Name       string
	Prefix     string
	Hash       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// NewToken is a token about to be stored.
type NewToken struct {
	UserID    string
	Name      string
	Prefix    string
	Hash      string
	CreatedAt time.Time
}

// SessionRecord is a stored web session, keyed by the SHA-256 of its id.
type SessionRecord struct {
	IDHash     string
	UserID     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// PendingLogin is a started, not yet completed OIDC login. One-time: Consume removes it. Its state,
// verifier and nonce are kernel.Secret.
type PendingLogin struct {
	State        kernel.Secret
	CodeVerifier kernel.Secret
	Nonce        kernel.Secret
	ReturnTo     string
	ExpiresAt    time.Time
}

// LoginLinkRecord is a one-time sign-in link. Only the SHA-256 of the code is stored.
type LoginLinkRecord struct {
	CodeHash  string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// UpsertLogin is the input of UserRepository.UpsertFromLogin.
type UpsertLogin struct {
	Claims domain.OIDCClaims
	Role   domain.Role
	At     time.Time
}

// UserRepository stores people.
type UserRepository interface {
	FindByID(ctx context.Context, id string) (*domain.User, error)
	FindBySub(ctx context.Context, sub string) (*domain.User, error)
	// UpsertFromLogin creates the person or refreshes name, email, groups and role; it never
	// touches Disabled. On insert CreatedAt and LastLoginAt both get At; on update LastLoginAt does.
	UpsertFromLogin(ctx context.Context, in UpsertLogin) (domain.User, error)
	// List returns everyone ordered by creation time, then name.
	List(ctx context.Context) ([]domain.User, error)
	// SetDisabled reports whether the person exists.
	SetDisabled(ctx context.Context, id string, disabled bool) (bool, error)
	// TouchLastLogin records a sign-in time only; everything else stays as it is.
	TouchLastLogin(ctx context.Context, id string, at time.Time) error
}

// TokenRepository stores API tokens.
type TokenRepository interface {
	Insert(ctx context.Context, t NewToken) (TokenRecord, error)
	FindByHash(ctx context.Context, hash string) (*TokenRecord, error)
	FindByID(ctx context.Context, id string) (*TokenRecord, error)
	// ListByUser returns the person's tokens, newest first.
	ListByUser(ctx context.Context, userID string) ([]TokenRecord, error)
	// Revoke sets RevokedAt unless it is already set.
	Revoke(ctx context.Context, id string, at time.Time) error
	// RevokeAllByUser revokes only the person's tokens that are not revoked yet: RevokedAt of
	// earlier revocations is preserved (SQL: "and revoked_at is null").
	RevokeAllByUser(ctx context.Context, userID string, at time.Time) error
	Touch(ctx context.Context, id string, at time.Time) error
}

// SessionRepository stores web sessions.
type SessionRepository interface {
	Insert(ctx context.Context, s SessionRecord) error
	FindByHash(ctx context.Context, idHash string) (*SessionRecord, error)
	Touch(ctx context.Context, idHash string, at time.Time) error
	Delete(ctx context.Context, idHash string) error
	DeleteByUser(ctx context.Context, userID string) error
	// DeleteDead deletes every session past either cutoff and returns how many.
	DeleteDead(ctx context.Context, c domain.SessionCutoffs) (int, error)
}

// PendingLoginRepository stores started OIDC logins.
type PendingLoginRepository interface {
	Insert(ctx context.Context, p PendingLogin) error
	// Consume atomically removes and returns the record; nil if unknown or already consumed.
	Consume(ctx context.Context, state string) (*PendingLogin, error)
}

// LoginLinkRepository stores one-time sign-in links.
type LoginLinkRepository interface {
	Insert(ctx context.Context, l LoginLinkRecord) error
	// Consume atomically removes and returns the record; nil if unknown or already consumed.
	Consume(ctx context.Context, codeHash string) (*LoginLinkRecord, error)
	DeleteByUser(ctx context.Context, userID string) error
	// DeleteExpired deletes links with ExpiresAt <= now and returns how many.
	DeleteExpired(ctx context.Context, now time.Time) (int, error)
}

// Deps is everything the identity use cases need.
type Deps struct {
	Users         UserRepository
	Tokens        TokenRepository
	Sessions      SessionRepository
	PendingLogins PendingLoginRepository
	LoginLinks    LoginLinkRepository
	OIDC          OIDCProvider
	Policy        domain.GroupPolicy
	Hasher        Hasher
	Random        Random
	TokenLimiter  RateLimiter
	LinkLimiter   RateLimiter
	// CallbackLimiter counts failed OIDC code exchanges per client IP; it only decides whether to audit one.
	CallbackLimiter RateLimiter
	Audit           kernel.AuditLog
	Clock           kernel.Clock
}

func ptr[T any](v T) *T { return &v }
