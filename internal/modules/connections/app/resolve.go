package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

// CredentialFailure is why there is no usable personal secret.
type CredentialFailure string

// The failures of ResolveCredential.
const (
	// FailureNeedsConnection: never connected, or the server is gone (or disabled).
	FailureNeedsConnection CredentialFailure = "needs_connection"
	// FailureExpired: connected but expired; the person connects again.
	FailureExpired CredentialFailure = "expired"
	// FailureUnavailable: the OAuth provider could not be reached to refresh; try later.
	FailureUnavailable CredentialFailure = "unavailable"
)

// CredentialResult is the person's upstream secret for a server (the OAuth access token or the
// PAT, raw) or the reason there is none (Failure set, Secret empty). It prints redacted.
type CredentialResult struct {
	Secret  string
	Failure CredentialFailure
}

// Ok reports whether there is a secret.
func (r CredentialResult) Ok() bool { return r.Failure == "" }

func (r CredentialResult) text() string {
	if r.Failure != "" {
		return "CredentialResult{failure:" + string(r.Failure) + "}"
	}
	return "CredentialResult{secret:" + redacted + "}"
}

// String prints the failure, or "[redacted]" in place of the secret.
func (r CredentialResult) String() string { return r.text() }

// GoString is String.
func (r CredentialResult) GoString() string { return r.text() }

// Format prints String for every verb.
func (r CredentialResult) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, r.text()) }

// MarshalJSON writes the text form: the failure, or "[redacted]" in place of the secret.
func (r CredentialResult) MarshalJSON() ([]byte, error) { return json.Marshal(r.text()) }

// MarshalText writes the text form.
func (r CredentialResult) MarshalText() ([]byte, error) { return []byte(r.text()), nil }

// LogValue is the slog form.
func (r CredentialResult) LogValue() slog.Value { return slog.StringValue(r.text()) }

func ok(secret string) CredentialResult { return CredentialResult{Secret: secret} }

func failed(f CredentialFailure) CredentialResult { return CredentialResult{Failure: f} }

// errOpen is returned when a stored token cannot be opened (no key opens it). The cause is not
// kept: it is the box's text about a token.
var errOpen = errors.New("connections: a stored token cannot be opened with the configured key")

func openToken(box kernel.SecretBox, sealed domain.Sealed) (string, error) {
	plain, err := box.Open([]byte(sealed))
	if err != nil {
		return "", errOpen
	}
	return plain, nil
}

// ResolveQuery asks for a person's secret for a server. ForceRefresh is set when upstream rejected
// the access token just handed out.
type ResolveQuery struct {
	UserID       string
	Server       string
	ForceRefresh bool
}

// ResolveCredential is the person's upstream secret for the server: the OAuth access token or the
// PAT, raw. It refreshes ahead of expiry or when ForceRefresh. One refresh per (person, server)
// at a time: in this process by the single flight, across processes by the connection lock, where
// the decision is taken again on the locked row (another process's fresh token is reused, even for
// a forced refresh). A secret issued for another server configuration (URL, auth kind, OAuth
// provider) is never handed out: the connection expires instead.
//
// Order: connection, server config (outside any lock) -> plan -> `use` and `expired` answer at
// once -> otherwise the shared renewal: client secret (before the lock), the lock with the
// provider call and the write, the audit record after the commit.
//
// The error is for unexpected failures (a store, a stored token no key opens, the audit log); the
// answers a caller acts on are CredentialFailure values with a nil error. A caller whose ctx ends
// while it waits for a renewal returns ctx.Err() and the renewal goes on without it.
func ResolveCredential(ctx context.Context, d Deps, q ResolveQuery) (CredentialResult, error) {
	seen, err := d.Connections.Get(ctx, q.UserID, q.Server)
	if err != nil {
		return CredentialResult{}, err
	}
	if seen == nil {
		return failed(FailureNeedsConnection), nil
	}
	cfg, err := d.Servers.Config(ctx, q.Server)
	if err != nil {
		return CredentialResult{}, err
	}
	if cfg == nil {
		return failed(FailureNeedsConnection), nil
	}
	fp := fingerprint(*cfg)
	switch plan := seen.CredentialPlan(d.Clock.Now(), domain.PlanOptions{ForceRefresh: q.ForceRefresh, Config: fp}); plan.Action {
	case domain.PlanUse:
		secret, err := openToken(d.Box, seen.AccessToken())
		if err != nil {
			return CredentialResult{}, err
		}
		return ok(secret), nil
	case domain.PlanExpired:
		return failed(FailureExpired), nil
	case domain.PlanRefresh, domain.PlanExpire:
	}
	in := renewal{seen: seen, cfg: *cfg, config: fp, forceRefresh: q.ForceRefresh}
	return d.Flights.Do(ctx, q.UserID+"\x00"+q.Server, func(fctx context.Context) (CredentialResult, error) {
		return renewUnderLock(fctx, d, in)
	})
}

