package http_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

const errSignIn = `{"error":{"code":"unauthenticated","message":"sign in required"}}`

// errBearer is the guard's refusal of an Authorization header.
const errBearer = `{"error":{"code":"unauthenticated","message":"tokens do not work on the web API"}}` //nolint:gosec // G101: an error text, no credential

func errBody(code, message string) string {
	return `{"error":{"code":"` + code + `","message":` + jsonString(message) + `}}`
}

func jsonString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(err)
	}
	return strings.TrimRight(buf.String(), "\n")
}

func expect(t *testing.T, what string, r reply, status int, body string) {
	t.Helper()
	if r.Status != status || r.Body != body {
		t.Fatalf("%s: got %d %q, want %d %q", what, r.Status, r.Body, status, body)
	}
}

func TestEveryRouteNeedsASession(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/connections", ""},
		{"POST", "/api/connections/gitea/start", ""},
		{"PUT", "/api/connections/tracker/pat", `{"token":"tok-1"}`},
		{"DELETE", "/api/connections/gitea", ""},
		// Under the prefix an unknown path is the guard's answer, not a 404.
		{"GET", "/api/connections/a/b/c", ""},
		// The callback's own path with another method is guarded as well.
		{"POST", "/api/connections/callback", ""},
	} {
		expect(t, "anonymous "+c.method+" "+c.path, e.do(t, "", c.method, c.path, c.body), 401, errSignIn)
		r := e.do(t, "alice", c.method, c.path, c.body, "Authorization", "Bearer sk_secret")
		expect(t, "token "+c.method+" "+c.path, r, 401, errBearer)
	}
	if e.pending.size() != 0 || len(e.store.rows) != 0 {
		t.Fatal("a refused request changed state")
	}
}

func TestListConnections(t *testing.T) {
	e := newEnv(t)
	want := `[{"server":"gitea","name":"Gitea","authKind":"oauth_user","status":"missing"},{"server":"tracker","name":"Tracker","authKind":"pat_user","status":"missing"}]`
	expect(t, "nothing connected", e.do(t, "alice", "GET", "/api/connections", ""), 200, want)

	expect(t, "save a PAT", e.do(t, "alice", "PUT", "/api/connections/tracker/pat", `{"token":"tok-1"}`), 204, "")
	got := e.do(t, "alice", "GET", "/api/connections", "")
	wantReady := `[{"server":"gitea","name":"Gitea","authKind":"oauth_user","status":"missing"},{"server":"tracker","name":"Tracker","authKind":"pat_user","status":"ready"}]`
	expect(t, "after the PAT", got, 200, wantReady)
	if got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type %q", got.Header.Get("Content-Type"))
	}
	// Somebody else's list is their own.
	expect(t, "bob", e.do(t, "bob", "GET", "/api/connections", ""), 200, want)

	// A server edited after the PAT was saved: the connection shows as expired.
	e.dir.servers[1] = serverOf("tracker", "Tracker", catalog.AuthPATUser)
	e.dir.servers[1].Config.URL = "https://elsewhere.example.com/mcp"
	expect(t, "config changed", e.do(t, "alice", "GET", "/api/connections", ""), 200,
		strings.Replace(wantReady, `"status":"ready"`, `"status":"expired"`, 1))

	e.dir.servers = nil
	expect(t, "no personal servers", e.do(t, "alice", "GET", "/api/connections", ""), 200, `[]`)
}

