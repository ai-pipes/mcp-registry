//go:build compat

package compat_test

// Platform scenarios: security headers, CSRF, body limit, static files and the SPA fallback,
// migrations, the secrets key check and the first start without OIDC. All of them go through
// HTTP, the CLI, the container's exit code and log, and SQL.
//
// Source cases that are not ported (they need the app's internals, not its outside):
//   - v1.0.1:apps/server/test/http/platform.int.test.ts "/healthz answers 503 when the database is down": the stack's database
//     is shared by every request; stopping it would be a different scenario (and a restart one).
//   - v1.0.1:apps/server/test/http/platform.int.test.ts "caps request bodies" without Content-Length (streaming): the harness
//     client always sends Content-Length; the Content-Length case is ported.
//   - v1.0.1:apps/server/test/http/platform.int.test.ts "logs method, path and status, never headers": pinned in
//     TestPlatformAccessLog below instead (through docker logs).
//   - v1.0.1:apps/server/test/http/csp.test.ts: a unit test of the middleware; the same value is asserted end to end on
//     five kinds of response in TestPlatformSecurityHeaders.
//   - v1.0.1:apps/server/test/platform/migrate.int.test.ts "rolls back a failing migration", "applies a new migration on top of
//     the known ones": they hand migrate() a directory of extra migration files, which a built
//     image does not accept. The migrations and key-check tests of the Go server cover them.
//   - v1.0.1:apps/server/test/platform/key-check.int.test.ts "two concurrent first checks both succeed and leave one row" and
//     "keeps working data from before the check existed" (with a sealed server secret): racing
//     two app processes on one empty database is not reproducible from the outside. The
//     "first start after the check did not exist" case is ported by deleting the row.
//   - v1.0.1:apps/server/test/platform/bootstrap.int.test.ts "serves /mcp with a token when OIDC is not configured": covered by
//     the control of TestPlatformBearerNeverOpensTheWebAPI (a token opens /mcp, tools/list shows
//     four tools) on the shared stack, which has no OIDC, and by /api/auth/methods in
//     TestFirstStartAnnouncementNeedsAnEmptyDatabaseAndNoOIDC (oidc=true there; the shared
//     stack's oidc=false is asserted below in TestPlatformMethodsWithoutOIDC).
//   - v1.0.1:apps/server/test/platform/bootstrap.int.test.ts "logs and continues when the announcement fails": needs a failing
//     user repository. "prints nothing when other people exist" and "prints nothing and creates
//     nobody when OIDC is configured" are ported (TestFirstStartAnnouncement, subtests).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

const (
	strictCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'"

	immutableCache = "public, max-age=31536000, immutable"
	restBodyLimit  = 1 << 20

	firstStartMsg   = "first start: open this one-time link within 15 minutes to sign in as admin"
	keyMismatchMsg  = `SECRETS_KEY does not match the data in this database; restore the original key (stored secrets cannot be read with this one). See "Recovering from a key mismatch" in the README.`
	keyCreatedMsg   = "secrets key check created: SECRETS_KEY is now verified at every start"
	keyOldOnlyMsg   = "the secrets key check still opens with SECRETS_KEY_OLD only: run rotate-secrets-key"
	newerDBMigrName = "9999_from_the_future.sql"
)

func platformImage() string { return harness.DefaultImage() }

// securityHeaders is the required set: every response carries each of these, once, with exactly
// this value (the owner's decision; the Go server must send the same). The reference image sends
// five more that hono's secureHeaders() adds by default and that are NOT required any more (and
// not asserted absent): Origin-Agent-Cluster, X-Dns-Prefetch-Control, X-Download-Options,
// X-Permitted-Cross-Domain-Policies and X-Xss-Protection. `Server` and `X-Powered-By` must be
// absent (assertSecurityHeaders): a deliberate product security requirement, since they disclose
// the stack.
var securityHeaders = map[string]string{
	"Content-Security-Policy":      strictCSP,
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Referrer-Policy":              "no-referrer",
	"Strict-Transport-Security":    "max-age=15552000; includeSubDomains",
	"X-Content-Type-Options":       "nosniff",
	"X-Frame-Options":              "SAMEORIGIN",
}

var requestID = regexp.MustCompile(`^[0-9a-f-]{36}$`)

func assertSecurityHeaders(t *testing.T, what string, r harness.Response, wantRequestID bool) {
	t.Helper()
	for name, want := range securityHeaders {
		if got := r.Header.Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("%s: header %s = %q, want exactly %q", what, name, got, want)
		}
	}
	if got := r.Header.Get("Content-Security-Policy"); strings.Contains(got, "style-src-elem") || strings.Contains(got, "sha256-") || strings.Contains(got, "unsafe-") {
		t.Errorf("%s: CSP is relaxed: %q", what, got)
	}
	// A product security requirement (they disclose the stack), not a "helpful header" absence.
	for _, name := range []string{"Server", "X-Powered-By"} {
		if got := r.Header.Values(name); len(got) != 0 {
			t.Errorf("%s: header %s = %q, must not be sent", what, name, got)
		}
	}
	id := r.Header.Get("X-Request-Id")
	if wantRequestID && !requestID.MatchString(id) {
		t.Errorf("%s: X-Request-Id = %q", what, id)
	}
}

