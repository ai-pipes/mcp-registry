package app_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

func startURL(t *testing.T, h *harness, server string) string {
	t.Helper()
	url, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: server})
	if err != nil {
		t.Fatal(err)
	}
	return url
}

func TestStartConnect(t *testing.T) {
	t.Run("remembers the attempt for 10 min and sends PKCE S256 with our redirect URI", func(t *testing.T) {
		h := newHarness(t)
		url, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "gitea"})
		if err != nil {
			t.Fatal(err)
		}
		pc := h.pending.only()
		if pc.UserID() != "alice" || pc.Server() != "gitea" || !pc.ExpiresAt().Equal(at(600_000)) {
			t.Fatalf("pending: %v", pc)
		}
		if pc.State() != stateOf(url) {
			t.Fatalf("the state in the URL %q is not the stored one", stateOf(url))
		}
		if !strings.Contains(url, "code_challenge=S256("+pc.CodeVerifier()+")") {
			t.Fatalf("challenge: %s", url)
		}
		if !strings.Contains(url, "redirect_uri="+h.d.RedirectURI) {
			t.Fatalf("redirect: %s", url)
		}
		if pc.IssuedFor() != h.fingerprintOf(h.gitea()) {
			t.Fatal("the attempt is not bound to the server configuration")
		}
		// No audit record, and no lock.
		if len(h.audit.all()) != 0 {
			t.Fatal("start is audited")
		}
	})

	t.Run("refuses a PAT server, a server the person may not use, an unknown one and a non-personal one", func(t *testing.T) {
		h := newHarness(t)
		for server, want := range map[string]string{
			"yt": "invalid_auth_kind", "secret": "not_found", "nope": "not_found", "shared": "not_found",
		} {
			_, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: server})
			if code(err) != want {
				t.Fatalf("%s: %s, want %s", server, code(err), want)
			}
		}
		if h.pending.size() != 0 {
			t.Fatal("an attempt was stored")
		}
		_, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "yt"})
		if err == nil || err.Error() != `server "yt" does not use OAuth` {
			t.Fatalf("message: %v", err)
		}
		_, err = app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "nope"})
		if err == nil || err.Error() != `server "nope" not found` {
			t.Fatalf("message: %v", err)
		}
	})

	t.Run("refuses an oauth_user server whose OAuth client is gone", func(t *testing.T) {
		h := newHarness(t)
		h.dir.servers = append(h.dir.servers, &fakeServer{slug: "bare", name: "Bare", kind: catalog.AuthOAuthUser, url: "https://mcp.example.com/bare"})
		_, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "bare"})
		if code(err) != "not_found" {
			t.Fatal(code(err))
		}
	})

	t.Run("a failing random source or authorize URL is an error", func(t *testing.T) {
		h := newHarness(t)
		h.pkce.fail = errors.New("no entropy")
		if _, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "gitea"}); err == nil || h.pending.size() != 0 {
			t.Fatal("no error or an attempt stored")
		}
		h.pkce.fail = nil
		h.oauth.authErr = errors.New("bad endpoint")
		if _, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "gitea"}); err == nil {
			t.Fatal("no error")
		}
	})
}

