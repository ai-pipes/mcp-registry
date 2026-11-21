package domain_test

import (
	"reflect"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

func groupsAccess(g ...string) domain.Access {
	return domain.Access{Kind: domain.AccessGroups, Groups: g}
}

func member(g ...string) domain.Actor { return domain.Actor{Role: domain.RoleMember, Groups: g} }

var admin = domain.Actor{Role: domain.RoleAdmin}

func TestCanUse(t *testing.T) {
	all := domain.Access{Kind: domain.AccessAll}
	dev := groupsAccess("dev")
	cases := []struct {
		name   string
		access domain.Access
		status domain.ServerStatus
		actor  domain.Actor
		want   bool
	}{
		{"listed group", dev, domain.StatusActive, member("dev"), true},
		{"other group", dev, domain.StatusActive, member("mcp-users"), false},
		{"no groups", dev, domain.StatusActive, member(), false},
		{"admin any active", dev, domain.StatusActive, admin, true},
		{"all-access member", all, domain.StatusActive, member(), true},
		{"disabled groups member", dev, domain.StatusDisabled, member("dev"), false},
		{"disabled admin", dev, domain.StatusDisabled, admin, false},
		{"disabled all", all, domain.StatusDisabled, member(), false},
		{"unknown status is not active", all, domain.ServerStatus("x"), admin, false},
	}
	for _, c := range cases {
		if got := domain.CanUse(c.access, c.status, c.actor); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestParseAccess(t *testing.T) {
	got, err := domain.ParseAccess(domain.Access{Kind: domain.AccessAll, Groups: []string{"ignored"}})
	if err != nil || !reflect.DeepEqual(got, domain.Access{Kind: domain.AccessAll}) {
		t.Fatalf("all: %+v %v", got, err)
	}
	got, err = domain.ParseAccess(groupsAccess(" dev ", "dev", "ops", "\u00a0ops\ufeff"))
	if err != nil || !reflect.DeepEqual(got, groupsAccess("dev", "ops")) {
		t.Fatalf("trim and dedupe: %+v %v", got, err)
	}
	for _, g := range [][]string{nil, {}, {" "}, {"dev", ""}} {
		_, err := domain.ParseAccess(groupsAccess(g...))
		wantMsg(t, err, "invalid_access", "list at least one non-empty group")
	}
	_, err = domain.ParseAccess(domain.Access{Kind: "x"})
	wantMsg(t, err, "invalid_access", "unknown access kind")
}

func TestIsAdmin(t *testing.T) {
	if !domain.IsAdmin(admin) || domain.IsAdmin(member()) {
		t.Fatal("only the admin role is admin")
	}
}

func TestAccessEqual(t *testing.T) {
	if !groupsAccess("a", "b").Equal(groupsAccess("a", "b")) {
		t.Fatal("same")
	}
	if groupsAccess("a", "b").Equal(groupsAccess("b", "a")) {
		t.Fatal("group order is significant")
	}
	if (domain.Access{Kind: domain.AccessAll}).Equal(groupsAccess("a")) {
		t.Fatal("kinds differ")
	}
	if !(domain.Access{Kind: domain.AccessAll}).Equal(domain.Access{Kind: domain.AccessAll}) {
		t.Fatal("all equals all")
	}
}

func TestParseAccessCopies(t *testing.T) {
	in := []string{"b", "a"}
	got, err := domain.ParseAccess(domain.Access{Kind: domain.AccessGroups, Groups: in})
	if err != nil {
		t.Fatal(err)
	}
	in[0] = "changed"
	if got.Groups[0] != "b" {
		t.Fatal("shares the input slice")
	}
}