func errorCode(t *testing.T, r harness.Response) string {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	r.JSON(t, "ErrorBody", &e)
	return e.Error.Code + "|" + e.Error.Message
}

func TestPlatformSecurityHeaders(t *testing.T) {
	s := harness.Shared(t)
	anon := s.Anonymous()
	admin := s.Admin(t)

	index := anon.Do(t, "GET", "/", nil, "Accept: text/html")
	asset := regexp.MustCompile(`/assets/[^"']+\.js`).FindString(string(index.Body))
	if asset == "" {
		t.Fatalf("index.html references no /assets/*.js file: %q", index.Body)
	}

	cases := []struct {
		name   string
		req    func() harness.Response
		status int
		// noRequestID: the response is not required to carry X-Request-Id (1.0.0: a foreign Host is
		// refused by the MCP transport before the access-log middleware, and the image sends none
		// there; neither its presence nor its absence is asserted). Decided (request-id-on-refusals):
		// every response carries it, that refusal included.
		noRequestID bool
	}{
		{"index", func() harness.Response { return index }, 200, false},
		// /healthz is asserted here again: the admin above is signed in before the table runs, so a
		// stall right after the boot has ended (the same mitigation as TestPlatformHealthz).
		{"healthz", func() harness.Response { return anon.Do(t, "GET", "/healthz", nil) }, 200, false},
		{"api public 200", func() harness.Response { return anon.Do(t, "GET", "/api/auth/methods", nil) }, 200, false},
		{"api 401", func() harness.Response { return anon.Do(t, "GET", "/api/auth/me", nil) }, 401, false},
		{"api 200", func() harness.Response { return admin.Do(t, "GET", "/api/auth/me", nil) }, 200, false},
		{"404 text", func() harness.Response { return anon.Do(t, "GET", "/no-such-page", nil) }, 404, false},
		{"api 404", func() harness.Response { return anon.Do(t, "GET", "/api/no-such", nil) }, 404, false},
		{"static asset", func() harness.Response { return anon.Do(t, "GET", asset, nil) }, 200, false},
		{"static asset 404", func() harness.Response { return anon.Do(t, "GET", "/assets/no-such.js", nil) }, 404, false},
		{"spa route", func() harness.Response { return anon.Do(t, "GET", "/catalog", nil, "Accept: text/html") }, 200, false},
		{"csrf 403", func() harness.Response { return anon.Do(t, "POST", "/api/tokens", nil, "Origin:") }, 403, false},
		{"mcp 401", func() harness.Response { return anon.Do(t, "GET", "/mcp", nil) }, 401, false},
		{"413", func() harness.Response {
			return anon.DoExpectingEarlyRefusal(t, "POST", "/api/tokens", make([]byte, restBodyLimit+1), "Content-Type: application/json")
		}, 413, false},
		{"foreign host on mcp", func() harness.Response { return anon.Do(t, "GET", "/mcp", nil, "Host: evil.example") }, 403, !s.Fixed(harness.DevRequestIDOnRefusals)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.req()
			if r.Status != c.status {
				t.Fatalf("status %d, want %d (body %.200q)", r.Status, c.status, r.Body)
			}
			assertSecurityHeaders(t, c.name, r, !c.noRequestID)
		})
	}

	t.Run("every response has its own request id", func(t *testing.T) {
		a := anon.Do(t, "GET", "/api/auth/methods", nil).Header.Get("X-Request-Id")
		b := anon.Do(t, "GET", "/api/auth/methods", nil).Header.Get("X-Request-Id")
		if a == b {
			t.Fatalf("two responses share the request id %q", a)
		}
	})
}

// TestPlatformHealthz is the canary scenario for /healthz (the self-check distorts it); the
// security-headers table and the ready-before-serving branch assert it too. It runs on the shared stack, which an earlier test of the package has booted long
// before in a full run; with -run on this test alone the stack may have just started, and the
// reference image can then answer 503 for a moment (decision ready-before-serving). No other
// scenario asserts /healthz right after a start: they use Stack.Answers.
func TestPlatformHealthz(t *testing.T) {
	s := harness.Shared(t)
	// A sign-in is real work (two CLI runs and a login through the app's event loop), so by the
	// time it is done a stall that began right after the boot has ended. The probe is asserted
	// strictly after it, with no retry.
	s.Admin(t)
	r := s.Anonymous().Do(t, "GET", "/healthz", nil)
	if r.Status != 200 || string(r.Body) != "ok" {
		t.Fatalf("healthz: %d %q", r.Status, r.Body)
	}
	assertMediaType(t, "healthz", r.Header.Get("Content-Type"), "text/plain", "utf-8")
	if t.Failed() {
		t.FailNow()
	}
}