type renewal struct {
	seen         *domain.Connection
	cfg          ServerConfig
	config       string
	forceRefresh bool
}

// locked is what the lock callback decided; the audit record is written after the commit.
type locked struct {
	result  CredentialResult
	expired domain.ExpiryReason
}

// lockEnv is everything the lock callback may use besides the row: the secret box, the clock and
// the provider. It holds no catalog, audit or other store, so the callback cannot reach them.
type lockEnv struct {
	box    kernel.SecretBox
	clock  kernel.Clock
	oauth  OAuthClient
	secret ClientSecret
}

// renewUnderLock renews `seen` under the connection lock. The client secret is read before the
// lock and the audit record is written after it.
func renewUnderLock(ctx context.Context, d Deps, in renewal) (CredentialResult, error) {
	var secret ClientSecret
	if in.cfg.OAuth != nil {
		var err error
		if secret, err = d.Servers.ClientSecret(ctx, in.cfg.Slug); err != nil {
			return CredentialResult{}, fmt.Errorf("connections: read the OAuth client secret of %q: %w", in.cfg.Slug, err)
		}
	}
	env := lockEnv{box: d.Box, clock: d.Clock, oauth: d.OAuth, secret: secret}
	var out locked
	err := d.Connections.WithLock(ctx, in.seen.UserID(), in.seen.Server(), func(lctx context.Context, current *domain.Connection) (*domain.Connection, error) {
		var write *domain.Connection
		var err error
		out, write, err = decideRenewal(lctx, env, in, current)
		return write, err
	})
	if err != nil {
		return CredentialResult{}, err
	}
	if out.expired != "" {
		if err := recordExpired(ctx, d.Audit, in.seen.UserID(), in.seen.Server(), out.expired); err != nil {
			return CredentialResult{}, err
		}
	}
	return out.result, nil
}

func failureOf(err error) *domain.RefreshFailure {
	var te *TokenEndpointError
	if errors.As(err, &te) {
		return &domain.RefreshFailure{Status: te.Status, Error: te.Code}
	}
	return nil
}

