//go:build compat

package compat_test

// Identity scenarios: OIDC sign-in, one-time sign-in links, sessions, API tokens and people.
// Everything goes through HTTP, the CLI, MCP, SQL and the app's log; the OIDC provider is a fake in
// the test process.
//
// Source cases that are not ported (they need the app's internals, not its outside):
//   - oidc.int.test.ts "PgSessionRepository.deleteDead removes idle and expired sessions only":
//     calls the repository with chosen cutoffs. The same housekeeping is ported through the
//     scheduler's run at start (TestIdentityNoOIDC, "a restart prunes ...").
//   - login-link.int.test.ts "consume returns the record once" and "deleteExpired removes only
//     links at or past expires_at": repository calls; once-only is ported through the HTTP flow
//     ("a used link"), and pruning through the restart case. The exact "at expires_at" boundary
//     cannot be hit from outside (the clock is the database's now()).
//   - oidc.int.test.ts "an sk_ token works on /mcp but never on /api/**": the refusal on /api/**
//     for six endpoints is TestPlatformBearerNeverOpensTheWebAPI; here the token works on /mcp
//     (MCP client), and is refused after revocation and after its owner is disabled.
//   - oidc.int.test.ts "an identity provider that is down at startup ... the server works": ported
//     on a stack whose provider is down from the start (TestIdentityOIDCProviderDownAtStart).
//   - auth-link.test.ts "answers 429 with Retry-After when the limiter refuses" injects a limiter;
//     it is ported end to end by really exhausting the limit (11 failures, TestIdentityNoOIDC).
//   - auth-link.test.ts: the `Secure` flag of the session cookie is checked on the stack with
//     COOKIE_SECURE=true (TestIdentityOIDCProviderDownAtStart), not on the link flow itself.
//
// The brief and the shared context name the stored token table "api_tokens"; the image's table is
// identity.tokens (hash char(64)), which is what the scenarios query.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

