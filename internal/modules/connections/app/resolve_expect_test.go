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

// Each part of the fingerprint counts: an expectation that differs in only one of them is stale.
func TestExpectationDiffersInAnyFingerprintedPartIsUnavailable(t *testing.T) {
	changes := map[string]func(*app.ServerConfig){
		"url":           func(c *app.ServerConfig) { c.URL = "https://other.example.com/mcp" },
		"token url":     func(c *app.ServerConfig) { o := *c.OAuth; o.TokenURL += "2"; c.OAuth = &o },
		"client id":     func(c *app.ServerConfig) { o := *c.OAuth; o.ClientID = "other"; c.OAuth = &o },
		"authorize url": func(c *app.ServerConfig) { o := *c.OAuth; o.AuthorizeURL += "2"; c.OAuth = &o },
		"auth kind":     func(c *app.ServerConfig) { c.AuthKind = catalog.AuthPATUser; c.OAuth = nil },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
			exp := *h.expectOf(false)
			change(&exp)
			r, err := app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea", Expect: &exp})
			wantFailure(t, r, err, app.FailureUnavailable)
		})
	}
	t.Run("a part that is not fingerprinted does not count", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
		exp := *h.expectOf(false)
		o := *exp.OAuth
		o.Scopes = []string{"more"}
		exp.OAuth = &o
		r, err := app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea", Expect: &exp})
		wantOK(t, r, err, "A1")
	})
}

// The secret box is never asked to open anything before the expectation has been checked.
func TestNothingIsOpenedBeforeTheExpectationIsChecked(t *testing.T) {
	for name, expires := range map[string]*time.Time{"fresh": tp(at(3_600_000)), "due": tp(at(-1000))} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.seedOAuth(expires, "R1", "alice")
			r, err := app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea", Expect: h.expectOf(true)})
			wantFailure(t, r, err, app.FailureUnavailable)
			if h.box.opens != 0 {
				t.Fatalf("the box opened %d values", h.box.opens)
			}
		})
	}
}

// Two callers that loaded DIFFERENT configurations (an admin change in between), each expecting
// the one it loaded, must not share one renewal: the follower would receive the token the leader's
// renewal produced under the other configuration. The follower waits for its own turn on the row
// and then finds the connection issued for the other configuration: expired, never the secret.
func TestCallersThatLoadedDifferentConfigurationsDoNotShareARenewal(t *testing.T) {
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
	a := h.expectOf(false)
	leader := make(chan out, 1)
	go func() {
		r, err := app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea", Expect: a})
		leader <- out{r, err}
	}()
	select {
	case <-h.oauth.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the renewal never started")
	}
	// The admin moves the server; the follower loads the new configuration and expects it.
	g := h.gitea()
	h.dir.mu.Lock()
	g.url = "https://moved.example.com/gitea"
	h.dir.mu.Unlock()
	follower := make(chan out, 1)
	go func() {
		r, err := app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea", Expect: h.expectOf(false)})
		follower <- out{r, err}
	}()
	// Wait until the follower has read the secret of its own renewal: it did not join the leader's
	// flight. (With a shared flight it never gets there; the bound only ends that wait.)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && countOf(h.j.all(), "servers.client_secret") < 2 {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	l, f := <-leader, <-follower
	wantOK(t, l.r, l.err, "new-R1")
	if f.err != nil || f.r.Secret != "" {
		t.Fatalf("the follower got a secret issued under the other configuration: %+v %v", f.r, f.err)
	}
	if f.r.Failure != app.FailureExpired {
		t.Fatalf("follower failure %q, want expired (the connection was issued for the old configuration)", f.r.Failure)
	}
}

func countOf(all []string, what string) int {
	n := 0
	for _, c := range all {
		if c == what {
			n++
		}
	}
	return n
}

// The client secret is read after the configuration: if the server changed in between, the new
// secret must not travel with the old configuration's token URL. Nothing is stored or sent.
func TestResolveRefusesWhenTheConfigurationChangedWhileTheClientSecretWasRead(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	before, _ := h.store.snap("alice", "gitea")
	h.dir.onSecret = func() {
		g := h.gitea()
		h.dir.mu.Lock()
		defer h.dir.mu.Unlock()
		o := *g.oauth
		o.TokenURL = "https://evil.example.com/token"
		g.oauth = &o
		g.clientSecret = "the-new-client-secret"
	}
	r, err := app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea"})
	wantFailure(t, r, err, app.FailureUnavailable)
	after, _ := h.store.snap("alice", "gitea")
	if !reflect.DeepEqual(before, after) || len(h.oauth.refreshed()) != 0 || len(h.audit.all()) != 0 {
		t.Fatalf("something happened: refreshed %v, audit %s", h.oauth.refreshed(), h.audit.text())
	}
}
