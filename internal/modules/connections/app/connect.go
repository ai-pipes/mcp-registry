package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

// Errors whose text must not carry the cause: the cause may quote a state or a verifier.
var (
	errAuthorizeURL = errors.New("connections: the authorize URL cannot be built for this server")
	errRandom       = errors.New("connections: no random token could be made")
)

// StartConnectCommand starts the OAuth connect of a person to a server.
type StartConnectCommand struct {
	Principal Principal
	Server    string
}

// StartConnect begins Authorization Code + PKCE (S256) for an oauth_user server the person may
// use: it stores the attempt (one-time state, 10 minutes, owned by the person, bound to the
// server's configuration fingerprint) and returns the provider URL to send the person to.
// Refusals: not_found (unknown, not personal, closed, disabled, or the OAuth client is gone),
// invalid_auth_kind (a pat_user server). No audit record.
func StartConnect(ctx context.Context, d Deps, c StartConnectCommand) (string, error) {
	server, err := usablePersonalServer(ctx, d.Servers, c.Principal, c.Server, catalog.AuthOAuthUser)
	if err != nil {
		return "", err
	}
	cfg := server.Config
	if cfg.OAuth == nil {
		return "", domain.ErrNotFound(c.Server)
	}
	state, err := d.Pkce.RandomToken()
	if err != nil {
		return "", errRandom
	}
	verifier, err := d.Pkce.RandomToken()
	if err != nil {
		return "", errRandom
	}
	pending := domain.NewPendingConnect(state, c.Principal.UserID, c.Server, verifier, fingerprint(cfg), d.Clock.Now())
	if err := d.Pending.Insert(ctx, pending); err != nil {
		return "", err
	}
	url, err := d.OAuth.AuthorizeURL(*cfg.OAuth, AuthorizeInput{RedirectURI: d.RedirectURI, State: state, CodeChallenge: d.Pkce.Challenge(verifier)})
	if err != nil {
		return "", errAuthorizeURL
	}
	return url, nil
}

// CompleteConnectCommand is the provider's callback for a person: the state and the code.
type CompleteConnectCommand struct {
	Principal Principal
	State     string
	Code      string
}

// Format prints "CompleteConnectCommand{}" for every verb: State and Code are secrets.
func (CompleteConnectCommand) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "CompleteConnectCommand{}")
}

// LogValue is the slog form: nothing but the type.
func (CompleteConnectCommand) LogValue() slog.Value {
	return slog.StringValue("CompleteConnectCommand{}")
}

// MarshalJSON writes "[redacted]".
func (CompleteConnectCommand) MarshalJSON() ([]byte, error) { return redactedJSON, nil }

// MarshalText writes "[redacted]".
func (CompleteConnectCommand) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// ConnectResult is the server a connect completed for.
type ConnectResult struct{ Server string }

