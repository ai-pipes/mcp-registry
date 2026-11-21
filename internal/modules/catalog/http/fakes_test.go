package http_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	cathttp "github.com/ai-pipes/mcp-registry/internal/modules/catalog/http"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

var t0 = time.Date(2026, 10, 30, 10, 0, 0, 123_000_000, time.UTC)

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

// fakeServers keeps servers like the Postgres repository: Save writes only at the version the
// aggregate was read at.
type fakeServers struct {
	mu    sync.Mutex
	rows  map[string]domain.ServerState
	tools *fakeTools
	// failFind makes FindBySlug and List fail with this error.
	failFind error
	// beforeSave runs inside Save before the version check.
	beforeSave func(slug string)
}

func stateOf(s *domain.Server, version int64) domain.ServerState {
	return domain.ServerState{
		Slug: s.Slug(), Version: version, Name: s.Name(), Description: s.Description(), URL: s.URL(), Auth: s.Auth(),
		SealedSecret: slices.Clone(s.SealedSecret()), Access: s.Access(), TimeoutMs: s.TimeoutMs(), DailyLimit: s.DailyLimit(),
		Status: s.Status(), LastSyncAt: s.LastSyncAt(), LastSyncError: s.LastSyncError(), SyncUserID: s.SyncUserID(),
		CreatedAt: s.CreatedAt(), UpdatedAt: s.UpdatedAt(),
	}
}

