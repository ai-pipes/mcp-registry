//go:build compat

package compat_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func TestConnectionsRefresh(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	env := connNewOAuth(t, s, admin, connOpts{})

	t.Run("a token is refreshed when fewer than 60 s remain, not before", func(t *testing.T) {
		p, issued := connConnected(t, env)
		requests := len(env.provider.Requests())

		// The documented threshold is 60 s. The test sits 3 s on each side of it so that the time
		// between moving expires_at and the gateway reading it (scheduling, a slow CI host) cannot
		// push the "no refresh" side below the threshold.
		connExpireIn(t, s, p.id, env.slug, 63)
		connExpectOK(t, "call with 63 s left", p.call(t, env))
		if n := len(env.provider.Requests()); n != requests {
			t.Fatalf("a refresh at 63 s left: %d new token request(s)", n-requests)
		}
		connEqual(t, "token with 63 s left", connAuths(env.up, "Authorization"), connBearer(issued.AccessToken))

		env.up.Reset()
		connExpireIn(t, s, p.id, env.slug, 57)
		connExpectOK(t, "call with 57 s left", p.call(t, env))
		reqs := env.provider.Requests()[requests:]
		if len(reqs) != 1 || reqs[0].GrantType != "refresh_token" {
			t.Fatalf("token requests at 57 s left: %+v, want exactly one refresh_token grant", reqs)
		}
		if got := reqs[0].Form.Get("refresh_token"); got != issued.RefreshToken {
			t.Fatalf("the refresh presented %q, want the issued refresh token", harness.Redact(got))
		}
		if reqs[0].Authorization != connBasic(env.provider) {
			t.Fatalf("the refresh authenticated with %q, want HTTP Basic of the client", harness.Redact(reqs[0].Authorization))
		}
		got := connAuths(env.up, "Authorization")
		if len(got) != 1 || got[0] == "Bearer "+issued.AccessToken || !strings.HasPrefix(got[0], "Bearer access-") {
			t.Fatalf("after the refresh the upstream saw %v, want a new access token", redactAll(got))
		}
		connNote(strings.TrimPrefix(got[0], "Bearer "))
		if row := connRowOf(t, s, p.id, env.slug); row.Status != "ready" || row.ExpiresAt == nil {
			t.Fatalf("row after the refresh: %+v", redactRow(row))
		}
		connExpectFreshExpiry(t, s, p.id, env.slug)

		// The provider did not rotate: the next refresh presents the previous refresh token again.
		connExpireIn(t, s, p.id, env.slug, 59)
		connExpectOK(t, "second refresh", p.call(t, env))
		connEqual(t, "refresh tokens presented", connRefreshTokens(env.provider)[len(connRefreshTokens(env.provider))-2:], []string{issued.RefreshToken, issued.RefreshToken})
	})

	t.Run("the refresh token from the response replaces the old one, else the previous is kept", func(t *testing.T) {
		p, issued := connConnected(t, env)
		before := len(connRefreshTokens(env.provider))
		connNote("acc-rot-1", "ref-rot-1", "acc-rot-2", "acc-rot-3")

		env.provider.Script(fakes.Ok{AccessToken: "acc-rot-1", RefreshToken: "ref-rot-1"}) //nolint:gosec // G101: made-up tokens of a test
		connExpireIn(t, s, p.id, env.slug, 59)
		connExpectOK(t, "first refresh", p.call(t, env))

		env.provider.Script(fakes.Ok{AccessToken: "acc-rot-2"}) //nolint:gosec // G101: made-up token; the answer has no refresh token
		connExpireIn(t, s, p.id, env.slug, 59)
		connExpectOK(t, "second refresh", p.call(t, env))

		env.provider.Script(fakes.Ok{AccessToken: "acc-rot-3"}) //nolint:gosec // G101: made-up token of a test
		connExpireIn(t, s, p.id, env.slug, 59)
		connExpectOK(t, "third refresh", p.call(t, env))

		// The provider invalidated the issued refresh token when it rotated: a gateway that kept
		// presenting it would have been refused (and the calls above would have failed).
		connEqual(t, "refresh tokens presented", connRefreshTokens(env.provider)[before:], []string{issued.RefreshToken, "ref-rot-1", "ref-rot-1"})
		connEqual(t, "access tokens at the upstream", connAuths(env.up, "Authorization"), connBearer("acc-rot-1", "acc-rot-2", "acc-rot-3"))
		if got := connStatus(t, p.c, env.slug); got != "ready" {
			t.Fatalf("status: %q", got)
		}
	})

	t.Run("without a refresh token the access token is used until it runs out, then the connection expires", func(t *testing.T) {
		p, issued := connConnected(t, env)
		requests := len(env.provider.Requests())
		execSQL(t, s, "update connections.connections set refresh = null where user_id = $1::uuid and server = $2", p.id, env.slug)

		connExpireIn(t, s, p.id, env.slug, 30) // inside the 60 s window, but nothing to refresh with
		connExpectOK(t, "call with 30 s left and no refresh token", p.call(t, env))
		connEqual(t, "token", connAuths(env.up, "Authorization"), connBearer(issued.AccessToken))
		if len(env.provider.Requests()) != requests {
			t.Fatal("the gateway contacted the provider without a refresh token")
		}
		if got := connStatus(t, p.c, env.slug); got != "ready" {
			t.Fatalf("status with 30 s left: %q", got)
		}

		env.up.Reset()
		connExpireIn(t, s, p.id, env.slug, -1)
		connExpectText(t, "call after the token ran out", p.call(t, env), connTextExpired(s, env.slug))
		if n := len(env.up.Requests()); n != 0 || len(env.provider.Requests()) != requests {
			t.Fatalf("an expired token reached the upstream (%d request(s)) or the provider", n)
		}
		if got := connStatus(t, p.c, env.slug); got != "expired" {
			t.Fatalf("status: %q", got)
		}
		connExpectExpiredAudit(t, admin, p.id, env.slug, "token_expired", 1)
		if outcomes := connOutcomes(t, admin, p.id, env.slug); !slices.Equal(outcomes, []string{"ok", "denied:expired"}) {
			t.Fatalf("call log: %v", outcomes)
		}
	})

	t.Run("an expired connection is not refreshed again until the person reconnects", func(t *testing.T) {
		p, _ := connConnected(t, env)
		env.provider.Script(fakes.Error{Status: 400, Code: "invalid_grant"})
		connExpireIn(t, s, p.id, env.slug, 59)
		connExpectText(t, "call with a refused refresh", p.call(t, env), connTextExpired(s, env.slug))
		requests := len(env.provider.Requests())
		for range 3 {
			connExpectText(t, "call on an expired connection", p.call(t, env), connTextExpired(s, env.slug))
		}
		// Even with a token that looks fresh in the row and a provider that would accept the refresh.
		connExpireIn(t, s, p.id, env.slug, 3600)
		connExpectText(t, "call on an expired connection with a fresh expires_at", p.call(t, env), connTextExpired(s, env.slug))
		if n := len(env.provider.Requests()); n != requests {
			t.Fatalf("%d token request(s) for an expired connection", n-requests)
		}
		if n := len(env.up.Requests()); n != 0 {
			t.Fatalf("the upstream saw %d request(s) from an expired connection", n)
		}
		connExpectExpiredAudit(t, admin, p.id, env.slug, "refresh_rejected", 1)

		// Reconnecting is what helps: a new row, ready, serving calls with the new token.
		issued := connConnect(t, env, p.c)
		connExpectOK(t, "call after reconnecting", p.call(t, env))
		connEqual(t, "token after reconnecting", connAuths(env.up, "Authorization"), connBearer(issued.AccessToken))
		if got := connStatus(t, p.c, env.slug); got != "ready" {
			t.Fatalf("status after reconnecting: %q", got)
		}
	})
}