func TestPlatformAccessLog(t *testing.T) {
	s := harness.Shared(t)
	marker := harness.Unique("sk_secret-token")
	query := "probe=" + harness.Unique("p")
	r := s.Anonymous().Do(t, "GET", "/api/auth/methods?"+query, nil, "Authorization: Bearer "+marker)
	id := r.Header.Get("X-Request-Id")
	if r.Status != 200 || !requestID.MatchString(id) {
		t.Fatalf("probe: %d, X-Request-Id %q", r.Status, id)
	}
	// Wait for exactly this request's record (the readiness probes leave records too).
	var rec map[string]any
	harness.Eventually(t, "the probe's record (reqId "+id+") in the access log", func() bool {
		recs, _ := logRecords(t, s.Logs(t))
		for _, x := range recs {
			if x["reqId"] == id {
				rec = x
				return true
			}
		}
		return false
	})
	// The record is identified by its request id; its message text is the logger's.
	if logLevel(rec) != "info" || rec["method"] != "GET" || rec["path"] != "/api/auth/methods" || rec["status"] != float64(200) {
		t.Fatalf("access log record: %v", rec)
	}
	if ms, ok := rec["ms"].(float64); !ok || ms < 0 {
		t.Fatalf("access log record has no ms: %v", rec)
	}
	if ip, ok := rec["ip"].(string); !ok || ip == "" {
		t.Fatalf("access log record has no ip: %v", rec)
	}
	for _, name := range []string{"headers", "query", "authorization", "req", "url"} {
		if _, ok := rec[name]; ok {
			t.Fatalf("access log record has a %q field: %v", name, rec)
		}
	}
	logs := s.Logs(t) // read after the record is there: the negative checks cover this request's line
	if strings.Contains(logs, marker) {
		t.Fatal("the app log contains the Authorization header value")
	}
	if strings.Contains(logs, query) || strings.Contains(logs, "probe=") {
		t.Fatal("the app log contains the query string")
	}
}

