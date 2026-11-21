//go:build compat

package compat_test

// A scenario of the platform that needs no person, no session, no token and no CLI, so it runs
// against an image that has only the platform layer.
//
// TestPlatformStartsOnReferenceDatabase: the database is created by the REFERENCE image (the
// TypeScript one, harness.ReferenceImage), then the app container is swapped for the image under
// test (COMPAT_IMAGE) with the same key. That is the hand-over an operator makes when changing
// images in place. The new image must:
//   - start healthy, apply no migration (the ten names in platform.schema_migrations are the same
//     and no row is new), leave the key check byte for byte as it was, log nothing at error level
//     or above, and serve the web shell on GET /;
//   - refuse another key: exit 1, one fatal record (the operator-facing key-mismatch text, which
//     names the README section), no key material in the log, the key check unchanged;
//   - refuse a database with a migration it does not know: exit 1, one fatal record that names that
//     migration, nothing applied; and start again once the row is gone.
//   - hand over the PEOPLE too: an admin and a member created and signed in by the reference image,
//     and an API token the admin created through the web API, are still there: both session
//     cookies work (GET /api/auth/me names the same person), the token list shows the token with
//     the same id and prefix, and a new sign-in by a link of the existing admin works.
// When the image under test IS the reference image (the default) this is a restart test.
//
// TestPlatformReverseHandOver is the other direction: the database is created by the image under
// test, then the app container is swapped for the REFERENCE image with the same key. What the new
// image wrote (the migrations, the sealed key check) must be accepted by 1.0.x as it is: healthy,
// nothing applied, the key check untouched, the web shell served, and the people, sessions and
// token that the image under test created are accepted the same way as in the forward direction.
// This is the rollback an operator makes after trying the new image.
//
// The token's use on /mcp is not checked here: the Go server has no /mcp before milestone 6 (the
// scenarios that use a token on /mcp are skipped for it until then), so the hand-over proves the
// token row only (id, prefix, not revoked), not that the other image still accepts the value.

