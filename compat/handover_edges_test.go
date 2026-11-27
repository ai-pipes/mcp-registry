//go:build compat

package compat_test

// What one image leaves behind for the next beyond the ordinary rows of the hand-over scenarios
// (platform_reference_db_test.go) and the upgrade scenario (upgrade_test.go): the connection rows
// of unusual shape and the half-finished connect, and a key rotation after the hand-over. Each is
// produced through the product of the image that runs first, never written by hand, and judged
// through the product of the image that runs second:
//   - an `expired` connection that still holds a refresh token (the provider refused a refresh):
//     the agent gets the reconnect answer, nothing goes to the provider or the upstream, and
//     reconnecting works;
//   - a member's access token about to expire: one refresh with the STORED refresh token;
//   - a connect started (state, PKCE verifier, the 10-minute window) and not finished before the
//     switch: the callback on the other image completes it; a started connect whose window ran out
//     is refused with invalid_state and exchanges nothing;
//   - a provider that issues no refresh token, on a server whose OAuth settings had no revoke URL
//     at issue time (revoke_url NULL; the URL is added afterwards): the token is used until it runs
//     out, then the connection expires without a refresh attempt, and a disconnect revokes nothing;
//   - a provider that issues no expires_in (expires_at NULL in both implementations), and one whose
//     expires_in lies beyond 100 years (the Go server stores NULL, the TypeScript one a date more
//     than 150 years ahead): the row is read as it is, the token is used, nothing is refreshed;
//   - a rotation: the next image starts with SECRETS_KEY = a new key and SECRETS_KEY_OLD = the key
//     the first image sealed with, its own CLI runs rotate-secrets-key (every value re-sealed), it
//     starts again on the new key alone, and a call through /mcp on a shared_secret, a pat_user and
//     an oauth_user server delivers the right credential; an expiring OAuth token is refreshed with
//     the re-sealed refresh token.

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// handOverOAuthServer registers an oauth_user server on its own provider and upstream; the provider
// answers code exchanges as shape says (before the administrator connects, so the administrator's
// connection has that shape too), the administrator connects and the tools are synced.
func handOverOAuthServer(t *testing.T, s *harness.Stack, admin *harness.Client, shape fakes.CodeGrant, opts ...fakes.ServerOpt) *connEnv {
	t.Helper()
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	provider := connWatch(fakes.StartOAuthProvider(t)) // every token it hands out is looked for by connLeakCheck
	provider.SetCodeGrant(shape)
	slug := harness.Unique("hand-edge")
	catalogRegister(t, admin, fakes.ServerInput(up, slug, "oauth_user", append([]fakes.ServerOpt{
		fakes.WithOAuth(provider, "repo"),
		fakes.WithExtraAuthorizeParams(map[string]string{"access_type": "offline"}),
		fakes.WithTimeoutMs(5000),
	}, opts...)...))
	env := &connEnv{s: s, admin: admin, slug: slug, ref: slug + ".echo", up: up, provider: provider}
	connConnect(t, env, admin)
	if r := catalogSyncOK(t, admin, slug); r.Added != 1 {
		t.Fatalf("sync of %s with the administrator's token: %+v, want one tool added", slug, r)
	}
	up.Reset()
	return env
}

// handOverCall makes one call_tool of ref with the person's token on a NEW MCP session of the image
// the stack runs now (a session opened before a switch is not reused).
func handOverCall(t *testing.T, s *harness.Stack, p connPerson, ref string) connResult {
	t.Helper()
	res := catalogCallMCP(t, s.MCP(t, p.lazy.token), "call_tool", map[string]any{"tool": ref, "arguments": map[string]any{"text": "hi"}})
	return connResult{IsError: res.IsError, Text: catalogText(res)}
}

// handOverStr shows an optional text for a failure message: nil or the value.
func handOverStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *p)
}

// handOverKeptLogs collects the app logs of the containers a scenario replaces: a swap removes the
// container and its log with it, so without this the leak scans at the end of a scenario would see
// only the last image's log.
type handOverKeptLogs struct{ logs []string }

// keep saves the log of the app container running now (call it right before a swap).
func (k *handOverKeptLogs) keep(t *testing.T, s *harness.Stack) {
	t.Helper()
	if k != nil {
		k.logs = append(k.logs, s.Logs(t))
	}
}

// add saves a log the scenario already holds (a refused start's, whose container is gone).
func (k *handOverKeptLogs) add(logs string) { k.logs = append(k.logs, logs) }

