package app

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

// LocalAdminSub is the sub of the administrator created for a server that has no OIDC provider.
const LocalAdminSub = "local-admin"

// localAdminName is the display name of that administrator.
const localAdminName = "Admin"

// IssueLoginLinkInput is the input of IssueLoginLink.
type IssueLoginLinkInput struct{ Sub string }

// IssuedLink is a freshly issued login link: the code is shown once and never stored.
type IssuedLink struct {
	Code      string
	ExpiresAt time.Time
}

// IssueLoginLink issues a one-time sign-in link for an existing, enabled person. Only the code's
// hash is kept. At most one link per person stays live: an earlier one stops working.
func IssueLoginLink(ctx context.Context, deps Deps, in IssueLoginLinkInput) (IssuedLink, error) {
	user, err := deps.Users.FindBySub(ctx, in.Sub)
	if err != nil {
		return IssuedLink{}, err
	}
	if user == nil {
		return IssuedLink{}, domain.Err(domain.CodeNotFound, `no user with sub "`+in.Sub+`"`)
	}
	if user.Disabled {
		return IssuedLink{}, domain.Err(domain.CodeDisabled, "account is disabled")
	}
	now := deps.Clock.Now()
	if err := deps.LoginLinks.DeleteByUser(ctx, user.ID); err != nil {
		return IssuedLink{}, err
	}
	random, err := deps.Random.Base64URL(32)
	if err != nil {
		return IssuedLink{}, err
	}
	code := domain.NewLoginCode(random)
	expiresAt := domain.LoginLinkExpiry(now)
	if err := deps.LoginLinks.Insert(ctx, LoginLinkRecord{
		CodeHash:  deps.Hasher.Hex(code),
		UserID:    user.ID,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}); err != nil {
		return IssuedLink{}, err
	}
	if err := deps.Audit.Record(ctx, kernel.AuditEntry{Action: "auth.link_issued", Target: ptr(user.ID)}); err != nil {
		return IssuedLink{}, err
	}
	return IssuedLink{Code: code, ExpiresAt: expiresAt}, nil
}

// CompleteLinkLoginInput is the input of CompleteLinkLogin; IP is the client address the limiter counts.
type CompleteLinkLoginInput struct {
	Code string
	IP   string
}

// CompleteLinkLogin redeems a link once and returns the new session id. Unknown, expired and
// already used codes are all invalid_link. Order of effects (pinned by the compatibility suite):
// shape check without the store; the link is consumed (burned even when refused afterwards); the
// person is looked up; a refusal of a consumed link is audited first and only then counted by the
// limiter, and a locked limiter turns any refusal into rate_limited. Success never touches the
// limiter, so a valid code works from a locked-out address.
func CompleteLinkLogin(ctx context.Context, deps Deps, in CompleteLinkLoginInput) (string, error) {
	if !domain.LooksLikeLoginCode(in.Code) {
		return "", rejectLink(ctx, deps, in.IP, domain.CodeInvalidLink, nil)
	}
	now := deps.Clock.Now()
	link, err := deps.LoginLinks.Consume(ctx, deps.Hasher.Hex(in.Code))
	if err != nil {
		return "", err
	}
	if link == nil {
		return "", rejectLink(ctx, deps, in.IP, domain.CodeInvalidLink, nil)
	}
	user, err := deps.Users.FindByID(ctx, link.UserID)
	if err != nil {
		return "", err
	}
	// The audit record names the person whose link was refused; nobody only when they are gone.
	consumed := &consumedLink{}
	if user != nil {
		consumed.actorID = &user.ID
	}
	switch {
	case domain.IsLoginLinkExpired(link.ExpiresAt, now):
		return "", rejectLink(ctx, deps, in.IP, domain.CodeInvalidLink, consumed)
	case user == nil:
		return "", rejectLink(ctx, deps, in.IP, domain.CodeInvalidLink, &consumedLink{})
	case user.Disabled:
		return "", rejectLink(ctx, deps, in.IP, domain.CodeDisabled, consumed)
	}

	sessionID, err := deps.Random.Base64URL(32)
	if err != nil {
		return "", err
	}
	if err := deps.Sessions.Insert(ctx, SessionRecord{
		IDHash:     deps.Hasher.Hex(sessionID),
		UserID:     user.ID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  domain.SessionExpiry(now),
	}); err != nil {
		return "", err
	}
	if err := deps.Users.TouchLastLogin(ctx, user.ID, now); err != nil {
		return "", err
	}
	if err := deps.Audit.Record(ctx, kernel.AuditEntry{
		ActorID: ptr(user.ID),
		Action:  "auth.login",
		Meta:    map[string]any{"via": "link"},
	}); err != nil {
		return "", err
	}
	return sessionID, nil
}

// consumedLink says a link was actually consumed; actorID is the person it belonged to, nil when
// they no longer exist.
type consumedLink struct{ actorID *string }

// rejectLink builds the refusal of a link sign-in. Only a consumed link is audited, so an
// unauthenticated caller cannot grow the audit log with unknown or malformed codes.
func rejectLink(ctx context.Context, deps Deps, ip, reason string, consumed *consumedLink) error {
	if consumed != nil {
		if err := deps.Audit.Record(ctx, kernel.AuditEntry{
			ActorID: consumed.actorID,
			Action:  "auth.login_failed",
			Meta:    map[string]any{"reason": reason},
		}); err != nil {
			return err
		}
	}
	if allowed, retryAfterSec := deps.LinkLimiter.Hit(ip); !allowed {
		return domain.ErrRetryAfter(domain.CodeRateLimited, "too many failed attempts", retryAfterSec)
	}
	if reason == domain.CodeDisabled {
		return domain.Err(domain.CodeDisabled, "account is disabled")
	}
	return domain.Err(domain.CodeInvalidLink, "the link is invalid or has expired")
}

// BootstrapLocalAdmin is for a server without OIDC: it issues a link for local-admin, creating
// that admin when there are no people at all (domain.NeedsBootstrapLink decides). It also issues a
// new link while local-admin is the only person and has never signed in, so a failed first start
// cannot lock the install out. It returns the code, or issued=false when nothing is due or the
// link could not be issued (that refusal is swallowed: the start continues). A failure of
// provisioning or of the store is returned. Two replicas starting on an empty database both
// provision; the first replica's logged link may be dead, recovery is `login-link local-admin`.
func BootstrapLocalAdmin(ctx context.Context, deps Deps) (code string, issued bool, err error) {
	users, err := deps.Users.List(ctx)
	if err != nil {
		return "", false, err
	}
	people := make([]domain.Person, 0, len(users))
	for _, u := range users {
		people = append(people, domain.Person{Sub: u.OIDCSub, Disabled: u.Disabled, CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt})
	}
	switch domain.NeedsBootstrapLink(people, LocalAdminSub) {
	case domain.BootstrapNone:
		return "", false, nil
	case domain.BootstrapCreate:
		if _, err := ProvisionUser(ctx, deps, ProvisionInput{Sub: LocalAdminSub, Name: localAdminName, Role: RoleAdmin}); err != nil {
			return "", false, err
		}
	case domain.BootstrapReissue:
	}
	link, err := IssueLoginLink(ctx, deps, IssueLoginLinkInput{Sub: LocalAdminSub})
	if err != nil {
		if _, refusal := domain.AsError(err); refusal {
			return "", false, nil
		}
		return "", false, err
	}
	return link.Code, true, nil
}
