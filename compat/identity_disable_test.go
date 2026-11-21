//go:build compat

package compat_test

// Disabling a person, judged through identity alone: no /mcp, no catalog. The same assertions sit in
// TestIdentityNoOIDC ("disabling a person ends their sessions, tokens and links at once"), which
// also asks /mcp and is skipped against the Go server until the /mcp route exists; this scenario
// carries over every assertion that does not need it, and the check that the token is revoked is
// the stored row. Source: apps/server/test/identity.int.test.ts ("disabling a person ...").

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func TestIdentityDisablingAPerson(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	sub := identityPerson(t, s, "disable")
	member := s.Member(t, sub)
	id := whoami(t, member).UserID
	member.Token(t, "laptop") // the row is what the scenario judges; the value is not used
	pending := identityLinkFor(t, s, sub)

	if n := countSQL(t, s, `select count(*) from identity.tokens where user_id = $1::uuid and revoked_at is null`, id); n != 1 {
		t.Fatalf("%d live tokens before disabling, want 1", n)
	}
	if r := member.Do(t, "GET", "/api/auth/me", nil); r.Status != 200 {
		t.Fatalf("the session before disabling: %d", r.Status)
	}

	off := admin.Do(t, "POST", "/api/users/"+id+"/disabled", map[string]bool{"disabled": true})
	if off.Status != 204 || len(off.Body) != 0 {
		t.Fatalf("disable: %d %q", off.Status, off.Body)
	}

	t.Run("the live session, the pending link and the token are ended at once", func(t *testing.T) {
		if r := member.Do(t, "GET", "/api/auth/me", nil); r.Status != 401 || errorCode(t, r) != identityUnauthenticated {
			t.Fatalf("session after disabling: %d %s", r.Status, r.Body)
		}
		if n := countSQL(t, s, `select count(*) from identity.sessions where user_id = $1::uuid`, id); n != 0 {
			t.Fatalf("%d sessions remain", n)
		}
		if n := countSQL(t, s, `select count(*) from identity.login_links where user_id = $1::uuid`, id); n != 0 {
			t.Fatalf("%d links remain", n)
		}
		if n := countSQL(t, s, `select count(*) from identity.tokens where user_id = $1::uuid and revoked_at is null`, id); n != 0 {
			t.Fatalf("%d tokens are not revoked", n)
		}
		if n := countSQL(t, s, `select count(*) from identity.tokens where user_id = $1::uuid`, id); n != 1 {
			t.Fatalf("%d token rows, want the one revoked token kept", n)
		}
		if r := identityPostLink(t, s.Anonymous(), pending); r.Status != 401 || errorCode(t, r) != identityInvalidLink {
			t.Fatalf("the pending link after disabling: %d %s", r.Status, r.Body)
		}
		// The CLI refuses a new link for a disabled person, with the documented text, and stores none.
		r := s.CLI(t, "", "login-link", sub)
		cliGolden(t, s, "login-link-disabled", r, 1)
		if n := countSQL(t, s, `select count(*) from identity.login_links where user_id = $1::uuid`, id); n != 0 {
			t.Fatalf("a link was stored for a disabled person")
		}
		var users []struct {
			ID       string
			Disabled bool
		}
		admin.Do(t, "GET", "/api/users", nil).JSON(t, "UserSummary[]", &users)
		listed := false
		for _, u := range users {
			if u.ID == id {
				listed = u.Disabled
			}
		}
		if !listed {
			t.Fatal("the person is not listed as disabled")
		}
	})

	t.Run("the audit trail names the admin, the person and the new state", func(t *testing.T) {
		adminID := whoami(t, admin).UserID
		got := auditWhere(auditAll(t, s), "user.disable", func(e auditEntry) bool { return ptrIs(e.Target, id) })
		if len(got) != 1 || !ptrIs(got[0].ActorID, adminID) || got[0].Meta["disabled"] != true || len(got[0].Meta) != 1 {
			t.Fatalf("user.disable audit: %+v (the actor must be %s)", got, adminID)
		}
	})

	t.Run("enabling brings back no token and no session; a new link works", func(t *testing.T) {
		if r := admin.Do(t, "POST", "/api/users/"+id+"/disabled", map[string]bool{"disabled": false}); r.Status != 204 {
			t.Fatalf("enable: %d", r.Status)
		}
		if n := countSQL(t, s, `select count(*) from identity.tokens where user_id = $1::uuid and revoked_at is null`, id); n != 0 {
			t.Fatalf("enabling restored %d token(s)", n)
		}
		if r := member.Do(t, "GET", "/api/auth/me", nil); r.Status != 401 {
			t.Fatalf("the ended session after enabling: %d", r.Status)
		}
		if r := identityPostLink(t, s.Anonymous(), pending); r.Status != 401 {
			t.Fatalf("the old link after enabling: %d", r.Status)
		}
		fresh := s.Anonymous()
		if r := identityPostLink(t, fresh, identityLinkFor(t, s, sub)); r.Status != 204 {
			t.Fatalf("sign-in after enabling: %d", r.Status)
		}
		if me := whoami(t, fresh); me.UserID != id {
			t.Fatalf("me: %+v", me)
		}
		again := auditWhere(auditAll(t, s), "user.disable", func(e auditEntry) bool { return ptrIs(e.Target, id) && e.Meta["disabled"] == false })
		if len(again) != 1 {
			t.Fatalf("the enabling is not audited: %+v", again)
		}
	})

	t.Run("an admin cannot disable themselves", func(t *testing.T) {
		me := whoami(t, admin)
		r := admin.Do(t, "POST", "/api/users/"+me.UserID+"/disabled", map[string]bool{"disabled": true})
		if r.Status != 403 || errorCode(t, r) != "forbidden|cannot disable yourself" {
			t.Fatalf("self-disable: %d %s", r.Status, r.Body)
		}
		if n := countSQL(t, s, `select count(*) from identity.users where id = $1::uuid and not disabled`, me.UserID); n != 1 {
			t.Fatal("the admin was disabled")
		}
		if got := whoami(t, admin); got.UserID != me.UserID {
			t.Fatalf("the admin lost the session: %+v", got)
		}
	})
}
