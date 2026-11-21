package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	catinfra "github.com/ai-pipes/mcp-registry/internal/modules/catalog/infra"
	idapp "github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	idinfra "github.com/ai-pipes/mcp-registry/internal/modules/identity/infra"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

// The composed identity wiring over in-memory adapters, run through the real composed handler.
// The limiters are the real ones of idinfra.NewPg (their numbers are pinned in that package); this
// file pins which limiter serves which use case and that the guards sit on the platform's routes.

type memUsers struct {
	idapp.UserRepository
	byID map[string]*domain.User
}

func (u *memUsers) FindByID(_ context.Context, id string) (*domain.User, error) {
	return u.byID[id], nil
}

type memSessions struct {
	idapp.SessionRepository
	byHash map[string]*idapp.SessionRecord
}

func (s *memSessions) FindByHash(_ context.Context, h string) (*idapp.SessionRecord, error) {
	return s.byHash[h], nil
}
func (s *memSessions) Touch(context.Context, string, time.Time) error { return nil }

type memTokens struct{ idapp.TokenRepository }

func (memTokens) FindByHash(context.Context, string) (*idapp.TokenRecord, error) { return nil, nil }

// memPending hands out a pending login for any state, so a callback reaches the code exchange.
type memPending struct{ idapp.PendingLoginRepository }

func (memPending) Consume(_ context.Context, state string) (*idapp.PendingLogin, error) {
	return &idapp.PendingLogin{State: state, ReturnTo: "/", ExpiresAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

type memLinks struct{ idapp.LoginLinkRepository }

func (memLinks) Consume(context.Context, string) (*idapp.LoginLinkRecord, error) { return nil, nil }

// rejectingOIDC refuses every code, as a provider does for a bad one.
type rejectingOIDC struct{ idapp.OIDCProvider }

func (rejectingOIDC) Exchange(context.Context, idapp.OIDCExchangeInput) (domain.OIDCClaims, error) {
	return domain.OIDCClaims{}, context.DeadlineExceeded
}

type countingAudit struct{ actions []string }

func (a *countingAudit) Record(_ context.Context, e kernel.AuditEntry) error {
	a.actions = append(a.actions, e.Action)
	return nil
}

func (a *countingAudit) count(action string) int {
	n := 0
	for _, x := range a.actions {
		if x == action {
			n++
		}
	}
	return n
}

const (
	wiringOrigin = "https://gateway.example"
	adminCookie  = "admin-session"
	memberCookie = "member-session"
)

type wiringEnv struct {
	w     identityWiring
	c     catalogWiring
	h     http.Handler
	audit *countingAudit
	logs  *logBuf
}

func newWiringEnv(t *testing.T) *wiringEnv {
	t.Helper()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	au := &countingAudit{}
	logs := &logBuf{}
	rt := Runtime{
		Config: config.Config{PublicURL: wiringOrigin, SyncIntervalHours: 6, UpstreamMaxResponseBytes: 12345},
		Logger: logging.New("info", logs),
		Clock:  kernel.FixedClock{T: now}, Audit: au, Version: "9.9.9",
	}
	pg := idinfra.NewPg(nil, rt.Clock) // no pool: the repositories are replaced, the limiters are the real ones
	hasher := pg.Hasher
	sessions := &memSessions{byHash: map[string]*idapp.SessionRecord{}}
	for cookie, uid := range map[string]string{adminCookie: "admin-1", memberCookie: "member-1"} {
		sessions.byHash[hasher.Hex(cookie)] = &idapp.SessionRecord{
			IDHash: hasher.Hex(cookie), UserID: uid, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
		}
	}
	pg.Users = &memUsers{byID: map[string]*domain.User{
		"admin-1":  {ID: "admin-1", Name: "Ann", Role: domain.RoleAdmin, Groups: []string{}},
		"member-1": {ID: "member-1", Name: "Bob", Role: domain.RoleMember, Groups: []string{}},
	}}
	pg.Sessions, pg.Tokens, pg.PendingLogins, pg.LoginLinks = sessions, memTokens{}, memPending{}, memLinks{}

	w := identityWiringOver(rt, pg, rejectingOIDC{})
	cw := catalogWiringOver(rt, w, catinfra.Repos{Servers: memServers{}, Tools: memTools{}}, noUpstream{})
	asm, err := assemble([]Module{w.module(), platformModule(rt, w), cw.module()})
	if err != nil {
		t.Fatal(err)
	}
	h := handler(rt.Config, rt.Logger, nil, t.TempDir(), asm.routes, asm.prefixes)
	return &wiringEnv{w: w, c: cw, h: h, audit: au, logs: logs}
}

func (e *wiringEnv) do(method, target, body, ip string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = ip + ":40000"
	r.Header.Set("Origin", wiringOrigin)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func cookie(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: value}) } //nolint:gosec // G124: a request cookie
}

