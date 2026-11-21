package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

func person(t *testing.T, f *fixture, sub string, admin bool) app.ProvisionResult {
	t.Helper()
	role := app.RoleMember
	if admin {
		role = app.RoleAdmin
	}
	r, err := app.ProvisionUser(ctx, f.Deps, app.ProvisionInput{Sub: sub, Name: sub, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func issue(t *testing.T, f *fixture, sub string) app.IssuedLink {
	t.Helper()
	l, err := app.IssueLoginLink(ctx, f.Deps, app.IssueLoginLinkInput{Sub: sub})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func redeem(f *fixture, code, ip string) (string, error) {
	return app.CompleteLinkLogin(ctx, f.Deps, app.CompleteLinkLoginInput{Code: code, IP: ip})
}

func failedAudits(f *fixture) []map[string]any {
	var metas []map[string]any
	for _, e := range f.Audit.Actions("auth.login_failed") {
		metas = append(metas, e.Meta)
	}
	return metas
}

func TestLocalAdminSub(t *testing.T) {
	if app.LocalAdminSub != "local-admin" {
		t.Fatal(app.LocalAdminSub)
	}
}

func TestIssueLoginLinkStoresTheHashNotTheCode(t *testing.T) {
	f := newFixture()
	user := person(t, f, "alice", false)
	link := issue(t, f, "alice")
	if !strings.HasPrefix(link.Code, "ll_") {
		t.Fatalf("code = %q", link.Code)
	}
	stored, ok := f.Store.links[f.Deps.Hasher.Hex(link.Code)]
	if !ok || stored.UserID != user.ID || stored.CodeHash != f.Deps.Hasher.Hex(link.Code) {
		t.Fatalf("stored = %+v", stored)
	}
	want := f.Clock.Now().Add(15 * time.Minute)
	if !link.ExpiresAt.Equal(want) || !stored.ExpiresAt.Equal(want) || !stored.CreatedAt.Equal(f.Clock.Now()) {
		t.Fatalf("expiry %v / %v, want %v", link.ExpiresAt, stored.ExpiresAt, want)
	}
	for _, c := range f.Store.links {
		if c.CodeHash == link.Code {
			t.Fatal("the code itself is stored")
		}
	}
	entries := f.Audit.Actions("auth.link_issued")
	if len(entries) != 1 || entries[0].ActorID != nil || entries[0].Target == nil || *entries[0].Target != user.ID || len(entries[0].Meta) != 0 {
		t.Fatalf("audit = %+v", entries)
	}
	if strings.Contains(strings.Join(auditText(f), " "), link.Code) {
		t.Fatal("the code reached the audit log")
	}
}

func auditText(f *fixture) []string {
	var out []string
	for _, e := range f.Audit.Entries {
		s := e.Action
		if e.Target != nil {
			s += " " + *e.Target
		}
		for k, v := range e.Meta {
			s += " " + k + "=" + toString(v)
		}
		out = append(out, s)
	}
	return out
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestIssueLoginLinkReplacesTheEarlierLink(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	a := issue(t, f, "alice")
	b := issue(t, f, "alice")
	_, err := redeem(f, a.Code, "1.1.1.1")
	_ = wantCode(t, err, domain.CodeInvalidLink)
	if _, err := redeem(f, b.Code, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
}

func TestIssueLoginLinkDeletesTheOldLinkBeforeInserting(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	seq := f.record()
	issue(t, f, "alice")
	want := []string{"users.findBySub", "links.deleteByUser", "links.insert", "audit:auth.link_issued"}
	if !slices.Equal(seq.Calls, want) {
		t.Fatalf("calls = %v, want %v", seq.Calls, want)
	}
}

func TestIssueLoginLinkRefusals(t *testing.T) {
	f := newFixture()
	_, err := app.IssueLoginLink(ctx, f.Deps, app.IssueLoginLinkInput{Sub: "nobody"})
	e := wantCode(t, err, domain.CodeNotFound)
	if e.Message != `no user with sub "nobody"` {
		t.Fatalf("message = %q", e.Message)
	}
	user := person(t, f, "alice", false)
	f.Store.userByID(user.ID).Disabled = true
	_, err = app.IssueLoginLink(ctx, f.Deps, app.IssueLoginLinkInput{Sub: "alice"})
	e = wantCode(t, err, domain.CodeDisabled)
	if e.Message != "account is disabled" {
		t.Fatalf("message = %q", e.Message)
	}
	if len(f.Store.links) != 0 {
		t.Fatal("a link was stored")
	}
}

func TestIssueLoginLinkRandomFailure(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	boom := errors.New("no entropy")
	f.Random.Fail = boom
	if _, err := app.IssueLoginLink(ctx, f.Deps, app.IssueLoginLinkInput{Sub: "alice"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestCompleteLinkLoginSignsInOnce(t *testing.T) {
	f := newFixture()
	user := person(t, f, "alice", false)
	link := issue(t, f, "alice")
	f.Clock.Advance(time.Minute)
	sessionID, err := redeem(f, link.Code, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	u := f.Store.userByID(user.ID)
	if u.LastLoginAt == nil || !u.LastLoginAt.Equal(f.Clock.Now()) || u.LastLoginAt.Equal(u.CreatedAt) {
		t.Fatalf("lastLoginAt = %v, createdAt = %v", u.LastLoginAt, u.CreatedAt)
	}
	s := f.Store.sessions[f.Deps.Hasher.Hex(sessionID)]
	if s == nil || s.UserID != user.ID || !s.ExpiresAt.Equal(domain.SessionExpiry(f.Clock.Now())) ||
		!s.CreatedAt.Equal(f.Clock.Now()) || !s.LastSeenAt.Equal(f.Clock.Now()) {
		t.Fatalf("session = %+v", s)
	}
	logins := f.Audit.Actions("auth.login")
	if len(logins) != 1 || logins[0].ActorID == nil || *logins[0].ActorID != user.ID || logins[0].Target != nil ||
		len(logins[0].Meta) != 1 || logins[0].Meta["via"] != "link" {
		t.Fatalf("audit = %+v", logins)
	}
	if strings.Contains(strings.Join(auditText(f), " "), link.Code) || strings.Contains(strings.Join(auditText(f), " "), sessionID) {
		t.Fatal("a secret reached the audit log")
	}
	_, err = redeem(f, link.Code, "1.1.1.1")
	_ = wantCode(t, err, domain.CodeInvalidLink)
}

func TestCompleteLinkLoginSuccessOrderAndNoLimiter(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	link := issue(t, f, "alice")
	seq := f.record()
	if _, err := redeem(f, link.Code, "9.9.9.9"); err != nil {
		t.Fatal(err)
	}
	want := []string{"links.consume", "users.findById", "sessions.insert", "users.touchLastLogin", "audit:auth.login"}
	if !slices.Equal(seq.Calls, want) {
		t.Fatalf("calls = %v, want %v", seq.Calls, want)
	}
	if len(f.LinkLimiter.Hits) != 0 {
		t.Fatalf("a success touched the limiter: %v", f.LinkLimiter.Hits)
	}
}

func TestCompleteLinkLoginExpiredLinkIsRefusedAndAudited(t *testing.T) {
	f := newFixture()
	user := person(t, f, "alice", false)
	link := issue(t, f, "alice")
	f.Clock.Advance(15 * time.Minute) // expired from the very instant
	_, err := redeem(f, link.Code, "1.1.1.1")
	e := wantCode(t, err, domain.CodeInvalidLink)
	if e.Message != "the link is invalid or has expired" {
		t.Fatalf("message = %q", e.Message)
	}
	if len(f.Store.sessions) != 0 {
		t.Fatal("a session was opened")
	}
	if len(f.Store.links) != 0 {
		t.Fatal("an expired link was not burned")
	}
	entries := f.Audit.Actions("auth.login_failed")
	if len(entries) != 1 || entries[0].Meta["reason"] != "invalid_link" || len(entries[0].Meta) != 1 ||
		entries[0].ActorID == nil || *entries[0].ActorID != user.ID {
		t.Fatalf("audit = %+v", entries)
	}
}

func TestCompleteLinkLoginJustBeforeExpirySucceeds(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	link := issue(t, f, "alice")
	f.Clock.Advance(15*time.Minute - time.Nanosecond)
	if _, err := redeem(f, link.Code, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteLinkLoginDisabledPersonIsRefusedAndAudited(t *testing.T) {
	f := newFixture()
	user := person(t, f, "alice", false)
	link := issue(t, f, "alice")
	f.Store.userByID(user.ID).Disabled = true
	_, err := redeem(f, link.Code, "1.1.1.1")
	e := wantCode(t, err, domain.CodeDisabled)
	if e.Message != "account is disabled" {
		t.Fatalf("message = %q", e.Message)
	}
	if len(f.Store.sessions) != 0 || len(f.Store.links) != 0 {
		t.Fatal("session opened or link kept")
	}
	entries := f.Audit.Actions("auth.login_failed")
	if len(entries) != 1 || entries[0].Meta["reason"] != "disabled" || entries[0].ActorID == nil || *entries[0].ActorID != user.ID {
		t.Fatalf("audit = %+v", entries)
	}
}

func TestCompleteLinkLoginUnknownAndMalformedCodesWriteNoAudit(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	before := len(f.Audit.Entries)
	for _, code := range []string{"sk_x", "ll_unknown", "", "ll_" + strings.Repeat("x", 98)} {
		_, err := redeem(f, code, "1.1.1.1")
		_ = wantCode(t, err, domain.CodeInvalidLink)
	}
	if len(f.Audit.Entries) != before {
		t.Fatalf("audit grew: %+v", f.Audit.Entries[before:])
	}
	if len(f.LinkLimiter.Hits) != 4 {
		t.Fatalf("hits = %v, want one per refusal", f.LinkLimiter.Hits)
	}
}

func TestCompleteLinkLoginMalformedCodeDoesNotTouchTheStore(t *testing.T) {
	f := newFixture()
	seq := f.record()
	for _, code := range []string{"sk_x", "", "ll_" + strings.Repeat("x", 98), "LL_abc"} {
		_, err := redeem(f, code, "1.1.1.1")
		_ = wantCode(t, err, domain.CodeInvalidLink)
	}
	for _, c := range seq.Calls {
		if c != "linkLimiter.hit" {
			t.Fatalf("a malformed code reached %s (calls %v)", c, seq.Calls)
		}
	}
}

func TestCompleteLinkLoginCodeOfExactlyMaxLengthIsLookedUp(t *testing.T) {
	f := newFixture()
	seq := f.record()
	_, err := redeem(f, "ll_"+strings.Repeat("x", 97), "1.1.1.1")
	_ = wantCode(t, err, domain.CodeInvalidLink)
	if len(seq.Calls) == 0 || seq.Calls[0] != "links.consume" {
		t.Fatalf("calls = %v", seq.Calls)
	}
}

func TestCompleteLinkLoginPersonGoneIsInvalidLinkWithoutActor(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	link := issue(t, f, "alice")
	f.Store.users = nil
	_, err := redeem(f, link.Code, "1.1.1.1")
	_ = wantCode(t, err, domain.CodeInvalidLink)
	entries := f.Audit.Actions("auth.login_failed")
	if len(entries) != 1 || entries[0].ActorID != nil || entries[0].Meta["reason"] != "invalid_link" {
		t.Fatalf("audit = %+v", entries)
	}
}

func TestCompleteLinkLoginExpiredLinkOfAGonePersonHasNoActor(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	link := issue(t, f, "alice")
	f.Store.users = nil
	f.Clock.Advance(time.Hour)
	_, err := redeem(f, link.Code, "1.1.1.1")
	_ = wantCode(t, err, domain.CodeInvalidLink)
	entries := f.Audit.Actions("auth.login_failed")
	if len(entries) != 1 || entries[0].ActorID != nil {
		t.Fatalf("audit = %+v", entries)
	}
}

// The order of a refusal of a consumed link: burn, look up, audit, and only then the limiter.
func TestCompleteLinkLoginRefusalOrderAuditBeforeLimiter(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fixture, userID string)
		want  []string
	}{
		{
			"disabled", func(f *fixture, id string) { f.Store.userByID(id).Disabled = true },
			[]string{"links.consume", "users.findById", "audit:auth.login_failed:disabled", "linkLimiter.hit"},
		},
		{
			"expired", func(f *fixture, _ string) { f.Clock.Advance(time.Hour) },
			[]string{"links.consume", "users.findById", "audit:auth.login_failed:invalid_link", "linkLimiter.hit"},
		},
		{
			"gone", func(f *fixture, _ string) { f.Store.users = nil },
			[]string{"links.consume", "users.findById", "audit:auth.login_failed:invalid_link", "linkLimiter.hit"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture()
			user := person(t, f, "alice", false)
			link := issue(t, f, "alice")
			c.setup(f, user.ID)
			seq := f.record()
			if _, err := redeem(f, link.Code, "1.1.1.1"); err == nil {
				t.Fatal("want a refusal")
			}
			if !slices.Equal(seq.Calls, c.want) {
				t.Fatalf("calls = %v, want %v", seq.Calls, c.want)
			}
		})
	}
}

func TestCompleteLinkLoginUnknownCodeOrderConsumeThenLimiterNoAudit(t *testing.T) {
	f := newFixture()
	seq := f.record()
	_, err := redeem(f, "ll_unknown", "1.1.1.1")
	_ = wantCode(t, err, domain.CodeInvalidLink)
	want := []string{"links.consume", "linkLimiter.hit"}
	if !slices.Equal(seq.Calls, want) {
		t.Fatalf("calls = %v, want %v", seq.Calls, want)
	}
}

func TestCompleteLinkLoginLockedAddressGetsRateLimitedOverEveryReason(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fixture, userID string) string // returns the code to redeem
	}{
		{"malformed", func(*fixture, string) string { return "sk_x" }},
		{"unknown", func(*fixture, string) string { return "ll_unknown" }},
		{"disabled", func(f *fixture, id string) string {
			code := issue(t, f, "alice").Code
			f.Store.userByID(id).Disabled = true
			return code
		}},
		{"expired", func(f *fixture, _ string) string {
			code := issue(t, f, "alice").Code
			f.Clock.Advance(time.Hour)
			return code
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture()
			user := person(t, f, "alice", false)
			code := c.setup(f, user.ID)
			f.LinkLimiter.Deny, f.LinkLimiter.RetryAfter = true, 30
			_, err := redeem(f, code, "2.2.2.2")
			e := wantCode(t, err, domain.CodeRateLimited)
			if e.RetryAfterSec != 30 || e.Message != "too many failed attempts" {
				t.Fatalf("error = %+v", e)
			}
			if !slices.Equal(f.LinkLimiter.Hits, []string{"2.2.2.2"}) {
				t.Fatalf("hits = %v", f.LinkLimiter.Hits)
			}
		})
	}
}

func TestCompleteLinkLoginLockedAddressStillAuditsTheConsumedLink(t *testing.T) {
	f := newFixture()
	user := person(t, f, "alice", false)
	link := issue(t, f, "alice")
	f.Store.userByID(user.ID).Disabled = true
	f.LinkLimiter.Deny = true
	_, err := redeem(f, link.Code, "2.2.2.2")
	_ = wantCode(t, err, domain.CodeRateLimited)
	if m := failedAudits(f); len(m) != 1 || m[0]["reason"] != "disabled" {
		t.Fatalf("audit = %v", m)
	}
}

func TestCompleteLinkLoginValidCodeWorksFromALockedOutAddress(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	link := issue(t, f, "alice")
	f.LinkLimiter.Deny, f.LinkLimiter.RetryAfter = true, 30
	if _, err := redeem(f, link.Code, "2.2.2.2"); err != nil {
		t.Fatal(err)
	}
	if len(f.LinkLimiter.Hits) != 0 {
		t.Fatalf("hits = %v", f.LinkLimiter.Hits)
	}
}

func TestCompleteLinkLoginCountsEachFailureWithTheClientAddress(t *testing.T) {
	f := newFixture()
	_, _ = redeem(f, "ll_a", "3.3.3.3")
	_, _ = redeem(f, "sk_b", "4.4.4.4")
	if !slices.Equal(f.LinkLimiter.Hits, []string{"3.3.3.3", "4.4.4.4"}) {
		t.Fatalf("hits = %v", f.LinkLimiter.Hits)
	}
}

func TestCompleteLinkLoginStoreAndAuditFailuresAreReturnedUnchanged(t *testing.T) {
	boom := errors.New("db down")
	t.Run("random", func(t *testing.T) {
		f := newFixture()
		person(t, f, "alice", false)
		link := issue(t, f, "alice")
		f.Random.Fail = boom
		if _, err := redeem(f, link.Code, "1.1.1.1"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("audit of a success", func(t *testing.T) {
		f := newFixture()
		person(t, f, "alice", false)
		link := issue(t, f, "alice")
		f.Audit.Fail = boom
		if _, err := redeem(f, link.Code, "1.1.1.1"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("audit of a refusal", func(t *testing.T) {
		f := newFixture()
		user := person(t, f, "alice", false)
		link := issue(t, f, "alice")
		f.Store.userByID(user.ID).Disabled = true
		f.Audit.Fail = boom
		if _, err := redeem(f, link.Code, "1.1.1.1"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if len(f.LinkLimiter.Hits) != 0 {
			t.Fatal("the limiter was hit after a failed audit write")
		}
	})
	t.Run("issue: audit", func(t *testing.T) {
		f := newFixture()
		person(t, f, "alice", false)
		f.Audit.Fail = boom
		if _, err := app.IssueLoginLink(ctx, f.Deps, app.IssueLoginLinkInput{Sub: "alice"}); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestBootstrapLocalAdminCreatesTheAdminAndALink(t *testing.T) {
	f := newFixture()
	code, issued, err := app.BootstrapLocalAdmin(ctx, f.Deps)
	if err != nil || !issued || !strings.HasPrefix(code, "ll_") {
		t.Fatalf("code = %q issued = %v err = %v", code, issued, err)
	}
	if len(f.Store.users) != 1 {
		t.Fatalf("users = %d", len(f.Store.users))
	}
	u := f.Store.users[0]
	if u.OIDCSub != "local-admin" || u.Name != "Admin" || u.Role != domain.RoleAdmin {
		t.Fatalf("user = %+v", u)
	}
	if len(f.Store.links) != 1 {
		t.Fatalf("links = %d", len(f.Store.links))
	}
}

func TestBootstrapLocalAdminReissuesWhileNeverSignedIn(t *testing.T) {
	f := newFixture()
	first, _, _ := app.BootstrapLocalAdmin(ctx, f.Deps)
	second, issued, err := app.BootstrapLocalAdmin(ctx, f.Deps)
	if err != nil || !issued || !strings.HasPrefix(second, "ll_") || second == first {
		t.Fatalf("second = %q first = %q issued = %v err = %v", second, first, issued, err)
	}
	if len(f.Store.users) != 1 || len(f.Store.links) != 1 {
		t.Fatalf("users %d links %d", len(f.Store.users), len(f.Store.links))
	}
	if _, err := redeem(f, first, "1.1.1.1"); err == nil {
		t.Fatal("the first link still works")
	}
}

func TestBootstrapLocalAdminNothingOnceLocalAdminSignedIn(t *testing.T) {
	f := newFixture()
	first, _, _ := app.BootstrapLocalAdmin(ctx, f.Deps)
	f.Clock.Advance(time.Minute)
	if _, err := redeem(f, first, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code, issued, err := app.BootstrapLocalAdmin(ctx, f.Deps)
	if err != nil || issued || code != "" {
		t.Fatalf("code = %q issued = %v err = %v", code, issued, err)
	}
}

func TestBootstrapLocalAdminProvisioningFailureIsReturned(t *testing.T) {
	f := newFixture()
	boom := errors.New("db down")
	f.Audit.Fail = boom // ProvisionUser writes its audit record last
	code, issued, err := app.BootstrapLocalAdmin(ctx, f.Deps)
	if !errors.Is(err, boom) || issued || code != "" {
		t.Fatalf("code = %q issued = %v err = %v", code, issued, err)
	}
	if len(f.Store.links) != 0 {
		t.Fatal("a link was issued after a failed provisioning")
	}
}

type failingUsers struct {
	app.UserRepository
	upsertErr, listErr error
}

func (u failingUsers) UpsertFromLogin(ctx2 context.Context, in app.UpsertLogin) (domain.User, error) {
	if u.upsertErr != nil {
		return domain.User{}, u.upsertErr
	}
	return u.UserRepository.UpsertFromLogin(ctx2, in)
}

func (u failingUsers) List(ctx2 context.Context) ([]domain.User, error) {
	if u.listErr != nil {
		return nil, u.listErr
	}
	return u.UserRepository.List(ctx2)
}

func TestBootstrapLocalAdminStoreFailures(t *testing.T) {
	boom := errors.New("db down")
	f := newFixture()
	f.Deps.Users = failingUsers{UserRepository: f.Deps.Users, upsertErr: boom}
	if _, _, err := app.BootstrapLocalAdmin(ctx, f.Deps); !errors.Is(err, boom) {
		t.Fatalf("upsert: err = %v", err)
	}
	f = newFixture()
	f.Deps.Users = failingUsers{UserRepository: f.Deps.Users, listErr: boom}
	if _, _, err := app.BootstrapLocalAdmin(ctx, f.Deps); !errors.Is(err, boom) {
		t.Fatalf("list: err = %v", err)
	}
}

func TestBootstrapLocalAdminSwallowsARefusedIssue(t *testing.T) {
	// local-admin exists, never signed in, but is disabled between the decision and the issue:
	// NeedsBootstrapLink says reissue for the stale list, IssueLoginLink then refuses.
	f := newFixture()
	person(t, f, "local-admin", true)
	f.Deps.Users = staleList{UserRepository: f.Deps.Users}
	f.Store.users[0].Disabled = true
	code, issued, err := app.BootstrapLocalAdmin(ctx, f.Deps)
	if err != nil || issued || code != "" {
		t.Fatalf("code = %q issued = %v err = %v", code, issued, err)
	}
}

// staleList reports the people as they were before anyone was disabled.
type staleList struct{ app.UserRepository }

func (s staleList) List(ctx2 context.Context) ([]domain.User, error) {
	us, err := s.UserRepository.List(ctx2)
	for i := range us {
		us[i].Disabled = false
		us[i].LastLoginAt = nil
	}
	return us, err
}

func TestBootstrapLocalAdminNothingWhenAnotherPersonExists(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	code, issued, err := app.BootstrapLocalAdmin(ctx, f.Deps)
	if err != nil || issued || code != "" || len(f.Store.links) != 0 {
		t.Fatalf("code = %q issued = %v err = %v links = %d", code, issued, err, len(f.Store.links))
	}
}

func TestBootstrapLocalAdminNothingWhenLocalAdminIsDisabled(t *testing.T) {
	f := newFixture()
	_, _, _ = app.BootstrapLocalAdmin(ctx, f.Deps)
	f.Store.users[0].Disabled = true
	code, issued, err := app.BootstrapLocalAdmin(ctx, f.Deps)
	if err != nil || issued || code != "" {
		t.Fatalf("code = %q issued = %v err = %v", code, issued, err)
	}
}

func TestBootstrapLocalAdminNothingWhenTheOnlyPersonIsDisabled(t *testing.T) {
	f := newFixture()
	user := person(t, f, "alice", false)
	f.Store.userByID(user.ID).Disabled = true
	code, issued, err := app.BootstrapLocalAdmin(ctx, f.Deps)
	if err != nil || issued || code != "" || len(f.Store.users) != 1 || len(f.Store.links) != 0 {
		t.Fatalf("code = %q issued = %v err = %v", code, issued, err)
	}
}

func TestLoginCodeAndLinkSessionAre32RandomBytes(t *testing.T) {
	f := newFixture()
	person(t, f, "alice", false)
	r := f.sizes()
	link := issue(t, f, "alice")
	wantSizes(t, r, 32)
	r.Sizes = nil
	if _, err := redeem(f, link.Code, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	wantSizes(t, r, 32)
}

func TestBootstrapLocalAdminLinkStoreFailureIsReturned(t *testing.T) {
	boom := errors.New("db down")
	for name, fl := range map[string]func(app.LoginLinkRepository) failingLinks{
		"insert": func(l app.LoginLinkRepository) failingLinks { return failingLinks{l, boom, nil} },
		"delete": func(l app.LoginLinkRepository) failingLinks { return failingLinks{l, nil, boom} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.Deps.LoginLinks = fl(f.Deps.LoginLinks)
			code, issued, err := app.BootstrapLocalAdmin(ctx, f.Deps)
			if !errors.Is(err, boom) || issued || code != "" {
				t.Fatalf("code = %q issued = %v err = %v", code, issued, err)
			}
			if _, refusal := domain.AsError(err); refusal {
				t.Fatal("a store failure must not look like a domain refusal")
			}
			if len(f.Store.users) != 1 {
				t.Fatal("provisioning should have succeeded")
			}
		})
	}
}