// scanAtEnd registers a cleanup that fails when a kept log contains a value secrets returns (read
// at the end, so values noted later count). Register it after connLeakCheck: cleanups run last
// first, so it reads the noted values before connLeakCheck consumes them.
func (k *handOverKeptLogs) scanAtEnd(t *testing.T, secrets func() []string) {
	t.Helper()
	t.Cleanup(func() {
		values := secrets()
		if len(k.logs) == 0 || len(values) == 0 {
			t.Errorf("the scan of the replaced containers' logs has %d log(s) and %d secret(s): it proves nothing", len(k.logs), len(values))
			return
		}
		for i, logs := range k.logs {
			for _, v := range values {
				for _, form := range []string{v, url.QueryEscape(v)} {
					if len(v) >= 6 && strings.Contains(logs, form) {
						t.Errorf("the log of replaced container #%d contains the secret %s", i+1, harness.Redact(v))
					}
				}
			}
		}
	})
}

// handOverPendingLeft is the time left in a started connect's window, by the database's clock.
func handOverPendingLeft(t *testing.T, s *harness.Stack, uid, slug string) float64 {
	t.Helper()
	var left float64
	if err := s.DB.QueryRow(t.Context(), "select extract(epoch from expires_at - now())::float8 from connections.pending where user_id = $1::uuid and server = $2", uid, slug).Scan(&left); err != nil {
		t.Fatalf("the started connect of %s: %v", slug, err)
	}
	return left
}

func handOverRefreshes(p *fakes.OAuthProvider, from int) []fakes.TokenRequest {
	var out []fakes.TokenRequest
	for _, r := range p.Requests()[from:] {
		if r.GrantType == "refresh_token" {
			out = append(out, r)
		}
	}
	return out
}

// handOverEdges are the unusual rows one image leaves behind (see the file header).
type handOverEdges struct {
	adminID string
	// e1: an ordinary provider (refresh tokens, a revoke URL).
	e1                    *connEnv
	expired, refresher    connPerson
	refresherTok          fakes.IssuedTokens
	pending, late         connPerson
	pendingBack, lateBack string // the callback paths the provider sent the browsers to
	// e2: a provider that issues no refresh token; no revoke URL when the tokens were issued.
	e2          *connEnv
	noRefresh   connPerson
	noRefreshAt fakes.IssuedTokens
	// e3: a provider that issues no expires_in (noExpiry) or one beyond 100 years (farExpiry).
	e3                      *connEnv
	noExpiry, farExpiry     connPerson
	noExpiryAt, farExpiryAt fakes.IssuedTokens
	rows                    map[string]connRow // "who/slug" as the first image left them
}

