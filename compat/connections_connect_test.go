//go:build compat

package compat_test

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// ---- OAuth connect ----

func TestConnectionsConnectFlow(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	env := connNewOAuth(t, s, admin, connOpts{})
	p := connNewPerson(t, s)
	issuedBefore := len(env.provider.Issued())
	requestsBefore := len(env.provider.Requests())

	if got := connStatus(t, p.c, env.slug); got != "missing" {
		t.Fatalf("status before connecting: %q", got)
	}
	u := connStart(t, p.c, env.slug)

	t.Run("the authorize URL carries PKCE S256, the redirect URI, the scope and the extra parameters", func(t *testing.T) {
		if got := u.Scheme + "://" + u.Host + u.Path; got != env.provider.AuthorizeURL {
			t.Fatalf("authorize URL %q, want %q", got, env.provider.AuthorizeURL)
		}
		q := u.Query()
		want := map[string]string{
			"response_type":         "code",
			"client_id":             env.provider.ClientID,
			"redirect_uri":          s.BaseURL + "/api/connections/callback",
			"code_challenge_method": "S256",
			"scope":                 "repo",
			"access_type":           "offline", // extraAuthorizeParams
		}
		for k, v := range want {
			if got := q.Get(k); got != v {
				t.Errorf("authorize parameter %s = %q, want %q", k, got, v)
			}
		}
		if got := q.Get("code_challenge"); len(got) != 43 { // base64url of a SHA-256
			t.Errorf("code_challenge %q: want 43 characters (S256)", got)
		}
		if got := q.Get("state"); len(got) < 16 {
			t.Errorf("state %q is too short to be unguessable", got)
		}
		// Nothing else: an extra parameter the gateway does not know must not show up.
		for k := range q {
			if _, known := want[k]; !known && k != "code_challenge" && k != "state" {
				t.Errorf("unexpected authorize parameter %q", k)
			}
		}
		// The pending state lasts 10 minutes.
		var left float64
		if err := s.DB.QueryRow(t.Context(), "select extract(epoch from expires_at - now()) from connections.pending where user_id = $1::uuid and server = $2", p.id, env.slug).Scan(&left); err != nil {
			t.Fatalf("pending row: %v", err)
		}
		if left < 580 || left > 601 {
			t.Fatalf("the pending connect expires in %.0f s, want about 600", left)
		}
	})

	back := connAuthorize(t, s, u)
	if !strings.HasPrefix(back, "/api/connections/callback?") || !strings.Contains(back, "state="+url.QueryEscape(u.Query().Get("state"))) {
		t.Fatalf("the provider's redirect %q does not lead back with the state", back)
	}
	if got := connStatus(t, p.c, env.slug); got != "missing" {
		t.Fatalf("status before the callback: %q (an authorize alone must not connect)", got)
	}
	if loc := connCallback(t, p.c, back); loc != "/connections?connected="+url.QueryEscape(env.slug) {
		t.Fatalf("callback Location %q", loc)
	}
	issued := env.provider.Issued()
	if len(issued) != issuedBefore+1 {
		t.Fatalf("the provider issued %d sets, want one more", len(issued)-issuedBefore)
	}
	mine := issued[len(issued)-1]
	connNote(mine.AccessToken, mine.RefreshToken)

	t.Run("the code exchange authenticates the client with HTTP Basic and sends the PKCE verifier", func(t *testing.T) {
		reqs := env.provider.Requests()[requestsBefore:]
		if len(reqs) != 1 || reqs[0].GrantType != "authorization_code" {
			t.Fatalf("token requests: %+v, want exactly one authorization_code exchange", reqs)
		}
		r := reqs[0]
		// RFC 6749 section 2.3.1: id and secret form-encoded before base64. The secret holds + % and :.
		if r.Authorization != connBasic(env.provider) {
			t.Fatalf("Authorization %q, want %q", harness.Redact(r.Authorization), harness.Redact(connBasic(env.provider)))
		}
		if id, secret, ok := r.BasicAuth(); !ok || id != env.provider.ClientID || secret != env.provider.ClientSecret {
			t.Fatalf("Basic credentials decode to %q / %q (ok %v)", id, harness.Redact(secret), ok)
		}
		if got := r.Form.Get("redirect_uri"); got != s.BaseURL+"/api/connections/callback" {
			t.Errorf("redirect_uri %q", got)
		}
		if v := r.Form.Get("code_verifier"); len(v) < 43 || len(v) > 128 { // RFC 7636
			t.Errorf("code_verifier has %d characters", len(v))
		}
		if r.Form.Get("code") == "" {
			t.Error("no code sent")
		}
		// The secret goes in the header only, never in the form.
		if r.Form.Get("client_secret") != "" || strings.Contains(r.Body, "secret") || strings.Contains(r.Body, "gate") {
			t.Errorf("the client secret leaked into the form body: %q", harness.Redact(r.Body))
		}
	})

	t.Run("the connection is ready and stored sealed", func(t *testing.T) {
		if got := connStatus(t, p.c, env.slug); got != "ready" {
			t.Fatalf("status after the callback: %q", got)
		}
		row := connRowOf(t, s, p.id, env.slug)
		if row.Kind != "oauth" || row.Status != "ready" || row.Refresh == nil || row.ExpiresAt == nil {
			t.Fatalf("row: %+v", row)
		}
		if row.RevokeURL == nil || *row.RevokeURL != env.provider.RevokeURL {
			t.Errorf("revoke_url %v, want the revoke URL in force at issue time %q", row.RevokeURL, env.provider.RevokeURL)
		}
		connExpectFreshExpiry(t, s, p.id, env.slug)
		// Sealed: neither the tokens nor their markers are readable in any column, in any encoding.
		for what, raw := range map[string][]byte{"access": row.Access, "refresh": row.Refresh} {
			for _, plain := range []string{mine.AccessToken, mine.RefreshToken, "access-", "refresh-"} {
				connExpectSealed(t, "column "+what, raw, plain)
			}
		}
		if strings.Contains(row.IssuedFor, mine.AccessToken) || strings.Contains(row.IssuedFor, mine.RefreshToken) {
			t.Error("issued_for holds a token")
		}
	})

	t.Run("the audit trail records the connect, with no token", func(t *testing.T) {
		got := connAudit(t, s, "connection.connect", p.id, env.slug)
		if len(got) != 1 || len(got[0].Meta) != 0 {
			t.Fatalf("connection.connect records: %+v, want one with no meta", got)
		}
	})

	t.Run("the connection serves calls with the issued access token", func(t *testing.T) {
		connExpectOK(t, "call", p.call(t, env))
		connEqual(t, "upstream Authorization", connAuths(env.up, "Authorization"), connBearer(mine.AccessToken))
	})

	t.Run("disconnect revokes the refresh token at the revoke URL, deletes the row and is idempotent", func(t *testing.T) {
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d %s", r.Status, r.Body)
		}
		connEqual(t, "revoked tokens", env.provider.Revoked(), []string{mine.RefreshToken})
		attempts := env.provider.RevokeAttempts()
		if len(attempts) != 1 || !attempts[0].Accepted || attempts[0].Authorization != connBasic(env.provider) {
			t.Fatalf("revoke attempts: %+v, want one with the client's Basic credentials", attempts)
		}
		if _, ok := connGetRow(t, s, p.id, env.slug); ok {
			t.Fatal("the row survived the disconnect")
		}
		if got := connStatus(t, p.c, env.slug); got != "missing" {
			t.Fatalf("status after disconnect: %q", got)
		}
		if got := connAudit(t, s, "connection.disconnect", p.id, env.slug); len(got) != 1 || len(got[0].Meta) != 0 {
			t.Fatalf("connection.disconnect records: %+v", got)
		}
		// Nothing left to disconnect: no second revoke, no second audit record.
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("second DELETE: %d", r.Status)
		}
		if n := len(env.provider.RevokeAttempts()); n != 1 {
			t.Fatalf("%d revoke attempts after a second disconnect, want 1", n)
		}
		if got := connAudit(t, s, "connection.disconnect", p.id, env.slug); len(got) != 1 {
			t.Fatalf("a second disconnect wrote %d audit records in all", len(got))
		}
		// The deleted connection no longer serves calls.
		connExpectText(t, "call after disconnect", p.call(t, env), connTextNotConnected(s, env.slug))
	})
}

