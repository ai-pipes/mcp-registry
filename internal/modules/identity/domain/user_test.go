package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

var policy = domain.GroupPolicy{LoginGroup: "mcp-users", AdminGroup: "mcp-admins"}

func baseUser() domain.User {
	return domain.User{
		ID: "u1", OIDCSub: "sub-1", Name: "Anna", Groups: []string{"mcp-users"},
		Role: domain.RoleMember, CreatedAt: time.Unix(0, 0),
	}
}

func TestAccessFor(t *testing.T) {
	cases := []struct {
		name   string
		groups []string
		want   domain.Role
		ok     bool
	}{
		{"login group", []string{"mcp-users"}, domain.RoleMember, true},
		{"admin group alone", []string{"mcp-admins"}, domain.RoleAdmin, true},
		{"both: admin wins", []string{"mcp-users", "mcp-admins"}, domain.RoleAdmin, true},
		{"other group", []string{"family"}, "", false},
		{"no groups", []string{}, "", false},
		{"nil groups", nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := domain.AccessFor(c.groups, policy)
			if got != c.want || ok != c.ok {
				t.Fatalf("AccessFor(%v) = %q, %v; want %q, %v", c.groups, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestPrincipalOf(t *testing.T) {
	u := baseUser()
	p := domain.PrincipalOf(u, domain.ViaSession, "")
	if p.UserID != "u1" || p.Name != "Anna" || p.Role != domain.RoleMember || p.Via != domain.ViaSession || p.TokenID != "" {
		t.Fatalf("principal = %+v", p)
	}
	if len(p.Groups) != 1 || p.Groups[0] != "mcp-users" {
		t.Fatalf("groups = %v", p.Groups)
	}
	tp := domain.PrincipalOf(u, domain.ViaToken, "t1")
	if tp.Via != domain.ViaToken || tp.TokenID != "t1" {
		t.Fatalf("token principal = %+v", tp)
	}
}

func TestPrincipalOfGroupsNeverNil(t *testing.T) {
	u := baseUser()
	u.Groups = nil
	p := domain.PrincipalOf(u, domain.ViaSession, "")
	if p.Groups == nil || len(p.Groups) != 0 {
		t.Fatalf("groups = %#v, want empty non-nil", p.Groups)
	}
	if g := domain.CopyGroups(nil); g == nil {
		t.Fatal("CopyGroups(nil) must be non-nil")
	}
}

func TestPrincipalOfDoesNotAliasUserGroups(t *testing.T) {
	u := baseUser()
	p := domain.PrincipalOf(u, domain.ViaSession, "")
	p.Groups[0] = "changed"
	if u.Groups[0] != "mcp-users" {
		t.Fatal("principal shares the user's groups slice")
	}
}

func TestErrorCarriesCodeAndRetry(t *testing.T) {
	e := domain.ErrRetryAfter(domain.CodeRateLimited, "m", 5)
	if e.Code != "rate_limited" || e.Message != "m" || e.RetryAfterSec != 5 || e.Error() != "m" {
		t.Fatalf("error = %+v", e)
	}
	var err error = e
	de, ok := kernel.AsDomainError(err)
	if !ok || de.Code != "rate_limited" {
		t.Fatalf("kernel.AsDomainError = %v, %v", de, ok)
	}
	got, ok := domain.AsError(errors.Join(errors.New("x"), err))
	if !ok || got.RetryAfterSec != 5 {
		t.Fatalf("AsError = %v, %v", got, ok)
	}
	if _, ok := domain.AsError(errors.New("plain")); ok {
		t.Fatal("AsError matched a plain error")
	}
	if domain.Err(domain.CodeNotFound, "x").RetryAfterSec != 0 {
		t.Fatal("Err sets a retry delay")
	}
}

func TestErrorCodeValues(t *testing.T) {
	want := map[string]string{
		domain.CodeInvalidState: "invalid_state", domain.CodeNotAllowed: "not_allowed",
		domain.CodeDisabled: "disabled", domain.CodeInvalidToken: "invalid_token",
		domain.CodeRateLimited: "rate_limited", domain.CodeForbidden: "forbidden",
		domain.CodeNotFound: "not_found", domain.CodeInvalidInput: "invalid_input",
		domain.CodeInvalidLink: "invalid_link", domain.CodeIDPUnavailable: "idp_unavailable",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("code %q != %q", got, w)
		}
	}
}

func principal(id string, role domain.Role) domain.Principal {
	return domain.Principal{UserID: id, Name: id, Role: role, Groups: []string{}, Via: domain.ViaSession}
}

func TestAuthorizationPolicy(t *testing.T) {
	if !domain.IsAdmin(principal("a", domain.RoleAdmin)) || domain.IsAdmin(principal("a", domain.RoleMember)) {
		t.Fatal("IsAdmin does not follow the role")
	}
	if !domain.CanRevokeToken(principal("a", domain.RoleMember), "a") {
		t.Error("owner may revoke")
	}
	if domain.CanRevokeToken(principal("b", domain.RoleMember), "a") {
		t.Error("stranger may not revoke")
	}
	if !domain.CanRevokeToken(principal("root", domain.RoleAdmin), "a") {
		t.Error("admin may revoke")
	}
}

func TestDisableDenial(t *testing.T) {
	d := domain.DisableDenial(principal("a", domain.RoleMember), "b", true)
	if d == nil || d.Code != domain.CodeForbidden || d.Message != "admin only" {
		t.Fatalf("member: %+v", d)
	}
	d = domain.DisableDenial(principal("root", domain.RoleAdmin), "root", true)
	if d == nil || d.Code != domain.CodeForbidden || d.Message != "cannot disable yourself" {
		t.Fatalf("self: %+v", d)
	}
	if domain.DisableDenial(principal("root", domain.RoleAdmin), "root", false) != nil {
		t.Error("enabling yourself is allowed")
	}
	if domain.DisableDenial(principal("root", domain.RoleAdmin), "b", true) != nil {
		t.Error("admin disabling another is allowed")
	}
}

func TestIsActiveAdmin(t *testing.T) {
	u := baseUser()
	u.Role = domain.RoleAdmin
	if !domain.IsActiveAdmin(&u) {
		t.Error("enabled admin")
	}
	m := baseUser()
	if domain.IsActiveAdmin(&m) {
		t.Error("member")
	}
	u.Disabled = true
	if domain.IsActiveAdmin(&u) {
		t.Error("disabled admin")
	}
	if domain.IsActiveAdmin(nil) {
		t.Error("nil")
	}
}

func TestProvisionedGroups(t *testing.T) {
	eq := func(got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
	eq(domain.ProvisionedGroups(nil, false, policy), []string{"mcp-users"})
	eq(domain.ProvisionedGroups(nil, true, policy), []string{"mcp-admins"})
	eq(domain.ProvisionedGroups([]string{"devs", "mcp-admins"}, false, policy), []string{"devs", "mcp-users"})
	eq(domain.ProvisionedGroups([]string{"devs", "mcp-users"}, true, policy), []string{"devs", "mcp-admins"})
	eq(domain.ProvisionedGroups([]string{"mcp-users", "mcp-admins", "x"}, true, policy), []string{"x", "mcp-admins"})
	for _, admin := range []bool{true, false} {
		role, _ := domain.AccessFor(domain.ProvisionedGroups(nil, admin, policy), policy)
		want := domain.RoleMember
		if admin {
			want = domain.RoleAdmin
		}
		if role != want {
			t.Errorf("admin=%v gives %q", admin, role)
		}
	}
}

func TestPlanProvision(t *testing.T) {
	existingAdmin := &domain.User{Role: domain.RoleAdmin, Groups: []string{"devs", "mcp-admins"}}
	existingMember := &domain.User{Role: domain.RoleMember, Groups: []string{"devs", "mcp-users"}}

	groups, role := domain.PlanProvision(nil, domain.ChooseAdmin, policy)
	if role != domain.RoleAdmin || len(groups) != 1 || groups[0] != "mcp-admins" {
		t.Errorf("new admin: %v %v", groups, role)
	}
	groups, role = domain.PlanProvision(nil, domain.ChooseMember, policy)
	if role != domain.RoleMember || len(groups) != 1 || groups[0] != "mcp-users" {
		t.Errorf("new member: %v %v", groups, role)
	}
	groups, role = domain.PlanProvision(nil, domain.ChooseKeep, policy)
	if role != domain.RoleMember || len(groups) != 1 || groups[0] != "mcp-users" {
		t.Errorf("new, keep means member: %v %v", groups, role)
	}
	groups, role = domain.PlanProvision(existingMember, domain.ChooseAdmin, policy)
	if role != domain.RoleAdmin || len(groups) != 2 || groups[0] != "devs" || groups[1] != "mcp-admins" {
		t.Errorf("promote: %v %v", groups, role)
	}
	groups, role = domain.PlanProvision(existingAdmin, domain.ChooseMember, policy)
	if role != domain.RoleMember || len(groups) != 2 || groups[1] != "mcp-users" {
		t.Errorf("demote: %v %v", groups, role)
	}
	groups, role = domain.PlanProvision(existingAdmin, domain.ChooseKeep, policy)
	if role != domain.RoleAdmin || len(groups) != 2 || groups[0] != "devs" || groups[1] != "mcp-admins" {
		t.Errorf("keep admin: %v %v", groups, role)
	}
	groups, _ = domain.PlanProvision(existingAdmin, domain.ChooseKeep, policy)
	groups[0] = "changed"
	if existingAdmin.Groups[0] != "devs" {
		t.Error("keep shares the existing groups slice")
	}
	_, role = domain.PlanProvision(&domain.User{Role: domain.RoleMember, Groups: nil}, domain.ChooseKeep, policy)
	if role != domain.RoleMember {
		t.Errorf("keep member: %v", role)
	}
}
