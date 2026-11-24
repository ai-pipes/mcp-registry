package app_test

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

// fakeClock is a clock tests move by hand.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// fakeOIDC answers with Next; a nil Next makes the exchange fail; Unavailable fails both methods.
type fakeOIDC struct {
	Next         *domain.OIDCClaims
	LastExchange *app.OIDCExchangeInput
	Unavailable  bool
}

func (o *fakeOIDC) AuthorizationURL(_ context.Context, in app.OIDCAuthorizationInput) (*url.URL, error) {
	if o.Unavailable {
		return nil, domain.Err(domain.CodeIDPUnavailable, "provider unreachable")
	}
	u, _ := url.Parse("https://id.example/authorize")
	q := u.Query()
	q.Set("state", in.State.Reveal())
	q.Set("nonce", in.Nonce.Reveal())
	q.Set("code_challenge", in.CodeChallenge)
	u.RawQuery = q.Encode()
	return u, nil
}

func (o *fakeOIDC) Exchange(_ context.Context, in app.OIDCExchangeInput) (domain.OIDCClaims, error) {
	o.LastExchange = &in
	if o.Unavailable {
		return domain.OIDCClaims{}, domain.Err(domain.CodeIDPUnavailable, "provider unreachable")
	}
	if o.Next == nil {
		return domain.OIDCClaims{}, fmt.Errorf("exchange failed")
	}
	return *o.Next, nil
}

// fakeLimiter allows everything unless Deny is set; it records every hit.
type fakeLimiter struct {
	Deny       bool
	RetryAfter int
	Hits       []string
}

func (l *fakeLimiter) Hit(key string) (bool, int) {
	l.Hits = append(l.Hits, key)
	return !l.Deny, l.RetryAfter
}
func (l *fakeLimiter) Check(string) (bool, int) { return !l.Deny, l.RetryAfter }

type fakeHasher struct{}

func (fakeHasher) Hex(s string) string       { return "h:" + s }
func (fakeHasher) Base64URL(s string) string { return "b:" + s }
func (fakeHasher) Equal(a, b string) bool    { return a == b }

type fakeRandom struct {
	n    int
	Fail error
}

func (r *fakeRandom) Base64URL(nBytes int) (string, error) {
	if r.Fail != nil {
		return "", r.Fail
	}
	r.n++
	return fmt.Sprintf("r%d-%s", r.n, strings.Repeat("x", nBytes)), nil
}

type fakeAudit struct {
	Entries []kernel.AuditEntry
	Fail    error
}

func (a *fakeAudit) Record(_ context.Context, e kernel.AuditEntry) error {
	if a.Fail != nil {
		return a.Fail
	}
	a.Entries = append(a.Entries, e)
	return nil
}