func TestCompleteConnect(t *testing.T) {
	t.Run("exchanges the code with the verifier and stores the tokens sealed", func(t *testing.T) {
		h := newHarness(t)
		h.withRevokeURL()
		h.connect(alice, "gitea")
		pc := h.pending.size()
		if pc != 0 {
			t.Fatal("the attempt was not consumed")
		}
		calls := h.oauth.exchanged()
		if len(calls) != 1 || calls[0].code != "the-code" || calls[0].redirect != h.d.RedirectURI || calls[0].secret != "client-secret" || !calls[0].secretSet || !strings.HasPrefix(calls[0].verifier, "VERIFIERMARK") {
			t.Fatalf("exchange: %+v", calls)
		}
		row, _ := h.store.snap("alice", "gitea")
		if row.Kind != domain.KindOAuth || row.Status != domain.StatusReady || row.IssuedFor != h.fingerprintOf(h.gitea()) {
			t.Fatalf("row: %v", row)
		}
		if row.RevokeURL == nil || *row.RevokeURL != "https://git.example.com/revoke" {
			t.Fatal("the revoke URL in force is not stored")
		}
		if !row.UpdatedAt.Equal(t0) {
			t.Fatal("updatedAt")
		}
		if bytes.Contains(row.Access, []byte("A1")) || bytes.Contains(row.Refresh, []byte("R1")) {
			t.Fatal("tokens stored in plain text")
		}
		recs := h.audit.all()
		if len(recs) != 1 || recs[0].Action != "connection.connect" || *recs[0].ActorID != "alice" || *recs[0].Target != "gitea" || len(recs[0].Meta) != 0 {
			t.Fatalf("audit: %s", h.audit.text())
		}
		r, err := h.resolve("alice", "gitea")
		wantOK(t, r, err, "A1")
		h.noLockViolations()
	})

	t.Run("a provider that sends no refresh token or expiry", func(t *testing.T) {
		h := newHarness(t)
		h.oauth.exchangeOut = app.TokenSet{AccessToken: "A1"}
		h.connect(alice, "gitea")
		row, _ := h.store.snap("alice", "gitea")
		if len(row.Refresh) != 0 || row.ExpiresAt != nil || row.RevokeURL != nil {
			t.Fatalf("row: %v", row)
		}
	})

	t.Run("replaces an earlier connection and makes it ready", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(-1)), "R0", "alice")
		_ = app.MarkExpired(ctx, h.d, app.MarkExpiredCommand{UserID: "alice", Server: "gitea"})
		h.connect(alice, "gitea")
		if h.status("alice", "gitea") != domain.StatusReady {
			t.Fatal("not ready")
		}
	})

	t.Run("someone else's state fails, saves nothing and is NOT consumed: the starter finishes it", func(t *testing.T) {
		h := newHarness(t)
		state := stateOf(startURL(t, h, "gitea"))
		_, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: bob, State: state, Code: "c"})
		if code(err) != "invalid_state" || err.Error() != "connect attempt is unknown or expired" {
			t.Fatalf("%v", err)
		}
		if len(h.oauth.exchanged()) != 0 || h.store.size() != 0 {
			t.Fatal("exchanged or stored")
		}
		if _, ok := h.pending.get(state); !ok {
			t.Fatal("the starter's state was consumed by someone else's callback")
		}
		// Decided deviation oauth-state-kept: the starter completes the same connect.
		res, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"})
		if err != nil || res.Server != "gitea" {
			t.Fatalf("%v %v", res, err)
		}
		if h.status("alice", "gitea") != domain.StatusReady {
			t.Fatal("not ready")
		}
		if _, ok := h.store.snap("bob", "gitea"); ok {
			t.Fatal("the refused person got a connection")
		}
	})

	t.Run("an unknown or used state is invalid_state", func(t *testing.T) {
		h := newHarness(t)
		_, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: "nope", Code: "c"})
		if code(err) != "invalid_state" {
			t.Fatal(code(err))
		}
		state := stateOf(startURL(t, h, "gitea"))
		if _, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"}); err != nil {
			t.Fatal(err)
		}
		before := len(h.oauth.exchanged())
		_, err = app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"})
		if code(err) != "invalid_state" || len(h.oauth.exchanged()) != before {
			t.Fatal("a replay was accepted")
		}
	})

	t.Run("every other refusal consumes the state, as the reference does", func(t *testing.T) {
		type refusal struct {
			name    string
			want    string
			arrange func(h *harness)
		}
		for _, r := range []refusal{
			{"older than 10 min", "invalid_state", func(h *harness) { h.clock.set(at(600_000)) }},
			{"configuration changed", "invalid_state", func(h *harness) {
				c := *h.gitea().oauth
				c.TokenURL = "https://evil.example.com/token"
				h.gitea().oauth = &c
			}},
			{"server disabled", "not_found", func(h *harness) { h.gitea().disabled = true }},
			{"access lost", "not_found", func(h *harness) { h.gitea().groups = []string{"ops"} }},
			{"changed to a PAT server", "invalid_auth_kind", func(h *harness) { h.gitea().kind = catalog.AuthPATUser; h.gitea().oauth = nil }},
			{"the provider refuses the code", "exchange_failed", func(h *harness) { h.oauth.exchangeErr = errors.New("invalid_grant") }},
		} {
			t.Run(r.name, func(t *testing.T) {
				h := newHarness(t)
				state := stateOf(startURL(t, h, "gitea"))
				r.arrange(h)
				_, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"})
				if code(err) != r.want {
					t.Fatalf("%s, want %s", code(err), r.want)
				}
				if _, ok := h.pending.get(state); ok {
					t.Fatal("the state survived the refusal")
				}
				if h.store.size() != 0 {
					t.Fatal("a connection was stored")
				}
				if r.want != "exchange_failed" && len(h.oauth.exchanged()) != 0 {
					t.Fatal("the code was sent to the provider")
				}
				if r.want == "exchange_failed" && err.Error() != "the provider rejected the authorization" {
					t.Fatalf("message: %v", err)
				}
			})
		}
	})

	t.Run("a foreign state is refused even for a server the caller cannot use", func(t *testing.T) {
		h := newHarness(t)
		state := stateOf(startURL(t, h, "gitea"))
		h.gitea().groups = []string{"dev"}
		outsider := who("carol", "ops")
		_, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: outsider, State: state, Code: "c"})
		if code(err) != "invalid_state" {
			t.Fatal(code(err))
		}
	})

	t.Run("a public client exchanges without a secret", func(t *testing.T) {
		h := newHarness(t)
		h.gitea().hasSecret = false
		h.connect(alice, "gitea")
		if c := h.oauth.exchanged(); len(c) != 1 || c[0].secretSet {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("an OAuth server whose client is gone", func(t *testing.T) {
		h := newHarness(t)
		state := stateOf(startURL(t, h, "gitea"))
		h.gitea().oauth = nil
		_, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"})
		// The fingerprint no longer matches either; the first refusal in the reference's order wins.
		if code(err) != "invalid_state" {
			t.Fatal(code(err))
		}
	})

	t.Run("an attempt bound to a configuration without an OAuth client is not_found", func(t *testing.T) {
		h := newHarness(t)
		h.gitea().oauth = nil
		h.pending.m["S"] = domain.NewPendingConnect("S", "alice", "gitea", "V", h.fingerprintOf(h.gitea()), t0)
		_, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: "S", Code: "c"})
		if code(err) != "not_found" || len(h.oauth.exchanged()) != 0 {
			t.Fatal(code(err))
		}
	})

	t.Run("two concurrent callbacks with one state exchange exactly once", func(t *testing.T) {
		h := newHarness(t)
		h.v.setStrict(false) // calls overlap on purpose
		state := stateOf(startURL(t, h, "gitea"))
		const n = 8
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"})
			}()
		}
		close(start)
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch code(err) {
			case "ok":
				ok++
			case "invalid_state":
			default:
				t.Fatalf("unexpected %s", code(err))
			}
		}
		if ok != 1 || len(h.oauth.exchanged()) != 1 {
			t.Fatalf("%d callbacks succeeded, %d exchanges", ok, len(h.oauth.exchanged()))
		}
	})

	t.Run("store and directory errors are errors, not refusals", func(t *testing.T) {
		h := newHarness(t)
		state := stateOf(startURL(t, h, "gitea"))
		h.dir.secretErr = errors.New("key mismatch")
		if _, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"}); err == nil || strings.HasPrefix(code(err), "exchange") {
			t.Fatalf("%v", err)
		}
		h2 := newHarness(t)
		state = stateOf(startURL(t, h2, "gitea"))
		h2.store.failLock = errors.New("db down")
		if _, err := app.CompleteConnect(ctx, h2.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"}); err == nil {
			t.Fatal("no error")
		}
	})
}

