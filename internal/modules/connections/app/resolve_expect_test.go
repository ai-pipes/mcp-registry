package app_test

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
)

// expectOf is what a caller read earlier: the configuration of gitea now, with another URL when
// moved is set (the server was changed after the caller read it, or before: either way the two
// views differ).
func (h *harness) expectOf(moved bool) *app.ServerConfig {
	cfg := h.dir.configOf(h.gitea())
	if moved {
		cfg.URL = "https://elsewhere.example.com/mcp"
	}
	return &cfg
}

func TestResolveWithTheExpectedConfigurationBehavesAsBefore(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(-1000)), "R1", "alice") // a refresh is due
	h.oauth.next = app.TokenSet{AccessToken: "A2", ExpiresAt: tp(at(3_600_000))}
	r, err := app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea", Expect: h.expectOf(false)})
	wantOK(t, r, err, "A2")
	if got := h.oauth.refreshed(); !slices.Equal(got, []string{"R1"}) {
		t.Fatalf("refreshed %v", got)
	}
}

// The caller's view of the server is stale: nothing is opened, sent, stored, expired or audited,
// even though a refresh was due (and, for the PlanUse case, the stored token is fine).
func TestResolveWithAnotherExpectedConfigurationIsUnavailableAndTouchesNothing(t *testing.T) {
	for name, expires := range map[string]*time.Time{"refresh due": tp(at(-1000)), "token fresh": tp(at(3_600_000))} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.seedOAuth(expires, "R1", "alice")
			before, _ := h.store.snap("alice", "gitea")
			h.j.reset()
			r, err := app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea", Expect: h.expectOf(true)})
			wantFailure(t, r, err, app.FailureUnavailable)
			after, _ := h.store.snap("alice", "gitea")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("the stored connection changed")
			}
			if len(h.oauth.refreshed()) != 0 || len(h.audit.all()) != 0 {
				t.Fatalf("the provider was called or audit written: %v %v", h.oauth.refreshed(), h.audit.all())
			}
			for _, c := range h.j.all() {
				if c == "lock.begin" || c == "oauth.refresh" || c == "audit" || c == "servers.secret" || c == "connections.write" {
					t.Fatalf("unexpected call %q in %v", c, h.j.all())
				}
			}
		})
	}
}

// A mismatch is judged per caller before the flight: a caller with the right view renews, one with
// a stale view is told so at once, whatever is in flight for the same key.
func TestTwoCallersWithDifferentExpectationsEachGetTheirOwnAnswer(t *testing.T) {
	h := newHarness(t)
	h.v.setStrict(false)
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.oauth.entered = make(chan string, 4)
	gate := make(chan struct{})
	h.oauth.gate = gate
	h.oauth.refreshFn = func(_ context.Context, rt string) (app.TokenSet, error) {
		return app.TokenSet{AccessToken: "new-" + rt, ExpiresAt: tp(at(3_600_000))}, nil
	}
	type out struct {
		r   app.CredentialResult
		err error
	}
	good := make(chan out, 1)
	go func() {
		r, err := app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea", Expect: h.expectOf(false)})
		good <- out{r, err}
	}()
	select {
	case <-h.oauth.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the renewal never started")
	}
	r, err := app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea", Expect: h.expectOf(true)})
	wantFailure(t, r, err, app.FailureUnavailable)
	close(gate)
	g := <-good
	wantOK(t, g.r, g.err, "new-R1")
	if got := h.oauth.refreshed(); len(got) != 1 {
		t.Fatalf("refreshed %v, want one renewal", got)
	}
}

func TestServerConfigOfIsTheDirectorysMapping(t *testing.T) {
	oa := giteaClient
	target := catalog.ServerCallTarget{Slug: "gitea", URL: giteaURL, Auth: catalog.AuthMode{Kind: catalog.AuthOAuthUser, OAuth: &oa}}
	got := app.ServerConfigOf(target)
	if got.Slug != "gitea" || got.URL != giteaURL || got.AuthKind != catalog.AuthOAuthUser || got.OAuth == nil || got.OAuth.TokenURL != oa.TokenURL {
		t.Fatalf("%+v", got)
	}
	pat := app.ServerConfigOf(catalog.ServerCallTarget{Slug: "yt", URL: "u", Auth: catalog.AuthMode{Kind: catalog.AuthPATUser, OAuth: &oa}})
	if pat.OAuth != nil {
		t.Fatal("only an oauth_user server has an OAuth client")
	}
}
