package app_test

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

func claims(groups ...string) *domain.OIDCClaims {
	email := "anna@example.com"
	return &domain.OIDCClaims{Sub: "sub-1", Name: "Anna", Email: &email, Groups: groups}
}

func start(t *testing.T, f *fixture, returnTo string) *url.URL {
	t.Helper()
	u, err := app.StartOIDCLogin(ctx, f.Deps, app.StartOIDCLoginInput{ReturnTo: returnTo})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func callback(f *fixture, state, ip string) (app.OIDCLoginResult, error) {
	return app.CompleteOIDCLogin(ctx, f.Deps, app.CompleteOIDCLoginInput{State: kernel.NewSecret(state), Code: kernel.NewSecret("code-1"), IP: ip})
}

// oidcLogin runs a whole sign-in with the given claims from 1.1.1.1.
func oidcLogin(t *testing.T, f *fixture, c *domain.OIDCClaims, returnTo string) (string, app.OIDCLoginResult, error) {
	t.Helper()
	f.OIDC.Next = c
	state := start(t, f, returnTo).Query().Get("state")
	res, err := callback(f, state, "1.1.1.1")
	return state, res, err
}

func onlyUser(t *testing.T, f *fixture) *domain.User {
	t.Helper()
	if len(f.Store.users) != 1 {
		t.Fatalf("users = %d, want 1", len(f.Store.users))
	}
	return f.Store.users[0]
}

func TestStartOIDCLoginStoresAPendingLoginAndSendsPKCEStateAndNonce(t *testing.T) {
	f := newFixture()
	u := start(t, f, "/tokens")
	state := u.Query().Get("state")
	if state == "" || u.Query().Get("nonce") == "" {
		t.Fatalf("url = %v", u)
	}
	p, ok := f.Store.pending[state]
	if !ok || p.ReturnTo != "/tokens" || p.Nonce.Reveal() != u.Query().Get("nonce") {
		t.Fatalf("pending = %+v", p)
	}
	if got := u.Query().Get("code_challenge"); got != f.Deps.Hasher.Base64URL(p.CodeVerifier.Reveal()) {
		t.Fatalf("challenge = %q", got)
	}
	if !p.ExpiresAt.Equal(f.Clock.Now().Add(10 * time.Minute)) {
		t.Fatalf("expiresAt = %v", p.ExpiresAt)
	}
	if state == p.Nonce.Reveal() || state == p.CodeVerifier.Reveal() || p.Nonce.Reveal() == p.CodeVerifier.Reveal() {
		t.Fatal("state, nonce and verifier must be three different random values")
	}
}

func TestStartOIDCLoginTurnsAnOffSiteReturnToIntoRoot(t *testing.T) {
	f := newFixture()
	start(t, f, "https://evil.example/")
	start(t, f, "//evil")
	start(t, f, `/\evil`)
	start(t, f, "/a\nb")
	if len(f.Store.pending) != 4 {
		t.Fatalf("pending = %d", len(f.Store.pending))
	}
	for _, p := range f.Store.pending {
		if p.ReturnTo != "/" {
			t.Fatalf("returnTo = %q", p.ReturnTo)
		}
	}
}

func TestStartOIDCLoginKeepsAPathWithQueryAndFragment(t *testing.T) {
	f := newFixture()
	start(t, f, "/admin/servers/new?x=1#y")
	for _, p := range f.Store.pending {
		if p.ReturnTo != "/admin/servers/new?x=1#y" {
			t.Fatalf("returnTo = %q", p.ReturnTo)
		}
	}
}

func TestStartOIDCLoginOrderProviderBeforeStore(t *testing.T) {
	f := newFixture()
	seq := f.record()
	start(t, f, "/")
	want := []string{"oidc.authorizationURL", "pending.insert"}
	if !slices.Equal(seq.Calls, want) {
		t.Fatalf("calls = %v, want %v", seq.Calls, want)
	}
}

func TestStartOIDCLoginUnavailableProviderStoresNothing(t *testing.T) {
	f := newFixture()
	f.OIDC.Unavailable = true
	seq := f.record()
	_, err := app.StartOIDCLogin(ctx, f.Deps, app.StartOIDCLoginInput{ReturnTo: "/"})
	_ = wantCode(t, err, domain.CodeIDPUnavailable)
	if len(f.Store.pending) != 0 {
		t.Fatal("a pending login was stored")
	}
	if !slices.Equal(seq.Calls, []string{"oidc.authorizationURL"}) {
		t.Fatalf("calls = %v", seq.Calls)
	}
}

type buggyOIDC struct{ app.OIDCProvider }

func (buggyOIDC) AuthorizationURL(_ context.Context, _ app.OIDCAuthorizationInput) (*url.URL, error) {
	return nil, errBug
}

var errBug = errors.New("bug")

func TestStartOIDCLoginErrorThatIsNotAboutTheProviderPropagates(t *testing.T) {
	f := newFixture()
	f.Deps.OIDC = buggyOIDC{f.Deps.OIDC}
	_, err := app.StartOIDCLogin(ctx, f.Deps, app.StartOIDCLoginInput{ReturnTo: "/"})
	if !errors.Is(err, errBug) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := domain.AsError(err); ok {
		t.Fatal("must not be a domain error")
	}
	if len(f.Store.pending) != 0 {
		t.Fatal("a pending login was stored")
	}
}

func TestStartOIDCLoginRandomFailure(t *testing.T) {
	f := newFixture()
	boom := errors.New("no entropy")
	f.Random.Fail = boom
	if _, err := app.StartOIDCLogin(ctx, f.Deps, app.StartOIDCLoginInput{ReturnTo: "/"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestCompleteOIDCLoginCreatesAMemberForTheLoginGroupAndOpensASession(t *testing.T) {
	f := newFixture()
	_, res, err := oidcLogin(t, f, claims("mcp-users"), "/servers")
	if err != nil {
		t.Fatal(err)
	}
	if res.ReturnTo != "/servers" {
		t.Fatalf("returnTo = %q", res.ReturnTo)
	}
	p, err := app.AuthenticateSession(ctx, f.Deps, res.SessionID.Reveal())
	if err != nil || p == nil {
		t.Fatalf("principal = %v err = %v", p, err)
	}
	if p.Role != domain.RoleMember || !slices.Equal(p.Groups, []string{"mcp-users"}) || p.Via != domain.ViaSession {
		t.Fatalf("principal = %+v", p)
	}
	s := f.Store.sessions[f.Deps.Hasher.Hex(res.SessionID.Reveal())]
	if s == nil || !s.ExpiresAt.Equal(domain.SessionExpiry(f.Clock.Now())) {
		t.Fatalf("session = %+v", s)
	}
	if len(f.Store.users) != 1 {
		t.Fatalf("users = %d", len(f.Store.users))
	}
}

func TestCompleteOIDCLoginAuditsTheSignInWithViaOIDC(t *testing.T) {
	f := newFixture()
	_, res, err := oidcLogin(t, f, claims("mcp-users"), "/")
	if err != nil {
		t.Fatal(err)
	}
	logins := f.Audit.Actions("auth.login")
	if len(logins) != 1 {
		t.Fatalf("audit = %+v", f.Audit.Entries)
	}
	e := logins[0]
	if e.ActorID == nil || *e.ActorID != onlyUser(t, f).ID || e.Target != nil || len(e.Meta) != 1 || e.Meta["via"] != "oidc" {
		t.Fatalf("entry = %+v", e)
	}
	if strings.Contains(strings.Join(auditText(f), " "), res.SessionID.Reveal()) {
		t.Fatal("the session id reached the audit log")
	}
}

func TestCompleteOIDCLoginUpdatesGroupsAndRoleOnTheNextLoginOfTheSameSub(t *testing.T) {
	f := newFixture()
	if _, _, err := oidcLogin(t, f, claims("mcp-users"), "/"); err != nil {
		t.Fatal(err)
	}
	first := *onlyUser(t, f)
	if _, _, err := oidcLogin(t, f, claims("mcp-users", "mcp-admins"), "/"); err != nil {
		t.Fatal(err)
	}
	again := onlyUser(t, f)
	if again.ID != first.ID || again.Role != domain.RoleAdmin || !slices.Equal(again.Groups, []string{"mcp-users", "mcp-admins"}) {
		t.Fatalf("user = %+v", again)
	}
}

func TestCompleteOIDCLoginRefusesGroupsOutsideThePolicyAndCreatesNobody(t *testing.T) {
	for name, groups := range map[string][]string{"foreign": {"family"}, "none": {}, "nil": nil} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			_, _, err := oidcLogin(t, f, claims(groups...), "/")
			e := wantCode(t, err, domain.CodeNotAllowed)
			if e.Message != "your account is not in an allowed group" {
				t.Fatalf("message = %q", e.Message)
			}
			if len(f.Store.users) != 0 || len(f.Store.sessions) != 0 {
				t.Fatal("a person or a session was created")
			}
			entries := f.Audit.Actions("auth.login_failed")
			if len(entries) != 1 || entries[0].ActorID != nil || entries[0].Target != nil ||
				len(entries[0].Meta) != 2 || entries[0].Meta["reason"] != "not_allowed" || entries[0].Meta["sub"] != "sub-1" {
				t.Fatalf("audit = %+v", entries)
			}
		})
	}
}

func TestCompleteOIDCLoginRefusesADisabledPersonWithoutASession(t *testing.T) {
	f := newFixture()
	if _, _, err := oidcLogin(t, f, claims("mcp-users"), "/"); err != nil {
		t.Fatal(err)
	}
	u := onlyUser(t, f)
	u.Disabled = true
	before := len(f.Audit.Actions("auth.login"))
	clear(f.Store.sessions)
	_, _, err := oidcLogin(t, f, claims("mcp-users"), "/")
	e := wantCode(t, err, domain.CodeDisabled)
	if e.Message != "account is disabled" {
		t.Fatalf("message = %q", e.Message)
	}
	if len(f.Store.sessions) != 0 {
		t.Fatal("a session was opened")
	}
	entries := f.Audit.Actions("auth.login_failed")
	if len(entries) != 1 || entries[0].ActorID == nil || *entries[0].ActorID != u.ID ||
		len(entries[0].Meta) != 1 || entries[0].Meta["reason"] != "disabled" {
		t.Fatalf("audit = %+v", entries)
	}
	if len(f.Audit.Actions("auth.login")) != before {
		t.Fatal("a refused sign-in was audited as a success")
	}
}

func TestCompleteOIDCLoginPassesTheCallbackIssAndTheAttemptSecretsToTheProvider(t *testing.T) {
	f := newFixture()
	f.OIDC.Next = claims("mcp-users")
	u := start(t, f, "/")
	state := u.Query().Get("state")
	pending := f.Store.pending[state]
	iss := "https://id.example"
	if _, err := app.CompleteOIDCLogin(ctx, f.Deps, app.CompleteOIDCLoginInput{State: kernel.NewSecret(state), Code: kernel.NewSecret("c"), Iss: &iss, IP: "1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	got := f.OIDC.LastExchange
	if got == nil || got.Code.Reveal() != "c" || got.CodeVerifier.Reveal() != pending.CodeVerifier.Reveal() || got.Nonce.Reveal() != pending.Nonce.Reveal() ||
		got.Iss == nil || *got.Iss != iss {
		t.Fatalf("exchange = %+v", got)
	}
}

func TestCompleteOIDCLoginWithoutIssSendsNilIss(t *testing.T) {
	f := newFixture()
	if _, _, err := oidcLogin(t, f, claims("mcp-users"), "/"); err != nil {
		t.Fatal(err)
	}
	if f.OIDC.LastExchange == nil || f.OIDC.LastExchange.Iss != nil {
		t.Fatalf("exchange = %+v", f.OIDC.LastExchange)
	}
}

func TestCompleteOIDCLoginRejectsAnUnknownState(t *testing.T) {
	f := newFixture()
	seq := f.record()
	_, err := callback(f, "nope", "1.1.1.1")
	e := wantCode(t, err, domain.CodeInvalidState)
	if e.Message != "login attempt is unknown or expired" {
		t.Fatalf("message = %q", e.Message)
	}
	if !slices.Equal(seq.Calls, []string{"pending.consume"}) {
		t.Fatalf("calls = %v: an unknown state reaches neither the provider, nor the limiter, nor the audit", seq.Calls)
	}
}

func TestCompleteOIDCLoginRejectsAStateOfExactlyTenMinutesAndConsumesIt(t *testing.T) {
	f := newFixture()
	f.OIDC.Next = claims("mcp-users")
	state := start(t, f, "/").Query().Get("state")
	f.Clock.Advance(10 * time.Minute)
	seq := f.record()
	_, err := callback(f, state, "1.1.1.1")
	_ = wantCode(t, err, domain.CodeInvalidState)
	if _, still := f.Store.pending[state]; still {
		t.Fatal("an expired state stays stored")
	}
	if !slices.Equal(seq.Calls, []string{"pending.consume"}) {
		t.Fatalf("calls = %v", seq.Calls)
	}
}

func TestCompleteOIDCLoginAcceptsAStateJustBeforeTenMinutes(t *testing.T) {
	f := newFixture()
	f.OIDC.Next = claims("mcp-users")
	state := start(t, f, "/").Query().Get("state")
	f.Clock.Advance(10*time.Minute - time.Nanosecond)
	if _, err := callback(f, state, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteOIDCLoginAcceptsAStateOnlyOnce(t *testing.T) {
	f := newFixture()
	state, _, err := oidcLogin(t, f, claims("mcp-users"), "/")
	if err != nil {
		t.Fatal(err)
	}
	_, err = callback(f, state, "1.1.1.1")
	_ = wantCode(t, err, domain.CodeInvalidState)
}

func TestCompleteOIDCLoginFailedExchangeIsInvalidStateAuditedAndConsumesTheState(t *testing.T) {
	f := newFixture()
	f.OIDC.Next = nil
	state := start(t, f, "/").Query().Get("state")
	seq := f.record()
	_, err := callback(f, state, "1.1.1.1")
	e := wantCode(t, err, domain.CodeInvalidState)
	if e.Message != "the identity provider rejected the login" {
		t.Fatalf("message = %q", e.Message)
	}
	if _, still := f.Store.pending[state]; still {
		t.Fatal("the state survived a failed exchange")
	}
	want := []string{"pending.consume", "oidc.exchange", "callbackLimiter.hit", "audit:auth.login_failed:exchange_failed"}
	if !slices.Equal(seq.Calls, want) {
		t.Fatalf("calls = %v, want %v", seq.Calls, want)
	}
	entries := f.Audit.Actions("auth.login_failed")
	if len(entries) != 1 || entries[0].ActorID != nil || entries[0].Target != nil ||
		len(entries[0].Meta) != 1 || entries[0].Meta["reason"] != "exchange_failed" {
		t.Fatalf("audit = %+v", entries)
	}
	// The same state cannot be tried again.
	f.OIDC.Next = claims("mcp-users")
	_, err = callback(f, state, "1.1.1.1")
	_ = wantCode(t, err, domain.CodeInvalidState)
}

func TestCompleteOIDCLoginAuditsFailedExchangesOnlyWhileTheLimiterAllows(t *testing.T) {
	f := newFixture()
	f.OIDC.Next = nil
	fail := func(ip string) error {
		state := start(t, f, "/").Query().Get("state")
		_, err := callback(f, state, ip)
		return err
	}
	_ = wantCode(t, fail("2.2.2.2"), domain.CodeInvalidState)
	if n := len(f.Audit.Entries); n != 1 {
		t.Fatalf("audit = %d, want 1", n)
	}
	f.CallbackLimit.Deny = true
	over := fail("2.2.2.2")
	e := wantCode(t, over, domain.CodeInvalidState)
	if e.RetryAfterSec != 0 {
		t.Fatalf("retryAfter = %d: a refused exchange is not rate_limited", e.RetryAfterSec)
	}
	if n := len(f.Audit.Entries); n != 1 {
		t.Fatalf("audit = %d, want still 1", n)
	}
	if !slices.Equal(f.CallbackLimit.Hits, []string{"2.2.2.2", "2.2.2.2"}) {
		t.Fatalf("hits = %v", f.CallbackLimit.Hits)
	}
}

func TestCompleteOIDCLoginValidCodeSignsInWhileTheLimiterIsLockedAndIsNotCounted(t *testing.T) {
	f := newFixture()
	f.CallbackLimit.Deny, f.CallbackLimit.RetryAfter = true, 60
	f.OIDC.Next = claims("mcp-users")
	state := start(t, f, "/").Query().Get("state")
	seq := f.record()
	if _, err := callback(f, state, "2.2.2.2"); err != nil {
		t.Fatal(err)
	}
	if len(f.CallbackLimit.Hits) != 0 {
		t.Fatalf("hits = %v", f.CallbackLimit.Hits)
	}
	if got := f.Audit.Entries; len(got) != 1 || got[0].Action != "auth.login" {
		t.Fatalf("audit = %+v", got)
	}
	want := []string{
		"pending.consume", "oidc.exchange", "users.findBySub", "users.upsertFromLogin",
		"sessions.insert", "audit:auth.login",
	}
	if !slices.Equal(seq.Calls, want) {
		t.Fatalf("calls = %v, want %v", seq.Calls, want)
	}
}

func TestCompleteOIDCLoginUnavailableProviderIsNeitherAuditedNorCountedAndConsumesTheState(t *testing.T) {
	f := newFixture()
	state := start(t, f, "/").Query().Get("state")
	f.OIDC.Unavailable = true
	seq := f.record()
	_, err := callback(f, state, "1.1.1.1")
	_ = wantCode(t, err, domain.CodeIDPUnavailable)
	if len(f.Audit.Entries) != 0 || len(f.CallbackLimit.Hits) != 0 || len(f.LinkLimiter.Hits) != 0 {
		t.Fatalf("audit %v hits %v", f.Audit.Entries, f.CallbackLimit.Hits)
	}
	if _, still := f.Store.pending[state]; still {
		t.Fatal("the state survived an outage")
	}
	want := []string{"pending.consume", "oidc.exchange"}
	if !slices.Equal(seq.Calls, want) {
		t.Fatalf("calls = %v, want %v", seq.Calls, want)
	}
}

func TestCompleteOIDCLoginRefusalOrders(t *testing.T) {
	t.Run("not allowed: audited before any store write", func(t *testing.T) {
		f := newFixture()
		f.OIDC.Next = claims("family")
		state := start(t, f, "/").Query().Get("state")
		seq := f.record()
		_, err := callback(f, state, "1.1.1.1")
		_ = wantCode(t, err, domain.CodeNotAllowed)
		want := []string{"pending.consume", "oidc.exchange", "audit:auth.login_failed:not_allowed"}
		if !slices.Equal(seq.Calls, want) {
			t.Fatalf("calls = %v, want %v", seq.Calls, want)
		}
		if len(f.CallbackLimit.Hits) != 0 {
			t.Fatal("a policy refusal touched the callback limiter")
		}
	})
	t.Run("disabled: looked up, audited, no upsert", func(t *testing.T) {
		f := newFixture()
		f.OIDC.Next = claims("mcp-users")
		person := func() {
			state := start(t, f, "/").Query().Get("state")
			if _, err := callback(f, state, "1.1.1.1"); err != nil {
				t.Fatal(err)
			}
		}
		person()
		onlyUser(t, f).Disabled = true
		state := start(t, f, "/").Query().Get("state")
		seq := f.record()
		_, err := callback(f, state, "1.1.1.1")
		_ = wantCode(t, err, domain.CodeDisabled)
		want := []string{"pending.consume", "oidc.exchange", "users.findBySub", "audit:auth.login_failed:disabled"}
		if !slices.Equal(seq.Calls, want) {
			t.Fatalf("calls = %v, want %v", seq.Calls, want)
		}
	})
}

func TestCompleteOIDCLoginPolicyRefusalDoesNotLookTheSubUp(t *testing.T) {
	f := newFixture()
	f.OIDC.Next = claims("family")
	state := start(t, f, "/").Query().Get("state")
	seq := f.record()
	_, _ = callback(f, state, "1.1.1.1")
	if slices.Contains(seq.Calls, "users.findBySub") {
		t.Fatalf("calls = %v", seq.Calls)
	}
}

func TestCompleteOIDCLoginStoreAndAuditFailuresAreReturnedUnchanged(t *testing.T) {
	boom := errors.New("db down")
	t.Run("audit of a success", func(t *testing.T) {
		f := newFixture()
		f.Audit.Fail = boom
		if _, _, err := oidcLogin(t, f, claims("mcp-users"), "/"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("audit of an exchange failure", func(t *testing.T) {
		f := newFixture()
		f.OIDC.Next = nil
		f.Audit.Fail = boom
		state := start(t, f, "/").Query().Get("state")
		if _, err := callback(f, state, "1.1.1.1"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("audit of a policy refusal", func(t *testing.T) {
		f := newFixture()
		f.Audit.Fail = boom
		if _, _, err := oidcLogin(t, f, claims("family"), "/"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("random", func(t *testing.T) {
		f := newFixture()
		f.OIDC.Next = claims("mcp-users")
		state := start(t, f, "/").Query().Get("state")
		f.Random.Fail = boom
		if _, err := callback(f, state, "1.1.1.1"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("user store", func(t *testing.T) {
		f := newFixture()
		f.OIDC.Next = claims("mcp-users")
		state := start(t, f, "/").Query().Get("state")
		f.Deps.Users = failingUsers{UserRepository: f.Deps.Users, upsertErr: boom}
		if _, err := callback(f, state, "1.1.1.1"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestOIDCSecretsAre32RandomBytes(t *testing.T) {
	f := newFixture()
	r := f.sizes()
	f.OIDC.Next = claims("mcp-users")
	state := start(t, f, "/").Query().Get("state")
	wantSizes(t, r, 32, 32, 32) // state, nonce, code verifier
	r.Sizes = nil
	if _, err := callback(f, state, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	wantSizes(t, r, 32) // the session id
}