func TestComposedPlatformRoutesAreGuarded(t *testing.T) {
	e := newWiringEnv(t)
	cases := []struct {
		name, method, path string
		mod                func(*http.Request)
		want               int
	}{
		{"audit without a session", "GET", "/api/audit", nil, http.StatusUnauthorized},
		{"audit as a member", "GET", "/api/audit", cookie("mcp_session", memberCookie), http.StatusForbidden},
		{"audit with a token instead of a session", "GET", "/api/audit", func(r *http.Request) { r.Header.Set("Authorization", "Bearer sk_x") }, http.StatusUnauthorized},
		{"audit, any method, without a session", "DELETE", "/api/audit", nil, http.StatusUnauthorized},
		{"version without a session", "GET", "/api/version", nil, http.StatusUnauthorized},
		{"version as a member", "GET", "/api/version", cookie("mcp_session", memberCookie), http.StatusOK},
		{"version as an admin", "GET", "/api/version", cookie("mcp_session", adminCookie), http.StatusOK},
	}
	for _, c := range cases {
		if got := e.do(c.method, c.path, "", "198.51.100.1", c.mod).Code; got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	if body := e.do("GET", "/api/version", "", "198.51.100.1", cookie("mcp_session", adminCookie)).Body.String(); body != `{"version":"9.9.9"}` {
		t.Errorf("version body %q", body)
	}
}

// One service is built for the module, and identity's own routes and the platform's routes use
// guards that agree on who is signed in.
func TestOneServiceServesBothModules(t *testing.T) {
	e := newWiringEnv(t)
	if e.w.service == nil {
		t.Fatal("no service")
	}
	r := e.do("GET", "/api/auth/me", "", "198.51.100.1", cookie("mcp_session", adminCookie))
	if r.Code != http.StatusOK {
		t.Fatalf("/api/auth/me: %d", r.Code)
	}
	ok, err := e.w.service.IsUserActiveAdmin(context.Background(), "admin-1")
	if err != nil || !ok {
		t.Fatalf("IsUserActiveAdmin: %v %v", ok, err)
	}
}

func TestTheThreeLimitersAreDistinctInstances(t *testing.T) {
	d := newWiringEnv(t).w.deps
	if d.TokenLimiter == d.LinkLimiter || d.LinkLimiter == d.CallbackLimiter || d.TokenLimiter == d.CallbackLimiter {
		t.Fatal("two of the limiters handed to the use cases are one instance")
	}
}

// Each limiter is spent only by its own failures: 10 link attempts, 30 token checks and 20 failed
// callbacks per address, each from an address of its own so one budget cannot hide another.
func TestEachFailureSpendsItsOwnLimiter(t *testing.T) {
	linkAttempt := func(e *wiringEnv, ip string) int {
		return e.do("POST", "/api/auth/link", `{"code":"ll_bogus"}`, ip, nil).Code
	}
	tokenCheck := func(e *wiringEnv, ip string) error {
		_, err := idapp.AuthenticateToken(context.Background(), e.w.deps, "sk_bogus", ip)
		return err
	}
	rateLimited := func(err error) bool {
		de, ok := domain.AsError(err)
		return ok && de.Code == domain.CodeRateLimited
	}
	callback := func(e *wiringEnv, ip, state string) int {
		return e.do("GET", "/api/auth/callback?code=bad&state="+url.QueryEscape(state), "", ip, cookie("mcp_login", state)).Code
	}

	t.Run("link attempts: 10 per address, and they do not spend token checks", func(t *testing.T) {
		e := newWiringEnv(t)
		for i := 1; i <= 10; i++ {
			if got := linkAttempt(e, "203.0.113.10"); got != http.StatusUnauthorized {
				t.Fatalf("attempt %d: %d", i, got)
			}
		}
		if got := linkAttempt(e, "203.0.113.10"); got != http.StatusTooManyRequests {
			t.Fatalf("attempt 11: %d, want 429", got)
		}
		for i := 1; i <= 30; i++ {
			if err := tokenCheck(e, "203.0.113.10"); rateLimited(err) {
				t.Fatalf("token check %d was limited after link attempts", i)
			}
		}
		if err := tokenCheck(e, "203.0.113.10"); !rateLimited(err) {
			t.Fatalf("token check 31: %v, want rate_limited", err)
		}
	})
	t.Run("token checks: 30 per address, and they do not spend link attempts", func(t *testing.T) {
		e := newWiringEnv(t)
		for i := 1; i <= 30; i++ {
			if err := tokenCheck(e, "203.0.113.20"); rateLimited(err) {
				t.Fatalf("token check %d limited", i)
			}
		}
		if err := tokenCheck(e, "203.0.113.20"); !rateLimited(err) {
			t.Fatalf("token check 31: %v", err)
		}
		for i := 1; i <= 10; i++ {
			if got := linkAttempt(e, "203.0.113.20"); got != http.StatusUnauthorized {
				t.Fatalf("link attempt %d after token failures: %d", i, got)
			}
		}
	})
	t.Run("failed callbacks: 20 audited per address, apart from the other two", func(t *testing.T) {
		e := newWiringEnv(t)
		for i := 1; i <= 25; i++ {
			if got := callback(e, "203.0.113.30", "state"); got != http.StatusFound {
				t.Fatalf("callback %d: %d", i, got)
			}
		}
		if n := e.audit.count("auth.login_failed"); n != 20 {
			t.Fatalf("%d failed callbacks audited out of 25, want 20", n)
		}
		if got := linkAttempt(e, "203.0.113.30"); got != http.StatusUnauthorized {
			t.Fatalf("link attempt after callbacks: %d", got)
		}
		if err := tokenCheck(e, "203.0.113.30"); rateLimited(err) {
			t.Fatal("token check limited after callbacks")
		}
	})
}
