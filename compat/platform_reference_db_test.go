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
// When the image under test IS the reference image (the default) this is a restart test.

import (
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// referenceMigrations is how many migrations the reference image bundles (apps/server/migrations).
const referenceMigrations = 10

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
	if len(migrations) != referenceMigrations {
		t.Fatalf("the reference image recorded %d migrations %v, want %d", len(migrations), migrations, referenceMigrations)
	}
	check := keyCheckBytes(t, s)

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
		if msg, _ := fatal[0]["msg"].(string); !strings.Contains(msg, newerDBMigrName) {
			t.Fatalf("the fatal record does not name the migration: %q", msg)
		}
		if got := migrationNames(t, s); len(got) != referenceMigrations+1 {
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
