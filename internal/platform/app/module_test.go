package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
)

func say(s string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(s)) })
}

// Building the modules makes no request (the CLI builds them for every command): a Runtime
// without a pool is enough.
func TestTheDefaultModuleListIsIdentityThenThePlatformRoutesThenTheCatalogThenSearchThenConnectionsThenTheGateway(t *testing.T) {
	got := modules(bareRuntime(t))
	if len(got) != 6 {
		t.Fatalf("modules() = %d entries", len(got))
	}
	paths := func(m Module) (out []string) {
		for _, r := range m.Routes {
			out = append(out, r.Method+" "+r.Path)
		}
		return out
	}
	if p := paths(got[0]); !slices.Contains(p, "POST /api/auth/link") || !slices.Contains(p, "GET /api/users") || slices.Contains(p, "GET /api/audit") {
		t.Errorf("identity routes: %v", p)
	}
	if p := paths(got[1]); !slices.Equal(p, []string{"GET /api/version", "GET /api/audit"}) {
		t.Errorf("platform routes: %v", p)
	}
	if p := paths(got[2]); !slices.Contains(p, "GET /api/catalog") || !slices.Contains(p, "POST /api/servers/{slug}/sync") {
		t.Errorf("catalog routes: %v", p)
	}
	if len(got[2].Jobs) != 1 || got[2].Jobs[0].Name != "catalog-sync" {
		t.Errorf("catalog job: %+v", got[2].Jobs)
	}
	if s := got[3]; len(s.Routes) != 0 || len(s.Handlers) != len(catalog.EventTypes()) || len(s.Sweeps) != 1 ||
		s.Sweeps[0].Every != 5*time.Minute || len(s.Commands) != 1 || s.Commands[0].Name != "reindex" ||
		s.BeforeServing == nil || len(s.Closers) != 1 {
		t.Errorf("search module: %+v", s)
	}
	// The gateway is last: /mcp and the call list with identity's guards, the daily retention.
	if p := paths(got[5]); !slices.Contains(p, "POST /mcp") || !slices.Contains(p, "GET /mcp") || !slices.Contains(p, "GET /api/calls") {
		t.Errorf("gateway routes: %v", p)
	}
	if g := got[5]; len(g.Jobs) != 1 || g.Jobs[0].Name != "gateway-retention" || g.Jobs[0].Every != 24*time.Hour ||
		len(g.Handlers) != 0 || len(g.Emits) != 0 || len(g.Closers) != 0 || len(g.Commands) != 0 {
		t.Errorf("gateway module: %+v", g)
	}
	if len(got[0].Jobs) != 1 || got[0].Jobs[0].Every != 24*time.Hour || got[0].FirstStart == nil {
		t.Errorf("identity job or first start: %+v", got[0])
	}
}