func (r *fakeServers) FindBySlug(_ context.Context, slug string) (*domain.Server, error) {
	if r.failFind != nil {
		return nil, r.failFind
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.rows[slug]
	if !ok {
		return nil, nil
	}
	return domain.RehydrateServer(st), nil
}

func (r *fakeServers) List(context.Context) ([]app.ServerListing, error) {
	if r.failFind != nil {
		return nil, r.failFind
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slugs := make([]string, 0, len(r.rows))
	for slug := range r.rows {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	out := make([]app.ServerListing, 0, len(slugs))
	for _, slug := range slugs {
		out = append(out, app.ServerListing{Server: domain.RehydrateServer(r.rows[slug]), ToolCount: r.tools.visibleCount(slug)})
	}
	return out, nil
}

func (r *fakeServers) Create(_ context.Context, s *domain.Server, _ []kernel.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.rows[s.Slug()]; taken {
		return kernel.NewDomainError(domain.CodeServerExists, `server "`+s.Slug()+`" already exists`)
	}
	r.rows[s.Slug()] = stateOf(s, 0)
	return nil
}

func (r *fakeServers) Save(_ context.Context, s *domain.Server, _ []kernel.Event) error {
	if r.beforeSave != nil {
		r.beforeSave(s.Slug())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.rows[s.Slug()]
	if !ok {
		return kernel.NewDomainError(domain.CodeNotFound, `server "`+s.Slug()+`" not found`)
	}
	if stored.Version != s.Version() {
		return kernel.NewDomainError(domain.CodeServerConflict, `server "`+s.Slug()+`" was changed meanwhile; reload it and try again`)
	}
	next := stateOf(s, stored.Version+1)
	next.LastSyncAt, next.LastSyncError, next.SyncUserID = stored.LastSyncAt, stored.LastSyncError, stored.SyncUserID
	if !s.SecretChanged() {
		next.SealedSecret = stored.SealedSecret
	}
	r.rows[s.Slug()] = next
	return nil
}

func (r *fakeServers) SaveSyncState(_ context.Context, slug string, rec domain.SyncRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.rows[slug]
	if !ok {
		return kernel.NewDomainError(domain.CodeNotFound, `server "`+slug+`" not found`)
	}
	s := domain.RehydrateServer(st)
	s.ApplySync(rec)
	st.LastSyncAt, st.LastSyncError, st.SyncUserID = s.LastSyncAt(), s.LastSyncError(), s.SyncUserID()
	r.rows[slug] = st
	return nil
}

func (r *fakeServers) ReplaceSealedSecret(context.Context, string, []byte, []byte) (bool, error) {
	return false, nil
}

func (r *fakeServers) row(slug string) (domain.ServerState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.rows[slug]
	return st, ok
}

type fakeTools struct {
	mu   sync.Mutex
	rows map[string]domain.ToolState
	// exists says whether a server is stored (ApplySnapshot refuses an unknown one).
	exists func(string) bool
}

func toolKey(server, name string) string { return server + "\x00" + name }

func toolStateOf(t *domain.Tool) domain.ToolState {
	return domain.ToolState{ToolSnapshot: t.Snapshot(), Server: t.Server(), Hidden: t.Hidden(), RemovedAt: t.RemovedAt(), SyncedAt: t.SyncedAt()}
}

func (r *fakeTools) visibleCount(server string) int {
	n := 0
	for _, st := range r.rows {
		if st.Server == server && domain.RehydrateTool(st).Visible() {
			n++
		}
	}
	return n
}

func (r *fakeTools) Find(_ context.Context, server, name string) (*domain.Tool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.rows[toolKey(server, name)]
	if !ok {
		return nil, nil
	}
	return domain.RehydrateTool(st), nil
}

func (r *fakeTools) list(server string) []*domain.Tool {
	var out []*domain.Tool
	for _, st := range r.rows {
		if st.Server == server {
			out = append(out, domain.RehydrateTool(st))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func (r *fakeTools) ListByServer(_ context.Context, server string) ([]*domain.Tool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.list(server), nil
}

func (r *fakeTools) Save(_ context.Context, t *domain.Tool, _ []kernel.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rows[toolKey(t.Server(), t.Name())]; !ok {
		return kernel.NewDomainError(domain.CodeNotFound, `tool "`+t.Ref()+`" not found`)
	}
	r.rows[toolKey(t.Server(), t.Name())] = toolStateOf(t)
	return nil
}

func (r *fakeTools) ApplySnapshot(_ context.Context, server string, snaps []domain.ToolSnapshot, now time.Time) (app.SnapshotResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exists != nil && !r.exists(server) {
		return app.SnapshotResult{}, kernel.NewDomainError(domain.CodeNotFound, `server "`+server+`" not found`)
	}
	rec := domain.ReconcileTools(server, r.list(server), snaps, now)
	for _, t := range rec.Upserts {
		r.rows[toolKey(t.Server(), t.Name())] = toolStateOf(t)
	}
	return app.SnapshotResult{ChangedRefs: rec.ChangedRefs, AddedRefs: rec.AddedRefs, RemovedRefs: rec.RemovedRefs}, nil
}

// fakeBox is reversible so a test can see what was sealed.
type fakeBox struct{}

func (fakeBox) Seal(plain string) ([]byte, error) {
	return []byte("k1:" + base64.StdEncoding.EncodeToString([]byte(plain))), nil
}

func (fakeBox) Open(sealed []byte) (string, error) {
	plain, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(string(sealed), "k1:"))
	return string(plain), err
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []kernel.AuditEntry
}

func (a *fakeAudit) Record(_ context.Context, e kernel.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func (a *fakeAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []string{}
	for _, e := range a.entries {
		out = append(out, e.Action)
	}
	return out
}

// fakeUpstream answers tools/list with Tools, or fails with Err.
type fakeUpstream struct {
	mu    sync.Mutex
	Tools []kernel.UpstreamTool
	Err   error
	// onList runs at the start of ListTools (to cancel the request while a sync is running).
	onList func()
	calls  int
}

func (u *fakeUpstream) ListTools(ctx context.Context, _ kernel.UpstreamTarget) ([]kernel.UpstreamTool, error) {
	u.mu.Lock()
	u.calls++
	hook := u.onList
	u.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return u.Tools, u.Err
}

func (u *fakeUpstream) CallTool(context.Context, kernel.UpstreamTarget, string, json.RawMessage) (kernel.UpstreamCallResult, error) {
	return nil, errors.New("not used")
}

// noCredentials never has a personal secret.
type noCredentials struct{}

func (noCredentials) IsActiveAdmin(context.Context, string) (bool, error) { return false, nil }
func (noCredentials) PersonalSecret(context.Context, app.ServerCallTarget, string) (app.PersonalCredential, error) {
	return app.PersonalCredential{Failure: domain.CredentialNeedsConnection}, nil
}

// ---- the session guards, faked: identity's own live in identity and need its application layer ----

type principalKey struct{}

// who is the header that says who is signed in: "admin", "member", "dev-member" (a member in the
// group dev); absent is nobody.
const who = "X-Test-Who"

var people = map[string]identity.Principal{
	"admin":  {UserID: "00000000-0000-4000-8000-000000000001", Name: "Ada", Role: identity.RoleAdmin, Groups: []string{}},
	"member": {UserID: "00000000-0000-4000-8000-000000000002", Name: "Max", Role: identity.RoleMember, Groups: []string{}},
	"dev":    {UserID: "00000000-0000-4000-8000-000000000003", Name: "Dev", Role: identity.RoleMember, Groups: []string{"dev"}},
}

func testGuards() cathttp.Guards {
	return cathttp.Guards{
		Session: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if len(r.Header.Values("Authorization")) > 0 {
					httpx.WriteError(w, "unauthenticated", "tokens do not work on the web API")
					return
				}
				p, ok := people[r.Header.Get(who)]
				if !ok {
					httpx.WriteError(w, "unauthenticated", "sign in required")
					return
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
			})
		},
		Admin: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p, _ := r.Context().Value(principalKey{}).(identity.Principal)
				if p.Role != identity.RoleAdmin {
					httpx.WriteError(w, "forbidden", "admin only")
					return
				}
				next.ServeHTTP(w, r)
			})
		},
		Principal: func(r *http.Request) (identity.Principal, bool) {
			p, ok := r.Context().Value(principalKey{}).(identity.Principal)
			return p, ok
		},
	}
}

// env is the route table over in-memory ports.
type env struct {
	t        *testing.T
	h        http.Handler
	deps     app.Deps
	servers  *fakeServers
	tools    *fakeTools
	audit    *fakeAudit
	upstream *fakeUpstream
	logs     *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	tools := &fakeTools{rows: map[string]domain.ToolState{}}
	servers := &fakeServers{rows: map[string]domain.ServerState{}, tools: tools}
	tools.exists = func(slug string) bool { _, ok := servers.row(slug); return ok }
	e := &env{t: t, servers: servers, tools: tools, audit: &fakeAudit{}, upstream: &fakeUpstream{}, logs: &bytes.Buffer{}}
	e.deps = app.Deps{
		Servers: servers, Tools: tools, Box: fakeBox{}, Clock: &fixedClock{t: t0}, Audit: e.audit,
		Upstream: e.upstream, Credentials: noCredentials{}, MaxResponseBytes: 1 << 20,
	}
	e.rebuild()
	return e
}

func (e *env) rebuild() {
	logger := slog.New(slog.NewJSONHandler(e.logs, nil))
	routes, prefixes := cathttp.RoutesWith(testGuards(), e.deps)
	e.h = httpx.Chain(httpx.NewRouter(routes, prefixes, httpx.NotFound()), httpx.RequestID(logger))
}

type reply struct {
	Status int
	Body   string
	Header http.Header
}

func (e *env) do(user, method, path string, body any, headers ...string) reply {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if user != "" {
		req.Header.Set(who, user)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return reply{Status: res.StatusCode, Body: string(out), Header: res.Header}
}

func (e *env) admin(method, path string, body any) reply { return e.do("admin", method, path, body) }

// addServer stores a server (and tools) through the use case, as an admin would.
func (e *env) addServer(slug string, secret *string, tools ...string) {
	e.t.Helper()
	_, err := app.CreateServer(context.Background(), e.deps, app.CreateServerCommand{
		Principal: people["admin"],
		Input: app.ServerInput{Slug: slug, Fields: app.ServerFields{
			Name: "Gitea", Description: "Git hosting", URL: "https://mcp.example.com/gitea",
			Auth: domain.AuthMode{Kind: domain.AuthNone}, Access: domain.Access{Kind: domain.AccessAll},
		}},
		Secret: secret,
	})
	if err != nil {
		e.t.Fatalf("create %s: %v", slug, err)
	}
	e.applyTools(slug, tools...)
}

func (e *env) applyTools(slug string, names ...string) {
	e.t.Helper()
	snaps := []domain.ToolSnapshot{}
	for _, n := range names {
		snaps = append(snaps, domain.ToolSnapshot{Name: n, Description: "does " + n, InputSchema: []byte(`{"type":"object"}`)})
	}
	if _, err := e.tools.ApplySnapshot(context.Background(), slug, snaps, t0); err != nil {
		e.t.Fatal(err)
	}
}

// errBody is the JSON error body for a code and message.
func errBody(code, message string) string {
	raw, _ := json.Marshal(map[string]map[string]string{"error": {"code": code, "message": message}})
	return string(raw)
}

func wantReply(t *testing.T, got reply, status int, body string) {
	t.Helper()
	if got.Status != status || got.Body != body {
		t.Fatalf("got %d %s\nwant %d %s", got.Status, got.Body, status, body)
	}
}

// validServer is a body that every test starts from.
func validServer(slug string) map[string]any {
	return map[string]any{
		"slug": slug, "name": "Gitea", "description": "Git hosting", "url": "https://mcp.example.com/gitea",
		"auth": map[string]any{"kind": "none"}, "access": map[string]any{"kind": "all"},
	}
}

func ptr[T any](v T) *T { return &v }