func TestSavePAT(t *testing.T) {
	t.Run("stores a PAT sealed for a pat_user server", func(t *testing.T) {
		h := newHarness(t)
		if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: padded}); err != nil {
			t.Fatal(err)
		}
		row, _ := h.store.snap("alice", "yt")
		if row.Kind != domain.KindPAT || row.Status != domain.StatusReady || len(row.Refresh) != 0 || row.ExpiresAt != nil || row.RevokeURL != nil {
			t.Fatalf("row: %v", row)
		}
		if bytes.Contains(row.Access, []byte("perm:xyz")) {
			t.Fatal("plain")
		}
		if row.IssuedFor != h.fingerprintOf(h.dir.find("yt")) {
			t.Fatal("fingerprint")
		}
		recs := h.audit.all()
		if len(recs) != 1 || recs[0].Action != "connection.pat_saved" || *recs[0].ActorID != "alice" || *recs[0].Target != "yt" || len(recs[0].Meta) != 0 {
			t.Fatalf("audit: %s", h.audit.text())
		}
	})

	t.Run("replaces any earlier connection, an OAuth one too, with fresh bytes", func(t *testing.T) {
		h := newHarness(t)
		h.store.put(domain.ConnectOAuth(domain.OAuthConnect{UserID: "alice", Server: "yt", IssuedFor: "x", Now: t0, Tokens: domain.SealedTokens{Access: mustSeal(t, h.box, "old"), Refresh: mustSeal(t, h.box, "oldr")}}))
		if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"}); err != nil {
			t.Fatal(err)
		}
		row, _ := h.store.snap("alice", "yt")
		if row.Kind != domain.KindPAT || len(row.Refresh) != 0 {
			t.Fatalf("row: %v", row)
		}
	})

	t.Run("a server error beats a token error", func(t *testing.T) {
		h := newHarness(t)
		for _, tc := range []struct {
			server, token, want string
		}{
			{"gitea", "t", "invalid_auth_kind"},
			{"gitea", "  ", "invalid_auth_kind"},
			{"secret", "t", "not_found"},
			{"shared", "t", "not_found"},
			{"nope", "t", "not_found"},
			{"yt", "  ", "invalid_pat"},
			{"yt", "a\nb", "invalid_pat"},
			{"yt", "a\x00b", "invalid_pat"},
			{"yt", strings.Repeat("x", 4097), "invalid_pat"},
		} {
			err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: tc.server, Token: tc.token})
			if code(err) != tc.want {
				t.Fatalf("%s %q: %s, want %s", tc.server, tc.token, code(err), tc.want)
			}
		}
		if h.store.size() != 0 || len(h.audit.all()) != 0 {
			t.Fatal("something was stored or audited")
		}
		err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "gitea", Token: "t"})
		if err == nil || err.Error() != `server "gitea" does not use personal access tokens` {
			t.Fatalf("message: %v", err)
		}
		if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: strings.Repeat("x", 4096)}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a failing store is an error", func(t *testing.T) {
		h := newHarness(t)
		h.store.failLock = errors.New("db down")
		if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"}); err == nil {
			t.Fatal("no error")
		}
		if len(h.audit.all()) != 0 {
			t.Fatal("audited a failed save")
		}
	})
}