func TestStartConnect(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, "alice", "POST", "/api/connections/gitea/start", "")
	expect(t, "start", r, 200, `{"url":"https://idp.example.com/authorize?state=STATEMARK-1&code_challenge=S256(VERIFIERMARK-1)"}`)
	if e.pending.size() != 1 {
		t.Fatal("the attempt is not stored")
	}
	expect(t, "pat server", e.do(t, "alice", "POST", "/api/connections/tracker/start", ""), 422,
		errBody("invalid_auth_kind", `server "tracker" does not use OAuth`))
	expect(t, "unknown", e.do(t, "alice", "POST", "/api/connections/nope/start", ""), 404,
		errBody("not_found", `server "nope" not found`))
	expect(t, "NUL", e.do(t, "alice", "POST", "/api/connections/a%00b/start", ""), 404,
		errBody("not_found", "server \"a\x00b\" not found"))
	expect(t, "invalid UTF-8", e.do(t, "alice", "POST", "/api/connections/%ff/start", ""), 404, errBody("not_found", "not found"))
	// A server called "callback" is an ordinary server for the other methods.
	expect(t, "server named callback", e.do(t, "alice", "POST", "/api/connections/callback/start", ""), 404,
		errBody("not_found", `server "callback" not found`))
}

func TestSavePAT(t *testing.T) {
	e := newEnv(t)
	put := func(user, server, body string) reply {
		return e.do(t, user, "PUT", "/api/connections/"+server+"/pat", body)
	}
	expect(t, "trimmed", put("alice", "tracker", `{"token":"  perm:abc \n"}`), 204, "")
	if !e.store.has("alice", "tracker") {
		t.Fatal("not stored")
	}
	if got := e.audit.actions(); len(got) != 1 || got[0] != "connection.pat_saved" {
		t.Fatalf("audit %v", got)
	}
	expect(t, "4096 characters", put("alice", "tracker", `{"token":"`+strings.Repeat("b", 4096)+`"}`), 204, "")

	for _, c := range []struct {
		name, server, body, code, msg string
		status                        int
	}{
		{"blank", "tracker", `{"token":"   "}`, "invalid_pat", "the token must be a non-empty single line", 422},
		{"LF inside", "tracker", `{"token":"abc\ndef"}`, "invalid_pat", "the token must be a non-empty single line", 422},
		{"NUL inside", "tracker", `{"token":"abc\u0000def"}`, "invalid_pat", "the token must be a non-empty single line", 422},
		{"empty", "tracker", `{"token":""}`, "invalid_request", "token: is required", 422},
		{"missing", "tracker", `{}`, "invalid_request", "token: is required", 422},
		{"null", "tracker", `{"token":null}`, "invalid_request", "token: is required", 422},
		{"too long", "tracker", `{"token":"` + strings.Repeat("a", 4097) + `"}`, "invalid_request", "token: must be at most 4096 characters", 422},
		{"wrong type", "tracker", `{"token":5}`, "invalid_request", "token: invalid type", 422},
		{"not JSON", "tracker", `{`, "invalid_request", "body is not valid JSON", 422},
		{"body before server", "nope", `{}`, "invalid_request", "token: is required", 422},
		{"oauth server", "gitea", `{"token":"t-1"}`, "invalid_auth_kind", `server "gitea" does not use personal access tokens`, 422},
		{"unknown server", "nope", `{"token":"t-1"}`, "not_found", `server "nope" not found`, 404},
		{"NUL server", "a%00b", `{"token":"t-1"}`, "not_found", "server \"a\x00b\" not found", 404},
	} {
		expect(t, c.name, put("alice", c.server, c.body), c.status, errBody(c.code, c.msg))
	}
	if e.store.has("bob", "tracker") || e.store.has("alice", "gitea") {
		t.Fatal("a refused token was stored")
	}
}