import (
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func migrationNames(t *testing.T, s *harness.Stack) []string {
	t.Helper()
	rows, err := s.DB.Query(t.Context(), `select name from platform.schema_migrations order by name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// handOverPeople is what one image leaves behind for the next: two signed-in people and a token.
type handOverPeople struct {
	admin, member       *harness.Client
	adminSub, memberSub string
	adminID, memberID   string
	tokenID, tokenValue string
	tokenPrefix         string
}

type handOverToken struct {
	ID, Name, Prefix string
	LastUsedAt       *string
	RevokedAt        *string
}

func handOverTokens(t *testing.T, c *harness.Client) []handOverToken {
	t.Helper()
	var out []handOverToken
	c.Do(t, "GET", "/api/tokens", nil).JSON(t, "TokenSummary[]", &out)
	return out
}

// createHandOverPeople makes an admin and a member, signs both in with a link and lets the admin
// create a token, all through the image the stack runs now.
func createHandOverPeople(t *testing.T, s *harness.Stack) *handOverPeople {
	t.Helper()
	p := &handOverPeople{adminSub: harness.Unique("hand-admin"), memberSub: harness.Unique("hand-member")}
	for _, c := range []struct {
		sub, name string
		extra     []string
		client    **harness.Client
	}{
		{p.adminSub, "Hand Admin", []string{"--admin"}, &p.admin},
		{p.memberSub, "Hand Member", nil, &p.member},
	} {
		args := append([]string{"create-user", "--sub", c.sub, "--name", c.name}, c.extra...)
		if r := s.CLI(t, "", args...); r.Code != 0 {
			t.Fatalf("create-user %s: %d %s", c.sub, r.Code, r.Stderr)
		}
		client := s.Anonymous()
		if r := identityPostLink(t, client, identityLinkFor(t, s, c.sub)); r.Status != 204 {
			t.Fatalf("sign in %s: %d %s", c.sub, r.Status, r.Body)
		}
		*c.client = client
	}
	p.adminID, p.memberID = whoami(t, p.admin).UserID, whoami(t, p.member).UserID
	p.tokenValue = p.admin.Token(t, "hand-over")
	list := handOverTokens(t, p.admin)
	if len(list) != 1 || list[0].Name != "hand-over" || !strings.HasPrefix(p.tokenValue, list[0].Prefix) {
		t.Fatalf("the token list after creating the token: %+v", list)
	}
	p.tokenID, p.tokenPrefix = list[0].ID, list[0].Prefix
	return p
}

// checkHandOverPeople asserts, against the image the stack runs now, that what the other image
// created is accepted: the sessions, the token row, and a new sign-in.
func checkHandOverPeople(t *testing.T, s *harness.Stack, p *handOverPeople) {
	t.Helper()
	if me := whoami(t, p.admin); me.UserID != p.adminID || me.Role != "admin" || me.Name != "Hand Admin" {
		t.Fatalf("the admin's old session: %+v, want %s as admin", me, p.adminID)
	}
	if me := whoami(t, p.member); me.UserID != p.memberID || me.Role != "member" || me.Name != "Hand Member" {
		t.Fatalf("the member's old session: %+v, want %s as member", me, p.memberID)
	}
	// The sessions are real: the admin may list people, the member may not.
	if r := p.admin.Do(t, "GET", "/api/users", nil); r.Status != 200 {
		t.Fatalf("the admin's old session on /api/users: %d %s", r.Status, r.Body)
	}
	if r := p.member.Do(t, "GET", "/api/users", nil); r.Status != 403 {
		t.Fatalf("the member's old session on /api/users: %d, want 403", r.Status)
	}
	list := handOverTokens(t, p.admin)
	if len(list) != 1 || list[0].ID != p.tokenID || list[0].Prefix != p.tokenPrefix || list[0].Name != "hand-over" || list[0].RevokedAt != nil {
		t.Fatalf("the token list: %+v, want id %s prefix %s, not revoked", list, p.tokenID, p.tokenPrefix)
	}
	if got := handOverTokens(t, p.member); len(got) != 0 {
		t.Fatalf("the member sees %d token(s), want none", len(got))
	}
	// A new sign-in of the existing admin works and names the same person.
	fresh := s.Anonymous()
	if r := identityPostLink(t, fresh, identityLinkFor(t, s, p.adminSub)); r.Status != 204 {
		t.Fatalf("a new sign-in: %d %s", r.Status, r.Body)
	}
	if me := whoami(t, fresh); me.UserID != p.adminID {
		t.Fatalf("a new sign-in names %+v, want %s", me, p.adminID)
	}
}

func TestPlatformStartsOnReferenceDatabase(t *testing.T) {
	harness.RequireReferenceImage(t)
	key := newKey(t)
	s := harness.Start(t, harness.Options{
		Image: harness.ReferenceImage(),
		CLI:   harness.ReferenceCLI(),
		Env:   map[string]string{"SECRETS_KEY": key},
	})
	image, cli := platformImage(), harness.DefaultCLI()

	migrations := migrationNames(t, s)
	if len(migrations) == 0 || migrations[0] != "0001_platform.sql" {
		t.Fatalf("the reference image recorded the migrations %v", migrations)
	}
	check := keyCheckBytes(t, s)
	people := createHandOverPeople(t, s)

	t.Run("the same key: healthy, nothing applied, the check untouched, the shell served", func(t *testing.T) {
		s.SwapImage(t, image, cli)
		if got := migrationNames(t, s); !slices.Equal(got, migrations) {
			t.Fatalf("migrations before %v, after %v", migrations, got)
		}
		if n := countSQL(t, s, "select count(*) from platform.key_check"); n != 1 || string(keyCheckBytes(t, s)) != string(check) {
			t.Fatalf("the key check changed (%d rows)", n)
		}
		recs, other := logRecords(t, s.Logs(t))
		if len(other) != 0 {
			t.Errorf("log lines that are not JSON: %q", other)
		}
		if bad := recordsFrom(recs, "error"); len(bad) != 0 {
			t.Fatalf("records at error level or above on a healthy start: %v", bad)
		}
		if n := len(recordsWithMsg(recs, keyCreatedMsg)); n != 0 {
			t.Fatalf("the key check was recorded again: %d records", n)
		}
		r := s.Anonymous().Do(t, "GET", "/", nil, "Accept: text/html")
		if r.Status != 200 || !strings.Contains(string(r.Body), `<div id="root">`) {
			t.Fatalf("GET /: %d %.200s", r.Status, r.Body)
		}
	})

	t.Run("the people of the reference image: old sessions and the token are accepted, a new sign-in works", func(t *testing.T) {
		// The image under test is running (the previous subtest swapped to it).
		checkHandOverPeople(t, s, people)
	})

	t.Run("another key: exit 1, one fatal record, no key in the log, the check unchanged", func(t *testing.T) {
		wrong := newKey(t)
		code, logs := s.SwapImageExpectingExit(t, image, cli, map[string]string{"SECRETS_KEY": wrong})
		if code != 1 {
			t.Fatalf("exit code %d, logs: %s", code, logs)
		}
		recs, _ := logRecords(t, logs)
		fatal := recordsAt(recs, "fatal")
		if len(fatal) != 1 || fatal[0]["msg"] != keyMismatchMsg {
			t.Fatalf("fatal records: %v", fatal)
		}
		assertNoKey(t, logs, key, wrong)
		if string(keyCheckBytes(t, s)) != string(check) {
			t.Fatal("a refused start changed the key check")
		}
	})

	t.Run("a migration the image does not know: exit 1, one fatal record naming it, nothing applied", func(t *testing.T) {
		execSQL(t, s, `insert into platform.schema_migrations (name) values ($1)`, newerDBMigrName)
		code, logs := s.SwapImageExpectingExit(t, image, cli, nil)
		if code != 1 {
			t.Fatalf("exit code %d, logs: %s", code, logs)
		}
		recs, _ := logRecords(t, logs)
		fatal := recordsAt(recs, "fatal")
		if len(fatal) != 1 {
			t.Fatalf("fatal records: %v", fatal)
		}
		// The two fragments an operator acts on, as in TestPlatformMigrationsAndKeyCheck.
		msg, _ := fatal[0]["msg"].(string)
		if !strings.Contains(msg, "migrated by a newer version of mcp-registry (unknown migration: "+newerDBMigrName+")") ||
			!strings.Contains(msg, "restore the backup taken before the upgrade") {
			t.Fatalf("fatal message: %q", msg)
		}
		if got := migrationNames(t, s); len(got) != len(migrations)+1 {
			t.Fatalf("migrations after the refusal: %v", got)
		}
		assertNoKey(t, logs, key)
		execSQL(t, s, `delete from platform.schema_migrations where name = $1`, newerDBMigrName)
		s.SwapImage(t, image, cli) // the lock was released and the key is the right one: a normal start
		if got := migrationNames(t, s); !slices.Equal(got, migrations) {
			t.Fatalf("migrations %v after the row was removed", got)
		}
	})
}

func TestPlatformReverseHandOver(t *testing.T) {
	harness.RequireReferenceImage(t)
	key := newKey(t)
	s := harness.Start(t, harness.Options{
		Image: platformImage(),
		CLI:   harness.DefaultCLI(),
		Env:   map[string]string{"SECRETS_KEY": key},
	})
	migrations := migrationNames(t, s)
	if len(migrations) == 0 || migrations[0] != "0001_platform.sql" {
		t.Fatalf("the image under test recorded the migrations %v", migrations)
	}
	if n := countSQL(t, s, "select count(*) from platform.key_check"); n != 1 {
		t.Fatalf("the image under test recorded %d key checks, want 1", n)
	}
	check := keyCheckBytes(t, s)
	people := createHandOverPeople(t, s)

	s.SwapImage(t, harness.ReferenceImage(), harness.ReferenceCLI())

	if got := migrationNames(t, s); !slices.Equal(got, migrations) {
		t.Fatalf("migrations before %v, after the reference image started: %v", migrations, got)
	}
	if n := countSQL(t, s, "select count(*) from platform.key_check"); n != 1 || string(keyCheckBytes(t, s)) != string(check) {
		t.Fatalf("the key check changed under the reference image (%d rows)", n)
	}
	recs, other := logRecords(t, s.Logs(t))
	if len(other) != 0 {
		t.Errorf("log lines that are not JSON: %q", other)
	}
	if bad := recordsFrom(recs, "error"); len(bad) != 0 {
		t.Fatalf("records at error level or above in the reference image's start: %v", bad)
	}
	if n := len(recordsWithMsg(recs, keyCreatedMsg)); n != 0 {
		t.Fatalf("the reference image recorded the key check again: %d records", n)
	}
	assertNoKey(t, s.Logs(t), key)
	r := s.Anonymous().Do(t, "GET", "/", nil, "Accept: text/html")
	if r.Status != 200 || !strings.Contains(string(r.Body), `<div id="root">`) {
		t.Fatalf("GET /: %d %.200s", r.Status, r.Body)
	}
	checkHandOverPeople(t, s, people)
}
