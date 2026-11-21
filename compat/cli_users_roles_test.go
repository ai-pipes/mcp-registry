//go:build compat

package compat_test

// create-user's role flags, judged through the CLI and the database only (no catalog, no /mcp).
// Source: apps/server/test/cli.int.test.ts ("create-user is idempotent by sub") and the usage text of
// `create-user`. TestCLICreateUser holds the same steps behind a request to /api/servers; this
// scenario needs identity alone, so it runs against the Go server before the catalog exists.
// The decided deviation create-user-keeps-role (compat/harness/deviations.go) splits the end of it.

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func TestCLICreateUserRoleFlags(t *testing.T) {
	s := harness.Shared(t)
	sub := harness.Unique("cli-roles")
	role := func() string { return cliMustUser(t, s, sub).Role }

	if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Role Example", "--admin"); r.Code != 0 {
		t.Fatalf("create-user --admin: %v", r)
	}
	id := cliMustUser(t, s, sub).ID
	if got := role(); got != "admin" {
		t.Fatalf("a new person created with --admin: role %q", got)
	}
	// Control: the role the CLI stored is the one the API reports.
	c := s.Anonymous()
	if lr := identityPostLink(t, c, identityLinkFor(t, s, sub)); lr.Status != 204 {
		t.Fatalf("sign in: %d", lr.Status)
	}
	if me := whoami(t, c); me.UserID != id || me.Role != "admin" {
		t.Fatalf("me: %+v", me)
	}

	if s.Fixed(harness.DevCreateUserKeepsRole) {
		// Decided (create-user-keeps-role): an existing person's role changes only with an explicit flag.
		if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Role Example"); r.Code != 0 {
			t.Fatalf("create-user without a role flag: %v", r)
		}
		if got := role(); got != "admin" {
			t.Fatalf("an admin run again without a flag: role %q, want admin", got)
		}
		if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Role Example", "--member"); r.Code != 0 {
			t.Fatalf("create-user --member: %v", r)
		}
		if got := role(); got != "member" {
			t.Fatalf("after --member: role %q, want member", got)
		}
		// Control: a member run again without a flag stays a member.
		if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Role Example"); r.Code != 0 {
			t.Fatalf("create-user without a role flag, again: %v", r)
		}
		if got := role(); got != "member" {
			t.Fatalf("a member run again without a flag: role %q, want member", got)
		}
		// Both flags together contradict each other: a usage error, and nothing changes. The message
		// is not pinned.
		if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Changed", "--admin", "--member"); r.Code != 2 {
			t.Fatalf("--admin with --member: exit %d, want 2: %v", r.Code, r)
		}
		if p := cliMustUser(t, s, sub); p.Role != "member" || p.Name != "Role Example" {
			t.Fatalf("a refused invocation changed the person: %+v", p)
		}
		return
	}

	// 1.0.x: a run without --admin demotes an existing admin to a member, and there is no --member.
	if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Role Example"); r.Code != 0 {
		t.Fatalf("create-user without --admin: %v", r)
	}
	if got := role(); got != "member" {
		t.Fatalf("an admin run again without --admin: role %q, want member", got)
	}
	if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Changed", "--admin"); r.Code != 0 {
		t.Fatalf("create-user --admin on a member: %v", r)
	}
	if got := role(); got != "admin" {
		t.Fatalf("a member run with --admin: role %q, want admin", got)
	}
	if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Other", "--member"); r.Code != 2 {
		t.Fatalf("--member is an unknown option: exit %d, want 2: %v", r.Code, r)
	}
	if p := cliMustUser(t, s, sub); p.Role != "admin" || p.Name != "Changed" {
		t.Fatalf("a refused invocation changed the person: %+v", p)
	}
}
