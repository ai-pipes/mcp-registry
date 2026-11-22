package httpx_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

func say(s string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(s)) })
}

func sayParam(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Param", httpx.PathParam(r, name))
	})
}

// tag is a middleware that appends its label to the X-Trace header before the next handler.
func tag(label string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("X-Trace", label)
			next.ServeHTTP(w, r)
		})
	}
}

func refuse(code int) httpx.Middleware {
	return func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
	}
}

func serveRaw(h http.Handler, method, rawPath string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/x", nil)
	// httptest.NewRequest parses the target; set the escaped form the way a real request carries it.
	u := *r.URL
	if i := strings.IndexByte(rawPath, '?'); i >= 0 {
		u.RawQuery = rawPath[i+1:]
		rawPath = rawPath[:i]
	}
	u.Path, u.RawPath = unescape(rawPath), rawPath
	r.URL = &u
	r.RequestURI = rawPath
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func unescape(p string) string {
	u, err := url.PathUnescape(p)
	if err != nil {
		return p
	}
	return u
}

func newTestRouter() http.Handler {
	return httpx.NewRouter([]httpx.Route{
		{Method: "GET", Path: "/api/tokens", Handler: say("list")},
		{Method: "POST", Path: "/api/tokens", Handler: say("create")},
		{Method: "DELETE", Path: "/api/tokens/{id}", Handler: sayParam("id")},
		{Method: "POST", Path: "/api/users/{id}/disabled", Handler: sayParam("id")},
		{Method: "GET", Path: "/api/users/me", Handler: say("me")},
		{Method: "GET", Path: "/api/users/{id}", Handler: sayParam("id")},
		{Method: "GET", Path: "/api/version", Handler: say("v")},
	}, nil, httpx.NotFound())
}

func TestRouterServesExactPathsAndParameters(t *testing.T) {
	h := newTestRouter()
	for _, tc := range []struct {
		method, path, want string
	}{
		{"GET", "/api/tokens", "list"},
		{"POST", "/api/tokens", "create"},
		{"DELETE", "/api/tokens/abc", "param=abc"},
		{"POST", "/api/users/u1/disabled", "param=u1"},
		{"GET", "/api/users/me", "me"}, // an exact path wins over a parameter
		{"GET", "/api/users/other", "param=other"},
		{"DELETE", "/api/tokens/a%2Fb", "param=a/b"},     // %2F stays inside the segment
		{"DELETE", "/api/tokens/a%20b", "param=a b"},     // decoded once
		{"DELETE", "/api/tokens/a%2520b", "param=a%20b"}, // not twice
		{"GET", "/api/version?x=1", "v"},
	} {
		w := serveRaw(h, tc.method, tc.path)
		got := w.Body.String()
		if p := w.Header().Get("X-Param"); p != "" {
			got = "param=" + p
		}
		if w.Code != 200 || got != tc.want {
			t.Errorf("%s %s: %d %q, want 200 %q", tc.method, tc.path, w.Code, got, tc.want)
		}
	}
}

func TestRouterAnswersNotFoundWithoutRedirectsOr405(t *testing.T) {
	h := newTestRouter()
	for _, tc := range []struct{ method, path string }{
		{"PUT", "/api/tokens"},       // another method on a known path: not 405
		{"GET", "/api/tokens/abc"},   // the path exists only for DELETE
		{"GET", "/api/tokens/"},      // trailing slash
		{"GET", "//api/version"},     // doubled slash
		{"GET", "/api/x/../version"}, // dot segments
		{"GET", "/api/./version"},
		{"DELETE", "/api/tokens/"},    // an empty parameter
		{"DELETE", "/api/tokens/a/b"}, // a parameter is one segment
		{"POST", "/api/users/u1"},
		{"GET", "/nope"},
	} {
		w := serveRaw(h, tc.method, tc.path)
		if w.Code != 404 || w.Header().Get("Location") != "" || w.Header().Get("Allow") != "" {
			t.Errorf("%s %s: %d Location=%q Allow=%q, want a plain 404", tc.method, tc.path, w.Code, w.Header().Get("Location"), w.Header().Get("Allow"))
		}
	}
	// The not-found handler is the one given: JSON under /api/.
	w := serveRaw(h, "PUT", "/api/tokens")
	if !strings.Contains(w.Body.String(), `"not_found"`) {
		t.Errorf("body %q", w.Body)
	}
}

func TestRouterServesHeadFromGet(t *testing.T) {
	h := newTestRouter()
	if w := serveRaw(h, "HEAD", "/api/version"); w.Code != 200 {
		t.Errorf("HEAD of a GET route: %d", w.Code)
	}
	if w := serveRaw(h, "HEAD", "/api/tokens/abc"); w.Code != 404 {
		t.Errorf("HEAD of a DELETE-only route: %d", w.Code)
	}
}

func TestRouterPrefixRunsForThePathAndEverythingUnderItButNotSiblings(t *testing.T) {
	h := httpx.NewRouter([]httpx.Route{
		{Method: "GET", Path: "/api/tokens", Handler: say("list")},
		{Method: "GET", Path: "/api/tokensx", Handler: say("sibling")},
	}, []httpx.Prefix{{Path: "/api/tokens", Middleware: []httpx.Middleware{refuse(401)}}}, httpx.NotFound())
	for path, want := range map[string]int{
		"/api/tokens":     401, // the prefix itself
		"/api/tokens/x/y": 401, // an unknown path under it gets the guard's answer, not 404
		"/api/tokens/abc": 401,
		"/api/tokensx":    200, // a sibling is not under the prefix
		"/api/tokensx/y":  404,
		"/api/other":      404,
	} {
		if w := serveRaw(h, "GET", path); w.Code != want {
			t.Errorf("GET %s: %d, want %d", path, w.Code, want)
		}
	}
}

func TestRouterPrefixesRunOutermostFirstInRegistrationOrder(t *testing.T) {
	h := httpx.NewRouter([]httpx.Route{
		{Method: "GET", Path: "/api/a/b", Handler: say("ok")},
	}, []httpx.Prefix{
		{Path: "/api", Middleware: []httpx.Middleware{tag("api1"), tag("api2")}},
		{Path: "/api/a", Middleware: []httpx.Middleware{tag("a")}},
		{Path: "/other", Middleware: []httpx.Middleware{tag("other")}},
	}, httpx.NotFound())
	w := serveRaw(h, "GET", "/api/a/b")
	if got := strings.Join(w.Header().Values("X-Trace"), ","); got != "api1,api2,a" {
		t.Errorf("order %q", got)
	}
}

func TestRouterRejectsInvalidRegistrations(t *testing.T) {
	ok := say("x")
	for name, tc := range map[string]struct {
		routes []httpx.Route
		want   string // a distinct text per cause
	}{
		"same name twice":       {[]httpx.Route{{Method: "GET", Path: "/a/{x}/b/{x}", Handler: ok}}, "appears twice"},
		"empty name":            {[]httpx.Route{{Method: "GET", Path: "/a/{}", Handler: ok}}, "whole segment"},
		"nested braces":         {[]httpx.Route{{Method: "GET", Path: "/a/{x{y}}", Handler: ok}}, "whole segment"},
		"partial segment":       {[]httpx.Route{{Method: "GET", Path: "/a/x{id}", Handler: ok}}, "whole segment"},
		"relative":              {[]httpx.Route{{Method: "GET", Path: "a", Handler: ok}}, "invalid route"},
		"trailing slash":        {[]httpx.Route{{Method: "GET", Path: "/a/", Handler: ok}}, "invalid route"},
		"no method":             {[]httpx.Route{{Path: "/a", Handler: ok}}, "invalid route"},
		"no handler":            {[]httpx.Route{{Method: "GET", Path: "/a"}}, "invalid route"},
		"duplicate":             {[]httpx.Route{{Method: "GET", Path: "/a", Handler: ok}, {Method: "GET", Path: "/a", Handler: ok}}, "registered twice"},
		"duplicate parameter":   {[]httpx.Route{{Method: "GET", Path: "/a/{x}/b/{y}", Handler: ok}, {Method: "GET", Path: "/a/{x}/b/{y}", Handler: ok}}, "registered twice"},
		"names differ, GET/PUT": {[]httpx.Route{{Method: "GET", Path: "/a/{x}/b/{y}", Handler: ok}, {Method: "PUT", Path: "/a/{p}/b/{q}", Handler: ok}}, "other parameter names"},
		"one name differs":      {[]httpx.Route{{Method: "GET", Path: "/a/{x}", Handler: ok}, {Method: "POST", Path: "/a/{y}", Handler: ok}}, "other parameter names"},
		"unguarded parameter":   {[]httpx.Route{{Method: "GET", Path: "/a/{x}", Handler: ok, Unguarded: true}}, "Unguarded"},
	} {
		func() {
			defer func() {
				msg := fmt.Sprint(recover())
				if msg == "<nil>" || !strings.Contains(msg, tc.want) {
					t.Errorf("%s: panic %q, want one containing %q", name, msg, tc.want)
				}
			}()
			httpx.NewRouter(tc.routes, nil, httpx.NotFound())
		}()
	}
}

func TestPathParamOutsideARouteIsEmpty(t *testing.T) {
	if got := httpx.PathParam(httptest.NewRequest("GET", "/", nil), "id"); got != "" {
		t.Errorf("PathParam = %q", got)
	}
}

// A route registered Unguarded is reached without the prefix middleware (a hono route registered
// before .use()): the connections OAuth callback handles a missing session itself.
func TestRouterUnguardedRouteSkipsThePrefixGuardButNothingElseDoes(t *testing.T) {
	h := httpx.NewRouter([]httpx.Route{
		{Method: "GET", Path: "/api/connections", Handler: say("list")},
		{Method: "GET", Path: "/api/connections/callback", Handler: say("callback"), Unguarded: true},
		{Method: "DELETE", Path: "/api/connections/{id}", Handler: say("delete")},
	}, []httpx.Prefix{{Path: "/api/connections", Middleware: []httpx.Middleware{refuse(401)}}}, httpx.NotFound())
	for _, tc := range []struct {
		method, path string
		code         int
		body         string
	}{
		{"GET", "/api/connections/callback", 200, "callback"}, // anonymous
		{"HEAD", "/api/connections/callback", 200, ""},        // HEAD is served by the GET route
		{"POST", "/api/connections/callback", 401, ""},        // another method: the guard answers
		{"DELETE", "/api/connections/callback", 401, ""},      // the parameter route is guarded
		{"GET", "/api/connections", 401, ""},                  // a sibling route
		{"GET", "/api/connections/other", 401, ""},            // an unknown path
		{"GET", "/api/connections/callback/x", 401, ""},       // under the unguarded path
		{"GET", "/api/connections/callback/", 401, ""},        // not clean, still under the prefix
	} {
		w := serveRaw(h, tc.method, tc.path)
		if w.Code != tc.code || (tc.body != "" && w.Body.String() != tc.body) {
			t.Errorf("%s %s: %d %q, want %d %q", tc.method, tc.path, w.Code, w.Body, tc.code, tc.body)
		}
	}
}

func TestRouterParameterHardening(t *testing.T) {
	h := httpx.NewRouter([]httpx.Route{
		{Method: "DELETE", Path: "/api/tokens/{id}", Handler: sayParam("id")},
		{Method: "GET", Path: "/{id}", Handler: sayParam("id")},
	}, nil, httpx.NotFound())
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"DELETE", "/api/tokens/..", 404},     // not clean
		{"DELETE", "/api/tokens/.", 404},      // not clean
		{"DELETE", "/api/tokens/%2e%2e", 404}, // clean when escaped, ".." once decoded
		{"DELETE", "/api/tokens/%2E", 404},
		{"DELETE", "/api/tokens/%ff", 404},    // not UTF-8 once decoded
		{"DELETE", "/api/tokens/a%c3", 404},   // truncated UTF-8
		{"DELETE", "/api/tokens/%c3%a9", 200}, // valid UTF-8
		{"DELETE", "/api/tokens/..a", 200},    // not a dot segment
		{"GET", "/", 404},                     // an empty parameter at the root
		{"GET", "/x", 200},
	} {
		if w := serveRaw(h, tc.method, tc.path); w.Code != tc.code {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, w.Code, tc.code)
		}
	}
}

