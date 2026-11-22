package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	catapp "github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	catinfra "github.com/ai-pipes/mcp-registry/internal/modules/catalog/infra"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/upstream"
)

// The composed catalog over in-memory repositories and the real guards of identity, run through
// the real composed handler. The route table is the one the composition produced, not the HTTP
// package's own (which injects fake guards).

// memServers lists nothing; every other method panics (a route that reaches it was not stopped by
// its guard).
type memServers struct{ catapp.ServerRepository }

// FindBySlug knows no server: the routes that look one up answer not-found.
func (memServers) FindBySlug(context.Context, string) (*domain.Server, error) { return nil, nil }

func (memServers) List(context.Context) ([]catapp.ServerListing, error) { return nil, nil }

type memTools struct{ catapp.ToolRepository }

func (memTools) Find(context.Context, string, string) (*domain.Tool, error) { return nil, nil }
func (memTools) ListByServer(context.Context, string) ([]*domain.Tool, error) {
	return nil, nil
}

type noUpstream struct{ kernel.UpstreamClient }

func catinfraRepos(s catapp.ServerRepository) catinfra.Repos {
	return catinfra.Repos{Servers: s, Tools: memTools{}}
}

var sessionOf = cookie

// catalogPaths is the catalog's route table as the server's own module list (modules(), the
// function Run uses) builds it. The table cannot be SERVED from that list without a database (its
// repositories are Postgres ones), so the requests below go to a handler assembled from the same
// wiring over in-memory repositories; the two tables must be equal.
func catalogPaths(t *testing.T, e *wiringEnv) []httpx.Route {
	t.Helper()
	composed := modules(e.c.rt)[2].Routes
	if len(composed) != 10 {
		t.Fatalf("the catalog registers %d routes, want 10", len(composed))
	}
	served := e.c.module().Routes
	key := func(r httpx.Route) string { return r.Method + " " + r.Path }
	var a, b []string
	for _, r := range composed {
		a = append(a, key(r))
	}
	for _, r := range served {
		b = append(b, key(r))
	}
	if !slices.Equal(a, b) {
		t.Fatalf("modules() routes %v differ from the served ones %v", a, b)
	}
	return composed
}

// concrete turns a route pattern into a request path.
func concrete(p string) string {
	p = strings.ReplaceAll(p, "{slug}", "gitea")
	return strings.ReplaceAll(p, "{name}", "search")
}

// The refusal of a token is the session guard's own, by name: the handlers' checks answer 401
// too, so the status alone would not tell a guard that was stripped of its Authorization check.
const tokenRefusal = //nolint:gosec // G101: an error message, not a credential
"tokens do not work on the web API"

func TestEveryCatalogRouteOfTheComposedServerIsGuarded(t *testing.T) {
	e := newWiringEnv(t)
	withToken := func(q *http.Request) { q.Header.Set("Authorization", "Bearer sk_x") }
	for _, r := range catalogPaths(t, e) {
		path := concrete(r.Path)
		isAdminRoute := strings.HasPrefix(r.Path, "/api/servers")
		do := func(mod func(*http.Request)) *httptest.ResponseRecorder {
			return e.do(r.Method, path, `{}`, "198.51.100.1", mod)
		}
		if got := do(nil); got.Code != http.StatusUnauthorized || !strings.Contains(got.Body.String(), "sign in required") {
			t.Errorf("%s %s without a session: %d %s, want 401 sign in required", r.Method, r.Path, got.Code, got.Body)
		}
		if got := do(withToken); got.Code != http.StatusUnauthorized || !strings.Contains(got.Body.String(), tokenRefusal) {
			t.Errorf("%s %s with an sk_ token: %d %s, want 401 %q", r.Method, r.Path, got.Code, got.Body, tokenRefusal)
		}
		// A token next to a valid admin session is refused too (as in TypeScript: the web API
		// takes the cookie alone).
		both := func(q *http.Request) { sessionOf("mcp_session", adminCookie)(q); withToken(q) }
		if got := do(both); got.Code != http.StatusUnauthorized || !strings.Contains(got.Body.String(), tokenRefusal) {
			t.Errorf("%s %s with a token and an admin session: %d %s, want 401 %q", r.Method, r.Path, got.Code, got.Body, tokenRefusal)
		}
		member := do(sessionOf("mcp_session", memberCookie)).Code
		if isAdminRoute && member != http.StatusForbidden {
			t.Errorf("%s %s as a member: %d, want 403", r.Method, r.Path, member)
		}
		if !isAdminRoute && member != http.StatusOK {
			t.Errorf("%s %s as a member: %d, want 200", r.Method, r.Path, member)
		}
		// An admin passes the guards: whatever the handler answers over empty repositories
		// (a not-found, a validation error, a list), it is neither a refusal nor a crash.
		if got := do(sessionOf("mcp_session", adminCookie)).Code; got == http.StatusUnauthorized || got == http.StatusForbidden || got >= 500 {
			t.Errorf("%s %s as an admin: %d", r.Method, r.Path, got)
		}
	}
}

