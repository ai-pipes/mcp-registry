package app_test

import (
	"context"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

// sequence records the port calls of a use case in the order they happen, across ports.
type sequence struct{ Calls []string }

func (s *sequence) add(c string) { s.Calls = append(s.Calls, c) }

// record wraps the ports of f.Deps that the login use cases order their effects around, so a test
// can compare the exact sequence of calls. Audit records show as "audit:<action>[:<reason>]".
func (f *fixture) record() *sequence {
	s := &sequence{}
	d := &f.Deps
	d.LoginLinks = recLinks{d.LoginLinks, s}
	d.Users = recUsers{d.Users, s}
	d.Sessions = recSessions{d.Sessions, s}
	d.PendingLogins = recPending{d.PendingLogins, s}
	d.OIDC = recOIDC{d.OIDC, s}
	d.Audit = recAudit{d.Audit, s}
	d.LinkLimiter = recLimiter{d.LinkLimiter, "linkLimiter.hit", s}
	d.CallbackLimiter = recLimiter{d.CallbackLimiter, "callbackLimiter.hit", s}
	return s
}

type recLinks struct {
	app.LoginLinkRepository
	s *sequence
}

func (r recLinks) Consume(ctx context.Context, h string) (*app.LoginLinkRecord, error) {
	r.s.add("links.consume")
	return r.LoginLinkRepository.Consume(ctx, h)
}

func (r recLinks) DeleteByUser(ctx context.Context, id string) error {
	r.s.add("links.deleteByUser")
	return r.LoginLinkRepository.DeleteByUser(ctx, id)
}

func (r recLinks) Insert(ctx context.Context, l app.LoginLinkRecord) error {
	r.s.add("links.insert")
	return r.LoginLinkRepository.Insert(ctx, l)
}

type recUsers struct {
	app.UserRepository
	s *sequence
}

func (r recUsers) FindByID(ctx context.Context, id string) (*domain.User, error) {
	r.s.add("users.findById")
	return r.UserRepository.FindByID(ctx, id)
}

func (r recUsers) FindBySub(ctx context.Context, sub string) (*domain.User, error) {
	r.s.add("users.findBySub")
	return r.UserRepository.FindBySub(ctx, sub)
}

func (r recUsers) UpsertFromLogin(ctx context.Context, in app.UpsertLogin) (domain.User, error) {
	r.s.add("users.upsertFromLogin")
	return r.UserRepository.UpsertFromLogin(ctx, in)
}

func (r recUsers) TouchLastLogin(ctx context.Context, id string, at time.Time) error {
	r.s.add("users.touchLastLogin")
	return r.UserRepository.TouchLastLogin(ctx, id, at)
}

type recSessions struct {
	app.SessionRepository
	s *sequence
}

func (r recSessions) Insert(ctx context.Context, rec app.SessionRecord) error {
	r.s.add("sessions.insert")
	return r.SessionRepository.Insert(ctx, rec)
}

type recPending struct {
	app.PendingLoginRepository
	s *sequence
}

func (r recPending) Consume(ctx context.Context, state string) (*app.PendingLogin, error) {
	r.s.add("pending.consume")
	return r.PendingLoginRepository.Consume(ctx, state)
}

func (r recPending) Insert(ctx context.Context, p app.PendingLogin) error {
	r.s.add("pending.insert")
	return r.PendingLoginRepository.Insert(ctx, p)
}

type recOIDC struct {
	app.OIDCProvider
	s *sequence
}

func (r recOIDC) AuthorizationURL(ctx context.Context, in app.OIDCAuthorizationInput) (*url.URL, error) {
	r.s.add("oidc.authorizationURL")
	return r.OIDCProvider.AuthorizationURL(ctx, in)
}

func (r recOIDC) Exchange(ctx context.Context, in app.OIDCExchangeInput) (domain.OIDCClaims, error) {
	r.s.add("oidc.exchange")
	return r.OIDCProvider.Exchange(ctx, in)
}

type recAudit struct {
	kernel.AuditLog
	s *sequence
}

func (r recAudit) Record(ctx context.Context, e kernel.AuditEntry) error {
	label := "audit:" + e.Action
	if reason, ok := e.Meta["reason"].(string); ok {
		label += ":" + reason
	}
	r.s.add(label)
	return r.AuditLog.Record(ctx, e)
}

type recLimiter struct {
	app.RateLimiter
	name string
	s    *sequence
}

func (r recLimiter) Hit(key string) (bool, int) {
	r.s.add(r.name)
	return r.RateLimiter.Hit(key)
}

// sizedRandom records the byte counts the use case asked the random source for.
type sizedRandom struct {
	app.Random
	Sizes []int
}

func (r *sizedRandom) Base64URL(n int) (string, error) {
	r.Sizes = append(r.Sizes, n)
	return r.Random.Base64URL(n)
}

// sizes wraps the random source of f.Deps and returns the recorder.
func (f *fixture) sizes() *sizedRandom {
	r := &sizedRandom{Random: f.Deps.Random}
	f.Deps.Random = r
	return r
}

func wantSizes(t *testing.T, r *sizedRandom, want ...int) {
	t.Helper()
	if !slices.Equal(r.Sizes, want) {
		t.Fatalf("random byte counts = %v, want %v", r.Sizes, want)
	}
}

// failingLinks fails the named link-store calls.
type failingLinks struct {
	app.LoginLinkRepository
	insertErr, deleteErr error
}

func (l failingLinks) Insert(ctx context.Context, rec app.LoginLinkRecord) error {
	if l.insertErr != nil {
		return l.insertErr
	}
	return l.LoginLinkRepository.Insert(ctx, rec)
}

func (l failingLinks) DeleteByUser(ctx context.Context, id string) error {
	if l.deleteErr != nil {
		return l.deleteErr
	}
	return l.LoginLinkRepository.DeleteByUser(ctx, id)
}
