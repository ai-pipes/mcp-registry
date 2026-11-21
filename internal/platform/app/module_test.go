package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
)

func say(s string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(s)) })
}

func TestTheDefaultModuleListIsEmptyUntilIdentityIsWired(t *testing.T) {
	if got := modules(Runtime{}); len(got) != 0 {
		t.Fatalf("modules() = %d entries", len(got))
	}
}

func TestAssembleMergesInOrder(t *testing.T) {
	h := func(context.Context, map[string]any) error { return nil }
	a, err := assemble([]Module{
		{
			Routes: []httpx.Route{{Method: "GET", Path: "/a", Handler: say("a")}}, Handlers: map[string]outbox.Handler{"x.one": h},
			Commands: []CLICommand{{Name: "one"}}, FirstStart: func(context.Context) error { return nil },
		},
		{
			Routes: []httpx.Route{{Method: "GET", Path: "/b", Handler: say("b")}}, Handlers: map[string]outbox.Handler{"x.two": h},
			Commands: []CLICommand{{Name: "two"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.routes) != 2 || a.routes[0].Path != "/a" || a.routes[1].Path != "/b" {
		t.Errorf("routes %v", a.routes)
	}
	if len(a.handlers) != 2 || len(a.commands) != 2 || len(a.firstStart) != 1 {
		t.Errorf("handlers %d commands %d firstStart %d", len(a.handlers), len(a.commands), len(a.firstStart))
	}
}

func TestAssembleRefusesTheSameEventTypeOrCommandTwice(t *testing.T) {
	h := func(context.Context, map[string]any) error { return nil }
	_, err := assemble([]Module{
		{Handlers: map[string]outbox.Handler{"catalog.tools_changed": h}},
		{Handlers: map[string]outbox.Handler{"catalog.tools_changed": h}},
	})
	if err == nil || !strings.Contains(err.Error(), "catalog.tools_changed") {
		t.Errorf("duplicate handler: %v", err)
	}
	_, err = assemble([]Module{{Commands: []CLICommand{{Name: "sync"}}}, {Commands: []CLICommand{{Name: "sync"}}}})
	if err == nil || !strings.Contains(err.Error(), `"sync"`) {
		t.Errorf("duplicate command: %v", err)
	}
}

func block(code int) httpx.Middleware {
	return func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
	}
}

func TestModuleRoutesAndPrefixesAreServedBeforeStaticFilesAndTheShell(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{"index.html": "<shell>", "hello.txt": "static"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig()
	h := handler(cfg, discardLogger(), nil, root, []httpx.Route{
		{Method: "GET", Path: "/api/stub", Handler: say("api")},
		{Method: "GET", Path: "/api/stub/{id}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Param", httpx.PathParam(r, "id"))
		})},
		{Method: "GET", Path: "/hello.txt", Handler: say("route")},
	}, []httpx.Prefix{{Path: "/api/guarded", Middleware: []httpx.Middleware{block(401)}}})

	for _, tc := range []struct {
		method, path string
		code         int
		body         string
	}{
		{"GET", "/api/stub", 200, "api"},
		{"GET", "/api/stub/7", 200, ""},
		{"GET", "/hello.txt", 200, "route"},       // a route wins over a static file
		{"GET", "/healthz", 503, "fail"},          // before the modules
		{"GET", "/api/guarded/anything", 401, ""}, // the prefix guard answers for unknown paths
		{"POST", "/api/stub", 403, ""},            // CSRF is in front of the modules (no Origin)
	} {
		w := do(h, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.code || (tc.body != "" && w.Body.String() != tc.body) {
			t.Errorf("%s %s: %d %q, want %d %q", tc.method, tc.path, w.Code, w.Body, tc.code, tc.body)
		}
	}
	if w := do(h, httptest.NewRequest("GET", "/api/stub/7", nil)); w.Header().Get("X-Param") != "7" {
		t.Errorf("the parameter reached the handler as %q", w.Header().Get("X-Param"))
	}
	// Nothing under /api or /mcp falls through to the shell; the UI still gets it.
	nav := func(path string) *http.Request {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Accept", "text/html")
		return r
	}
	if w := do(h, nav("/api/unknown")); w.Code != 404 || !strings.Contains(w.Body.String(), `"not_found"`) {
		t.Errorf("/api/unknown: %d %q", w.Code, w.Body)
	}
	if w := do(h, httptest.NewRequest("PUT", "/api/stub", nil)); w.Code == 405 {
		t.Errorf("PUT on a GET route is 405")
	}
	if w := do(h, nav("/mcp/x")); w.Code != 404 || strings.Contains(w.Body.String(), "<shell>") {
		t.Errorf("/mcp/x: %d %q", w.Code, w.Body)
	}
	if w := do(h, nav("/catalog")); w.Code != 200 || w.Body.String() != "<shell>" {
		t.Errorf("/catalog: %d %q", w.Code, w.Body)
	}
}
