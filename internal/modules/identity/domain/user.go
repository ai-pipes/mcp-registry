package domain

import (
	"slices"
	"time"
)

// Role is what a person may do: admins manage servers and people, members use the gateway.
type Role string

// The two roles.
const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Via says how a Principal authenticated.
type Via string

// How a principal authenticated: a web session or an API token.
const (
	ViaSession Via = "session"
	ViaToken   Via = "token"
)

// Principal is who is making a request: a person with a session, or the owner of an sk_ token.
type Principal struct {
	UserID string
	Name   string
	Role   Role
	// Groups as of the person's last login; never nil.
	Groups []string
	Via    Via
	// TokenID is set (non-empty) when Via is ViaToken.
	TokenID string
}

// OIDCClaims is what the identity provider tells us about a person.
type OIDCClaims struct {
	Sub    string
	Name   string
	Email  *string
	Groups []string
}

// User is a person known to the gateway.
type User struct {
	ID          string
	OIDCSub     string
	Name        string
	Email       *string
	Groups      []string
	Role        Role
	Disabled    bool
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

// GroupPolicy names the provider groups that open the door (LoginGroup) and that make a person an admin.
type GroupPolicy struct {
	LoginGroup string
	AdminGroup string
}

// RoleChoice is what an operator asks for when provisioning a person.
type RoleChoice int

// The three role arguments of provisioning.
const (
	// ChooseKeep leaves an existing person's role and policy groups alone; a new person becomes a member.
	ChooseKeep RoleChoice = iota
	ChooseAdmin
	ChooseMember
)

// CopyGroups returns a fresh, never-nil copy of groups.
func CopyGroups(groups []string) []string {
	return append([]string{}, groups...)
}

// AccessFor is the role a person with these groups gets; ok is false when they may not sign in at all.
func AccessFor(groups []string, policy GroupPolicy) (role Role, ok bool) {
	if slices.Contains(groups, policy.AdminGroup) {
		return RoleAdmin, true
	}
	if slices.Contains(groups, policy.LoginGroup) {
		return RoleMember, true
	}
	return "", false
}

// ProvisionedGroups are the groups for a person the operator provisions without the provider: the
// groups of their last login (nil for a new person) without the two policy groups, plus exactly one
// of them: the admin group with admin, else the login group. Their next real login replaces them.
func ProvisionedGroups(existing []string, admin bool, policy GroupPolicy) []string {
	out := make([]string, 0, len(existing)+1)
	for _, g := range existing {
		if g != policy.LoginGroup && g != policy.AdminGroup {
			out = append(out, g)
		}
	}
	if admin {
		return append(out, policy.AdminGroup)
	}
	return append(out, policy.LoginGroup)
}

// PlanProvision decides the groups and role a provisioned person ends up with. existing is nil for
// a new person. ChooseAdmin and ChooseMember swap the policy group; ChooseKeep keeps the existing
// groups and role (a new person becomes a member).
func PlanProvision(existing *User, choice RoleChoice, policy GroupPolicy) (groups []string, role Role) {
	if choice == ChooseKeep && existing != nil {
		return CopyGroups(existing.Groups), existing.Role
	}
	var current []string
	if existing != nil {
		current = existing.Groups
	}
	groups = ProvisionedGroups(current, choice == ChooseAdmin, policy)
	role, ok := AccessFor(groups, policy)
	if !ok {
		role = RoleMember
	}
	return groups, role
}

// IsAdmin reports whether the principal is an admin.
func IsAdmin(p Principal) bool { return p.Role == RoleAdmin }

// IsActiveAdmin is an admin as of their last login who has not been disabled since.
func IsActiveAdmin(u *User) bool {
	return u != nil && !u.Disabled && u.Role == RoleAdmin
}

// CanRevokeToken reports whether p may revoke a token of tokenOwnerID: anyone may revoke their own token; admins may revoke anyone's.
func CanRevokeToken(p Principal, tokenOwnerID string) bool {
	return p.UserID == tokenOwnerID || IsAdmin(p)
}

// DisableDenial is why p may not set the disabled flag of targetID, or nil when allowed.
func DisableDenial(p Principal, targetID string, disabled bool) *Error {
	if !IsAdmin(p) {
		return Err(CodeForbidden, "admin only")
	}
	if disabled && targetID == p.UserID {
		return Err(CodeForbidden, "cannot disable yourself")
	}
	return nil
}

// PrincipalOf is the principal of a stored person; tokenID is empty unless via is ViaToken.
func PrincipalOf(u User, via Via, tokenID string) Principal {
	return Principal{
		UserID:  u.ID,
		Name:    u.Name,
		Role:    u.Role,
		Groups:  CopyGroups(u.Groups),
		Via:     via,
		TokenID: tokenID,
	}
}