func createHandOverEdges(t *testing.T, s *harness.Stack, admin *harness.Client) *handOverEdges {
	t.Helper()
	e := &handOverEdges{adminID: whoami(t, admin).UserID, rows: map[string]connRow{}}

	e.e1 = handOverOAuthServer(t, s, admin, fakes.CodeGrant{})
	// An expired row that still holds a refresh token: the provider refuses the refresh.
	e.expired = connNewPerson(t, s)
	connConnect(t, e.e1, e.expired.c)
	e.e1.provider.Script(fakes.Error{Status: 400, Code: "invalid_grant"})
	connExpireIn(t, s, e.expired.id, e.e1.slug, -10)
	connExpectText(t, "the first image: a call with a refused refresh", handOverCall(t, s, e.expired, e.e1.ref), connTextExpired(s, e.e1.slug))
	if row := connRowOf(t, s, e.expired.id, e.e1.slug); row.Status != "expired" || row.Refresh == nil {
		t.Fatalf("the first image left the refused connection as %s (refresh token kept: %v), want expired with its refresh token", row.Status, row.Refresh != nil)
	}
	e.refresher = connNewPerson(t, s)
	e.refresherTok = connConnect(t, e.e1, e.refresher.c)
	// Two connects started and not finished: the browsers are back from the provider with a code.
	for _, c := range []struct {
		p    *connPerson
		back *string
	}{{&e.pending, &e.pendingBack}, {&e.late, &e.lateBack}} {
		*c.p = connNewPerson(t, s)
		*c.back = connAuthorize(t, s, connStart(t, c.p.c, e.e1.slug))
		if left := handOverPendingLeft(t, s, c.p.id, e.e1.slug); left < 590 || left > 601 {
			t.Fatalf("the first image gave the started connect %.1f s, want about 600", left)
		}
	}

	e.e2 = handOverOAuthServer(t, s, admin, fakes.CodeGrant{NoRefreshToken: true}, fakes.WithoutRevokeURL())
	e.noRefresh = connNewPerson(t, s)
	e.noRefreshAt = connConnect(t, e.e2, e.noRefresh.c)
	for who, id := range map[string]string{"admin": e.adminID, "member": e.noRefresh.id} {
		if row := connRowOf(t, s, id, e.e2.slug); row.Refresh != nil || row.RevokeURL != nil || row.ExpiresAt == nil {
			t.Fatalf("the first image stored the %s's token of a provider without refresh tokens and a server without a revoke URL as refresh %v, revoke_url %v, expires_at %v; want no refresh token, no revoke URL, an expiry",
				who, row.Refresh != nil, handOverStr(row.RevokeURL), row.ExpiresAt)
		}
	}
	// The revoke URL comes afterwards: the rows keep NULL, the URL in force at issue time. A change
	// of the revoke URL clears the client secret (the secret's destination changed), so it is set
	// again; the connections stay ready (the revoke URL is not part of the fingerprint).
	connChangeServer(t, admin, e.e2.slug, func(b map[string]any) { connOAuthOf(b)["revokeUrl"] = e.e2.provider.RevokeURL })
	if r := admin.Do(t, "PUT", "/api/servers/"+e.e2.slug+"/secret", map[string]any{"secret": e.e2.provider.ClientSecret}); r.Status != 204 {
		t.Fatalf("PUT the client secret again: %d %s", r.Status, r.Body)
	}
	if got := connStatus(t, e.noRefresh.c, e.e2.slug); got != "ready" {
		t.Fatalf("after the revoke URL was added the connection is %q, want ready", got)
	}

	e.e3 = handOverOAuthServer(t, s, admin, fakes.CodeGrant{ExpiresIn: -1})
	e.noExpiry = connNewPerson(t, s)
	e.noExpiryAt = connConnect(t, e.e3, e.noExpiry.c)
	if row := connRowOf(t, s, e.noExpiry.id, e.e3.slug); row.ExpiresAt != nil {
		t.Fatalf("a token without expires_in was stored with expires_at %v, want NULL", row.ExpiresAt)
	}
	e.e3.provider.SetCodeGrant(fakes.CodeGrant{ExpiresIn: 5_000_000_000}) // about 158 years
	e.farExpiry = connNewPerson(t, s)
	e.farExpiryAt = connConnect(t, e.e3, e.farExpiry.c)
	if row := connRowOf(t, s, e.farExpiry.id, e.e3.slug); row.ExpiresAt != nil && row.ExpiresAt.Before(time.Now().AddDate(150, 0, 0)) {
		t.Fatalf("a token with expires_in beyond 100 years was stored with expires_at %v, want NULL or more than 150 years ahead", row.ExpiresAt)
	} else {
		t.Logf("a token with expires_in beyond 100 years: the first image stored expires_at %v (NULL is the Go server's, a date the TypeScript one's)", row.ExpiresAt)
	}
	for _, p := range []connPerson{e.noExpiry, e.farExpiry} {
		e.rows[p.id+"/"+e.e3.slug] = connRowOf(t, s, p.id, e.e3.slug)
	}
	return e
}