func TestPlatformCSRF(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	body := map[string]string{"name": harness.Unique("csrf")}
	const want = "csrf|cross-origin request rejected"

	t.Run("without Origin", func(t *testing.T) {
		r := admin.Do(t, "POST", "/api/tokens", body, "Origin:")
		if r.Status != 403 || errorCode(t, r) != want {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})
	t.Run("foreign Origin", func(t *testing.T) {
		for _, origin := range []string{"http://evil.example", "https://localhost", s.BaseURL + ".evil.example", "null"} {
			r := admin.Do(t, "POST", "/api/tokens", body, "Origin: "+origin)
			if r.Status != 403 || errorCode(t, r) != want {
				t.Fatalf("Origin %s: %d %s", origin, r.Status, r.Body)
			}
		}
	})
	t.Run("every mutating method is covered", func(t *testing.T) {
		for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			r := admin.Do(t, m, "/api/tokens", nil, "Origin: http://evil.example")
			if r.Status != 403 || errorCode(t, r) != want {
				t.Fatalf("%s: %d %s", m, r.Status, r.Body)
			}
		}
	})
	t.Run("refused request changes nothing", func(t *testing.T) {
		var before, after []struct{ Name string }
		admin.Do(t, "GET", "/api/tokens", nil).JSON(t, "TokenSummary[]", &before)
		admin.Do(t, "POST", "/api/tokens", body, "Origin: http://evil.example")
		admin.Do(t, "GET", "/api/tokens", nil).JSON(t, "TokenSummary[]", &after)
		if len(before) != len(after) {
			t.Fatalf("the refused POST created a token: %d -> %d", len(before), len(after))
		}
	})
	t.Run("safe methods need no Origin", func(t *testing.T) {
		r := admin.Do(t, "GET", "/api/auth/me", nil, "Origin:")
		if r.Status != 200 {
			t.Fatalf("GET without Origin: %d %s", r.Status, r.Body)
		}
	})
	t.Run("the right Origin is accepted (control)", func(t *testing.T) {
		r := admin.Do(t, "POST", "/api/tokens", body)
		if r.Status != 201 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		var created struct{ Token string }
		r.JSON(t, "CreateTokenResponse", &created)
	})
	t.Run("Bearer does not exempt a mutation from the Origin check", func(t *testing.T) {
		token := admin.Token(t, harness.Unique("tok"))
		r := s.Anonymous().Do(t, "POST", "/api/x", nil, "Origin:", "Authorization: Bearer "+token)
		if r.Status != 403 || errorCode(t, r) != want {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})
	t.Run("an unknown /api path with the right Origin is a 404 (control)", func(t *testing.T) {
		r := admin.Do(t, "POST", "/api/x", nil)
		if r.Status != 404 || errorCode(t, r) != "not_found|not found" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})
}

func TestPlatformBearerNeverOpensTheWebAPI(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	token := admin.Token(t, harness.Unique("tok"))
	anon := s.Anonymous()
	const want = "unauthenticated|tokens do not work on the web API"

	for _, c := range []struct{ method, path string }{
		{"GET", "/api/auth/me"},
		{"GET", "/api/tokens"},
		{"GET", "/api/servers"},
		{"GET", "/api/catalog"},
		{"POST", "/api/tokens"},
		{"GET", "/api/version"},
	} {
		r := anon.Do(t, c.method, c.path, nil, "Authorization: Bearer "+token)
		if r.Status != 401 {
			t.Errorf("%s %s with a valid token: %d %s", c.method, c.path, r.Status, r.Body)
			continue
		}
		if errorCode(t, r) != want {
			t.Errorf("%s %s: %s", c.method, c.path, r.Body)
		}
	}

	t.Run("the same token opens /mcp (control)", func(t *testing.T) {
		res, err := s.MCP(t, token).ListTools(context.Background(), nil)
		if err != nil || len(res.Tools) != 4 {
			t.Fatalf("tools/list: %v %v", res, err)
		}
	})
	t.Run("a bad token on /mcp is 401 with a challenge", func(t *testing.T) {
		r := anon.Do(t, "GET", "/mcp", nil, "Authorization: Bearer sk_nope")
		if r.Status != 401 || r.Header.Get("Www-Authenticate") != "Bearer" {
			t.Fatalf("%d %v %s", r.Status, r.Header, r.Body)
		}
	})
}

// The server answers 413 and closes the connection without reading the rest of the body; the
// oversized requests go through DoExpectingEarlyRefusal, which reads the answer whatever
// happened to the write; a request that must succeed (exactly the limit) goes through plain Do.
func TestPlatformBodyLimit(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	const want = "limit_exceeded|request body is too large"

	padded := func(n int) []byte {
		prefix := fmt.Sprintf(`{"name":%q}`, harness.Unique("big"))
		return []byte(prefix + strings.Repeat(" ", n-len(prefix)))
	}
	t.Run("a body over 1 MiB is refused with 413", func(t *testing.T) {
		r := admin.DoExpectingEarlyRefusal(t, "POST", "/api/tokens", padded(restBodyLimit+1), "Content-Type: application/json")
		if !strings.Contains(r.Sent.Get("Cookie"), "mcp_session=") {
			t.Fatalf("the signed-in request did not carry the session cookie (Cookie names: %v)", cookieNames(r.Sent.Get("Cookie")))
		}
		if r.Status != 413 || errorCode(t, r) != want {
			t.Fatalf("%d %.200s", r.Status, r.Body)
		}
	})
	t.Run("the limit applies before sign-in", func(t *testing.T) {
		r := s.Anonymous().DoExpectingEarlyRefusal(t, "POST", "/api/tokens", padded(restBodyLimit+1), "Content-Type: application/json")
		if r.Sent.Get("Cookie") != "" {
			t.Fatalf("the anonymous request carried a cookie (names: %v)", cookieNames(r.Sent.Get("Cookie")))
		}
		if r.Status != 413 || errorCode(t, r) != want {
			t.Fatalf("%d %.200s", r.Status, r.Body)
		}
	})
	t.Run("a body of exactly 1 MiB is not refused for its size (control)", func(t *testing.T) {
		r := admin.Do(t, "POST", "/api/tokens", padded(restBodyLimit), "Content-Type: application/json")
		if r.Status != 201 {
			t.Fatalf("%d %.200s", r.Status, r.Body)
		}
		var created struct{ Token string }
		r.JSON(t, "CreateTokenResponse", &created)
	})
	t.Run("a small body is accepted (control)", func(t *testing.T) {
		if r := admin.Do(t, "POST", "/api/tokens", map[string]string{"name": harness.Unique("small")}); r.Status != 201 {
			t.Fatalf("%d %.200s", r.Status, r.Body)
		}
	})
}

func TestPlatformStaticFilesAndSPA(t *testing.T) {
	s := harness.Shared(t)
	anon := s.Anonymous()
	const html = "Accept: text/html"

	index := anon.Do(t, "GET", "/", nil, html)
	// Reference 1.0.1: the shell is revalidated, like the client routes served as the SPA fallback.
	if cc := index.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("GET /: Cache-Control %q, want no-cache", cc)
	}
	if index.Status != 200 || !strings.Contains(string(index.Body), `<div id="root">`) {
		t.Fatalf("GET / with Accept text/html: %d %.200s", index.Status, index.Body)
	}
	assertMediaType(t, "GET /", index.Header.Get("Content-Type"), "text/html", "utf-8")
	assets := regexp.MustCompile(`/assets/[^"']+`).FindAllString(string(index.Body), -1)
	if len(assets) < 2 {
		t.Fatalf("index.html references %v", assets)
	}

	t.Run("hashed assets are cached forever", func(t *testing.T) {
		for _, a := range assets {
			r := anon.Do(t, "GET", a, nil)
			if r.Status != 200 || r.Header.Get("Cache-Control") != immutableCache {
				t.Errorf("%s: %d Cache-Control %q", a, r.Status, r.Header.Get("Cache-Control"))
			}
			if len(r.Body) == 0 {
				t.Errorf("%s: empty body", a)
			}
		}
		js := anon.Do(t, "GET", firstWithSuffix(assets, ".js"), nil)
		if ct := js.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("js Content-Type %q", ct)
		}
		css := anon.Do(t, "GET", firstWithSuffix(assets, ".css"), nil)
		if ct := css.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
			t.Errorf("css Content-Type %q", ct)
		}
	})
	t.Run("a missing asset is a 404 that is not cached", func(t *testing.T) {
		r := anon.Do(t, "GET", "/assets/no-such-file-abc123.js", nil)
		// Not cached as the hashed assets are: whatever else the response says about caching is
		// not pinned.
		if cc := r.Header.Get("Cache-Control"); r.Status != 404 || strings.Contains(cc, "immutable") || strings.Contains(cc, "max-age=31536000") || string(r.Body) != "Not found" {
			t.Fatalf("%d Cache-Control %q body %q", r.Status, cc, r.Body)
		}
	})
	t.Run("a client-side route gets index.html, never cached", func(t *testing.T) {
		for _, p := range []string{"/catalog", "/servers/gitea", "/admin/servers/new", "/connections"} {
			r := anon.Do(t, "GET", p, nil, html)
			if r.Status != 200 || r.Header.Get("Cache-Control") != "no-cache" ||
				!strings.Contains(string(r.Body), `<div id="root">`) {
				t.Errorf("%s: %d Cache-Control %q %.100s", p, r.Status, r.Header.Get("Cache-Control"), r.Body)
			}
			assertMediaType(t, p, r.Header.Get("Content-Type"), "text/html", "utf-8")
		}
		head := anon.Do(t, "HEAD", "/catalog", nil, html)
		if head.Status != 200 || len(head.Body) != 0 {
			t.Errorf("HEAD /catalog: %d, %d body bytes", head.Status, len(head.Body))
		}
	})
	t.Run("without Accept text/html a route is a 404 (control)", func(t *testing.T) {
		for _, accept := range []string{"", "Accept: application/json", "Accept: */*"} {
			var hdr []string
			if accept != "" {
				hdr = append(hdr, accept)
			}
			r := anon.Do(t, "GET", "/catalog", nil, hdr...)
			if r.Status != 404 || string(r.Body) != "Not found" {
				t.Errorf("%q: %d %.100s", accept, r.Status, r.Body)
			}
		}
	})
	t.Run("a path with a file extension is a 404, not the shell", func(t *testing.T) {
		r := anon.Do(t, "GET", "/missing.js", nil, html)
		if r.Status != 404 || string(r.Body) != "Not found" {
			t.Fatalf("%d %.100s", r.Status, r.Body)
		}
	})
	t.Run("only GET and HEAD get the shell", func(t *testing.T) {
		r := anon.Do(t, "POST", "/catalog", nil, html)
		if r.Status != 404 {
			t.Fatalf("POST /catalog: %d %.100s", r.Status, r.Body)
		}
	})
	t.Run("/api, /mcp and /healthz are never the shell", func(t *testing.T) {
		api := anon.Do(t, "GET", "/api/unknown", nil, html)
		if api.Status != 404 || errorCode(t, api) != "not_found|not found" ||
			!strings.HasPrefix(api.Header.Get("Content-Type"), "application/json") {
			t.Errorf("/api/unknown: %d %s", api.Status, api.Body)
		}
		mcp := anon.Do(t, "GET", "/mcp", nil, html)
		if mcp.Status != 401 || errorCode(t, mcp) != "unauthenticated|bearer token required" ||
			strings.Contains(mcp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("/mcp: %d %s", mcp.Status, mcp.Body)
		}
		mcpSub := anon.Do(t, "GET", "/mcp/sub", nil, html)
		// /mcp/sub is not a route of the MCP endpoint: the plain-text 404, not the shell.
		if mcpSub.Status != 404 || string(mcpSub.Body) != "Not found" {
			t.Errorf("/mcp/sub: %d %.100s", mcpSub.Status, mcpSub.Body)
		}
		hz := anon.Do(t, "GET", "/healthz/x", nil, html)
		if hz.Status != 404 || strings.Contains(string(hz.Body), `<div id="root">`) {
			t.Errorf("/healthz/x: %d %.100s", hz.Status, hz.Body)
		}
	})
}

