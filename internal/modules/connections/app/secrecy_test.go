package app_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

var markers = []string{
	"ACCESSMARK", "REFRESHMARK", "PATMARK", "CODEMARK", "CLIENTSECRETMARK", "STATEMARK", "VERIFIERMARK",
	"NEWACCESSMARK", "NEWREFRESHMARK",
}

// errSloppy is what a careless provider client would return: an error quoting every secret.
var errSloppy = errors.New("provider said: " + strings.Join(markers, " "))

func secretHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.gitea().clientSecret = "CLIENTSECRETMARK"
	h.withRevokeURL()
	h.oauth.exchangeOut = app.TokenSet{AccessToken: "ACCESSMARK", RefreshToken: "REFRESHMARK", ExpiresAt: tp(at(3_600_000))}
	h.oauth.next = app.TokenSet{AccessToken: "NEWACCESSMARK", RefreshToken: "NEWREFRESHMARK", ExpiresAt: tp(at(3_600_000))}
	return h
}

func seedMarked(t *testing.T, h *harness, expiresAt int64, refresh string) {
	t.Helper()
	tokens := domain.SealedTokens{Access: mustSeal(t, h.box, "ACCESSMARK"), ExpiresAt: tp(at(expiresAt))}
	if refresh != "" {
		tokens.Refresh = mustSeal(t, h.box, refresh)
	}
	h.store.put(domain.ConnectOAuth(domain.OAuthConnect{
		UserID: "alice", Server: "gitea", Tokens: tokens, IssuedFor: h.fingerprintOf(h.gitea()), RevokeURL: h.gitea().oauth.RevokeURL, Now: t0,
	}))
}

// leaks reports every marker found in any rendering of v.
func leaks(v any) []string {
	var found []string
	text := fmt.Sprintf("%v|%+v|%#v|%s|%q", v, v, v, v, v)
	for _, m := range markers {
		if strings.Contains(text, m) {
			found = append(found, m)
		}
	}
	return found
}

