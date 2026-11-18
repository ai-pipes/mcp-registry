//go:build compat

package compat_test

// Connections scenarios: a person's credentials for an upstream service (OAuth connect, personal
// access tokens, refresh, revocation, config-change safety), driven through REST, MCP (call_tool),
// SQL (time travel and "stored sealed" checks) and the fakes. Source: connections.int.test.ts.
//
// Credential resolution happens when a tool is called, so the refresh rules are driven by
// call_tool on an oauth_user server whose upstream is a fake: Upstream.Calls() shows exactly
// which token reached the upstream and OAuthProvider.Requests() exactly what the gateway sent to
// the provider. A token is made "about to expire" by moving expires_at of its row.
//
// Stack time: the file starts no stack of its own (harness.Shared). One test, "an unresponsive
// provider", waits for the gateway's 10 s token endpoint timeout (the only 10 s wait here); the
// rest of the file stays in seconds. Every rule has a positive and a negative control in the same
// test (the call that must work next to the one that must be refused).
//
// Source cases that are not ported (they need the app's internals, not its outside):
//   - "two processes, expired token: one refresh at the provider, both get the new token":
//     two replicas sharing one row lock. The image under test is one process; the single-flight
//     rule inside it (20 concurrent calls, one refresh) is ported as TestConnectionsSingleFlight.
//   - "a pool of 2 connections: 3 concurrent renewals for different people all finish": a
//     database pool of two connections is a container internal. Three people refreshing at once
//     all succeeding is ported (TestConnectionsSingleFlight, different people).
//   - "an unexpected failure (database down) -> error=internal": needs the store to fail inside
//     the process. Not reproducible from outside without breaking the shared database.
//   - the scheduler half of "catalog synchronisation with the admin's connection" (sync with
//     principal null, last successful admin demoted or disabled): the scheduler runs every
//     SYNC_INTERVAL_HOURS hours and has no trigger. The admin-button half is ported
//     (TestConnectionsAdminSync).
//   - the direct resolveCredential(...) results ({ok:false,error:'unavailable'}...): the same
//     rules are asserted through the text and outcome call_tool gives the agent.

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// ---- secrets that must never leak ----

var (
	connSecretsMu sync.Mutex
	connSecrets   []string
)

// connNote registers values that must appear in no audit record and no app log line.
func connNote(values ...string) {
	connSecretsMu.Lock()
	defer connSecretsMu.Unlock()
	connSecrets = append(connSecrets, values...)
}

// connLeakCheck makes the test end by scanning the app log and the whole audit trail for every
// token the test noted (issued, scripted, personal access tokens, the client secret).
func connLeakCheck(t *testing.T, s *harness.Stack, admin *harness.Client) {
	t.Helper()
	t.Cleanup(func() {
		connSecretsMu.Lock()
		secrets := slices.Clone(connSecrets)
		connSecrets = nil
		connSecretsMu.Unlock()
		if len(secrets) == 0 {
			t.Error("the leak check found no secret to look for: the test noted none")
			return
		}
		logs := s.Logs(t)
		audit := mustJSON(t, auditAll(t, admin))
		for _, sec := range secrets {
			if len(sec) < 6 {
				t.Errorf("a noted secret is too short to be meaningful: %q", sec)
				continue
			}
			for _, form := range []string{sec, url.QueryEscape(sec)} {
				if strings.Contains(logs, form) {
					t.Errorf("the app log contains the secret %s", harness.Redact(sec))
				}
				if strings.Contains(audit, form) {
					t.Errorf("an audit record contains the secret %s", harness.Redact(sec))
				}
			}
		}
	})
}

// ---- texts the agent gets ----

func connTextExpired(s *harness.Stack, slug string) string {
	return fmt.Sprintf("%s connection expired. Ask the user to reconnect at %s/connections.", slug, s.BaseURL)
}

func connTextNotConnected(s *harness.Stack, slug string) string {
	return fmt.Sprintf("%s is not connected. Ask the user to open %s/connections and connect %s.", slug, s.BaseURL, slug)
}

func connTextUnavailable(slug string) string { return slug + " is unavailable." }

// ---- environment ----

// connEnv is an oauth_user server with its own provider and upstream. The admin is connected, so
// the server's tools are synced.
type connEnv struct {
	s        *harness.Stack
	admin    *harness.Client
	slug     string
	ref      string
	up       *fakes.Upstream
	provider *fakes.OAuthProvider
}

type connOpts struct {
	upstream fakes.UpstreamOptions
	server   []fakes.ServerOpt
}