func TestConnectionsConnectCallbackRefusals(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	env := connNewOAuth(t, s, admin, connOpts{})
	exchanges := func() int { return len(connGrants(env.provider)) } // every token request so far

	t.Run("a state is one-time: the replay is invalid_state and exchanges nothing", func(t *testing.T) {
		p := connNewPerson(t, s)
		u := connStart(t, p.c, env.slug)
		back := connAuthorize(t, s, u)
		if loc := connCallback(t, p.c, back); loc != "/connections?connected="+url.QueryEscape(env.slug) {
			t.Fatalf("first callback: %q", loc)
		}
		after := exchanges()
		if loc := connCallback(t, p.c, back); loc != "/connections?error=invalid_state" {
			t.Fatalf("replayed callback: %q, want /connections?error=invalid_state", loc)
		}
		if exchanges() != after {
			t.Fatal("the replay reached the token endpoint")
		}
		if got := connStatus(t, p.c, env.slug); got != "ready" {
			t.Fatalf("the replay changed the connection: %q", got)
		}
	})

	t.Run("a state belongs to its starter: another person's session is refused", func(t *testing.T) {
		a, b := connNewPerson(t, s), connNewPerson(t, s)
		u := connStart(t, a.c, env.slug)
		back := connAuthorize(t, s, u)
		before := exchanges()
		if loc := connCallback(t, b.c, back); loc != "/connections?error=invalid_state" {
			t.Fatalf("someone else's callback: %q", loc)
		}
		if exchanges() != before {
			t.Fatal("the code was exchanged for the wrong person")
		}
		if connStatus(t, a.c, env.slug) != "missing" || connStatus(t, b.c, env.slug) != "missing" {
			t.Fatal("a refused callback created a connection")
		}
		if s.Fixed(harness.DevOAuthStateKept) {
			// Decided (oauth-state-kept): the refusal leaves the state usable, the starter finishes the
			// same connect.
			if loc := connCallback(t, a.c, back); loc != "/connections?connected="+url.QueryEscape(env.slug) {
				t.Fatalf("the starter's callback after the refused one: %q, want the connect to complete", loc)
			}
			if got := connStatus(t, a.c, env.slug); got != "ready" {
				t.Fatalf("the starter's connection after finishing: %q", got)
			}
			if connStatus(t, b.c, env.slug) != "missing" {
				t.Fatal("the refused person got a connection")
			}
			return
		}
		// 1.0.0 (pinned): the refused attempt consumed the state (one-time), so the starter cannot
		// finish it.
		if loc := connCallback(t, a.c, back); loc != "/connections?error=invalid_state" {
			t.Fatalf("the starter's callback after the refused one: %q", loc)
		}
	})

	t.Run("without a session the callback says unauthenticated", func(t *testing.T) {
		p := connNewPerson(t, s)
		u := connStart(t, p.c, env.slug)
		back := connAuthorize(t, s, u)
		before := exchanges()
		if loc := connCallback(t, s.Anonymous(), back); loc != "/connections?error=unauthenticated" {
			t.Fatalf("anonymous callback with a valid state and code: %q", loc)
		}
		if exchanges() != before || connStatus(t, p.c, env.slug) != "missing" {
			t.Fatal("an anonymous callback must not exchange the code or connect anyone")
		}
		// Control: the same callback with the session connects.
		if loc := connCallback(t, p.c, back); loc != "/connections?connected="+url.QueryEscape(env.slug) {
			t.Fatalf("callback with the session: %q", loc)
		}
	})

	t.Run("a pending connect lives 10 minutes", func(t *testing.T) {
		p := connNewPerson(t, s)
		u := connStart(t, p.c, env.slug)
		back := connAuthorize(t, s, u)
		execSQL(t, s, "update connections.pending set expires_at = now() - interval '1 second' where user_id = $1::uuid and server = $2", p.id, env.slug)
		before := exchanges()
		if loc := connCallback(t, p.c, back); loc != "/connections?error=invalid_state" {
			t.Fatalf("callback of an expired state: %q", loc)
		}
		if exchanges() != before || connStatus(t, p.c, env.slug) != "missing" {
			t.Fatal("an expired state was exchanged")
		}
		// Control: a state with a minute left still works.
		u = connStart(t, p.c, env.slug)
		back = connAuthorize(t, s, u)
		execSQL(t, s, "update connections.pending set expires_at = now() + interval '1 minute' where user_id = $1::uuid and server = $2", p.id, env.slug)
		if loc := connCallback(t, p.c, back); loc != "/connections?connected="+url.QueryEscape(env.slug) {
			t.Fatalf("callback with a minute left: %q", loc)
		}
	})

	t.Run("a state is bound to the server config it started with", func(t *testing.T) {
		p := connNewPerson(t, s)
		u := connStart(t, p.c, env.slug)
		back := connAuthorize(t, s, u)
		connChangeServer(t, admin, env.slug, func(b map[string]any) { connOAuthOf(b)["clientId"] = "other-client" })
		before := exchanges()
		if loc := connCallback(t, p.c, back); loc != "/connections?error=invalid_state" {
			t.Fatalf("callback after the client id changed: %q", loc)
		}
		if exchanges() != before {
			t.Fatal("the code was sent to the provider although the client id changed since the start")
		}
		connChangeServer(t, admin, env.slug, func(b map[string]any) { connOAuthOf(b)["clientId"] = env.provider.ClientID })
		// Reference 1.0.1: a change of the client ID cleared the stored client secret (it is never sent
		// to a new place), so it is entered again with the restored config.
		if r := admin.Do(t, "PUT", "/api/servers/"+env.slug+"/secret", map[string]string{"secret": env.provider.ClientSecret}); r.Status != 204 {
			t.Fatalf("set the client secret again: %d %s", r.Status, r.Body)
		}
		// Control: a fresh start under the restored config connects.
		u = connStart(t, p.c, env.slug)
		if loc := connCallback(t, p.c, connAuthorize(t, s, u)); loc != "/connections?connected="+url.QueryEscape(env.slug) {
			t.Fatalf("fresh connect after restoring the config: %q", loc)
		}
	})

	t.Run("provider errors map to the documented codes", func(t *testing.T) {
		p := connNewPerson(t, s)
		u := connStart(t, p.c, env.slug)
		state := u.Query().Get("state")
		if loc := connCallback(t, p.c, "/api/connections/callback?state="+url.QueryEscape(state)+"&error=access_denied"); loc != "/connections?error=denied" {
			t.Fatalf("the person declined at the provider: %q", loc)
		}
		if loc := connCallback(t, p.c, "/api/connections/callback"); loc != "/connections?error=denied" {
			t.Fatalf("a callback with no code: %q", loc)
		}
		// A code the provider does not know: the exchange fails.
		u = connStart(t, p.c, env.slug)
		if loc := connCallback(t, p.c, "/api/connections/callback?state="+url.QueryEscape(u.Query().Get("state"))+"&code=bogus"); loc != "/connections?error=exchange_failed" {
			t.Fatalf("a bogus code: %q", loc)
		}
		// The provider refusing the gateway's client credentials is an exchange failure too.
		u = connStart(t, p.c, env.slug)
		back := connAuthorize(t, s, u)
		if r := admin.Do(t, "PUT", "/api/servers/"+env.slug+"/secret", map[string]string{"secret": "wrong-client-secret"}); r.Status != 204 {
			t.Fatalf("PUT secret: %d", r.Status)
		}
		loc := connCallback(t, p.c, back)
		if r := admin.Do(t, "PUT", "/api/servers/"+env.slug+"/secret", map[string]string{"secret": env.provider.ClientSecret}); r.Status != 204 {
			t.Fatalf("PUT secret back: %d", r.Status)
		}
		if loc != "/connections?error=exchange_failed" {
			t.Fatalf("a wrong client secret: %q", loc)
		}
		if got := connStatus(t, p.c, env.slug); got != "missing" {
			t.Fatalf("a failed exchange stored a connection: %q", got)
		}
		// Control: with everything right the same person connects.
		connConnect(t, env, p.c)
		if connStatus(t, p.c, env.slug) != "ready" {
			t.Fatal("not connected after a clean flow")
		}
	})

	t.Run("a server disabled or changed to another kind between start and callback", func(t *testing.T) {
		p := connNewPerson(t, s)
		u := connStart(t, p.c, env.slug)
		back := connAuthorize(t, s, u)
		connSetStatus(t, admin, env.slug, "disabled")
		if loc := connCallback(t, p.c, back); loc != "/connections?error=not_found" {
			t.Fatalf("callback for a disabled server: %q", loc)
		}
		connSetStatus(t, admin, env.slug, "active")

		u = connStart(t, p.c, env.slug)
		back = connAuthorize(t, s, u)
		connChangeServer(t, admin, env.slug, func(b map[string]any) {
			b["auth"] = map[string]any{"kind": "pat_user", "header": "Authorization", "template": "Bearer {secret}"}
		})
		if loc := connCallback(t, p.c, back); loc != "/connections?error=invalid_auth_kind" {
			t.Fatalf("callback for a server that no longer uses OAuth: %q", loc)
		}
	})
}

