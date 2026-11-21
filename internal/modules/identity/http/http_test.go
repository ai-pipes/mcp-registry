package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	idhttp "github.com/ai-pipes/mcp-registry/internal/modules/identity/http"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

const origin = "https://mcp.example.com"

type env struct {
	t    *testing.T
	f    *fixture
	h    http.Handler
	logs *bytes.Buffer
}

type reqOpt func(*http.Request)

func withCookie(c *http.Cookie) reqOpt { return func(r *http.Request) { r.AddCookie(c) } }
func withHeader(k, v string) reqOpt    { return func(r *http.Request) { r.Header.Set(k, v) } }

func fromAddr(addr string) reqOpt { return func(r *http.Request) { r.RemoteAddr = addr } }

func newEnv(t *testing.T, oidc, secure bool) *env {
	t.Helper()
	e := &env{t: t, f: newFixture(), logs: &bytes.Buffer{}}
	e.rebuild(oidc, secure)
	return e
}

// rebuild (re)creates the handler from the current f.Deps, so a test can swap a port first.
func (e *env) rebuild(oidc, secure bool) {
	logger := slog.New(slog.NewJSONHandler(e.logs, nil))
	d := e.f.Deps
	routes, prefixes := idhttp.Routes(identity.New(d), d, idhttp.Options{CookieSecure: secure, OIDCConfigured: oidc, Logger: logger})
	router := httpx.NewRouter(routes, prefixes, httpx.NotFound())
	e.h = httpx.Chain(router, httpx.RequestID(logger), httpx.ClientIP(false, 1))
}

func (e *env) do(method, path, body string, opts ...reqOpt) *http.Response {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Origin", origin)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w.Result()
}

func bodyOf(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func cookieNamed(r *http.Response, name string) *http.Cookie {
	for _, c := range r.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func schemaPath(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "compat", "schemas", name+".json")
}

// valid checks a JSON document against compat/schemas/<name>.json. The schemas use only type,
// properties, required, additionalProperties, enum and items, which is all this checks (the harness
// of the compat suite uses a full validator, but a vendor library is not allowed in this layer).
func valid(t *testing.T, name, doc string) {
	t.Helper()
	raw, err := os.ReadFile(schemaPath(name)) //nolint:gosec // G304: the path is built from the test's own location
	if err != nil {
		t.Fatal(err)
	}
	var schema, value any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(doc), &value); err != nil {
		t.Fatalf("%s: not JSON: %q", name, doc)
	}
	if problem := check(schema.(map[string]any), value, "$"); problem != "" {
		t.Errorf("%s does not satisfy the schema: %s\n%s", name, problem, doc)
	}
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	}
	return "object"
}

var implemented = []string{"$schema", "type", "properties", "required", "additionalProperties", "enum", "items"}

// unknownKeyword names the first keyword of schema (or of a sub-schema) that check does not implement.
func unknownKeyword(schema map[string]any) string {
	for k, v := range schema {
		if !slices.Contains(implemented, k) {
			return "unimplemented keyword " + k
		}
		switch k {
		case "properties":
			for _, sub := range v.(map[string]any) {
				if p := unknownKeyword(sub.(map[string]any)); p != "" {
					return p
				}
			}
		case "items":
			if p := unknownKeyword(v.(map[string]any)); p != "" {
				return p
			}
		}
	}
	return ""
}

func check(schema map[string]any, v any, at string) string {
	if p := unknownKeyword(schema); p != "" {
		return at + ": " + p
	}
	if ty, ok := schema["type"]; ok {
		var types []string
		switch x := ty.(type) {
		case string:
			types = []string{x}
		case []any:
			for _, e := range x {
				types = append(types, e.(string))
			}
		}
		if !slices.Contains(types, jsonType(v)) {
			return at + ": type " + jsonType(v) + ", want " + strings.Join(types, "|")
		}
	}
	if enum, ok := schema["enum"].([]any); ok && !slices.Contains(enum, v) {
		return at + ": not in the enum"
	}
	if obj, ok := v.(map[string]any); ok {
		props, _ := schema["properties"].(map[string]any)
		if req, ok := schema["required"].([]any); ok {
			for _, k := range req {
				if _, present := obj[k.(string)]; !present {
					return at + ": missing " + k.(string)
				}
			}
		}
		for k, child := range obj {
			sub, known := props[k].(map[string]any)
			if !known {
				if schema["additionalProperties"] == false {
					return at + ": extra field " + k
				}
				continue
			}
			if p := check(sub, child, at+"."+k); p != "" {
				return p
			}
		}
	}
	if arr, ok := v.([]any); ok {
		if items, ok := schema["items"].(map[string]any); ok {
			for _, e := range arr {
				if p := check(items, e, at+"[]"); p != "" {
					return p
				}
			}
		}
	}
	return ""
}

func validList(t *testing.T, name, doc string) {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(doc), &items); err != nil || items == nil {
		t.Fatalf("%s: not a JSON array: %q", name, doc)
	}
	for _, it := range items {
		valid(t, name, string(it))
	}
}

