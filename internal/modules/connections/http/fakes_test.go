package http_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
	conhttp "github.com/ai-pipes/mcp-registry/internal/modules/connections/http"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

type fixedClock struct{}

func (fixedClock) Now() time.Time { return t0 }

func person(id string, groups ...string) identity.Principal {
	if groups == nil {
		groups = []string{}
	}
	return identity.Principal{UserID: id, Name: id, Role: identity.RoleMember, Groups: groups, Via: identity.ViaSession}
}

// The people of the tests are signed in by a cookie "sid=<user id>".
var people = map[string]identity.Principal{"alice": person("alice"), "bob": person("bob")}

// box seals reversibly, never in plain text.
type box struct{ id byte }

func (b box) Seal(plain string) ([]byte, error) {
	out := []byte{0xee, b.id}
	for _, c := range []byte(plain) {
		out = append(out, c^0x5a)
	}
	return out, nil
}

func (b box) Open(sealed []byte) (string, error) {
	if len(sealed) < 2 || sealed[0] != 0xee || sealed[1] != b.id {
		return "", errors.New("box: cannot open")
	}
	plain := make([]byte, 0, len(sealed))
	for _, c := range sealed[2:] {
		plain = append(plain, c^0x5a)
	}
	return string(plain), nil
}

type memStore struct {
	mu          sync.Mutex
	rows        map[string]domain.Snapshot
	failGet     error
	failDelete  error
	gets, locks int
	// cancelledWrites counts writes whose context had already ended.
	cancelledWrites int
}

func (s *memStore) counts() (gets, locks int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.locks
}

func (s *memStore) Get(_ context.Context, u, srv string) (*domain.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if r, ok := s.rows[u+"\x00"+srv]; ok {
		return domain.Restore(r), nil
	}
	return nil, nil
}

func (s *memStore) ListForUser(_ context.Context, u string) ([]*domain.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGet != nil {
		return nil, s.failGet
	}
	var out []*domain.Connection
	for _, r := range s.rows {
		if r.UserID == u {
			out = append(out, domain.Restore(r))
		}
	}
	return out, nil
}

func (s *memStore) WithLock(ctx context.Context, u, srv string, fn app.LockedFunc) error {
	s.mu.Lock()
	s.locks++
	s.mu.Unlock()
	cur, _ := s.Get(ctx, u, srv)
	w, err := fn(ctx, cur)
	if err != nil || w == nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		s.cancelledWrites++
	}
	s.rows[u+"\x00"+srv] = w.Snapshot()
	return nil
}

func (s *memStore) Delete(_ context.Context, u, srv string) (*domain.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failDelete != nil {
		return nil, s.failDelete
	}
	r, ok := s.rows[u+"\x00"+srv]
	if !ok {
		return nil, nil
	}
	delete(s.rows, u+"\x00"+srv)
	return domain.Restore(r), nil
}

func (s *memStore) ListKeys(context.Context) ([]app.ConnectionKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []app.ConnectionKey
	for _, r := range s.rows {
		out = append(out, app.ConnectionKey{UserID: r.UserID, Server: r.Server})
	}
	slices.SortFunc(out, func(a, b app.ConnectionKey) int {
		return strings.Compare(a.UserID+"\x00"+a.Server, b.UserID+"\x00"+b.Server)
	})
	return out, nil
}

func (s *memStore) has(u, srv string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rows[u+"\x00"+srv]
	return ok
}

type memPending struct {
	mu      sync.Mutex
	m       map[string]domain.PendingConnect
	failErr error
}

func (p *memPending) Insert(_ context.Context, pc domain.PendingConnect) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[pc.State()] = pc
	return nil
}

func (p *memPending) Consume(_ context.Context, state, userID string) (*domain.PendingConnect, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failErr != nil {
		return nil, p.failErr
	}
	// Like Postgres: a text that is not valid UTF-8 is an error, not "no such row".
	if !utf8.ValidString(state) {
		return nil, errors.New("pending: invalid byte sequence for encoding UTF8 (SQLSTATE 22021)")
	}
	pc, ok := p.m[state]
	if !ok || pc.UserID() != userID {
		return nil, nil
	}
	delete(p.m, state)
	return &pc, nil
}

func (p *memPending) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}

type counterPkce struct {
	mu sync.Mutex
	n  int
}

func (p *counterPkce) RandomToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	if p.n%2 == 1 {
		return "STATEMARK-" + strconv.Itoa((p.n+1)/2), nil
	}
	return "VERIFIERMARK-" + strconv.Itoa(p.n/2), nil
}

func (*counterPkce) Challenge(v string) string { return "S256(" + v + ")" }

type audit struct {
	mu   sync.Mutex
	list []string
}

func (a *audit) Record(_ context.Context, e kernel.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.list = append(a.list, e.Action)
	return nil
}

func (a *audit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.list)
}

type oauth struct {
	mu          sync.Mutex
	exchangeErr error
	revoked     []string
	codes       []string
	// refreshErr makes Refresh fail; entered is closed when the first Refresh starts and gate holds
	// that first call until it is closed.
	refreshErr error
	refreshes  int
	entered    chan struct{}
	gate       chan struct{}
}

func (*oauth) AuthorizeURL(_ catalog.OAuthClientConfig, in app.AuthorizeInput) (string, error) {
	return "https://idp.example.com/authorize?state=" + in.State + "&code_challenge=" + in.CodeChallenge, nil
}

func (o *oauth) ExchangeCode(ctx context.Context, _ catalog.OAuthClientConfig, _ app.ClientSecret, in app.ExchangeInput) (app.TokenSet, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ctx.Err() != nil { // a request that ended must not stop the exchange of a consumed state
		return app.TokenSet{}, ctx.Err()
	}
	o.codes = append(o.codes, in.Code)
	if o.exchangeErr != nil {
		return app.TokenSet{}, o.exchangeErr
	}
	return app.TokenSet{AccessToken: "ACCESS-1", RefreshToken: "REFRESH-1"}, nil
}