func TestNoSecretInErrorsAuditOrPrintedValues(t *testing.T) {
	type scenario struct {
		name string
		run  func(h *harness) []any // the errors and values the call returned
	}
	complete := func(h *harness, p string) (any, error) {
		url, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "gitea"})
		if err != nil {
			return nil, err
		}
		return app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: stateOf(url), Code: p})
	}
	scenarios := []scenario{
		{"refresh: no answer with a errSloppy error", func(h *harness) []any {
			seedMarked(t, h, -1000, "REFRESHMARK")
			h.oauth.refreshFn = func(_ context.Context, _ string) (app.TokenSet, error) { return app.TokenSet{}, errSloppy }
			r, err := h.resolve("alice", "gitea")
			return []any{r, err}
		}},
		{"refresh: rejected with a errSloppy error code", func(h *harness) []any {
			seedMarked(t, h, -1000, "REFRESHMARK")
			h.oauth.refreshFn = func(_ context.Context, _ string) (app.TokenSet, error) {
				return app.TokenSet{}, &app.TokenEndpointError{Status: 400, Code: "invalid_grant"}
			}
			r, err := h.resolve("alice", "gitea")
			return []any{r, err}
		}},
		{"refresh: success returns the secret, which must not print", func(h *harness) []any {
			seedMarked(t, h, -1000, "REFRESHMARK")
			r, err := h.resolve("alice", "gitea")
			if r.Secret != "NEWACCESSMARK" {
				t.Fatalf("secret %q", r.Secret)
			}
			return []any{r, &r, err}
		}},
		{"use: success returns the secret, which must not print", func(h *harness) []any {
			seedMarked(t, h, 3_600_000, "REFRESHMARK")
			r, err := h.resolve("alice", "gitea")
			if r.Secret != "ACCESSMARK" {
				t.Fatalf("secret %q", r.Secret)
			}
			return []any{r, err}
		}},
		{"key mismatch on the access token", func(h *harness) []any {
			seedMarked(t, h, 3_600_000, "REFRESHMARK")
			h.box.failOpenOf("ACCESSMARK")
			r, err := h.resolve("alice", "gitea")
			return []any{r, err}
		}},
		{"key mismatch on the refresh token", func(h *harness) []any {
			seedMarked(t, h, -1000, "REFRESHMARK")
			h.box.failOpenOf("REFRESHMARK")
			r, err := h.resolve("alice", "gitea")
			return []any{r, err}
		}},
		{"client secret read fails", func(h *harness) []any {
			seedMarked(t, h, -1000, "REFRESHMARK")
			h.dir.secretErr = errors.New("cannot open the stored secret")
			r, err := h.resolve("alice", "gitea")
			return []any{r, err}
		}},
		{"config changed", func(h *harness) []any {
			seedMarked(t, h, 3_600_000, "REFRESHMARK")
			h.moveGitea()
			r, err := h.resolve("alice", "gitea")
			return []any{r, err}
		}},
		{"mark expired", func(h *harness) []any {
			seedMarked(t, h, 3_600_000, "REFRESHMARK")
			return []any{app.MarkExpired(ctx, h.d, app.MarkExpiredCommand{UserID: "alice", Server: "gitea"})}
		}},
		{"start: authorize URL fails with a errSloppy error", func(h *harness) []any {
			h.oauth.authErr = errSloppy
			_, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "gitea"})
			return []any{err}
		}},
		{"start: random source fails", func(h *harness) []any {
			h.pkce.fail = errSloppy
			_, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "gitea"})
			return []any{err}
		}},
		{"complete: exchange fails with a errSloppy error", func(h *harness) []any {
			h.oauth.exchangeErr = errSloppy
			res, err := complete(h, "CODEMARK")
			return []any{res, err}
		}},
		{"complete: success", func(h *harness) []any {
			res, err := complete(h, "CODEMARK")
			return []any{res, err}
		}},
		{"complete: unknown state that is a code", func(h *harness) []any {
			_, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: "STATEMARK-9", Code: "CODEMARK"})
			return []any{err}
		}},
		{"complete: foreign person", func(h *harness) []any {
			url, _ := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "gitea"})
			_, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: bob, State: stateOf(url), Code: "CODEMARK"})
			return []any{err}
		}},
		{"complete: store fails", func(h *harness) []any {
			h.store.failLock = errors.New("db down")
			res, err := complete(h, "CODEMARK")
			return []any{res, err}
		}},
		{"savePat: refused tokens", func(h *harness) []any {
			var out []any
			for _, tok := range []string{"PATMARK\nPATMARK", "PATMARK\x00", strings.Repeat("PATMARK", 700)} {
				out = append(out, app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: tok}))
			}
			out = append(out, app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "gitea", Token: "PATMARK"}))
			return out
		}},
		{"savePat: success", func(h *harness) []any {
			return []any{app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "PATMARK"})}
		}},
		{"disconnect: revoke fails with a errSloppy error", func(h *harness) []any {
			seedMarked(t, h, 3_600_000, "REFRESHMARK")
			h.oauth.revokeErr = errSloppy
			return []any{app.Disconnect(ctx, h.d, app.DisconnectCommand{Principal: alice, Server: "gitea"})}
		}},
		{"disconnect: key mismatch", func(h *harness) []any {
			seedMarked(t, h, 3_600_000, "REFRESHMARK")
			h.box.failOpenOf("REFRESHMARK")
			return []any{app.Disconnect(ctx, h.d, app.DisconnectCommand{Principal: alice, Server: "gitea"})}
		}},
		{"list", func(h *harness) []any {
			seedMarked(t, h, 3_600_000, "REFRESHMARK")
			list, err := app.ListConnections(ctx, h.d, app.ListQuery{Principal: alice})
			return []any{list, err}
		}},
		{"reseal", func(h *harness) []any {
			seedMarked(t, h, 3_600_000, "REFRESHMARK")
			sum, err := app.ResealConnections(ctx, h.store, h.box, newBox(2))
			return []any{sum, err}
		}},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			h := secretHarness(t)
			for _, v := range s.run(h) {
				if found := leaks(v); len(found) != 0 {
					t.Fatalf("%T leaks %v", v, found)
				}
			}
			if found := leaks(h.audit.text()); len(found) != 0 {
				t.Fatalf("audit leaks %v:\n%s", found, h.audit.text())
			}
			for _, e := range h.audit.all() {
				if found := leaks(e.Meta); len(found) != 0 {
					t.Fatalf("meta leaks %v", found)
				}
			}
		})
	}
}

func TestSecretCarryingTypesRedactWhenPrinted(t *testing.T) {
	values := []any{
		app.CredentialResult{Secret: "ACCESSMARK"},
		&app.CredentialResult{Secret: "ACCESSMARK"},
		app.TokenSet{AccessToken: "ACCESSMARK", RefreshToken: "REFRESHMARK"},
		&app.TokenSet{AccessToken: "ACCESSMARK", RefreshToken: "REFRESHMARK"},
		app.NewClientSecret("CLIENTSECRETMARK"),
		app.ExchangeInput{Code: "CODEMARK", CodeVerifier: "VERIFIERMARK", RedirectURI: "https://x"},
		app.AuthorizeInput{State: "STATEMARK", CodeChallenge: "c", RedirectURI: "https://x"},
	}
	for _, v := range values {
		if found := leaks(v); len(found) != 0 {
			t.Errorf("%T prints %v", v, found)
		}
		var sb strings.Builder
		slog.New(slog.NewTextHandler(&sb, nil)).Info("x", "v", v)
		slog.New(slog.NewJSONHandler(&sb, nil)).Info("x", "v", v)
		for _, m := range markers {
			if strings.Contains(sb.String(), m) {
				t.Errorf("%T logs %s", v, m)
			}
		}
	}
	// The value is still readable by the code that needs it.
	if s, ok := app.NewClientSecret("x").Value(); !ok || s != "x" {
		t.Fatal("Value")
	}
	if _, ok := (app.ClientSecret{}).Value(); ok {
		t.Fatal("the zero value must be a public client")
	}
}
