//go:build compat

package compat_test

// Connections scenarios: a person's credentials for an upstream service (OAuth connect, personal
// access tokens, refresh, revocation, config-change safety), driven through REST, MCP (call_tool),
// SQL (time travel and "stored sealed" checks) and the fakes. Source: connections.int.test.ts.
//
// Credential resolution happens when a tool is called, so the refresh rules are driven by
// call_tool on an oauth_user server whose upstream is a fake: Upstream.Requests() and Calls() show exactly
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
	"context"
	"encoding/base64"
	"encoding/hex"
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
	connProviders []*fakes.OAuthProvider
)

// connWatch registers a provider whose every handed-out token the leak check will look for.
func connWatch(p *fakes.OAuthProvider) *fakes.OAuthProvider {
	connSecretsMu.Lock()
	defer connSecretsMu.Unlock()
	connProviders = append(connProviders, p)
	return p
}

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
		for _, p := range connProviders {
			// Every token the fake ever put into a response, refreshes included, and the client
			// credentials in every form they can travel in.
			secrets = append(secrets, p.HandedOut()...)
			secrets = append(secrets, p.ClientSecret,
				base64.StdEncoding.EncodeToString([]byte(p.ClientID+":"+p.ClientSecret)),
				strings.TrimPrefix(connBasic(p), "Basic "))
		}
		connSecrets, connProviders = nil, nil
		connSecretsMu.Unlock()
		if len(secrets) == 0 {
			t.Error("the leak check found no secret to look for: the test noted none")
			return
		}
		logs := s.Logs(t)
		// The whole table, read from the database (see connAudit), plus what the API shows.
		var table string
		if err := s.DB.QueryRow(context.Background(), "select coalesce(string_agg(action || ' ' || coalesce(target, '') || ' ' || coalesce(meta::text, ''), E'\\n'), '') from platform.audit_log").Scan(&table); err != nil {
			t.Errorf("read audit_log: %v", err)
		}
		var page []auditEntry
		if r := admin.Do(t, "GET", "/api/audit?limit=500", nil); r.Status == 200 {
			r.JSON(t, "AuditList", &page)
		} else {
			t.Errorf("GET /api/audit: %d", r.Status)
		}
		audit := table + mustJSON(t, page)
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

// The texts name the server by its display name (fakes.DisplayName), never by its slug.
func connTextExpired(s *harness.Stack, slug string) string {
	return fmt.Sprintf("%s connection expired. Ask the user to reconnect at %s/connections.", fakes.DisplayName(slug), s.BaseURL)
}

func connTextNotConnected(s *harness.Stack, slug string) string {
	name := fakes.DisplayName(slug)
	return fmt.Sprintf("%s is not connected. Ask the user to open %s/connections and connect %s.", name, s.BaseURL, name)
}

func connTextUnavailable(slug string) string { return fakes.DisplayName(slug) + " is unavailable." }

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
	provider := connWatch(fakes.StartOAuthProvider(t))
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

// connExpiresInSecs is the time left until expires_at, computed by the database against its own
// clock (the host clock may differ from the Docker VM's).
func connExpiresInSecs(t *testing.T, s *harness.Stack, uid, slug string) float64 {
	t.Helper()
	var left float64
	if err := s.DB.QueryRow(t.Context(), "select extract(epoch from expires_at - now())::float8 from connections.connections where user_id = $1::uuid and server = $2", uid, slug).Scan(&left); err != nil {
		t.Fatalf("expires_at of %s: %v", slug, err)
	}
	return left
}

// connExpectFreshExpiry checks a new token's expiry against the documented expires_in of 3600 s,
// allowing only for the time elapsed inside the test.
func connExpectFreshExpiry(t *testing.T, s *harness.Stack, uid, slug string) {
	t.Helper()
	if left := connExpiresInSecs(t, s, uid, slug); left < 3590 || left > 3601 {
		t.Fatalf("expires_at is %.1f s ahead of the database clock, want about 3600 (expires_in)", left)
	}
}

// connExpectSealed asserts that stored bytes do not reveal the token: not as text, not as base64
// (standard and URL, padded and not), not as hex.
func connExpectSealed(t *testing.T, what string, stored []byte, token string) {
	t.Helper()
	forms := map[string]string{
		"plain":         token,
		"base64":        base64.StdEncoding.EncodeToString([]byte(token)),
		"base64 raw":    base64.RawStdEncoding.EncodeToString([]byte(token)),
		"base64url":     base64.URLEncoding.EncodeToString([]byte(token)),
		"base64url raw": base64.RawURLEncoding.EncodeToString([]byte(token)),
		"hex":           hex.EncodeToString([]byte(token)),
	}
	for name, f := range forms {
		if strings.Contains(string(stored), f) {
			t.Errorf("%s holds the token as %s", what, name)
		}
	}
}