func TestConnectionsRefreshFailures(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	env := connNewOAuth(t, s, admin, connOpts{})

	// A token endpoint that says no ends the grant.
	rejections := []struct {
		name string
		step fakes.Error
	}{
		{"400 invalid_grant", fakes.Error{Status: 400, Code: "invalid_grant"}},
		{"401 invalid_grant", fakes.Error{Status: 401, Code: "invalid_grant"}},
		{"400 with another OAuth error", fakes.Error{Status: 400, Code: "invalid_request"}},
		{"401 with another OAuth error", fakes.Error{Status: 401, Code: "invalid_token"}},
		{"400 unauthorized_client (Gitea's unparseable refresh token)", fakes.Error{Status: 400, Code: "unauthorized_client"}},
		{"403 invalid_grant", fakes.Error{Status: 403, Code: "invalid_grant"}},
	}
	for _, c := range rejections {
		t.Run("the provider refusing the refresh expires the connection: "+c.name, func(t *testing.T) {
			p, issued := connConnected(t, env)
			env.provider.Script(c.step)
			connExpireIn(t, s, p.id, env.slug, 59) // the token still works for a while: the refusal must still win
			connExpectText(t, "call", p.call(t, env), connTextExpired(s, env.slug))
			if got := connRefreshTokens(env.provider); len(got) == 0 || got[len(got)-1] != issued.RefreshToken {
				t.Fatalf("the refresh was not attempted with the issued token: %v", redactAll(got))
			}
			if n := len(env.up.Requests()); n != 0 {
				t.Fatalf("the upstream saw %d request(s) although the refresh was refused", n)
			}
			if got := connStatus(t, p.c, env.slug); got != "expired" {
				t.Fatalf("status: %q", got)
			}
			if outcomes := connOutcomes(t, admin, p.id, env.slug); !slices.Equal(outcomes, []string{"denied:expired"}) {
				t.Fatalf("call log: %v", outcomes)
			}
			connExpectExpiredAudit(t, admin, p.id, env.slug, "refresh_rejected", 1)
			// Not refreshed again.
			n := len(env.provider.Requests())
			connExpectText(t, "second call", p.call(t, env), connTextExpired(s, env.slug))
			if len(env.provider.Requests()) != n {
				t.Fatal("an expired connection was refreshed again")
			}
		})
	}

	// No answer, or a provider that is down, says nothing about the grant: nothing is stored.
	outages := []struct {
		name string
		step fakes.TokenStep
	}{
		{"the connection dropped without an answer (a network error)", fakes.Drop{}},
		{"503", fakes.Error{Status: 503, Code: "temporarily_unavailable"}},
		{"500", fakes.Error{Status: 500, Code: "server_error"}},
		{"502 with a non-OAuth body", fakes.Error{Status: 502, Code: ""}},
	}
	for _, c := range outages {
		t.Run("a provider that is down stores nothing: "+c.name, func(t *testing.T) {
			p, issued := connConnected(t, env)
			env.provider.Script(c.step, c.step)

			// The access token has not run out: it is used.
			connExpireIn(t, s, p.id, env.slug, 59)
			rowBefore := connRowOf(t, s, p.id, env.slug)
			requests := len(env.provider.Requests())
			connExpectOK(t, "call with 59 s left and the provider down", p.call(t, env))
			connExpectSameRow(t, "the unexpired token with the provider down", rowBefore, connRowOf(t, s, p.id, env.slug))
			if n := len(env.provider.Requests()) - requests; n != 1 {
				t.Fatalf("%d token request(s), want the refresh to be attempted once", n)
			}
			connEqual(t, "token", connAuths(env.up, "Authorization"), connBearer(issued.AccessToken))
			if got := connStatus(t, p.c, env.slug); got != "ready" {
				t.Fatalf("status: %q", got)
			}

			// The access token has run out: nothing to use, and nothing to blame the person for.
			env.up.Reset()
			connExpireIn(t, s, p.id, env.slug, -5)
			rowBefore = connRowOf(t, s, p.id, env.slug)
			connExpectText(t, "call with an expired token and the provider down", p.call(t, env), connTextUnavailable(env.slug))
			if n := len(env.up.Requests()); n != 0 {
				t.Fatalf("the upstream saw %d request(s) although no credential was available", n)
			}
			connExpectSameRow(t, "the expired token with the provider down", rowBefore, connRowOf(t, s, p.id, env.slug))
			if got := connStatus(t, p.c, env.slug); got != "ready" {
				t.Fatalf("status: %q", got)
			}
			if n := len(connAudit(t, admin, "connection.expired", p.id, env.slug)); n != 0 {
				t.Fatalf("%d connection.expired record(s) for an outage", n)
			}
			if outcomes := connOutcomes(t, admin, p.id, env.slug); !slices.Equal(outcomes, []string{"ok", "transport_error"}) {
				t.Fatalf("call log: %v", outcomes)
			}

			// Control: the provider recovers and the very same connection works again.
			env.up.Reset()
			connExpectOK(t, "call after the provider recovered", p.call(t, env))
			got := connAuths(env.up, "Authorization")
			if len(got) != 1 || got[0] == "Bearer "+issued.AccessToken {
				t.Fatalf("after recovery the upstream saw %v, want a refreshed token", redactAll(got))
			}
			connNote(strings.TrimPrefix(got[0], "Bearer "))
			if got := connRefreshTokens(env.provider); got[len(got)-1] != issued.RefreshToken {
				t.Fatal("the refresh token changed during the outage")
			}
		})
	}

	// The gateway's own client credentials failing is the admin's problem, not the person's.
	clientFailures := []struct {
		name string
		step fakes.Error
	}{
		{"401 invalid_client", fakes.Error{Status: 401, Code: "invalid_client"}},
		{"400 invalid_client (Gitea)", fakes.Error{Status: 400, Code: "invalid_client"}},
		{"401 unauthorized_client", fakes.Error{Status: 401, Code: "unauthorized_client"}},
	}
	for _, c := range clientFailures {
		t.Run("the provider refusing the gateway's client credentials is unavailable, not expired: "+c.name, func(t *testing.T) {
			p, _ := connConnected(t, env)
			env.provider.Script(c.step)
			connExpireIn(t, s, p.id, env.slug, -5)
			rowBefore := connRowOf(t, s, p.id, env.slug)
			requests := len(env.provider.Requests())
			connExpectText(t, "call", p.call(t, env), connTextUnavailable(env.slug))
			if n := len(env.provider.Requests()) - requests; n != 1 {
				t.Fatalf("%d token request(s), want one", n)
			}
			if row := connRowOf(t, s, p.id, env.slug); row.Status != "ready" || !row.UpdatedAt.Equal(rowBefore.UpdatedAt) {
				t.Fatalf("a client credentials failure changed the connection: %+v", row)
			}
			if got := connStatus(t, p.c, env.slug); got != "ready" {
				t.Fatalf("status: %q", got)
			}
			if n := len(connAudit(t, admin, "connection.expired", p.id, env.slug)); n != 0 {
				t.Fatalf("%d connection.expired record(s)", n)
			}
			if outcomes := connOutcomes(t, admin, p.id, env.slug); !slices.Equal(outcomes, []string{"transport_error"}) {
				t.Fatalf("call log: %v", outcomes)
			}
			// Control: the script is spent and the same connection refreshes.
			env.up.Reset()
			connExpectOK(t, "call after the failure", p.call(t, env))
			if got := connStatus(t, p.c, env.slug); got != "ready" {
				t.Fatalf("status after: %q", got)
			}
		})
	}

	t.Run("a really wrong client secret at the provider is unavailable too, and fixing it heals every connection", func(t *testing.T) {
		p, _ := connConnected(t, env)
		connExpireIn(t, s, p.id, env.slug, -5)
		if r := admin.Do(t, "PUT", "/api/servers/"+env.slug+"/secret", map[string]string{"secret": "not-the-secret"}); r.Status != 204 {
			t.Fatalf("PUT secret: %d", r.Status)
		}
		connExpectText(t, "call with a wrong client secret", p.call(t, env), connTextUnavailable(env.slug))
		last := env.provider.Requests()[len(env.provider.Requests())-1]
		if _, secret, ok := last.BasicAuth(); !ok || secret != "not-the-secret" {
			t.Fatalf("the refresh did not use the server's current client secret")
		}
		if got := connStatus(t, p.c, env.slug); got != "ready" {
			t.Fatalf("status: %q", got)
		}
		if r := admin.Do(t, "PUT", "/api/servers/"+env.slug+"/secret", map[string]string{"secret": env.provider.ClientSecret}); r.Status != 204 {
			t.Fatalf("PUT secret back: %d", r.Status)
		}
		connExpectOK(t, "call with the secret fixed", p.call(t, env))
	})

	t.Run("an unresponsive provider is no answer, not a refusal (takes about 10 s: the token endpoint timeout)", func(t *testing.T) {
		p, issued := connConnected(t, env)
		env.provider.Script(fakes.Hang{})
		connExpireIn(t, s, p.id, env.slug, 59)
		rowBefore := connRowOf(t, s, p.id, env.slug)
		started := time.Now()
		connExpectOK(t, "call with 59 s left and a hanging provider", p.call(t, env))
		took := time.Since(started)
		if took < 8*time.Second || took > 25*time.Second {
			t.Fatalf("the call took %s: the gateway should give up on the token endpoint after about 10 s", took)
		}
		connEqual(t, "token", connAuths(env.up, "Authorization"), connBearer(issued.AccessToken))
		connExpectSameRow(t, "the unexpired token with a hanging provider", rowBefore, connRowOf(t, s, p.id, env.slug))
		if got := connStatus(t, p.c, env.slug); got != "ready" {
			t.Fatalf("status: %q", got)
		}
		if n := len(connAudit(t, admin, "connection.expired", p.id, env.slug)); n != 0 {
			t.Fatalf("%d connection.expired record(s) for a timeout", n)
		}
	})
}

