package app

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

// UserSummary is a person as listed to admins.
type UserSummary struct {
	ID          string
	Name        string
	Email       *string
	Role        domain.Role
	Groups      []string // never nil
	Disabled    bool
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

// RoleChoice is the role argument of ProvisionUser; see the constants.
type RoleChoice = domain.RoleChoice

// The three role arguments: for an existing person RoleKeep leaves the role and the policy groups
// as they are, for a new person it means member.
const (
	RoleAdmin  = domain.ChooseAdmin
	RoleMember = domain.ChooseMember
	RoleKeep   = domain.ChooseKeep
)

// ListUsers returns everyone; admins only.
func ListUsers(ctx context.Context, deps Deps, principal domain.Principal) ([]UserSummary, error) {
	if !domain.IsAdmin(principal) {
		return nil, domain.Err(domain.CodeForbidden, "admin only")
	}
	rows, err := deps.Users.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]UserSummary, 0, len(rows))
	for _, u := range rows {
		out = append(out, UserSummary{
			ID: u.ID, Name: u.Name, Email: u.Email, Role: u.Role, Groups: domain.CopyGroups(u.Groups),
			Disabled: u.Disabled, CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt,
		})
	}
	return out, nil
}

// SetDisabledInput is the input of SetDisabled.
type SetDisabledInput struct {
	Principal domain.Principal
	UserID    string
	Disabled  bool
}

// SetDisabled sets the disabled flag of a person. Disabling also ends every session, drops pending
// login links and revokes every token of the person; enabling restores nothing. It is not
// transactional, the steps run in that order, and the change is audited even when nothing changed.
func SetDisabled(ctx context.Context, deps Deps, in SetDisabledInput) error {
	if denied := domain.DisableDenial(in.Principal, in.UserID, in.Disabled); denied != nil {
		return denied
	}
	found, err := deps.Users.SetDisabled(ctx, in.UserID, in.Disabled)
	if err != nil {
		return err
	}
	if !found {
		return domain.Err(domain.CodeNotFound, "user not found")
	}
	if in.Disabled {
		if err := deps.Sessions.DeleteByUser(ctx, in.UserID); err != nil {
			return err
		}
		if err := deps.LoginLinks.DeleteByUser(ctx, in.UserID); err != nil {
			return err
		}
		if err := deps.Tokens.RevokeAllByUser(ctx, in.UserID, deps.Clock.Now()); err != nil {
			return err
		}
	}
	return deps.Audit.Record(ctx, kernel.AuditEntry{
		ActorID: ptr(in.Principal.UserID),
		Action:  "user.disable",
		Target:  ptr(in.UserID),
		Meta:    map[string]any{"disabled": in.Disabled},
	})
}

// IsUserActiveAdmin reports whether the person is still an enabled admin (role as of their last
// login), for work done on an admin's behalf without a session, such as scheduled catalog sync.
func IsUserActiveAdmin(ctx context.Context, deps Deps, userID string) (bool, error) {
	u, err := deps.Users.FindByID(ctx, userID)
	if err != nil {
		return false, err
	}
	return domain.IsActiveAdmin(u), nil
}

// ProvisionInput is the input of ProvisionUser.
type ProvisionInput struct {
	Sub  string
	Name string
	Role RoleChoice
}

// ProvisionResult is what ProvisionUser reports.
type ProvisionResult struct {
	ID      string
	Role    domain.Role
	Created bool
}

// ProvisionUser is operator provisioning (CLI): it creates the person as their first OIDC login
// would, with the login group or, with RoleAdmin, the admin group. It is idempotent by sub: an
// existing person keeps their id, email and other groups, and a disabled person stays disabled.
// Like the first login it stamps last_login_at with now, for an existing person too.
func ProvisionUser(ctx context.Context, deps Deps, in ProvisionInput) (ProvisionResult, error) {
	sub := kernel.JSTrim(in.Sub)
	name := kernel.JSTrim(in.Name)
	if sub == "" || name == "" {
		return ProvisionResult{}, domain.Err(domain.CodeInvalidInput, "sub and name must not be blank")
	}
	existing, err := deps.Users.FindBySub(ctx, sub)
	if err != nil {
		return ProvisionResult{}, err
	}
	if existing != nil && existing.Disabled {
		return ProvisionResult{}, domain.Err(domain.CodeDisabled, "account is disabled")
	}
	groups, role := domain.PlanProvision(existing, in.Role, deps.Policy)
	var email *string
	if existing != nil {
		email = existing.Email
	}
	user, err := deps.Users.UpsertFromLogin(ctx, UpsertLogin{
		Claims: domain.OIDCClaims{Sub: sub, Name: name, Email: email, Groups: groups},
		Role:   role,
		At:     deps.Clock.Now(),
	})
	if err != nil {
		return ProvisionResult{}, err
	}
	created := existing == nil
	if err := deps.Audit.Record(ctx, kernel.AuditEntry{
		Action: "user.provision",
		Target: ptr(user.ID),
		Meta:   map[string]any{"via": "cli", "role": string(role), "created": created},
	}); err != nil {
		return ProvisionResult{}, err
	}
	return ProvisionResult{ID: user.ID, Role: user.Role, Created: created}, nil
}

// ActAs is the principal of an enabled person, for operator commands (CLI) run on their behalf.
func ActAs(ctx context.Context, deps Deps, sub string) (domain.Principal, error) {
	user, err := deps.Users.FindBySub(ctx, sub)
	if err != nil {
		return domain.Principal{}, err
	}
	if user == nil {
		return domain.Principal{}, domain.Err(domain.CodeNotFound, `no user with sub "`+sub+`"`)
	}
	if user.Disabled {
		return domain.Principal{}, domain.Err(domain.CodeDisabled, "account is disabled")
	}
	return domain.PrincipalOf(*user, domain.ViaSession, ""), nil
}