func TestConnectionsStartRefusals(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	member := connNewPerson(t, s)
	env := connNewOAuth(t, s, admin, connOpts{})
	pat := connNewPAT(t, s, admin)
	shared := harness.Unique("shared")
	catalogRegister(t, admin, fakes.ServerInput(env.up, shared, "shared_secret", fakes.WithSecret("shared-key")))
	none := harness.Unique("none")
	catalogRegister(t, admin, fakes.ServerInput(env.up, none, "none"))
	closed := harness.Unique("closed")
	catalogRegister(t, admin, fakes.ServerInput(env.up, closed, "oauth_user", fakes.WithOAuth(env.provider, "repo"), fakes.WithGroups(harness.Unique("nobody"))))
	disabled := harness.Unique("off")
	catalogRegister(t, admin, fakes.ServerInput(env.up, disabled, "oauth_user", fakes.WithOAuth(env.provider, "repo")))
	connSetStatus(t, admin, disabled, "disabled")

	start := func(slug string) harness.Response {
		return member.c.Do(t, "POST", "/api/connections/"+slug+"/start", nil)
	}
	notFound := func(slug string) string {
		return fmt.Sprintf("not_found|server %q not found", slug)
	}

	// Control: an oauth_user server the person may use starts.
	if r := start(env.slug); r.Status != 200 {
		t.Fatalf("start for a usable oauth_user server: %d %s", r.Status, r.Body)
	}
	catalogExpectError(t, "a pat_user server", start(pat.slug), 422, fmt.Sprintf("invalid_auth_kind|server %q does not use OAuth", pat.slug))
	catalogExpectError(t, "an unknown server", start("no-such-server"), 404, notFound("no-such-server"))
	catalogExpectError(t, "a shared_secret server", start(shared), 404, notFound(shared))
	catalogExpectError(t, "a server with auth none", start(none), 404, notFound(none))
	catalogExpectError(t, "a server closed to the person", start(closed), 404, notFound(closed))
	catalogExpectError(t, "a disabled server", start(disabled), 404, notFound(disabled))
	// The same server is startable by an admin (admins use any active server): the refusal above
	// is about the person, not the server.
	if r := admin.Do(t, "POST", "/api/connections/"+closed+"/start", nil); r.Status != 200 {
		t.Fatalf("an admin starting the closed server: %d %s", r.Status, r.Body)
	}
	if r := admin.Do(t, "POST", "/api/connections/"+disabled+"/start", nil); r.Status != 404 {
		t.Fatalf("an admin starting a disabled server: %d, want 404 (nobody uses a disabled server)", r.Status)
	}

	t.Run("extraAuthorizeParams cannot override the flow parameters", func(t *testing.T) {
		for _, key := range []string{"state", "redirect_uri", "scope", "code_challenge", "client_id"} {
			body := fakes.ServerInput(env.up, harness.Unique("clash"), "oauth_user", fakes.WithOAuth(env.provider, "repo"), fakes.WithExtraAuthorizeParams(map[string]string{key: "evil"}))
			// A domain error: the exact message is pinned.
			catalogExpectError(t, "extraAuthorizeParams "+key, admin.Do(t, "POST", "/api/servers", body), 422, fmt.Sprintf("invalid_auth|extra authorize parameter %q is not allowed", key))
		}
		// Control: a harmless extra is accepted and reaches the authorize URL (see the flow test).
		body := fakes.ServerInput(env.up, harness.Unique("extra"), "oauth_user", fakes.WithOAuth(env.provider, "repo"), fakes.WithExtraAuthorizeParams(map[string]string{"prompt": "consent"}))
		catalogRegister(t, admin, body)
		u := connStart(t, admin, body["slug"].(string))
		if u.Query().Get("prompt") != "consent" || u.Query().Get("scope") != "repo" {
			t.Fatalf("authorize URL %s", u.RawQuery)
		}
	})
}