// connExpectSameRow asserts that nothing of the stored connection changed.
func connExpectSameRow(t *testing.T, what string, before, after connRow) {
	t.Helper()
	same := before.Kind == after.Kind && before.Status == after.Status && before.IssuedFor == after.IssuedFor &&
		string(before.Access) == string(after.Access) && string(before.Refresh) == string(after.Refresh) &&
		before.UpdatedAt.Equal(after.UpdatedAt) && ptrEq(before.RevokeURL, after.RevokeURL) &&
		(before.ExpiresAt == nil) == (after.ExpiresAt == nil) && (before.ExpiresAt == nil || before.ExpiresAt.Equal(*after.ExpiresAt))
	if !same {
		t.Fatalf("%s: the stored connection changed: before %+v after %+v", what, redactRow(before), redactRow(after))
	}
}

func ptrEq(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func redactRow(r connRow) connRow {
	r.Access, r.Refresh = nil, nil
	return r
}

func connQuota(t *testing.T, s *harness.Stack, uid, slug string) int {
	t.Helper()
	return countSQL(t, s, "select coalesce(sum(count), 0)::int from gateway.quota where user_id = $1::uuid and server = $2", uid, slug)
}

// connAudit returns the audit entries of the action about this person and server, oldest first,
// read through the shared auditRows (SQL; see auditAll for why not the API).
func connAudit(t *testing.T, s *harness.Stack, action, uid, slug string) []auditEntry {
	t.Helper()
	return auditRows(t, s, "where action = $1 and actor_id = $2::uuid and target = $3 order by id", action, uid, slug)
}

func connExpectExpiredAudit(t *testing.T, s *harness.Stack, uid, slug, reason string, n int) {
	t.Helper()
	got := connAudit(t, s, "connection.expired", uid, slug)
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

// connRequestHeaders returns the header of every HTTP request the upstream received, refused ones
// included, oldest first and not folded: counts matter.
func connRequestHeaders(up *fakes.Upstream, header string) []string {
	var out []string
	for _, r := range up.Requests() {
		out = append(out, r.Header.Get(header))
	}
	return out
}

// connExpectOneCallWith asserts what any correct exchange with the upstream looks like, whatever
// MCP client library makes it: at least one request, every request carries the credential, and
// exactly one tools/call was made (a retry of a successful call, or a second attempt, would make two).
// The number and the JSON-RPC methods of the other requests belong to the client library, not to
// the product, and are never pinned.
func connExpectOneCallWith(t *testing.T, what string, up *fakes.Upstream, header, want string) {
	t.Helper()
	connExpectEveryRequestWith(t, what, up, header, want)
	if calls := connAuths(up, header); len(calls) != 1 || calls[0] != want {
		t.Fatalf("%s: tools/call credentials %v, want exactly one carrying the credential", what, redactAll(calls))
	}
}

// connExpectRefusedThenRefreshed asserts a refusal followed by one retry: the first request is the
// refused attempt and carries the old credential (a 401 ends any client's exchange, so a refused
// attempt is exactly one request); every later request carries the refreshed one (none the old
// one, none without a credential); exactly one tools/call was made, with the refreshed credential.
func connExpectRefusedThenRefreshed(t *testing.T, up *fakes.Upstream, header, old, fresh string) {
	t.Helper()
	got := connRequestHeaders(up, header)
	if len(got) < 2 || got[0] != old {
		t.Fatalf("requests carried %v, want the refused attempt with the old credential first and the retry after it", redactAll(got))
	}
	for i, v := range got[1:] {
		if v != fresh {
			t.Fatalf("request %d after the refusal carried %q, want the refreshed credential %q", i+1, harness.Redact(v), harness.Redact(fresh))
		}
	}
	if calls := connAuths(up, header); len(calls) != 1 || calls[0] != fresh {
		t.Fatalf("tools/call credentials %v, want exactly one, with the refreshed credential", redactAll(calls))
	}
}

// connExpectEveryRequestWith asserts that the upstream got at least one request and that every
// one carried the value (used where the number of requests is not the point, such as a sync).
func connExpectEveryRequestWith(t *testing.T, what string, up *fakes.Upstream, header, want string) {
	t.Helper()
	got := connRequestHeaders(up, header)
	if len(got) == 0 {
		t.Fatalf("%s: the upstream received nothing", what)
	}
	for _, v := range got {
		if v != want {
			t.Fatalf("%s: a request carried %q, want %q on every request", what, harness.Redact(v), harness.Redact(want))
		}
	}
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

// connConnected returns a new person connected to env and the tokens issued to them.
func connConnected(t *testing.T, env *connEnv) (connPerson, fakes.IssuedTokens) {
	t.Helper()
	p := connNewPerson(t, env.s)
	issued := connConnect(t, env, p.c)
	env.up.Reset()
	return p, issued
}
