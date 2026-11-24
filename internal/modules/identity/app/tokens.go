package app

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

// TokenSummary is a token as listed to its owner; the hash never leaves the store.
type TokenSummary struct {
	ID         string
	Name       string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// CreatedToken is the one moment the full token value is known.
type CreatedToken struct {
	ID     string
	Token  kernel.Secret
	Prefix string
}

// CreateToken issues an sk_ token for the principal. Only its SHA-256 is stored.
func CreateToken(ctx context.Context, deps Deps, principal domain.Principal, name string) (CreatedToken, error) {
	random, err := deps.Random.Base64URL(32)
	if err != nil {
		return CreatedToken{}, err
	}
	token := domain.NewTokenValue(random)
	prefix := domain.TokenPrefix(token)
	record, err := deps.Tokens.Insert(ctx, NewToken{
		UserID:    principal.UserID,
		Name:      name,
		Prefix:    prefix,
		Hash:      deps.Hasher.Hex(token),
		CreatedAt: deps.Clock.Now(),
	})
	if err != nil {
		return CreatedToken{}, err
	}
	if err := deps.Audit.Record(ctx, kernel.AuditEntry{
		ActorID: ptr(principal.UserID),
		Action:  "token.create",
		Target:  ptr(record.ID),
		Meta:    map[string]any{"name": name, "prefix": prefix},
	}); err != nil {
		return CreatedToken{}, err
	}
	return CreatedToken{ID: record.ID, Token: kernel.NewSecret(token), Prefix: prefix}, nil
}

// ListTokens returns the principal's own tokens, newest first; never nil.
func ListTokens(ctx context.Context, deps Deps, principal domain.Principal) ([]TokenSummary, error) {
	rows, err := deps.Tokens.ListByUser(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]TokenSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, TokenSummary{
			ID: r.ID, Name: r.Name, Prefix: r.Prefix, CreatedAt: r.CreatedAt,
			LastUsedAt: r.LastUsedAt, RevokedAt: r.RevokedAt,
		})
	}
	return out, nil
}

// RevokeToken revokes a token of the principal, or of anyone for an admin. Revoking a revoked
// token succeeds without a second audit record.
func RevokeToken(ctx context.Context, deps Deps, principal domain.Principal, id string) error {
	token, err := deps.Tokens.FindByID(ctx, id)
	if err != nil {
		return err
	}
	if token == nil {
		return domain.Err(domain.CodeNotFound, "token not found")
	}
	if !domain.CanRevokeToken(principal, token.UserID) {
		return domain.Err(domain.CodeForbidden, "not your token")
	}
	if token.RevokedAt != nil {
		return nil // idempotent: nothing changed, nothing to audit
	}
	if err := deps.Tokens.Revoke(ctx, token.ID, deps.Clock.Now()); err != nil {
		return err
	}
	return deps.Audit.Record(ctx, kernel.AuditEntry{
		ActorID: ptr(principal.UserID),
		Action:  "token.revoke",
		Target:  ptr(token.ID),
	})
}

// AuthenticateToken checks an sk_ token presented by client ip. Failures (wrong shape, unknown,
// compare mismatch, revoked, owner gone or disabled) and only those are counted per ip: while the
// ip is within its budget the answer is invalid_token, afterwards rate_limited with a retry delay.
// A genuinely valid token is still accepted from a locked-out ip, since guessing 256 bits is not
// what the limiter protects against.
func AuthenticateToken(ctx context.Context, deps Deps, token, ip string) (domain.Principal, error) {
	reject := func() (domain.Principal, error) {
		allowed, retryAfter := deps.TokenLimiter.Hit(ip)
		if allowed {
			return domain.Principal{}, domain.Err(domain.CodeInvalidToken, "invalid token")
		}
		return domain.Principal{}, domain.ErrRetryAfter(domain.CodeRateLimited, "too many failed attempts", retryAfter)
	}

	if !domain.LooksLikeToken(token) {
		return reject()
	}
	hash := deps.Hasher.Hex(token)
	record, err := deps.Tokens.FindByHash(ctx, hash)
	if err != nil {
		return domain.Principal{}, err
	}
	if record == nil || !deps.Hasher.Equal(record.Hash, hash) || record.RevokedAt != nil {
		return reject()
	}
	user, err := deps.Users.FindByID(ctx, record.UserID)
	if err != nil {
		return domain.Principal{}, err
	}
	if user == nil || user.Disabled {
		return reject()
	}
	now := deps.Clock.Now()
	if domain.ShouldTouchLastUsed(record.LastUsedAt, now) {
		if err := deps.Tokens.Touch(ctx, record.ID, now); err != nil {
			return domain.Principal{}, err
		}
	}
	return domain.PrincipalOf(*user, domain.ViaToken, record.ID), nil
}