// ---- personal access tokens ----

func TestConnectionsPAT(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	env := connNewPAT(t, s, admin)
	p := connNewPerson(t, s)
	put := func(c *harness.Client, slug string, body any) harness.Response {
		return c.Do(t, "PUT", "/api/connections/"+slug+"/pat", body)
	}

	t.Run("a token is stored sealed and reaches the upstream in the configured header and template", func(t *testing.T) {
		connNote("perm:abc.def-123")
		// Surrounding whitespace is trimmed.
		if r := put(p.c, env.slug, map[string]string{"token": "  perm:abc.def-123 \n"}); r.Status != 204 { //nolint:gosec // G101: a made-up token of a test
			t.Fatalf("PUT pat: %d %s", r.Status, r.Body)
		}
		if got := connStatus(t, p.c, env.slug); got != "ready" {
			t.Fatalf("status: %q", got)
		}
		row := connRowOf(t, s, p.id, env.slug)
		if row.Kind != "pat" || row.Status != "ready" || row.Refresh != nil || row.ExpiresAt != nil || row.RevokeURL != nil {
			t.Fatalf("row: %+v (a pat has no refresh token, no expiry and nothing to revoke)", row)
		}
		connExpectSealed(t, "the stored pat", row.Access, "perm:abc.def-123")
		got := connAudit(t, s, "connection.pat_saved", p.id, env.slug)
		if len(got) != 1 || len(got[0].Meta) != 0 {
			t.Fatalf("connection.pat_saved records: %+v", got)
		}
		res := p.callRef(t, env.ref)
		connExpectOK(t, "call with the pat", res)
		calls := env.up.Calls()
		if len(calls) != 1 {
			t.Fatalf("upstream calls: %d, want one", len(calls))
		}
		connExpectOneCallWith(t, "the pat at the upstream", env.up, "X-Token", "pat perm:abc.def-123")
		if calls[0].Header.Get("Authorization") != "" {
			t.Errorf("the credential also went out in Authorization: %q", harness.Redact(calls[0].Header.Get("Authorization")))
		}
		if outcomes := connOutcomes(t, admin, p.id, env.slug); !slices.Equal(outcomes, []string{"ok"}) {
			t.Fatalf("call log: %v", outcomes)
		}
	})

	t.Run("a new token replaces the old one", func(t *testing.T) {
		env.up.Reset()
		connNote("second-token-xyz")
		if r := put(p.c, env.slug, map[string]string{"token": "second-token-xyz"}); r.Status != 204 {
			t.Fatalf("PUT pat: %d", r.Status)
		}
		connExpectOK(t, "call with the second pat", p.callRef(t, env.ref))
		connEqual(t, "X-Token", connAuths(env.up, "X-Token"), []string{"pat second-token-xyz"})
	})

	t.Run("saving the same token twice stores different bytes (sealed with a fresh nonce)", func(t *testing.T) {
		q := connNewPerson(t, s)
		connNote("same-token-value")
		var stored [2][]byte
		for i := range stored {
			if r := put(q.c, env.slug, map[string]string{"token": "same-token-value"}); r.Status != 204 {
				t.Fatalf("PUT pat: %d %s", r.Status, r.Body)
			}
			stored[i] = connRowOf(t, s, q.id, env.slug).Access
			connExpectSealed(t, "the stored pat", stored[i], "same-token-value")
		}
		if string(stored[0]) == string(stored[1]) {
			t.Fatal("the same token produced the same stored bytes twice: the sealing is deterministic")
		}
	})

	t.Run("refused tokens change nothing", func(t *testing.T) {
		before := connRowOf(t, s, p.id, env.slug)
		long := strings.Repeat("a", 4097)
		cases := []struct {
			name   string
			body   any
			status int
			code   string // "code|message" of a domain error, compared exactly
			zod    string // set for a zod body error: only status, code and this path are compared
		}{
			{"blank", map[string]string{"token": "   "}, 422, "invalid_pat|the token must be a non-empty single line", ""},
			{"empty", map[string]string{"token": ""}, 422, "", "token"},
			{"CR LF inside", map[string]string{"token": "abc\r\nX-Evil: 1"}, 422, //nolint:gosec // G101: a made-up token of a test
				"invalid_pat|the token must be a non-empty single line", ""},
			{"LF inside", map[string]string{"token": "abc\ndef"}, 422, "invalid_pat|the token must be a non-empty single line", ""},
			{"NUL inside", map[string]string{"token": "abc\x00def"}, 422, "invalid_pat|the token must be a non-empty single line", ""},
			{"4097 characters", map[string]string{"token": long}, 422, "", "token"},
			{"no token field", map[string]string{}, 422, "", "token"},
		}
		for _, c := range cases {
			if c.zod != "" {
				expectZodBodyError(t, c.name, put(p.c, env.slug, c.body), c.status, c.zod)
			} else {
				catalogExpectError(t, c.name, put(p.c, env.slug, c.body), c.status, c.code)
			}
		}
		after := connRowOf(t, s, p.id, env.slug)
		if string(after.Access) != string(before.Access) || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatal("a refused token changed the stored connection")
		}
		// Control: 4096 characters are fine.
		if r := put(p.c, env.slug, map[string]string{"token": strings.Repeat("b", 4096)}); r.Status != 204 {
			t.Fatalf("a 4096-character token: %d %s", r.Status, r.Body)
		}
		connNote(strings.Repeat("b", 4096))
		if r := put(p.c, env.slug, map[string]string{"token": "second-token-xyz"}); r.Status != 204 {
			t.Fatalf("restoring the token: %d", r.Status)
		}
	})

	t.Run("only a pat_user server takes a token", func(t *testing.T) {
		oauth := connNewOAuth(t, s, admin, connOpts{})
		shared := harness.Unique("shared")
		catalogRegister(t, admin, fakes.ServerInput(env.up, shared, "shared_secret", fakes.WithSecret("shared-key")))
		none := harness.Unique("none")
		catalogRegister(t, admin, fakes.ServerInput(env.up, none, "none"))
		catalogExpectError(t, "an oauth_user server", put(p.c, oauth.slug, map[string]string{"token": "t-value-1"}), 422,
			fmt.Sprintf("invalid_auth_kind|server %q does not use personal access tokens", oauth.slug))
		catalogExpectError(t, "a shared_secret server", put(p.c, shared, map[string]string{"token": "t-value-1"}), 404, fmt.Sprintf("not_found|server %q not found", shared))
		catalogExpectError(t, "a server with auth none", put(p.c, none, map[string]string{"token": "t-value-1"}), 404, fmt.Sprintf("not_found|server %q not found", none))
		catalogExpectError(t, "an unknown server", put(p.c, "no-such-server", map[string]string{"token": "t-value-1"}), 404, `not_found|server "no-such-server" not found`)
		if _, ok := connGetRow(t, s, p.id, oauth.slug); ok {
			t.Fatal("a pat was stored for an oauth_user server")
		}
		// A server closed to the person is not found either.
		closed := harness.Unique("closed")
		catalogRegister(t, admin, fakes.ServerInput(env.up, closed, "pat_user", fakes.WithGroups(harness.Unique("nobody"))))
		catalogExpectError(t, "a pat_user server closed to the person", put(p.c, closed, map[string]string{"token": "t-value-1"}), 404, fmt.Sprintf("not_found|server %q not found", closed))
		if r := put(admin, closed, map[string]string{"token": "t-value-1"}); r.Status != 204 {
			t.Fatalf("an admin saving a pat for the closed server: %d %s", r.Status, r.Body)
		}
		connNote("t-value-1")
	})

	t.Run("the upstream rejecting the token expires the connection at once, with no retry", func(t *testing.T) {
		q2 := connNewPerson(t, s)
		connNote("rejected-pat-2")
		if r := put(q2.c, env.slug, map[string]string{"token": "rejected-pat-2"}); r.Status != 204 {
			t.Fatalf("PUT pat: %d", r.Status)
		}
		env.up.Reset()
		env.up.FailWith(401)
		connExpectText(t, "call with a rejected pat", q2.callRef(t, env.ref), connTextExpired(s, env.slug))
		// The upstream refuses at the session handshake, so no tools/call is recorded: count requests.
		reqs := env.up.Requests()
		if len(reqs) != 1 || reqs[0].Header.Get("X-Token") != "pat rejected-pat-2" {
			t.Fatalf("the upstream saw %d request(s), want exactly one attempt (a pat is never retried)", len(reqs))
		}
		if got := connStatus(t, q2.c, env.slug); got != "expired" {
			t.Fatalf("status: %q", got)
		}
		if row := connRowOf(t, s, q2.id, env.slug); row.Status != "expired" {
			t.Fatalf("row status %q", row.Status)
		}
		connExpectExpiredAudit(t, s, q2.id, env.slug, "rejected_upstream", 1)
		if outcomes := connOutcomes(t, admin, q2.id, env.slug); !slices.Equal(outcomes, []string{"denied:expired"}) {
			t.Fatalf("call log: %v", outcomes)
		}

		// An expired connection is not tried again: the upstream sees nothing, even though it works now.
		env.up.Reset()
		connExpectText(t, "call after expiry", q2.callRef(t, env.ref), connTextExpired(s, env.slug))
		if n := len(env.up.Requests()); n != 0 {
			t.Fatalf("an expired connection reached the upstream %d time(s)", n)
		}
		connExpectExpiredAudit(t, s, q2.id, env.slug, "rejected_upstream", 1) // not recorded twice

		// Reconnecting (a new token) is what helps.
		connNote("fresh-pat-3")
		if r := put(q2.c, env.slug, map[string]string{"token": "fresh-pat-3"}); r.Status != 204 {
			t.Fatalf("PUT new pat: %d", r.Status)
		}
		connExpectOK(t, "call with the new pat", q2.callRef(t, env.ref))
		connEqual(t, "X-Token", connAuths(env.up, "X-Token"), []string{"pat fresh-pat-3"})
	})

	t.Run("an unconnected person is told where to connect, spends no quota and reaches no upstream", func(t *testing.T) {
		limited := connNewPAT(t, s, admin, fakes.WithDailyLimit(3))
		q := connNewPerson(t, s)
		connExpectText(t, "call without a connection", q.callRef(t, limited.ref), connTextNotConnected(s, limited.slug))
		if n := len(limited.up.Requests()); n != 0 {
			t.Fatalf("the upstream saw %d request(s) from an unconnected person", n)
		}
		if n := connQuota(t, s, q.id, limited.slug); n != 0 {
			t.Fatalf("quota spent: %d (an unconnected service spends none)", n)
		}
		if outcomes := connOutcomes(t, admin, q.id, limited.slug); !slices.Equal(outcomes, []string{"denied:needs_connection"}) {
			t.Fatalf("call log: %v", outcomes)
		}
		// Control: once connected the same call goes through and spends one.
		connNote("limited-pat-1")
		if r := put(q.c, limited.slug, map[string]string{"token": "limited-pat-1"}); r.Status != 204 {
			t.Fatalf("PUT pat: %d %s", r.Status, r.Body)
		}
		connExpectOK(t, "call after connecting", q.callRef(t, limited.ref))
		if n := connQuota(t, s, q.id, limited.slug); n != 1 {
			t.Fatalf("quota after one call: %d", n)
		}
	})

	t.Run("disconnect deletes the pat and contacts nobody", func(t *testing.T) {
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d", r.Status)
		}
		if _, ok := connGetRow(t, s, p.id, env.slug); ok {
			t.Fatal("the row survived")
		}
		if got := connStatus(t, p.c, env.slug); got != "missing" {
			t.Fatalf("status: %q", got)
		}
		if got := connAudit(t, s, "connection.disconnect", p.id, env.slug); len(got) != 1 {
			t.Fatalf("connection.disconnect records: %d", len(got))
		}
	})
}