// --- migrations and the secrets key check -------------------------------------------------

func newKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func keyForms(t *testing.T, key string) []string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		t.Fatal(err)
	}
	return []string{key, strings.TrimRight(key, "="), hex.EncodeToString(raw)}
}

func assertNoKey(t *testing.T, logs string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		for _, form := range keyForms(t, k) {
			if strings.Contains(logs, form) {
				t.Fatalf("the log contains key material (%d characters of a key form)", len(form))
			}
		}
	}
}

// cookieNames lists the names of a Cookie header, never its values.
func cookieNames(header string) []string {
	var names []string
	for _, kv := range strings.Split(header, ";") {
		name, _, _ := strings.Cut(strings.TrimSpace(kv), "=")
		names = append(names, name)
	}
	return names
}

func keyCheckBytes(t *testing.T, s *harness.Stack) []byte {
	t.Helper()
	var b []byte
	if err := s.DB.QueryRow(context.Background(), "select sealed from platform.key_check").Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// assertKeyCreatedOnce: one info-level record that the check was recorded, from this container run.
func assertKeyCreatedOnce(t *testing.T, recs []map[string]any) {
	t.Helper()
	created := recordsWithMsg(recs, keyCreatedMsg)
	if len(created) != 1 || logLevel(created[0]) != "info" {
		t.Fatalf("key-check records: %v", created)
	}
}

// healthy asserts that the app answers after a start (not /healthz: see Stack.Answers).
func healthy(t *testing.T, s *harness.Stack) {
	t.Helper()
	s.Answers(t)
}

// TestPlatformMigrationsAndKeyCheck runs on one stack with a key the test knows; the subtests
// depend on each other and run in order.
func TestPlatformMigrationsAndKeyCheck(t *testing.T) {
	key := newKey(t)
	s := harness.Start(t, harness.Options{Env: map[string]string{"SECRETS_KEY": key}})
	image := platformImage()
	var migrations int

	t.Run("migrations create the platform schema", func(t *testing.T) {
		rows, err := s.DB.Query(context.Background(),
			"select table_name from information_schema.tables where table_schema = 'platform' order by 1")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			got = append(got, n)
		}
		if strings.Join(got, ",") != "audit_log,key_check,outbox,schema_migrations" {
			t.Fatalf("platform tables: %v", got)
		}
		if countSQL(t, s, "select count(*) from platform.schema_migrations where name = '0001_platform.sql'") != 1 {
			t.Fatal("0001_platform.sql is not recorded")
		}
		migrations = countSQL(t, s, "select count(*) from platform.schema_migrations")
		for _, schema := range []string{"identity", "catalog", "connections", "search", "gateway", "platform"} {
			if countSQL(t, s, "select count(*) from information_schema.schemata where schema_name = $1", schema) != 1 {
				t.Errorf("schema %s is missing", schema)
			}
		}
	})

	t.Run("the first start records the key check", func(t *testing.T) {
		if n := countSQL(t, s, "select count(*) from platform.key_check"); n != 1 {
			t.Fatalf("key_check rows after the first start: %d", n)
		}
		recs, _ := logRecords(t, s.Logs(t))
		if n := len(recordsAt(recs, "fatal")); n != 0 {
			t.Fatalf("fatal records on a healthy start: %d", n)
		}
		// This container's log is the first run only: exactly one info record about the check.
		assertKeyCreatedOnce(t, recs)
	})

	t.Run("a restart with the same key applies nothing and keeps the check", func(t *testing.T) {
		before := keyCheckBytes(t, s)
		s.SwapImage(t, image, "")
		if n := countSQL(t, s, "select count(*) from platform.schema_migrations"); n != migrations {
			t.Fatalf("migrations %d -> %d after a restart", migrations, n)
		}
		if n := countSQL(t, s, "select count(*) from platform.key_check"); n != 1 {
			t.Fatalf("key_check rows: %d", n)
		}
		if string(keyCheckBytes(t, s)) != string(before) {
			t.Fatal("the key check was rewritten by a restart")
		}
		recs, _ := logRecords(t, s.Logs(t))
		if n := len(recordsWithMsg(recs, keyCreatedMsg)); n != 0 {
			t.Fatalf("%d key-check records on a restart with a matching key", n)
		}
		if len(recordsAt(recs, "fatal")) != 0 {
			t.Fatalf("fatal records after a restart with the same key: %v", recs)
		}
		// local-admin has not signed in on this stack, so the first-start link is the only warning.
		for _, r := range recordsAt(recs, "warn") {
			if r["msg"] != firstStartMsg {
				t.Fatalf("unexpected warning after a restart with the same key: %v", r)
			}
		}
	})

	t.Run("a database from before the check gets it on the next start", func(t *testing.T) {
		if _, err := s.DB.Exec(context.Background(), "delete from platform.key_check"); err != nil {
			t.Fatal(err)
		}
		s.SwapImage(t, image, "") // a new container: its log holds this run only
		if n := countSQL(t, s, "select count(*) from platform.key_check"); n != 1 {
			t.Fatalf("key_check rows: %d", n)
		}
		recs, _ := logRecords(t, s.Logs(t))
		assertKeyCreatedOnce(t, recs)
	})

	t.Run("another key: exit 1, one fatal record, no key in the log, check unchanged", func(t *testing.T) {
		before := keyCheckBytes(t, s)
		wrong := newKey(t)
		code, logs := s.SwapImageExpectingExit(t, image, "", map[string]string{"SECRETS_KEY": wrong})
		if code != 1 {
			t.Fatalf("exit code %d, logs: %s", code, logs)
		}
		recs, _ := logRecords(t, logs)
		fatal := recordsAt(recs, "fatal")
		if len(fatal) != 1 || fatal[0]["msg"] != keyMismatchMsg {
			t.Fatalf("fatal records: %v", fatal)
		}
		assertNoKey(t, logs, key, wrong)
		if string(keyCheckBytes(t, s)) != string(before) || countSQL(t, s, "select count(*) from platform.key_check") != 1 {
			t.Fatal("a refused start changed the key check")
		}
		s.SwapImage(t, image, "") // the right key brings the app back
		healthy(t, s)
	})

	t.Run("the old key alone: healthy, one warning; rotate-secrets-key re-seals the check", func(t *testing.T) {
		next := newKey(t)
		s.SwapImage(t, image, "", map[string]string{"SECRETS_KEY": next, "SECRETS_KEY_OLD": key})
		healthy(t, s)
		recs, _ := logRecords(t, s.Logs(t))
		warn := recordsWithMsg(recs, keyOldOnlyMsg)
		if len(warn) != 1 || logLevel(warn[0]) != "warn" {
			t.Fatalf("old-key warnings: %v", warn)
		}
		assertNoKey(t, s.Logs(t), key, next)

		r := s.CLI(t, "", "rotate-secrets-key")
		if r.Code != 0 || !hasLine(r.Stdout, "key check: re-sealed") {
			t.Fatalf("rotate-secrets-key: %+v", r)
		}
		again := s.CLI(t, "", "rotate-secrets-key")
		if again.Code != 0 || !hasLine(again.Stdout, "key check: already current") {
			t.Fatalf("second rotate-secrets-key: %+v", again)
		}

		// With the new key alone the check opens: no warning, healthy.
		s.SwapImage(t, image, "", map[string]string{"SECRETS_KEY": next, "SECRETS_KEY_OLD": ""})
		healthy(t, s)
		recs, _ = logRecords(t, s.Logs(t))
		if n := len(recordsWithMsg(recs, keyOldOnlyMsg)); n != 0 {
			t.Fatalf("still warning about the old key after the rotation: %d", n)
		}
		// The old key no longer opens the check.
		code, logs := s.SwapImageExpectingExit(t, image, "", map[string]string{"SECRETS_KEY": key, "SECRETS_KEY_OLD": ""})
		if code != 1 {
			t.Fatalf("the old key alone: exit %d, logs %s", code, logs)
		}
		oldRecs, _ := logRecords(t, logs)
		if fatal := recordsAt(oldRecs, "fatal"); len(fatal) != 1 || fatal[0]["msg"] != keyMismatchMsg {
			t.Fatalf("fatal records with the old key alone: %v", fatal)
		}
		assertNoKey(t, logs, key, next)
		// Bring the app back with the new key alone and make it the stack's key.
		s.SwapImage(t, image, "", map[string]string{"SECRETS_KEY": next, "SECRETS_KEY_OLD": ""})
		key = next
	})

	t.Run("a database from a newer version is refused with one fatal record and nothing applied", func(t *testing.T) {
		if _, err := s.DB.Exec(context.Background(),
			"insert into platform.schema_migrations (name) values ($1)", newerDBMigrName); err != nil {
			t.Fatal(err)
		}
		before := countSQL(t, s, "select count(*) from platform.schema_migrations")
		code, logs := s.SwapImageExpectingExit(t, image, "", map[string]string{"SECRETS_KEY": key})
		if code != 1 {
			t.Fatalf("exit code %d, logs: %s", code, logs)
		}
		recs, _ := logRecords(t, logs)
		fatal := recordsAt(recs, "fatal")
		if len(fatal) != 1 {
			t.Fatalf("fatal records: %v", fatal)
		}
		msg, _ := fatal[0]["msg"].(string)
		if !strings.Contains(msg, "migrated by a newer version of mcp-registry (unknown migration: "+newerDBMigrName+")") ||
			!strings.Contains(msg, "restore the backup taken before the upgrade") {
			t.Fatalf("fatal message: %q", msg)
		}
		if n := countSQL(t, s, "select count(*) from platform.schema_migrations"); n != before {
			t.Fatalf("migration rows %d -> %d", before, n)
		}
		assertNoKey(t, logs, key)

		if _, err := s.DB.Exec(context.Background(),
			"delete from platform.schema_migrations where name = $1", newerDBMigrName); err != nil {
			t.Fatal(err)
		}
		s.SwapImage(t, image, "", map[string]string{"SECRETS_KEY": key})
		healthy(t, s) // the advisory lock was released: a normal start works again
	})
}