func TestDisconnect(t *testing.T) {
	disconnect := func(h *harness, server string) error {
		return app.Disconnect(ctx, h.d, app.DisconnectCommand{Principal: alice, Server: server})
	}

	t.Run("deletes, audits first, then revokes the refresh token when a revoke URL is set", func(t *testing.T) {
		h := newHarness(t)
		h.withRevokeURL()
		h.connect(alice, "gitea")
		h.audit.mu.Lock()
		h.audit.list = nil
		h.audit.mu.Unlock()
		h.j.reset()
		if err := disconnect(h, "gitea"); err != nil {
			t.Fatal(err)
		}
		if h.store.size() != 0 {
			t.Fatal("not deleted")
		}
		if got := h.oauth.revoked(); !slices.Equal(got, []string{"R1"}) || h.oauth.revokeSecrets[0] != "client-secret" {
			t.Fatalf("revoked: %v", got)
		}
		recs := h.audit.all()
		if len(recs) != 1 || recs[0].Action != "connection.disconnect" || *recs[0].Target != "gitea" || len(recs[0].Meta) != 0 {
			t.Fatalf("audit: %s", h.audit.text())
		}
		want := []string{"store.delete", "audit:connection.disconnect", "servers.config", "servers.client_secret", "oauth.revoke"}
		if got := h.j.all(); !slices.Equal(got, want) {
			t.Fatalf("sequence:\n got %v\nwant %v", got, want)
		}
	})

	t.Run("a failing revoke is ignored; no revoke URL, a PAT or nothing stored means no revoke", func(t *testing.T) {
		h := newHarness(t)
		h.withRevokeURL()
		h.oauth.revokeErr = errors.New("boom")
		h.connect(alice, "gitea")
		if err := disconnect(h, "gitea"); err != nil {
			t.Fatal(err)
		}
		if h.store.size() != 0 || !slices.Equal(h.oauth.revoked(), []string{"R1"}) {
			t.Fatal("not deleted or no attempt")
		}

		// No refresh token: the access token is revoked.
		h.oauth.mu.Lock()
		h.oauth.revokeCalls = nil
		h.oauth.mu.Unlock()
		h.seedOAuth(tp(at(3_600_000)), "", "alice")
		if err := disconnect(h, "gitea"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(h.oauth.revoked(), []string{"A1"}) {
			t.Fatalf("revoked %v", h.oauth.revoked())
		}

		h.oauth.mu.Lock()
		h.oauth.revokeCalls = nil
		h.oauth.mu.Unlock()
		if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"}); err != nil {
			t.Fatal(err)
		}
		if err := disconnect(h, "yt"); err != nil {
			t.Fatal(err)
		}
		n := len(h.audit.all())
		if err := disconnect(h, "nothing"); err != nil {
			t.Fatal(err)
		}
		if len(h.oauth.revoked()) != 0 || len(h.audit.all()) != n {
			t.Fatal("revoke or audit for a PAT or an absent row")
		}
	})

	t.Run("without a revoke URL nothing is revoked", func(t *testing.T) {
		h := newHarness(t)
		h.connect(alice, "gitea")
		if err := disconnect(h, "gitea"); err != nil {
			t.Fatal(err)
		}
		if len(h.oauth.revoked()) != 0 {
			t.Fatal("revoked")
		}
	})

	t.Run("a changed revoke URL alone: deletes, sends nothing to the new URL", func(t *testing.T) {
		h := newHarness(t)
		h.withRevokeURL()
		h.connect(alice, "gitea")
		c := *h.gitea().oauth
		c.RevokeURL = sp("https://evil.example.com/revoke")
		h.gitea().oauth = &c
		if got := summaryStatus(t, h, "gitea"); got != "ready" {
			t.Fatalf("status %s", got)
		}
		if err := disconnect(h, "gitea"); err != nil {
			t.Fatal(err)
		}
		if h.store.size() != 0 || len(h.oauth.revoked()) != 0 {
			t.Fatal("state or revoke")
		}
	})

	t.Run("a revoke URL added after connecting is not used; connecting again binds it", func(t *testing.T) {
		h := newHarness(t)
		h.connect(alice, "gitea")
		h.withRevokeURL()
		if got := summaryStatus(t, h, "gitea"); got != "ready" {
			t.Fatalf("status %s", got)
		}
		if err := disconnect(h, "gitea"); err != nil {
			t.Fatal(err)
		}
		if len(h.oauth.revoked()) != 0 {
			t.Fatal("revoked to a URL added later")
		}
		h.connect(alice, "gitea")
		if err := disconnect(h, "gitea"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(h.oauth.revoked(), []string{"R1"}) {
			t.Fatalf("revoked %v", h.oauth.revoked())
		}
	})

	t.Run("deletes but sends no revoke to a changed provider or a moved upstream", func(t *testing.T) {
		for _, change := range []func(h *harness){
			func(h *harness) {
				c := *h.gitea().oauth
				c.TokenURL = "https://evil.example.com/token"
				h.gitea().oauth = &c
			},
			func(h *harness) { h.moveGitea() },
		} {
			h := newHarness(t)
			h.withRevokeURL()
			h.connect(alice, "gitea")
			change(h)
			if err := disconnect(h, "gitea"); err != nil {
				t.Fatal(err)
			}
			if h.store.size() != 0 || len(h.oauth.revoked()) != 0 {
				t.Fatal("state or revoke")
			}
		}
	})

	t.Run("a disabled or unknown server: the row is still deleted, nobody is contacted", func(t *testing.T) {
		h := newHarness(t)
		h.withRevokeURL()
		h.connect(alice, "gitea")
		h.gitea().disabled = true
		if err := disconnect(h, "gitea"); err != nil {
			t.Fatal(err)
		}
		if h.store.size() != 0 || len(h.oauth.revoked()) != 0 {
			t.Fatal("state or revoke")
		}
	})

	t.Run("a key mismatch after the delete is an error, the row stays deleted and audited", func(t *testing.T) {
		h := newHarness(t)
		h.withRevokeURL()
		h.connect(alice, "gitea")
		h.box.failOpenOf("R1")
		err := disconnect(h, "gitea")
		if err == nil {
			t.Fatal("no error")
		}
		if h.store.size() != 0 || !slices.Contains(h.audit.actions(), "connection.disconnect") || len(h.oauth.revoked()) != 0 {
			t.Fatal("state")
		}
	})

	t.Run("a client secret error after the delete is an error", func(t *testing.T) {
		h := newHarness(t)
		h.withRevokeURL()
		h.connect(alice, "gitea")
		h.dir.secretErr = errors.New("key mismatch")
		if err := disconnect(h, "gitea"); err == nil {
			t.Fatal("no error")
		}
	})
}

func summaryStatus(t *testing.T, h *harness, server string) string {
	t.Helper()
	list, err := app.ListConnections(ctx, h.d, app.ListQuery{Principal: alice})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.Server == server {
			return string(s.Status)
		}
	}
	t.Fatalf("%s not listed", server)
	return ""
}