// ---- people ----

func TestConnectionsArePersonal(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	env := connNewOAuth(t, s, admin, connOpts{})
	a, b := connNewPerson(t, s), connNewPerson(t, s)

	// Two people, two connections to one server, each call carries the caller's own token.
	if connStatus(t, a.c, env.slug) != "missing" || connStatus(t, b.c, env.slug) != "missing" {
		t.Fatal("a person starts without a connection, whatever the admin's or anyone else's")
	}
	tokenA := connConnect(t, env, a.c)
	if got := connStatus(t, a.c, env.slug); got != "ready" {
		t.Fatalf("A after connecting: %q", got)
	}
	if got := connStatus(t, b.c, env.slug); got != "missing" {
		t.Fatalf("B sees A's connection as %q, want missing", got)
	}
	// B cannot use A's connection: B is told to connect, and the upstream is not called.
	env.up.Reset()
	connExpectText(t, "B before connecting", b.call(t, env), connTextNotConnected(s, env.slug))
	if n := len(env.up.Requests()); n != 0 {
		t.Fatalf("the upstream saw %d request(s) from B, who has no connection", n)
	}
	tokenB := connConnect(t, env, b.c)
	if tokenA.AccessToken == tokenB.AccessToken {
		t.Fatal("the provider issued the same token twice")
	}
	env.up.Reset()
	for _, who := range []struct {
		name string
		p    connPerson
		want string
	}{{"A", a, tokenA.AccessToken}, {"B", b, tokenB.AccessToken}, {"A", a, tokenA.AccessToken}, {"B", b, tokenB.AccessToken}} {
		env.up.Reset()
		connExpectOK(t, who.name+" call", who.p.call(t, env))
		connEqual(t, who.name+"'s credential at the upstream", connAuths(env.up, "Authorization"), connBearer(who.want))
	}

	t.Run("one person's expiry, refresh and disconnect do not touch the other", func(t *testing.T) {
		before := len(env.provider.Requests())
		connExpireIn(t, s, a.id, env.slug, 59)
		env.up.Reset()
		connExpectOK(t, "A refreshing", a.call(t, env))
		refreshes := connRefreshTokens(env.provider)
		if got := refreshes[len(refreshes)-1]; got != tokenA.RefreshToken || len(env.provider.Requests())-before != 1 {
			t.Fatalf("A's refresh presented %q (requests %d)", harness.Redact(got), len(env.provider.Requests())-before)
		}
		env.up.Reset()
		connExpectOK(t, "B after A refreshed", b.call(t, env))
		connEqual(t, "B's credential", connAuths(env.up, "Authorization"), connBearer(tokenB.AccessToken))

		// A's connection expires (the provider refuses A's refresh token); B is unaffected.
		env.provider.Script(fakes.Error{Status: 400, Code: "invalid_grant"})
		connExpireIn(t, s, a.id, env.slug, 59)
		connExpectText(t, "A refused", a.call(t, env), connTextExpired(s, env.slug))
		if connStatus(t, a.c, env.slug) != "expired" || connStatus(t, b.c, env.slug) != "ready" {
			t.Fatalf("statuses A %q B %q", connStatus(t, a.c, env.slug), connStatus(t, b.c, env.slug))
		}
		env.up.Reset()
		connExpectOK(t, "B after A expired", b.call(t, env))

		// B disconnects: B's refresh token is revoked, A's row is not touched.
		if r := b.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d", r.Status)
		}
		connEqual(t, "revoked", env.provider.Revoked(), []string{tokenB.RefreshToken})
		if _, ok := connGetRow(t, s, a.id, env.slug); !ok {
			t.Fatal("B's disconnect deleted A's row")
		}
		if connStatus(t, a.c, env.slug) != "expired" || connStatus(t, b.c, env.slug) != "missing" {
			t.Fatal("statuses after B disconnected")
		}
	})

	t.Run("the list holds the servers that need a personal connection that the person may use", func(t *testing.T) {
		pat := connNewPAT(t, s, admin)
		shared := harness.Unique("shared")
		catalogRegister(t, admin, fakes.ServerInput(env.up, shared, "shared_secret", fakes.WithSecret("shared-key")))
		none := harness.Unique("none")
		catalogRegister(t, admin, fakes.ServerInput(env.up, none, "none"))
		closed := harness.Unique("closed")
		catalogRegister(t, admin, fakes.ServerInput(env.up, closed, "pat_user", fakes.WithGroups(harness.Unique("nobody"))))
		off := harness.Unique("off")
		catalogRegister(t, admin, fakes.ServerInput(env.up, off, "pat_user"))
		connSetStatus(t, admin, off, "disabled")

		c := connNewPerson(t, s)
		r := c.c.Do(t, "GET", "/api/connections", nil)
		var list []struct {
			Server   string `json:"server"`
			Name     string `json:"name"`
			AuthKind string `json:"authKind"`
			Status   string `json:"status"`
		}
		r.JSON(t, "ConnectionSummary[]", &list)
		got := map[string][3]string{}
		for _, e := range list {
			got[e.Server] = [3]string{e.Name, e.AuthKind, e.Status}
		}
		for slug, want := range map[string][3]string{
			env.slug: {fakes.DisplayName(env.slug), "oauth_user", "missing"},
			pat.slug: {fakes.DisplayName(pat.slug), "pat_user", "missing"},
		} {
			if got[slug] != want {
				t.Errorf("%s: %v, want %v", slug, got[slug], want)
			}
		}
		for _, slug := range []string{shared, none, closed, off} {
			if _, listed := got[slug]; listed {
				t.Errorf("%s is listed: a member's list holds only the personal servers they may use", slug)
			}
		}
		// Control: an admin may use the closed server.
		if connStatus(t, admin, closed) != "missing" {
			t.Error("the admin's list lacks the closed pat_user server")
		}
	})

	t.Run("the agent learns each person's connection state and where to connect", func(t *testing.T) {
		pat := connNewPAT(t, s, admin)
		shared := harness.Unique("shared")
		catalogRegister(t, admin, fakes.ServerInput(env.up, shared, "shared_secret", fakes.WithSecret("shared-key")))
		c := connNewPerson(t, s)
		find := func(slug string) catalogListed {
			for _, l := range catalogListServers(t, c.mcp) {
				if l.Slug == slug {
					return l
				}
			}
			t.Fatalf("list_servers does not list %s", slug)
			return catalogListed{}
		}
		link := s.BaseURL + "/connections"
		if l := find(env.slug); l.Connection != "needs_connection" || l.ConnectURL != link {
			t.Fatalf("before connecting: %+v", l)
		}
		if l := find(pat.slug); l.Connection != "needs_connection" || l.ConnectURL != link {
			t.Fatalf("pat server before connecting: %+v", l)
		}
		if l := find(shared); l.Connection != "not_required" || l.ConnectURL != "" {
			t.Fatalf("shared_secret server: %+v", l)
		}
		connConnect(t, env, c.c)
		if l := find(env.slug); l.Connection != "ready" || l.ConnectURL != "" {
			t.Fatalf("after connecting: %+v", l)
		}
		env.provider.Script(fakes.Error{Status: 400, Code: "invalid_grant"})
		connExpireIn(t, s, c.id, env.slug, 59)
		connExpectText(t, "call", c.call(t, env), connTextExpired(s, env.slug))
		if l := find(env.slug); l.Connection != "expired" || l.ConnectURL != link {
			t.Fatalf("after the refresh was refused: %+v", l)
		}
	})
}

