package app

import (
	"context"
	"net/url"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

// StartOIDCLoginInput is the input of StartOIDCLogin; ReturnTo is untrusted and normalised.
type StartOIDCLoginInput struct{ ReturnTo string }

// StartOIDCLogin begins Authorization Code + PKCE: it returns the provider's authorization URL
// and remembers the attempt. While the provider is unreachable the error is idp_unavailable and
// nothing is remembered. ReturnTo is normalised to a same-origin path before it is stored.
func StartOIDCLogin(ctx context.Context, deps Deps, in StartOIDCLoginInput) (*url.URL, error) {
	state, err := deps.Random.Base64URL(32)
	if err != nil {
		return nil, err
	}
	nonce, err := deps.Random.Base64URL(32)
	if err != nil {
		return nil, err
	}
	codeVerifier, err := deps.Random.Base64URL(32)
	if err != nil {
		return nil, err
	}
	authURL, err := deps.OIDC.AuthorizationURL(ctx, OIDCAuthorizationInput{
		State:         state,
		Nonce:         nonce,
		CodeChallenge: deps.Hasher.Base64URL(codeVerifier),
	})
	if err != nil {
		return nil, err
	}
	if err := deps.PendingLogins.Insert(ctx, PendingLogin{
		State:        state,
		CodeVerifier: codeVerifier,
		Nonce:        nonce,
		ReturnTo:     domain.NormalizeReturnTo(in.ReturnTo),
		ExpiresAt:    domain.LoginStateExpiry(deps.Clock.Now()),
	}); err != nil {
		return nil, err
	}
	return authURL, nil
}

// CompleteOIDCLoginInput is the input of CompleteOIDCLogin; Iss is the callback's iss parameter
// when the provider sent one, IP the client address the callback limiter counts.
type CompleteOIDCLoginInput struct {
	State string
	Code  string
	Iss   *string
	IP    string
}

// OIDCLoginResult is a finished sign-in: the new session id and where to send the browser.
type OIDCLoginResult struct {
	SessionID string
	ReturnTo  string
}

// CompleteOIDCLogin finishes the login: it checks the state, the provider's answer and the group
// policy. Order of effects (pinned by the compatibility suite): the pending state is consumed
// first and stays consumed when the exchange fails or the provider is down; idp_unavailable is
// returned without audit and without the limiter; any other exchange failure is audited as
// exchange_failed only while the callback limiter allows, and is reported as invalid_state; a
// successful exchange never touches the limiter. A policy refusal or a disabled person is
// audited before the refusal is returned.
func CompleteOIDCLogin(ctx context.Context, deps Deps, in CompleteOIDCLoginInput) (OIDCLoginResult, error) {
	pending, err := deps.PendingLogins.Consume(ctx, in.State)
	if err != nil {
		return OIDCLoginResult{}, err
	}
	if pending == nil || domain.IsLoginStateExpired(pending.ExpiresAt, deps.Clock.Now()) {
		return OIDCLoginResult{}, domain.Err(domain.CodeInvalidState, "login attempt is unknown or expired")
	}

	claims, err := deps.OIDC.Exchange(ctx, OIDCExchangeInput{
		Code:         in.Code,
		CodeVerifier: pending.CodeVerifier,
		Nonce:        pending.Nonce,
		Iss:          in.Iss,
	})
	if err != nil {
		if refusal, ok := domain.AsError(err); ok && refusal.Code == domain.CodeIDPUnavailable {
			return OIDCLoginResult{}, err
		}
		// Anyone can start a login and send a bad code: past the limit per IP the refusal is not
		// recorded (it is still a refusal, and a valid code is never held back by the count).
		if allowed, _ := deps.CallbackLimiter.Hit(in.IP); allowed {
			if err := deps.Audit.Record(ctx, kernel.AuditEntry{
				Action: "auth.login_failed",
				Meta:   map[string]any{"reason": "exchange_failed"},
			}); err != nil {
				return OIDCLoginResult{}, err
			}
		}
		return OIDCLoginResult{}, domain.Err(domain.CodeInvalidState, "the identity provider rejected the login")
	}

	role, ok := domain.AccessFor(claims.Groups, deps.Policy)
	if !ok {
		if err := deps.Audit.Record(ctx, kernel.AuditEntry{
			Action: "auth.login_failed",
			Meta:   map[string]any{"reason": "not_allowed", "sub": claims.Sub},
		}); err != nil {
			return OIDCLoginResult{}, err
		}
		return OIDCLoginResult{}, domain.Err(domain.CodeNotAllowed, "your account is not in an allowed group")
	}

	existing, err := deps.Users.FindBySub(ctx, claims.Sub)
	if err != nil {
		return OIDCLoginResult{}, err
	}
	if existing != nil && existing.Disabled {
		if err := deps.Audit.Record(ctx, kernel.AuditEntry{
			ActorID: ptr(existing.ID),
			Action:  "auth.login_failed",
			Meta:    map[string]any{"reason": "disabled"},
		}); err != nil {
			return OIDCLoginResult{}, err
		}
		return OIDCLoginResult{}, domain.Err(domain.CodeDisabled, "account is disabled")
	}

	now := deps.Clock.Now()
	user, err := deps.Users.UpsertFromLogin(ctx, UpsertLogin{Claims: claims, Role: role, At: now})
	if err != nil {
		return OIDCLoginResult{}, err
	}
	sessionID, err := deps.Random.Base64URL(32)
	if err != nil {
		return OIDCLoginResult{}, err
	}
	if err := deps.Sessions.Insert(ctx, SessionRecord{
		IDHash:     deps.Hasher.Hex(sessionID),
		UserID:     user.ID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  domain.SessionExpiry(now),
	}); err != nil {
		return OIDCLoginResult{}, err
	}
	if err := deps.Audit.Record(ctx, kernel.AuditEntry{
		ActorID: ptr(user.ID),
		Action:  "auth.login",
		Meta:    map[string]any{"via": "oidc"},
	}); err != nil {
		return OIDCLoginResult{}, err
	}
	return OIDCLoginResult{SessionID: sessionID, ReturnTo: pending.ReturnTo}, nil
}
