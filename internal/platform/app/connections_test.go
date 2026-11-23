package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	conapp "github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	condomain "github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// testBox is a secret box over a fixed key.
func testBox(t *testing.T) kernel.SecretBox {
	t.Helper()
	box, err := crypto.NewAESGCMBox([]byte("0123456789abcdef0123456789abcdef"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// bareRuntime is a Runtime without a pool (building the modules makes no request) but with every
// value the connections service insists on.
func bareRuntime(t *testing.T) Runtime {
	t.Helper()
	return Runtime{
		Config: config.Config{PublicURL: "http://localhost:3000", UpstreamMaxResponseBytes: 1 << 20, MCPMaxBodyBytes: 1 << 20, CallLogRetentionDays: 90},
		Clock:  kernel.SystemClock{}, Logger: discardLogger(), Version: "1.2.3",
		Box: testBox(t), Audit: &countingAudit{},
	}
}

// memConnections knows no connection; every method the routes of the matrix do not reach panics.
// Get is counted: it shows that a request went through the connections service.
type memConnections struct {
	conapp.ConnectionStore
	gets atomic.Int32
}

func (m *memConnections) Get(context.Context, string, string) (*condomain.Connection, error) {
	m.gets.Add(1)
	return nil, nil
}

func (*memConnections) ListForUser(context.Context, string) ([]*condomain.Connection, error) {
	return nil, nil
}

func (*memConnections) Delete(context.Context, string, string) (*condomain.Connection, error) {
	return nil, nil
}

type memPendingConnects struct{ conapp.PendingConnects }

func (*memPendingConnects) Consume(context.Context, string, string) (*condomain.PendingConnect, error) {
	return nil, nil
}

// connectionsPaths is the route table of the server's own module list (the fifth module); like the
// catalog's, it is compared with the one served over in-memory stores.
func connectionsPaths(t *testing.T, e *wiringEnv) []httpx.Route {
	t.Helper()
	all := modules(bareRuntime(t))
	composed := all[4].Routes
	if len(composed) != 5 {
		t.Fatalf("connections registers %d routes, want 5", len(composed))
	}
	key := func(r httpx.Route) string { return r.Method + " " + r.Path }
	var a, b []string
	for _, r := range composed {
		a = append(a, key(r))
	}
	for _, r := range e.con.module().Routes {
		b = append(b, key(r))
	}
	if !slices.Equal(a, b) {
		t.Fatalf("modules() routes %v differ from the served ones %v", a, b)
	}
	return composed
}

func TestEveryConnectionsRouteOfTheComposedServerIsGuarded(t *testing.T) {
	e := newWiringEnv(t)
	withToken := func(q *http.Request) { q.Header.Set("Authorization", "Bearer sk_x") }
	for _, r := range connectionsPaths(t, e) {
		path := strings.ReplaceAll(r.Path, "{server}", "gitea")
		do := func(mod func(*http.Request)) *httptest.ResponseRecorder {
			return e.do(r.Method, path, `{}`, "198.51.100.1", mod)
		}
		if r.Path == "/api/connections/callback" {
			// Reachable without a session, and answers with a redirect to the UI, never JSON.
			got := do(nil)
			if got.Code != http.StatusFound || got.Header().Get("Location") != "/connections?error=unauthenticated" {
				t.Errorf("callback without a session: %d %q", got.Code, got.Header().Get("Location"))
			}
			// The platform's middleware is in front of it: security headers and a request id.
			if got.Header().Get("Content-Security-Policy") == "" || got.Header().Get("X-Request-Id") == "" {
				t.Errorf("callback lacks the platform headers: %v", got.Header())
			}
			continue
		}
		if got := do(nil); got.Code != http.StatusUnauthorized || !strings.Contains(got.Body.String(), "sign in required") {
			t.Errorf("%s %s without a session: %d %s, want 401 sign in required", r.Method, r.Path, got.Code, got.Body)
		}
		if got := do(withToken); got.Code != http.StatusUnauthorized || !strings.Contains(got.Body.String(), tokenRefusal) {
			t.Errorf("%s %s with an sk_ token: %d %s, want 401 %q", r.Method, r.Path, got.Code, got.Body, tokenRefusal)
		}
		both := func(q *http.Request) { sessionOf("mcp_session", adminCookie)(q); withToken(q) }
		if got := do(both); got.Code != http.StatusUnauthorized || !strings.Contains(got.Body.String(), tokenRefusal) {
			t.Errorf("%s %s with a token and a session: %d %s, want 401 %q", r.Method, r.Path, got.Code, got.Body, tokenRefusal)
		}
		// Connections are everyone's: a member and an admin pass the guard (over empty stores a
		// handler answers a not-found, a validation error or a list, never a refusal or a crash).
		for _, c := range []string{memberCookie, adminCookie} {
			if got := do(sessionOf("mcp_session", c)).Code; got == http.StatusUnauthorized || got == http.StatusForbidden || got >= 500 {
				t.Errorf("%s %s with session %s: %d", r.Method, r.Path, c, got)
			}
		}
	}
	// A path under the prefix that no route serves is answered by the guard.
	if got := e.do("GET", "/api/connections/x/unknown", "", "198.51.100.1", nil).Code; got != http.StatusUnauthorized {
		t.Errorf("unknown path without a session: %d", got)
	}
}

func TestPlatformMiddlewareAppliesToConnectionsRoutes(t *testing.T) {
	e := newWiringEnv(t)
	member := sessionOf("mcp_session", memberCookie)
	foreign := func(r *http.Request) { member(r); r.Header.Set("Origin", "https://evil.example") }
	if got := e.do("POST", "/api/connections/gitea/start", `{}`, "198.51.100.1", foreign).Code; got != http.StatusForbidden {
		t.Errorf("a foreign Origin: %d, want 403", got)
	}
	big := `{"token":"` + strings.Repeat("a", restBodyLimit+10) + `"}`
	if got := e.do("PUT", "/api/connections/gitea/pat", big, "198.51.100.1", member).Code; got != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over 1 MiB: %d, want 413", got)
	}
}

func TestConnectionsWiringFacts(t *testing.T) {
	e := newWiringEnv(t)
	d := e.con.deps
	// The provider compares the redirect URI exactly: it is what `new URL(path, PUBLIC_URL)` makes.
	if d.RedirectURI != wiringOrigin+"/api/connections/callback" {
		t.Errorf("RedirectURI = %q", d.RedirectURI)
	}
	for name, v := range map[string]any{
		"Connections": d.Connections, "Pending": d.Pending, "Servers": d.Servers, "OAuth": d.OAuth,
		"Pkce": d.Pkce, "Box": d.Box, "Clock": d.Clock, "Audit": d.Audit, "service": e.con.service,
	} {
		if v == nil {
			t.Errorf("%s is nil", name)
		}
	}
	// One service: the routes are built from its completed dependencies (the one single flight) and
	// the catalog's credentials are bound to that same instance.
	if d.Flights == nil || d.Flights != e.con.service.Deps().Flights {
		t.Error("the routes' dependencies do not carry the service's single flight")
	}
	if e.c.creds.bound() != credentialResolver(e.con.service) {
		t.Error("the catalog's credentials are bound to another service instance")
	}
	m := e.con.module()
	if len(m.Jobs) != 0 || len(m.Sweeps) != 0 || len(m.Emits) != 0 || len(m.Handlers) != 0 || m.FirstStart != nil || len(m.Commands) != 0 {
		t.Errorf("connections has no job, sweep, event, handler, first start or command: %+v", m)
	}
}

func TestRedirectURIFollowsPublicURLLikeTheReference(t *testing.T) {
	for public, want := range map[string]string{
		"http://localhost:3000":        "http://localhost:3000/api/connections/callback",
		"https://gw.example.com":       "https://gw.example.com/api/connections/callback",
		"https://gw.example.com/":      "https://gw.example.com/api/connections/callback",
		"https://GW.example.com:443/x": "https://gw.example.com/api/connections/callback",
		"http://localhost:80":          "http://localhost/api/connections/callback",
	} {
		rt := bareRuntime(t)
		rt.Config.PublicURL = public
		id := newIdentityWiring(rt)
		cat := newCatalogWiring(rt, id)
		if got := connectionsWiringOver(rt, id, cat, &memConnections{}, &memPendingConnects{}).deps.RedirectURI; got != want {
			t.Errorf("PUBLIC_URL %q: %q, want %q", public, got, want)
		}
	}
}

// The default list assembles with connections fifth (the gateway follows), the catalog bound to
// it, and no event or handler of its own.
func TestTheDefaultModuleListHasConnectionsFifth(t *testing.T) {
	got := modules(bareRuntime(t))
	last := got[4]
	var paths []string
	for _, r := range last.Routes {
		paths = append(paths, r.Method+" "+r.Path)
	}
	for _, want := range []string{"GET /api/connections", "GET /api/connections/callback", "POST /api/connections/{server}/start", "PUT /api/connections/{server}/pat", "DELETE /api/connections/{server}"} {
		if !slices.Contains(paths, want) {
			t.Errorf("%s is not served: %v", want, paths)
		}
	}
	if _, err := assemble(got); err != nil {
		t.Fatal(err)
	}
}

// Nothing is nil after the wiring: the catalog's credentials reach the connections service.
func TestTheCatalogIsBoundToTheConnectionsServiceAfterWiring(t *testing.T) {
	e := newWiringEnv(t)
	if e.c.creds.bound() == nil {
		t.Fatal("the catalog's sync credentials are not bound to connections")
	}
	// A request travels through the real service: it reads the store of the connections module.
	store := e.con.deps.Connections.(*memConnections)
	got, err := e.c.deps.Credentials.PersonalSecret(context.Background(), catalog.ServerCallTarget{Slug: "gitea"}, "admin-1")
	if err != nil || got.Secret != "" || got.Failure != catalog.CredentialNeedsConnection {
		t.Fatalf("PersonalSecret = %+v, %v", got, err)
	}
	if store.gets.Load() != 1 {
		t.Fatalf("the connections store was read %d times, want 1", store.gets.Load())
	}
	for uid, want := range map[string]bool{"admin-1": true, "member-1": false, "nobody": false} {
		got, err := e.c.deps.Credentials.IsActiveAdmin(context.Background(), uid)
		if err != nil || got != want {
			t.Errorf("IsActiveAdmin(%s) = %v, %v; want %v", uid, got, err, want)
		}
	}
}

func TestACredentialRequestBeforeBindingIsAnErrorNotAPanic(t *testing.T) {
	e := newWiringEnv(t)
	w := catalogWiringOver(e.c.rt, e.w, catinfraRepos(memServers{}), noUpstream{})
	got, err := w.deps.Credentials.PersonalSecret(context.Background(), catalog.ServerCallTarget{Slug: "gitea"}, "admin-1")
	if !errors.Is(err, errCredentialsNotBound) || got.Secret != "" {
		t.Fatalf("PersonalSecret before binding = %+v, %v", got, err)
	}
}

func TestBindingTwiceOrToNothingIsRefused(t *testing.T) {
	e := newWiringEnv(t)
	for name, f := range map[string]func(){
		"twice": func() { e.c.creds.bind(fakeResolver{}) },
		"nil":   func() { (&lateSyncCredentials{}).bind(nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("binding %s did not panic", name)
				}
			}()
			f()
		}()
	}
}

type fakeResolver struct {
	res connections.CredentialResult
	err error
}

func (f fakeResolver) ResolveForSending(context.Context, string, connections.ServerCallTarget, bool) (connections.CredentialResult, error) {
	return f.res, f.err
}

// sendAsk is one ResolveForSending call as the adapter made it.
type sendAsk struct {
	userID       string
	target       connections.ServerCallTarget
	forceRefresh bool
}

type recordingResolver struct {
	fakeResolver
	asked *[]sendAsk
}

func (r recordingResolver) ResolveForSending(ctx context.Context, userID string, target connections.ServerCallTarget, forceRefresh bool) (connections.CredentialResult, error) {
	*r.asked = append(*r.asked, sendAsk{userID, target, forceRefresh})
	return r.fakeResolver.ResolveForSending(ctx, userID, target, forceRefresh)
}

// The mapping of what connections answers to what the catalog stores (SyncOwner.failureOf then
// syncFailureText: needs_connection and expired are both "An administrator needs to connect this
// server", unavailable is "The OAuth provider is unavailable").
func TestSyncCredentialsMapConnectionsAnswersToTheCatalogsFailures(t *testing.T) {
	cases := []struct {
		name string
		res  connections.CredentialResult
		want catalog.PersonalCredential
	}{
		{"a secret", connections.CredentialResult{Secret: "tok"}, catalog.PersonalCredential{Secret: "tok"}},
		{"needs_connection", connections.CredentialResult{Failure: connections.FailureNeedsConnection}, catalog.PersonalCredential{Failure: catalog.CredentialNeedsConnection}},
		{"expired", connections.CredentialResult{Failure: connections.FailureExpired}, catalog.PersonalCredential{Failure: catalog.CredentialExpired}},
		{"unavailable", connections.CredentialResult{Failure: connections.FailureUnavailable}, catalog.PersonalCredential{Failure: catalog.CredentialUnavailable}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var asked []sendAsk
			l := &lateSyncCredentials{}
			l.bind(recordingResolver{fakeResolver: fakeResolver{res: c.res}, asked: &asked})
			got, err := l.PersonalSecret(context.Background(), catalog.ServerCallTarget{Slug: "gitea"}, "u1")
			if err != nil || got != c.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, c.want)
			}
			// No forced refresh in a synchronisation: the reference does not ask for one.
			// The target the sync read is what the secret is checked against.
			if len(asked) != 1 || asked[0].userID != "u1" || asked[0].forceRefresh ||
				!reflect.DeepEqual(asked[0].target, catalog.ServerCallTarget{Slug: "gitea"}) {
				t.Fatalf("asked %+v", asked)
			}
		})
	}
	t.Run("an error is passed on, an unknown failure is refused", func(t *testing.T) {
		boom := errors.New("boom")
		l := &lateSyncCredentials{}
		l.bind(fakeResolver{err: boom})
		if _, err := l.PersonalSecret(context.Background(), catalog.ServerCallTarget{}, "u"); !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
		l2 := &lateSyncCredentials{}
		l2.bind(fakeResolver{res: connections.CredentialResult{Failure: "new_one"}})
		if _, err := l2.PersonalSecret(context.Background(), catalog.ServerCallTarget{}, "u"); err == nil {
			t.Error("an unknown failure must be an error")
		}
	})
}