// ---- single flight ----

func TestConnectionsSingleFlight(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	env := connNewOAuth(t, s, admin, connOpts{})
	env.provider.SetRefreshDelay(700 * time.Millisecond) // long enough for every concurrent call to arrive while one refresh runs

	t.Run("20 concurrent calls with an expiring token make exactly one refresh request", func(t *testing.T) {
		p, issued := connConnected(t, env)
		connNote("acc-flight")
		env.provider.Script(fakes.Ok{AccessToken: "acc-flight"}) // a second refresh would get a random token instead
		connExpireIn(t, s, p.id, env.slug, 59)
		before := len(connRefreshTokens(env.provider))

		started := time.Now()
		results := p.callMany(t, env.ref, 20)
		for i, r := range results {
			connExpectOK(t, fmt.Sprintf("concurrent call %d", i), r)
		}
		if took := time.Since(started); took < 700*time.Millisecond {
			t.Fatalf("the calls took %s: they cannot have waited for a provider answer held for 700 ms", took)
		}
		refreshes := connRefreshTokens(env.provider)[before:]
		connEqual(t, "refresh requests", refreshes, []string{issued.RefreshToken})
		got := connAuths(env.up, "Authorization")
		if len(got) != 20 {
			t.Fatalf("the upstream saw %d call(s), want 20", len(got))
		}
		for _, h := range got {
			if h != "Bearer acc-flight" {
				t.Fatalf("a call reached the upstream with %q, want every one to use the single refresh's token", harness.Redact(h))
			}
		}
		// Control: with a fresh token, 20 more calls refresh nothing.
		env.up.Reset()
		for i, r := range p.callMany(t, env.ref, 20) {
			connExpectOK(t, fmt.Sprintf("follow-up call %d", i), r)
		}
		if n := len(connRefreshTokens(env.provider)[before:]); n != 1 {
			t.Fatalf("%d refresh requests after the follow-up calls, want still 1", n)
		}
	})

	t.Run("different people do not share a refresh", func(t *testing.T) {
		a, issuedA := connConnected(t, env)
		b, issuedB := connConnected(t, env)
		c, issuedC := connConnected(t, env)
		for _, p := range []connPerson{a, b, c} {
			connExpireIn(t, s, p.id, env.slug, 59)
		}
		before := len(connRefreshTokens(env.provider))
		var wg sync.WaitGroup
		results := make([][]connResult, 3)
		// A distinct access token per refresh: the i-th refresh request gets the i-th token.
		scripted := []string{"acc-pp-1", "acc-pp-2", "acc-pp-3"}
		connNote(scripted...)
		for _, tk := range scripted {
			env.provider.Script(fakes.Ok{AccessToken: tk}) //nolint:gosec // G101: made-up tokens of a test
		}
		for i, p := range []connPerson{a, b, c} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = p.callMany(t, env.ref, 7)
			}()
		}
		wg.Wait()
		for i, rs := range results {
			for j, r := range rs {
				connExpectOK(t, fmt.Sprintf("person %d call %d", i, j), r)
			}
		}
		got := connRefreshTokens(env.provider)[before:]
		want := []string{issuedA.RefreshToken, issuedB.RefreshToken, issuedC.RefreshToken}
		slices.Sort(got)
		slices.Sort(want)
		connEqual(t, "refresh requests (one per person, each with their own refresh token)", got, want)
		// Three different access tokens went upstream, seven calls each.
		counts := map[string]int{}
		for _, h := range connAuths(env.up, "Authorization") {
			counts[h]++
		}
		if len(counts) != 3 {
			t.Fatalf("%d distinct tokens at the upstream, want 3", len(counts))
		}
		for h, n := range counts {
			if n != 7 {
				t.Fatalf("token %s used %d times, want 7", harness.Redact(h), n)
			}
		}
		// Attribution: each person's refresh got the token scripted for its place in the provider's
		// order, and a sequential call per person now carries exactly that person's own token.
		order := connRefreshTokens(env.provider)[before:]
		for _, who := range []struct {
			name    string
			p       connPerson
			refresh string
		}{{"A", a, issuedA.RefreshToken}, {"B", b, issuedB.RefreshToken}, {"C", c, issuedC.RefreshToken}} {
			i := slices.Index(order, who.refresh)
			if i < 0 {
				t.Fatalf("%s's refresh token was never presented", who.name)
			}
			env.up.Reset()
			connExpectOK(t, who.name+"'s sequential call", who.p.call(t, env))
			connExpectCallsWith(t, who.name+"'s credential at the upstream", env.up, "Authorization", "Bearer "+scripted[i])
		}
	})
}

