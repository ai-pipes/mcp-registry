//go:build integration

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	idapp "github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	firstStartMsg = "first start: open this one-time link within 15 minutes to sign in as admin"
	publicOrigin  = "http://localhost:3000"
)

// realServer is a running server with the real module list.
type realServer struct {
	*instance
	rt   Runtime
	deps idapp.Deps
	url  string // the database
}

func startReal(t *testing.T, url string, key []byte, edit func(*config.Config)) *realServer {
	t.Helper()
	return startRealWith(t, url, key, edit, modules)
}

// startRealWith is startReal over the module list that build returns for the runtime (a test
// that runs without a module, or with another embedder, starts from modules(rt)).
func startRealWith(t *testing.T, url string, key []byte, edit func(*config.Config), build func(Runtime) []Module) *realServer {
	t.Helper()
	return startRealOpts(t, url, key, edit, build, nil)
}

// startRealOpts is startRealWith with a hook on the Options (a test seam such as callsGrace).
func startRealOpts(t *testing.T, url string, key []byte, edit func(*config.Config), build func(Runtime) []Module, opts func(*Options)) *realServer {
	t.Helper()
	cfg := baseConfig(url, key)
	cfg.OIDCLoginGroup, cfg.OIDCAdminGroup = "mcp-users", "mcp-admins" // the defaults of config.Load
	if edit != nil {
		edit(&cfg)
	}
	rs := &realServer{url: url}
	rs.instance = startInstance(t, cfg, func(o *Options) {
		if opts != nil {
			opts(o)
		}
		o.modules = func(rt Runtime) []Module {
			rs.rt = rt
			return build(rt)
		}
	})
	rs.deps = newIdentityWiring(rs.rt).deps
	return rs
}

// call sends one request; non-GET requests carry the Origin the CSRF check wants.
func call(t *testing.T, method, url, cookie string, body any, header ...string) (int, []byte, http.Header) {
	t.Helper()
	// A string body is sent as it is; anything else as JSON.
	var rd io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet {
		req.Header.Set("Origin", publicOrigin)
	}
	if _, raw := body.(string); body != nil && !raw {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "mcp_session", Value: cookie}) //nolint:gosec // G124: a request cookie of a test client; attributes only matter on Set-Cookie
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b, res.Header
}

// signIn redeems a link code and returns the session cookie value.
func (s *realServer) signIn(code string) string {
	s.t.Helper()
	status, body, h := call(s.t, "POST", s.base+"/api/auth/link", "", map[string]string{"code": code})
	if status != 204 {
		s.t.Fatalf("POST /api/auth/link: %d %s", status, body)
	}
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name == "mcp_session" && c.Value != "" {
			return c.Value
		}
	}
	s.t.Fatal("no session cookie")
	return ""
}

// person provisions someone as the CLI would and signs them in.
func (s *realServer) person(sub string, role idapp.RoleChoice) string {
	s.t.Helper()
	ctx := context.Background()
	if _, err := idapp.ProvisionUser(ctx, s.deps, idapp.ProvisionInput{Sub: sub, Name: sub, Role: role}); err != nil {
		s.t.Fatal(err)
	}
	link, err := idapp.IssueLoginLink(ctx, s.deps, idapp.IssueLoginLinkInput{Sub: sub})
	if err != nil {
		s.t.Fatal(err)
	}
	return s.signIn(link.Code.Reveal())
}

// firstLinkCode is the code of the one first-start record of the log.
func (s *realServer) firstLinkCode() string {
	s.t.Helper()
	recs := s.logs.find(s.t, firstStartMsg)
	if len(recs) != 1 {
		s.t.Fatalf("first-start records: %d\n%s", len(recs), s.logs.String())
	}
	return codeOf(s.t, recs[0])
}

func codeOf(t *testing.T, rec map[string]any) string {
	t.Helper()
	u, _ := rec["url"].(string)
	i := strings.Index(u, "#link=")
	if i < 0 {
		t.Fatalf("no link in %v", rec)
	}
	return u[i+len("#link="):]
}

func queryInt(t *testing.T, url, sql string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	var n int
	if err := c.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func schemaCheck(t *testing.T, name string, body []byte) {
	t.Helper()
	file := filepath.Join("..", "..", "..", "compat", "schemas", name+".json")
	raw, err := os.ReadFile(file) //nolint:gosec // G304: a fixed directory and a name from the test itself
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	id := "https://schemas.invalid/" + name + ".json"
	if err := c.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: not JSON: %s", name, body)
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, body)
	}
}