func connNewOAuth(t *testing.T, s *harness.Stack, admin *harness.Client, o connOpts) *connEnv {
	t.Helper()
	if o.upstream.Tools == nil {
		o.upstream.Tools = fakes.Tools("echo")
	}
	up := fakes.StartUpstream(t, o.upstream)
	provider := fakes.StartOAuthProvider(t)
	slug := harness.Unique("conn")
	opts := append([]fakes.ServerOpt{
		fakes.WithOAuth(provider, "repo"),
		fakes.WithExtraAuthorizeParams(map[string]string{"access_type": "offline"}),
		fakes.WithTimeoutMs(5000),
	}, o.server...)
	catalogRegister(t, admin, fakes.ServerInput(up, slug, "oauth_user", opts...))
	env := &connEnv{s: s, admin: admin, slug: slug, ref: slug + ".echo", up: up, provider: provider}
	connNote(provider.ClientSecret)
	connConnect(t, env, admin)
	if r := catalogSyncOK(t, admin, slug); r.Added != 1 {
		t.Fatalf("sync with the admin's token: %+v, want one tool added", r)
	}
	up.Reset()
	return env
}

// connPAT is a pat_user server (header X-Token, template "pat {secret}") whose admin has a token,
// so its tools are synced.
type connPAT struct {
	s     *harness.Stack
	admin *harness.Client
	slug  string
	ref   string
	up    *fakes.Upstream
}

func connNewPAT(t *testing.T, s *harness.Stack, admin *harness.Client, opts ...fakes.ServerOpt) *connPAT {
	t.Helper()
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	slug := harness.Unique("pat")
	opts = append([]fakes.ServerOpt{fakes.WithHeader("X-Token", "pat {secret}"), fakes.WithTimeoutMs(5000)}, opts...)
	catalogRegister(t, admin, fakes.ServerInput(up, slug, "pat_user", opts...))
	env := &connPAT{s: s, admin: admin, slug: slug, ref: slug + ".echo", up: up}
	connNote("admin-pat-value")
	if r := admin.Do(t, "PUT", "/api/connections/"+slug+"/pat", map[string]string{"token": "admin-pat-value"}); r.Status != 204 {
		t.Fatalf("PUT admin pat: %d %s", r.Status, r.Body)
	}
	if r := catalogSyncOK(t, admin, slug); r.Added != 1 {
		t.Fatalf("sync with the admin's pat: %+v", r)
	}
	up.Reset()
	return env
}

// connPerson is a member with a browser session and an MCP session.
type connPerson struct {
	c   *harness.Client
	id  string
	mcp *mcp.ClientSession
}

func connNewPerson(t *testing.T, s *harness.Stack) connPerson {
	t.Helper()
	c := s.Member(t, harness.Unique("conn-person"))
	return connPerson{c: c, id: whoami(t, c).UserID, mcp: s.MCP(t, c.Token(t, "conn"))}
}

type connResult struct {
	IsError bool
	Text    string
}

func (p connPerson) callRef(t *testing.T, ref string) connResult {
	t.Helper()
	res := catalogCallMCP(t, p.mcp, "call_tool", map[string]any{"tool": ref, "arguments": map[string]any{"text": "hi"}})
	return connResult{IsError: res.IsError, Text: catalogText(res)}
}

func (p connPerson) call(t *testing.T, env *connEnv) connResult {
	t.Helper()
	return p.callRef(t, env.ref)
}

// callMany sends n calls at once on one MCP session and returns the results.
func (p connPerson) callMany(t *testing.T, ref string, n int) []connResult {
	t.Helper()
	out := make([]connResult, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "call_tool", Arguments: map[string]any{"tool": ref, "arguments": map[string]any{"text": "hi"}}})
			if err != nil {
				out[i] = connResult{IsError: true, Text: "protocol error: " + err.Error()}
				return
			}
			out[i] = connResult{IsError: res.IsError, Text: catalogText(res)}
		}()
	}
	wg.Wait()
	return out
}

func connExpectOK(t *testing.T, what string, r connResult) {
	t.Helper()
	if r.IsError || r.Text != `{"text":"hi"}` {
		t.Fatalf("%s: isError %v, text %q, want the echo of the arguments", what, r.IsError, r.Text)
	}
}

func connExpectText(t *testing.T, what string, r connResult, want string) {
	t.Helper()
	if !r.IsError || r.Text != want {
		t.Fatalf("%s: isError %v, text %q, want an error with %q", what, r.IsError, r.Text, want)
	}
}

// ---- the connect round trip ----

func connStart(t *testing.T, c *harness.Client, slug string) *url.URL {
	t.Helper()
	r := c.Do(t, "POST", "/api/connections/"+slug+"/start", nil)
	if r.Status != 200 {
		t.Fatalf("POST /api/connections/%s/start: %d %s", slug, r.Status, r.Body)
	}
	var b struct {
		URL string `json:"url"`
	}
	r.JSON(t, "StartConnectResponse", &b)
	u, err := url.Parse(b.URL)
	if err != nil {
		t.Fatalf("start url %q: %v", b.URL, err)
	}
	return u
}