// A path under the guarded prefixes that no route serves is still answered by the guard.
func TestUnknownPathsUnderTheServersPrefixAreGuarded(t *testing.T) {
	e := newWiringEnv(t)
	for _, path := range []string{"/api/servers/x/unknown", "/api/servers/"} {
		if got := e.do("GET", path, "", "198.51.100.1", nil).Code; got != http.StatusUnauthorized {
			t.Errorf("GET %s without a session: %d", path, got)
		}
	}
	// Only the route GET /api/catalog is guarded, not the paths below it (as in TypeScript).
	if got := e.do("GET", "/api/catalog/x", "", "198.51.100.1", nil).Code; got != http.StatusNotFound {
		t.Errorf("GET /api/catalog/x without a session: %d, want 404", got)
	}
	if got := e.do("DELETE", "/api/servers/gitea", "", "198.51.100.1", sessionOf("mcp_session", memberCookie)).Code; got != http.StatusForbidden {
		t.Errorf("DELETE /api/servers/gitea as a member: %d, want 403", got)
	}
}

// The platform's middleware covers the catalog routes: Origin and the body limit.
func TestPlatformMiddlewareAppliesToCatalogRoutes(t *testing.T) {
	e := newWiringEnv(t)
	admin := sessionOf("mcp_session", adminCookie)
	foreign := func(r *http.Request) { admin(r); r.Header.Set("Origin", "https://evil.example") }
	if got := e.do("POST", "/api/servers", `{}`, "198.51.100.1", foreign).Code; got != http.StatusForbidden {
		t.Errorf("a foreign Origin: %d, want 403", got)
	}
	big := `{"name":"` + strings.Repeat("a", restBodyLimit+10) + `"}`
	if got := e.do("POST", "/api/servers", big, "198.51.100.1", admin).Code; got != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over 1 MiB: %d, want 413", got)
	}
}

func TestCatalogWiringFacts(t *testing.T) {
	e := newWiringEnv(t)
	d := e.c.deps
	if d.Credentials == nil {
		t.Fatal("no credentials")
	}
	if d.MaxResponseBytes != 12345 {
		t.Errorf("MaxResponseBytes = %d, want UPSTREAM_MAX_RESPONSE_BYTES", d.MaxResponseBytes)
	}
	if e.c.interval != 6*time.Hour {
		t.Errorf("interval = %v, want SYNC_INTERVAL_HOURS", e.c.interval)
	}
	jobs := e.c.module().Jobs
	if len(jobs) != 1 || jobs[0].Name != "catalog-sync" || jobs[0].Every != 6*time.Hour {
		t.Errorf("jobs = %+v", jobs)
	}
	// The production composition: the system clock and the real client.
	rt := Runtime{Config: e.c.rt.Config, Logger: e.c.rt.Logger, Clock: kernel.SystemClock{}, Audit: e.c.rt.Audit}
	w := newCatalogWiring(rt, e.w)
	if _, ok := w.deps.Upstream.(*upstream.Client); !ok {
		t.Errorf("upstream is %T", w.deps.Upstream)
	}
	if _, ok := w.deps.Clock.(kernel.SystemClock); !ok {
		t.Errorf("clock is %T", w.deps.Clock)
	}
}