func TestDisconnect(t *testing.T) {
	e := newEnv(t)
	del := func(user, server string) reply { return e.do(t, user, "DELETE", "/api/connections/"+server, "") }
	expect(t, "nothing stored", del("alice", "tracker"), 204, "")
	expect(t, "unknown server", del("alice", "nope"), 204, "")
	expect(t, "NUL", del("alice", "a%00b"), 204, "")
	if len(e.audit.actions()) != 0 {
		t.Fatalf("audit for nothing: %v", e.audit.actions())
	}
	e.do(t, "alice", "PUT", "/api/connections/tracker/pat", `{"token":"tok-1"}`)
	expect(t, "stored", del("alice", "tracker"), 204, "")
	if e.store.has("alice", "tracker") {
		t.Fatal("not deleted")
	}
	if got := e.audit.actions(); len(got) != 2 || got[1] != "connection.disconnect" {
		t.Fatalf("audit %v", got)
	}

	// An OAuth connection is revoked at the provider with the refresh token.
	u := e.do(t, "alice", "POST", "/api/connections/gitea/start", "")
	state := stateOf(t, u)
	if loc := e.callback(t, "alice", "state="+url.QueryEscape(state)+"&code=C1"); loc != "/connections?connected=gitea" {
		t.Fatalf("callback %q", loc)
	}
	expect(t, "oauth", del("alice", "gitea"), 204, "")
	if len(e.oauth.revoked) != 1 || e.oauth.revoked[0] != "REFRESH-1" {
		t.Fatalf("revoked %v", e.oauth.revoked)
	}
}

func stateOf(t *testing.T, r reply) string {
	t.Helper()
	i := strings.Index(r.Body, "state=")
	if r.Status != 200 || i < 0 {
		t.Fatalf("start: %d %s", r.Status, r.Body)
	}
	rest := r.Body[i+len("state="):]
	return rest[:strings.IndexAny(rest, `&"`)]
}

// callback visits the callback and returns the Location of its 302.
func (e *env) callback(t testing.TB, user, query string, headers ...string) string {
	t.Helper()
	target := "/api/connections/callback"
	if query != "" {
		target += "?" + query
	}
	r := e.do(t, user, "GET", target, "", headers...)
	if r.Status != 302 || r.Body != "" {
		t.Fatalf("callback %q: %d %q, want an empty 302", query, r.Status, r.Body)
	}
	return r.Header.Get("Location")
}

func (e *env) start(t testing.TB, user, server string) string {
	t.Helper()
	r := e.do(t, user, "POST", "/api/connections/"+server+"/start", "")
	if r.Status != 200 {
		t.Fatalf("start: %d %s", r.Status, r.Body)
	}
	i := strings.Index(r.Body, "state=")
	rest := r.Body[i+len("state="):]
	return rest[:strings.IndexAny(rest, `&"`)]
}