func errorCode(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not an error body: %s", body)
	}
	return e.Error.Code, e.Error.Message
}

// Full path: first start -> one record with the link -> POST /api/auth/link -> GET /api/auth/me.
// The code is in that record and nowhere else.
func TestFirstStartLinkSignsTheLocalAdminIn(t *testing.T) {
	url := pgtest.StartURL(t)
	s := startReal(t, url, newKey(t), nil)

	recs := s.logs.find(t, firstStartMsg)
	if len(recs) != 1 || levelOf(recs[0]) != levelWarn {
		t.Fatalf("records: %v", recs)
	}
	u, _ := recs[0]["url"].(string)
	if !regexp.MustCompile(`^http://localhost:3000/login#link=ll_[A-Za-z0-9_-]{43}$`).MatchString(u) {
		t.Fatalf("url field: %q", u)
	}
	code := codeOf(t, recs[0])

	// The code appears in exactly that line of the log and nowhere in the audit.
	lines := 0
	for _, l := range strings.Split(s.logs.String(), "\n") {
		if strings.Contains(l, code) {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("the code appears in %d log lines", lines)
	}
	if n := queryInt(t, url, `select count(*) from platform.audit_log where position($1 in action || coalesce(target,'') || meta::text) > 0`, code); n != 0 {
		t.Errorf("the code is in %d audit rows", n)
	}
	if n := queryInt(t, url, `select count(*) from platform.audit_log where action = 'auth.link_issued'`); n != 1 {
		t.Errorf("auth.link_issued rows: %d", n)
	}

	cookie := s.signIn(code)
	status, body, _ := call(t, "GET", s.base+"/api/auth/me", cookie, nil)
	var me struct{ UserID, Name, Role string }
	if err := json.Unmarshal(body, &me); status != 200 || err != nil || me.Role != "admin" {
		t.Fatalf("/api/auth/me: %d %s", status, body)
	}
	schemaCheck(t, "MeResponse", body)
	if status, _, _ := call(t, "POST", s.base+"/api/auth/link", "", map[string]string{"code": code}); status != 401 {
		t.Errorf("a used code: %d", status)
	}
	s.stop("SIGTERM")
}

// A restart before the first sign-in prints a new link and kills the old one; after the sign-in no
// link is printed.
func TestFirstStartRepeatsUntilTheAdminSignedIn(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	s1 := startReal(t, url, key, nil)
	first := s1.firstLinkCode()
	s1.stop("SIGTERM")

	s2 := startReal(t, url, key, nil)
	second := s2.firstLinkCode()
	if second == first {
		t.Fatal("the restart printed the same code")
	}
	if status, _, _ := call(t, "POST", s2.base+"/api/auth/link", "", map[string]string{"code": first}); status != 401 {
		t.Errorf("the old code: %d", status)
	}
	s2.signIn(second)
	s2.stop("SIGTERM")

	s3 := startReal(t, url, key, nil)
	if n := len(s3.logs.find(t, firstStartMsg)); n != 0 {
		t.Errorf("%d first-start records after the sign-in", n)
	}
	if strings.Contains(s3.logs.String(), "ll_") {
		t.Error("a link code in the log")
	}
	if n := queryInt(t, url, `select count(*) from identity.users`); n != 1 {
		t.Errorf("users: %d", n)
	}
	s3.stop("SIGTERM")
}

// With OIDC configured nobody is created and nothing is printed; the server starts although the
// identity provider is not reachable (discovery is lazy).
func TestNoFirstStartWithOIDC(t *testing.T) {
	url := pgtest.StartURL(t)
	s := startReal(t, url, newKey(t), func(c *config.Config) {
		c.PublicURL = publicOrigin
		c.OIDC = &config.OIDCClient{Issuer: "http://127.0.0.1:1", ClientID: "id", ClientSecret: config.NewSecret("secret")}
		c.OIDCScopes = "openid profile email groups"
	})
	if n := len(s.logs.find(t, firstStartMsg)) + len(s.logs.find(t, "first start: could not issue the sign-in link")); n != 0 {
		t.Errorf("%d first-start records", n)
	}
	if n := queryInt(t, url, `select count(*) from identity.users`); n != 0 {
		t.Errorf("users: %d", n)
	}
	status, body, _ := call(t, "GET", s.base+"/api/auth/methods", "", nil)
	if status != 200 || string(body) != `{"oidc":true}` {
		t.Errorf("methods: %d %s", status, body)
	}
	// The provider is down: login answers with the redirect, not with a failure of the server.
	req, _ := http.NewRequest("GET", s.base+"/api/auth/login", nil)
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 302 || res.Header.Get("Location") != "/?loginError=idp_unavailable" {
		t.Errorf("login: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	s.stop("SIGTERM")
}

// The session guards are injected into identity's routes and the platform routes; these requests go
// through the whole composed server.
func TestTheGuardsAreWiredIntoTheComposedServer(t *testing.T) {
	url := pgtest.StartURL(t)
	s := startReal(t, url, newKey(t), nil)
	admin := s.signIn(s.firstLinkCode())
	member := s.person("ann", idapp.RoleMember)

	type tc struct {
		name, method, path, cookie string
		header                     []string
		status                     int
		code, message              string
	}
	for _, c := range []tc{
		{"anonymous GET /api/tokens", "GET", "/api/tokens", "", nil, 401, "unauthenticated", "sign in required"},
		{"anonymous GET /api/tokens/x", "GET", "/api/tokens/x", "", nil, 401, "unauthenticated", "sign in required"},
		{"member GET /api/users", "GET", "/api/users", member, nil, 403, "forbidden", "admin only"},
		{"member GET /api/users/x/y", "GET", "/api/users/x/y", member, nil, 403, "forbidden", "admin only"},
		{"anonymous GET /api/users", "GET", "/api/users", "", nil, 401, "unauthenticated", "sign in required"},
		{"cookie and Authorization on /api/auth/me", "GET", "/api/auth/me", admin, []string{"Authorization", "Bearer sk_x"}, 401, "unauthenticated", "tokens do not work on the web API"},
		{"cookie and Authorization on /api/tokens", "GET", "/api/tokens", admin, []string{"Authorization", "Bearer sk_x"}, 401, "unauthenticated", "tokens do not work on the web API"},
		{"anonymous GET /api/audit", "GET", "/api/audit", "", nil, 401, "unauthenticated", "sign in required"},
		{"member GET /api/audit", "GET", "/api/audit", member, nil, 403, "forbidden", "admin only"},
		{"member GET /api/audit with a bad query", "GET", "/api/audit?limit=abc", member, nil, 403, "forbidden", "admin only"},
		{"cookie and Authorization on /api/audit", "GET", "/api/audit", admin, []string{"Authorization", "Bearer sk_x"}, 401, "unauthenticated", "tokens do not work on the web API"},
		{"anonymous GET /api/audit/x", "GET", "/api/audit/x", "", nil, 401, "unauthenticated", "sign in required"},
		{"anonymous GET /api/version", "GET", "/api/version", "", nil, 401, "unauthenticated", "sign in required"},
		{"Authorization only on /api/version", "GET", "/api/version", "", []string{"Authorization", "Bearer sk_x"}, 401, "unauthenticated", "tokens do not work on the web API"},
	} {
		status, body, _ := call(t, c.method, s.base+c.path, c.cookie, nil, c.header...)
		code, msg := "", ""
		if status != 200 {
			code, msg = errorCode(t, body)
		}
		if status != c.status || code != c.code || msg != c.message {
			t.Errorf("%s: %d %s/%s, want %d %s/%s", c.name, status, code, msg, c.status, c.code, c.message)
		}
	}
	// The admin check runs before the body is parsed; an unknown path under a prefix is the JSON 404
	// for an admin; a cookie of 65 characters is no session; the sign-in routes need no session.
	if status, body, _ := call(t, "POST", s.base+"/api/users/not-a-uuid/disabled", member, "x", "Content-Type", "text/plain"); status != 403 {
		t.Errorf("member POST /api/users/not-a-uuid/disabled: %d %s", status, body)
	}
	if status, body, _ := call(t, "GET", s.base+"/api/users/x/y", admin, nil); status != 404 {
		t.Errorf("admin on an unknown path under /api/users: %d %s", status, body)
	} else if code, _ := errorCode(t, body); code != "not_found" {
		t.Errorf("admin unknown path body: %s", body)
	}
	// The longest session id that is looked up is 64 characters: a stored session with a 64-character
	// id opens /api/auth/me, the same person with a 65-character one is anonymous.
	for n, want := range map[int]int{64: 200, 65: 401} {
		id := strings.Repeat("s", n)
		sum := sha256.Sum256([]byte(id))
		exec(t, url, `insert into identity.sessions (id_hash, user_id, created_at, last_seen_at, expires_at)
			select $1, id, now(), now(), now() + interval '1 day' from identity.users where oidc_sub = 'ann'`, hex.EncodeToString(sum[:]))
		if status, _, _ := call(t, "GET", s.base+"/api/auth/me", id, nil); status != want {
			t.Errorf("session cookie of %d characters: %d, want %d", n, status, want)
		}
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/api/auth/login", "/api/auth/callback"} {
		res, err := noFollow.Get(s.base + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 302 {
			t.Errorf("anonymous GET %s: %d, want the redirect", path, res.StatusCode)
		}
		if loc := res.Header.Get("Location"); path == "/api/auth/login" && loc != "/?loginError=oidc_not_configured" {
			t.Errorf("GET /api/auth/login without OIDC: Location %q", loc)
		}
	}
	if status, body, _ := call(t, "POST", s.base+"/api/auth/link", "", map[string]string{"code": "ll_nope"}); status != 401 {
		t.Errorf("anonymous POST /api/auth/link: %d %s", status, body)
	}
	// Controls: the same routes open for the right person.
	for _, c := range []struct{ path, cookie string }{
		{"/api/tokens", member},
		{"/api/users", admin},
		{"/api/auth/me", member},
		{"/api/audit", admin},
		{"/api/version", member},
		{"/api/version", admin},
	} {
		if status, body, _ := call(t, "GET", s.base+c.path, c.cookie, nil); status != 200 {
			t.Errorf("GET %s: %d %s", c.path, status, body)
		}
	}
	s.stop("SIGTERM")
}

func TestVersionRoute(t *testing.T) {
	url := pgtest.StartURL(t)
	s := startReal(t, url, newKey(t), nil)
	member := s.person("ann", idapp.RoleMember)
	status, body, h := call(t, "GET", s.base+"/api/version", member, nil)
	if status != 200 || string(body) != `{"version":"9.9.9-test"}` || !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s %v", status, body, h)
	}
	schemaCheck(t, "VersionResponse", body)
	s.stop("SIGTERM")
}

type auditRow struct {
	ID      string         `json:"id"`
	At      string         `json:"at"`
	ActorID *string        `json:"actorId"`
	Action  string         `json:"action"`
	Target  *string        `json:"target"`
	Meta    map[string]any `json:"meta"`
}

func (s *realServer) audit(admin, query string) (int, []byte) {
	s.t.Helper()
	status, body, _ := call(s.t, "GET", s.base+"/api/audit"+query, admin, nil)
	return status, body
}

func TestAuditRoute(t *testing.T) {
	url := pgtest.StartURL(t)
	s := startReal(t, url, newKey(t), nil)
	admin := s.signIn(s.firstLinkCode())

	// Enough rows to cross from 9 to 10 digits-wise: a text order would put "9" after "10".
	for i := range 14 {
		if status, body, _ := call(t, "POST", s.base+"/api/tokens", admin, map[string]string{"name": fmt.Sprintf("t%d", i)}); status != 201 {
			t.Fatalf("create token: %d %s", status, body)
		}
	}
	// A row without meta and one with a number a double would round.
	exec(t, url, `insert into platform.audit_log (action, meta) values ('test.null_meta', null), ('test.big', '{"n": 12345678901234567890, "a": [1, 2]}')`)

	var all []string
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(ctx, `select id::text from platform.audit_log order by audit_log.id desc`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		all = append(all, id)
	}
	rows.Close()
	_ = c.Close(ctx)
	if len(all) < 15 {
		t.Fatalf("only %d audit rows", len(all))
	}

	status, body := s.audit(admin, "")
	if status != 200 {
		t.Fatalf("%d %s", status, body)
	}
	schemaCheck(t, "AuditList", body)
	var first []auditRow
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	if len(first) != len(all) || first[0].ID != all[0] {
		t.Fatalf("default list: %d entries, first %s; want %d, %s", len(first), first[0].ID, len(all), all[0])
	}
	iso := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
	byAction := map[string]auditRow{}
	for _, e := range first {
		if !iso.MatchString(e.At) {
			t.Errorf("at %q", e.At)
		}
		byAction[e.Action] = e
	}
	if e := byAction["test.null_meta"]; e.Meta == nil || len(e.Meta) != 0 || e.ActorID != nil || e.Target != nil {
		t.Errorf("null meta row: %+v", e)
	}
	if !strings.Contains(string(body), `"n":12345678901234567890`) {
		t.Errorf("a large number was changed: %s", body)
	}
	if e := byAction["token.create"]; e.ActorID == nil || e.Target == nil || e.Meta["name"] == nil {
		t.Errorf("token.create: %+v", e)
	}

	// Paging with before and every limit: no gaps, no repeats, numeric order.
	for _, limit := range []int{1, 4, 7} {
		var got []string
		q := "?limit=" + strconv.Itoa(limit)
		for range len(all) + 2 {
			status, body := s.audit(admin, q)
			if status != 200 {
				t.Fatalf("GET /api/audit%s: %d %s", q, status, body)
			}
			var page []auditRow
			if err := json.Unmarshal(body, &page); err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			if len(page) > limit {
				t.Fatalf("%d entries for limit %d", len(page), limit)
			}
			for _, e := range page {
				got = append(got, e.ID)
			}
			q = "?limit=" + strconv.Itoa(limit) + "&before=" + page[len(page)-1].ID
		}
		if !slices.Equal(got, all) {
			t.Errorf("limit %d: paged %v, want %v", limit, got, all)
		}
	}

	// Query validation: the path in the message is the field.
	for _, c := range []struct {
		query string
		ok    bool
		field string
	}{
		{"?limit=0", false, "limit"},
		{"?limit=501", false, "limit"},
		{"?limit=abc", false, "limit"},
		{"?limit=", false, "limit"},
		{"?limit=1.5", false, "limit"},
		{"?limit=-1", false, "limit"},
		{"?before=abc", false, "before"},
		{"?before=", false, "before"},
		{"?before=" + strings.Repeat("9", 19), false, "before"},
		{"?before=-1", false, "before"},
		{"?before=abc&limit=0", false, "before"},
		{"?before=1&limit=0", false, "limit"},
		{"?limit=1e2", true, ""},
		{"?limit=%205%20", true, ""},
		{"?limit=0x10", true, ""},
		{"?limit=500", true, ""},
		{"?before=" + strings.Repeat("9", 18), true, ""},
		{"?before=0", true, ""},
		{"?other=1", true, ""},
	} {
		status, body := s.audit(admin, c.query)
		if c.ok {
			if status != 200 {
				t.Errorf("%s: %d %s", c.query, status, body)
			}
			continue
		}
		code, msg := errorCode(t, body)
		if status != 422 || code != "invalid_request" || !strings.HasPrefix(msg, c.field+": ") {
			t.Errorf("%s: %d %s %q, want 422 invalid_request %q", c.query, status, code, msg, c.field+": ...")
		}
	}
	if status, body := s.audit(admin, "?before=1"); status != 200 || string(body) != "[]" {
		t.Errorf("before=1: %d %s", status, body)
	}
	if status, body := s.audit(admin, "?limit=1e2"); status != 200 {
		t.Errorf("limit=1e2: %d %s", status, body)
	}
	// 403 for a member, with the same error shape.
	member := s.person("bob", idapp.RoleMember)
	if status, body := s.audit(member, ""); status != 403 {
		t.Errorf("member: %d %s", status, body)
	}
	// Mutations are not routes.
	if status, _, _ := call(t, "POST", s.base+"/api/audit", admin, nil); status != 404 && status != 405 {
		t.Errorf("POST /api/audit: %d", status)
	}
	s.stop("SIGTERM")
}

// The identity module registers one job that prunes at start and every 24 hours; the platform
// module's routes come after identity's.
func TestPruningJobDeletesWhatExpired(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	s := startReal(t, url, key, nil)
	s.person("ann", idapp.RoleMember)
	s.stop("SIGTERM")

	// A person with an expired link, a dead session (idle 13 h), an expired one (30 d), and live ones.
	exec(t, url, `insert into identity.users (oidc_sub, name, role) values ('p', 'P', 'member')`)
	seed := func() {
		exec(t, url, `delete from identity.sessions; delete from identity.login_links`)
		exec(t, url, `insert into identity.login_links (code_hash, user_id, created_at, expires_at)
			select 'expired', id, now() - interval '20 minutes', now() - interval '5 minutes' from identity.users where oidc_sub = 'p'`)
		exec(t, url, `insert into identity.login_links (code_hash, user_id, created_at, expires_at)
			select 'live', id, now(), now() + interval '10 minutes' from identity.users where oidc_sub = 'p'`)
		exec(t, url, `insert into identity.sessions (id_hash, user_id, created_at, last_seen_at, expires_at)
			select h, id, now() - interval '14 hours', now() - ls::interval, now() + ex::interval
			from identity.users, (values (repeat('a', 64), '13 hours', '1 day'), (repeat('b', 64), '1 minute', '-1 minute'), (repeat('c', 64), '1 minute', '1 day')) v(h, ls, ex)
			where oidc_sub = 'p'`)
	}
	seed()

	// Run once the way the scheduler does (at start): only the live link and the live session stay.
	s2 := startReal(t, url, key, nil)
	deadline := time.Now().Add(waitLimit)
	for queryInt(t, url, `select count(*) from identity.sessions`) != 1 || queryInt(t, url, `select count(*) from identity.login_links where code_hash in ('expired', 'live')`) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the start did not prune")
		}
		time.Sleep(20 * time.Millisecond) //nolint:forbidigo // polling a database count with a deadline
	}
	if queryInt(t, url, `select count(*) from identity.sessions where id_hash = repeat('c', 64)`) != 1 ||
		queryInt(t, url, `select count(*) from identity.login_links where code_hash = 'live'`) != 1 {
		t.Error("a live row was deleted")
	}
	if recs := s2.logs.find(t, "session retention: done"); len(recs) < 1 || recs[0]["sessions"] != float64(2) || levelOf(recs[0]) != levelInfo {
		t.Errorf("done record: %v", recs)
	}

	// And the job itself, called directly.
	var jobs []string
	var run func(context.Context) error
	for _, m := range modules(s2.rt) {
		for _, j := range m.Jobs {
			jobs = append(jobs, j.Name)
			if j.Name == "session-retention" {
				if j.Every != 24*time.Hour {
					t.Errorf("period %v", j.Every)
				}
				run = j.Run
			}
		}
	}
	if run == nil {
		t.Fatalf("no session-retention job among %v", jobs)
	}
	seed()
	if err := run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if queryInt(t, url, `select count(*) from identity.sessions`) != 1 || queryInt(t, url, `select count(*) from identity.login_links where code_hash = 'expired'`) != 0 {
		t.Error("the job did not prune")
	}
	s2.stop("SIGTERM")
}

// With another person in the database a start neither announces a link nor creates anybody.
func TestFirstStartPrintsNothingWhenAnotherPersonExists(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	startReal(t, url, key, nil).stop("SIGTERM") // creates local-admin
	exec(t, url, `insert into identity.users (oidc_sub, name, role) values ('bob', 'Bob', 'member')`)
	s := startReal(t, url, key, nil)
	if n := len(s.logs.find(t, firstStartMsg)); n != 0 {
		t.Errorf("%d first-start records although another person exists", n)
	}
	if n := queryInt(t, url, `select count(*) from identity.users`); n != 2 {
		t.Errorf("users: %d", n)
	}
	if strings.Contains(s.logs.String(), "ll_") {
		t.Error("a link code in the log")
	}
	s.stop("SIGTERM")
}

// Paging across 99 -> 100: a text order would put "99" after "100".
func TestAuditPagingAcrossNinetyNineAndAHundred(t *testing.T) {
	url := pgtest.StartURL(t)
	s := startReal(t, url, newKey(t), nil)
	admin := s.signIn(s.firstLinkCode())
	exec(t, url, `insert into platform.audit_log (action) select 'test.fill' from generate_series(1, 130)`)
	if n := queryInt(t, url, `select count(*) from platform.audit_log where id = 99 or id = 100`); n != 2 {
		t.Fatalf("the ids 99 and 100 must exist, found %d", n)
	}
	status, body := s.audit(admin, "?limit=2&before=101")
	var page []auditRow
	if err := json.Unmarshal(body, &page); status != 200 || err != nil || len(page) != 2 || page[0].ID != "100" || page[1].ID != "99" {
		t.Fatalf("before=101: %d %s", status, body)
	}
	var got []string
	q := "?limit=50"
	for range 10 {
		_, body := s.audit(admin, q)
		var p []auditRow
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
		if len(p) == 0 {
			break
		}
		for _, e := range p {
			got = append(got, e.ID)
		}
		q = "?limit=50&before=" + p[len(p)-1].ID
	}
	total := queryInt(t, url, `select count(*) from platform.audit_log`)
	if len(got) != total {
		t.Fatalf("paged %d entries, the table holds %d", len(got), total)
	}
	for i, id := range got {
		if i > 0 {
			prev, _ := strconv.Atoi(got[i-1])
			cur, _ := strconv.Atoi(id)
			if cur >= prev {
				t.Fatalf("not strictly descending at %d: %s after %s", i, id, got[i-1])
			}
		}
	}
	s.stop("SIGTERM")
}