// decideRenewal takes the decision again on the locked row and, when it needs a new token, asks
// the provider. It returns what to report and the connection to write (nil: nothing).
func decideRenewal(ctx context.Context, env lockEnv, in renewal, current *domain.Connection) (locked, *domain.Connection, error) {
	if current == nil {
		return locked{result: failed(FailureNeedsConnection)}, nil, nil
	}
	// A forced caller that lost the race to another process takes the new token.
	force := in.forceRefresh && !current.IsNewerThan(in.seen)
	plan := current.CredentialPlan(env.clock.Now(), domain.PlanOptions{ForceRefresh: force, Config: in.config})
	expire := func(reason domain.ExpiryReason) (locked, *domain.Connection, error) {
		current.Expire(env.clock.Now())
		return locked{result: failed(FailureExpired), expired: reason}, current, nil
	}
	switch plan.Action {
	case domain.PlanUse:
		secret, err := openToken(env.box, current.AccessToken())
		if err != nil {
			return locked{}, nil, err
		}
		return locked{result: ok(secret)}, nil, nil
	case domain.PlanExpired:
		return locked{result: failed(FailureExpired)}, nil, nil
	case domain.PlanExpire:
		return expire(plan.Reason)
	case domain.PlanRefresh:
	}
	// An OAuth connection bound to this config, yet the config has no OAuth client: an
	// inconsistent catalog; leave the connection alone.
	if in.cfg.OAuth == nil {
		return locked{result: failed(FailureNeedsConnection)}, nil, nil
	}

	tokens, refreshErr := refreshTokens(ctx, env, in.cfg, plan.RefreshToken)
	if refreshErr != nil {
		if domain.IsRefreshRejected(failureOf(refreshErr)) {
			return expire(domain.ReasonRefreshRejected)
		}
		// No answer is not a "no": the grant may be fine, so nothing is saved, and a token that has
		// not run out yet still serves.
		if current.UsableWhileProviderDown(env.clock.Now(), domain.ProviderDownOptions{ForceRefresh: force}) {
			secret, err := openToken(env.box, current.AccessToken())
			if err != nil {
				return locked{}, nil, err
			}
			return locked{result: ok(secret)}, nil, nil
		}
		return locked{result: failed(FailureUnavailable)}, nil, nil
	}

	access, err := env.box.Seal(tokens.AccessToken)
	if err != nil {
		return locked{}, nil, errSeal
	}
	sealed := domain.SealedTokens{Access: domain.Sealed(access), ExpiresAt: tokens.ExpiresAt}
	if tokens.RefreshToken != "" {
		refresh, err := env.box.Seal(tokens.RefreshToken)
		if err != nil {
			return locked{}, nil, errSeal
		}
		sealed.Refresh = domain.Sealed(refresh)
	}
	current.Refreshed(sealed, env.clock.Now())
	return locked{result: ok(tokens.AccessToken)}, current, nil
}

// errSeal is returned when the secret box cannot seal a token; its text is not kept.
var errSeal = errors.New("connections: a token cannot be sealed with the configured key")

// errNoAnswer stands for a refresh that produced no usable answer.
var errNoAnswer = errors.New("connections: the OAuth provider gave no usable answer")

// refreshTokens asks the provider. A refresh token no key opens, and an answer without an access
// token, count as no answer (an error that is not a *TokenEndpointError).
func refreshTokens(ctx context.Context, env lockEnv, cfg ServerConfig, sealedRefresh domain.Sealed) (TokenSet, error) {
	refresh, err := env.box.Open([]byte(sealedRefresh))
	if err != nil {
		return TokenSet{}, errNoAnswer
	}
	tokens, err := env.oauth.Refresh(ctx, *cfg.OAuth, env.secret, refresh)
	if err != nil {
		return TokenSet{}, err
	}
	if tokens.AccessToken == "" {
		return TokenSet{}, errNoAnswer
	}
	return tokens, nil
}

// recordExpired writes the audit record of an expiry: connection.expired with meta.reason.
func recordExpired(ctx context.Context, log kernel.AuditLog, userID, server string, reason domain.ExpiryReason) error {
	return record(ctx, log, userID, "connection.expired", server, map[string]any{"reason": string(reason)})
}

// MarkExpiredCommand names a connection upstream rejected.
type MarkExpiredCommand struct {
	UserID string
	Server string
}

// MarkExpired turns a ready connection expired because upstream rejected its credential: a PAT at
// once, an OAuth token whose just-refreshed replacement was rejected too (the gateway decides
// which: this use case only records the fact). An unknown or already expired connection is left
// alone and writes nothing. Audit: connection.expired, reason rejected_upstream, after the commit.
func MarkExpired(ctx context.Context, d Deps, c MarkExpiredCommand) error {
	changed := false
	clock := d.Clock
	err := d.Connections.WithLock(ctx, c.UserID, c.Server, func(_ context.Context, current *domain.Connection) (*domain.Connection, error) {
		if current == nil || current.Status() == domain.StatusExpired {
			return nil, nil
		}
		current.Expire(clock.Now())
		changed = true
		return current, nil
	})
	if err != nil {
		return err
	}
	if changed {
		return recordExpired(ctx, d.Audit, c.UserID, c.Server, domain.ReasonRejectedUpstream)
	}
	return nil
}