func TestAssembleMergesInOrder(t *testing.T) {
	h := func(context.Context, map[string]any) error { return nil }
	a, err := assemble([]Module{
		{
			Routes: []httpx.Route{{Method: "GET", Path: "/a", Handler: say("a")}}, Handlers: map[string]outbox.Handler{"x.one": h},
			Commands: []CLICommand{{Name: "one"}}, FirstStart: func(context.Context) {},
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

func okHandler(context.Context, map[string]any) error { return nil }

func handlersFor(types ...string) map[string]outbox.Handler {
	m := map[string]outbox.Handler{}
	for _, typ := range types {
		m[typ] = okHandler
	}
	return m
}

// The worker marks an event of a type without a handler as processed, so a table that handles
// only some of the types a module emits would lose the others for good.
func TestAssembleRefusesAnIncompleteHandlerTableAndNamesTheMissingTypes(t *testing.T) {
	_, err := assemble([]Module{
		{Emits: []string{"a.one", "a.two", "a.three"}},
		{Handlers: handlersFor("a.two")},
	})
	if err == nil {
		t.Fatal("an incomplete table was accepted")
	}
	for _, want := range []string{"a.one", "a.three"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "a.two") {
		t.Errorf("the error names a handled type: %v", err)
	}
}

func TestAssembleAcceptsACompleteTableFromAnyModuleOrder(t *testing.T) {
	a, err := assemble([]Module{
		{Handlers: handlersFor("a.two", "extra.type")},
		{Emits: []string{"a.one", "a.two"}, Handlers: handlersFor("a.one")},
	})
	if err != nil || len(a.handlers) != 3 {
		t.Fatalf("%v %d", err, len(a.handlers))
	}
}

// An empty table means "worker off": nothing is checked, however many types are emitted.
func TestAssembleAcceptsAnEmptyTableWhateverIsEmitted(t *testing.T) {
	a, err := assemble([]Module{{Emits: []string{"a.one"}}})
	if err != nil || len(a.handlers) != 0 {
		t.Fatalf("%v %d", err, len(a.handlers))
	}
}

func TestAssembleComposesTheSweepsOfAllModules(t *testing.T) {
	var ran []string
	sweep := func(name string, every time.Duration) outbox.Sweep {
		return outbox.Sweep{Every: every, Run: func(context.Context) error { ran = append(ran, name); return nil }}
	}
	a, err := assemble([]Module{
		{Sweeps: []outbox.Sweep{sweep("one", 5*time.Minute)}},
		{},
		{Sweeps: []outbox.Sweep{sweep("two", time.Minute), sweep("three", time.Hour)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.sweep == nil || a.sweep.Every != time.Minute {
		t.Fatalf("composed sweep: %+v", a.sweep)
	}
	if err := a.sweep.Run(context.Background()); err != nil || !slices.Equal(ran, []string{"one", "two", "three"}) {
		t.Fatalf("ran %v: %v", ran, err)
	}
	if a, _ := assemble([]Module{{}}); a.sweep != nil {
		t.Error("no sweeps must leave the sweep nil")
	}
}

func TestTheDefaultModulesAnnounceTheCatalogEventTypes(t *testing.T) {
	got := modules(bareRuntime(t))
	if !slices.Equal(got[2].Emits, catalog.EventTypes()) {
		t.Fatalf("catalog Emits = %v, want %v", got[2].Emits, catalog.EventTypes())
	}
}

func TestAssembleCollectsTheHooksBeforeServingAndTheClosers(t *testing.T) {
	var order []string
	mark := func(s string) func(context.Context) { return func(context.Context) { order = append(order, s) } }
	closer := func(string) func(context.Context) error { return func(context.Context) error { return nil } }
	a, err := assemble([]Module{
		{BeforeServing: mark("one"), Closers: []func(context.Context) error{closer("a")}},
		{},
		{BeforeServing: mark("two"), Closers: []func(context.Context) error{closer("b"), closer("c")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range a.beforeServing {
		f(context.Background())
	}
	if !slices.Equal(order, []string{"one", "two"}) || len(a.closers) != 3 {
		t.Fatalf("order %v, %d closers", order, len(a.closers))
	}
}

// The embedder (a closer) must outlive the outbox worker: a worker still indexing when the embedder
// closes would get ErrClosed with a live context and clear existing vectors.
func TestClosersRunOnlyAfterEveryStopHasReturned(t *testing.T) {
	var mu sync.Mutex
	var events []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, s)
	}
	closerRan := make(chan struct{})
	// A stop that takes its time: it gives a closer started too early the chance to run first.
	slowWorker := func(context.Context) error {
		select {
		case <-closerRan:
		case <-time.After(200 * time.Millisecond):
		}
		note("worker stopped")
		return nil
	}
	server := func(context.Context) error { note("server stopped"); return nil }
	closer := func(context.Context) error { note("embedder closed"); close(closerRan); return nil }
	if err := stopAllThenClose(context.Background(), []func(context.Context) error{slowWorker, server}, []func(context.Context) error{closer}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2] != "embedder closed" {
		t.Fatalf("events %v: the closer ran before a stop returned", events)
	}
}

func TestClosersDoNotRunWhenAStopFailed(t *testing.T) {
	failed := errors.New("did not stop in time")
	closed := false
	err := stopAllThenClose(context.Background(),
		[]func(context.Context) error{func(context.Context) error { return failed }},
		[]func(context.Context) error{func(context.Context) error { closed = true; return nil }})
	if !errors.Is(err, failed) || closed {
		t.Fatalf("err %v, closed %v: something may still use what the closer releases", err, closed)
	}
}

func TestACloserErrorIsReportedAndTheOthersStillRun(t *testing.T) {
	bad := errors.New("close failed")
	ran := false
	err := stopAllThenClose(context.Background(), nil, []func(context.Context) error{
		func(context.Context) error { return bad },
		func(context.Context) error { ran = true; return nil },
	})
	if !errors.Is(err, bad) || !ran {
		t.Fatalf("err %v, second closer ran %v", err, ran)
	}
}

// The default list must assemble: the search module's handlers cover every event type the catalog
// emits (an incomplete table is a start-up error), and the sweep and the command are registered.
func TestTheDefaultModuleListAssembles(t *testing.T) {
	a, err := assemble(modules(bareRuntime(t)))
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(a.handlers); !slices.Equal(got, sortedCopy(catalog.EventTypes())) {
		t.Errorf("handlers %v", got)
	}
	if a.sweep == nil || a.sweep.Every != 5*time.Minute {
		t.Errorf("sweep %+v", a.sweep)
	}
	if _, ok := a.commands["reindex"]; !ok {
		t.Error("reindex is not registered")
	}
	if len(a.beforeServing) != 1 || len(a.closers) != 1 {
		t.Errorf("beforeServing %d, closers %d", len(a.beforeServing), len(a.closers))
	}
}