// connAuthorize is the person's browser at the provider; it returns the callback path and query.
func connAuthorize(t *testing.T, s *harness.Stack, authorize *url.URL) string {
	t.Helper()
	back := locationAt(t, authorize.String())
	rest, ok := strings.CutPrefix(back, s.BaseURL)
	if !ok {
		t.Fatalf("the provider redirected to %q, not back to %s", back, s.BaseURL)
	}
	return rest
}

// connCallback visits the callback and returns the Location of its 302.
func connCallback(t *testing.T, c *harness.Client, path string) string {
	t.Helper()
	r := c.Do(t, "GET", path, nil)
	if r.Status != 302 {
		t.Fatalf("GET %s: %d %.200s, want 302", path, r.Status, r.Body)
	}
	return r.Header.Get("Location")
}

// connConnect runs the whole connect for c and returns the tokens the provider issued.
func connConnect(t *testing.T, env *connEnv, c *harness.Client) fakes.IssuedTokens {
	t.Helper()
	before := len(env.provider.Issued())
	u := connStart(t, c, env.slug)
	loc := connCallback(t, c, connAuthorize(t, env.s, u))
	if want := "/connections?connected=" + url.QueryEscape(env.slug); loc != want {
		t.Fatalf("callback Location %q, want %q", loc, want)
	}
	issued := env.provider.Issued()
	if len(issued) != before+1 {
		t.Fatalf("the provider issued %d token sets, want %d", len(issued), before+1)
	}
	got := issued[len(issued)-1]
	connNote(got.AccessToken, got.RefreshToken)
	return got
}

func connStatus(t *testing.T, c *harness.Client, slug string) string {
	t.Helper()
	r := c.Do(t, "GET", "/api/connections", nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/connections: %d %s", r.Status, r.Body)
	}
	var list []struct {
		Server   string `json:"server"`
		Name     string `json:"name"`
		AuthKind string `json:"authKind"`
		Status   string `json:"status"`
	}
	r.JSON(t, "ConnectionSummary[]", &list)
	for _, e := range list {
		if e.Server == slug {
			return e.Status
		}
	}
	return "<absent>"
}

func connBasic(p *fakes.OAuthProvider) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(p.ClientID)+":"+url.QueryEscape(p.ClientSecret)))
}

// ---- the database ----

type connRow struct {
	Kind, Status, IssuedFor string
	Access, Refresh         []byte
	ExpiresAt               *time.Time
	UpdatedAt               time.Time
	RevokeURL               *string
}

func connGetRow(t *testing.T, s *harness.Stack, uid, slug string) (connRow, bool) {
	t.Helper()
	var r connRow
	rows, err := s.DB.Query(t.Context(), `select kind, status, issued_for, access, refresh, expires_at, updated_at, revoke_url
		from connections.connections where user_id = $1::uuid and server = $2`, uid, slug)
	if err != nil {
		t.Fatalf("select connection: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return r, false
	}
	if err := rows.Scan(&r.Kind, &r.Status, &r.IssuedFor, &r.Access, &r.Refresh, &r.ExpiresAt, &r.UpdatedAt, &r.RevokeURL); err != nil {
		t.Fatalf("scan connection: %v", err)
	}
	return r, true
}

func connRowOf(t *testing.T, s *harness.Stack, uid, slug string) connRow {
	t.Helper()
	r, ok := connGetRow(t, s, uid, slug)
	if !ok {
		t.Fatalf("no connection row for %s / %s", uid, slug)
	}
	return r
}

// connExpireIn moves expires_at to now + secs seconds (negative: in the past).
func connExpireIn(t *testing.T, s *harness.Stack, uid, slug string, secs float64) {
	t.Helper()
	execSQL(t, s, "update connections.connections set expires_at = now() + make_interval(secs => $3) where user_id = $1::uuid and server = $2", uid, slug, secs)
}

func connQuota(t *testing.T, s *harness.Stack, uid, slug string) int {
	t.Helper()
	return countSQL(t, s, "select coalesce(sum(count), 0)::int from gateway.quota where user_id = $1::uuid and server = $2", uid, slug)
}

// connAudit returns the audit entries of the action about this person and server.
func connAudit(t *testing.T, admin *harness.Client, action, uid, slug string) []auditEntry {
	t.Helper()
	return auditWhere(auditAll(t, admin), action, func(e auditEntry) bool { return ptrIs(e.ActorID, uid) && ptrIs(e.Target, slug) })
}

func connExpectExpiredAudit(t *testing.T, admin *harness.Client, uid, slug, reason string, n int) {
	t.Helper()
	got := connAudit(t, admin, "connection.expired", uid, slug)
	if len(got) != n {
		t.Fatalf("connection.expired audit records for %s: %d, want %d (%+v)", slug, len(got), n, got)
	}
	for _, e := range got {
		if len(e.Meta) != 1 || e.Meta["reason"] != reason {
			t.Fatalf("connection.expired meta %v, want {reason: %q}", e.Meta, reason)
		}
	}
}

// connOutcomes returns the call log outcomes of the person on the server, oldest first.
func connOutcomes(t *testing.T, admin *harness.Client, uid, slug string) []string {
	t.Helper()
	r := admin.Do(t, "GET", "/api/calls?server="+slug+"&user="+uid, nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/calls: %d %s", r.Status, r.Body)
	}
	var list []struct {
		Outcome string `json:"outcome"`
	}
	r.JSON(t, "CallList", &list)
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.Outcome)
	}
	slices.Reverse(out)
	return out
}