// checkHandOverEdges judges, against the image the stack runs now, the rows the other image left.
func checkHandOverEdges(t *testing.T, s *harness.Stack, e *handOverEdges) {
	t.Helper()
	t.Run("an expired row stays expired: the reconnect answer, no refresh; reconnecting works", func(t *testing.T) {
		env := e.e1
		requests := len(env.provider.Requests())
		env.up.Reset()
		connExpectText(t, "a call on the expired connection", handOverCall(t, s, e.expired, env.ref), connTextExpired(s, env.slug))
		if n := len(env.provider.Requests()) - requests; n != 0 {
			t.Fatalf("%d token request(s) for an expired connection", n)
		}
		if n := len(env.up.Calls()); n != 0 {
			t.Fatalf("the upstream got %d call(s) from an expired connection", n)
		}
		if got := connStatus(t, e.expired.c, env.slug); got != "expired" {
			t.Fatalf("status %q, want expired", got)
		}
		issued := connConnect(t, env, e.expired.c)
		connExpectOK(t, "a call after reconnecting", handOverCall(t, s, e.expired, env.ref))
		connEqual(t, "the token after reconnecting", connAuths(env.up, "Authorization"), connBearer(issued.AccessToken))
		if got := connStatus(t, e.expired.c, env.slug); got != "ready" {
			t.Fatalf("status after reconnecting %q, want ready", got)
		}
	})

	t.Run("a member's access token about to expire is refreshed once with the stored refresh token", func(t *testing.T) {
		env := e.e1
		connExpireIn(t, s, e.refresher.id, env.slug, 10)
		requests := len(env.provider.Requests())
		env.up.Reset()
		connExpectOK(t, "a call with 10 s left", handOverCall(t, s, e.refresher, env.ref))
		got := handOverRefreshes(env.provider, requests)
		if len(got) != 1 || got[0].Form.Get("refresh_token") != e.refresherTok.RefreshToken || got[0].Authorization != connBasic(env.provider) || got[0].AccessToken == "" {
			t.Fatalf("refresh requests: %d, want exactly one with the stored refresh token and the client's Basic header that handed out a token", len(got))
		}
		connEqual(t, "the token at the upstream", connAuths(env.up, "Authorization"), connBearer(got[0].AccessToken))
		connExpectFreshExpiry(t, s, e.refresher.id, env.slug)
	})

	t.Run("a connect started on the other image completes here; one whose window ran out is refused", func(t *testing.T) {
		env := e.e1
		requests, issuedBefore := len(env.provider.Requests()), len(env.provider.Issued())
		if loc := connCallback(t, e.pending.c, e.pendingBack); loc != "/connections?connected="+url.QueryEscape(env.slug) {
			t.Fatalf("the callback of the started connect: Location %q", loc)
		}
		issued := env.provider.Issued()
		if len(issued) != issuedBefore+1 {
			t.Fatalf("the provider issued %d token set(s) for the started connect, want 1", len(issued)-issuedBefore)
		}
		codes := env.provider.Requests()[requests:]
		if len(codes) != 1 || codes[0].GrantType != "authorization_code" || codes[0].Form.Get("code_verifier") == "" {
			t.Fatalf("token requests of the callback: %d, want one authorization_code exchange with the stored PKCE verifier", len(codes))
		}
		connNote(issued[len(issued)-1].AccessToken, issued[len(issued)-1].RefreshToken)
		env.up.Reset()
		connExpectOK(t, "a call after the completed connect", handOverCall(t, s, e.pending, env.ref))
		connEqual(t, "the token at the upstream", connAuths(env.up, "Authorization"), connBearer(issued[len(issued)-1].AccessToken))

		execSQL(t, s, "update connections.pending set expires_at = now() - interval '1 second' where user_id = $1::uuid and server = $2", e.late.id, env.slug)
		requests = len(env.provider.Requests())
		if loc := connCallback(t, e.late.c, e.lateBack); loc != "/connections?error=invalid_state" {
			t.Fatalf("the callback after the window: Location %q, want /connections?error=invalid_state", loc)
		}
		if n := len(env.provider.Requests()) - requests; n != 0 {
			t.Fatalf("the refused callback made %d token request(s)", n)
		}
		if got := connStatus(t, e.late.c, env.slug); got != "missing" {
			t.Fatalf("status after the refused callback %q, want missing", got)
		}
	})

	t.Run("no refresh token and no revoke URL at issue time: used until it runs out, then expired; disconnect revokes nothing", func(t *testing.T) {
		env := e.e2
		requests := len(env.provider.Requests())
		connExpireIn(t, s, e.noRefresh.id, env.slug, 30) // inside the 60 s window, nothing to refresh with
		env.up.Reset()
		connExpectOK(t, "a call with 30 s left and no refresh token", handOverCall(t, s, e.noRefresh, env.ref))
		connEqual(t, "the token at the upstream", connAuths(env.up, "Authorization"), connBearer(e.noRefreshAt.AccessToken))
		connExpireIn(t, s, e.noRefresh.id, env.slug, -1)
		env.up.Reset()
		connExpectText(t, "a call after the token ran out", handOverCall(t, s, e.noRefresh, env.ref), connTextExpired(s, env.slug))
		if n := len(env.up.Calls()); n != 0 {
			t.Fatalf("an expired token reached the upstream (%d call(s))", n)
		}
		if got := connStatus(t, e.noRefresh.c, env.slug); got != "expired" {
			t.Fatalf("status %q, want expired", got)
		}
		if n := len(env.provider.Requests()) - requests; n != 0 {
			t.Fatalf("%d token request(s) without a refresh token", n)
		}
		// The administrator's row has revoke_url NULL while the server has a revoke URL now.
		if r := env.admin.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d %s", r.Status, r.Body)
		}
		if n := len(env.provider.RevokeAttempts()); n != 0 {
			t.Fatalf("%d revoke attempt(s) for a token issued when there was no revoke URL", n)
		}
		if _, ok := connGetRow(t, s, e.adminID, env.slug); ok {
			t.Fatal("the row survived the disconnect")
		}
	})

	t.Run("no expiry stored (no expires_in, or one beyond 100 years): the row is read as it is, the token used, nothing refreshed", func(t *testing.T) {
		env := e.e3
		for _, c := range []struct {
			what   string
			p      connPerson
			issued fakes.IssuedTokens
		}{{"no expires_in", e.noExpiry, e.noExpiryAt}, {"expires_in beyond 100 years", e.farExpiry, e.farExpiryAt}} {
			connExpectSameRow(t, c.what, e.rows[c.p.id+"/"+env.slug], connRowOf(t, s, c.p.id, env.slug))
			if got := connStatus(t, c.p.c, env.slug); got != "ready" {
				t.Fatalf("%s: status %q, want ready", c.what, got)
			}
			requests := len(env.provider.Requests())
			env.up.Reset()
			connExpectOK(t, c.what+": a call", handOverCall(t, s, c.p, env.ref))
			connEqual(t, c.what+": the token at the upstream", connAuths(env.up, "Authorization"), connBearer(c.issued.AccessToken))
			if n := len(env.provider.Requests()) - requests; n != 0 {
				t.Fatalf("%s: %d token request(s)", c.what, n)
			}
			connExpectSameRow(t, c.what+" after the call", e.rows[c.p.id+"/"+env.slug], connRowOf(t, s, c.p.id, env.slug))
		}
	})
}