var (
	sessionCookieValue = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	linkCodePattern    = regexp.MustCompile(`^ll_[A-Za-z0-9_-]{43}$`)
	tokenPattern       = regexp.MustCompile(`^sk_[A-Za-z0-9_-]{43}$`)
)

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// countSQL runs a "select count(*)" style query.
func countSQL(t *testing.T, s *harness.Stack, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func execSQL(t *testing.T, s *harness.Stack, query string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(t.Context(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// assertSessionCookie checks the flags of the session cookie a sign-in sets, and that the value is
// not the code that was spent (when given).
func assertSessionCookie(t *testing.T, r harness.Response, secure bool, spent string) *http.Cookie {
	t.Helper()
	c := cookieNamed(r, "mcp_session")
	if c == nil {
		t.Fatalf("no mcp_session cookie (%d Set-Cookie headers)", len(r.Header.Values("Set-Cookie")))
	}
	if !sessionCookieValue.MatchString(c.Value) || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode ||
		c.Path != "/" || c.Secure != secure || c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Errorf("session cookie %q: value shape %v httpOnly %v sameSite %v path %q secure %v (want %v) maxAge %d expires %v",
			c.Name, sessionCookieValue.MatchString(c.Value), c.HttpOnly, c.SameSite, c.Path, c.Secure, secure, c.MaxAge, c.Expires)
	}
	if spent != "" && strings.Contains(strings.Join(r.Header.Values("Set-Cookie"), "\n"), spent) {
		t.Error("the Set-Cookie header carries the sign-in code")
	}
	return c
}

// loginPerson describes one signed-in person of the OIDC scenarios.
func loginPerson(sub, name string, groups ...string) fakes.Claims {
	return fakes.Claims{Sub: sub, Name: name, Email: strings.ToLower(name) + "@example.test", Groups: groups}
}

func TestIdentityOIDC(t *testing.T) {
	idp := fakes.StartIdP(t)
	s := harness.Start(t, harness.Options{OIDC: idp})
	admin := s.Admin(t)
	anon := s.Anonymous()
	var leaked []string // codes and session ids: none may reach the audit log

	userRow := func(t *testing.T, sub string) (groups []string, role string, n int) {
		t.Helper()
		n = countSQL(t, s, `select count(*) from identity.users where oidc_sub = $1`, sub)
		if n == 1 {
			if err := s.DB.QueryRow(t.Context(), `select groups, role from identity.users where oidc_sub = $1`, sub).Scan(&groups, &role); err != nil {
				t.Fatal(err)
			}
		}
		return groups, role, n
	}
	loginErr := func(t *testing.T, cb harness.Response, want string) {
		t.Helper()
		if cb.Status != 302 || cb.Header.Get("Location") != want || cookieNamed(cb, "mcp_session") != nil {
			t.Fatalf("callback: %d Location %q session cookie %v, want 302 %q without a session (body %.200q)",
				cb.Status, cb.Header.Get("Location"), cookieNamed(cb, "mcp_session") != nil, want, cb.Body)
		}
	}

	t.Run("methods say oidc is on", func(t *testing.T) {
		r := anon.Do(t, "GET", "/api/auth/methods", nil)
		var m struct {
			OIDC bool `json:"oidc"`
		}
		r.JSON(t, "AuthMethods", &m)
		if r.Status != 200 || !m.OIDC || string(r.Body) != `{"oidc":true}` {
			t.Fatalf("methods: %d %s", r.Status, r.Body)
		}
	})

	t.Run("a member signs in, lands on returnTo and /me shows the claims", func(t *testing.T) {
		sub := harness.Unique("member")
		a := beginIDPLogin(t, s, idp, loginPerson(sub, "Anna", "mcp-users"), "/servers")
		// The binding cookie of the attempt: Lax, HttpOnly, scoped to /api/auth, ten minutes.
		b := cookieNamed(a.Start, "mcp_login")
		if b == nil || !b.HttpOnly || b.SameSite != http.SameSiteLaxMode || b.Path != "/api/auth" || b.MaxAge != 600 || b.Secure {
			t.Fatalf("binding cookie: %s", cookieDesc(b))
		}
		cb := a.callback(t)
		if cb.Status != 302 || cb.Header.Get("Location") != "/servers" {
			t.Fatalf("callback: %d Location %q", cb.Status, cb.Header.Get("Location"))
		}
		leaked = append(leaked, a.Code, cookieNamed(cb, "mcp_session").Value)
		assertSessionCookie(t, cb, false, a.Code)
		// The binding is spent whatever the outcome: cleared with Max-Age=0 on the same path.
		if cl := cookieNamed(cb, "mcp_login"); cl == nil || cl.Value != "" || cl.MaxAge >= 0 || cl.Path != "/api/auth" {
			t.Fatalf("binding cookie after the callback: %s", cookieDesc(cl))
		}
		me := whoami(t, a.Client)
		if me.Name != "Anna" || me.Role != "member" || !slices.Equal(me.Groups, []string{"mcp-users"}) {
			t.Fatalf("me: %+v", me)
		}
		// The e-mail claim is stored (not exposed by /me).
		var users []struct {
			ID, Name string
			Email    *string
			Role     string
		}
		admin.Do(t, "GET", "/api/users", nil).JSON(t, "UserSummary[]", &users)
		found := false
		for _, u := range users {
			if u.ID == me.UserID {
				found = true
				if u.Email == nil || *u.Email != "anna@example.test" || u.Role != "member" {
					t.Fatalf("user row: %+v", u)
				}
			}
		}
		if !found {
			t.Fatalf("the signed-in person is not in /api/users: %+v", users)
		}
	})

	t.Run("the authorize request carries PKCE, state, nonce, scopes and the registered redirect_uri", func(t *testing.T) {
		before := len(idp.AuthorizeRequests())
		beginIDPLogin(t, s, idp, loginPerson(harness.Unique("p"), "Pat", "mcp-users"), "")
		reqs := idp.AuthorizeRequests()
		if len(reqs) != before+1 {
			t.Fatalf("authorize requests: %d, want %d", len(reqs), before+1)
		}
		q := reqs[len(reqs)-1]
		if q.Get("client_id") != idp.ClientID || q.Get("response_type") != "code" ||
			q.Get("scope") != "openid profile email groups" || q.Get("code_challenge_method") != "S256" ||
			q.Get("redirect_uri") != s.BaseURL+"/api/auth/callback" {
			t.Errorf("authorize query: %v", q)
		}
		for _, k := range []string{"state", "nonce", "code_challenge"} {
			if len(q.Get(k)) < 40 {
				t.Errorf("authorize %s = %q, want a long random value", k, q.Get(k))
			}
		}
	})

	t.Run("members of the admin group are admins; groups and role refresh on the next login", func(t *testing.T) {
		sub := harness.Unique("promoted")
		first := loginViaIDP(t, s, idp, loginPerson(sub, "Boris", "mcp-users"))
		if me := whoami(t, first); me.Role != "member" || !slices.Equal(me.Groups, []string{"mcp-users"}) {
			t.Fatalf("first login: %+v", me)
		}
		second := loginViaIDP(t, s, idp, loginPerson(sub, "Boris", "mcp-users", "mcp-admins"))
		me := whoami(t, second)
		if me.Role != "admin" || !slices.Equal(me.Groups, []string{"mcp-users", "mcp-admins"}) {
			t.Fatalf("second login: %+v", me)
		}
		if _, _, n := userRow(t, sub); n != 1 {
			t.Fatalf("%d people for one sub", n)
		}
		// The admin group alone is enough (no login group needed); the admin reaches admin routes.
		only := loginViaIDP(t, s, idp, loginPerson(harness.Unique("ops"), "Olga", "mcp-admins"))
		if me := whoami(t, only); me.Role != "admin" {
			t.Fatalf("admin group only: %+v", me)
		}
		if r := only.Do(t, "GET", "/api/users", nil); r.Status != 200 {
			t.Fatalf("admin GET /api/users: %d", r.Status)
		}
		// Demotion: the next login without the admin group is a member again.
		third := loginViaIDP(t, s, idp, loginPerson(sub, "Boris", "mcp-users"))
		if me := whoami(t, third); me.Role != "member" {
			t.Fatalf("after losing the admin group: %+v", me)
		}
		if r := third.Do(t, "GET", "/api/users", nil); r.Status != 403 {
			t.Fatalf("a member GET /api/users: %d", r.Status)
		}
	})

	t.Run("groups as a list or as an object, in the ID token or only at UserInfo", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			claims fakes.Claims
			role   string
			groups []string
		}{
			{"list in the ID token", fakes.Claims{Groups: []string{"mcp-users", "devs"}}, "member", []string{"mcp-users", "devs"}},
			// A JSON object is written with sorted keys, so its group order is alphabetical.
			{"object in the ID token (roles as keys)", fakes.Claims{Groups: []string{"mcp-users", "devs"}, GroupsShape: fakes.GroupsObject}, "member", []string{"devs", "mcp-users"}},
			{"list only at UserInfo", fakes.Claims{Groups: []string{"mcp-users", "mcp-admins"}, Where: fakes.InUserInfo}, "admin", []string{"mcp-users", "mcp-admins"}},
			{"object only at UserInfo", fakes.Claims{Groups: []string{"mcp-users", "devs"}, GroupsShape: fakes.GroupsObject, Where: fakes.InUserInfo}, "member", []string{"devs", "mcp-users"}},
		} {
			c.claims.Sub, c.claims.Name = harness.Unique("shape"), "Shape"
			me := whoami(t, loginViaIDP(t, s, idp, c.claims))
			if me.Role != c.role || !slices.Equal(me.Groups, c.groups) {
				t.Errorf("%s: me %+v, want role %s groups %v", c.name, me, c.role, c.groups)
			}
		}
	})

	t.Run("people outside the allowed groups are refused and nobody is created", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			claims fakes.Claims
		}{
			{"another group", fakes.Claims{Groups: []string{"family"}}},
			{"no groups claim at all (also at UserInfo)", fakes.Claims{}},
			{"an empty list", fakes.Claims{Groups: []string{}}},
			{"the groups only at UserInfo, in a foreign group", fakes.Claims{Groups: []string{"family"}, Where: fakes.InUserInfo}},
		} {
			c.claims.Sub, c.claims.Name = harness.Unique("outsider"), "Outsider"
			a := beginIDPLogin(t, s, idp, c.claims, "/servers")
			loginErr(t, a.callback(t), "/?loginError=not_allowed")
			if _, _, n := userRow(t, c.claims.Sub); n != 0 {
				t.Errorf("%s: the refused person was created", c.name)
			}
		}
		// Control: the same flow with an allowed group signs in.
		loginViaIDP(t, s, idp, loginPerson(harness.Unique("insider"), "Insider", "mcp-users"))
	})

	t.Run("UserInfo failing denies the login and logs a warning without tokens or claims", func(t *testing.T) {
		sub := harness.Unique("noinfo")
		claims := loginPerson(sub, "Nina", "mcp-users")
		claims.Where = fakes.InUserInfo
		idp.UserInfoDown(true)
		a := beginIDPLogin(t, s, idp, claims, "")
		cb := a.callback(t)
		idp.UserInfoDown(false)
		loginErr(t, cb, "/?loginError=not_allowed")
		if _, _, n := userRow(t, sub); n != 0 {
			t.Fatal("the person was created")
		}
		recs, _ := logRecords(t, s.Logs(t))
		var warns []map[string]any
		for _, r := range recs {
			if m, _ := r["msg"].(string); level(r) == pinoWarn && strings.Contains(m, "userinfo failed") {
				warns = append(warns, r)
			}
		}
		if len(warns) != 1 {
			t.Fatalf("%d warnings about UserInfo, want 1", len(warns))
		}
		line := mustJSON(t, warns[0])
		if !strings.Contains(line, sub) || strings.Contains(line, "access-") || strings.Contains(line, "nina@example.test") {
			t.Fatalf("the warning must name the sub but not the token or the e-mail: %s", line)
		}
		// Control: the same person signs in once UserInfo is back.
		if me := whoami(t, loginViaIDP(t, s, idp, claims)); me.Role != "member" {
			t.Fatalf("after UserInfo recovered: %+v", me)
		}
	})

	t.Run("returnTo is a relative path of this site only", func(t *testing.T) {
		for _, c := range []struct{ returnTo, want string }{
			{"/servers", "/servers"},
			{"/admin/servers/new?x=1#y", "/admin/servers/new?x=1"}, // the fragment never reaches the server
			{"https://evil.example/", "/"},
			{"//evil.example/x", "/"},
			{"/\\evil.example", "/"},
			{"servers", "/"},
			{"/ok\r\nSet-Cookie: x=1", "/"},
			{"/tab\there", "/"},
			{"", "/"},
		} {
			sub := harness.Unique("rt")
			a := beginIDPLogin(t, s, idp, loginPerson(sub, "Ray", "mcp-users"), strings.SplitN(c.returnTo, "#", 2)[0])
			cb := a.callback(t)
			if cb.Status != 302 || cb.Header.Get("Location") != c.want {
				t.Errorf("returnTo %q: %d Location %q, want %q", c.returnTo, cb.Status, cb.Header.Get("Location"), c.want)
			}
		}
	})

	t.Run("a state is accepted once and only in the browser that started it", func(t *testing.T) {
		a := beginIDPLogin(t, s, idp, loginPerson(harness.Unique("once"), "Ola", "mcp-users"), "/servers")
		first := a.callback(t)
		if first.Status != 302 || first.Header.Get("Location") != "/servers" {
			t.Fatalf("first callback: %d %q", first.Status, first.Header.Get("Location"))
		}
		// Replay with the very same binding cookie in a new browser: the state is spent.
		replay := s.Anonymous().Do(t, "GET", a.Back, nil, "Cookie: "+a.Binding)
		loginErr(t, replay, "/?loginError=invalid_state")

		// Login CSRF: a callback that did not start in this browser has no binding cookie.
		b := beginIDPLogin(t, s, idp, loginPerson(harness.Unique("csrf"), "Cal", "mcp-users"), "")
		loginErr(t, s.Anonymous().Do(t, "GET", b.Back, nil), "/?loginError=invalid_state")
		// The binding cookie of another attempt does not match this state either.
		other := beginIDPLogin(t, s, idp, loginPerson(harness.Unique("csrf"), "Cal", "mcp-users"), "")
		loginErr(t, s.Anonymous().Do(t, "GET", b.Back, nil, "Cookie: "+other.Binding), "/?loginError=invalid_state")
		// None of it spent the attempt or created anybody: the right browser still gets in.
		if cb := b.callback(t); cb.Status != 302 || cb.Header.Get("Location") != "/" || cookieNamed(cb, "mcp_session") == nil {
			t.Fatalf("the genuine callback after the refused ones: %d %q", cb.Status, cb.Header.Get("Location"))
		}
	})

	t.Run("a wrong or missing iss is refused", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			mutate func(url.Values)
		}{
			{"foreign iss", func(q url.Values) { q.Set("iss", "https://evil.example") }},
			{"no iss", func(q url.Values) { q.Del("iss") }},
		} {
			sub := harness.Unique("iss")
			a := beginIDPLogin(t, s, idp, loginPerson(sub, "Ivo", "mcp-users"), "")
			u, err := url.Parse(a.Back)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			c.mutate(q)
			u.RawQuery = q.Encode()
			cb := a.Client.Do(t, "GET", u.String(), nil)
			if cb.Status != 302 || cb.Header.Get("Location") != "/?loginError=invalid_state" || cookieNamed(cb, "mcp_session") != nil {
				t.Errorf("%s: %d %q", c.name, cb.Status, cb.Header.Get("Location"))
			}
			if _, _, n := userRow(t, sub); n != 0 {
				t.Errorf("%s: the person was created", c.name)
			}
		}
	})

	t.Run("an unknown state, a bare callback and a provider error are invalid_state", func(t *testing.T) {
		for _, path := range []string{
			"/api/auth/callback?code=c&state=nope",
			"/api/auth/callback",
			"/api/auth/callback?error=access_denied&state=nope",
		} {
			loginErr(t, anon.Do(t, "GET", path, nil), "/?loginError=invalid_state")
		}
	})

	t.Run("a rejected code is invalid_state and audited; a provider outage is idp_unavailable and is not", func(t *testing.T) {
		countFailed := func() int { return len(auditWhere(auditAll(t, admin), "auth.login_failed", nil)) }
		before := countFailed()

		// The provider answers the code exchange with an OAuth error (a code it never issued).
		a := beginIDPLogin(t, s, idp, loginPerson(harness.Unique("badcode"), "Bea", "mcp-users"), "")
		u, _ := url.Parse(a.Back)
		q := u.Query()
		q.Set("code", "forged-code")
		u.RawQuery = q.Encode()
		loginErr(t, a.Client.Do(t, "GET", u.String(), nil), "/?loginError=invalid_state")
		failed := auditWhere(auditAll(t, admin), "auth.login_failed", func(e auditEntry) bool { return e.Meta["reason"] == "exchange_failed" })
		if len(failed) == 0 || failed[0].ActorID != nil || failed[0].Target != nil || len(failed[0].Meta) != 1 {
			t.Fatalf("exchange_failed audit: %+v", failed)
		}
		if countFailed() != before+1 {
			t.Fatalf("auth.login_failed records: %d, want %d", countFailed(), before+1)
		}

		// The provider's token endpoint is down (a 5xx): not the person's fault, not audited.
		b := beginIDPLogin(t, s, idp, loginPerson(harness.Unique("outage"), "Ben", "mcp-users"), "")
		idp.Down(true)
		cb := b.callback(t)
		idp.Down(false)
		loginErr(t, cb, "/?loginError=idp_unavailable")
		if countFailed() != before+1 {
			t.Fatalf("an outage was audited as a failed login (%d records)", countFailed())
		}
		// The server itself is fine, and sign-in works again.
		if r := anon.Do(t, "GET", "/healthz", nil); r.Status != 200 {
			t.Fatalf("healthz during the outage: %d", r.Status)
		}
		loginViaIDP(t, s, idp, loginPerson(harness.Unique("back"), "Ann", "mcp-users"))
	})

	t.Run("an unexpected failure redirects with loginError=internal, not a JSON 500", func(t *testing.T) {
		execSQL(t, s, `create function public.compat_block() returns trigger language plpgsql as
			$$ begin raise exception 'blocked by the compat suite'; end $$`)
		execSQL(t, s, `create trigger compat_block before insert or update on identity.users
			for each row execute function public.compat_block()`)
		restored := false
		restore := func() {
			if !restored {
				restored = true
				execSQL(t, s, `drop trigger compat_block on identity.users`)
				execSQL(t, s, `drop function public.compat_block()`)
			}
		}
		defer restore()
		sub := harness.Unique("db")
		a := beginIDPLogin(t, s, idp, loginPerson(sub, "Dan", "mcp-users"), "")
		loginErr(t, a.callback(t), "/?loginError=internal")
		restore()
		if _, _, n := userRow(t, sub); n != 0 {
			t.Fatal("a person was created although the write failed")
		}
		var logged map[string]any
		recs, _ := logRecords(t, s.Logs(t))
		for _, r := range recordsWithMsg(recs, "auth: login callback failed") {
			logged = r
		}
		if logged == nil || level(logged) != 50 {
			t.Fatalf("no error record for the failed callback (%v)", logged)
		}
		if strings.Contains(mustJSON(t, logged), "blocked by the compat suite") {
			t.Fatalf("the error record quotes the failure message: %v", logged)
		}
		// Control: once the database accepts writes the same person signs in.
		loginViaIDP(t, s, idp, loginPerson(sub, "Dan", "mcp-users"))
	})

	t.Run("a disabled person is refused with loginError=disabled and the refusal is audited", func(t *testing.T) {
		sub := harness.Unique("off")
		claims := loginPerson(sub, "Olaf", "mcp-users")
		victim := loginViaIDP(t, s, idp, claims)
		id := whoami(t, victim).UserID
		if r := admin.Do(t, "POST", "/api/users/"+id+"/disabled", map[string]bool{"disabled": true}); r.Status != 204 {
			t.Fatalf("disable: %d %s", r.Status, r.Body)
		}
		if r := victim.Do(t, "GET", "/api/auth/me", nil); r.Status != 401 {
			t.Fatalf("the disabled person's session still works: %d", r.Status)
		}
		a := beginIDPLogin(t, s, idp, claims, "")
		loginErr(t, a.callback(t), "/?loginError=disabled")
		got := auditWhere(auditAll(t, admin), "auth.login_failed", func(e auditEntry) bool { return ptrIs(e.ActorID, id) })
		if len(got) != 1 || got[0].Meta["reason"] != "disabled" || len(got[0].Meta) != 1 {
			t.Fatalf("audit of the refused login: %+v", got)
		}
		// Control: enabling the person lets them in again; the old session stays dead.
		if r := admin.Do(t, "POST", "/api/users/"+id+"/disabled", map[string]bool{"disabled": false}); r.Status != 204 {
			t.Fatalf("enable: %d", r.Status)
		}
		if r := victim.Do(t, "GET", "/api/auth/me", nil); r.Status != 401 {
			t.Fatalf("the old session came back: %d", r.Status)
		}
		loginViaIDP(t, s, idp, claims)
	})

	t.Run("logout ends the session and is audited; without a session it is still 204", func(t *testing.T) {
		c := loginViaIDP(t, s, idp, loginPerson(harness.Unique("bye"), "Bye", "mcp-users"))
		id := whoami(t, c).UserID
		out := c.Do(t, "POST", "/api/auth/logout", nil)
		if out.Status != 204 || len(out.Body) != 0 {
			t.Fatalf("logout: %d %q", out.Status, out.Body)
		}
		if cl := cookieNamed(out, "mcp_session"); cl == nil || cl.Value != "" || cl.MaxAge >= 0 || cl.Path != "/" {
			t.Fatalf("logout does not clear the cookie: %s", cookieDesc(cl))
		}
		if r := c.Do(t, "GET", "/api/auth/me", nil); r.Status != 401 {
			t.Fatalf("me after logout: %d", r.Status)
		}
		if got := auditWhere(auditAll(t, admin), "auth.logout", func(e auditEntry) bool { return ptrIs(e.ActorID, id) }); len(got) != 1 || got[0].Target != nil {
			t.Fatalf("logout audit: %+v", got)
		}
		if r := anon.Do(t, "POST", "/api/auth/logout", nil); r.Status != 204 {
			t.Fatalf("logout without a session: %d", r.Status)
		}
	})

	t.Run("the audit log has the documented login records and no secret", func(t *testing.T) {
		entries := auditAll(t, admin)
		// OIDC logins carry the actor only; the admin of this test signed in with a link.
		oidcLogins := auditWhere(entries, "auth.login", func(e auditEntry) bool { return e.Meta["via"] == nil })
		if len(oidcLogins) == 0 {
			t.Fatal("no auth.login record of an OIDC login")
		}
		for _, e := range oidcLogins {
			if e.ActorID == nil || e.Target != nil || len(e.Meta) != 0 {
				t.Errorf("an OIDC login is recorded with the actor only: %+v", e)
			}
		}
		if links := auditWhere(entries, "auth.login", func(e auditEntry) bool { return e.Meta["via"] == "link" }); len(links) != 1 {
			t.Errorf("link logins: %+v", links)
		}
		notAllowed := auditWhere(entries, "auth.login_failed", func(e auditEntry) bool {
			return e.Meta["reason"] == "not_allowed" && strings.HasPrefix(mustString(e.Meta["sub"]), "outsider")
		})
		if len(notAllowed) != 4 {
			t.Fatalf("not_allowed audit of the outsiders: %+v", notAllowed)
		}
		for _, e := range notAllowed {
			if e.ActorID != nil || e.Target != nil || len(e.Meta) != 2 {
				t.Errorf("not_allowed record: %+v", e)
			}
		}
		all := mustJSON(t, entries)
		for _, secret := range leaked {
			if secret == "" || strings.Contains(all, secret) {
				t.Errorf("the audit log contains a code or a session id (%d chars)", len(secret))
			}
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustString(v any) string {
	s, _ := v.(string)
	return s
}

// firstLine is the first line of a CLI output.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// cliToken creates a person (if needed) and an API token for them through the CLI.
func cliToken(t *testing.T, s *harness.Stack, sub string) string {
	t.Helper()
	if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Cli "+sub); r.Code != 0 {
		t.Fatalf("create-user: %d %s", r.Code, r.Stderr)
	}
	r := s.CLI(t, "", "create-token", sub, "--name", "cli")
	token := firstLine(r.Stdout)
	if r.Code != 0 || !tokenPattern.MatchString(token) {
		t.Fatalf("create-token: %d stdout %q stderr %q", r.Code, harness.Redact(r.Stdout), r.Stderr)
	}
	return token
}

// TestIdentityOIDCProviderDownAtStart runs a stack whose provider is down from the start, with the
// provider-specific settings (group names, scopes, groups claim) and COOKIE_SECURE=true.
func TestIdentityOIDCProviderDownAtStart(t *testing.T) {
	const claim = "https://example.com/groups"
	idp := fakes.StartIdP(t)
	idp.Down(true)
	s := harness.Start(t, harness.Options{OIDC: idp, Env: map[string]string{
		"COOKIE_SECURE":     "true",
		"OIDC_LOGIN_GROUP":  "staff",
		"OIDC_ADMIN_GROUP":  "ops",
		"OIDC_SCOPES":       "openid profile email",
		"OIDC_GROUPS_CLAIM": claim,
	}})
	anon := s.Anonymous()

	t.Run("the server works without the provider: health, MCP with a token", func(t *testing.T) {
		if r := anon.Do(t, "GET", "/healthz", nil); r.Status != 200 {
			t.Fatalf("healthz: %d", r.Status)
		}
		token := cliToken(t, s, harness.Unique("ops"))
		tools, err := s.MCP(t, token).ListTools(t.Context(), nil)
		if err != nil || len(tools.Tools) != 4 {
			t.Fatalf("tools/list: %v %v", tools, err)
		}
		var m struct {
			OIDC bool `json:"oidc"`
		}
		anon.Do(t, "GET", "/api/auth/methods", nil).JSON(t, "AuthMethods", &m)
		if !m.OIDC {
			t.Fatal("methods.oidc is false although OIDC is configured")
		}
	})

	t.Run("login while the provider is down is idp_unavailable and sets no binding cookie", func(t *testing.T) {
		r := anon.Do(t, "GET", "/api/auth/login?returnTo=/", nil)
		if r.Status != 302 || r.Header.Get("Location") != "/?loginError=idp_unavailable" || len(r.Header.Values("Set-Cookie")) != 0 {
			t.Fatalf("login: %d Location %q, %d Set-Cookie headers", r.Status, r.Header.Get("Location"), len(r.Header.Values("Set-Cookie")))
		}
	})

	idp.Down(false)

	t.Run("once the provider is back, login works: binding cookie is Secure, scopes and claim are configured", func(t *testing.T) {
		a := beginIDPLogin(t, s, idp, fakes.Claims{Sub: harness.Unique("staff"), Name: "Sam", Groups: []string{"staff"}, GroupsClaim: claim}, "/servers")
		b := cookieNamed(a.Start, "mcp_login")
		if b == nil || !b.Secure || !b.HttpOnly || b.SameSite != http.SameSiteLaxMode || b.Path != "/api/auth" || b.MaxAge != 600 {
			t.Fatalf("binding cookie: %s", cookieDesc(b))
		}
		reqs := idp.AuthorizeRequests()
		if got := reqs[len(reqs)-1].Get("scope"); got != "openid profile email" {
			t.Fatalf("scope %q, want the configured OIDC_SCOPES", got)
		}
		// A Secure cookie is not sent back over http by the harness's jar: pass it by hand.
		cb := a.Client.Do(t, "GET", a.Back, nil, "Cookie: "+a.Binding)
		if cb.Status != 302 || cb.Header.Get("Location") != "/servers" {
			t.Fatalf("callback: %d %q", cb.Status, cb.Header.Get("Location"))
		}
		sess := assertSessionCookie(t, cb, true, a.Code)
		var me meBody
		a.Client.Do(t, "GET", "/api/auth/me", nil, "Cookie: mcp_session="+sess.Value).JSON(t, "MeResponse", &me)
		if me.Role != "member" || !slices.Equal(me.Groups, []string{"staff"}) {
			t.Fatalf("me: %+v", me)
		}
	})

	t.Run("OIDC_LOGIN_GROUP, OIDC_ADMIN_GROUP and OIDC_GROUPS_CLAIM decide who gets in", func(t *testing.T) {
		type result struct{ location, role string }
		run := func(c fakes.Claims) result {
			c.Sub, c.Name = harness.Unique("g"), "G"
			a := beginIDPLogin(t, s, idp, c, "")
			cb := a.Client.Do(t, "GET", a.Back, nil, "Cookie: "+a.Binding)
			res := result{location: cb.Header.Get("Location")}
			if sess := cookieNamed(cb, "mcp_session"); sess != nil {
				var me meBody
				a.Client.Do(t, "GET", "/api/auth/me", nil, "Cookie: mcp_session="+sess.Value).JSON(t, "MeResponse", &me)
				res.role = me.Role
			}
			return res
		}
		for _, c := range []struct {
			name   string
			claims fakes.Claims
			want   result
		}{
			{"the configured admin group", fakes.Claims{Groups: []string{"ops"}, GroupsClaim: claim}, result{"/", "admin"}},
			{"the configured claim only at UserInfo", fakes.Claims{Groups: []string{"staff"}, GroupsClaim: claim, Where: fakes.InUserInfo}, result{"/", "member"}},
			{"the default group name is no longer enough", fakes.Claims{Groups: []string{"mcp-users"}, GroupsClaim: claim}, result{"/?loginError=not_allowed", ""}},
			{"the default admin group name is no longer enough", fakes.Claims{Groups: []string{"mcp-admins"}, GroupsClaim: claim}, result{"/?loginError=not_allowed", ""}},
			{"the right group under the default claim name is ignored", fakes.Claims{Groups: []string{"staff"}}, result{"/?loginError=not_allowed", ""}},
		} {
			if got := run(c.claims); got != c.want {
				t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
			}
		}
	})

	t.Run("a link sign-in sets a Secure session cookie", func(t *testing.T) {
		sub := harness.Unique("link")
		s.CLI(t, "", "create-user", "--sub", sub, "--name", "Lin")
		r := s.CLI(t, "", "login-link", sub)
		_, code, _ := strings.Cut(strings.TrimSpace(r.Stdout), "#link=")
		resp := anon.Do(t, "POST", "/api/auth/link", map[string]string{"code": code})
		if resp.Status != 204 {
			t.Fatalf("link: %d %s", resp.Status, resp.Body)
		}
		assertSessionCookie(t, resp, true, code)
	})
}

var ipSeq atomic.Int32

// nextIP is a client address no other scenario uses; the stack trusts X-Forwarded-For, so each
// scenario gets its own failure budget.
func nextIP() string { return fmt.Sprintf("203.0.113.%d", ipSeq.Add(1)) }

func forwarded(ip string) string { return "X-Forwarded-For: " + ip }

// linkFor issues a sign-in link for an existing person through the CLI and returns the code. The
// link's exact form is asserted in its own scenario.
func linkFor(t *testing.T, s *harness.Stack, sub string) string {
	t.Helper()
	r := s.CLI(t, "", "login-link", sub)
	if r.Code != 0 {
		t.Fatalf("login-link %s: %d %s", sub, r.Code, r.Stderr)
	}
	_, code, ok := strings.Cut(strings.TrimSpace(r.Stdout), "#link=")
	if !ok {
		t.Fatalf("login-link %s: no code in %q", sub, harness.Redact(r.Stdout))
	}
	return code
}

// person creates a member and returns the sub.
func person(t *testing.T, s *harness.Stack, prefix string) string {
	t.Helper()
	sub := harness.Unique(prefix)
	if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "P "+sub); r.Code != 0 {
		t.Fatalf("create-user: %d %s", r.Code, r.Stderr)
	}
	return sub
}

func userID(t *testing.T, s *harness.Stack, sub string) string {
	t.Helper()
	var id string
	if err := s.DB.QueryRow(t.Context(), `select id::text from identity.users where oidc_sub = $1`, sub).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func postLink(t *testing.T, c *harness.Client, code string, hdr ...string) harness.Response {
	t.Helper()
	return c.Do(t, "POST", "/api/auth/link", map[string]string{"code": code}, hdr...)
}

// mcpInitialize sends a raw initialize request with a Bearer token.
func mcpInitialize(t *testing.T, c *harness.Client, token string, hdr ...string) harness.Response {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"compat","version":"1"}}}`
	hdr = append([]string{"Authorization: Bearer " + token, "Content-Type: application/json", "Accept: application/json, text/event-stream"}, hdr...)
	return c.Do(t, "POST", "/mcp", body, hdr...)
}

func randomLinkCode() string { return "ll_" + strings.Repeat("A", 11) + harness.Unique("")[1:] }

const (
	unauthenticatedSession = "unauthenticated|sign in required"
	invalidLink            = "invalid_link|the link is invalid or has expired"
)

// TestIdentityNoOIDC runs the scenarios of a server without OIDC on one stack that trusts
// X-Forwarded-For (so every scenario has its own client address and failure budget): links,
// sessions, tokens, people, the rate limits and the housekeeping at start. The rate-limit and
// restart scenarios come last.
func TestIdentityNoOIDC(t *testing.T) {
	s := harness.Start(t, harness.Options{Env: map[string]string{"TRUST_PROXY": "true"}})
	anon := s.Anonymous()
	admin := s.Admin(t)
	var leaked []string // link codes, tokens and session ids: none may reach the audit log or the app log

	t.Run("methods say oidc is off and login points back to the UI", func(t *testing.T) {
		r := anon.Do(t, "GET", "/api/auth/methods", nil)
		var m struct {
			OIDC bool `json:"oidc"`
		}
		r.JSON(t, "AuthMethods", &m)
		if r.Status != 200 || m.OIDC || string(r.Body) != `{"oidc":false}` {
			t.Fatalf("methods: %d %s", r.Status, r.Body)
		}
		l := anon.Do(t, "GET", "/api/auth/login?returnTo=/servers", nil)
		if l.Status != 302 || l.Header.Get("Location") != "/?loginError=oidc_not_configured" || len(l.Header.Values("Set-Cookie")) != 0 {
			t.Fatalf("login: %d Location %q, %d Set-Cookie headers", l.Status, l.Header.Get("Location"), len(l.Header.Values("Set-Cookie")))
		}
	})

	t.Run("the CLI link has the documented form, is stored only as its SHA-256 and lives 15 minutes", func(t *testing.T) {
		sub := person(t, s, "form")
		r := s.CLI(t, "", "login-link", sub)
		line := strings.TrimSuffix(r.Stdout, "\n")
		code, ok := strings.CutPrefix(line, s.BaseURL+"/login#link=")
		if r.Code != 0 || !ok || !linkCodePattern.MatchString(code) || r.Stderr != "" || strings.Count(r.Stdout, "\n") != 1 {
			t.Fatalf("login-link: code %d stdout %q stderr %q", r.Code, harness.Redact(r.Stdout), r.Stderr)
		}
		leaked = append(leaked, code)
		id := userID(t, s, sub)
		var hash string
		var ttl bool
		if err := s.DB.QueryRow(t.Context(), `select code_hash, expires_at - created_at = interval '15 minutes'
			from identity.login_links where user_id = $1::uuid`, id).Scan(&hash, &ttl); err != nil {
			t.Fatal(err)
		}
		if hash != sha256hex(code) || !ttl {
			t.Fatalf("stored hash matches sha256(code) %v, lifetime is 15 minutes %v", hash == sha256hex(code), ttl)
		}
		if n := countSQL(t, s, `select count(*) from identity.login_links l where position($1 in l::text) > 0`, code); n != 0 {
			t.Fatal("the raw code is stored")
		}
		// Control: a person that does not exist, a disabled one, and a missing argument.
		if r := s.CLI(t, "", "login-link", harness.Unique("nobody")); r.Code != 1 || !strings.HasPrefix(r.Stderr, "error:") {
			t.Errorf("unknown person: %d %q", r.Code, r.Stderr)
		}
		if r := s.CLI(t, "", "login-link"); r.Code != 2 {
			t.Errorf("no sub: %d %q", r.Code, r.Stderr)
		}
	})

	t.Run("a valid code signs in: 204, cookie flags, /me; last_login_at moves, role and groups stay", func(t *testing.T) {
		sub := person(t, s, "valid")
		id := userID(t, s, sub)
		var groupsBefore []string
		var roleBefore string
		var loginBefore time.Time
		q := `select groups, role, last_login_at from identity.users where id = $1::uuid`
		if err := s.DB.QueryRow(t.Context(), q, id).Scan(&groupsBefore, &roleBefore, &loginBefore); err != nil {
			t.Fatal(err)
		}
		code := linkFor(t, s, sub)
		c := s.Anonymous()
		r := postLink(t, c, code, forwarded(nextIP()))
		if r.Status != 204 || len(r.Body) != 0 {
			t.Fatalf("link: %d %q", r.Status, r.Body)
		}
		leaked = append(leaked, code, assertSessionCookie(t, r, false, code).Value)
		me := whoami(t, c)
		if me.UserID != id || me.Role != "member" || !slices.Equal(me.Groups, []string{"mcp-users"}) {
			t.Fatalf("me: %+v", me)
		}
		var groupsAfter []string
		var roleAfter string
		var loginAfter time.Time
		if err := s.DB.QueryRow(t.Context(), q, id).Scan(&groupsAfter, &roleAfter, &loginAfter); err != nil {
			t.Fatal(err)
		}
		if !loginAfter.After(loginBefore) || !slices.Equal(groupsAfter, groupsBefore) || roleAfter != roleBefore {
			t.Fatalf("last_login_at %v -> %v, groups %v -> %v, role %q -> %q", loginBefore, loginAfter, groupsBefore, groupsAfter, roleBefore, roleAfter)
		}
		if n := countSQL(t, s, `select count(*) from identity.login_links where user_id = $1::uuid`, id); n != 0 {
			t.Fatalf("the used link is still stored (%d rows)", n)
		}
		// A person made by the CLI with --admin is an admin and sees admin routes.
		if r := admin.Do(t, "GET", "/api/users", nil); r.Status != 200 {
			t.Fatalf("admin: %d", r.Status)
		}
	})

	t.Run("a used, an expired, an unknown and a malformed code are indistinguishable", func(t *testing.T) {
		sub := person(t, s, "indist")
		id := userID(t, s, sub)
		failedBefore := len(auditWhere(auditAll(t, admin), "auth.login_failed", nil))

		used := linkFor(t, s, sub)
		if r := postLink(t, s.Anonymous(), used, forwarded(nextIP())); r.Status != 204 {
			t.Fatalf("first use: %d", r.Status)
		}
		expired := linkFor(t, s, sub)
		execSQL(t, s, `update identity.login_links set expires_at = now() - interval '1 second' where user_id = $1::uuid`, id)
		leaked = append(leaked, used, expired)

		type probe struct {
			name, code string
		}
		probes := []probe{
			{"used", used},
			{"expired", expired},
			{"unknown", "ll_" + strings.Repeat("Z", 43)},
			{"no ll_ mark", strings.Repeat("a", 46)},
			{"100 characters, not a code", "ll_" + strings.Repeat("a", 97)},
		}
		var ref harness.Response
		for i, p := range probes {
			r := postLink(t, s.Anonymous(), p.code, forwarded(nextIP()))
			if r.Status != 401 || errorCode(t, r) != invalidLink || r.Header.Get("Retry-After") != "" || len(r.Header.Values("Set-Cookie")) != 0 {
				t.Errorf("%s: %d %s, %d Set-Cookie headers", p.name, r.Status, r.Body, len(r.Header.Values("Set-Cookie")))
			}
			if i == 0 {
				ref = r
			} else if string(r.Body) != string(ref.Body) || r.Status != ref.Status {
				t.Errorf("%s answers differently from the used code: %d %s vs %d %s", p.name, r.Status, r.Body, ref.Status, ref.Body)
			}
		}
		// Only a link that was consumed and then refused (the expired one) is audited, with the person.
		got := auditWhere(auditAll(t, admin), "auth.login_failed", nil)
		if len(got) != failedBefore+1 || got[0].Meta["reason"] != "invalid_link" || !ptrIs(got[0].ActorID, id) || got[0].Target != nil || len(got[0].Meta) != 1 {
			t.Fatalf("audit of the refused links: %d records (before %d), newest %+v", len(got), failedBefore, got[0])
		}
		if n := countSQL(t, s, `select count(*) from identity.login_links where user_id = $1::uuid`, id); n != 0 {
			t.Fatal("the expired link was not consumed")
		}
	})

	t.Run("expiry is the stored expires_at: just inside works, just past does not", func(t *testing.T) {
		sub := person(t, s, "expiry")
		id := userID(t, s, sub)
		inside := linkFor(t, s, sub)
		execSQL(t, s, `update identity.login_links set expires_at = now() + interval '1 minute' where user_id = $1::uuid`, id)
		if r := postLink(t, s.Anonymous(), inside, forwarded(nextIP())); r.Status != 204 {
			t.Fatalf("a link with a minute left: %d %s", r.Status, r.Body)
		}
		past := linkFor(t, s, sub)
		execSQL(t, s, `update identity.login_links set expires_at = now() - interval '1 second' where user_id = $1::uuid`, id)
		if r := postLink(t, s.Anonymous(), past, forwarded(nextIP())); r.Status != 401 || errorCode(t, r) != invalidLink {
			t.Fatalf("an expired link: %d %s", r.Status, r.Body)
		}
		leaked = append(leaked, inside, past)
	})

	t.Run("a person has one live link: a new one replaces the earlier one", func(t *testing.T) {
		sub := person(t, s, "one")
		id := userID(t, s, sub)
		first, second := linkFor(t, s, sub), linkFor(t, s, sub)
		leaked = append(leaked, first, second)
		if n := countSQL(t, s, `select count(*) from identity.login_links where user_id = $1::uuid`, id); n != 1 {
			t.Fatalf("%d links stored for one person", n)
		}
		if r := postLink(t, s.Anonymous(), first, forwarded(nextIP())); r.Status != 401 || errorCode(t, r) != invalidLink {
			t.Fatalf("the replaced link: %d %s", r.Status, r.Body)
		}
		if r := postLink(t, s.Anonymous(), second, forwarded(nextIP())); r.Status != 204 {
			t.Fatalf("the new link: %d %s", r.Status, r.Body)
		}
		// Another person's link is untouched by it.
		other := person(t, s, "other")
		otherCode := linkFor(t, s, other)
		linkFor(t, s, sub)
		if r := postLink(t, s.Anonymous(), otherCode, forwarded(nextIP())); r.Status != 204 {
			t.Fatalf("another person's link: %d", r.Status)
		}
		leaked = append(leaked, otherCode)
		if got := auditWhere(auditAll(t, admin), "auth.link_issued", func(e auditEntry) bool { return ptrIs(e.Target, id) }); len(got) != 3 || got[0].ActorID != nil || len(got[0].Meta) != 0 {
			t.Fatalf("auth.link_issued records for the person: %+v", got)
		}
	})

	t.Run("a disabled person's link is refused with 403 disabled and audited", func(t *testing.T) {
		sub := person(t, s, "dis")
		id := userID(t, s, sub)
		code := linkFor(t, s, sub)
		leaked = append(leaked, code)
		execSQL(t, s, `update identity.users set disabled = true where id = $1::uuid`, id)
		r := postLink(t, s.Anonymous(), code, forwarded(nextIP()))
		if r.Status != 403 || errorCode(t, r) != "disabled|account is disabled" || len(r.Header.Values("Set-Cookie")) != 0 {
			t.Fatalf("disabled: %d %s", r.Status, r.Body)
		}
		got := auditWhere(auditAll(t, admin), "auth.login_failed", func(e auditEntry) bool { return ptrIs(e.ActorID, id) })
		if len(got) != 1 || got[0].Meta["reason"] != "disabled" || len(got[0].Meta) != 1 {
			t.Fatalf("audit: %+v", got)
		}
		// The CLI issues no link for them either (exit 1), nor re-provisions them.
		if r := s.CLI(t, "", "login-link", sub); r.Code != 1 || !strings.HasPrefix(r.Stderr, "error:") {
			t.Errorf("login-link for a disabled person: %d %q", r.Code, r.Stderr)
		}
		if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "x"); r.Code != 1 {
			t.Errorf("create-user for a disabled person: %d %q", r.Code, r.Stderr)
		}
		// Control: enabled again, a new link works.
		execSQL(t, s, `update identity.users set disabled = false where id = $1::uuid`, id)
		if r := postLink(t, s.Anonymous(), linkFor(t, s, sub), forwarded(nextIP())); r.Status != 204 {
			t.Fatalf("after enabling: %d", r.Status)
		}
	})

	t.Run("a malformed body is 422 and does not spend the code", func(t *testing.T) {
		sub := person(t, s, "body")
		code := linkFor(t, s, sub)
		leaked = append(leaked, code)
		for name, body := range map[string]any{
			"no code":           map[string]string{},
			"empty code":        map[string]string{"code": ""},
			"101 characters":    map[string]string{"code": "ll_" + strings.Repeat("a", 98)},
			"a number":          map[string]int{"code": 5},
			"not an object":     []string{code},
			"null":              nil,
			"a string for json": "nope",
		} {
			r := s.Anonymous().Do(t, "POST", "/api/auth/link", body, forwarded(nextIP()))
			if r.Status != 422 {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		}
		// Unknown fields are ignored (the zod object is not strict), so this still signs in.
		r := s.Anonymous().Do(t, "POST", "/api/auth/link", map[string]string{"code": code, "extra": "x"}, forwarded(nextIP()))
		if r.Status != 204 {
			t.Fatalf("the code was spent by refused bodies (or extra fields are refused): %d %s", r.Status, r.Body)
		}
	})

	t.Run("a cross-origin or originless link request is refused and does not spend the code", func(t *testing.T) {
		sub := person(t, s, "csrf")
		code := linkFor(t, s, sub)
		leaked = append(leaked, code)
		for _, h := range []string{"Origin:", "Origin: https://evil.example"} {
			r := postLink(t, s.Anonymous(), code, h)
			if r.Status != 403 || errorCode(t, r) != "csrf|cross-origin request rejected" {
				t.Errorf("%q: %d %s", h, r.Status, r.Body)
			}
		}
		if r := postLink(t, s.Anonymous(), code, forwarded(nextIP())); r.Status != 204 {
			t.Fatalf("the code was spent: %d", r.Status)
		}
	})

	t.Run("a successful link sign-in ends the session it replaces; a rejected link leaves it alone", func(t *testing.T) {
		ann, bob := person(t, s, "ann"), person(t, s, "bob")
		annClient := s.Anonymous()
		annCode := linkFor(t, s, ann)
		old := postLink(t, annClient, annCode, forwarded(nextIP()))
		oldCookie := assertSessionCookie(t, old, false, annCode)
		leaked = append(leaked, annCode, oldCookie.Value)

		rejected := postLink(t, annClient, randomLinkCode(), forwarded(nextIP()))
		if rejected.Status != 401 || len(rejected.Header.Values("Set-Cookie")) != 0 {
			t.Fatalf("rejected link: %d, %d Set-Cookie headers", rejected.Status, len(rejected.Header.Values("Set-Cookie")))
		}
		if me := whoami(t, annClient); me.UserID != userID(t, s, ann) {
			t.Fatalf("a rejected link changed the session: %+v", me)
		}

		bobCode := linkFor(t, s, bob)
		replaced := postLink(t, annClient, bobCode, forwarded(nextIP()))
		newCookie := assertSessionCookie(t, replaced, false, bobCode)
		leaked = append(leaked, bobCode, newCookie.Value)
		if oldCookie.Value == newCookie.Value {
			t.Fatal("the session id did not change")
		}
		// The jar now holds bob's cookie; the old one is dead when sent by hand.
		if r := s.Anonymous().Do(t, "GET", "/api/auth/me", nil, "Cookie: mcp_session="+oldCookie.Value); r.Status != 401 {
			t.Fatalf("the replaced session still works: %d", r.Status)
		}
		if me := whoami(t, annClient); me.UserID != userID(t, s, bob) {
			t.Fatalf("the new session is not bob's: %+v", me)
		}
		// The replaced session's end is a logout in the audit log.
		if got := auditWhere(auditAll(t, admin), "auth.logout", func(e auditEntry) bool { return ptrIs(e.ActorID, userID(t, s, ann)) }); len(got) != 1 {
			t.Fatalf("auth.logout records of the replaced session: %+v", got)
		}
	})

	t.Run("two concurrent redemptions of one code create exactly one session", func(t *testing.T) {
		sub := person(t, s, "race")
		id := userID(t, s, sub)
		code := linkFor(t, s, sub)
		leaked = append(leaked, code)
		statuses := make([]int, 2)
		var wg sync.WaitGroup
		for i := range statuses {
			wg.Add(1)
			go func() {
				defer wg.Done()
				statuses[i] = postLink(t, s.Anonymous(), code, forwarded(nextIP())).Status
			}()
		}
		wg.Wait()
		slices.Sort(statuses)
		if statuses[0] != 204 || statuses[1] != 401 {
			t.Fatalf("statuses %v, want one 204 and one 401", statuses)
		}
		if n := countSQL(t, s, `select count(*) from identity.sessions where user_id = $1::uuid`, id); n != 1 {
			t.Fatalf("%d sessions", n)
		}
	})

	t.Run("a session ends after 12 hours idle and at its absolute 30 days", func(t *testing.T) {
		newSession := func() (*harness.Client, string) {
			sub := person(t, s, "sess")
			code := linkFor(t, s, sub)
			c := s.Anonymous()
			leaked = append(leaked, assertSessionCookie(t, postLink(t, c, code, forwarded(nextIP())), false, code).Value, code)
			return c, userID(t, s, sub)
		}
		status := func(c *harness.Client) int { return c.Do(t, "GET", "/api/auth/me", nil).Status }

		// The stored lifetime: 30 days from creation, last seen at creation.
		c, id := newSession()
		var lifetime, seen bool
		if err := s.DB.QueryRow(t.Context(), `select expires_at - created_at = interval '30 days', last_seen_at = created_at
			from identity.sessions where user_id = $1::uuid`, id).Scan(&lifetime, &seen); err != nil || !lifetime || !seen {
			t.Fatalf("session row: lifetime 30 days %v, last seen at creation %v (%v)", lifetime, seen, err)
		}

		// Idle: 11 h 59 min is alive (and the use moves last_seen_at to now), 12 h 1 min is dead.
		execSQL(t, s, `update identity.sessions set last_seen_at = now() - interval '11 hours 59 minutes' where user_id = $1::uuid`, id)
		if got := status(c); got != 200 {
			t.Fatalf("idle 11h59m: %d", got)
		}
		var fresh bool
		if err := s.DB.QueryRow(t.Context(), `select last_seen_at > now() - interval '1 minute' from identity.sessions where user_id = $1::uuid`, id).Scan(&fresh); err != nil || !fresh {
			t.Fatalf("the use did not move last_seen_at to now (%v)", err)
		}
		execSQL(t, s, `update identity.sessions set last_seen_at = now() - interval '12 hours 1 minute' where user_id = $1::uuid`, id)
		if got := status(c); got != 401 {
			t.Fatalf("idle 12h01m: %d", got)
		}
		if got := status(c); got != 401 {
			t.Fatalf("a dead session came back: %d", got)
		}

		// Absolute: created 29 d 23 h 59 min ago is alive even though it was used a minute ago,
		// created 30 d 1 min ago is dead although it was just used.
		c, id = newSession()
		execSQL(t, s, `update identity.sessions set created_at = now() - interval '29 days 23 hours 59 minutes',
			expires_at = now() - interval '29 days 23 hours 59 minutes' + interval '30 days' where user_id = $1::uuid`, id)
		if got := status(c); got != 200 {
			t.Fatalf("age 29d23h59m: %d", got)
		}
		execSQL(t, s, `update identity.sessions set created_at = now() - interval '30 days 1 minute',
			expires_at = now() - interval '30 days 1 minute' + interval '30 days' where user_id = $1::uuid`, id)
		if got := status(c); got != 401 {
			t.Fatalf("age 30d01m: %d", got)
		}
	})

	// --- API tokens ---

	// A token works on /mcp only; the refusal of a token on /api/** (six endpoints, also next to a
	// valid cookie) is TestPlatformBearerNeverOpensTheWebAPI.
	t.Run("an sk_ token: shape, stored only as SHA-256, listed without its value, works on /mcp", func(t *testing.T) {
		owner := s.Member(t, person(t, s, "tok"))
		made := owner.Do(t, "POST", "/api/tokens", map[string]string{"name": "laptop"})
		var created struct{ ID, Prefix, Token string }
		made.JSON(t, "CreateTokenResponse", &created)
		if made.Status != 201 || !tokenPattern.MatchString(created.Token) || created.Prefix != created.Token[:10] {
			t.Fatalf("create: %d id %q prefix %q token %q", made.Status, created.ID, created.Prefix, harness.Redact(created.Token))
		}
		leaked = append(leaked, created.Token)

		var hash string
		var name, prefix string
		if err := s.DB.QueryRow(t.Context(), `select hash, name, prefix from identity.tokens where id = $1::uuid`, created.ID).Scan(&hash, &name, &prefix); err != nil {
			t.Fatal(err)
		}
		if hash != sha256hex(created.Token) || name != "laptop" || prefix != created.Prefix {
			t.Fatalf("stored: hash is sha256 hex of the token %v, name %q, prefix %q", hash == sha256hex(created.Token), name, prefix)
		}
		if n := countSQL(t, s, `select count(*) from identity.tokens t where position($1 in t::text) > 0`, created.Token); n != 0 {
			t.Fatal("the raw token is stored")
		}

		list := func() []struct {
			ID, Name, Prefix string
			LastUsedAt       *string
			RevokedAt        *string
		} {
			r := owner.Do(t, "GET", "/api/tokens", nil)
			var out []struct {
				ID, Name, Prefix string
				LastUsedAt       *string
				RevokedAt        *string
			}
			r.JSON(t, "TokenSummary[]", &out)
			if strings.Contains(string(r.Body), created.Token) {
				t.Fatal("the list carries the token value")
			}
			return out
		}
		before := list()
		if len(before) != 1 || before[0].ID != created.ID || before[0].Name != "laptop" || before[0].Prefix != created.Prefix || before[0].LastUsedAt != nil || before[0].RevokedAt != nil {
			t.Fatalf("list before use: %+v", before)
		}
		tools, err := s.MCP(t, created.Token).ListTools(t.Context(), nil)
		if err != nil || len(tools.Tools) != 4 {
			t.Fatalf("tools/list with the token: %v %v", tools, err)
		}
		if after := list(); after[0].LastUsedAt == nil {
			t.Fatalf("lastUsedAt stays null after use: %+v", after)
		}
		if r := mcpInitialize(t, anon, created.Token, forwarded(nextIP())); r.Status != 200 || !strings.Contains(string(r.Body), `"serverInfo"`) {
			t.Fatalf("raw initialize: %d %.200s", r.Status, r.Body)
		}
		// Names: 1..100 characters.
		for name, body := range map[string]any{"missing": map[string]string{}, "empty": map[string]string{"name": ""}, "101 characters": map[string]string{"name": strings.Repeat("n", 101)}} {
			if r := owner.Do(t, "POST", "/api/tokens", body); r.Status != 422 {
				t.Errorf("name %s: %d %s", name, r.Status, r.Body)
			}
		}
		if r := owner.Do(t, "POST", "/api/tokens", map[string]string{"name": strings.Repeat("n", 100)}); r.Status != 201 {
			t.Errorf("name of 100 characters: %d", r.Status)
		}
		// Without a session there is no list and no creation.
		if r := anon.Do(t, "GET", "/api/tokens", nil); r.Status != 401 || errorCode(t, r) != unauthenticatedSession {
			t.Errorf("anonymous list: %d %s", r.Status, r.Body)
		}
	})

	t.Run("a revoked token is refused; revoking is idempotent, audited once, and limited to the owner (or an admin)", func(t *testing.T) {
		ownerSub := person(t, s, "rev")
		owner, other := s.Member(t, ownerSub), s.Member(t, person(t, s, "rev2"))
		token := owner.Token(t, "laptop")
		leaked = append(leaked, token)
		var list []struct{ ID string }
		owner.Do(t, "GET", "/api/tokens", nil).JSON(t, "TokenSummary[]", &list)
		id := list[0].ID
		if _, err := s.MCP(t, token).ListTools(t.Context(), nil); err != nil {
			t.Fatalf("the token before revoking: %v", err)
		}

		// Someone else's token: not yours (403), unknown (404), not a uuid (404).
		if r := other.Do(t, "DELETE", "/api/tokens/"+id, nil); r.Status != 403 || errorCode(t, r) != "forbidden|not your token" {
			t.Errorf("another member revokes: %d %s", r.Status, r.Body)
		}
		for _, path := range []string{"/api/tokens/5b4d9c0e-0000-4000-8000-000000000000", "/api/tokens/not-a-uuid"} {
			if r := owner.Do(t, "DELETE", path, nil); r.Status != 404 || errorCode(t, r) != "not_found|token not found" {
				t.Errorf("DELETE %s: %d %s", path, r.Status, r.Body)
			}
		}
		if r := mcpInitialize(t, anon, token, forwarded(nextIP())); r.Status != 200 {
			t.Fatalf("the token after the refused revocations: %d", r.Status)
		}

		if r := owner.Do(t, "DELETE", "/api/tokens/"+id, nil); r.Status != 204 || len(r.Body) != 0 {
			t.Fatalf("revoke: %d %q", r.Status, r.Body)
		}
		ip := nextIP()
		bad := mcpInitialize(t, anon, token, forwarded(ip))
		if bad.Status != 401 || errorCode(t, bad) != "invalid_token|invalid token" || bad.Header.Get("Www-Authenticate") != "Bearer" {
			t.Fatalf("revoked token: %d %v %s", bad.Status, bad.Header, bad.Body)
		}
		var after []struct {
			ID        string
			RevokedAt *string
		}
		owner.Do(t, "GET", "/api/tokens", nil).JSON(t, "TokenSummary[]", &after)
		if len(after) != 1 || after[0].RevokedAt == nil {
			t.Fatalf("the revoked token is not listed as revoked: %+v", after)
		}
		if r := owner.Do(t, "DELETE", "/api/tokens/"+id, nil); r.Status != 204 {
			t.Fatalf("revoking twice: %d", r.Status)
		}
		ownerID := userID(t, s, ownerSub)
		got := auditWhere(auditAll(t, admin), "token.revoke", func(e auditEntry) bool { return ptrIs(e.Target, id) })
		if len(got) != 1 || !ptrIs(got[0].ActorID, ownerID) || len(got[0].Meta) != 0 {
			t.Fatalf("token.revoke audit: %+v", got)
		}
		created := auditWhere(auditAll(t, admin), "token.create", func(e auditEntry) bool { return ptrIs(e.Target, id) })
		if len(created) != 1 || !ptrIs(created[0].ActorID, ownerID) || len(created[0].Meta) != 2 || created[0].Meta["name"] != "laptop" || created[0].Meta["prefix"] != token[:10] {
			t.Fatalf("token.create audit: %+v", created)
		}

		// An admin may revoke anyone's token.
		second := other.Token(t, "phone")
		leaked = append(leaked, second)
		var otherList []struct{ ID string }
		other.Do(t, "GET", "/api/tokens", nil).JSON(t, "TokenSummary[]", &otherList)
		if r := admin.Do(t, "DELETE", "/api/tokens/"+otherList[0].ID, nil); r.Status != 204 {
			t.Fatalf("admin revokes a member's token: %d", r.Status)
		}
		if r := mcpInitialize(t, anon, second, forwarded(nextIP())); r.Status != 401 {
			t.Fatalf("the token an admin revoked: %d", r.Status)
		}
	})

	// --- People ---

	t.Run("disabling a person ends their sessions, tokens and links at once; enabling brings back no token", func(t *testing.T) {
		sub := person(t, s, "victim")
		victim := s.Member(t, sub)
		id := whoami(t, victim).UserID
		token := victim.Token(t, "laptop")
		pending := linkFor(t, s, sub)
		leaked = append(leaked, token, pending)
		if r := mcpInitialize(t, anon, token, forwarded(nextIP())); r.Status != 200 {
			t.Fatalf("the token before: %d", r.Status)
		}

		off := admin.Do(t, "POST", "/api/users/"+id+"/disabled", map[string]bool{"disabled": true})
		if off.Status != 204 || len(off.Body) != 0 {
			t.Fatalf("disable: %d %q", off.Status, off.Body)
		}
		if r := victim.Do(t, "GET", "/api/auth/me", nil); r.Status != 401 || errorCode(t, r) != unauthenticatedSession {
			t.Fatalf("session after disabling: %d %s", r.Status, r.Body)
		}
		if r := mcpInitialize(t, anon, token, forwarded(nextIP())); r.Status != 401 || errorCode(t, r) != "invalid_token|invalid token" {
			t.Fatalf("token after disabling: %d %s", r.Status, r.Body)
		}
		if n := countSQL(t, s, `select count(*) from identity.sessions where user_id = $1::uuid`, id); n != 0 {
			t.Fatalf("%d sessions remain", n)
		}
		if n := countSQL(t, s, `select count(*) from identity.login_links where user_id = $1::uuid`, id); n != 0 {
			t.Fatalf("%d links remain", n)
		}
		if n := countSQL(t, s, `select count(*) from identity.tokens where user_id = $1::uuid and revoked_at is null`, id); n != 0 {
			t.Fatalf("%d tokens are not revoked", n)
		}
		if r := postLink(t, s.Anonymous(), pending, forwarded(nextIP())); r.Status != 401 || errorCode(t, r) != invalidLink {
			t.Fatalf("the pending link after disabling: %d %s", r.Status, r.Body)
		}
		var users []struct {
			ID       string
			Disabled bool
		}
		admin.Do(t, "GET", "/api/users", nil).JSON(t, "UserSummary[]", &users)
		listed := false
		for _, u := range users {
			if u.ID == id {
				listed = u.Disabled
			}
		}
		if !listed {
			t.Fatal("the person is not listed as disabled")
		}
		got := auditWhere(auditAll(t, admin), "user.disable", func(e auditEntry) bool { return ptrIs(e.Target, id) })
		if len(got) != 1 || got[0].ActorID == nil || got[0].Meta["disabled"] != true || len(got[0].Meta) != 1 {
			t.Fatalf("user.disable audit: %+v", got)
		}

		// Enabling does not bring the revoked token or the ended session back; a new link works.
		if r := admin.Do(t, "POST", "/api/users/"+id+"/disabled", map[string]bool{"disabled": false}); r.Status != 204 {
			t.Fatalf("enable: %d", r.Status)
		}
		if r := mcpInitialize(t, anon, token, forwarded(nextIP())); r.Status != 401 {
			t.Fatalf("the revoked token after enabling: %d", r.Status)
		}
		if r := victim.Do(t, "GET", "/api/auth/me", nil); r.Status != 401 {
			t.Fatalf("the ended session after enabling: %d", r.Status)
		}
		fresh := s.Anonymous()
		if r := postLink(t, fresh, linkFor(t, s, sub), forwarded(nextIP())); r.Status != 204 {
			t.Fatalf("sign-in after enabling: %d", r.Status)
		}
		if me := whoami(t, fresh); me.UserID != id {
			t.Fatalf("me: %+v", me)
		}
		again := auditWhere(auditAll(t, admin), "user.disable", func(e auditEntry) bool { return ptrIs(e.Target, id) && e.Meta["disabled"] == false })
		if len(again) != 1 {
			t.Fatalf("the enabling is not audited: %+v", again)
		}
	})

	t.Run("an admin cannot disable themselves; members cannot administer people", func(t *testing.T) {
		self := admin.Do(t, "GET", "/api/auth/me", nil)
		var me meBody
		self.JSON(t, "MeResponse", &me)
		r := admin.Do(t, "POST", "/api/users/"+me.UserID+"/disabled", map[string]bool{"disabled": true})
		if r.Status != 403 || errorCode(t, r) != "forbidden|cannot disable yourself" {
			t.Fatalf("self-disable: %d %s", r.Status, r.Body)
		}
		if n := countSQL(t, s, `select count(*) from identity.users where id = $1::uuid and not disabled`, me.UserID); n != 1 {
			t.Fatal("the admin was disabled")
		}
		if got := whoami(t, admin); got.UserID != me.UserID {
			t.Fatalf("the admin lost the session: %+v", got)
		}
		// Control: another admin can be disabled by this one.
		otherSub := harness.Unique("admin2")
		if r := s.CLI(t, "", "create-user", "--sub", otherSub, "--name", "Second", "--admin"); r.Code != 0 {
			t.Fatalf("create-user: %d", r.Code)
		}
		if r := admin.Do(t, "POST", "/api/users/"+userID(t, s, otherSub)+"/disabled", map[string]bool{"disabled": true}); r.Status != 204 {
			t.Fatalf("disabling another admin: %d %s", r.Status, r.Body)
		}

		member := s.Member(t, person(t, s, "plain"))
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{"GET", "/api/users", nil},
			{"POST", "/api/users/" + me.UserID + "/disabled", map[string]bool{"disabled": true}},
			{"POST", "/api/users/not-a-uuid/disabled", "this is not json"}, // refused before the id and the body are read
		} {
			if r := member.Do(t, c.method, c.path, c.body); r.Status != 403 || errorCode(t, r) != "forbidden|admin only" {
				t.Errorf("member %s %s: %d %s", c.method, c.path, r.Status, r.Body)
			}
		}
		if r := anon.Do(t, "GET", "/api/users", nil); r.Status != 401 || errorCode(t, r) != unauthenticatedSession {
			t.Errorf("anonymous GET /api/users: %d %s", r.Status, r.Body)
		}
		if r := anon.Do(t, "POST", "/api/users/"+me.UserID+"/disabled", map[string]bool{"disabled": true}); r.Status != 401 {
			t.Errorf("anonymous disable: %d", r.Status)
		}
		// Admin input errors: an id that is not a uuid or unknown is 404, a bad body is 422.
		for _, path := range []string{"/api/users/not-a-uuid/disabled", "/api/users/5b4d9c0e-0000-4000-8000-000000000000/disabled"} {
			if r := admin.Do(t, "POST", path, map[string]bool{"disabled": true}); r.Status != 404 || errorCode(t, r) != "not_found|user not found" {
				t.Errorf("POST %s: %d %s", path, r.Status, r.Body)
			}
		}
		for name, body := range map[string]any{"empty": map[string]string{}, "a string": map[string]string{"disabled": "yes"}} {
			if r := admin.Do(t, "POST", "/api/users/"+me.UserID+"/disabled", body); r.Status != 422 {
				t.Errorf("body %s: %d %s", name, r.Status, r.Body)
			}
		}
	})

	t.Run("the people list shows roles, groups and sign-ins", func(t *testing.T) {
		adminSub := harness.Unique("lister")
		s.CLI(t, "", "create-user", "--sub", adminSub, "--name", "Lister", "--admin")
		memberSub := person(t, s, "listed")
		var users []struct {
			ID, Name   string
			Email      *string
			Role       string
			Groups     []string
			Disabled   bool
			LastLoginA *string `json:"lastLoginAt"`
		}
		admin.Do(t, "GET", "/api/users", nil).JSON(t, "UserSummary[]", &users)
		seen := map[string]bool{}
		for _, u := range users {
			switch u.ID {
			case userID(t, s, adminSub):
				seen["admin"] = true
				if u.Role != "admin" || !slices.Equal(u.Groups, []string{"mcp-admins"}) || u.Name != "Lister" || u.Email != nil || u.Disabled || u.LastLoginA == nil {
					t.Errorf("admin row: %+v", u)
				}
			case userID(t, s, memberSub):
				seen["member"] = true
				if u.Role != "member" || !slices.Equal(u.Groups, []string{"mcp-users"}) || u.Disabled {
					t.Errorf("member row: %+v", u)
				}
			}
		}
		if len(seen) != 2 {
			t.Fatalf("rows missing: %v", seen)
		}
		// The CLI is idempotent by sub: a second create-user updates, keeps the id.
		if r := s.CLI(t, "", "create-user", "--sub", memberSub, "--name", "Renamed"); r.Code != 0 || !strings.HasPrefix(r.Stdout, `updated member "Renamed" (sub `+memberSub+", id "+userID(t, s, memberSub)+")") {
			t.Errorf("second create-user: %d %q", r.Code, r.Stdout)
		}
	})

	// --- Rate limits: the last scenarios, each with its own client addresses ---

	t.Run("the 11th failed link attempt within a minute from one address is 429 with Retry-After", func(t *testing.T) {
		ip := nextIP()
		for i := 1; i <= 10; i++ {
			r := postLink(t, s.Anonymous(), randomLinkCode(), forwarded(ip))
			if r.Status != 401 || errorCode(t, r) != invalidLink {
				t.Fatalf("failure %d: %d %s", i, r.Status, r.Body)
			}
		}
		r := postLink(t, s.Anonymous(), randomLinkCode(), forwarded(ip))
		if r.Status != 429 || errorCode(t, r) != "rate_limited|too many failed attempts" || r.Header.Get("Retry-After") != "60" {
			t.Fatalf("11th failure: %d Retry-After %q %s", r.Status, r.Header.Get("Retry-After"), r.Body)
		}
		// Still locked: another failure is 429 with the remaining time (an integer within the lock).
		again := postLink(t, s.Anonymous(), randomLinkCode(), forwarded(ip))
		if secs, err := strconv.Atoi(again.Header.Get("Retry-After")); again.Status != 429 || err != nil || secs < 1 || secs > 60 {
			t.Fatalf("12th failure: %d Retry-After %q", again.Status, again.Header.Get("Retry-After"))
		}
		// A valid code is still accepted from the locked-out address.
		sub := person(t, s, "locked")
		code := linkFor(t, s, sub)
		leaked = append(leaked, code)
		if r := postLink(t, s.Anonymous(), code, forwarded(ip)); r.Status != 204 {
			t.Fatalf("a valid code from a locked-out address: %d %s", r.Status, r.Body)
		}
		// The lock is per address: another one is not affected (control) ...
		if r := postLink(t, s.Anonymous(), randomLinkCode(), forwarded(nextIP())); r.Status != 401 {
			t.Fatalf("another address: %d", r.Status)
		}
		// ... and only the address the proxy added counts: a forged leftmost entry does not dodge the lock.
		if r := postLink(t, s.Anonymous(), randomLinkCode(), forwarded(nextIP()+", "+ip)); r.Status != 429 {
			t.Fatalf("forged X-Forwarded-For: %d", r.Status)
		}
		// The token limiter is separate: this address may still check tokens.
		if r := mcpInitialize(t, anon, "sk_"+strings.Repeat("x", 43), forwarded(ip)); r.Status != 401 || errorCode(t, r) != "invalid_token|invalid token" {
			t.Fatalf("a bad token from the link-locked address: %d %s", r.Status, r.Body)
		}
	})

	t.Run("the 31st failed token check within a minute from one address is 429; a valid token still works", func(t *testing.T) {
		ip := nextIP()
		// Requests without a token are not failed checks: they never count.
		for i := 0; i < 40; i++ {
			r := anon.Do(t, "GET", "/mcp", nil, forwarded(ip))
			if r.Status != 401 || errorCode(t, r) != "unauthenticated|bearer token required" || r.Header.Get("Www-Authenticate") != "Bearer" {
				t.Fatalf("no token %d: %d %s", i, r.Status, r.Body)
			}
		}
		owner := s.Member(t, person(t, s, "limited"))
		good := owner.Token(t, "laptop")
		leaked = append(leaked, good)
		for i := 1; i <= 30; i++ {
			// A wrong sk_ token and a value without the sk_ mark both count.
			bad := fmt.Sprintf("sk_%043d", i)
			if i%2 == 0 {
				bad = fmt.Sprintf("nope-%d", i)
			}
			r := anon.Do(t, "GET", "/mcp", nil, forwarded(ip), "Authorization: Bearer "+bad)
			if r.Status != 401 || errorCode(t, r) != "invalid_token|invalid token" || r.Header.Get("Www-Authenticate") != "Bearer" {
				t.Fatalf("failure %d: %d %v %s", i, r.Status, r.Header, r.Body)
			}
		}
		r := anon.Do(t, "GET", "/mcp", nil, forwarded(ip), "Authorization: Bearer sk_"+strings.Repeat("y", 43))
		if r.Status != 429 || errorCode(t, r) != "rate_limited|too many failed attempts" || r.Header.Get("Retry-After") != "" || r.Header.Get("Www-Authenticate") != "" {
			// Unlike the link endpoint, the bearer middleware sets no Retry-After (pinned as is).
			t.Fatalf("31st failure: %d %v %s", r.Status, r.Header, r.Body)
		}
		if r := mcpInitialize(t, anon, good, forwarded(ip)); r.Status != 200 {
			t.Fatalf("a valid token from the locked-out address: %d %s", r.Status, r.Body)
		}
		// Another address is not locked (control), and the link limiter is separate.
		if r := anon.Do(t, "GET", "/mcp", nil, forwarded(nextIP()), "Authorization: Bearer sk_"+strings.Repeat("z", 43)); r.Status != 401 {
			t.Fatalf("another address: %d", r.Status)
		}
		if r := postLink(t, s.Anonymous(), randomLinkCode(), forwarded(ip)); r.Status != 401 || errorCode(t, r) != invalidLink {
			t.Fatalf("a bad link from the token-locked address: %d %s", r.Status, r.Body)
		}
	})

	t.Run("the audit log and the app log hold no link code, token or session id", func(t *testing.T) {
		entries := auditAll(t, admin)
		all := mustJSON(t, entries)
		logs := s.Logs(t)
		for _, secret := range leaked {
			if secret == "" {
				t.Fatal("an empty secret was recorded")
			}
			if strings.Contains(all, secret) {
				t.Errorf("the audit log contains %s", harness.Redact(secret))
			}
			if strings.Contains(logs, secret) {
				t.Errorf("the app log contains %s", harness.Redact(secret))
			}
		}
		if len(leaked) < 20 {
			t.Fatalf("only %d secrets were checked", len(leaked))
		}
		// A link sign-in is recorded with the person and via=link, nothing else.
		links := auditWhere(entries, "auth.login", nil)
		if len(links) < 10 {
			t.Fatalf("only %d auth.login records", len(links))
		}
		for _, e := range links {
			if e.ActorID == nil || e.Target != nil || len(e.Meta) != 1 || e.Meta["via"] != "link" {
				t.Errorf("auth.login record: %+v", e)
			}
		}
		// The provisioning by the CLI is audited with no actor.
		prov := auditWhere(entries, "user.provision", nil)
		if len(prov) == 0 || prov[0].ActorID != nil || prov[0].Meta["via"] != "cli" || prov[0].Target == nil {
			t.Fatalf("user.provision audit: %+v", prov)
		}
	})

	t.Run("a restart prunes expired links and dead sessions and keeps live ones", func(t *testing.T) {
		sub := person(t, s, "prune")
		id := userID(t, s, sub)
		// Rows of one person: a live and an expired link; a live, an idle and an expired session.
		for name, hours := range map[string]string{"live-link": "+15 minutes", "dead-link": "-1 minute"} {
			execSQL(t, s, `insert into identity.login_links (code_hash, user_id, created_at, expires_at)
				values ($1, $2::uuid, now(), now() + $3::interval)`, sha256hex(name), id, hours)
		}
		for name, c := range map[string][2]string{
			"live-session": {"0 minutes", "+30 days"},
			"idle-session": {"-13 hours", "+30 days"},
			"aged-session": {"0 minutes", "-1 minute"},
		} {
			execSQL(t, s, `insert into identity.sessions (id_hash, user_id, created_at, last_seen_at, expires_at)
				values ($1, $2::uuid, now() - interval '31 days', now() + $3::interval, now() + $4::interval)`, sha256hex(name), id, c[0], c[1])
		}
		s.SwapImage(t, platformImage(), "")
		harness.Eventually(t, "expired links and dead sessions are removed at start", func() bool {
			return countSQL(t, s, `select count(*) from identity.login_links where user_id = $1::uuid and code_hash = $2`, id, sha256hex("dead-link")) == 0 &&
				countSQL(t, s, `select count(*) from identity.sessions where id_hash in ($1, $2)`, sha256hex("idle-session"), sha256hex("aged-session")) == 0
		})
		if n := countSQL(t, s, `select count(*) from identity.login_links where user_id = $1::uuid and code_hash = $2`, id, sha256hex("live-link")); n != 1 {
			t.Errorf("the live link was pruned")
		}
		if n := countSQL(t, s, `select count(*) from identity.sessions where id_hash = $1`, sha256hex("live-session")); n != 1 {
			t.Errorf("the live session was pruned")
		}
		// The admin's session of this test survived the restart (sessions live in the database).
		if me := whoami(t, admin); me.Role != "admin" {
			t.Errorf("the session after the restart: %+v", me)
		}
	})
}

// cookieDesc describes a cookie without its value.
func cookieDesc(c *http.Cookie) string {
	if c == nil {
		return "<none>"
	}
	return fmt.Sprintf("%s (value %d chars) path %q httpOnly %v secure %v sameSite %v maxAge %d", c.Name, len(c.Value), c.Path, c.HttpOnly, c.Secure, c.SameSite, c.MaxAge)
}