// connAuths returns the header of every tools/call the upstream received, oldest first.
func connAuths(up *fakes.Upstream, header string) []string {
	var out []string
	for _, c := range up.Calls() {
		out = append(out, c.Header.Get(header))
	}
	return out
}

// connAttempts returns the header of every HTTP request the upstream received (refused ones
// included, as a refusal happens at the session handshake), consecutive duplicates folded: one
// entry per attempt with a distinct credential.
func connAttempts(up *fakes.Upstream, header string) []string {
	var out []string
	for _, r := range up.Requests() {
		if v := r.Header.Get(header); len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

func connBearer(tokens ...string) []string {
	out := make([]string, 0, len(tokens))
	for _, tk := range tokens {
		out = append(out, "Bearer "+tk)
	}
	return out
}

// connGrants returns the grant types of the provider's token requests, oldest first.
func connGrants(p *fakes.OAuthProvider) []string {
	var out []string
	for _, r := range p.Requests() {
		out = append(out, r.GrantType)
	}
	return out
}

func connRefreshTokens(p *fakes.OAuthProvider) []string {
	var out []string
	for _, r := range p.Requests() {
		if r.GrantType == "refresh_token" {
			out = append(out, r.Form.Get("refresh_token"))
		}
	}
	return out
}

func connEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s: %v, want %v", what, redactAll(got), redactAll(want))
	}
}

func redactAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, harness.Redact(v))
	}
	return out
}

// connChangeServer applies mut to the body of PUT /api/servers/:slug (the server as it is) and
// expects 200.
func connChangeServer(t *testing.T, admin *harness.Client, slug string, mut func(body map[string]any)) {
	t.Helper()
	v := catalogGet(t, admin, slug)
	r := admin.Do(t, "PUT", "/api/servers/"+slug, catalogPutBody(v, mut))
	if r.Status != 200 {
		t.Fatalf("PUT /api/servers/%s: %d %.300s", slug, r.Status, r.Body)
	}
}

func connOAuthOf(body map[string]any) map[string]any {
	a, _ := body["auth"].(map[string]any)
	o, _ := a["oauth"].(map[string]any)
	return o
}