func TestRouterRefusesAnEmptyOrRootPrefix(t *testing.T) {
	for _, p := range []string{"", "/", "api", "/api/", "/a/{x}"} {
		func() {
			defer func() {
				r := recover()
				msg, _ := r.(string)
				if !strings.Contains(msg, "prefix") {
					t.Errorf("prefix %q: recovered %v, want a panic naming the prefix", p, r)
				}
			}()
			httpx.NewRouter(nil, []httpx.Prefix{{Path: p}}, httpx.NotFound())
		}()
	}
}

func multiParamRouter() http.Handler {
	return httpx.NewRouter([]httpx.Route{
		{Method: "GET", Path: "/api/servers/{slug}", Handler: sayParams("slug")},
		{Method: "GET", Path: "/api/servers/{slug}/tools", Handler: sayParams("slug")},
		{Method: "PUT", Path: "/api/servers/{slug}/tools/{name}/hidden", Handler: sayParams("slug", "name")},
		{Method: "GET", Path: "/api/servers/{slug}/{other}", Handler: sayParams("slug", "other")},
		{Method: "GET", Path: "/api/x/{id}", Handler: sayParams("id")},
		{Method: "GET", Path: "/api/x/new", Handler: say("new")},
		{Method: "GET", Path: "/api/y/{a}/{b}", Handler: sayParams("a", "b")},
		{Method: "GET", Path: "/api/y/{a}/fixed", Handler: say("fixed")},
		{Method: "GET", Path: "/api/y/lit/{b}", Handler: say("lit")},
	}, nil, httpx.NotFound())
}