func TestConnectionsWebAPIRules(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	env := connNewOAuth(t, s, admin, connOpts{})
	pat := connNewPAT(t, s, admin)
	p := connNewPerson(t, s)

	const signIn = "unauthenticated|sign in required"
	anon := s.Anonymous()
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/connections"},
		{"POST", "/api/connections/" + env.slug + "/start"},
		{"PUT", "/api/connections/" + pat.slug + "/pat"},
		{"DELETE", "/api/connections/" + env.slug},
	} {
		var body any
		if c.method == "PUT" {
			body = map[string]string{"token": "tok-value-1"}
		}
		catalogExpectError(t, "anonymous "+c.method+" "+c.path, anon.Do(t, c.method, c.path, body), 401, signIn)
	}
	// A token of the agent (sk_) never works on the web API.
	bearer := "Authorization: Bearer " + p.c.Token(t, "bearer-check")
	catalogExpectError(t, "an sk_ token on GET", p.c.Do(t, "GET", "/api/connections", nil, bearer), 401, "unauthenticated|tokens do not work on the web API")
	if connStatus(t, p.c, pat.slug) != "missing" {
		t.Fatal("setup")
	}
	// Mutations need the app's own Origin.
	for _, origin := range []string{"Origin: https://evil.example", "Origin:"} {
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{"POST", "/api/connections/" + env.slug + "/start", nil},
			{"PUT", "/api/connections/" + pat.slug + "/pat", map[string]string{"token": "tok-value-1"}},
			{"DELETE", "/api/connections/" + pat.slug, nil},
		} {
			catalogExpectError(t, origin+" "+c.method+" "+c.path, p.c.Do(t, c.method, c.path, c.body, origin), 403, "csrf|cross-origin request rejected")
		}
	}
	if connStatus(t, p.c, pat.slug) != "missing" {
		t.Fatal("a cross-origin request stored a token")
	}
	// Control: the same requests with the right Origin work.
	if r := p.c.Do(t, "PUT", "/api/connections/"+pat.slug+"/pat", map[string]string{"token": "tok-value-1"}); r.Status != 204 {
		t.Fatalf("PUT with the right Origin: %d", r.Status)
	}
	connNote("tok-value-1")
}
