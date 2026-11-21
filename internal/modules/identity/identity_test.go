package identity_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type users map[string]*domain.User

func (u users) FindByID(_ context.Context, id string) (*domain.User, error) {
	if x := u[id]; x != nil {
		c := *x
		return &c, nil
	}
	return nil, nil
}
func (u users) FindBySub(context.Context, string) (*domain.User, error) { return nil, nil }
func (u users) UpsertFromLogin(context.Context, app.UpsertLogin) (domain.User, error) {
	return domain.User{}, nil
}
func (u users) List(context.Context) ([]domain.User, error)             { return nil, nil }
func (u users) SetDisabled(context.Context, string, bool) (bool, error) { return false, nil }
func (u users) TouchLastLogin(context.Context, string, time.Time) error { return nil }

type sessions map[string]*app.SessionRecord

func (s sessions) Insert(context.Context, app.SessionRecord) error { return nil }
func (s sessions) FindByHash(_ context.Context, h string) (*app.SessionRecord, error) {
	if x := s[h]; x != nil {
		c := *x
		return &c, nil
	}
	return nil, nil
}
func (s sessions) Touch(context.Context, string, time.Time) error { return nil }
func (s sessions) Delete(context.Context, string) error           { return nil }
func (s sessions) DeleteByUser(context.Context, string) error     { return nil }
func (s sessions) DeleteDead(context.Context, domain.SessionCutoffs) (int, error) {
	return 0, nil
}

type tokens struct{ rec *app.TokenRecord }

func (t tokens) Insert(context.Context, app.NewToken) (app.TokenRecord, error) {
	return app.TokenRecord{}, nil
}

func (t tokens) FindByHash(_ context.Context, h string) (*app.TokenRecord, error) {
	if t.rec != nil && t.rec.Hash == h {
		return t.rec, nil
	}
	return nil, nil
}
func (t tokens) FindByID(context.Context, string) (*app.TokenRecord, error)    { return nil, nil }
func (t tokens) ListByUser(context.Context, string) ([]app.TokenRecord, error) { return nil, nil }
func (t tokens) Revoke(context.Context, string, time.Time) error               { return nil }
func (t tokens) RevokeAllByUser(context.Context, string, time.Time) error      { return nil }
func (t tokens) Touch(context.Context, string, time.Time) error                { return nil }

type hasher struct{}

func (hasher) Hex(s string) string       { return "h:" + s }
func (hasher) Base64URL(s string) string { return s }
func (hasher) Equal(a, b string) bool    { return a == b }

type limiter struct {
	hits  []string
	allow bool
	after int
}

func (l *limiter) Hit(k string) (bool, int) { l.hits = append(l.hits, k); return l.allow, l.after }
func (l *limiter) Check(string) (bool, int) { return l.allow, l.after }

type clock struct{}

func (clock) Now() time.Time { return now }

type setup struct {
	deps    app.Deps
	limiter *limiter
}

func newSetup() *setup {
	lim := &limiter{allow: true}
	u := users{"u1": {ID: "u1", Name: "Ann", Role: domain.RoleAdmin, Groups: []string{"g"}}, "u2": {ID: "u2", Name: "Bob", Role: domain.RoleMember, Disabled: false}}
	return &setup{limiter: lim, deps: app.Deps{
		Users: u,
		Sessions: sessions{
			"h:sess1": {IDHash: "h:sess1", UserID: "u1", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)},
			"h:sess2": {IDHash: "h:sess2", UserID: "u2", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)},
		},
		Tokens:       tokens{&app.TokenRecord{ID: "t1", UserID: "u1", Hash: "h:sk_good", CreatedAt: now}},
		Hasher:       hasher{},
		TokenLimiter: lim,
		Clock:        clock{},
	}}
}

func serve(h http.Handler, r *http.Request) *http.Response {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func request(opts ...func(*http.Request)) *http.Request {
	r := httptest.NewRequest("GET", "/x", nil)
	for _, o := range opts {
		o(r)
	}
	return r
}

func auth(v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", v) }
}

func sess(v string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "mcp_session", Value: v}) } //nolint:gosec // G124: a request cookie, the attributes are the browser's
}

func echo(w http.ResponseWriter, r *http.Request) {
	p, ok := identity.PrincipalOf(r)
	if !ok {
		http.Error(w, "no principal", 500)
		return
	}
	_, _ = w.Write([]byte(p.UserID + "/" + string(p.Via) + "/" + p.TokenID))
}

func body(r *http.Response) string {
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}