// sayParams writes every named parameter, "name=value" joined by ";", into the body.
func sayParams(names ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var parts []string
		for _, n := range names {
			parts = append(parts, n+"="+httpx.PathParam(r, n))
		}
		w.Header().Set("X-Params", strings.Join(parts, ";"))
	})
}

// out is what a handler said: the X-Params header of sayParams, else the body of say.
func out(w *httptest.ResponseRecorder) string {
	if p := w.Header().Get("X-Params"); p != "" {
		return p
	}
	return w.Body.String()
}

func TestRouterSeveralParameters(t *testing.T) {
	h := multiParamRouter()
	for _, tc := range []struct {
		method, path string
		code         int
		want         string
	}{
		{"PUT", "/api/servers/gitea/tools/search.issues/hidden", 200, "slug=gitea;name=search.issues"},
		{"PUT", "/api/servers/a%2Fb/tools/c%20d/hidden", 200, "slug=a/b;name=c d"}, // %2F stays in the segment, decoded once
		{"PUT", "/api/servers/a/tools/%252F/hidden", 200, "slug=a;name=%2F"},
		{"PUT", "/api/servers/a/tools/%c3%a9/hidden", 200, "slug=a;name=\u00e9"},
		{"PUT", "/api/servers/a/tools/%2e%2e/hidden", 404, ""}, // ".." once decoded
		{"PUT", "/api/servers/%2E/tools/n/hidden", 404, ""},
		{"PUT", "/api/servers/a/tools/%ff/hidden", 404, ""}, // not UTF-8
		{"PUT", "/api/servers/a/tools//hidden", 404, ""},    // empty segment, not clean
		{"PUT", "/api/servers/a/tools/n", 404, ""},          // too short
		{"PUT", "/api/servers/a/tools/n/hidden/x", 404, ""}, // too long
		{"GET", "/api/servers/a/tools/n/hidden", 404, ""},   // another method
		{"HEAD", "/api/servers/a/tools", 200, "slug=a"},     // HEAD from GET
		{"GET", "/api/y/p/q", 200, "a=p;b=q"},
	} {
		w := serveRaw(h, tc.method, tc.path)
		want := strings.ReplaceAll(tc.want, "\\u00e9", "\u00e9")
		if w.Code != tc.code || (tc.code == 200 && out(w) != want) {
			t.Errorf("%s %s: %d %q, want %d %q", tc.method, tc.path, w.Code, out(w), tc.code, want)
		}
	}
}