func (a *fakeAudit) Actions(action string) []kernel.AuditEntry {
	var out []kernel.AuditEntry
	for _, e := range a.Entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// store holds the in-memory state behind every fake repository. Insertion order is kept where the
// real stores order (users by creation).
type store struct {
	nextID   int
	users    []*domain.User
	tokens   []*app.TokenRecord
	sessions map[string]*app.SessionRecord
	pending  map[string]app.PendingLogin
	links    map[string]app.LoginLinkRecord
	calls    []string // repository calls that change state, in order
}

func newStore() *store {
	return &store{
		sessions: map[string]*app.SessionRecord{},
		pending:  map[string]app.PendingLogin{},
		links:    map[string]app.LoginLinkRecord{},
	}
}

func (s *store) userByID(id string) *domain.User {
	for _, u := range s.users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

type fakeUsers struct{ s *store }

func (r fakeUsers) FindByID(_ context.Context, id string) (*domain.User, error) {
	if u := r.s.userByID(id); u != nil {
		c := *u
		return &c, nil
	}
	return nil, nil
}

func (r fakeUsers) FindBySub(_ context.Context, sub string) (*domain.User, error) {
	for _, u := range r.s.users {
		if u.OIDCSub == sub {
			c := *u
			return &c, nil
		}
	}
	return nil, nil
}

func (r fakeUsers) UpsertFromLogin(_ context.Context, in app.UpsertLogin) (domain.User, error) {
	at := in.At
	for _, u := range r.s.users {
		if u.OIDCSub == in.Claims.Sub {
			u.Name, u.Email, u.Role = in.Claims.Name, in.Claims.Email, in.Role
			u.Groups = slices.Clone(in.Claims.Groups)
			u.LastLoginAt = &at
			return *u, nil
		}
	}
	r.s.nextID++
	u := &domain.User{
		ID: fmt.Sprintf("user-%d", r.s.nextID), OIDCSub: in.Claims.Sub, Name: in.Claims.Name,
		Email: in.Claims.Email, Groups: slices.Clone(in.Claims.Groups), Role: in.Role,
		CreatedAt: at, LastLoginAt: &at,
	}
	r.s.users = append(r.s.users, u)
	return *u, nil
}

func (r fakeUsers) List(context.Context) ([]domain.User, error) {
	out := make([]domain.User, 0, len(r.s.users))
	for _, u := range r.s.users {
		out = append(out, *u)
	}
	return out, nil
}

func (r fakeUsers) SetDisabled(_ context.Context, id string, disabled bool) (bool, error) {
	u := r.s.userByID(id)
	if u == nil {
		return false, nil
	}
	u.Disabled = disabled
	r.s.calls = append(r.s.calls, "users.setDisabled")
	return true, nil
}

func (r fakeUsers) TouchLastLogin(_ context.Context, id string, at time.Time) error {
	if u := r.s.userByID(id); u != nil {
		u.LastLoginAt = &at
	}
	return nil
}

type fakeTokens struct{ s *store }

func (r fakeTokens) Insert(_ context.Context, t app.NewToken) (app.TokenRecord, error) {
	r.s.nextID++
	rec := &app.TokenRecord{
		ID: fmt.Sprintf("tok-%d", r.s.nextID), UserID: t.UserID, Name: t.Name, Prefix: t.Prefix,
		Hash: t.Hash, CreatedAt: t.CreatedAt,
	}
	r.s.tokens = append(r.s.tokens, rec)
	return *rec, nil
}

func (r fakeTokens) find(match func(*app.TokenRecord) bool) *app.TokenRecord {
	for _, t := range r.s.tokens {
		if match(t) {
			c := *t
			return &c
		}
	}
	return nil
}

func (r fakeTokens) FindByHash(_ context.Context, hash string) (*app.TokenRecord, error) {
	return r.find(func(t *app.TokenRecord) bool { return t.Hash == hash }), nil
}

func (r fakeTokens) FindByID(_ context.Context, id string) (*app.TokenRecord, error) {
	return r.find(func(t *app.TokenRecord) bool { return t.ID == id }), nil
}

func (r fakeTokens) ListByUser(_ context.Context, userID string) ([]app.TokenRecord, error) {
	var out []app.TokenRecord
	for i := len(r.s.tokens) - 1; i >= 0; i-- { // newest first
		if t := r.s.tokens[i]; t.UserID == userID {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (r fakeTokens) Revoke(_ context.Context, id string, at time.Time) error {
	for _, t := range r.s.tokens {
		if t.ID == id && t.RevokedAt == nil {
			t.RevokedAt = &at
		}
	}
	return nil
}

func (r fakeTokens) RevokeAllByUser(_ context.Context, userID string, at time.Time) error {
	r.s.calls = append(r.s.calls, "tokens.revokeAllByUser")
	for _, t := range r.s.tokens {
		if t.UserID == userID && t.RevokedAt == nil {
			t.RevokedAt = &at
		}
	}
	return nil
}

func (r fakeTokens) Touch(_ context.Context, id string, at time.Time) error {
	for _, t := range r.s.tokens {
		if t.ID == id {
			t.LastUsedAt = &at
		}
	}
	return nil
}

type fakeSessions struct{ s *store }

func (r fakeSessions) Insert(_ context.Context, rec app.SessionRecord) error {
	r.s.sessions[rec.IDHash] = &rec
	return nil
}

func (r fakeSessions) FindByHash(_ context.Context, h string) (*app.SessionRecord, error) {
	if rec := r.s.sessions[h]; rec != nil {
		c := *rec
		return &c, nil
	}
	return nil, nil
}

func (r fakeSessions) Touch(_ context.Context, h string, at time.Time) error {
	if rec := r.s.sessions[h]; rec != nil {
		rec.LastSeenAt = at
	}
	return nil
}

func (r fakeSessions) Delete(_ context.Context, h string) error {
	delete(r.s.sessions, h)
	return nil
}

func (r fakeSessions) DeleteByUser(_ context.Context, userID string) error {
	r.s.calls = append(r.s.calls, "sessions.deleteByUser")
	for h, rec := range r.s.sessions {
		if rec.UserID == userID {
			delete(r.s.sessions, h)
		}
	}
	return nil
}

func (r fakeSessions) DeleteDead(_ context.Context, c domain.SessionCutoffs) (int, error) {
	n := 0
	for h, rec := range r.s.sessions {
		if !rec.ExpiresAt.After(c.ExpiresAtOrBefore) || rec.LastSeenAt.Before(c.LastSeenBefore) {
			delete(r.s.sessions, h)
			n++
		}
	}
	return n, nil
}

type fakePending struct{ s *store }

func (r fakePending) Insert(_ context.Context, p app.PendingLogin) error {
	r.s.pending[p.State.Reveal()] = p
	return nil
}

func (r fakePending) Consume(_ context.Context, state string) (*app.PendingLogin, error) {
	p, ok := r.s.pending[state]
	delete(r.s.pending, state)
	if !ok {
		return nil, nil
	}
	return &p, nil
}

type fakeLinks struct{ s *store }

func (r fakeLinks) Insert(_ context.Context, l app.LoginLinkRecord) error {
	r.s.links[l.CodeHash] = l
	return nil
}

func (r fakeLinks) Consume(_ context.Context, h string) (*app.LoginLinkRecord, error) {
	l, ok := r.s.links[h]
	delete(r.s.links, h)
	if !ok {
		return nil, nil
	}
	return &l, nil
}

func (r fakeLinks) DeleteByUser(_ context.Context, userID string) error {
	r.s.calls = append(r.s.calls, "links.deleteByUser")
	for h, l := range r.s.links {
		if l.UserID == userID {
			delete(r.s.links, h)
		}
	}
	return nil
}

func (r fakeLinks) DeleteExpired(_ context.Context, now time.Time) (int, error) {
	n := 0
	for h, l := range r.s.links {
		if !l.ExpiresAt.After(now) {
			delete(r.s.links, h)
			n++
		}
	}
	return n, nil
}

// fixture wires the fakes into app.Deps and keeps handles for assertions.
type fixture struct {
	Deps          app.Deps
	Store         *store
	Clock         *fakeClock
	OIDC          *fakeOIDC
	Audit         *fakeAudit
	TokenLimiter  *fakeLimiter
	LinkLimiter   *fakeLimiter
	CallbackLimit *fakeLimiter
	Random        *fakeRandom
}

func newFixture() *fixture {
	st := newStore()
	f := &fixture{
		Store:         st,
		Clock:         &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		OIDC:          &fakeOIDC{},
		Audit:         &fakeAudit{},
		TokenLimiter:  &fakeLimiter{},
		LinkLimiter:   &fakeLimiter{},
		CallbackLimit: &fakeLimiter{},
		Random:        &fakeRandom{},
	}
	f.Deps = app.Deps{
		Users: fakeUsers{st}, Tokens: fakeTokens{st}, Sessions: fakeSessions{st},
		PendingLogins: fakePending{st}, LoginLinks: fakeLinks{st}, OIDC: f.OIDC,
		Policy: domain.GroupPolicy{LoginGroup: "mcp-users", AdminGroup: "mcp-admins"},
		Hasher: fakeHasher{}, Random: f.Random,
		TokenLimiter: f.TokenLimiter, LinkLimiter: f.LinkLimiter, CallbackLimiter: f.CallbackLimit,
		Audit: f.Audit, Clock: f.Clock,
	}
	return f
}