func wantError(t *testing.T, r *http.Response, status int, code, message string) {
	t.Helper()
	b := bodyOf(t, r)
	if r.StatusCode != status {
		t.Fatalf("status = %d, want %d (body %s)", r.StatusCode, status, b)
	}
	want, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	if b != string(want) {
		t.Fatalf("body = %s, want %s", b, want)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	valid(t, "ErrorBody", b)
}

// signIn provisions a person, issues a link and redeems it over HTTP; it returns the session cookie.
func (e *env) signIn(sub string, admin bool) (*http.Cookie, domain.User) {
	e.t.Helper()
	ctx := context.Background()
	choice := app.RoleMember
	if admin {
		choice = app.RoleAdmin
	}
	if _, err := app.ProvisionUser(ctx, e.f.Deps, app.ProvisionInput{Sub: sub, Name: sub, Role: choice}); err != nil {
		e.t.Fatal(err)
	}
	link, err := app.IssueLoginLink(ctx, e.f.Deps, app.IssueLoginLinkInput{Sub: sub})
	if err != nil {
		e.t.Fatal(err)
	}
	r := e.do("POST", "/api/auth/link", `{"code":"`+link.Code+`"}`)
	if r.StatusCode != 204 {
		e.t.Fatalf("sign-in: %d %s", r.StatusCode, bodyOf(e.t, r))
	}
	c := cookieNamed(r, "mcp_session")
	if c == nil {
		e.t.Fatal("no session cookie")
	}
	u, _ := e.f.Deps.Users.FindBySub(ctx, sub)
	return reqCookie(c.Name, c.Value), *u
}

func (e *env) newLinkCode(sub string) string {
	e.t.Helper()
	if _, err := app.ProvisionUser(context.Background(), e.f.Deps, app.ProvisionInput{Sub: sub, Name: sub, Role: app.RoleKeep}); err != nil {
		e.t.Fatal(err)
	}
	l, err := app.IssueLoginLink(context.Background(), e.f.Deps, app.IssueLoginLinkInput{Sub: sub})
	if err != nil {
		e.t.Fatal(err)
	}
	return l.Code
}

func TestMethods(t *testing.T) {
	for _, oidc := range []bool{true, false} {
		e := newEnv(t, oidc, true)
		r := e.do("GET", "/api/auth/methods", "")
		want := `{"oidc":false}`
		if oidc {
			want = `{"oidc":true}`
		}
		if b := bodyOf(t, r); r.StatusCode != 200 || b != want {
			t.Errorf("oidc=%v: %d %q", oidc, r.StatusCode, b)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		valid(t, "AuthMethods", want)
	}
}

func TestLinkSuccessCookie(t *testing.T) {
	for _, secure := range []bool{true, false} {
		e := newEnv(t, false, secure)
		code := e.newLinkCode("ann")
		r := e.do("POST", "/api/auth/link", `{"code":"`+code+`","extra":1}`)
		if b := bodyOf(t, r); r.StatusCode != 204 || b != "" {
			t.Fatalf("%d %q", r.StatusCode, b)
		}
		c := cookieNamed(r, "mcp_session")
		if c == nil || c.Value == "" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" ||
			c.Secure != secure || c.MaxAge != 0 || !c.Expires.IsZero() {
			t.Fatalf("cookie = %+v", c)
		}
		if strings.Contains(strings.Join(r.Header.Values("Set-Cookie"), "\n"), code) {
			t.Error("Set-Cookie carries the code")
		}
		me := e.do("GET", "/api/auth/me", "", withCookie(reqCookie("mcp_session", c.Value)))
		if me.StatusCode != 200 {
			t.Errorf("me: %d", me.StatusCode)
		}
	}
}

func TestLinkEndsTheReplacedSession(t *testing.T) {
	e := newEnv(t, false, true)
	old, _ := e.signIn("ann", false)
	code := e.newLinkCode("bob")
	r := e.do("POST", "/api/auth/link", `{"code":"`+code+`"}`, withCookie(old))
	if r.StatusCode != 204 {
		t.Fatal(r.StatusCode)
	}
	if got := e.do("GET", "/api/auth/me", "", withCookie(old)).StatusCode; got != 401 {
		t.Errorf("old session: %d, want 401", got)
	}
	nc := cookieNamed(r, "mcp_session")
	if got := e.do("GET", "/api/auth/me", "", withCookie(nc)).StatusCode; got != 200 {
		t.Errorf("new session: %d", got)
	}
	if n := len(e.f.Audit.Actions("auth.logout")); n != 1 {
		t.Errorf("auth.logout records = %d, want 1", n)
	}
}

func TestLinkIgnoresAnOverlongSessionCookie(t *testing.T) {
	e := newEnv(t, false, true)
	code := e.newLinkCode("ann")
	r := e.do("POST", "/api/auth/link", `{"code":"`+code+`"}`, withCookie(reqCookie("mcp_session", strings.Repeat("a", 65))))
	if r.StatusCode != 204 || len(e.f.Audit.Actions("auth.logout")) != 0 {
		t.Fatalf("%d, logouts %d", r.StatusCode, len(e.f.Audit.Actions("auth.logout")))
	}
}

func TestLinkRefusals(t *testing.T) {
	t.Run("unknown and used codes are 401 and keep the session", func(t *testing.T) {
		e := newEnv(t, false, true)
		cookie, _ := e.signIn("ann", false)
		r := e.do("POST", "/api/auth/link", `{"code":"ll_bogus"}`, withCookie(cookie))
		wantError(t, r, 401, "invalid_link", "the link is invalid or has expired")
		if len(r.Header.Values("Set-Cookie")) != 0 {
			t.Error("Set-Cookie on a refusal")
		}
		if e.do("GET", "/api/auth/me", "", withCookie(cookie)).StatusCode != 200 {
			t.Error("session was ended by a rejected link")
		}
		code := e.newLinkCode("bob")
		e.do("POST", "/api/auth/link", `{"code":"`+code+`"}`)
		wantError(t, e.do("POST", "/api/auth/link", `{"code":"`+code+`"}`), 401, "invalid_link", "the link is invalid or has expired")
	})
	t.Run("a disabled person is 403", func(t *testing.T) {
		e := newEnv(t, false, true)
		code := e.newLinkCode("ann")
		for _, u := range e.f.Store.users {
			u.Disabled = true
		}
		r := e.do("POST", "/api/auth/link", `{"code":"`+code+`"}`)
		wantError(t, r, 403, "disabled", "account is disabled")
		if len(r.Header.Values("Set-Cookie")) != 0 {
			t.Error("Set-Cookie on a refusal")
		}
	})
	t.Run("a locked limiter is 429 with Retry-After", func(t *testing.T) {
		e := newEnv(t, false, true)
		e.f.LinkLimiter.Deny, e.f.LinkLimiter.RetryAfter = true, 42
		r := e.do("POST", "/api/auth/link", `{"code":"nope"}`)
		if r.Header.Get("Retry-After") != "42" {
			t.Errorf("Retry-After = %q", r.Header.Get("Retry-After"))
		}
		wantError(t, r, 429, "rate_limited", "too many failed attempts")
	})
	t.Run("a malformed body is 422 and touches neither limiter nor code", func(t *testing.T) {
		e := newEnv(t, false, true)
		cases := []struct{ body, msg string }{
			{`{}`, "code: is required"},
			{``, "code: is required"},
			{`{"code":""}`, "code: is required"},
			{`{"code":"` + strings.Repeat("x", 101) + `"}`, "code: must be at most 100 characters"},
			{`{"code":5}`, "code: invalid type"},
			{`{nope`, "body is not valid JSON"},
		}
		for _, c := range cases {
			wantError(t, e.do("POST", "/api/auth/link", c.body), 422, "invalid_request", c.msg)
		}
		if len(e.f.LinkLimiter.Hits) != 0 {
			t.Error("the limiter was hit")
		}
	})
	t.Run("an Authorization header is ignored", func(t *testing.T) {
		e := newEnv(t, false, true)
		code := e.newLinkCode("ann")
		if r := e.do("POST", "/api/auth/link", `{"code":"`+code+`"}`, withHeader("Authorization", "Bearer sk_x")); r.StatusCode != 204 {
			t.Errorf("%d", r.StatusCode)
		}
	})
}

// Link sign-in and the OIDC callback key their limiters by the client address that
// httpx.ClientIP resolved (here the socket peer, without its port), one entry per address.
func TestLimitersAreKeyedByTheClientAddress(t *testing.T) {
	peers := []string{"203.0.113.7:51000", "198.51.100.9:52000"}
	want := []string{"203.0.113.7", "198.51.100.9"}
	t.Run("link", func(t *testing.T) {
		e := newEnv(t, false, true)
		for _, p := range peers {
			e.do("POST", "/api/auth/link", `{"code":"ll_bogus"}`, fromAddr(p))
		}
		if !slices.Equal(e.f.LinkLimiter.Hits, want) {
			t.Errorf("link limiter keys = %q, want %q", e.f.LinkLimiter.Hits, want)
		}
	})
	t.Run("callback", func(t *testing.T) {
		e := newEnv(t, true, true)
		for _, p := range peers {
			state, bind := e.startLogin("/")
			e.do("GET", "/api/auth/callback?code=bad&state="+state, "", withCookie(bind), fromAddr(p))
		}
		if !slices.Equal(e.f.CallbackLimit.Hits, want) {
			t.Errorf("callback limiter keys = %q, want %q", e.f.CallbackLimit.Hits, want)
		}
	})
}

func TestLogin(t *testing.T) {
	t.Run("without OIDC", func(t *testing.T) {
		e := newEnv(t, false, true)
		r := e.do("GET", "/api/auth/login", "")
		if r.StatusCode != 302 || r.Header.Get("Location") != "/?loginError=oidc_not_configured" || len(r.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("%d %q %v", r.StatusCode, r.Header.Get("Location"), r.Header.Values("Set-Cookie"))
		}
	})
	t.Run("redirects to the provider and binds the browser", func(t *testing.T) {
		for _, secure := range []bool{true, false} {
			e := newEnv(t, true, secure)
			r := e.do("GET", "/api/auth/login?returnTo=/servers", "")
			loc, _ := url.Parse(r.Header.Get("Location"))
			if r.StatusCode != 302 || loc.Host != "id.example" {
				t.Fatalf("%d %q", r.StatusCode, r.Header.Get("Location"))
			}
			b := cookieNamed(r, "mcp_login")
			if b == nil || b.Value != loc.Query().Get("state") || b.Value == "" || !b.HttpOnly ||
				b.SameSite != http.SameSiteLaxMode || b.Path != "/api/auth" || b.MaxAge != 600 || b.Secure != secure {
				t.Fatalf("binding cookie = %+v", b)
			}
		}
	})
	t.Run("returnTo longer than 2048 is 422", func(t *testing.T) {
		e := newEnv(t, true, true)
		wantError(t, e.do("GET", "/api/auth/login?returnTo=/"+strings.Repeat("a", 2048), ""), 422, "invalid_request", "returnTo: must be at most 2048 characters")
	})
	t.Run("an unreachable provider redirects with idp_unavailable and sets no cookie", func(t *testing.T) {
		e := newEnv(t, true, true)
		e.f.OIDC.Unavailable = true
		r := e.do("GET", "/api/auth/login", "")
		if r.StatusCode != 302 || r.Header.Get("Location") != "/?loginError=idp_unavailable" || len(r.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("%d %q %v", r.StatusCode, r.Header.Get("Location"), r.Header.Values("Set-Cookie"))
		}
	})
	t.Run("any other failure is a JSON 500, not a redirect", func(t *testing.T) {
		e := newEnv(t, true, true)
		e.f.Random.Fail = errors.New("entropy gone: secret-detail")
		r := e.do("GET", "/api/auth/login", "")
		wantError(t, r, 500, "internal", "Internal error")
		if strings.Contains(e.logs.String(), "secret-detail") {
			t.Error("the error text reached the log")
		}
	})
}

// startLogin runs /login and returns the state and the binding cookie.
func (e *env) startLogin(returnTo string) (string, *http.Cookie) {
	e.t.Helper()
	r := e.do("GET", "/api/auth/login?returnTo="+url.QueryEscape(returnTo), "")
	loc, _ := url.Parse(r.Header.Get("Location"))
	b := cookieNamed(r, "mcp_login")
	if b == nil {
		e.t.Fatalf("login did not bind: %d", r.StatusCode)
	}
	return loc.Query().Get("state"), reqCookie(b.Name, b.Value)
}

type failingPending struct{ app.PendingLoginRepository }

func (failingPending) Consume(context.Context, string) (*app.PendingLogin, error) {
	return nil, errors.New("db-secret-detail")
}

func TestCallback(t *testing.T) {
	member := &domain.OIDCClaims{Sub: "s1", Name: "Ann", Groups: []string{"mcp-users"}}
	t.Run("success sets the session, clears the binding and redirects to returnTo", func(t *testing.T) {
		e := newEnv(t, true, true)
		e.f.OIDC.Next = member
		state, bind := e.startLogin("/servers")
		// A returnTo on the callback itself is not the stored one and must be ignored.
		r := e.do("GET", "/api/auth/callback?code=c1&returnTo=//evil.example&state="+state, "", withCookie(bind))
		if r.StatusCode != 302 || r.Header.Get("Location") != "/servers" || bodyOf(t, r) != "" {
			t.Fatalf("%d %q", r.StatusCode, r.Header.Get("Location"))
		}
		s := cookieNamed(r, "mcp_session")
		if s == nil || s.Value == "" || !s.HttpOnly || s.SameSite != http.SameSiteStrictMode || s.Path != "/" || !s.Secure || s.MaxAge != 0 {
			t.Fatalf("session cookie = %+v", s)
		}
		cl := cookieNamed(r, "mcp_login")
		if cl == nil || cl.Value != "" || cl.MaxAge >= 0 || !cl.Expires.IsZero() || cl.Path != "/api/auth" || !cl.HttpOnly || cl.SameSite != http.SameSiteLaxMode || !cl.Secure {
			t.Fatalf("clearing cookie = %+v", cl)
		}
		if e.do("GET", "/api/auth/me", "", withCookie(reqCookie("mcp_session", s.Value))).StatusCode != 200 {
			t.Error("session does not work")
		}
	})
	t.Run("a missing or wrong binding, or a bad query, is invalid_state and does not consume the state", func(t *testing.T) {
		e := newEnv(t, true, true)
		e.f.OIDC.Next = member
		state, bind := e.startLogin("/")
		bad := []struct {
			name, query string
			c           *http.Cookie
		}{
			{"no cookie", "code=c1&state=" + state, nil},
			{"other cookie", "code=c1&state=" + state, reqCookie("mcp_login", "other")},
			{"no code (provider error)", "error=access_denied&state=" + state, bind},
			{"no state", "code=c1", bind},
			{"state too long", "code=c1&state=" + strings.Repeat("a", 257), reqCookie("mcp_login", strings.Repeat("a", 257))},
			{"code too long", "code=" + strings.Repeat("a", 4097) + "&state=" + state, bind},
			{"iss too long", "code=c1&iss=" + strings.Repeat("a", 2049) + "&state=" + state, bind},
		}
		for _, c := range bad {
			var opts []reqOpt
			if c.c != nil {
				opts = append(opts, withCookie(c.c))
			}
			r := e.do("GET", "/api/auth/callback?"+c.query, "", opts...)
			if r.StatusCode != 302 || r.Header.Get("Location") != "/?loginError=invalid_state" || cookieNamed(r, "mcp_session") != nil {
				t.Errorf("%s: %d %q", c.name, r.StatusCode, r.Header.Get("Location"))
			}
			if cl := cookieNamed(r, "mcp_login"); cl == nil || cl.MaxAge >= 0 {
				t.Errorf("%s: binding not cleared", c.name)
			}
		}
		if e.f.OIDC.LastExchange != nil {
			t.Error("the use case ran for a refused callback")
		}
		if r := e.do("GET", "/api/auth/callback?code=c1&state="+state, "", withCookie(bind)); r.Header.Get("Location") != "/" || cookieNamed(r, "mcp_session") == nil {
			t.Errorf("the genuine callback: %d %q", r.StatusCode, r.Header.Get("Location"))
		}
	})
	t.Run("the iss parameter reaches the exchange", func(t *testing.T) {
		e := newEnv(t, true, true)
		e.f.OIDC.Next = member
		state, bind := e.startLogin("/")
		e.do("GET", "/api/auth/callback?code=c1&iss=https%3A%2F%2Fid.example&state="+state, "", withCookie(bind))
		if got := e.f.OIDC.LastExchange; got == nil || got.Iss == nil || *got.Iss != "https://id.example" {
			t.Errorf("exchange input = %+v", got)
		}
	})
	t.Run("a domain refusal redirects with its code", func(t *testing.T) {
		e := newEnv(t, true, true)
		e.f.OIDC.Next = &domain.OIDCClaims{Sub: "s2", Name: "Out", Groups: []string{"nobody"}}
		state, bind := e.startLogin("/")
		r := e.do("GET", "/api/auth/callback?code=c1&state="+state, "", withCookie(bind))
		if r.StatusCode != 302 || r.Header.Get("Location") != "/?loginError=not_allowed" || cookieNamed(r, "mcp_session") != nil {
			t.Errorf("%d %q", r.StatusCode, r.Header.Get("Location"))
		}
		e.f.OIDC.Next = nil // the exchange fails
		state, bind = e.startLogin("/")
		r = e.do("GET", "/api/auth/callback?code=c1&state="+state, "", withCookie(bind))
		if r.Header.Get("Location") != "/?loginError=invalid_state" {
			t.Errorf("failed exchange: %q", r.Header.Get("Location"))
		}
	})
	t.Run("an unexpected error is logged without its text and redirects with internal", func(t *testing.T) {
		e := newEnv(t, true, true)
		e.f.Deps.PendingLogins = failingPending{fakePending{e.f.Store}}
		e.rebuild(true, true)
		r := e.do("GET", "/api/auth/callback?code=c1&state=s", "", withCookie(reqCookie("mcp_login", "s")))
		if r.StatusCode != 302 || r.Header.Get("Location") != "/?loginError=internal" || cookieNamed(r, "mcp_session") != nil {
			t.Fatalf("%d %q", r.StatusCode, r.Header.Get("Location"))
		}
		logs := e.logs.String()
		if !strings.Contains(logs, "auth: login callback failed") || !strings.Contains(logs, `"level":"ERROR"`) || strings.Contains(logs, "db-secret-detail") {
			t.Errorf("log = %s", logs)
		}
	})
}

func TestLogout(t *testing.T) {
	t.Run("without a cookie it is 204 and clears the cookie", func(t *testing.T) {
		e := newEnv(t, false, true)
		r := e.do("POST", "/api/auth/logout", "")
		c := cookieNamed(r, "mcp_session")
		if b := bodyOf(t, r); r.StatusCode != 204 || b != "" || c == nil || c.Value != "" || c.MaxAge >= 0 || !c.Expires.IsZero() || c.Path != "/" ||
			!c.HttpOnly || c.SameSite != http.SameSiteStrictMode || !c.Secure {
			t.Fatalf("%d %q %+v", r.StatusCode, b, c)
		}
		if len(e.f.Audit.Actions("auth.logout")) != 0 {
			t.Error("a no-op logout was audited")
		}
	})
	t.Run("with a session it ends it", func(t *testing.T) {
		e := newEnv(t, false, true)
		cookie, _ := e.signIn("ann", false)
		if r := e.do("POST", "/api/auth/logout", "", withCookie(cookie)); r.StatusCode != 204 {
			t.Fatal(r.StatusCode)
		}
		if e.do("GET", "/api/auth/me", "", withCookie(cookie)).StatusCode != 401 {
			t.Error("session survived")
		}
		if len(e.f.Audit.Actions("auth.logout")) != 1 {
			t.Error("logout not audited once")
		}
	})
	t.Run("an overlong cookie is not looked up", func(t *testing.T) {
		e := newEnv(t, false, true)
		sessions := len(e.f.Store.sessions)
		r := e.do("POST", "/api/auth/logout", "", withCookie(reqCookie("mcp_session", strings.Repeat("a", 65))))
		if r.StatusCode != 204 || len(e.f.Audit.Entries) != 0 || len(e.f.Store.sessions) != sessions {
			t.Errorf("%d", r.StatusCode)
		}
	})
}

func TestMe(t *testing.T) {
	e := newEnv(t, false, true)
	wantError(t, e.do("GET", "/api/auth/me", ""), 401, "unauthenticated", "sign in required")
	wantError(t, e.do("GET", "/api/auth/me", "", withCookie(reqCookie("mcp_session", "nope"))), 401, "unauthenticated", "sign in required")
	cookie, u := e.signIn("ann", true)
	wantError(t, e.do("GET", "/api/auth/me", "", withCookie(cookie), withHeader("Authorization", "Bearer sk_whatever")), 401, "unauthenticated", "tokens do not work on the web API")
	r := e.do("GET", "/api/auth/me", "", withCookie(cookie))
	b := bodyOf(t, r)
	want := `{"userId":"` + u.ID + `","name":"ann","role":"admin","groups":["mcp-admins"]}`
	if r.StatusCode != 200 || b != want {
		t.Fatalf("%d %s, want %s", r.StatusCode, b, want)
	}
	valid(t, "MeResponse", b)
}

func TestNoGuardUnderAuth(t *testing.T) {
	e := newEnv(t, false, true)
	wantError(t, e.do("GET", "/api/auth/zzz", ""), 404, "not_found", "not found")
}

var isoMillis = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)

func TestTokens(t *testing.T) {
	e := newEnv(t, false, true)
	for _, p := range [][2]string{{"GET", "/api/tokens"}, {"POST", "/api/tokens"}, {"GET", "/api/tokens/anything/else"}, {"DELETE", "/api/tokens/x"}} {
		wantError(t, e.do(p[0], p[1], ""), 401, "unauthenticated", "sign in required")
	}
	ann, _ := e.signIn("ann", false)
	bob, _ := e.signIn("bob", false)
	wantError(t, e.do("GET", "/api/tokens", "", withCookie(ann), withHeader("Authorization", "Bearer sk_x")), 401, "unauthenticated", "tokens do not work on the web API")

	if r := e.do("GET", "/api/tokens", "", withCookie(ann)); bodyOf(t, r) != "[]" {
		t.Error("an empty list is []")
	}
	r := e.do("POST", "/api/tokens", `{"name":"  my laptop  ","x":1}`, withCookie(ann))
	b := bodyOf(t, r)
	if r.StatusCode != 201 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	valid(t, "CreateTokenResponse", b)
	var full map[string]string
	_ = json.Unmarshal([]byte(b), &full)
	if !strings.HasPrefix(full["token"], "sk_") || len(full["prefix"]) != 10 {
		t.Errorf("created = %s", b)
	}
	if got := e.f.Store.tokens[0].Name; got != "my laptop" {
		t.Errorf("stored name = %q", got)
	}

	list := bodyOf(t, e.do("GET", "/api/tokens", "", withCookie(ann)))
	validList(t, "TokenSummary", list)
	var rows []map[string]any
	_ = json.Unmarshal([]byte(list), &rows)
	if len(rows) != 1 || rows[0]["lastUsedAt"] != nil || rows[0]["revokedAt"] != nil || !isoMillis.MatchString(rows[0]["createdAt"].(string)) {
		t.Errorf("list = %s", list)
	}
	if strings.Contains(list, full["token"]) {
		t.Error("the list shows the token")
	}
	if got := bodyOf(t, e.do("GET", "/api/tokens", "", withCookie(bob))); got != "[]" {
		t.Errorf("another person's list = %s", got)
	}

	// 100 characters pass, 101 do not, blanks are empty.
	if r := e.do("POST", "/api/tokens", `{"name":"`+strings.Repeat("n", 100)+`"}`, withCookie(ann)); r.StatusCode != 201 {
		t.Errorf("100 chars: %d", r.StatusCode)
	}
	for body, msg := range map[string]string{
		`{"name":"` + strings.Repeat("n", 101) + `"}`: "name: must be at most 100 characters",
		`{"name":"   "}`: "name: is required",
		`{}`:             "name: is required",
		`{"name":7}`:     "name: invalid type",
	} {
		wantError(t, e.do("POST", "/api/tokens", body, withCookie(ann)), 422, "invalid_request", msg)
	}

	id := full["id"]
	wantError(t, e.do("DELETE", "/api/tokens/not-a-uuid", "", withCookie(ann)), 404, "not_found", "token not found")
	wantError(t, e.do("DELETE", "/api/tokens/00000000-0000-4000-8000-999999999999", "", withCookie(ann)), 404, "not_found", "token not found")
	wantError(t, e.do("DELETE", "/api/tokens/"+id, "", withCookie(bob)), 403, "forbidden", "not your token")
	for range 2 {
		if r := e.do("DELETE", "/api/tokens/"+id, "", withCookie(ann)); r.StatusCode != 204 || bodyOf(t, r) != "" {
			t.Errorf("revoke: %d", r.StatusCode)
		}
	}
	if n := len(e.f.Audit.Actions("token.revoke")); n != 1 {
		t.Errorf("revocations audited %d times", n)
	}
	var after []map[string]any
	_ = json.Unmarshal([]byte(bodyOf(t, e.do("GET", "/api/tokens", "", withCookie(ann)))), &after)
	revoked := 0
	for _, row := range after {
		if s, ok := row["revokedAt"].(string); ok && isoMillis.MatchString(s) {
			revoked++
		}
	}
	if revoked != 1 {
		t.Errorf("revoked rows = %d", revoked)
	}
}

func TestUsers(t *testing.T) {
	e := newEnv(t, false, true)
	wantError(t, e.do("GET", "/api/users/x/y", ""), 401, "unauthenticated", "sign in required")
	member, memberUser := e.signIn("mia", false)
	admin, adminUser := e.signIn("root", true)

	for _, p := range [][3]string{{"GET", "/api/users", ""}, {"GET", "/api/users/anything", ""}, {"POST", "/api/users/not-a-uuid/disabled", "this is not json"}, {"POST", "/api/users/x/y/z", ""}} {
		wantError(t, e.do(p[0], p[1], p[2], withCookie(member)), 403, "forbidden", "admin only")
	}
	wantError(t, e.do("GET", "/api/users", "", withCookie(admin), withHeader("Authorization", "Bearer sk_x")), 401, "unauthenticated", "tokens do not work on the web API")
	wantError(t, e.do("GET", "/api/users/x/y", "", withCookie(admin)), 404, "not_found", "not found")

	r := e.do("GET", "/api/users", "", withCookie(admin))
	b := bodyOf(t, r)
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	validList(t, "UserSummary", b)
	var rows []map[string]any
	_ = json.Unmarshal([]byte(b), &rows)
	if len(rows) != 2 || rows[0]["email"] != nil || !isoMillis.MatchString(rows[0]["createdAt"].(string)) || !isoMillis.MatchString(rows[0]["lastLoginAt"].(string)) {
		t.Errorf("list = %s", b)
	}

	wantError(t, e.do("POST", "/api/users/not-a-uuid/disabled", `{"disabled":true}`, withCookie(admin)), 404, "not_found", "user not found")
	wantError(t, e.do("POST", "/api/users/not-a-uuid/disabled", `garbage`, withCookie(admin)), 404, "not_found", "user not found")
	wantError(t, e.do("POST", "/api/users/"+memberUser.ID+"/disabled", `{}`, withCookie(admin)), 422, "invalid_request", "disabled: is required")
	wantError(t, e.do("POST", "/api/users/"+memberUser.ID+"/disabled", `{"disabled":"yes"}`, withCookie(admin)), 422, "invalid_request", "disabled: invalid type")
	wantError(t, e.do("POST", "/api/users/"+adminUser.ID+"/disabled", `{"disabled":true}`, withCookie(admin)), 403, "forbidden", "cannot disable yourself")
	wantError(t, e.do("POST", "/api/users/00000000-0000-4000-8000-999999999999/disabled", `{"disabled":true}`, withCookie(admin)), 404, "not_found", "user not found")

	if r := e.do("POST", "/api/users/"+memberUser.ID+"/disabled", `{"disabled":true}`, withCookie(admin)); r.StatusCode != 204 || bodyOf(t, r) != "" {
		t.Fatalf("disable: %d", r.StatusCode)
	}
	if e.do("GET", "/api/auth/me", "", withCookie(member)).StatusCode != 401 {
		t.Error("the disabled person's session survived")
	}
}

// longID is a session id one character over the limit.
var longID = strings.Repeat("a", 65)

// seedSession stores a live session for a person under the id the cookie will carry.
func (e *env) seedSession(id string, u domain.User) {
	e.t.Helper()
	now := e.f.Clock.Now()
	e.f.Store.sessions[e.f.Deps.Hasher.Hex(id)] = &app.SessionRecord{
		IDHash: e.f.Deps.Hasher.Hex(id), UserID: u.ID, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}
}

// countingSessions counts the lookups the guards and use cases make.
type countingSessions struct {
	app.SessionRepository
	finds *int
}

func (c countingSessions) FindByHash(ctx context.Context, h string) (*app.SessionRecord, error) {
	*c.finds++
	return c.SessionRepository.FindByHash(ctx, h)
}

func TestSessionIDLengthLimit(t *testing.T) {
	setup := func(t *testing.T) (*env, domain.User, *int) {
		e := newEnv(t, false, true)
		_, u := e.signIn("ann", false)
		finds := 0
		e.f.Deps.Sessions = countingSessions{fakeSessions{e.f.Store}, &finds}
		e.rebuild(false, true)
		return e, u, &finds
	}
	t.Run("a live 65-character id is anonymous and never looked up", func(t *testing.T) {
		e, u, finds := setup(t)
		e.seedSession(longID, u)
		wantError(t, e.do("GET", "/api/auth/me", "", withCookie(reqCookie("mcp_session", longID))), 401, "unauthenticated", "sign in required")
		if *finds != 0 {
			t.Errorf("the store was asked %d times", *finds)
		}
	})
	t.Run("a live 64-character id is accepted", func(t *testing.T) {
		e, u, _ := setup(t)
		id := strings.Repeat("b", 64)
		e.seedSession(id, u)
		if got := e.do("GET", "/api/auth/me", "", withCookie(reqCookie("mcp_session", id))).StatusCode; got != 200 {
			t.Errorf("64 characters: %d", got)
		}
	})
	t.Run("link sign-in and logout leave a live 65-character session alone", func(t *testing.T) {
		e, u, _ := setup(t)
		e.seedSession(longID, u)
		code := e.newLinkCode("bob")
		if r := e.do("POST", "/api/auth/link", `{"code":"`+code+`"}`, withCookie(reqCookie("mcp_session", longID))); r.StatusCode != 204 {
			t.Fatalf("link: %d", r.StatusCode)
		}
		if r := e.do("POST", "/api/auth/logout", "", withCookie(reqCookie("mcp_session", longID))); r.StatusCode != 204 {
			t.Fatalf("logout: %d", r.StatusCode)
		}
		if e.f.Store.sessions[e.f.Deps.Hasher.Hex(longID)] == nil {
			t.Error("the 65-character session was deleted")
		}
		if n := len(e.f.Audit.Actions("auth.logout")); n != 0 {
			t.Errorf("auth.logout records = %d", n)
		}
	})
}

// failingDelete fails every session delete.
type failingDelete struct{ app.SessionRepository }

func (failingDelete) Delete(context.Context, string) error { return errors.New("db-secret-detail") }

func TestLinkEndsTheOldSessionBeforeTheNewCookie(t *testing.T) {
	e := newEnv(t, false, true)
	old, _ := e.signIn("ann", false)
	code := e.newLinkCode("bob")
	e.f.Deps.Sessions = failingDelete{fakeSessions{e.f.Store}}
	e.rebuild(false, true)
	r := e.do("POST", "/api/auth/link", `{"code":"`+code+`"}`, withCookie(old))
	wantError(t, r, 500, "internal", "Internal error")
	if len(r.Header.Values("Set-Cookie")) != 0 {
		t.Errorf("Set-Cookie after a failed logout: %v", r.Header.Values("Set-Cookie"))
	}
	if strings.Contains(e.logs.String(), "db-secret-detail") {
		t.Error("the error text reached the log")
	}
}

func TestLogoutFailureIsNotIgnored(t *testing.T) {
	e := newEnv(t, false, true)
	c, _ := e.signIn("ann", false)
	e.f.Deps.Sessions = failingDelete{fakeSessions{e.f.Store}}
	e.rebuild(false, true)
	r := e.do("POST", "/api/auth/logout", "", withCookie(c))
	wantError(t, r, 500, "internal", "Internal error")
	if len(r.Header.Values("Set-Cookie")) != 0 {
		t.Errorf("the cookie was cleared although the session lives on: %v", r.Header.Values("Set-Cookie"))
	}
}

func TestRedirectEncodesLikeEncodeURI(t *testing.T) {
	member := &domain.OIDCClaims{Sub: "s1", Name: "Ann", Groups: []string{"mcp-users"}}
	for returnTo, want := range map[string]string{
		"/сервер":                 "/%D1%81%D0%B5%D1%80%D0%B2%D0%B5%D1%80",
		"/a b/сервер?x=1&y=2#f":   "/a%20b/%D1%81%D0%B5%D1%80%D0%B2%D0%B5%D1%80?x=1&y=2#f",
		"/path;a,b:c@d$e+f!g~'()": "/path;a,b:c@d$e+f!g~'()",
		"/caf\u00e9":              "/caf\u00e9", // Latin-1 only: left as it is (accepted difference)
	} {
		e := newEnv(t, true, true)
		e.f.OIDC.Next = member
		state, bind := e.startLogin(returnTo)
		r := e.do("GET", "/api/auth/callback?code=c1&state="+state, "", withCookie(bind))
		if got := r.Header.Get("Location"); r.StatusCode != 302 || got != want {
			t.Errorf("returnTo %q: %d Location %q, want %q", returnTo, r.StatusCode, got, want)
		}
	}
}

// keywordsChecked are the JSON Schema keywords the checker below implements.
func TestSchemaCheckerRefusesUnknownKeywords(t *testing.T) {
	schema := map[string]any{"type": "string", "minLength": float64(3)}
	if p := check(schema, "ab", "$"); !strings.Contains(p, "minLength") {
		t.Errorf("an unimplemented keyword was skipped silently: %q", p)
	}
	nested := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string", "pattern": "x"}}}
	if p := check(nested, map[string]any{"a": "y"}, "$"); !strings.Contains(p, "pattern") {
		t.Errorf("a nested unimplemented keyword was skipped: %q", p)
	}
	// Every schema the tests use passes through the checker without an unknown keyword.
	for _, name := range []string{"AuthMethods", "MeResponse", "TokenSummary", "CreateTokenResponse", "UserSummary", "ErrorBody"} {
		raw, err := os.ReadFile(schemaPath(name)) //nolint:gosec // G304: the path is built from the test's own location
		if err != nil {
			t.Fatal(err)
		}
		var sch map[string]any
		if err := json.Unmarshal(raw, &sch); err != nil {
			t.Fatal(err)
		}
		if p := unknownKeyword(sch); p != "" {
			t.Errorf("%s: %s", name, p)
		}
	}
}