func TestCallbackRedirects(t *testing.T) {
	longState := strings.Repeat("s", 257)
	longCode := strings.Repeat("c", 4097)

	t.Run("no session", func(t *testing.T) {
		e := newEnv(t)
		state := e.start(t, "alice", "gitea")
		q := "state=" + state + "&code=C1"
		if loc := e.callback(t, "", q); loc != "/connections?error=unauthenticated" {
			t.Fatal(loc)
		}
		// A token is no session, not even next to a valid cookie.
		if loc := e.callback(t, "alice", q, "Authorization", "Bearer sk_abc"); loc != "/connections?error=unauthenticated" {
			t.Fatal(loc)
		}
		if e.pending.size() != 1 || len(e.oauth.codes) != 0 {
			t.Fatal("an unauthenticated callback touched the attempt")
		}
		// The unauthenticated check comes before the parameters.
		if loc := e.callback(t, "", ""); loc != "/connections?error=unauthenticated" {
			t.Fatal(loc)
		}
	})

	t.Run("denied", func(t *testing.T) {
		e := newEnv(t)
		state := e.start(t, "alice", "gitea")
		for name, q := range map[string]string{
			"no parameters":    "",
			"only a state":     "state=" + state,
			"only a code":      "code=C1",
			"provider refusal": "state=" + state + "&error=access_denied",
			"empty state":      "state=&code=C1",
			"empty code":       "state=" + state + "&code=",
			"state too long":   "state=" + longState + "&code=C1",
			"code too long":    "state=" + state + "&code=" + longCode,
		} {
			if loc := e.callback(t, "alice", q); loc != "/connections?error=denied" {
				t.Fatalf("%s: %q", name, loc)
			}
		}
		if e.pending.size() != 1 {
			t.Fatal("a callback without a usable code consumed the attempt")
		}
		// The limits are inclusive, counted in characters.
		ok := strings.Repeat("é", 256)
		if loc := e.callback(t, "alice", "state="+url.QueryEscape(ok)+"&code="+strings.Repeat("c", 4096)); loc != "/connections?error=invalid_state" {
			t.Fatalf("at the limits: %q", loc)
		}
	})

	t.Run("success", func(t *testing.T) {
		e := newEnv(t)
		state := e.start(t, "alice", "gitea")
		r := e.do(t, "alice", "GET", "/api/connections/callback?state="+state+"&code=C1", "")
		if r.Status != 302 || r.Header.Get("Location") != "/connections?connected=gitea" || r.Body != "" {
			t.Fatalf("%d %q %q", r.Status, r.Header.Get("Location"), r.Body)
		}
		if r.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("Referrer-Policy %q", r.Header.Get("Referrer-Policy"))
		}
		if !e.store.has("alice", "gitea") || len(e.oauth.codes) != 1 || e.oauth.codes[0] != "C1" {
			t.Fatal("not connected")
		}
		if got := e.audit.actions(); len(got) != 1 || got[0] != "connection.connect" {
			t.Fatalf("audit %v", got)
		}
		expect(t, "listed ready", e.do(t, "alice", "GET", "/api/connections", ""), 200,
			`[{"server":"gitea","name":"Gitea","authKind":"oauth_user","status":"ready"},{"server":"tracker","name":"Tracker","authKind":"pat_user","status":"missing"}]`)
	})

	t.Run("the first value of a repeated parameter counts", func(t *testing.T) {
		e := newEnv(t)
		state := e.start(t, "alice", "gitea")
		if loc := e.callback(t, "alice", "state=zzz&state="+state+"&code=C1"); loc != "/connections?error=invalid_state" {
			t.Fatal(loc)
		}
		if loc := e.callback(t, "alice", "state="+state+"&state=zzz&code=C1&code=C2&extra=1"); loc != "/connections?connected=gitea" {
			t.Fatal(loc)
		}
		if e.oauth.codes[0] != "C1" {
			t.Fatalf("code %v", e.oauth.codes)
		}
	})

	t.Run("invalid_state", func(t *testing.T) {
		e := newEnv(t)
		state := e.start(t, "alice", "gitea")
		q := "state=" + state + "&code=C1"
		if loc := e.callback(t, "alice", "state=unknown&code=C1"); loc != "/connections?error=invalid_state" {
			t.Fatal(loc)
		}
		if loc := e.callback(t, "alice", "state=a%00b&code=C1"); loc != "/connections?error=invalid_state" {
			t.Fatalf("NUL: %q", loc)
		}
		if loc := e.callback(t, "alice", q); loc != "/connections?connected=gitea" {
			t.Fatal(loc)
		}
		// Replay.
		if loc := e.callback(t, "alice", q); loc != "/connections?error=invalid_state" {
			t.Fatal(loc)
		}
		if len(e.oauth.codes) != 1 {
			t.Fatalf("the provider was asked %d times", len(e.oauth.codes))
		}
	})

	t.Run("another person: refused, and the starter can still finish", func(t *testing.T) {
		e := newEnv(t)
		state := e.start(t, "alice", "gitea")
		q := "state=" + state + "&code=C1"
		if loc := e.callback(t, "bob", q); loc != "/connections?error=invalid_state" {
			t.Fatal(loc)
		}
		if e.store.has("bob", "gitea") || len(e.oauth.codes) != 0 || e.pending.size() != 1 {
			t.Fatal("bob's callback changed something")
		}
		if loc := e.callback(t, "alice", q); loc != "/connections?connected=gitea" {
			t.Fatalf("the starter could not finish: %q", loc)
		}
	})

	t.Run("not_found and invalid_auth_kind", func(t *testing.T) {
		e := newEnv(t)
		state := e.start(t, "alice", "gitea")
		e.dir.remove("gitea")
		if loc := e.callback(t, "alice", "state="+state+"&code=C1"); loc != "/connections?error=not_found" {
			t.Fatal(loc)
		}
		e = newEnv(t)
		state = e.start(t, "alice", "gitea")
		e.dir.servers[0] = serverOf("gitea", "Gitea", catalog.AuthPATUser)
		if loc := e.callback(t, "alice", "state="+state+"&code=C1"); loc != "/connections?error=invalid_auth_kind" {
			t.Fatal(loc)
		}
		if len(e.oauth.codes) != 0 {
			t.Fatal("the code went to a provider of a server that is no longer an OAuth one")
		}
	})

	t.Run("the configuration changed since the start", func(t *testing.T) {
		e := newEnv(t)
		state := e.start(t, "alice", "gitea")
		e.dir.servers[0].Config.OAuth.TokenURL = "https://other.example.com/token"
		if loc := e.callback(t, "alice", "state="+state+"&code=C1"); loc != "/connections?error=invalid_state" {
			t.Fatal(loc)
		}
		if len(e.oauth.codes) != 0 {
			t.Fatal("the code went to a changed token endpoint")
		}
	})

	t.Run("exchange_failed", func(t *testing.T) {
		e := newEnv(t)
		e.oauth.exchangeErr = errors.New("provider said no, code C1 state")
		state := e.start(t, "alice", "gitea")
		if loc := e.callback(t, "alice", "state="+state+"&code=C1"); loc != "/connections?error=exchange_failed" {
			t.Fatal(loc)
		}
		if e.store.has("alice", "gitea") {
			t.Fatal("stored after a failed exchange")
		}
	})

	t.Run("an unknown error code is exchange_failed", func(t *testing.T) {
		e := newEnv(t)
		e.pending.failErr = kernel.NewDomainError("some_new_code", "x")
		if loc := e.callback(t, "alice", "state=abc&code=C1"); loc != "/connections?error=exchange_failed" {
			t.Fatal(loc)
		}
		if strings.Contains(e.logs.String(), "some_new_code") {
			t.Fatal("an expected failure was logged")
		}
	})

	t.Run("the server slug is encoded like encodeURIComponent", func(t *testing.T) {
		e := newEnv(t)
		e.dir.servers = []app.PersonalServer{serverOf("we ird!'()*~é+&", "W", catalog.AuthOAuthUser)}
		state := e.start(t, "alice", "we%20ird%21%27%28%29%2A~%C3%A9%2B%26")
		want := "/connections?connected=we%20ird!'()*~%C3%A9%2B%26"
		if loc := e.callback(t, "alice", "state="+state+"&code=C1"); loc != want {
			t.Fatalf("%q, want %q", loc, want)
		}
	})
}