// ---- the upstream rejecting the token ----

func TestConnectionsUpstreamRejectsTheToken(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	env := connNewOAuth(t, s, admin, connOpts{server: []fakes.ServerOpt{fakes.WithDailyLimit(5)}})

	t.Run("one 401: a forced refresh and one retry, the quota is spent once", func(t *testing.T) {
		p, issued := connConnected(t, env)
		connNote("acc-fresh-1")
		env.provider.Script(fakes.Ok{AccessToken: "acc-fresh-1"})
		env.up.SetRequireHeader("Authorization", "Bearer acc-fresh-1")
		defer env.up.SetRequireHeader("", "")
		requests := len(env.provider.Requests())

		connExpectOK(t, "call", p.call(t, env))
		// Raw request counts: a refused attempt is exactly one request (the 401 arrives at the
		// handshake), so the old token was offered once, and the retry is exactly one successful call.
		connEqual(t, "credentials the upstream was offered, one per request",
			connRequestHeaders(env.up, "Authorization"),
			append(connBearer(issued.AccessToken), connRepeat("Bearer acc-fresh-1", connCallRequests)...))
		reqs := env.provider.Requests()[requests:]
		if len(reqs) != 1 || reqs[0].GrantType != "refresh_token" || reqs[0].Form.Get("refresh_token") != issued.RefreshToken {
			t.Fatalf("token requests: %+v, want one refresh with the issued refresh token", reqs)
		}
		if n := connQuota(t, s, p.id, env.slug); n != 1 {
			t.Fatalf("quota spent: %d, want 1 (the retry does not spend it twice)", n)
		}
		if outcomes := connOutcomes(t, admin, p.id, env.slug); !slices.Equal(outcomes, []string{"ok"}) {
			t.Fatalf("call log: %v, want one ok entry", outcomes)
		}
		// The refreshed token is now the one in use: no more refreshes.
		env.up.Reset()
		connExpectOK(t, "second call", p.call(t, env))
		connExpectCallsWith(t, "second call credential", env.up, "Authorization", "Bearer acc-fresh-1")
		if n := len(env.provider.Requests()) - requests; n != 1 {
			t.Fatalf("%d token requests after two calls, want 1", n)
		}
		if got := connStatus(t, p.c, env.slug); got != "ready" {
			t.Fatalf("status: %q", got)
		}
	})

	t.Run("a second 401 expires the connection", func(t *testing.T) {
		p, issued := connConnected(t, env)
		connNote("acc-refused-1")
		env.provider.Script(fakes.Ok{AccessToken: "acc-refused-1"})
		env.up.SetRequireHeader("Authorization", "Bearer never-valid")
		defer env.up.SetRequireHeader("", "")
		requests := len(env.provider.Requests())

		connExpectText(t, "call", p.call(t, env), connTextExpired(s, env.slug))
		// Exactly two refused requests in all: the old token once, the refreshed token once.
		connEqual(t, "credentials the upstream was offered, one per request", connRequestHeaders(env.up, "Authorization"), connBearer(issued.AccessToken, "acc-refused-1"))
		if n := len(env.provider.Requests()) - requests; n != 1 {
			t.Fatalf("%d token requests, want exactly one forced refresh", n)
		}
		if n := connQuota(t, s, p.id, env.slug); n != 1 {
			t.Fatalf("quota spent: %d, want 1", n)
		}
		if got := connStatus(t, p.c, env.slug); got != "expired" {
			t.Fatalf("status: %q", got)
		}
		connExpectExpiredAudit(t, admin, p.id, env.slug, "rejected_upstream", 1)
		if outcomes := connOutcomes(t, admin, p.id, env.slug); !slices.Equal(outcomes, []string{"denied:expired"}) {
			t.Fatalf("call log: %v", outcomes)
		}
		// Expired: nothing more is sent anywhere and the quota stays where it was.
		env.up.Reset()
		requests = len(env.provider.Requests())
		connExpectText(t, "call on the expired connection", p.call(t, env), connTextExpired(s, env.slug))
		if len(env.up.Requests()) != 0 || len(env.provider.Requests()) != requests {
			t.Fatal("an expired connection contacted the upstream or the provider")
		}
		if n := connQuota(t, s, p.id, env.slug); n != 1 {
			t.Fatalf("quota after the call on an expired connection: %d, want 1 (credentials come before the quota)", n)
		}
	})

	t.Run("a refused forced refresh expires the connection with the refusal as the reason", func(t *testing.T) {
		p, _ := connConnected(t, env)
		env.provider.Script(fakes.Error{Status: 400, Code: "invalid_grant"})
		env.up.SetRequireHeader("Authorization", "Bearer never-valid")
		defer env.up.SetRequireHeader("", "")
		connExpectText(t, "call", p.call(t, env), connTextExpired(s, env.slug))
		if got := connStatus(t, p.c, env.slug); got != "expired" {
			t.Fatalf("status: %q", got)
		}
		connExpectExpiredAudit(t, admin, p.id, env.slug, "refresh_rejected", 1)
	})

	t.Run("a provider that is down during the forced refresh is unavailable, and the connection stays", func(t *testing.T) {
		p, _ := connConnected(t, env)
		env.provider.Script(fakes.Error{Status: 503, Code: "temporarily_unavailable"})
		env.up.SetRequireHeader("Authorization", "Bearer never-valid")
		defer env.up.SetRequireHeader("", "")
		connExpectText(t, "call", p.call(t, env), connTextUnavailable(env.slug))
		if got := connStatus(t, p.c, env.slug); got != "ready" {
			t.Fatalf("status: %q", got)
		}
		if n := len(connAudit(t, admin, "connection.expired", p.id, env.slug)); n != 0 {
			t.Fatalf("%d connection.expired record(s) for an outage", n)
		}
		if outcomes := connOutcomes(t, admin, p.id, env.slug); !slices.Equal(outcomes, []string{"transport_error"}) {
			t.Fatalf("call log: %v", outcomes)
		}
	})

	t.Run("the daily limit counts calls per person and refuses the next one", func(t *testing.T) {
		p, _ := connConnected(t, env)
		for range 5 {
			connExpectOK(t, "call within the limit", p.call(t, env))
		}
		res := p.call(t, env)
		if !res.IsError || !strings.HasPrefix(res.Text, "Daily limit for "+env.slug+" reached. Resets at ") {
			t.Fatalf("the sixth call: %+v, want the daily limit text", res)
		}
		if n := connQuota(t, s, p.id, env.slug); n != 5 {
			t.Fatalf("quota: %d, want 5", n)
		}
	})
}

