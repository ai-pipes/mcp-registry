package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

var errBoom = errors.New("boom")

// faults makes named repository calls fail; every wrapper embeds the working fake.
type faults map[string]bool

func (f faults) at(name string) error {
	if f[name] {
		return errBoom
	}
	return nil
}

type faultyUsers struct {
	app.UserRepository
	f faults
}

func (r faultyUsers) FindByID(c context.Context, id string) (*domain.User, error) {
	if err := r.f.at("users.FindByID"); err != nil {
		return nil, err
	}
	return r.UserRepository.FindByID(c, id)
}

func (r faultyUsers) FindBySub(c context.Context, s string) (*domain.User, error) {
	if err := r.f.at("users.FindBySub"); err != nil {
		return nil, err
	}
	return r.UserRepository.FindBySub(c, s)
}

func (r faultyUsers) UpsertFromLogin(c context.Context, in app.UpsertLogin) (domain.User, error) {
	if err := r.f.at("users.UpsertFromLogin"); err != nil {
		return domain.User{}, err
	}
	return r.UserRepository.UpsertFromLogin(c, in)
}

func (r faultyUsers) List(c context.Context) ([]domain.User, error) {
	if err := r.f.at("users.List"); err != nil {
		return nil, err
	}
	return r.UserRepository.List(c)
}

func (r faultyUsers) SetDisabled(c context.Context, id string, d bool) (bool, error) {
	if err := r.f.at("users.SetDisabled"); err != nil {
		return false, err
	}
	return r.UserRepository.SetDisabled(c, id, d)
}

type faultyTokens struct {
	app.TokenRepository
	f faults
}

func (r faultyTokens) Insert(c context.Context, t app.NewToken) (app.TokenRecord, error) {
	if err := r.f.at("tokens.Insert"); err != nil {
		return app.TokenRecord{}, err
	}
	return r.TokenRepository.Insert(c, t)
}

func (r faultyTokens) FindByHash(c context.Context, h string) (*app.TokenRecord, error) {
	if err := r.f.at("tokens.FindByHash"); err != nil {
		return nil, err
	}
	return r.TokenRepository.FindByHash(c, h)
}

func (r faultyTokens) FindByID(c context.Context, id string) (*app.TokenRecord, error) {
	if err := r.f.at("tokens.FindByID"); err != nil {
		return nil, err
	}
	return r.TokenRepository.FindByID(c, id)
}

func (r faultyTokens) ListByUser(c context.Context, u string) ([]app.TokenRecord, error) {
	if err := r.f.at("tokens.ListByUser"); err != nil {
		return nil, err
	}
	return r.TokenRepository.ListByUser(c, u)
}

func (r faultyTokens) Revoke(c context.Context, id string, at time.Time) error {
	if err := r.f.at("tokens.Revoke"); err != nil {
		return err
	}
	return r.TokenRepository.Revoke(c, id, at)
}

func (r faultyTokens) RevokeAllByUser(c context.Context, u string, at time.Time) error {
	if err := r.f.at("tokens.RevokeAllByUser"); err != nil {
		return err
	}
	return r.TokenRepository.RevokeAllByUser(c, u, at)
}

func (r faultyTokens) Touch(c context.Context, id string, at time.Time) error {
	if err := r.f.at("tokens.Touch"); err != nil {
		return err
	}
	return r.TokenRepository.Touch(c, id, at)
}

type faultySessions struct {
	app.SessionRepository
	f faults
}

func (r faultySessions) FindByHash(c context.Context, h string) (*app.SessionRecord, error) {
	if err := r.f.at("sessions.FindByHash"); err != nil {
		return nil, err
	}
	return r.SessionRepository.FindByHash(c, h)
}

func (r faultySessions) Touch(c context.Context, h string, at time.Time) error {
	if err := r.f.at("sessions.Touch"); err != nil {
		return err
	}
	return r.SessionRepository.Touch(c, h, at)
}

func (r faultySessions) Delete(c context.Context, h string) error {
	if err := r.f.at("sessions.Delete"); err != nil {
		return err
	}
	return r.SessionRepository.Delete(c, h)
}

func (r faultySessions) DeleteByUser(c context.Context, u string) error {
	if err := r.f.at("sessions.DeleteByUser"); err != nil {
		return err
	}
	return r.SessionRepository.DeleteByUser(c, u)
}