func TestCallbackUnexpectedErrorsRedirectAndLogNothingSensitive(t *testing.T) {
	const leak = "pq: password=hunter2 for STATEMARK-1 CODEMARK"
	for name, setup := range map[string]func(e *env){
		"the store fails":   func(e *env) { e.pending.failErr = errors.New(leak) },
		"the session fails": func(e *env) { e.sessionErr = errors.New(leak) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			state := e.start(t, "alice", "gitea")
			setup(e)
			if loc := e.callback(t, "alice", "state="+state+"&code=CODEMARK"); loc != "/connections?error=internal" {
				t.Fatal(loc)
			}
			logged := e.logs.String()
			if strings.Count(logged, "\n") != 1 || !strings.Contains(logged, `"level":"ERROR"`) {
				t.Fatalf("want exactly one error record, got %q", logged)
			}
			for _, secret := range []string{"hunter2", "STATEMARK", "CODEMARK", leak, "code="} {
				if strings.Contains(logged, secret) {
					t.Fatalf("the log carries %q: %s", secret, logged)
				}
			}
		})
	}
}

func TestCallbackWorksWithTheAccessLogAndNeverLogsItsQuery(t *testing.T) {
	e := newEnv(t)
	state := e.start(t, "alice", "gitea")
	var logs strings.Builder
	h := httpx.RequestID(slog.New(slog.NewJSONHandler(&logs, nil)))(e.h)
	req := httptest.NewRequest("GET", "/api/connections/callback?state="+state+"&code=CODEMARK", nil)
	req.Header.Set("Cookie", "sid=alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 302 {
		t.Fatalf("status %d", rec.Code)
	}
	got := logs.String()
	if !strings.Contains(got, `"path":"/api/connections/callback"`) || strings.Contains(got, "CODEMARK") || strings.Contains(got, state) || strings.Contains(got, "state=") {
		t.Fatalf("access log: %s", got)
	}
}

