//go:build integration

package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	catapp "github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	catdomain "github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	catinfra "github.com/ai-pipes/mcp-registry/internal/modules/catalog/infra"
	idapp "github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/ai-pipes/mcp-registry/internal/platform/upstream"
)

// oauthProvider is a small OAuth authorization server: authorization code with PKCE (S256) checked
// at the token endpoint, refresh with ONE-TIME refresh tokens (a used one is invalid_grant), RFC
// 7009 revoke, failure modes, and a record of every request. Every token, code and the client
// secret carries a MARKER the tests look for in logs and audit records.
type oauthProvider struct {
	srv          *httptest.Server
	name         string
	clientID     string
	clientSecret string
	redirectURI  string

	mu           sync.Mutex
	seq          int
	codes        map[string]issuedCode
	refreshOK    map[string]bool
	expiresIn    int           // seconds an issued access token lives
	mode         string        // "ok", "invalid_grant" or "down" (token endpoint)
	refreshDelay time.Duration // held inside a refresh, so that concurrent callers overlap
	reqs         []providerRequest
	issued       []string // every secret the provider made, for the scans
	verifiers    []string
}

type issuedCode struct {
	challenge, redirect string
	used                bool
}

type providerRequest struct {
	path, grant, token, basic string
}

func newOAuthProvider(t *testing.T, name, redirectURI string) *oauthProvider {
	t.Helper()
	p := &oauthProvider{
		name: name, clientID: "cid-" + name, clientSecret: "CSECRET-MARKER-" + name, redirectURI: redirectURI,
		codes: map[string]issuedCode{}, refreshOK: map[string]bool{}, expiresIn: 3600, mode: "ok",
	}
	p.issued = append(p.issued, p.clientSecret)
	mux := http.NewServeMux()
	mux.HandleFunc("/token", p.token)
	mux.HandleFunc("/revoke", p.revoke)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *oauthProvider) endpoint(path string) string { return p.srv.URL + path }

func (p *oauthProvider) set(f func(*oauthProvider)) { p.mu.Lock(); f(p); p.mu.Unlock() }

func (p *oauthProvider) record(r providerRequest) {
	p.reqs = append(p.reqs, r)
}

// count is the number of requests to path with that grant ("" for any).
func (p *oauthProvider) count(path, grant string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.reqs {
		if r.path == path && (grant == "" || r.grant == grant) {
			n++
		}
	}
	return n
}

func (p *oauthProvider) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func (p *oauthProvider) tokensSeen(path string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.reqs {
		if r.path == path {
			out = append(out, r.token)
		}
	}
	return out
}

func (p *oauthProvider) secrets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append(append([]string{}, p.issued...), p.verifiers...)
}

// authorize is the person approving at the provider: it checks what the gateway put in the
// authorization URL and answers the code the browser would carry back.
func (p *oauthProvider) authorize(t *testing.T, authURL string) (code, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if !strings.HasPrefix(authURL, p.srv.URL+"/authorize?") || q.Get("response_type") != "code" || q.Get("client_id") != p.clientID ||
		q.Get("redirect_uri") != p.redirectURI || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatalf("authorization URL: %s", authURL)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	code = fmt.Sprintf("CODE-MARKER-%s-%d", p.name, p.seq)
	p.codes[code] = issuedCode{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	p.issued = append(p.issued, code, q.Get("state"))
	return code, q.Get("state")
}

func (p *oauthProvider) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if code != "" {
		_, _ = fmt.Fprintf(w, `{"error":%q}`, code)
	}
}