// The most specific pattern wins: at the first position where two patterns differ, a literal
// beats a parameter, whatever the registration order.
func TestRouterMostSpecificPatternWins(t *testing.T) {
	h := multiParamRouter()
	for _, tc := range []struct{ path, want string }{
		{"/api/servers/s/tools", "slug=s"},             // not {other}
		{"/api/servers/s/other", "slug=s;other=other"}, // only the parameter pattern fits
		{"/api/x/new", "new"},                          // registered after {id}, still wins
		{"/api/x/newer", "id=newer"},
		{"/api/y/lit/z", "lit"},     // literal at position 2 beats a parameter there
		{"/api/y/p/fixed", "fixed"}, // first difference is the last segment
		{"/api/y/lit/fixed", "lit"}, // the earlier position decides
	} {
		w := serveRaw(h, "GET", tc.path)
		if w.Code != 200 || out(w) != tc.want {
			t.Errorf("GET %s: %d %q, want %q", tc.path, w.Code, out(w), tc.want)
		}
	}
	// Registration order does not matter.
	rev := httpx.NewRouter([]httpx.Route{
		{Method: "GET", Path: "/api/x/new", Handler: say("new")},
		{Method: "GET", Path: "/api/x/{id}", Handler: sayParams("id")},
	}, nil, httpx.NotFound())
	if w := serveRaw(rev, "GET", "/api/x/new"); out(w) != "new" {
		t.Errorf("reversed order: %q", out(w))
	}
}