func TestListConnections(t *testing.T) {
	t.Run("every usable personal server with ready, expired or missing", func(t *testing.T) {
		h := newHarness(t)
		h.connect(alice, "gitea")
		if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"}); err != nil {
			t.Fatal(err)
		}
		if err := app.MarkExpired(ctx, h.d, app.MarkExpiredCommand{UserID: "alice", Server: "yt"}); err != nil {
			t.Fatal(err)
		}
		got, err := app.ListConnections(ctx, h.d, app.ListQuery{Principal: alice})
		if err != nil {
			t.Fatal(err)
		}
		want := []app.ConnectionSummary{
			{Server: "gitea", Name: "Gitea", AuthKind: catalog.AuthOAuthUser, Status: app.SummaryReady},
			{Server: "yt", Name: "YouTrack", AuthKind: catalog.AuthPATUser, Status: app.SummaryExpired},
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %+v", got)
		}
		got, err = app.ListConnections(ctx, h.d, app.ListQuery{Principal: bob})
		if err != nil {
			t.Fatal(err)
		}
		want = []app.ConnectionSummary{
			{Server: "gitea", Name: "Gitea", AuthKind: catalog.AuthOAuthUser, Status: app.SummaryMissing},
			{Server: "yt", Name: "YouTrack", AuthKind: catalog.AuthPATUser, Status: app.SummaryMissing},
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("a connection for an older server configuration shows as expired, without a write", func(t *testing.T) {
		h := newHarness(t)
		h.connect(alice, "gitea")
		h.moveGitea()
		h.audit.mu.Lock()
		h.audit.list = nil
		h.audit.mu.Unlock()
		if got := summaryStatus(t, h, "gitea"); got != "expired" {
			t.Fatalf("status %s", got)
		}
		if h.status("alice", "gitea") != domain.StatusReady || len(h.audit.all()) != 0 {
			t.Fatal("listing wrote something")
		}
	})

	t.Run("an empty list when there is no personal server; store errors propagate", func(t *testing.T) {
		h := newHarness(t)
		h.dir.servers = nil
		got, err := app.ListConnections(ctx, h.d, app.ListQuery{Principal: alice})
		if err != nil || len(got) != 0 {
			t.Fatalf("%v %v", got, err)
		}
		// Not nil: the REST layer renders [].
		if got == nil {
			t.Fatal("nil slice")
		}
	})
}

// Every audit record is written with a context that outlives a cancelled request.
func TestAuditOutlivesACancelledRequest(t *testing.T) {
	type run func(h *harness, c context.Context) error
	for name, f := range map[string]run{
		"connect": func(h *harness, c context.Context) error {
			state := stateOf(startURL(t, h, "gitea"))
			_, err := app.CompleteConnect(c, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"})
			return err
		},
		"pat": func(h *harness, c context.Context) error {
			return app.SavePAT(c, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"})
		},
		"disconnect": func(h *harness, c context.Context) error {
			h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
			return app.Disconnect(c, h.d, app.DisconnectCommand{Principal: alice, Server: "gitea"})
		},
		"mark expired": func(h *harness, c context.Context) error {
			h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
			return app.MarkExpired(c, h.d, app.MarkExpiredCommand{UserID: "alice", Server: "gitea"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			rctx, cancel := context.WithCancel(ctx)
			defer cancel()
			// The request is cancelled the moment the store has finished.
			h.store.afterOp = cancel
			if err := f(h, rctx); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if len(h.audit.all()) == 0 {
				t.Fatal("no audit record")
			}
			if h.audit.cancelled != 0 {
				t.Fatalf("%d audit calls were made with a cancelled context", h.audit.cancelled)
			}
		})
	}
}

func TestAuditErrorsPropagate(t *testing.T) {
	h := newHarness(t)
	h.audit.err = errors.New("audit down")
	if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"}); err == nil {
		t.Fatal("savePat swallowed the audit error")
	}
	state := stateOf(startURL(t, h, "gitea"))
	if _, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"}); err == nil {
		t.Fatal("completeConnect swallowed the audit error")
	}
	h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
	if err := app.Disconnect(ctx, h.d, app.DisconnectCommand{Principal: alice, Server: "gitea"}); err == nil {
		t.Fatal("disconnect swallowed the audit error")
	}
	if len(h.oauth.revoked()) != 0 {
		t.Fatal("revoked after a failed audit")
	}
}

// The same window in the callback: the client secret is read after the configuration; if the
// server changed in between, nothing is sent (no code, no new secret) and the attempt is refused
// like any other configuration change (consumed, invalid_state).
func TestCompleteConnectRefusesWhenTheConfigurationChangedWhileTheClientSecretWasRead(t *testing.T) {
	h := newHarness(t)
	state := stateOf(startURL(t, h, "gitea"))
	h.dir.onSecret = func() {
		g := h.gitea()
		h.dir.mu.Lock()
		defer h.dir.mu.Unlock()
		o := *g.oauth
		o.TokenURL = "https://evil.example.com/token"
		g.oauth = &o
		g.clientSecret = "the-new-client-secret"
	}
	_, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"})
	if code(err) != "invalid_state" {
		t.Fatalf("%v", err)
	}
	if len(h.oauth.exchanged()) != 0 || h.store.size() != 0 {
		t.Fatal("the code was sent or a connection stored")
	}
	if _, ok := h.pending.get(state); ok {
		t.Fatal("the state was not consumed")
	}
}