// --- first start without OIDC ---------------------------------------------------------------

func linkRecords(t *testing.T, logs string) []map[string]any {
	t.Helper()
	recs, _ := logRecords(t, logs)
	var out []map[string]any
	for _, r := range recs {
		if u, _ := r["url"].(string); strings.Contains(u, "#link=") {
			out = append(out, r)
		}
	}
	return out
}

func codeOfLink(t *testing.T, rec map[string]any) string {
	t.Helper()
	u, _ := rec["url"].(string)
	_, code, ok := strings.Cut(u, "#link=")
	if !ok || !strings.HasPrefix(code, "ll_") {
		t.Fatalf("the url field has no link code: %q", harness.Redact(u))
	}
	return code
}

func signInWith(t *testing.T, s *harness.Stack, code string) (*harness.Client, harness.Response) {
	t.Helper()
	c := s.Anonymous()
	return c, c.Do(t, "POST", "/api/auth/link", map[string]string{"code": code})
}

func TestFirstStartAnnouncement(t *testing.T) {
	s := harness.Start(t, harness.Options{}) // no OIDC, no user created by the test
	image := platformImage()

	first := linkRecords(t, s.Logs(t))
	if len(first) != 1 {
		t.Fatalf("link records on the first start: %d", len(first))
	}
	rec := first[0]
	if logLevel(rec) != "warn" || rec["msg"] != firstStartMsg {
		t.Fatalf("record: level %q msg %q", logLevel(rec), rec["msg"])
	}
	url, _ := rec["url"].(string)
	if !regexp.MustCompile("^" + regexp.QuoteMeta(s.BaseURL) + `/login#link=ll_[A-Za-z0-9_-]+$`).MatchString(url) {
		t.Fatalf("url field: %q", harness.Redact(url))
	}
	all, _ := logRecords(t, s.Logs(t))
	var warns int
	for _, r := range recordsAt(all, "warn") {
		if r["msg"] == firstStartMsg {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("first-start warn records: %d", warns)
	}
	if n := countSQL(t, s, "select count(*) from identity.users"); n != 1 {
		t.Fatalf("users after the first start: %d", n)
	}
	firstCode := codeOfLink(t, rec)

	t.Run("a restart before the admin signed in prints a new link and kills the old one", func(t *testing.T) {
		s.SwapImage(t, image, "")
		second := linkRecords(t, s.Logs(t))
		if len(second) != 1 || logLevel(second[0]) != "warn" || second[0]["msg"] != firstStartMsg {
			t.Fatalf("link records after the restart: %v", second)
		}
		secondCode := codeOfLink(t, second[0])
		if secondCode == firstCode {
			t.Fatal("the restart printed the same link code")
		}
		_, stale := signInWith(t, s, firstCode)
		if stale.Status != 401 || !strings.HasPrefix(errorCode(t, stale), "invalid_link|") {
			t.Fatalf("the old code: %d %s", stale.Status, stale.Body)
		}
		firstCode = secondCode
	})

	t.Run("the link signs in local-admin as an admin", func(t *testing.T) {
		c, r := signInWith(t, s, firstCode)
		if r.Status != 204 || len(r.Body) != 0 {
			t.Fatalf("POST /api/auth/link: %d %s", r.Status, r.Body)
		}
		var me struct {
			UserID string `json:"userId"`
			Name   string `json:"name"`
			Role   string `json:"role"`
		}
		c.Do(t, "GET", "/api/auth/me", nil).JSON(t, "MeResponse", &me)
		if me.Role != "admin" {
			t.Fatalf("me: %+v", me)
		}
		var sub string
		if err := s.DB.QueryRow(context.Background(), "select oidc_sub from identity.users where id::text = $1", me.UserID).Scan(&sub); err != nil || sub != "local-admin" {
			t.Fatalf("subject %q (%v)", sub, err)
		}
		// The code was one-time.
		_, again := signInWith(t, s, firstCode)
		if again.Status != 401 {
			t.Fatalf("a used code: %d %s", again.Status, again.Body)
		}
	})

	t.Run("after local-admin signed in a restart prints no link", func(t *testing.T) {
		s.SwapImage(t, image, "")
		recs, _ := logRecords(t, s.Logs(t))
		if n := len(linkRecords(t, s.Logs(t))); n != 0 {
			t.Fatalf("%d link records after the admin signed in", n)
		}
		for _, r := range recs {
			if r["msg"] == firstStartMsg {
				t.Fatalf("first-start record after the admin signed in: %v", r)
			}
		}
		if strings.Contains(s.Logs(t), "ll_") {
			t.Fatal("the log contains a link code")
		}
		if n := countSQL(t, s, "select count(*) from identity.users"); n != 1 {
			t.Fatalf("users: %d", n)
		}
	})
}

func TestFirstStartAnnouncementNeedsAnEmptyDatabaseAndNoOIDC(t *testing.T) {
	t.Run("nothing when another person exists", func(t *testing.T) {
		s := harness.Start(t, harness.Options{})
		// The first start created local-admin; a second person is added before the restart, and
		// the restart must neither announce a link nor create anybody.
		if n := countSQL(t, s, "select count(*) from identity.users"); n != 1 {
			t.Fatalf("users: %d", n)
		}
		if r := s.CLI(t, "", "create-user", "--sub", harness.Unique("bob"), "--name", "Bob"); r.Code != 0 {
			t.Fatalf("create-user: %+v", r)
		}
		s.SwapImage(t, platformImage(), "")
		if n := len(linkRecords(t, s.Logs(t))); n != 0 {
			t.Fatalf("%d link records although another person exists", n)
		}
		if n := countSQL(t, s, "select count(*) from identity.users"); n != 2 {
			t.Fatalf("users after the restart: %d, want 2", n)
		}
	})
	t.Run("nothing, and nobody created, with OIDC configured", func(t *testing.T) {
		idp := fakes.StartIdP(t)
		s := harness.Start(t, harness.Options{OIDC: idp})
		if n := len(linkRecords(t, s.Logs(t))); n != 0 {
			t.Fatalf("%d link records with OIDC configured", n)
		}
		if n := countSQL(t, s, "select count(*) from identity.users"); n != 0 {
			t.Fatalf("users created with OIDC configured: %d", n)
		}
		healthy(t, s)
		r := s.Anonymous().Do(t, "GET", "/api/auth/methods", nil)
		var m struct {
			OIDC bool `json:"oidc"`
		}
		r.JSON(t, "AuthMethods", &m)
		if !m.OIDC {
			t.Fatal("methods.oidc is false with OIDC configured")
		}
	})
}

func firstWithSuffix(list []string, suffix string) string {
	for _, v := range list {
		if strings.HasSuffix(v, suffix) {
			return v
		}
	}
	return "/assets/missing" + suffix
}

// hasLine reports whether text has a line equal to want (after trimming the line).
func hasLine(text, want string) bool {
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

func TestPlatformMethodsWithoutOIDC(t *testing.T) {
	var m struct {
		OIDC bool `json:"oidc"`
	}
	r := harness.Shared(t).Anonymous().Do(t, "GET", "/api/auth/methods", nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/auth/methods: %d %.200s", r.Status, r.Body)
	}
	r.JSON(t, "AuthMethods", &m)
	if m.OIDC {
		t.Fatal("methods.oidc is true without OIDC configured")
	}
}