// A more specific pattern that has no handler for the method does not hide a less specific one.
func TestRouterFallsBackToALessSpecificPatternForTheMethod(t *testing.T) {
	h := httpx.NewRouter([]httpx.Route{
		{Method: "POST", Path: "/api/x/new", Handler: say("create")},
		{Method: "GET", Path: "/api/x/{id}", Handler: sayParams("id")},
	}, nil, httpx.NotFound())
	if w := serveRaw(h, "GET", "/api/x/new"); out(w) != "id=new" {
		t.Errorf("GET /api/x/new: %q", out(w))
	}
	if w := serveRaw(h, "POST", "/api/x/new"); out(w) != "create" {
		t.Errorf("POST /api/x/new: %q", out(w))
	}
}

func TestRouterPrefixAndUnguardedWithSeveralParameters(t *testing.T) {
	h := httpx.NewRouter([]httpx.Route{
		{Method: "PUT", Path: "/api/servers/{slug}/tools/{name}/hidden", Handler: sayParams("slug", "name")},
		{Method: "GET", Path: "/api/servers/public", Handler: say("pub"), Unguarded: true},
	}, []httpx.Prefix{
		{Path: "/api/servers", Middleware: []httpx.Middleware{tag("guard"), refuse(403)}},
	}, httpx.NotFound())
	if w := serveRaw(h, "PUT", "/api/servers/a/tools/b/hidden"); w.Code != 403 || w.Header().Get("X-Trace") != "guard" {
		t.Errorf("guarded: %d trace %q", w.Code, w.Header().Get("X-Trace"))
	}
	if w := serveRaw(h, "GET", "/api/servers/a/tools/b/hidden"); w.Code != 403 { // another method: the guard answers
		t.Errorf("other method: %d", w.Code)
	}
	w := serveRaw(h, "GET", "/api/servers/public")
	if w.Code != 200 || out(w) != "pub" || w.Header().Get("X-Trace") != "" {
		t.Errorf("unguarded: %d %q trace %q", w.Code, out(w), w.Header().Get("X-Trace"))
	}
	if w := serveRaw(h, "GET", "/api/servers/public/tools/b/hidden"); w.Code != 403 {
		t.Errorf("a parameter route next to the unguarded one stays guarded: %d", w.Code)
	}
}

// A parameter pattern without a handler for the method does not hide a less specific parameter
// pattern that has one.
func TestRouterFallsBackBetweenParameterPatterns(t *testing.T) {
	h := httpx.NewRouter([]httpx.Route{
		{Method: "POST", Path: "/api/y/{a}/fixed", Handler: say("create")},
		{Method: "GET", Path: "/api/y/{a}/{b}", Handler: sayParams("a", "b")},
	}, nil, httpx.NotFound())
	if w := serveRaw(h, "GET", "/api/y/p/fixed"); w.Code != 200 || out(w) != "a=p;b=fixed" {
		t.Errorf("GET: %d %q", w.Code, out(w))
	}
	if w := serveRaw(h, "POST", "/api/y/p/fixed"); out(w) != "create" {
		t.Errorf("POST: %q", out(w))
	}
	if w := serveRaw(h, "POST", "/api/y/p/other"); w.Code != 404 {
		t.Errorf("POST /api/y/p/other: %d", w.Code)
	}
}

// The earlier segment decides: "/a/b/{y}" beats "/a/{x}/c" for "/a/b/c", in either order.
func TestRouterEarlierSegmentDecidesTheTie(t *testing.T) {
	r1 := httpx.Route{Method: "GET", Path: "/a/b/{y}", Handler: sayParams("y")}
	r2 := httpx.Route{Method: "GET", Path: "/a/{x}/c", Handler: sayParams("x")}
	for name, routes := range map[string][]httpx.Route{"literal first": {r1, r2}, "parameter first": {r2, r1}} {
		h := httpx.NewRouter(routes, nil, httpx.NotFound())
		if w := serveRaw(h, "GET", "/a/b/c"); out(w) != "y=c" {
			t.Errorf("%s: %q", name, out(w))
		}
	}
}