// CompleteConnect finishes the connect: the one-time state of this person, then the code is
// exchanged for tokens, which are stored sealed (replacing any earlier connection) and audited
// (connection.connect, no meta).
//
// The state is consumed atomically with the ownership check (PendingConnects.Consume), so of two
// callbacks with one state exactly one exchanges. A callback by another person finds nothing and
// leaves the starter's attempt untouched (decided deviation oauth-state-kept: the reference
// consumed it); every other refusal below consumes it, as the reference does. Refusals, in order:
// invalid_state (unknown, used, another person's, expired), not_found / invalid_auth_kind (the
// server is no longer an OAuth server the person may use; the code is not exchanged),
// invalid_state (the server configuration changed since the start; no exchange), not_found (the
// OAuth client is gone), exchange_failed (the provider did not authorise: any error from the
// exchange, a wrong client secret included; nothing is stored).
func CompleteConnect(ctx context.Context, d Deps, c CompleteConnectCommand) (ConnectResult, error) {
	pending, err := d.Pending.Consume(ctx, c.State, c.Principal.UserID)
	if err != nil {
		return ConnectResult{}, err
	}
	if pending == nil || !pending.CanComplete(c.Principal.UserID, d.Clock.Now()) {
		return ConnectResult{}, domain.ErrInvalidState()
	}
	server, err := usablePersonalServer(ctx, d.Servers, c.Principal, pending.Server(), catalog.AuthOAuthUser)
	if err != nil {
		return ConnectResult{}, err
	}
	cfg := server.Config
	issuedFor := fingerprint(cfg)
	if !pending.StartedWithConfig(issuedFor) {
		return ConnectResult{}, domain.ErrInvalidState()
	}
	if cfg.OAuth == nil {
		return ConnectResult{}, domain.ErrNotFound(pending.Server())
	}
	secret, err := d.Servers.ClientSecret(ctx, pending.Server())
	if err != nil {
		return ConnectResult{}, fmt.Errorf("connections: read the OAuth client secret of %q: %w", pending.Server(), err)
	}
	// The secret was read after the configuration: if the server changed in between, neither the
	// code nor the new secret goes to the old token URL (refused like any configuration change).
	if again, err := d.Servers.Config(ctx, pending.Server()); err != nil {
		return ConnectResult{}, err
	} else if again == nil || fingerprint(*again) != issuedFor {
		return ConnectResult{}, domain.ErrInvalidState()
	}
	tokens, err := d.OAuth.ExchangeCode(ctx, *cfg.OAuth, secret, ExchangeInput{Code: c.Code, CodeVerifier: pending.CodeVerifier(), RedirectURI: d.RedirectURI})
	if err != nil {
		return ConnectResult{}, domain.ErrExchangeFailed()
	}
	sealed, err := sealTokens(d, tokens)
	if err != nil {
		return ConnectResult{}, err
	}
	var revokeURL *string
	if cfg.OAuth.RevokeURL != nil {
		u := *cfg.OAuth.RevokeURL
		revokeURL = &u
	}
	connection := domain.ConnectOAuth(domain.OAuthConnect{
		UserID: c.Principal.UserID, Server: pending.Server(), Tokens: sealed,
		IssuedFor: issuedFor, RevokeURL: revokeURL, Now: d.Clock.Now(),
	})
	if err := writeConnection(ctx, d, connection); err != nil {
		return ConnectResult{}, err
	}
	if err := record(ctx, d.Audit, c.Principal.UserID, "connection.connect", pending.Server(), nil); err != nil {
		return ConnectResult{}, err
	}
	return ConnectResult{Server: pending.Server()}, nil
}

func sealTokens(d Deps, t TokenSet) (domain.SealedTokens, error) {
	access, err := d.Box.Seal(t.AccessToken)
	if err != nil {
		return domain.SealedTokens{}, errSeal
	}
	out := domain.SealedTokens{Access: domain.Sealed(access), ExpiresAt: t.ExpiresAt}
	if t.RefreshToken != "" {
		refresh, err := d.Box.Seal(t.RefreshToken)
		if err != nil {
			return domain.SealedTokens{}, errSeal
		}
		out.Refresh = domain.Sealed(refresh)
	}
	return out, nil
}

// writeConnection stores the connection under its lock, replacing any earlier row for the pair.
func writeConnection(ctx context.Context, d Deps, c *domain.Connection) error {
	return d.Connections.WithLock(ctx, c.UserID(), c.Server(), func(context.Context, *domain.Connection) (*domain.Connection, error) {
		return c, nil
	})
}

// SavePATCommand stores a personal access token.
type SavePATCommand struct {
	Principal Principal
	Server    string
	Token     string
}

// Format prints the server only, for every verb: Token is a secret.
func (c SavePATCommand) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "SavePATCommand{server:%s}", c.Server)
}

// LogValue is the slog form: the server only.
func (c SavePATCommand) LogValue() slog.Value {
	return slog.GroupValue(slog.String("server", c.Server))
}

// MarshalJSON writes "[redacted]".
func (SavePATCommand) MarshalJSON() ([]byte, error) { return redactedJSON, nil }

