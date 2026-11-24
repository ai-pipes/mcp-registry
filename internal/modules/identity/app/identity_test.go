package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

var ctx = context.Background()

// signIn stores a person as a first OIDC login would and opens a session for them.
func signIn(t *testing.T, f *fixture, sub string, groups []string) (domain.Principal, string) {
	t.Helper()
	role, ok := domain.AccessFor(groups, f.Deps.Policy)
	if !ok {
		t.Fatalf("groups %v open no door", groups)
	}
	u, err := f.Deps.Users.UpsertFromLogin(ctx, app.UpsertLogin{
		Claims: domain.OIDCClaims{Sub: sub, Name: sub, Groups: groups}, Role: role, At: f.Clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "session-of-" + sub
	now := f.Clock.Now()
	if err := f.Deps.Sessions.Insert(ctx, app.SessionRecord{
		IDHash: f.Deps.Hasher.Hex(sessionID), UserID: u.ID, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: domain.SessionExpiry(now),
	}); err != nil {
		t.Fatal(err)
	}
	p, err := app.AuthenticateSession(ctx, f.Deps, sessionID)
	if err != nil || p == nil {
		t.Fatalf("no session: %v %v", p, err)
	}
	return *p, sessionID
}

func member(id string) domain.Principal {
	return domain.Principal{UserID: id, Name: "m", Role: domain.RoleMember, Groups: []string{}, Via: domain.ViaSession}
}

func wantCode(t *testing.T, err error, code string) *domain.Error {
	t.Helper()
	e, ok := domain.AsError(err)
	if !ok || e.Code != code {
		t.Fatalf("error = %v, want code %q", err, code)
	}
	return e
}

func TestAuthenticateSessionRefreshesLastSeen(t *testing.T) {
	f := newFixture()
	_, id := signIn(t, f, "a", []string{"mcp-users"})
	f.Clock.Advance(11 * time.Hour)
	if p, err := app.AuthenticateSession(ctx, f.Deps, id); err != nil || p == nil {
		t.Fatalf("first: %v %v", p, err)
	}
	f.Clock.Advance(11 * time.Hour) // the idle clock restarted at the touch above
	if p, err := app.AuthenticateSession(ctx, f.Deps, id); err != nil || p == nil {
		t.Fatalf("second: %v %v", p, err)
	}
}

func TestAuthenticateSessionDropsIdleSession(t *testing.T) {
	f := newFixture()
	_, id := signIn(t, f, "a", []string{"mcp-users"})
	f.Clock.Advance(12*time.Hour + time.Nanosecond)
	if p, err := app.AuthenticateSession(ctx, f.Deps, id); err != nil || p != nil {
		t.Fatalf("got %v %v", p, err)
	}
	if len(f.Store.sessions) != 0 {
		t.Fatal("dead session row kept")
	}
}

func TestAuthenticateSessionBoundaries(t *testing.T) {
	f := newFixture()
	_, id := signIn(t, f, "a", []string{"mcp-users"})
	f.Clock.Advance(12 * time.Hour) // exactly 12h idle is still alive
	if p, _ := app.AuthenticateSession(ctx, f.Deps, id); p == nil {
		t.Fatal("alive at exactly 12h idle")
	}
	h := f.Deps.Hasher.Hex(id)
	f.Store.sessions[h].ExpiresAt = f.Clock.Now() // absolute limit: now < expiresAt fails
	if p, _ := app.AuthenticateSession(ctx, f.Deps, id); p != nil {
		t.Fatal("dead at the absolute limit")
	}
}

func TestAuthenticateSessionUnknownAndDisabled(t *testing.T) {
	f := newFixture()
	if p, err := app.AuthenticateSession(ctx, f.Deps, "nope"); p != nil || err != nil {
		t.Fatalf("unknown: %v %v", p, err)
	}
	_, id := signIn(t, f, "a", []string{"mcp-users"})
	f.Store.users[0].Disabled = true
	if p, err := app.AuthenticateSession(ctx, f.Deps, id); p != nil || err != nil {
		t.Fatalf("disabled: %v %v", p, err)
	}
	if len(f.Store.sessions) != 1 {
		t.Fatal("a disabled person's session row must be kept by this path")
	}
}

func TestAuthenticateSessionPrincipalGroupsNeverNil(t *testing.T) {
	f := newFixture()
	_, id := signIn(t, f, "a", []string{"mcp-users"})
	f.Store.users[0].Groups = nil
	p, err := app.AuthenticateSession(ctx, f.Deps, id)
	if err != nil || p == nil || p.Groups == nil {
		t.Fatalf("principal = %+v, %v", p, err)
	}
}

func TestLogout(t *testing.T) {
	f := newFixture()
	p, id := signIn(t, f, "a", []string{"mcp-users"})
	if err := app.Logout(ctx, f.Deps, id); err != nil {
		t.Fatal(err)
	}
	if got, _ := app.AuthenticateSession(ctx, f.Deps, id); got != nil {
		t.Fatal("session survived logout")
	}
	if err := app.Logout(ctx, f.Deps, "nope"); err != nil {
		t.Fatal(err)
	}
	entries := f.Audit.Actions("auth.logout")
	if len(entries) != 1 {
		t.Fatalf("logout audits = %d, want 1 (unknown session is a no-op)", len(entries))
	}
	e := entries[0]
	if e.ActorID == nil || *e.ActorID != p.UserID || e.Target != nil || e.Meta != nil {
		t.Fatalf("entry = %+v", e)
	}
}

func TestPruneExpiredSessions(t *testing.T) {
	f := newFixture()
	_, live := signIn(t, f, "a", []string{"mcp-users"})
	_, idle := signIn(t, f, "b", []string{"mcp-users"})
	_, old := signIn(t, f, "c", []string{"mcp-users"})
	h := f.Deps.Hasher.Hex
	now := f.Clock.Now()
	f.Store.sessions[h(idle)].LastSeenAt = now.Add(-12*time.Hour - time.Nanosecond)
	f.Store.sessions[h(old)].ExpiresAt = now
	f.Store.links["l1"] = app.LoginLinkRecord{CodeHash: "l1", ExpiresAt: now}
	f.Store.links["l2"] = app.LoginLinkRecord{CodeHash: "l2", ExpiresAt: now.Add(time.Second)}
	n, err := app.PruneExpiredSessions(ctx, f.Deps)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v; want 2", n, err)
	}
	if _, ok := f.Store.sessions[h(live)]; !ok || len(f.Store.sessions) != 1 {
		t.Fatal("only the live session may remain")
	}
	if _, ok := f.Store.links["l1"]; ok {
		t.Error("expired link kept")
	}
	if _, ok := f.Store.links["l2"]; !ok {
		t.Error("live link deleted")
	}
	if n, _ := app.PruneExpiredSessions(ctx, f.Deps); n != 0 {
		t.Fatalf("second run pruned %d", n)
	}
}

func TestCreateToken(t *testing.T) {
	f := newFixture()
	p, _ := signIn(t, f, "a", []string{"mcp-users"})
	tok, err := app.CreateToken(ctx, f.Deps, p, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Token.Reveal(), "sk_") || tok.Prefix != tok.Token.Reveal()[:10] {
		t.Fatalf("token = %+v", tok)
	}
	rec := f.Store.tokens[0]
	if rec.Hash != "h:"+tok.Token.Reveal() || rec.UserID != p.UserID || rec.Name != "laptop" || !rec.CreatedAt.Equal(f.Clock.Now()) {
		t.Fatalf("stored = %+v", rec)
	}
	e := f.Audit.Actions("token.create")
	if len(e) != 1 || *e[0].ActorID != p.UserID || *e[0].Target != tok.ID ||
		e[0].Meta["name"] != "laptop" || e[0].Meta["prefix"] != tok.Prefix || len(e[0].Meta) != 2 {
		t.Fatalf("audit = %+v", e)
	}
	for _, en := range f.Audit.Entries {
		for _, v := range en.Meta {
			if v == tok.Token.Reveal() {
				t.Fatal("token value reached the audit log")
			}
		}
	}
}

func TestCreateTokenRandomFailure(t *testing.T) {
	f := newFixture()
	p, _ := signIn(t, f, "a", []string{"mcp-users"})
	f.Random.Fail = errors.New("no entropy")
	if _, err := app.CreateToken(ctx, f.Deps, p, "x"); err == nil {
		t.Fatal("want error")
	}
	if len(f.Store.tokens) != 0 {
		t.Fatal("token stored without randomness")
	}
}

func TestAuthenticateTokenSuccess(t *testing.T) {
	f := newFixture()
	p, _ := signIn(t, f, "a", []string{"mcp-users"})
	tok, _ := app.CreateToken(ctx, f.Deps, p, "laptop")
	got, err := app.AuthenticateToken(ctx, f.Deps, tok.Token.Reveal(), "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != p.UserID || got.Via != domain.ViaToken || got.TokenID != tok.ID || len(got.Groups) != 1 || got.Groups[0] != "mcp-users" {
		t.Fatalf("principal = %+v", got)
	}
	list, err := app.ListTokens(ctx, f.Deps, p)
	if err != nil || len(list) != 1 || list[0].LastUsedAt == nil {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if len(f.TokenLimiter.Hits) != 0 {
		t.Fatal("a success must not be counted by the limiter")
	}
}

func TestAuthenticateTokenFailuresAreAllCounted(t *testing.T) {
	f := newFixture()
	p, _ := signIn(t, f, "a", []string{"mcp-users"})
	live, _ := app.CreateToken(ctx, f.Deps, p, "live")
	revoked, _ := app.CreateToken(ctx, f.Deps, p, "revoked")
	if err := app.RevokeToken(ctx, f.Deps, p, revoked.ID); err != nil {
		t.Fatal(err)
	}
	ghost, _ := app.CreateToken(ctx, f.Deps, p, "ghost-owner")
	f.Store.tokens[2].UserID = "no-such-user"
	disabledOwner := member("")
	d, _ := f.Deps.Users.UpsertFromLogin(ctx, app.UpsertLogin{
		Claims: domain.OIDCClaims{Sub: "dis", Name: "dis", Groups: []string{"mcp-users"}}, Role: domain.RoleMember, At: f.Clock.Now(),
	})
	disabledOwner.UserID = d.ID
	dt, _ := app.CreateToken(ctx, f.Deps, disabledOwner, "dis")
	f.Store.users[1].Disabled = true

	cases := []struct {
		name  string
		token string
	}{
		{"wrong shape", "abc"},
		{"too long", "sk_" + strings.Repeat("a", 200)},
		{"unknown hash", "sk_unknown"},
		{"revoked", revoked.Token.Reveal()},
		{"owner missing", ghost.Token.Reveal()},
		{"owner disabled", dt.Token.Reveal()},
	}
	for i, c := range cases {
		_, err := app.AuthenticateToken(ctx, f.Deps, c.token, "ip")
		e := wantCode(t, err, domain.CodeInvalidToken)
		if e.Message != "invalid token" {
			t.Errorf("%s: message %q", c.name, e.Message)
		}
		if len(f.TokenLimiter.Hits) != i+1 {
			t.Errorf("%s: limiter hits = %d, want %d", c.name, len(f.TokenLimiter.Hits), i+1)
		}
	}
	// lastUsedAt is touched only after the owner checks pass.
	for _, i := range []int{2, 3} { // owner missing, owner disabled
		if f.Store.tokens[i].LastUsedAt != nil {
			t.Errorf("token %d: lastUsedAt touched although the owner check failed", i)
		}
	}
	// A hash that matches but is not equal in the constant-time check also counts.
	f.Deps.Hasher = unequalHasher{}
	if _, err := app.AuthenticateToken(ctx, f.Deps, live.Token.Reveal(), "ip"); err == nil {
		t.Fatal("compare mismatch accepted")
	}
	if got := len(f.TokenLimiter.Hits); got != len(cases)+1 {
		t.Errorf("mismatch hits = %d", got)
	}
}

type unequalHasher struct{ fakeHasher }

func (unequalHasher) Equal(string, string) bool { return false }

func TestAuthenticateTokenRateLimited(t *testing.T) {
	f := newFixture()
	f.TokenLimiter.Deny, f.TokenLimiter.RetryAfter = true, 30
	_, err := app.AuthenticateToken(ctx, f.Deps, "sk_x", "ip")
	e := wantCode(t, err, domain.CodeRateLimited)
	if e.RetryAfterSec != 30 || e.Message != "too many failed attempts" {
		t.Fatalf("error = %+v", e)
	}
	if f.TokenLimiter.Hits[0] != "ip" {
		t.Fatal("limiter key is the client ip")
	}
}

func TestAuthenticateTokenAcceptsValidTokenFromLockedOutIP(t *testing.T) {
	f := newFixture()
	p, _ := signIn(t, f, "a", []string{"mcp-users"})
	tok, _ := app.CreateToken(ctx, f.Deps, p, "x")
	f.TokenLimiter.Deny = true
	if _, err := app.AuthenticateToken(ctx, f.Deps, tok.Token.Reveal(), "ip"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateTokenTouchesLastUsedOncePerMinute(t *testing.T) {
	f := newFixture()
	p, _ := signIn(t, f, "a", []string{"mcp-users"})
	tok, _ := app.CreateToken(ctx, f.Deps, p, "x")
	use := func() {
		t.Helper()
		if _, err := app.AuthenticateToken(ctx, f.Deps, tok.Token.Reveal(), "ip"); err != nil {
			t.Fatal(err)
		}
	}
	use()
	first := *f.Store.tokens[0].LastUsedAt
	if !first.Equal(f.Clock.Now()) {
		t.Fatal("first use sets lastUsedAt")
	}
	f.Clock.Advance(59*time.Second + 999*time.Millisecond)
	use()
	if !f.Store.tokens[0].LastUsedAt.Equal(first) {
		t.Fatal("touched again inside a minute")
	}
	f.Clock.Advance(time.Millisecond) // exactly 60 s since the first touch
	use()
	if !f.Store.tokens[0].LastUsedAt.Equal(f.Clock.Now()) {
		t.Fatal("not touched at 60 s")
	}
}

func TestRevokeToken(t *testing.T) {
	f := newFixture()
	alice, _ := signIn(t, f, "alice", []string{"mcp-users"})
	bob, _ := signIn(t, f, "bob", []string{"mcp-users"})
	root, _ := signIn(t, f, "root", []string{"mcp-admins"})
	tok, _ := app.CreateToken(ctx, f.Deps, alice, "x")

	if e := wantCode(t, app.RevokeToken(ctx, f.Deps, bob, tok.ID), domain.CodeForbidden); e.Message != "not your token" {
		t.Errorf("message %q", e.Message)
	}
	if e := wantCode(t, app.RevokeToken(ctx, f.Deps, alice, "missing"), domain.CodeNotFound); e.Message != "token not found" {
		t.Errorf("message %q", e.Message)
	}
	if err := app.RevokeToken(ctx, f.Deps, root, tok.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.RevokeToken(ctx, f.Deps, alice, tok.ID); err != nil { // idempotent
		t.Fatal(err)
	}
	e := f.Audit.Actions("token.revoke")
	if len(e) != 1 || *e[0].ActorID != root.UserID || *e[0].Target != tok.ID || e[0].Meta != nil {
		t.Fatalf("audit = %+v", e)
	}
	if _, err := app.AuthenticateToken(ctx, f.Deps, tok.Token.Reveal(), "ip"); err == nil {
		t.Fatal("revoked token still works")
	}
}

func TestListTokensOwnNewestFirstNeverNil(t *testing.T) {
	f := newFixture()
	a, _ := signIn(t, f, "a", []string{"mcp-users"})
	b, _ := signIn(t, f, "b", []string{"mcp-users"})
	empty, err := app.ListTokens(ctx, f.Deps, a)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %#v, %v", empty, err)
	}
	first, _ := app.CreateToken(ctx, f.Deps, a, "first")
	second, _ := app.CreateToken(ctx, f.Deps, a, "second")
	_, _ = app.CreateToken(ctx, f.Deps, b, "other")
	list, _ := app.ListTokens(ctx, f.Deps, a)
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Name != "second" || list[0].Prefix != second.Prefix {
		t.Fatalf("summary = %+v", list[0])
	}
}

func TestListUsersAdminOnly(t *testing.T) {
	f := newFixture()
	root, _ := signIn(t, f, "root", []string{"mcp-admins"})
	signIn(t, f, "bob", []string{"mcp-users"})
	users, err := app.ListUsers(ctx, f.Deps, root)
	if err != nil || len(users) != 2 || users[0].Name != "root" || users[1].Name != "bob" {
		t.Fatalf("users = %+v, %v", users, err)
	}
	f.Store.users[1].Groups = nil
	users, _ = app.ListUsers(ctx, f.Deps, root)
	if users[1].Groups == nil {
		t.Fatal("groups must not be nil")
	}
	e := wantCode(t, func() error { _, err := app.ListUsers(ctx, f.Deps, member("x")); return err }(), domain.CodeForbidden)
	if e.Message != "admin only" {
		t.Fatalf("message %q", e.Message)
	}
}

func TestSetDisabledEndsEverythingAndEnablingRestoresNothing(t *testing.T) {
	f := newFixture()
	root, _ := signIn(t, f, "root", []string{"mcp-admins"})
	bob, sessionID := signIn(t, f, "bob", []string{"mcp-users"})
	tok, _ := app.CreateToken(ctx, f.Deps, bob, "x")
	f.Store.links["l"] = app.LoginLinkRecord{CodeHash: "l", UserID: bob.UserID, ExpiresAt: f.Clock.Now().Add(time.Hour)}
	f.Store.calls = nil

	if err := app.SetDisabled(ctx, f.Deps, app.SetDisabledInput{Principal: root, UserID: bob.UserID, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	want := []string{"users.setDisabled", "sessions.deleteByUser", "links.deleteByUser", "tokens.revokeAllByUser"}
	if strings.Join(f.Store.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", f.Store.calls, want)
	}
	if got, _ := app.AuthenticateSession(ctx, f.Deps, sessionID); got != nil {
		t.Fatal("session survived")
	}
	if len(f.Store.links) != 0 {
		t.Fatal("login link survived")
	}
	if f.Store.tokens[0].RevokedAt == nil || !f.Store.tokens[0].RevokedAt.Equal(f.Clock.Now()) {
		t.Fatal("token not revoked at now")
	}
	if _, err := app.AuthenticateToken(ctx, f.Deps, tok.Token.Reveal(), "ip"); err == nil {
		t.Fatal("token of a disabled person works")
	}

	// Enabling again restores nothing.
	f.Store.links["l"] = app.LoginLinkRecord{CodeHash: "l", UserID: bob.UserID}
	f.Store.calls = nil
	if err := app.SetDisabled(ctx, f.Deps, app.SetDisabledInput{Principal: root, UserID: bob.UserID, Disabled: false}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.Store.calls, ",") != "users.setDisabled" {
		t.Fatalf("enable touched more: %v", f.Store.calls)
	}
	if f.Store.tokens[0].RevokedAt == nil {
		t.Fatal("enable restored a token")
	}
	if got, _ := app.AuthenticateSession(ctx, f.Deps, sessionID); got != nil {
		t.Fatal("enable restored a session")
	}
	if len(f.Store.links) != 1 {
		t.Fatal("enable deleted a link")
	}

	audits := f.Audit.Actions("user.disable")
	if len(audits) != 2 || *audits[0].ActorID != root.UserID || *audits[0].Target != bob.UserID ||
		audits[0].Meta["disabled"] != true || audits[1].Meta["disabled"] != false {
		t.Fatalf("audits = %+v", audits)
	}
}

func TestSetDisabledAuditedEvenWhenNothingChanged(t *testing.T) {
	f := newFixture()
	root, _ := signIn(t, f, "root", []string{"mcp-admins"})
	bob, _ := signIn(t, f, "bob", []string{"mcp-users"})
	for i := 0; i < 2; i++ {
		if err := app.SetDisabled(ctx, f.Deps, app.SetDisabledInput{Principal: root, UserID: bob.UserID, Disabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.Audit.Actions("user.disable")); n != 2 {
		t.Fatalf("audits = %d, want 2", n)
	}
}

func TestSetDisabledGuards(t *testing.T) {
	f := newFixture()
	root, _ := signIn(t, f, "root", []string{"mcp-admins"})
	if e := wantCode(t, app.SetDisabled(ctx, f.Deps, app.SetDisabledInput{Principal: member("x"), UserID: root.UserID, Disabled: true}), domain.CodeForbidden); e.Message != "admin only" {
		t.Errorf("message %q", e.Message)
	}
	if e := wantCode(t, app.SetDisabled(ctx, f.Deps, app.SetDisabledInput{Principal: root, UserID: root.UserID, Disabled: true}), domain.CodeForbidden); e.Message != "cannot disable yourself" {
		t.Errorf("message %q", e.Message)
	}
	if e := wantCode(t, app.SetDisabled(ctx, f.Deps, app.SetDisabledInput{Principal: root, UserID: "ghost", Disabled: false}), domain.CodeNotFound); e.Message != "user not found" {
		t.Errorf("message %q", e.Message)
	}
	if err := app.SetDisabled(ctx, f.Deps, app.SetDisabledInput{Principal: root, UserID: root.UserID, Disabled: false}); err != nil {
		t.Fatalf("enabling yourself: %v", err)
	}
	if len(f.Audit.Actions("user.disable")) != 1 {
		t.Fatal("refusals must not be audited")
	}
}

func TestIsUserActiveAdmin(t *testing.T) {
	f := newFixture()
	root, _ := signIn(t, f, "root", []string{"mcp-admins"})
	ann, _ := signIn(t, f, "ann", []string{"mcp-admins"})
	bob, _ := signIn(t, f, "bob", []string{"mcp-users"})
	check := func(id string) bool {
		ok, err := app.IsUserActiveAdmin(ctx, f.Deps, id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !check(ann.UserID) || check(bob.UserID) || check("ghost") {
		t.Fatal("admin / member / unknown")
	}
	if err := app.SetDisabled(ctx, f.Deps, app.SetDisabledInput{Principal: root, UserID: ann.UserID, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if check(ann.UserID) {
		t.Fatal("disabled admin")
	}
	signIn(t, f, "root", []string{"mcp-users"}) // role as of the last login
	if check(root.UserID) {
		t.Fatal("demoted admin")
	}
}

func provision(t *testing.T, f *fixture, sub, name string, role app.RoleChoice) app.ProvisionResult {
	t.Helper()
	r, err := app.ProvisionUser(ctx, f.Deps, app.ProvisionInput{Sub: sub, Name: name, Role: role})
	if err != nil {
		t.Fatalf("provision %s: %v", sub, err)
	}
	return r
}

func TestProvisionUserCreatesAndActsAs(t *testing.T) {
	f := newFixture()
	r := provision(t, f, "owner", "Owner", app.RoleAdmin)
	if r.Role != domain.RoleAdmin || !r.Created || r.ID == "" {
		t.Fatalf("result = %+v", r)
	}
	p, err := app.ActAs(ctx, f.Deps, "owner")
	if err != nil || p.Name != "Owner" || p.Role != domain.RoleAdmin || len(p.Groups) != 1 || p.Groups[0] != "mcp-admins" || p.Via != domain.ViaSession {
		t.Fatalf("principal = %+v, %v", p, err)
	}
	e := f.Audit.Actions("user.provision")
	if len(e) != 1 || e[0].ActorID != nil || *e[0].Target != r.ID ||
		e[0].Meta["via"] != "cli" || e[0].Meta["role"] != "admin" || e[0].Meta["created"] != true || len(e[0].Meta) != 3 {
		t.Fatalf("audit = %+v", e)
	}
}

func TestProvisionUserIsIdempotentBySub(t *testing.T) {
	f := newFixture()
	a := provision(t, f, "bob", "Bob", app.RoleMember)
	b := provision(t, f, "bob", "Bob", app.RoleMember)
	if b.ID != a.ID || b.Role != domain.RoleMember || b.Created {
		t.Fatalf("b = %+v", b)
	}
	if len(f.Store.users) != 1 {
		t.Fatal("second person created")
	}
	if got := f.Audit.Actions("user.provision")[1].Meta["created"]; got != false {
		t.Fatalf("created = %v", got)
	}
}

func TestProvisionUserTrimsLikeJavaScript(t *testing.T) {
	f := newFixture()
	provision(t, f, " \u00a0\ufeffbob\u2003", "\n Bob \t", app.RoleMember)
	if u := f.Store.users[0]; u.OIDCSub != "bob" || u.Name != "Bob" {
		t.Fatalf("user = %+v", u)
	}
	// U+0085 is not JavaScript whitespace.
	_, err := app.ProvisionUser(ctx, f.Deps, app.ProvisionInput{Sub: "\u0085", Name: "x"})
	if err != nil {
		t.Fatalf("U+0085 is not whitespace: %v", err)
	}
}

func TestProvisionUserKeepsOtherGroupsAndSwapsPolicyGroup(t *testing.T) {
	f := newFixture()
	signIn(t, f, "carol", []string{"mcp-users", "devs"})
	r := provision(t, f, "carol", "Carol", app.RoleAdmin)
	if r.Role != domain.RoleAdmin || r.Created {
		t.Fatalf("result = %+v", r)
	}
	p, _ := app.ActAs(ctx, f.Deps, "carol")
	if len(p.Groups) != 2 || p.Groups[0] != "devs" || p.Groups[1] != "mcp-admins" {
		t.Fatalf("groups = %v", p.Groups)
	}
}

func TestProvisionUserKeepsEmailOfExistingPerson(t *testing.T) {
	f := newFixture()
	if _, err := f.Deps.Users.UpsertFromLogin(ctx, app.UpsertLogin{
		Claims: domain.OIDCClaims{Sub: "e", Name: "E", Email: ptr("e@x.io"), Groups: []string{"mcp-users"}},
		Role:   domain.RoleMember, At: f.Clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	provision(t, f, "e", "E", app.RoleMember)
	if got := f.Store.users[0].Email; got == nil || *got != "e@x.io" {
		t.Fatalf("email = %v", got)
	}
}

func TestProvisionUserRoleKeep(t *testing.T) {
	f := newFixture()
	signIn(t, f, "ann", []string{"mcp-admins", "devs"})
	r := provision(t, f, "ann", "Ann", app.RoleKeep)
	if r.Role != domain.RoleAdmin || r.Created {
		t.Fatalf("result = %+v", r)
	}
	u := f.Store.users[0]
	if u.Role != domain.RoleAdmin || len(u.Groups) != 2 || u.Groups[0] != "mcp-admins" || u.Groups[1] != "devs" {
		t.Fatalf("keep changed the person: %+v", u)
	}
	if m := f.Audit.Actions("user.provision")[0].Meta; m["role"] != "admin" || m["created"] != false {
		t.Fatalf("meta = %v", m)
	}

	// A member stays a member.
	signIn(t, f, "bob", []string{"mcp-users"})
	if r := provision(t, f, "bob", "Bob", app.RoleKeep); r.Role != domain.RoleMember {
		t.Fatalf("bob = %+v", r)
	}
	// A new person with "keep" is a member.
	if r := provision(t, f, "new", "New", app.RoleKeep); r.Role != domain.RoleMember || !r.Created {
		t.Fatalf("new = %+v", r)
	}
	// Explicit member demotes an admin.
	if r := provision(t, f, "ann", "Ann", app.RoleMember); r.Role != domain.RoleMember {
		t.Fatalf("demote = %+v", r)
	}
}

func TestProvisionUserSetsLastLoginForExistingPersonToo(t *testing.T) {
	f := newFixture()
	r := provision(t, f, "local-admin", "Admin", app.RoleAdmin)
	created := f.Store.users[0].CreatedAt
	if f.Store.users[0].LastLoginAt == nil || !f.Store.users[0].LastLoginAt.Equal(created) {
		t.Fatal("a new person has lastLoginAt = createdAt")
	}
	f.Clock.Advance(time.Hour)
	provision(t, f, "local-admin", "Admin", app.RoleKeep)
	u := f.Store.users[0]
	if u.ID != r.ID || u.LastLoginAt == nil || !u.LastLoginAt.Equal(f.Clock.Now()) {
		t.Fatalf("lastLoginAt = %v, want now", u.LastLoginAt)
	}
	if !u.CreatedAt.Equal(created) {
		t.Fatal("createdAt moved")
	}
}

func TestProvisionUserRefusals(t *testing.T) {
	f := newFixture()
	a := provision(t, f, "dan", "Dan", app.RoleMember)
	f.Store.userByID(a.ID).Disabled = true
	_, err := app.ProvisionUser(ctx, f.Deps, app.ProvisionInput{Sub: "dan", Name: "Dan"})
	if e := wantCode(t, err, domain.CodeDisabled); e.Message != "account is disabled" {
		t.Errorf("message %q", e.Message)
	}
	_, err = app.ActAs(ctx, f.Deps, "dan")
	_ = wantCode(t, err, domain.CodeDisabled)
	for _, in := range []app.ProvisionInput{{Sub: " ", Name: "X"}, {Sub: "x", Name: "  "}, {Sub: "", Name: ""}} {
		_, err := app.ProvisionUser(ctx, f.Deps, in)
		if e := wantCode(t, err, domain.CodeInvalidInput); e.Message != "sub and name must not be blank" {
			t.Errorf("message %q", e.Message)
		}
	}
	if n := len(f.Audit.Actions("user.provision")); n != 1 {
		t.Fatalf("refusals were audited: %d", n)
	}
}

func TestActAsUnknownSub(t *testing.T) {
	f := newFixture()
	_, err := app.ActAs(ctx, f.Deps, "ghost")
	if e := wantCode(t, err, domain.CodeNotFound); e.Message != `no user with sub "ghost"` {
		t.Fatalf("message %q", e.Message)
	}
}

type failingAudit struct{ err error }

func (a failingAudit) Record(context.Context, kernel.AuditEntry) error { return a.err }

func TestStoreAndAuditFailuresAreNotDomainErrors(t *testing.T) {
	f := newFixture()
	boom := errors.New("boom")
	f.Deps.Audit = failingAudit{boom}
	_, err := app.ProvisionUser(ctx, f.Deps, app.ProvisionInput{Sub: "a", Name: "A"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := domain.AsError(err); ok {
		t.Fatal("an infrastructure failure became a domain error")
	}
}

func ptr[T any](v T) *T { return &v }