func (p *oauthProvider) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id, secret, hasBasic := r.BasicAuth()
	grant := r.PostForm.Get("grant_type")
	p.mu.Lock()
	rec := providerRequest{path: "/token", grant: grant, token: r.PostForm.Get("refresh_token")}
	if hasBasic {
		rec.basic = id
	}
	p.record(rec)
	mode, delay := p.mode, p.refreshDelay
	p.mu.Unlock()
	if mode == "down" {
		p.fail(w, http.StatusServiceUnavailable, "")
		return
	}
	if !hasBasic || id != p.clientID || secret != p.clientSecret {
		p.fail(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	switch grant {
	case "authorization_code":
		p.mu.Lock()
		c, ok := p.codes[r.PostForm.Get("code")]
		verifier := r.PostForm.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		valid := ok && !c.used && c.redirect == r.PostForm.Get("redirect_uri") &&
			base64.RawURLEncoding.EncodeToString(sum[:]) == c.challenge
		if ok {
			c.used = true
			p.codes[r.PostForm.Get("code")] = c
		}
		p.verifiers = append(p.verifiers, verifier)
		p.mu.Unlock()
		if !valid || mode == "invalid_grant" {
			p.fail(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		p.issue(w)
	case "refresh_token":
		if delay > 0 {
			time.Sleep(delay) // inside the provider: the callers overlap, the test does not sleep
		}
		p.mu.Lock()
		old := r.PostForm.Get("refresh_token")
		valid := p.refreshOK[old]
		delete(p.refreshOK, old) // one-time: a second use of the same token is refused
		p.mu.Unlock()
		if !valid || mode == "invalid_grant" {
			p.fail(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		p.issue(w)
	default:
		p.fail(w, http.StatusBadRequest, "unsupported_grant_type")
	}
}

func (p *oauthProvider) issue(w http.ResponseWriter) {
	p.mu.Lock()
	p.seq++
	access := fmt.Sprintf("AT-MARKER-%s-%d", p.name, p.seq)
	refresh := fmt.Sprintf("RT-MARKER-%s-%d", p.name, p.seq)
	p.refreshOK[refresh] = true
	p.issued = append(p.issued, access, refresh)
	exp := p.expiresIn
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","refresh_token":%q,"expires_in":%d}`, access, refresh, exp)
}

func (p *oauthProvider) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	p.record(providerRequest{path: "/revoke", token: r.PostForm.Get("token")})
	p.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// lastAccess is the access token the provider issued last.
func (p *oauthProvider) lastAccess() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.issued) - 1; i >= 0; i-- {
		if strings.HasPrefix(p.issued[i], "AT-MARKER-") {
			return p.issued[i]
		}
	}
	return ""
}

// --- the scenario kit -------------------------------------------------------------------------

const needsConnectionText = "An administrator needs to connect this server"

type conEnv struct {
	t   *testing.T
	rs  *realServer
	url string
	// secrets are the values the final scan looks for: the PAT, plus what the providers made.
	provs   []*oauthProvider
	oldLogs []string // the logs of the servers this test stopped
	secrets []string
}

func (e *conEnv) redirectURI() string { return "http://localhost:3000/api/connections/callback" }

func (e *conEnv) provider(name string) *oauthProvider {
	p := newOAuthProvider(e.t, name, e.redirectURI())
	e.provs = append(e.provs, p)
	return p
}

func (e *conEnv) admin(sub string) person {
	return person{e.rs, e.rs.person(sub, idapp.RoleAdmin)}
}

func (e *conEnv) member(sub string) person {
	return person{e.rs, e.rs.person(sub, idapp.RoleMember)}
}

func oauthAuth(p *oauthProvider) map[string]any {
	return map[string]any{
		"kind": "oauth_user", "header": "Authorization", "template": "Bearer {secret}",
		"oauth": map[string]any{
			"authorizeUrl": p.endpoint("/authorize"), "tokenUrl": p.endpoint("/token"), "revokeUrl": p.endpoint("/revoke"),
			"clientId": p.clientID, "scopes": []string{"read"},
		},
	}
}

// oauthServer registers an oauth_user server with the provider's client secret. Its first
// synchronisation (nobody is connected) fails with needs_connection, which is stored.
func (e *conEnv) oauthServer(admin person, slug string, p *oauthProvider, up *mcpFake) {
	e.t.Helper()
	body := serverBody(slug, up.url(), oauthAuth(p))
	body["secret"] = p.clientSecret
	status, v, raw := admin.do("POST", "/api/servers", body)
	if status != 201 || v.(map[string]any)["lastSyncError"] != needsConnectionText {
		e.t.Fatalf("create %s: %d %s", slug, status, raw)
	}
}

// noRedirect GETs a path with a session and returns the status and the Location header.
func (e *conEnv) noRedirect(path, cookie string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest("GET", e.rs.base+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "mcp_session", Value: cookie}) //nolint:gosec // G124: a request cookie of a test client
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode, res.Header.Get("Location")
}

// start begins a connect and approves it at the provider: the code and the state the browser
// would bring to the callback.
func (e *conEnv) start(p person, slug string, prov *oauthProvider) (code, state string) {
	e.t.Helper()
	status, v, raw := p.do("POST", "/api/connections/"+slug+"/start", nil)
	if status != 200 {
		e.t.Fatalf("start %s: %d %s", slug, status, raw)
	}
	return prov.authorize(e.t, v.(map[string]any)["url"].(string))
}

func callbackPath(code, state string) string {
	return "/api/connections/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
}

// connect runs the whole flow for a person and expects the success redirect.
func (e *conEnv) connect(p person, slug string, prov *oauthProvider) {
	e.t.Helper()
	code, state := e.start(p, slug, prov)
	status, loc := e.noRedirect(callbackPath(code, state), p.cookie)
	if status != http.StatusFound || loc != "/connections?connected="+slug {
		e.t.Fatalf("callback: %d %q", status, loc)
	}
}

func (e *conEnv) sync(p person, slug string) (int, string) {
	e.t.Helper()
	status, _, raw := p.do("POST", "/api/servers/"+slug+"/sync", nil)
	return status, raw
}

func (e *conEnv) lastSyncError(p person, slug string) any {
	e.t.Helper()
	_, v, _ := p.do("GET", "/api/servers/"+slug, nil)
	return v.(map[string]any)["lastSyncError"]
}

func (e *conEnv) connectionStatus(p person, slug string) string {
	e.t.Helper()
	_, v, raw := p.do("GET", "/api/connections", nil)
	for _, c := range v.([]any) {
		if m := c.(map[string]any); m["server"] == slug {
			return m["status"].(string)
		}
	}
	e.t.Fatalf("%s is not listed: %s", slug, raw)
	return ""
}

func (e *conEnv) rows(slug string) int {
	return queryInt(e.t, e.url, `select count(*)::int from connections.connections where server = $1`, slug)
}

// snapshot fingerprints the stored row: nothing may change in it.
func (e *conEnv) snapshot(slug string) string {
	return queryText(e.t, e.url, `select md5(access) || ':' || coalesce(md5(refresh), '-') || ':' || status || ':' || updated_at::text from connections.connections where server = $1`, slug)
}

func (e *conEnv) audit(action, slug string) int {
	return queryInt(e.t, e.url, `select count(*)::int from platform.audit_log where action = $1 and target = $2`, action, slug)
}

func (e *conEnv) expiredReason(slug string) string {
	return queryText(e.t, e.url, `select meta->>'reason' from platform.audit_log where action = 'connection.expired' and target = $1 order by id desc limit 1`, slug)
}

func lastOf(h []string) string {
	if len(h) == 0 {
		return ""
	}
	return h[len(h)-1]
}

func TestConnectionsOnTheComposedServer(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	rs := startReal(t, url, key, catalogConfig)
	e := &conEnv{t: t, rs: rs, url: url}
	const pat = "PAT-MARKER-0001"
	e.secrets = append(e.secrets, pat)

	t.Run("connect through HTTP, the sealed row, the audit, and a synchronisation with the person's token", func(t *testing.T) {
		prov, up := e.provider("flow"), newMCPFake(t, "alpha", "beta")
		alice := e.admin("alice")
		e.oauthServer(alice, "flow", prov, up)
		if got := e.connectionStatus(alice, "flow"); got != "missing" {
			t.Fatalf("before connecting: %s", got)
		}
		if status, raw := e.sync(alice, "flow"); status != 400 || !strings.Contains(raw, "needs_connection") {
			t.Fatalf("sync before connecting: %d %s", status, raw)
		}
		if up.listed.Load() != 0 {
			t.Fatal("the upstream was contacted without a credential")
		}

		e.connect(alice, "flow", prov)
		if prov.count("/token", "authorization_code") != 1 {
			t.Fatalf("code exchanges: %d", prov.count("/token", "authorization_code"))
		}
		if got := e.connectionStatus(alice, "flow"); got != "ready" {
			t.Fatalf("after connecting: %s", got)
		}
		if n := queryInt(t, url, `select count(*)::int from connections.connections where server = 'flow' and kind = 'oauth' and status = 'ready' and position(convert_to($1, 'utf8') in access) = 0 and position(convert_to($2, 'utf8') in refresh) = 0`, prov.lastAccess(), "RT-MARKER-flow-"); n != 1 {
			t.Fatal("the row is missing or holds a token in plain text")
		}
		if e.audit("connection.connect", "flow") != 1 {
			t.Fatal("no connection.connect audit record")
		}

		// The synchronisation by the same admin uses their token; the stub's stored text is cleared.
		status, raw := e.sync(alice, "flow")
		if status != 200 {
			t.Fatalf("sync after connecting: %d %s", status, raw)
		}
		if got := lastOf(up.authHeaders()); got != "Bearer "+prov.lastAccess() {
			t.Fatalf("the upstream saw %q, want the person's access token", got)
		}
		if got := e.lastSyncError(alice, "flow"); got != nil {
			t.Fatalf("lastSyncError = %v, want null after a successful sync", got)
		}
	})

	t.Run("a callback by another signed-in person creates nothing and leaves the attempt to its owner; a state is used once", func(t *testing.T) {
		prov, up := e.provider("other"), newMCPFake(t, "alpha")
		alice, bob := e.admin("alice2"), e.member("bob2")
		e.oauthServer(alice, "other", prov, up)
		code, state := e.start(alice, "other", prov)

		status, loc := e.noRedirect(callbackPath(code, state), bob.cookie)
		if status != http.StatusFound || loc != "/connections?error=invalid_state" {
			t.Fatalf("callback by another person: %d %q", status, loc)
		}
		if e.rows("other") != 0 || prov.count("/token", "authorization_code") != 0 {
			t.Fatal("another person's callback created a connection or reached the provider")
		}
		status, loc = e.noRedirect(callbackPath(code, state), alice.cookie)
		if status != http.StatusFound || loc != "/connections?connected=other" {
			t.Fatalf("the owner afterwards: %d %q", status, loc)
		}
		status, loc = e.noRedirect(callbackPath(code, state), alice.cookie)
		if status != http.StatusFound || loc != "/connections?error=invalid_state" {
			t.Fatalf("a second callback with the same state: %d %q", status, loc)
		}
		if prov.count("/token", "authorization_code") != 1 || e.rows("other") != 1 {
			t.Fatal("the state was used twice")
		}
		// The callback needs no session of its own, but never creates a connection without one.
		if status, loc := e.noRedirect(callbackPath("x", "y"), ""); status != http.StatusFound || loc != "/connections?error=unauthenticated" {
			t.Fatalf("callback without a session: %d %q", status, loc)
		}
	})

	t.Run("raw bytes that are not UTF-8 in the state are an unknown state, not an internal error", func(t *testing.T) {
		alice := e.admin("alice-bytes")
		// Postgres refuses such a text (SQLSTATE 22021); the page must say invalid_state.
		for _, state := range []string{"%ff%fe", "\xff\xfe", "ab\xc3"} {
			status, loc := e.noRedirect("/api/connections/callback?code=x&state="+state, alice.cookie)
			if status != http.StatusFound || loc != "/connections?error=invalid_state" {
				t.Fatalf("state %q: %d %q", state, status, loc)
			}
		}
	})

	t.Run("a PAT is saved, and a synchronisation of a pat_user server sends it in the catalog's header", func(t *testing.T) {
		up := newMCPFake(t, "alpha")
		alice := e.admin("alice3")
		auth := map[string]any{"kind": "pat_user", "header": "Authorization", "template": "Token {secret}"}
		if status, _, raw := alice.do("POST", "/api/servers", serverBody("patsrv", up.url(), auth)); status != 201 {
			t.Fatalf("create: %d %s", status, raw)
		}
		if status, raw := e.sync(alice, "patsrv"); status != 400 || !strings.Contains(raw, "needs_connection") {
			t.Fatalf("sync without a PAT: %d %s", status, raw)
		}
		if status, _, raw := alice.do("PUT", "/api/connections/patsrv/pat", map[string]any{"token": pat}); status != 204 {
			t.Fatalf("save PAT: %d %s", status, raw)
		}
		if got := e.connectionStatus(alice, "patsrv"); got != "ready" {
			t.Fatalf("status %s", got)
		}
		if status, raw := e.sync(alice, "patsrv"); status != 200 {
			t.Fatalf("sync with a PAT: %d %s", status, raw)
		}
		up.mu.Lock()
		defer up.mu.Unlock()
		if len(up.auths) == 0 || up.auths[len(up.auths)-1] != "Token "+pat {
			t.Fatalf("the upstream saw %q", up.auths)
		}
		if e.audit("connection.pat_saved", "patsrv") != 1 {
			t.Fatal("no connection.pat_saved audit record")
		}
		if n := queryInt(t, url, `select count(*)::int from connections.connections where server = 'patsrv' and kind = 'pat' and position(convert_to($1, 'utf8') in access) = 0`, pat); n != 1 {
			t.Fatal("the PAT is stored in plain text")
		}
	})

	t.Run("two concurrent synchronisations refresh once, with a one-time refresh token", func(t *testing.T) {
		prov, up := e.provider("race"), newMCPFake(t, "alpha")
		alice := e.admin("alice4")
		e.oauthServer(alice, "race", prov, up)
		prov.set(func(p *oauthProvider) { p.expiresIn = 30 }) // inside the 60 s refresh margin
		e.connect(alice, "race", prov)
		first := prov.lastAccess()
		prov.set(func(p *oauthProvider) { p.expiresIn = 3600; p.refreshDelay = 300 * time.Millisecond })

		var wg sync.WaitGroup
		statuses := make([]int, 2)
		for i := range statuses {
			wg.Add(1)
			go func() {
				defer wg.Done()
				statuses[i], _ = e.sync(alice, "race")
			}()
		}
		wg.Wait()
		if statuses[0] != 200 || statuses[1] != 200 {
			t.Fatalf("syncs: %v (a second use of the one-time refresh token would be invalid_grant)", statuses)
		}
		if n := prov.count("/token", "refresh_token"); n != 1 {
			t.Fatalf("the provider saw %d refreshes, want exactly 1", n)
		}
		second := prov.lastAccess()
		if second == first {
			t.Fatal("no new token was issued")
		}
		for _, h := range up.authHeaders() {
			if h != "Bearer "+second {
				t.Fatalf("the upstream saw %q, want the refreshed token", h)
			}
		}
		if e.connectionStatus(alice, "race") != "ready" {
			t.Fatal("the connection is not ready")
		}
	})

	t.Run("a refresh the provider rejects expires the connection and records the reference's failure", func(t *testing.T) {
		prov, up := e.provider("reject"), newMCPFake(t, "alpha")
		alice := e.admin("alice5")
		e.oauthServer(alice, "reject", prov, up)
		prov.set(func(p *oauthProvider) { p.expiresIn = 30 })
		e.connect(alice, "reject", prov)
		prov.set(func(p *oauthProvider) { p.mode = "invalid_grant" })

		status, raw := e.sync(alice, "reject")
		if status != 400 || !strings.Contains(raw, "needs_connection") {
			t.Fatalf("sync: %d %s", status, raw)
		}
		if got := e.lastSyncError(alice, "reject"); got != needsConnectionText {
			t.Fatalf("lastSyncError = %v", got)
		}
		if got := e.connectionStatus(alice, "reject"); got != "expired" {
			t.Fatalf("GET /api/connections: %s, want expired", got)
		}
		if e.expiredReason("reject") != "refresh_rejected" {
			t.Fatalf("audit reason %q", e.expiredReason("reject"))
		}
		if len(up.authHeaders()) != 0 {
			t.Fatal("the upstream was contacted without a credential")
		}
		// Expired is not refreshed again until the person reconnects.
		before := prov.count("/token", "refresh_token")
		if status, _ := e.sync(alice, "reject"); status != 400 || prov.count("/token", "refresh_token") != before {
			t.Fatal("an expired connection was refreshed again")
		}
	})

	t.Run("provider down: an unexpired token still works, an expired one is unavailable and nothing is stored", func(t *testing.T) {
		prov, up := e.provider("down"), newMCPFake(t, "alpha")
		alice := e.admin("alice6")
		e.oauthServer(alice, "down", prov, up)
		prov.set(func(p *oauthProvider) { p.expiresIn = 30 })
		e.connect(alice, "down", prov)
		token := prov.lastAccess()
		before := e.snapshot("down")
		prov.set(func(p *oauthProvider) { p.mode = "down" })

		if status, raw := e.sync(alice, "down"); status != 200 {
			t.Fatalf("sync with the provider down and an unexpired token: %d %s", status, raw)
		}
		if got := lastOf(up.authHeaders()); got != "Bearer "+token {
			t.Fatalf("the upstream saw %q, want the stored token", got)
		}
		if e.snapshot("down") != before {
			t.Fatal("the row changed although the provider did not answer")
		}

		exec(t, url, `update connections.connections set expires_at = now() - interval '1 hour' where server = 'down'`)
		before = e.snapshot("down")
		calls := len(up.authHeaders())
		status, raw := e.sync(alice, "down")
		if status == 200 || strings.Contains(raw, "needs_connection") {
			t.Fatalf("sync with the provider down and an expired token: %d %s", status, raw)
		}
		if got := e.lastSyncError(alice, "down"); got != "The OAuth provider is unavailable" {
			t.Fatalf("lastSyncError = %v", got)
		}
		if e.snapshot("down") != before || len(up.authHeaders()) != calls {
			t.Fatal("the row changed or the upstream was contacted")
		}
		if got := e.connectionStatus(alice, "down"); got != "ready" {
			t.Fatalf("status %s: an unreachable provider says nothing about the person's grant", got)
		}
	})

	t.Run("an admin who moves the token URL never has the old token used against the new endpoint", func(t *testing.T) {
		old, moved, up := e.provider("old"), e.provider("moved"), newMCPFake(t, "alpha")
		alice := e.admin("alice7")
		e.oauthServer(alice, "move", old, up)
		old.set(func(p *oauthProvider) { p.expiresIn = 30 }) // a refresh would be due
		e.connect(alice, "move", old)
		refreshes := old.count("/token", "refresh_token")

		next := oauthAuth(old)
		next["oauth"].(map[string]any)["tokenUrl"] = moved.endpoint("/token")
		if status, _, raw := alice.do("PUT", "/api/servers/move", serverBody("move", up.url(), next)); status != 200 {
			t.Fatalf("update: %d %s", status, raw)
		}
		status, raw := e.sync(alice, "move")
		if status != 400 || !strings.Contains(raw, "needs_connection") {
			t.Fatalf("sync after the move: %d %s", status, raw)
		}
		if n := moved.total(); n != 0 {
			t.Fatalf("the new endpoint was contacted %d times", n)
		}
		if old.count("/token", "refresh_token") != refreshes {
			t.Fatal("the old provider was asked to refresh after the move")
		}
		if got := e.connectionStatus(alice, "move"); got != "expired" || e.expiredReason("move") != "config_changed" {
			t.Fatalf("status %s, reason %q", got, e.expiredReason("move"))
		}
		if len(up.authHeaders()) != 0 {
			t.Fatal("the upstream was contacted with a token issued elsewhere")
		}
	})

	t.Run("disconnect revokes at the stored revoke URL and deletes; a changed revoke URL gets nothing", func(t *testing.T) {
		prov, up := e.provider("bye"), newMCPFake(t, "alpha")
		alice := e.admin("alice8")
		e.oauthServer(alice, "bye", prov, up)
		e.connect(alice, "bye", prov)
		refresh := "RT-MARKER-bye-"
		if status, _, raw := alice.do("DELETE", "/api/connections/bye", nil); status != 204 {
			t.Fatalf("disconnect: %d %s", status, raw)
		}
		if e.rows("bye") != 0 || e.audit("connection.disconnect", "bye") != 1 {
			t.Fatal("the row is still there or the audit record is missing")
		}
		seen := prov.tokensSeen("/revoke")
		if len(seen) != 1 || !strings.HasPrefix(seen[0], refresh) {
			t.Fatalf("revoked %v, want the refresh token", seen)
		}

		other, up2 := e.provider("bye2"), newMCPFake(t, "alpha")
		e.oauthServer(alice, "bye2", other, up2)
		e.connect(alice, "bye2", other)
		elsewhere := oauthAuth(other)
		elsewhere["oauth"].(map[string]any)["revokeUrl"] = other.endpoint("/revoke2")
		if status, _, raw := alice.do("PUT", "/api/servers/bye2", serverBody("bye2", up2.url(), elsewhere)); status != 200 {
			t.Fatalf("update: %d %s", status, raw)
		}
		if status, _, _ := alice.do("DELETE", "/api/connections/bye2", nil); status != 204 || e.rows("bye2") != 0 {
			t.Fatal("disconnect failed")
		}
		if n := other.count("/revoke", "") + other.count("/revoke2", ""); n != 0 {
			t.Fatalf("a token was revoked at a URL it was not issued with (%d)", n)
		}
	})

	t.Run("an upstream 401 during a sync leaves the connection ready and refreshes nothing", func(t *testing.T) {
		prov, up := e.provider("r401"), newMCPFake(t, "alpha")
		alice := e.admin("alice10")
		e.oauthServer(alice, "r401", prov, up)
		e.connect(alice, "r401", prov)
		before, refreshes := e.snapshot("r401"), prov.count("/token", "refresh_token")
		up.setReject(true)
		status, raw := e.sync(alice, "r401")
		if status != 502 || !strings.Contains(raw, "sync_failed") {
			t.Fatalf("sync against a rejecting upstream: %d %s", status, raw)
		}
		if got := e.lastSyncError(alice, "r401"); got != "Upstream rejected the credentials (HTTP 401)" {
			t.Fatalf("lastSyncError = %v", got)
		}
		if e.snapshot("r401") != before || e.connectionStatus(alice, "r401") != "ready" || e.audit("connection.expired", "r401") != 0 {
			t.Fatal("the connection was changed by an upstream 401 in a sync")
		}
		if prov.count("/token", "refresh_token") != refreshes {
			t.Fatal("a refresh was triggered by an upstream 401 in a sync")
		}
		up.setReject(false)
		if status, raw := e.sync(alice, "r401"); status != 200 {
			t.Fatalf("sync after the upstream recovered: %d %s", status, raw)
		}
	})

	t.Run("the scheduled sync of a restarted server uses the sync owner's connection and clears the stub's stored text", func(t *testing.T) {
		up := newMCPFake(t, "alpha")
		prov := e.provider("sched")
		alice := e.admin("alice9")
		e.oauthServer(alice, "sched", prov, up)
		e.connect(alice, "sched", prov)
		if status, raw := e.sync(alice, "sched"); status != 200 { // records alice as the credentials owner
			t.Fatalf("sync: %d %s", status, raw)
		}
		// What a milestone 3 or 4 image left behind on a personal-auth server.
		exec(t, url, `update catalog.servers set last_sync_error = $1 where slug = 'sched'`, needsConnectionText)
		if code := rs.stop("SIGTERM"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		e.oldLogs = append(e.oldLogs, rs.logs.String())
		again := startReal(t, url, key, catalogConfig)
		again.waitFor("catalog sync: done")
		for _, rec := range again.logs.find(t, "catalog sync: done") {
			for _, f := range rec["failed"].([]any) {
				if f.(map[string]any)["server"] == "sched" {
					t.Fatalf("the scheduled sync of sched failed: %v", rec)
				}
			}
		}
		if got := queryText(t, url, `select coalesce(last_sync_error, 'null') from catalog.servers where slug = 'sched'`); got != "null" {
			t.Fatalf("last_sync_error = %q, want cleared by the next successful sync", got)
		}
		rs = again
		e.rs = again
	})

	t.Run("no secret appears in the logs or the audit records of any scenario", func(t *testing.T) {
		all := append([]string{}, e.secrets...)
		for _, p := range e.provs {
			all = append(all, p.secrets()...)
		}
		if len(all) < 20 {
			t.Fatalf("only %d markers were planted", len(all))
		}
		audit := queryText(t, url, `select coalesce(string_agg(to_jsonb(a)::text, ' '), '') from platform.audit_log a`)
		logs := rs.logs.String() + strings.Join(e.oldLogs, "")
		for _, s := range all {
			if strings.Contains(logs, s) {
				t.Errorf("the log holds a secret (%d characters, prefix %q)", len(s), s[:min(10, len(s))])
			}
			if strings.Contains(audit, s) {
				t.Errorf("the audit log holds a secret (prefix %q)", s[:min(10, len(s))])
			}
		}
	})
}

// holdServers pauses ONE synchronisation right after it read its server: the window in which an
// admin can change the server before the sync asks connections for the person's secret.
type holdServers struct {
	catapp.ServerRepository
	arm     atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (h *holdServers) FindBySlug(ctx context.Context, slug string) (*catdomain.Server, error) {
	s, err := h.ServerRepository.FindBySlug(ctx, slug)
	if err == nil && h.arm.CompareAndSwap(true, false) {
		h.entered <- struct{}{}
		<-h.release
	}
	return s, err
}

// A sync that read configuration A must never send a token issued for configuration B to A's URL,
// even when the person reconnected for B in between.
func TestAResolvedCredentialIsBoundToTheConfigurationTheSyncWillUse(t *testing.T) {
	url := pgtest.StartURL(t)
	hold := &holdServers{entered: make(chan struct{}), release: make(chan struct{})}
	build := func(rt Runtime) []Module {
		id := newIdentityWiring(rt)
		repos := catinfra.NewPg(rt.Pool)
		hold.ServerRepository = repos.Servers
		repos.Servers = hold
		cat := catalogWiringOver(rt, id, repos, upstream.New(upstream.Options{}))
		con := newConnectionsWiring(rt, id, cat)
		return []Module{id.module(), platformModule(rt, id), cat.module(), con.module()}
	}
	rs := startRealWith(t, url, newKey(t), catalogConfig, build)
	e := &conEnv{t: t, rs: rs, url: url}
	alice := e.admin("alice")
	upA, upB := newMCPFake(t, "alpha"), newMCPFake(t, "alpha")
	const patA, patB = "PAT-MARKER-A", "PAT-MARKER-B"
	auth := map[string]any{"kind": "pat_user", "header": "Authorization", "template": "Token {secret}"}
	if status, _, raw := alice.do("POST", "/api/servers", serverBody("moving", upA.url(), auth)); status != 201 {
		t.Fatalf("create: %d %s", status, raw)
	}
	if status, _, raw := alice.do("PUT", "/api/connections/moving/pat", map[string]any{"token": patA}); status != 204 {
		t.Fatalf("save PAT: %d %s", status, raw)
	}

	// The sync reads configuration A and stops.
	hold.arm.Store(true)
	type answer struct {
		status int
		raw    string
	}
	done := make(chan answer, 1)
	go func() {
		s, raw := e.sync(alice, "moving")
		done <- answer{s, raw}
	}()
	await(t, hold.entered, "the sync to read the server")

	// Meanwhile the admin moves the server to B and the person reconnects for B.
	if status, _, raw := alice.do("PUT", "/api/servers/moving", serverBody("moving", upB.url(), auth)); status != 200 {
		t.Fatalf("move: %d %s", status, raw)
	}
	if status, _, raw := alice.do("PUT", "/api/connections/moving/pat", map[string]any{"token": patB}); status != 204 {
		t.Fatalf("save the PAT for B: %d %s", status, raw)
	}
	before := e.snapshot("moving")
	close(hold.release)
	got := <-done

	if got.status == 200 {
		t.Fatalf("the sync that read A succeeded: %s", got.raw)
	}
	for _, h := range upA.authHeaders() {
		if strings.Contains(h, patB) {
			t.Fatalf("the token issued for B was sent to A: %q", h)
		}
	}
	if upB.listed.Load() != 0 {
		t.Fatal("B was contacted by the sync that read A")
	}
	if text := e.lastSyncError(alice, "moving"); text != "The OAuth provider is unavailable" {
		t.Fatalf("lastSyncError = %v, want the unavailable-class text", text)
	}
	if e.snapshot("moving") != before || e.connectionStatus(alice, "moving") != "ready" {
		t.Fatal("the connection for B was changed")
	}

	// A new sync reads B and works with B's token.
	if status, raw := e.sync(alice, "moving"); status != 200 {
		t.Fatalf("sync against B: %d %s", status, raw)
	}
	if h := lastOf(upB.authHeaders()); h != "Token "+patB {
		t.Fatalf("B saw %q", h)
	}
}

// A NUL in the state reaches no query: Postgres would refuse such a text, so the callback answers
// invalid_state itself (the fakes cannot show the difference, only the real store can).
func TestACallbackStateWithANULIsInvalidStateAgainstTheRealStore(t *testing.T) {
	url := pgtest.StartURL(t)
	rs := startReal(t, url, newKey(t), catalogConfig)
	e := &conEnv{t: t, rs: rs, url: url}
	alice := e.member("alice")
	status, loc := e.noRedirect("/api/connections/callback?code=x&state=%00", alice.cookie)
	if status != http.StatusFound || loc != "/connections?error=invalid_state" {
		t.Fatalf("%d %q", status, loc)
	}
	if n := len(rs.logs.find(t, "connections: connect callback failed")); n != 0 {
		t.Fatalf("%d unexpected-error records", n)
	}
}