func (r faultySessions) DeleteDead(c context.Context, cut domain.SessionCutoffs) (int, error) {
	if err := r.f.at("sessions.DeleteDead"); err != nil {
		return 0, err
	}
	return r.SessionRepository.DeleteDead(c, cut)
}

type faultyLinks struct {
	app.LoginLinkRepository
	f faults
}

func (r faultyLinks) DeleteByUser(c context.Context, u string) error {
	if err := r.f.at("links.DeleteByUser"); err != nil {
		return err
	}
	return r.LoginLinkRepository.DeleteByUser(c, u)
}

func (r faultyLinks) DeleteExpired(c context.Context, now time.Time) (int, error) {
	if err := r.f.at("links.DeleteExpired"); err != nil {
		return 0, err
	}
	return r.LoginLinkRepository.DeleteExpired(c, now)
}

// withFaults returns deps whose repositories fail at the named calls.
func withFaults(d app.Deps, f faults) app.Deps {
	d.Users = faultyUsers{d.Users, f}
	d.Tokens = faultyTokens{d.Tokens, f}
	d.Sessions = faultySessions{d.Sessions, f}
	d.LoginLinks = faultyLinks{d.LoginLinks, f}
	return d
}

// TestStoreFailuresPropagate runs every use case with each of its repository calls failing in
// turn: the store error comes back unchanged, never as a domain refusal and never swallowed.
func TestStoreFailuresPropagate(t *testing.T) {
	setup := func() (*fixture, domain.Principal, domain.Principal, string, string) {
		f := newFixture()
		root, _ := signIn(t, f, "root", []string{"mcp-admins"})
		bob, sid := signIn(t, f, "bob", []string{"mcp-users"})
		tok, _ := app.CreateToken(ctx, f.Deps, bob, "x")
		f.Store.sessions[f.Deps.Hasher.Hex(sid)].LastSeenAt = f.Clock.Now()
		return f, root, bob, sid, tok.Token
	}
	type call func(d app.Deps, root, bob domain.Principal, sid, tok string) error
	cases := []struct {
		fault string
		run   call
		prep  func(f *fixture, sid string)
	}{
		{"sessions.FindByHash", func(d app.Deps, _, _ domain.Principal, sid, _ string) error { return app.Logout(ctx, d, sid) }, nil},
		{"sessions.Delete", func(d app.Deps, _, _ domain.Principal, sid, _ string) error { return app.Logout(ctx, d, sid) }, nil},
		{"sessions.FindByHash", func(d app.Deps, _, _ domain.Principal, sid, _ string) error {
			_, err := app.AuthenticateSession(ctx, d, sid)
			return err
		}, nil},
		{"sessions.Delete", func(d app.Deps, _, _ domain.Principal, sid, _ string) error {
			_, err := app.AuthenticateSession(ctx, d, sid)
			return err
		}, func(f *fixture, _ string) { f.Clock.Advance(13 * time.Hour) }},
		{"users.FindByID", func(d app.Deps, _, _ domain.Principal, sid, _ string) error {
			_, err := app.AuthenticateSession(ctx, d, sid)
			return err
		}, nil},
		{"sessions.Touch", func(d app.Deps, _, _ domain.Principal, sid, _ string) error {
			_, err := app.AuthenticateSession(ctx, d, sid)
			return err
		}, nil},
		{"links.DeleteExpired", func(d app.Deps, _, _ domain.Principal, _, _ string) error {
			_, err := app.PruneExpiredSessions(ctx, d)
			return err
		}, nil},
		{"sessions.DeleteDead", func(d app.Deps, _, _ domain.Principal, _, _ string) error {
			_, err := app.PruneExpiredSessions(ctx, d)
			return err
		}, nil},
		{"tokens.Insert", func(d app.Deps, _, bob domain.Principal, _, _ string) error {
			_, err := app.CreateToken(ctx, d, bob, "y")
			return err
		}, nil},
		{"tokens.ListByUser", func(d app.Deps, _, bob domain.Principal, _, _ string) error {
			_, err := app.ListTokens(ctx, d, bob)
			return err
		}, nil},
		{"tokens.FindByID", func(d app.Deps, _, bob domain.Principal, _, _ string) error {
			return app.RevokeToken(ctx, d, bob, "tok-3")
		}, nil},
		{"tokens.Revoke", func(d app.Deps, _, bob domain.Principal, _, _ string) error {
			return app.RevokeToken(ctx, d, bob, "tok-3")
		}, nil},
		{"tokens.FindByHash", func(d app.Deps, _, _ domain.Principal, _, tok string) error {
			_, err := app.AuthenticateToken(ctx, d, tok, "ip")
			return err
		}, nil},
		{"users.FindByID", func(d app.Deps, _, _ domain.Principal, _, tok string) error {
			_, err := app.AuthenticateToken(ctx, d, tok, "ip")
			return err
		}, nil},
		{"tokens.Touch", func(d app.Deps, _, _ domain.Principal, _, tok string) error {
			_, err := app.AuthenticateToken(ctx, d, tok, "ip")
			return err
		}, nil},
		{"users.List", func(d app.Deps, root, _ domain.Principal, _, _ string) error {
			_, err := app.ListUsers(ctx, d, root)
			return err
		}, nil},
		{"users.SetDisabled", func(d app.Deps, root, bob domain.Principal, _, _ string) error {
			return app.SetDisabled(ctx, d, app.SetDisabledInput{Principal: root, UserID: bob.UserID, Disabled: true})
		}, nil},
		{"sessions.DeleteByUser", func(d app.Deps, root, bob domain.Principal, _, _ string) error {
			return app.SetDisabled(ctx, d, app.SetDisabledInput{Principal: root, UserID: bob.UserID, Disabled: true})
		}, nil},
		{"links.DeleteByUser", func(d app.Deps, root, bob domain.Principal, _, _ string) error {
			return app.SetDisabled(ctx, d, app.SetDisabledInput{Principal: root, UserID: bob.UserID, Disabled: true})
		}, nil},
		{"tokens.RevokeAllByUser", func(d app.Deps, root, bob domain.Principal, _, _ string) error {
			return app.SetDisabled(ctx, d, app.SetDisabledInput{Principal: root, UserID: bob.UserID, Disabled: true})
		}, nil},
		{"users.FindByID", func(d app.Deps, _, bob domain.Principal, _, _ string) error {
			_, err := app.IsUserActiveAdmin(ctx, d, bob.UserID)
			return err
		}, nil},
		{"users.FindBySub", func(d app.Deps, _, _ domain.Principal, _, _ string) error {
			_, err := app.ActAs(ctx, d, "bob")
			return err
		}, nil},
		{"users.FindBySub", func(d app.Deps, _, _ domain.Principal, _, _ string) error {
			_, err := app.ProvisionUser(ctx, d, app.ProvisionInput{Sub: "bob", Name: "Bob"})
			return err
		}, nil},
		{"users.UpsertFromLogin", func(d app.Deps, _, _ domain.Principal, _, _ string) error {
			_, err := app.ProvisionUser(ctx, d, app.ProvisionInput{Sub: "bob", Name: "Bob"})
			return err
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.fault, func(t *testing.T) {
			f, root, bob, sid, tok := setup()
			if c.prep != nil {
				c.prep(f, sid)
			}
			err := c.run(withFaults(f.Deps, faults{c.fault: true}), root, bob, sid, tok)
			if !errors.Is(err, errBoom) {
				t.Fatalf("err = %v, want the store error", err)
			}
			if _, ok := domain.AsError(err); ok {
				t.Fatal("store failure turned into a domain error")
			}
		})
	}
}

func TestAuditFailuresPropagate(t *testing.T) {
	f := newFixture()
	root, _ := signIn(t, f, "root", []string{"mcp-admins"})
	bob, sid := signIn(t, f, "bob", []string{"mcp-users"})
	tok, _ := app.CreateToken(ctx, f.Deps, bob, "x")
	f.Deps.Audit = failingAudit{errBoom}
	checks := map[string]error{
		"logout": app.Logout(ctx, f.Deps, sid),
		"create": func() error { _, err := app.CreateToken(ctx, f.Deps, bob, "y"); return err }(),
		"revoke": app.RevokeToken(ctx, f.Deps, bob, tok.ID),
		"disable": app.SetDisabled(ctx, f.Deps, app.SetDisabledInput{
			Principal: root, UserID: bob.UserID, Disabled: true,
		}),
	}
	for name, err := range checks {
		if !errors.Is(err, errBoom) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