func connSetStatus(t *testing.T, admin *harness.Client, slug, status string) {
	t.Helper()
	if r := admin.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]string{"status": status}); r.Status != 204 {
		t.Fatalf("POST status %s: %d %s", status, r.Status, r.Body)
	}
}

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
		if left := time.Until(*row.ExpiresAt); left < 3500*time.Second || left > 3601*time.Second {
			t.Errorf("expires_at is %s from now, want about 1 h (expires_in 3600)", left)
		}
		// Sealed: neither the tokens nor their markers are readable in any column.
		for what, raw := range map[string][]byte{"access": row.Access, "refresh": row.Refresh} {
			text := string(raw)
			for _, plain := range []string{mine.AccessToken, mine.RefreshToken, "access-", "refresh-"} {
				if strings.Contains(text, plain) {
					t.Errorf("column %s holds %q in plain text", what, harness.Redact(plain))
				}
			}
		}
		if strings.Contains(row.IssuedFor, mine.AccessToken) || strings.Contains(row.IssuedFor, mine.RefreshToken) {
			t.Error("issued_for holds a token")
		}
	})

	t.Run("the audit trail records the connect, with no token", func(t *testing.T) {
		got := connAudit(t, admin, "connection.connect", p.id, env.slug)
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
		if got := connAudit(t, admin, "connection.disconnect", p.id, env.slug); len(got) != 1 || len(got[0].Meta) != 0 {
			t.Fatalf("connection.disconnect records: %+v", got)
		}
		// Nothing left to disconnect: no second revoke, no second audit record.
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("second DELETE: %d", r.Status)
		}
		if n := len(env.provider.RevokeAttempts()); n != 1 {
			t.Fatalf("%d revoke attempts after a second disconnect, want 1", n)
		}
		if got := connAudit(t, admin, "connection.disconnect", p.id, env.slug); len(got) != 1 {
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
		// Pinned: the refused attempt consumed the state (one-time), so the starter cannot finish it.
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
			if r := admin.Do(t, "POST", "/api/servers", body); r.Status != 422 {
				t.Errorf("extraAuthorizeParams %q accepted: %d %.200s", key, r.Status, r.Body)
			}
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
		if strings.Contains(string(row.Access), "perm:abc") {
			t.Fatal("the pat is stored in plain text")
		}
		got := connAudit(t, admin, "connection.pat_saved", p.id, env.slug)
		if len(got) != 1 || len(got[0].Meta) != 0 {
			t.Fatalf("connection.pat_saved records: %+v", got)
		}
		res := p.callRef(t, env.ref)
		connExpectOK(t, "call with the pat", res)
		calls := env.up.Calls()
		if len(calls) != 1 || calls[0].Header.Get("X-Token") != "pat perm:abc.def-123" {
			t.Fatalf("upstream calls: %d, X-Token %q, want one with %q", len(calls), func() string {
				if len(calls) == 0 {
					return ""
				}
				return calls[0].Header.Get("X-Token")
			}(), "pat perm:abc.def-123")
		}
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

	t.Run("refused tokens change nothing", func(t *testing.T) {
		before := connRowOf(t, s, p.id, env.slug)
		long := strings.Repeat("a", 4097)
		cases := []struct {
			name   string
			body   any
			status int
			code   string
		}{
			{"blank", map[string]string{"token": "   "}, 422, "invalid_pat|the token must be a non-empty single line"},
			{"empty", map[string]string{"token": ""}, 422, "invalid_request|token: Too small: expected string to have >=1 characters"},
			{"CR LF inside", map[string]string{"token": "abc\r\nX-Evil: 1"}, 422, //nolint:gosec // G101: a made-up token of a test
				"invalid_pat|the token must be a non-empty single line"},
			{"LF inside", map[string]string{"token": "abc\ndef"}, 422, "invalid_pat|the token must be a non-empty single line"},
			{"NUL inside", map[string]string{"token": "abc\x00def"}, 422, "invalid_pat|the token must be a non-empty single line"},
			{"4097 characters", map[string]string{"token": long}, 422, "invalid_request|token: Too big: expected string to have <=4096 characters"},
			{"no token field", map[string]string{}, 422, "invalid_request|token: Invalid input: expected string, received undefined"},
		}
		for _, c := range cases {
			catalogExpectError(t, c.name, put(p.c, env.slug, c.body), c.status, c.code)
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
		connExpectExpiredAudit(t, admin, q2.id, env.slug, "rejected_upstream", 1)
		if outcomes := connOutcomes(t, admin, q2.id, env.slug); !slices.Equal(outcomes, []string{"denied:expired"}) {
			t.Fatalf("call log: %v", outcomes)
		}

		// An expired connection is not tried again: the upstream sees nothing, even though it works now.
		env.up.Reset()
		connExpectText(t, "call after expiry", q2.callRef(t, env.ref), connTextExpired(s, env.slug))
		if n := len(env.up.Calls()); n != 0 {
			t.Fatalf("an expired connection reached the upstream %d time(s)", n)
		}
		connExpectExpiredAudit(t, admin, q2.id, env.slug, "rejected_upstream", 1) // not recorded twice

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
		if n := len(limited.up.Calls()); n != 0 {
			t.Fatalf("the upstream saw %d call(s) from an unconnected person", n)
		}
		if n := connQuota(t, s, q.id, limited.slug); n != 0 {
			t.Fatalf("quota spent: %d (an unconnected service spends none)", n)
		}
		if outcomes := connOutcomes(t, admin, q.id, limited.slug); !slices.Equal(outcomes, []string{"denied:needs_connection"}) {
			t.Fatalf("call log: %v", outcomes)
		}
		// Control: once connected the same call goes through and spends one.
		connNote("limited-pat-1")
		put(q.c, limited.slug, map[string]string{"token": "limited-pat-1"})
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
		if got := connAudit(t, admin, "connection.disconnect", p.id, env.slug); len(got) != 1 {
			t.Fatalf("connection.disconnect records: %d", len(got))
		}
	})
}

// ---- refresh ----

// connConnected returns a new person connected to env and the tokens issued to them.
func connConnected(t *testing.T, env *connEnv) (connPerson, fakes.IssuedTokens) {
	t.Helper()
	p := connNewPerson(t, env.s)
	issued := connConnect(t, env, p.c)
	env.up.Reset()
	return p, issued
}

func TestConnectionsRefresh(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	env := connNewOAuth(t, s, admin, connOpts{})

	t.Run("a token is refreshed when fewer than 60 s remain, not before", func(t *testing.T) {
		p, issued := connConnected(t, env)
		requests := len(env.provider.Requests())

		connExpireIn(t, s, p.id, env.slug, 61)
		connExpectOK(t, "call with 61 s left", p.call(t, env))
		if n := len(env.provider.Requests()); n != requests {
			t.Fatalf("a refresh at 61 s left: %d new token request(s)", n-requests)
		}
		connEqual(t, "token with 61 s left", connAuths(env.up, "Authorization"), connBearer(issued.AccessToken))

		env.up.Reset()
		connExpireIn(t, s, p.id, env.slug, 59)
		connExpectOK(t, "call with 59 s left", p.call(t, env))
		reqs := env.provider.Requests()[requests:]
		if len(reqs) != 1 || reqs[0].GrantType != "refresh_token" {
			t.Fatalf("token requests at 59 s left: %+v, want exactly one refresh_token grant", reqs)
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
		if row := connRowOf(t, s, p.id, env.slug); row.Status != "ready" || row.ExpiresAt == nil || time.Until(*row.ExpiresAt) < 3500*time.Second {
			t.Fatalf("row after the refresh: %+v", row)
		}

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
		step fakes.Error
	}{
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
			rowAfter := connRowOf(t, s, p.id, env.slug)
			if rowAfter.Status != "ready" || string(rowAfter.Access) != string(rowBefore.Access) || string(rowAfter.Refresh) != string(rowBefore.Refresh) ||
				!rowAfter.UpdatedAt.Equal(rowBefore.UpdatedAt) || !rowAfter.ExpiresAt.Equal(*rowBefore.ExpiresAt) {
				t.Fatalf("a failed refresh changed the stored connection: before %+v after %+v", rowBefore, rowAfter)
			}
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
		started := time.Now()
		connExpectOK(t, "call with 59 s left and a hanging provider", p.call(t, env))
		took := time.Since(started)
		if took < 8*time.Second || took > 25*time.Second {
			t.Fatalf("the call took %s: the gateway should give up on the token endpoint after about 10 s", took)
		}
		connEqual(t, "token", connAuths(env.up, "Authorization"), connBearer(issued.AccessToken))
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
			connNote(strings.TrimPrefix(h, "Bearer "))
		}
		if len(counts) != 3 {
			t.Fatalf("%d distinct tokens at the upstream, want 3", len(counts))
		}
		for h, n := range counts {
			if n != 7 {
				t.Fatalf("token %s used %d times, want 7", harness.Redact(h), n)
			}
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
		connEqual(t, "credentials the upstream was offered", connAttempts(env.up, "Authorization"), connBearer(issued.AccessToken, "acc-fresh-1"))
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
		connEqual(t, "second call credential", connAttempts(env.up, "Authorization"), connBearer("acc-fresh-1"))
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
		connEqual(t, "credentials the upstream was offered", connAttempts(env.up, "Authorization"), connBearer(issued.AccessToken, "acc-refused-1"))
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

	t.Run("the daily limit still counts per person: the quota of the retry's call is one", func(t *testing.T) {
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

// ---- the server config changes after connecting ----

func TestConnectionsConfigChange(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)

	// Where a token must never be sent after the change.
	other := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	otherProvider := fakes.StartOAuthProvider(t)

	expiring := []struct {
		name   string
		change func(b map[string]any)
	}{
		{"the upstream URL", func(b map[string]any) { b["url"] = other.URL }},
		{"the authorize URL", func(b map[string]any) { connOAuthOf(b)["authorizeUrl"] = otherProvider.AuthorizeURL }},
		{"the token URL", func(b map[string]any) { connOAuthOf(b)["tokenUrl"] = otherProvider.TokenURL }},
		{"the client id", func(b map[string]any) { connOAuthOf(b)["clientId"] = "another-client" }},
	}
	for _, c := range expiring {
		t.Run("a changed "+c.name+" expires the connection and the token never goes to the new place", func(t *testing.T) {
			env := connNewOAuth(t, s, admin, connOpts{})
			p, issued := connConnected(t, env)
			other.Reset()
			// Control: before the change the call works.
			connExpectOK(t, "call before the change", p.call(t, env))
			connEqual(t, "token before the change", connAuths(env.up, "Authorization"), connBearer(issued.AccessToken))
			env.up.Reset()

			// An expiring token: were the fingerprint ignored, a refresh would go to the (new) token endpoint.
			connExpireIn(t, s, p.id, env.slug, 59)
			provRequests, otherRequests := len(env.provider.Requests()), len(otherProvider.Requests())
			connChangeServer(t, admin, env.slug, c.change)
			if got := connStatus(t, p.c, env.slug); got != "expired" {
				t.Fatalf("status right after the change: %q, want expired", got)
			}
			connExpectText(t, "call after the change", p.call(t, env), connTextExpired(s, env.slug))
			if got := connStatus(t, p.c, env.slug); got != "expired" {
				t.Fatalf("status after the call: %q", got)
			}
			if row := connRowOf(t, s, p.id, env.slug); row.Status != "expired" {
				t.Fatalf("row status %q", row.Status)
			}
			connExpectExpiredAudit(t, admin, p.id, env.slug, "config_changed", 1)
			if outcomes := connOutcomes(t, admin, p.id, env.slug); !slices.Equal(outcomes, []string{"ok", "denied:expired"}) {
				t.Fatalf("call log: %v", outcomes)
			}
			// No token request anywhere, no call at either upstream.
			if n := len(env.provider.Requests()) - provRequests; n != 0 {
				t.Fatalf("the old provider got %d token request(s) after the change", n)
			}
			if n := len(otherProvider.Requests()) - otherRequests; n != 0 {
				t.Fatalf("the new provider got %d token request(s) after the change", n)
			}
			if n := len(env.up.Requests()); n != 0 {
				t.Fatalf("the old upstream got %d request(s)", n)
			}
			for _, r := range other.Requests() {
				if r.Header.Get("Authorization") != "" {
					t.Fatalf("the new upstream received a credential: %q", harness.Redact(r.Header.Get("Authorization")))
				}
			}
			// Disconnect deletes without revoking, anywhere.
			if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
				t.Fatalf("DELETE: %d", r.Status)
			}
			if n := len(env.provider.RevokeAttempts()) + len(otherProvider.RevokeAttempts()); n != 0 {
				t.Fatalf("%d revoke attempt(s) after the config changed", n)
			}
			if _, ok := connGetRow(t, s, p.id, env.slug); ok {
				t.Fatal("the row survived the disconnect")
			}
		})
	}

	t.Run("after a changed upstream URL a reconnect works and the new token goes to the new place", func(t *testing.T) {
		env := connNewOAuth(t, s, admin, connOpts{})
		p, issued := connConnected(t, env)
		other.Reset()
		connChangeServer(t, admin, env.slug, func(b map[string]any) { b["url"] = other.URL })
		connExpectText(t, "call after the change", p.call(t, env), connTextExpired(s, env.slug))
		fresh := connConnect(t, env, p.c)
		connExpectOK(t, "call after reconnecting", p.call(t, env))
		connEqual(t, "credentials at the new upstream", connAuths(other, "Authorization"), connBearer(fresh.AccessToken))
		if n := len(env.up.Requests()); n != 0 {
			t.Fatalf("the old upstream got %d request(s)", n)
		}
		if fresh.AccessToken == issued.AccessToken {
			t.Fatal("reconnecting did not issue a new token")
		}
	})

	t.Run("other scopes or a changed or removed revoke URL expire nobody; a changed revoke URL is never used", func(t *testing.T) {
		changes := []struct {
			name         string
			revokeAtOnce bool // the revoke URL is unchanged, so disconnect still revokes at it
			change       func(env *connEnv, b map[string]any)
		}{
			{"scopes", true, func(_ *connEnv, b map[string]any) { connOAuthOf(b)["scopes"] = []string{"repo", "user"} }},
			{"revoke URL to another provider", false, func(_ *connEnv, b map[string]any) { connOAuthOf(b)["revokeUrl"] = otherProvider.RevokeURL }},
			{"revoke URL to the same endpoint with a query", false, func(env *connEnv, b map[string]any) { connOAuthOf(b)["revokeUrl"] = env.provider.RevokeURL + "?v=2" }},
			{"revoke URL removed", false, func(_ *connEnv, b map[string]any) { delete(connOAuthOf(b), "revokeUrl") }},
		}
		for _, c := range changes {
			env := connNewOAuth(t, s, admin, connOpts{})
			p, issued := connConnected(t, env)
			connExpireIn(t, s, p.id, env.slug, 59)
			connChangeServer(t, admin, env.slug, func(b map[string]any) { c.change(env, b) })
			if got := connStatus(t, p.c, env.slug); got != "ready" {
				t.Fatalf("%s: status %q, want ready", c.name, got)
			}
			// The expiring token refreshes at the (unchanged) token endpoint: the connection is alive.
			connExpectOK(t, c.name+": call", p.call(t, env))
			if got := connRefreshTokens(env.provider); len(got) == 0 || got[len(got)-1] != issued.RefreshToken {
				t.Fatalf("%s: the refresh did not happen at the token endpoint", c.name)
			}
			if n := len(connAudit(t, admin, "connection.expired", p.id, env.slug)); n != 0 {
				t.Fatalf("%s: connection.expired recorded", c.name)
			}
			otherBefore := len(otherProvider.RevokeAttempts())
			if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
				t.Fatalf("%s: DELETE %d", c.name, r.Status)
			}
			if c.revokeAtOnce {
				// Control for the cases below: an unchanged revoke URL is used.
				connEqual(t, c.name+": revoked", env.provider.Revoked(), []string{issued.RefreshToken})
			} else if n := len(env.provider.RevokeAttempts()); n != 0 {
				t.Fatalf("%s: %d revoke attempt(s) at the old revoke URL, want none (the URL changed)", c.name, n)
			}
			if n := len(otherProvider.RevokeAttempts()) - otherBefore; n != 0 {
				t.Fatalf("%s: %d revoke attempt(s) at the new revoke URL, a token goes only where it was issued", c.name, n)
			}
			if _, ok := connGetRow(t, s, p.id, env.slug); ok {
				t.Fatalf("%s: the row survived", c.name)
			}
		}
	})

	t.Run("a revoke URL added after the token was issued is not used either", func(t *testing.T) {
		env := connNewOAuth(t, s, admin, connOpts{})
		// A server whose OAuth settings had no revoke URL when the person connected.
		noRevoke := connNewOAuth(t, s, admin, connOpts{server: []fakes.ServerOpt{fakes.WithoutRevokeURL()}})
		p, _ := connConnected(t, noRevoke)
		if row := connRowOf(t, s, p.id, noRevoke.slug); row.RevokeURL != nil {
			t.Fatalf("revoke_url %v, want null", row.RevokeURL)
		}
		connChangeServer(t, admin, noRevoke.slug, func(b map[string]any) { connOAuthOf(b)["revokeUrl"] = noRevoke.provider.RevokeURL })
		if got := connStatus(t, p.c, noRevoke.slug); got != "ready" {
			t.Fatalf("status: %q", got)
		}
		if r := p.c.Do(t, "DELETE", "/api/connections/"+noRevoke.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d", r.Status)
		}
		if n := len(noRevoke.provider.RevokeAttempts()); n != 0 {
			t.Fatalf("%d revoke attempt(s) at a revoke URL that did not exist at issue time", n)
		}
		// Control: a revoke URL that never changed is used (the connect flow test pins the token).
		q, issued := connConnected(t, env)
		if r := q.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d", r.Status)
		}
		connEqual(t, "revoked", env.provider.Revoked(), []string{issued.RefreshToken})
	})
}

// ---- revoke failures ----

func TestConnectionsDisconnectIgnoresRevokeFailures(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	withRevokeURL := func(u string) fakes.ServerOpt {
		return func(b map[string]any) { connOAuthOf(b)["revokeUrl"] = u }
	}

	t.Run("a revoke endpoint answering 500", func(t *testing.T) {
		failing := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		failing.FailWith(500)
		env := connNewOAuth(t, s, admin, connOpts{server: []fakes.ServerOpt{withRevokeURL(failing.URL)}})
		p, _ := connConnected(t, env)
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d %s", r.Status, r.Body)
		}
		if n := len(failing.Requests()); n != 1 {
			t.Fatalf("the revoke endpoint got %d request(s), want the one attempt", n)
		}
		if _, ok := connGetRow(t, s, p.id, env.slug); ok {
			t.Fatal("the row survived a failed revoke")
		}
		if got := connStatus(t, p.c, env.slug); got != "missing" {
			t.Fatalf("status: %q", got)
		}
	})

	t.Run("a revoke endpoint nobody listens on", func(t *testing.T) {
		env := connNewOAuth(t, s, admin, connOpts{server: []fakes.ServerOpt{withRevokeURL("http://host.docker.internal:1/revoke")}})
		p, _ := connConnected(t, env)
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d %s", r.Status, r.Body)
		}
		if _, ok := connGetRow(t, s, p.id, env.slug); ok {
			t.Fatal("the row survived")
		}
	})

	t.Run("a revoke endpoint refusing the client's credentials", func(t *testing.T) {
		env := connNewOAuth(t, s, admin, connOpts{})
		p, issued := connConnected(t, env)
		if r := admin.Do(t, "PUT", "/api/servers/"+env.slug+"/secret", map[string]string{"secret": "not-the-secret"}); r.Status != 204 {
			t.Fatalf("PUT secret: %d", r.Status)
		}
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d", r.Status)
		}
		attempts := env.provider.RevokeAttempts()
		if len(attempts) != 1 || attempts[0].Accepted || attempts[0].Token != issued.RefreshToken {
			t.Fatalf("revoke attempts: %+v, want one refused attempt for the refresh token", attempts)
		}
		if len(env.provider.Revoked()) != 0 {
			t.Fatal("a refused revoke is recorded as accepted")
		}
		if _, ok := connGetRow(t, s, p.id, env.slug); ok {
			t.Fatal("the row survived")
		}
	})

	t.Run("one's own credentials can be removed after the server is disabled", func(t *testing.T) {
		env := connNewOAuth(t, s, admin, connOpts{})
		p, _ := connConnected(t, env)
		connSetStatus(t, admin, env.slug, "disabled")
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d", r.Status)
		}
		if _, ok := connGetRow(t, s, p.id, env.slug); ok {
			t.Fatal("the row survived")
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
			env.slug: {env.slug, "oauth_user", "missing"},
			pat.slug: {pat.slug, "pat_user", "missing"},
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

// ---- the admin's own connection syncs the catalog ----

func TestConnectionsAdminSync(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo", "ping")})
	provider := fakes.StartOAuthProvider(t)
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
	if got := connAttempts(up, "Authorization"); !slices.Equal(got, connBearer(issued.AccessToken)) {
		t.Fatalf("the sync presented %v, want the admin's own access token on every request", redactAll(got))
	}

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
	if got := connAttempts(up, "Authorization"); !slices.Equal(got, connBearer("acc-sync-1")) {
		t.Fatalf("the sync after a refresh presented %v", redactAll(got))
	}

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