type failingServers struct{ memServers }

func (failingServers) List(context.Context) ([]catapp.ServerListing, error) {
	return nil, errors.New("password=hunter2")
}

func TestSyncJobLogsLikeTheScheduler(t *testing.T) {
	t.Run("done", func(t *testing.T) {
		e := newWiringEnv(t)
		if err := e.c.syncAll(context.Background()); err != nil {
			t.Fatal(err)
		}
		recs := e.logs.find(t, "catalog sync: done")
		if len(recs) != 1 || recs[0]["servers"] != float64(0) || recs[0]["level"] == nil {
			t.Fatalf("records: %v\n%s", recs, e.logs.String())
		}
		if f, ok := recs[0]["failed"].([]any); !ok || len(f) != 0 {
			t.Fatalf("failed = %#v, want []", recs[0]["failed"])
		}
	})
	t.Run("an unexpected error is logged without its text and the schedule goes on", func(t *testing.T) {
		e := newWiringEnv(t)
		w := catalogWiringOver(e.c.rt, e.w, catinfraRepos(failingServers{}), noUpstream{})
		if err := w.syncAll(context.Background()); err != nil {
			t.Fatalf("a failed run must not be returned: %v", err)
		}
		if n := len(e.logs.find(t, "catalog sync: run failed")); n != 1 {
			t.Fatalf("records: %d\n%s", n, e.logs.String())
		}
		if strings.Contains(e.logs.String(), "hunter2") {
			t.Fatalf("log has the error text: %s", e.logs.String())
		}
	})
	t.Run("a shutdown is returned, not logged", func(t *testing.T) {
		e := newWiringEnv(t)
		w := catalogWiringOver(e.c.rt, e.w, catinfraRepos(failingServers{}), noUpstream{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := w.syncAll(ctx); err == nil {
			t.Fatal("want the error of the cancelled run")
		}
		if strings.Contains(e.logs.String(), "catalog sync") {
			t.Fatalf("log: %s", e.logs.String())
		}
	})
}

// The module list the server really runs carries the catalog: its routes under the guarded
// prefixes and its synchronisation job (this one fails when modules() forgets it).
func TestTheServerModuleListHasTheCatalog(t *testing.T) {
	e := newWiringEnv(t)
	asm, err := assemble(modules(e.c.rt))
	if err != nil {
		t.Fatal(err)
	}
	has := func(method, path string) bool {
		for _, r := range asm.routes {
			if r.Method == method && r.Path == path {
				return true
			}
		}
		return false
	}
	for _, r := range e.c.module().Routes {
		if !has(r.Method, r.Path) {
			t.Errorf("%s %s is not served", r.Method, r.Path)
		}
	}
	var names []string
	for _, j := range asm.jobs {
		names = append(names, j.Name)
	}
	if !slices.Contains(names, "catalog-sync") || !slices.Contains(names, "session-retention") {
		t.Errorf("jobs: %v", names)
	}
	// Since milestone 4 the search module handles every catalog event, so the worker starts.
	if got := sortedKeys(asm.handlers); !slices.Equal(got, sortedCopy(catalog.EventTypes())) {
		t.Errorf("handlers %v, want one for each of %v", got, catalog.EventTypes())
	}
}

func TestCatalogWiringRefusesToBeBuiltWithoutCredentials(t *testing.T) {
	e := newWiringEnv(t)
	defer func() {
		if recover() == nil {
			t.Fatal("no panic for a wiring without the identity service")
		}
	}()
	catalogWiringOver(e.c.rt, identityWiring{}, catinfraRepos(memServers{}), noUpstream{})
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