func TestSessionPrincipal(t *testing.T) {
	s := newSetup()
	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{"valid cookie", request(sess("sess1")), "u1"},
		{"no cookie", request(), ""},
		{"unknown session", request(sess("nope")), ""},
		{"overlong cookie", request(sess(strings.Repeat("a", 65))), ""},
		{"Authorization present even with a valid cookie", request(sess("sess1"), auth("Bearer sk_good")), ""},
	}
	for _, c := range cases {
		p, ok, err := identity.SessionPrincipal(s.deps, c.req)
		if err != nil || ok != (c.want != "") || p.UserID != c.want {
			t.Errorf("%s: %+v %v %v", c.name, p, ok, err)
		}
	}
	if p, _, _ := identity.SessionPrincipal(s.deps, request(sess("sess1"))); p.Via != domain.ViaSession {
		t.Errorf("via = %q", p.Via)
	}
}

func TestRequireSessionAndAdmin(t *testing.T) {
	s := newSetup()
	h := httpx.Chain(http.HandlerFunc(echo), identity.RequireSession(s.deps), identity.RequireAdmin())
	if r := serve(h, request(sess("sess1"))); r.StatusCode != 200 || body(r) != "u1/session/" {
		t.Errorf("admin: %d", r.StatusCode)
	}
	if r := serve(h, request(sess("sess2"))); r.StatusCode != 403 || body(r) != `{"error":{"code":"forbidden","message":"admin only"}}` {
		t.Errorf("member: %d", r.StatusCode)
	}
	if r := serve(h, request()); r.StatusCode != 401 || body(r) != `{"error":{"code":"unauthenticated","message":"sign in required"}}` {
		t.Errorf("anonymous: %d", r.StatusCode)
	}
	if r := serve(h, request(sess("sess1"), auth("Bearer sk_good"))); r.StatusCode != 401 || body(r) != `{"error":{"code":"unauthenticated","message":"tokens do not work on the web API"}}` {
		t.Errorf("Authorization: %d", r.StatusCode)
	}
	if r := serve(h, request(sess("sess1"), auth(""))); r.StatusCode != 401 {
		t.Errorf("empty Authorization: %d", r.StatusCode)
	}
	if _, ok := identity.PrincipalOf(request()); ok {
		t.Error("principal on a bare request")
	}
}

func TestRequireBearerToken(t *testing.T) {
	s := newSetup()
	h := httpx.Chain(http.HandlerFunc(echo), httpx.ClientIP(false, 1), identity.RequireBearerToken(s.deps))
	ok := func(v string) {
		t.Helper()
		if r := serve(h, request(auth(v))); r.StatusCode != 200 || body(r) != "u1/token/t1" {
			t.Errorf("%q: %d", v, r.StatusCode)
		}
	}
	ok("Bearer sk_good")
	ok("bearer sk_good")
	ok("BEARER \t sk_good")

	missingHeaderBody := `{"error":{"code":"unauthenticated","message":"bearer token required"}}`
	for _, v := range []string{"", "Basic abc", "Bearer", "Bearer ", "Bearer sk_good extra", "Token sk_good", "Bearersk_good"} {
		var opts []func(*http.Request)
		if v != "" {
			opts = append(opts, auth(v))
		}
		r := serve(h, request(opts...))
		if r.StatusCode != 401 || body(r) != missingHeaderBody || r.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%q: %d %s", v, r.StatusCode, r.Header.Get("WWW-Authenticate"))
		}
	}
	if len(s.limiter.hits) != 0 {
		t.Errorf("requests without a token were counted: %v", s.limiter.hits)
	}
	// The cookie is ignored.
	if r := serve(h, request(sess("sess1"))); r.StatusCode != 401 {
		t.Errorf("cookie accepted: %d", r.StatusCode)
	}

	// An invalid token: 401 invalid_token with the header, counted for the client address.
	r := serve(h, request(auth("Bearer nope")))
	if r.StatusCode != 401 || body(r) != `{"error":{"code":"invalid_token","message":"invalid token"}}` || r.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("invalid: %d", r.StatusCode)
	}
	if len(s.limiter.hits) != 1 || s.limiter.hits[0] != "192.0.2.1" {
		t.Errorf("limiter keys = %v", s.limiter.hits)
	}

	// A refused address: 429, no WWW-Authenticate, one positive-integer Retry-After.
	s.limiter.allow, s.limiter.after = false, 37
	r = serve(h, request(auth("Bearer sk_unknown")))
	ra := r.Header.Values("Retry-After")
	if n, err := strconv.Atoi(strings.Join(ra, ",")); r.StatusCode != 429 || len(ra) != 1 || err != nil || n != 37 ||
		r.Header.Get("WWW-Authenticate") != "" || body(r) != `{"error":{"code":"rate_limited","message":"too many failed attempts"}}` {
		t.Errorf("limited: %d %v", r.StatusCode, ra)
	}
	// A valid token still passes while locked out.
	ok("Bearer sk_good")
}

func TestIsUserActiveAdmin(t *testing.T) {
	s := newSetup()
	for id, want := range map[string]bool{"u1": true, "u2": false, "gone": false} {
		if got, err := identity.IsUserActiveAdmin(context.Background(), s.deps, id); err != nil || got != want {
			t.Errorf("%s: %v %v", id, got, err)
		}
	}
}

var _ kernel.Clock = clock{}