// handOverRotation is what a key rotation after the hand-over must keep usable: a person with a
// token, a shared_secret server whose upstream accepts only the stored secret, a pat_user server
// with the person's token, and an oauth_user server with the person's connection.
type handOverRotation struct {
	person          connPerson
	shared, pat     string
	sharedUp, patUp *fakes.Upstream
	secret, patTok  string
	oauth           *connEnv
	issued          fakes.IssuedTokens
}

func createHandOverRotation(t *testing.T, s *harness.Stack, admin *harness.Client) *handOverRotation {
	t.Helper()
	r := &handOverRotation{
		person: connNewPerson(t, s),
		shared: harness.Unique("hand-rot-shared"), pat: harness.Unique("hand-rot-pat"),
		secret: harness.Unique("hand-rot-key"), patTok: harness.Unique("hand-rot-pat-token"),
	}
	connNote(r.secret, r.patTok)
	r.sharedUp = fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo"), RequireHeader: [2]string{"Authorization", "Bearer " + r.secret}})
	if v := catalogRegister(t, admin, fakes.ServerInput(r.sharedUp, r.shared, "shared_secret", fakes.WithSecret(r.secret), fakes.WithTimeoutMs(5000))); v.ToolCount != 1 {
		t.Fatalf("the shared_secret server: %d tools (sync error %q)", v.ToolCount, v.syncError())
	}
	r.patUp = fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	catalogRegister(t, admin, fakes.ServerInput(r.patUp, r.pat, "pat_user", fakes.WithHeader("X-Token", "pat {secret}"), fakes.WithTimeoutMs(5000)))
	if msg := savePAT(t, admin, r.pat, harness.Unique("hand-rot-admin-pat")); msg != "" {
		t.Fatal(msg)
	}
	if res := catalogSyncOK(t, admin, r.pat); res.Added != 1 {
		t.Fatalf("sync of the pat_user server: %+v", res)
	}
	if msg := savePAT(t, r.person.c, r.pat, r.patTok); msg != "" {
		t.Fatal(msg)
	}
	r.oauth = handOverOAuthServer(t, s, admin, fakes.CodeGrant{})
	r.issued = connConnect(t, r.oauth, r.person.c)
	return r
}

