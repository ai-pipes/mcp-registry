package domain

import "slices"

// AccessKind says who may use a server.
type AccessKind string

// The kinds of access.
const (
	AccessAll    AccessKind = "all"
	AccessGroups AccessKind = "groups"
)

// Access is "everybody who may sign in" or a list of groups. Group order is kept: it takes part
// in the "access changed" comparison, as in the TypeScript code.
type Access struct {
	Kind   AccessKind
	Groups []string // only for AccessGroups
}

// ServerStatus is active or disabled.
type ServerStatus string

// The statuses of a server.
const (
	StatusActive   ServerStatus = "active"
	StatusDisabled ServerStatus = "disabled"
)

// Role is the part of a person the catalog rules need.
type Role string

// The roles the catalog rules distinguish.
const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Actor is a person as the catalog rules see them (the application layer maps the identity
// principal onto it).
type Actor struct {
	Role   Role
	Groups []string
}

// ParseAccess validates and normalises access: groups are trimmed and de-duplicated (first
// occurrence kept), and at least one non-empty group is required.
func ParseAccess(a Access) (Access, error) {
	switch a.Kind {
	case AccessAll:
		return Access{Kind: AccessAll}, nil
	case AccessGroups:
		groups := make([]string, 0, len(a.Groups))
		for _, g := range a.Groups {
			g = trim(g)
			if !slices.Contains(groups, g) {
				groups = append(groups, g)
			}
		}
		if len(groups) == 0 || slices.Contains(groups, "") {
			return Access{}, newError(CodeInvalidAccess, "list at least one non-empty group")
		}
		return Access{Kind: AccessGroups, Groups: groups}, nil
	default:
		return Access{}, newError(CodeInvalidAccess, "unknown access kind")
	}
}

// Equal compares kind and groups in order.
func (a Access) Equal(b Access) bool {
	return a.Kind == b.Kind && slices.Equal(a.Groups, b.Groups)
}

// IsAdmin reports whether the actor is an administrator.
func IsAdmin(a Actor) bool { return a.Role == RoleAdmin }

// CanUse reports whether the actor may use the server: an admin can use any active server; a member only an active one open to all or to one
// of their groups; a disabled server nobody.
func CanUse(access Access, status ServerStatus, actor Actor) bool {
	if status != StatusActive {
		return false
	}
	if IsAdmin(actor) || access.Kind == AccessAll {
		return true
	}
	for _, g := range actor.Groups {
		if slices.Contains(access.Groups, g) {
			return true
		}
	}
	return false
}
