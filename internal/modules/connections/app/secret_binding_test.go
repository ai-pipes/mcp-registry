package app_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
)

// The OAuth client secret is read with the configuration it belongs to: an admin who moves the
// server to B (new secret) and back to A while a call is in flight never gets B's secret sent to
// A's addresses, on any of the three paths that use it.

const secretB = "the-secret-of-B"

// toB is the admin's save that points the OAuth client at another provider and sets its secret.
func toB(h *harness) {
	h.dir.edit("gitea", func(g *fakeServer) {
		o := *g.oauth
		o.TokenURL = "https://b.example.com/token"
		o.RevokeURL = sp("https://b.example.com/revoke")
		g.oauth = &o
	})
	h.dir.edit("gitea", func(g *fakeServer) { g.clientSecret, g.hasSecret = secretB, true })
}

// backToA restores the original provider (the secret is cleared by that save).
func backToA(h *harness) {
	h.dir.edit("gitea", func(g *fakeServer) {
		o := *g.oauth
		o.TokenURL = giteaClient.TokenURL
		o.RevokeURL = sp("https://git.example.com/revoke")
		g.oauth = &o
	})
}

// abaAround makes the secret read hit the moment B's secret is stored; A is back right after.
func abaAround(h *harness) {
	h.dir.onSecret = func() { toB(h) }
	h.dir.afterSecret = func() { backToA(h) }
}

func TestRenewalNeverSendsBSecretToAAfterAnABAChange(t *testing.T) {
	h := newHarness(t)
	h.withRevokeURL()
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	abaAround(h)
	r, err := h.resolve("alice", "gitea")
	wantFailure(t, r, err, app.FailureUnavailable)
	if len(h.oauth.refreshed()) != 0 {
		t.Fatalf("the provider was called: %v", h.oauth.refreshSecrets)
	}
}

func TestCompleteConnectNeverSendsBSecretToAAfterAnABAChange(t *testing.T) {
	h := newHarness(t)
	h.withRevokeURL()
	state := stateOf(startURL(t, h, "gitea"))
	abaAround(h)
	_, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: kernel.NewSecret(state), Code: kernel.NewSecret("c")})
	if code(err) != "invalid_state" {
		t.Fatalf("%v", err)
	}
	if len(h.oauth.exchanged()) != 0 || h.store.size() != 0 {
		t.Fatalf("exchanged %v, rows %d", h.oauth.exchanged(), h.store.size())
	}
}

func TestDisconnectNeverSendsBSecretToAAfterAnABAChange(t *testing.T) {
	h := newHarness(t)
	h.withRevokeURL()
	h.connect(alice, "gitea")
	abaAround(h)
	if err := app.Disconnect(ctx, h.d, app.DisconnectCommand{Principal: alice, Server: "gitea"}); err != nil {
		t.Fatal(err)
	}
	if len(h.oauth.revoked()) != 0 || h.store.size() != 0 {
		t.Fatalf("revoked %v (secrets %v), rows %d", h.oauth.revoked(), h.oauth.revokeSecrets, h.store.size())
	}
	for _, s := range h.oauth.revokeSecrets {
		if s == secretB {
			t.Fatal("B's secret was sent")
		}
	}
}

// A change to B and back before the read leaves A's configuration with no secret (the save that
// returned cleared it): the call goes on with none, never with B's.
func TestRenewalAfterAnABAChangeBeforeTheReadUsesNoSecret(t *testing.T) {
	h := newHarness(t)
	h.withRevokeURL()
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.dir.onSecret = func() { toB(h); backToA(h) }
	r, err := h.resolve("alice", "gitea")
	if err != nil || r.Secret.Reveal() != "A-new" {
		t.Fatalf("%v %v", r, err)
	}
	if got := h.oauth.refreshSecrets; len(got) != 1 || got[0] != (exchangeSecret{}) {
		t.Fatalf("the refresh carried %v", got)
	}
}

func TestRenewalOfAServerWithoutAClientSecretCarriesNone(t *testing.T) {
	h := newHarness(t)
	h.gitea().hasSecret, h.gitea().clientSecret = false, ""
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	if _, err := h.resolve("alice", "gitea"); err != nil {
		t.Fatal(err)
	}
	if got := h.oauth.refreshSecrets; len(got) != 1 || got[0] != (exchangeSecret{}) {
		t.Fatalf("the refresh carried %v", got)
	}
}

func TestRenewalCarriesTheStoredClientSecret(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	if _, err := h.resolve("alice", "gitea"); err != nil {
		t.Fatal(err)
	}
	if got := h.oauth.refreshSecrets; len(got) != 1 || got[0] != (exchangeSecret{"client-secret", true}) {
		t.Fatalf("the refresh carried %v", got)
	}
}

// Decided consequence (as in the reference): an authorize-URL-only change between the reads is not
// a secret destination, so it is not caught on the spot. The renewal refreshes at the unchanged
// token URL; the connection is expired by the next resolve (config_changed). No token or secret
// goes anywhere new.
func TestAnAuthorizeURLChangeBetweenTheReadsIsCaughtByTheNextResolve(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.dir.onSecret = func() {
		h.dir.edit("gitea", func(g *fakeServer) {
			o := *g.oauth
			o.AuthorizeURL = "https://other.example.com/authorize"
			g.oauth = &o
		})
	}
	r, err := h.resolve("alice", "gitea")
	if err != nil || r.Secret.Reveal() != "A-new" {
		t.Fatalf("%v %v", r, err)
	}
	h.dir.onSecret = nil
	r, err = h.resolve("alice", "gitea")
	wantFailure(t, r, err, app.FailureExpired)
}

// Decided consequence: the revoke URL is a destination of the secret but not part of the
// fingerprint, so a change of ONLY the revoke URL between the configuration read and the secret
// read makes a renewal `unavailable` (before the single-read change it went through). Nothing is
// stored or sent; the next call, with the configuration read again, renews normally.
func TestARevokeURLOnlyChangeBetweenTheReadsMakesTheRenewalUnavailable(t *testing.T) {
	h := newHarness(t)
	h.withRevokeURL()
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.dir.onSecret = func() {
		h.dir.edit("gitea", func(g *fakeServer) {
			o := *g.oauth
			o.RevokeURL = sp("https://other.example.com/revoke")
			g.oauth = &o
		})
	}
	r, err := h.resolve("alice", "gitea")
	wantFailure(t, r, err, app.FailureUnavailable)
	if len(h.oauth.refreshed()) != 0 {
		t.Fatalf("the provider was called: %v", h.oauth.refreshSecrets)
	}
	h.dir.onSecret = nil
	r, err = h.resolve("alice", "gitea")
	if err != nil || r.Failure != "" {
		t.Fatalf("%v %v", r, err)
	}
}