// MarshalText writes "[redacted]".
func (SavePATCommand) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// SavePAT stores a personal access token for a pat_user server the person may use, replacing any
// earlier connection (an OAuth one too). The server is checked first: not_found, invalid_auth_kind;
// then the token (domain.ParsePAT: trimmed, a non-empty single line of at most 4096 UTF-16 units):
// invalid_pat. Audit: connection.pat_saved, no meta.
func SavePAT(ctx context.Context, d Deps, c SavePATCommand) error {
	server, err := usablePersonalServer(ctx, d.Servers, c.Principal, c.Server, catalog.AuthPATUser)
	if err != nil {
		return err
	}
	token, err := domain.ParsePAT(c.Token)
	if err != nil {
		return err
	}
	sealed, err := d.Box.Seal(token)
	if err != nil {
		return errSeal
	}
	connection := domain.ConnectPAT(domain.PATConnect{
		UserID: c.Principal.UserID, Server: c.Server, Token: domain.Sealed(sealed),
		IssuedFor: fingerprint(server.Config), Now: d.Clock.Now(),
	})
	if err := writeConnection(ctx, d, connection); err != nil {
		return err
	}
	return record(ctx, d.Audit, c.Principal.UserID, "connection.pat_saved", c.Server, nil)
}

// DisconnectCommand removes a person's connection.
type DisconnectCommand struct {
	Principal Principal
	Server    string
}

// Disconnect deletes the person's connection. It works even when the server is no longer usable:
// one's own credentials can always be removed. Nothing stored: nothing happens (no audit, no
// revoke). Otherwise the audit record (connection.disconnect, no meta) is written first; then, for
// an OAuth connection whose server still has an OAuth client with the configuration it was issued
// for and the revoke URL it was issued with (Connection.MayRevokeAt: a changed or newly added URL
// gets nothing), the refresh token (else the access token) is revoked at the provider (RFC 7009:
// the refresh token ends the whole grant). Only the revoke call's own failure is ignored; the
// delete and the audit are done by then, so a client secret or stored token no key opens is an
// error that leaves the row deleted.
func Disconnect(ctx context.Context, d Deps, c DisconnectCommand) error {
	removed, err := d.Connections.Delete(ctx, c.Principal.UserID, c.Server)
	if err != nil {
		return err
	}
	if removed == nil {
		return nil
	}
	if err := record(ctx, d.Audit, c.Principal.UserID, "connection.disconnect", c.Server, nil); err != nil {
		return err
	}
	if removed.Kind() != domain.KindOAuth {
		return nil
	}
	cfg, err := d.Servers.Config(ctx, c.Server)
	if err != nil {
		return err
	}
	// Only to the provider that issued the token: never to one an admin has put in its place.
	if cfg == nil || cfg.OAuth == nil || !removed.IssuedFor(fingerprint(*cfg)) {
		return nil
	}
	if !removed.MayRevokeAt(cfg.OAuth.RevokeURL) {
		return nil
	}
	secret, err := d.Servers.ClientSecret(ctx, c.Server)
	if err != nil {
		return fmt.Errorf("connections: read the OAuth client secret of %q: %w", c.Server, err)
	}
	// The secret was read after the configuration: if the server changed in between, the new
	// secret must not go to the old revoke URL. The row is deleted already; nothing is sent.
	again, err := d.Servers.Config(ctx, c.Server)
	if err != nil {
		return err
	}
	if again == nil || again.OAuth == nil || !removed.IssuedFor(fingerprint(*again)) || !removed.MayRevokeAt(again.OAuth.RevokeURL) {
		return nil
	}
	sealed := removed.RefreshToken()
	if len(sealed) == 0 {
		sealed = removed.AccessToken()
	}
	token, err := openToken(d.Box, sealed)
	if err != nil {
		return err
	}
	// The local credentials are gone already; the upstream token runs out on its own.
	_ = d.OAuth.Revoke(ctx, *again.OAuth, secret, token)
	return nil
}