// ---- the admin's own connection syncs the catalog ----

func TestConnectionsAdminSync(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo", "ping")})
	provider := connWatch(fakes.StartOAuthProvider(t))
	slug := harness.Unique("sync")
	catalogRegister(t, admin, fakes.ServerInput(up, slug, "oauth_user", fakes.WithOAuth(provider, "repo"), fakes.WithTimeoutMs(5000)))
	connNote(provider.ClientSecret)
	env := &connEnv{s: s, admin: admin, slug: slug, ref: slug + ".echo", up: up, provider: provider}
	me := whoami(t, admin).UserID

	sync := func() harness.Response { return admin.Do(t, "POST", "/api/servers/"+slug+"/sync", nil) }

	// Without a connection of its own the admin cannot sync an oauth_user server.
	up.Reset()
	catalogExpectError(t, "sync before the admin connected", sync(), 400, "needs_connection|"+catalogMsgNeedsConnection)
	if n := len(up.Requests()); n != 0 {
		t.Fatalf("the upstream was contacted %d time(s) without a credential", n)
	}

	issued := connConnect(t, env, admin)
	if res := catalogSyncOK(t, admin, slug); res.Added != 2 {
		t.Fatalf("sync after connecting: %+v, want 2 added", res)
	}
	connExpectEveryRequestWith(t, "the sync", up, "Authorization", "Bearer "+issued.AccessToken)

	// A member cannot sync (admin only), and a member's connection is not the admin's.
	member := connNewPerson(t, s)
	catalogExpectError(t, "a member syncing", member.c.Do(t, "POST", "/api/servers/"+slug+"/sync", nil), 403, catalogAdminErr)

	// The sync refreshes an expiring token like any other use of it.
	up.Reset()
	connNote("acc-sync-1")
	provider.Script(fakes.Ok{AccessToken: "acc-sync-1"}) //nolint:gosec // G101: made-up token of a test
	connExpireIn(t, s, me, slug, 59)
	if res := catalogSyncOK(t, admin, slug); res.Added+res.Changed+res.Removed != 0 {
		t.Fatalf("a second sync: %+v", res)
	}
	connExpectEveryRequestWith(t, "the sync after a refresh", up, "Authorization", "Bearer acc-sync-1")

	// The provider down with an unusable token: the sync fails and says why.
	provider.Script(fakes.Error{Status: 503, Code: "temporarily_unavailable"})
	connExpireIn(t, s, me, slug, -5)
	catalogExpectError(t, "sync with the provider down", sync(), 502, "sync_failed|The OAuth provider is unavailable")
	if v := catalogGet(t, admin, slug); v.syncError() != "The OAuth provider is unavailable" {
		t.Fatalf("lastSyncError %q", v.syncError())
	}

	// An expired connection stops the sync, and the admin is told to connect again.
	execSQL(t, s, "update connections.connections set status = 'expired' where user_id = $1::uuid and server = $2", me, slug)
	catalogExpectError(t, "sync with an expired connection", sync(), 400, "needs_connection|"+catalogMsgNeedsConnection)
}