func (o *oauth) Refresh(ctx context.Context, _ catalog.OAuthClientConfig, _ app.ClientSecret, _ string) (app.TokenSet, error) {
	o.mu.Lock()
	o.refreshes++
	first := o.refreshes == 1
	err := o.refreshErr
	o.mu.Unlock()
	if first && o.entered != nil {
		close(o.entered)
		select {
		case <-o.gate:
		case <-ctx.Done():
			return app.TokenSet{}, ctx.Err()
		}
	}
	if err != nil {
		return app.TokenSet{}, err
	}
	exp := t0.Add(time.Hour)
	return app.TokenSet{AccessToken: "ACCESS-NEW", RefreshToken: "REFRESH-NEW", ExpiresAt: &exp}, nil
}

func (o *oauth) Revoke(_ context.Context, _ catalog.OAuthClientConfig, _ app.ClientSecret, token string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.revoked = append(o.revoked, token)
	return nil
}

type directory struct {
	mu      sync.Mutex
	servers []app.PersonalServer
	listErr error
}

func serverOf(slug, name string, kind catalog.AuthKind) app.PersonalServer {
	cfg := app.ServerConfig{Slug: slug, URL: "https://" + slug + ".example.com/mcp", AuthKind: kind}
	if kind == catalog.AuthOAuthUser {
		rev := "https://idp.example.com/revoke"
		cfg.OAuth = &catalog.OAuthClientConfig{ //nolint:gosec // G101: URLs and an id of a test server, no credential
			AuthorizeURL: "https://idp.example.com/authorize", TokenURL: "https://idp.example.com/token",
			ClientID: "client-1", Scopes: []string{}, RevokeURL: &rev,
		}
	}
	return app.PersonalServer{Slug: slug, Name: name, AuthKind: kind, Config: cfg}
}

func (d *directory) PersonalServersFor(context.Context, app.Principal) ([]app.PersonalServer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listErr != nil {
		return nil, d.listErr
	}
	return slices.Clone(d.servers), nil
}

func (d *directory) Config(_ context.Context, slug string) (*app.ServerConfig, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.servers {
		if s.Slug == slug {
			c := s.Config
			return &c, nil
		}
	}
	return nil, nil
}

func (*directory) ClientSecret(context.Context, string) (app.ClientSecret, error) {
	return app.ClientSecret{}, nil
}

func (d *directory) remove(slug string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.servers = slices.DeleteFunc(d.servers, func(s app.PersonalServer) bool { return s.Slug == slug })
}

// env is the module over fakes, behind the real route table with fake guards.
type env struct {
	h       http.Handler
	deps    app.Deps
	store   *memStore
	pending *memPending
	oauth   *oauth
	audit   *audit
	dir     *directory
	logs    *bytes.Buffer
	// sessionErr makes the session lookup of the callback fail.
	sessionErr error
}

type ctxKey struct{}

const redirectURI = "https://gw.example.com/api/connections/callback"

func newEnv(t testing.TB) *env {
	t.Helper()
	e := &env{
		store:   &memStore{rows: map[string]domain.Snapshot{}},
		pending: &memPending{m: map[string]domain.PendingConnect{}},
		oauth:   &oauth{},
		audit:   &audit{},
		dir: &directory{servers: []app.PersonalServer{
			serverOf("gitea", "Gitea", catalog.AuthOAuthUser),
			serverOf("tracker", "Tracker", catalog.AuthPATUser),
		}},
		logs: &bytes.Buffer{},
	}
	e.deps = app.Deps{
		Connections: e.store, Pending: e.pending, Servers: e.dir, OAuth: e.oauth, Pkce: &counterPkce{},
		Box: box{1}, Flights: app.NewSingleFlight[app.CredentialResult](), Clock: fixedClock{}, Audit: e.audit, RedirectURI: redirectURI,
	}
	session := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				httpx.WriteError(w, "unauthenticated", "tokens do not work on the web API")
				return
			}
			c, err := r.Cookie("sid")
			p, ok := people[""]
			if err == nil {
				p, ok = people[c.Value]
			}
			if !ok {
				httpx.WriteError(w, "unauthenticated", "sign in required")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
		})
	}
	g := conhttp.Guards{
		Session: session,
		Principal: func(r *http.Request) (identity.Principal, bool) {
			p, ok := r.Context().Value(ctxKey{}).(identity.Principal)
			return p, ok
		},
		SessionPrincipal: func(r *http.Request) (identity.Principal, bool, error) {
			if e.sessionErr != nil {
				return identity.Principal{}, false, e.sessionErr
			}
			if r.Header.Get("Authorization") != "" {
				return identity.Principal{}, false, nil
			}
			c, err := r.Cookie("sid")
			if err != nil {
				return identity.Principal{}, false, nil
			}
			p, ok := people[c.Value]
			return p, ok, nil
		},
	}
	logger := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	routes, prefixes := conhttp.RoutesWith(g, e.deps, conhttp.Options{Logger: logger})
	notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { httpx.WriteError(w, "not_found", "not found") })
	e.h = httpx.Chain(httpx.NewRouter(routes, prefixes, notFound), httpx.SecurityHeaders())
	return e
}

type reply struct {
	Status int
	Body   string
	Header http.Header
}

// do sends a request as the person ("" is nobody) with an optional JSON body.
func (e *env) do(t testing.TB, user, method, target, body string, headers ...string) reply {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rd)
	if user != "" {
		req.Header.Set("Cookie", "sid="+user)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return reply{Status: rec.Code, Body: rec.Body.String(), Header: rec.Header()}
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
