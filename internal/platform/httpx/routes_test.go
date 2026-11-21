package httpx_test

import (
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
	for name, routes := range map[string][]httpx.Route{
		"two parameters":      {{Method: "GET", Path: "/a/{x}/{y}", Handler: ok}},
		"partial segment":     {{Method: "GET", Path: "/a/x{id}", Handler: ok}},
		"relative":            {{Method: "GET", Path: "a", Handler: ok}},
		"trailing slash":      {{Method: "GET", Path: "/a/", Handler: ok}},
		"no method":           {{Path: "/a", Handler: ok}},
		"no handler":          {{Method: "GET", Path: "/a"}},
		"duplicate":           {{Method: "GET", Path: "/a", Handler: ok}, {Method: "GET", Path: "/a", Handler: ok}},
		"duplicate parameter": {{Method: "GET", Path: "/a/{x}", Handler: ok}, {Method: "GET", Path: "/a/{y}", Handler: ok}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: NewRouter did not panic", name)
				}
			}()
			httpx.NewRouter(routes, nil, httpx.NotFound())
		}()
	}
}

func TestPathParamOutsideARouteIsEmpty(t *testing.T) {
	if got := httpx.PathParam(httptest.NewRequest("GET", "/", nil), "id"); got != "" {
		t.Errorf("PathParam = %q", got)
	}
}