// checkHandOverRotation rotates the key with the CLI of image (cli), which the stack is switched to
// first with SECRETS_KEY = a new key and SECRETS_KEY_OLD = oldKey (the key everything was sealed
// with), then starts image on the new key alone and uses every credential. The stack's own key is
// not changed: a later swap without overrides would start on oldKey again, so this comes last.
func checkHandOverRotation(t *testing.T, s *harness.Stack, r *handOverRotation, image, cli, oldKey string, kept *handOverKeptLogs) {
	t.Helper()
	next := newKey(t)
	secrets, conns := cliSecretCounts(t, s)
	if secrets < 3 || conns < 3 {
		t.Fatalf("%d server secrets and %d connections: the rotation has too little to re-seal", secrets, conns)
	}
	checkBefore, sharedBefore := keyCheckBytes(t, s), cliSealed(t, s, r.shared)
	rowBefore := connRowOf(t, s, r.person.id, r.oauth.slug)

	kept.keep(t, s)
	s.SwapImage(t, image, cli, map[string]string{"SECRETS_KEY": next, "SECRETS_KEY_OLD": oldKey})
	logs := s.Logs(t)
	recs, _ := logRecords(t, logs)
	if bad := recordsFrom(recs, "error"); len(bad) != 0 {
		t.Fatalf("records at error level or above in the start with the previous key configured: %v", bad)
	}
	assertNoKey(t, logs, oldKey, next)

	res := s.CLI(t, "", "rotate-secrets-key")
	if want := cliRotateStdout([2]int{secrets, 0}, [2]int{conns, 0}, "re-sealed"); res.Code != 0 || res.Stdout != want || res.Stderr != "" {
		t.Fatalf("rotate-secrets-key: %v, want exit 0 and stdout %q (every value sealed with the old key)", res, want)
	}
	for _, v := range []string{r.secret, r.patTok, r.issued.AccessToken, r.issued.RefreshToken} {
		if strings.Contains(res.Stdout+res.Stderr, v) {
			t.Fatalf("the rotation printed %s", harness.Redact(v))
		}
	}
	assertNoKey(t, res.Stdout+res.Stderr, oldKey, next)
	rowAfter := connRowOf(t, s, r.person.id, r.oauth.slug)
	if bytes.Equal(checkBefore, keyCheckBytes(t, s)) || bytes.Equal(sharedBefore, cliSealed(t, s, r.shared)) ||
		bytes.Equal(rowBefore.Access, rowAfter.Access) || bytes.Equal(rowBefore.Refresh, rowAfter.Refresh) {
		t.Fatal("the key check, the server secret or the connection's tokens were not re-sealed")
	}

	kept.keep(t, s)
	s.SwapImage(t, image, cli, map[string]string{"SECRETS_KEY": next})
	logs = s.Logs(t)
	recs, _ = logRecords(t, logs)
	if bad := recordsFrom(recs, "error"); len(bad) != 0 {
		t.Fatalf("records at error level or above in the start on the new key alone: %v", bad)
	}
	assertNoKey(t, logs, oldKey, next)

	r.sharedUp.Reset()
	r.patUp.Reset()
	r.oauth.up.Reset()
	for _, slug := range []string{r.shared, r.pat, r.oauth.slug} {
		connExpectOK(t, "a call on "+slug+" after the rotation", handOverCall(t, s, r.person, slug+".echo"))
	}
	connEqual(t, "the shared secret at its upstream", connAuths(r.sharedUp, "Authorization"), connBearer(r.secret))
	connEqual(t, "the personal token at its upstream", connAuths(r.patUp, "X-Token"), []string{"pat " + r.patTok})
	connEqual(t, "the OAuth access token at its upstream", connAuths(r.oauth.up, "Authorization"), connBearer(r.issued.AccessToken))

	connExpireIn(t, s, r.person.id, r.oauth.slug, 10)
	requests := len(r.oauth.provider.Requests())
	r.oauth.up.Reset()
	connExpectOK(t, "a call with 10 s left after the rotation", handOverCall(t, s, r.person, r.oauth.ref))
	got := handOverRefreshes(r.oauth.provider, requests)
	if len(got) != 1 || got[0].Form.Get("refresh_token") != r.issued.RefreshToken || got[0].AccessToken == "" {
		t.Fatalf("refresh requests after the rotation: %d, want exactly one presenting the re-sealed refresh token", len(got))
	}
	connEqual(t, "the refreshed token at the upstream", connAuths(r.oauth.up, "Authorization"), connBearer(got[0].AccessToken))
	if row := connRowOf(t, s, r.person.id, r.oauth.slug); row.Status != "ready" {
		t.Fatalf("the connection is %s after the refresh, want ready", row.Status)
	}
	cliExpectNoSecret(t, s, r.secret, res)
}