func TestCallbackRouteIsUnguardedButOnlyItself(t *testing.T) {
	e := newEnv(t)
	// Reaches the callback handler (not the session guard, not a {server} handler).
	if loc := e.callback(t, "", ""); loc != "/connections?error=unauthenticated" {
		t.Fatal(loc)
	}
	// HEAD is served by the GET route.
	r := e.do(t, "", "HEAD", "/api/connections/callback", "")
	if r.Status != 302 || r.Header.Get("Location") != "/connections?error=unauthenticated" {
		t.Fatalf("HEAD: %d %q", r.Status, r.Header.Get("Location"))
	}
	// Another method on the path and paths below it stay behind the guard.
	expect(t, "POST", e.do(t, "", "POST", "/api/connections/callback", ""), 401, errSignIn)
	expect(t, "below", e.do(t, "", "GET", "/api/connections/callback/x", ""), 401, errSignIn)
	// A signed-in person gets the ordinary not found for an unknown method and path.
	expect(t, "POST signed in", e.do(t, "alice", "POST", "/api/connections/callback", ""), 404, errBody("not_found", "not found"))
	expect(t, "GET below signed in", e.do(t, "alice", "GET", "/api/connections/callback/x", ""), 404, errBody("not_found", "not found"))
	// A server called "callback": the {server} routes still work for it.
	e.dir.servers = append(e.dir.servers, serverOf("callback", "Callback", catalog.AuthPATUser))
	expect(t, "PAT", e.do(t, "alice", "PUT", "/api/connections/callback/pat", `{"token":"t-1"}`), 204, "")
	expect(t, "delete", e.do(t, "alice", "DELETE", "/api/connections/callback", ""), 204, "")
	if e.store.has("alice", "callback") {
		t.Fatal("not deleted")
	}
}

func TestUnexpectedErrorsOnJSONRoutes(t *testing.T) {
	const leak = "pq: password=hunter2"
	e := newEnv(t)
	e.store.failGet = errors.New(leak)
	r := e.do(t, "alice", "GET", "/api/connections", "")
	expect(t, "list", r, 500, errBody("internal", "Internal error"))
	e = newEnv(t)
	e.store.failDelete = errors.New(leak)
	expect(t, "delete", e.do(t, "alice", "DELETE", "/api/connections/gitea", ""), 500, errBody("internal", "Internal error"))
	e = newEnv(t)
	e.dir.listErr = errors.New(leak)
	expect(t, "start", e.do(t, "alice", "POST", "/api/connections/gitea/start", ""), 500, errBody("internal", "Internal error"))
	expect(t, "pat", e.do(t, "alice", "PUT", "/api/connections/tracker/pat", `{"token":"t"}`), 500, errBody("internal", "Internal error"))
}

func TestWritesSurviveAClientThatWentAway(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := contextWithCancel()
	cancel()
	req := httptest.NewRequest("PUT", "/api/connections/tracker/pat", strings.NewReader(`{"token":"t-1"}`)).WithContext(ctx)
	req.Header.Set("Cookie", "sid=alice")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 204 || !e.store.has("alice", "tracker") || e.store.cancelledWrites != 0 {
		t.Fatalf("%d stored=%v cancelled writes=%d", rec.Code, e.store.has("alice", "tracker"), e.store.cancelledWrites)
	}
}
