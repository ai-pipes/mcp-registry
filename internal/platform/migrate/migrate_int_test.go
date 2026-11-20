//go:build integration

package migrate_test

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/ai-pipes/mcp-registry/apps/server/migrations"
	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/ai-pipes/mcp-registry/internal/platform/migrate"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func count(t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func bundled(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestRunAppliesEveryFileOnceThenNothing(t *testing.T) {
	pool := pgtest.Start(t)
	ctx := context.Background()

	applied, err := migrate.Run(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 10 || applied[0] != "0001_platform.sql" || applied[9] != "0010_key_check.sql" {
		t.Fatalf("applied %v", applied)
	}
	if !slices.Equal(applied, bundled(t)) {
		t.Fatalf("applied %v, bundled %v", applied, bundled(t))
	}
	if n := count(t, pool, `select count(*) from platform.schema_migrations`); n != 10 {
		t.Fatalf("recorded %d", n)
	}

	again, err := migrate.Run(ctx, pool, migrations.FS)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run: %v %v", again, err)
	}
}

func TestRunRefusesADatabaseNewerThanTheBinary(t *testing.T) {
	pool := pgtest.Start(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `create schema platform;
		create table platform.schema_migrations (name text primary key, applied_at timestamptz not null default now());
		insert into platform.schema_migrations (name) values ('0011_from_the_future.sql'), ('0012_later.sql')`); err != nil {
		t.Fatal(err)
	}

	applied, err := migrate.Run(ctx, pool, migrations.FS)
	var newer *migrate.NewerDatabaseError
	if !errors.As(err, &newer) || newer.Migration != "0011_from_the_future.sql" {
		t.Fatalf("want NewerDatabaseError for the first unknown name, got %v", err)
	}
	const want = "the database was migrated by a newer version of mcp-registry (unknown migration: 0011_from_the_future.sql); start that version or restore the backup taken before the upgrade"
	if err.Error() != want {
		t.Fatalf("text:\n%s", err.Error())
	}
	if len(applied) != 0 {
		t.Fatalf("applied %v", applied)
	}
	if n := count(t, pool, `select count(*) from platform.schema_migrations`); n != 2 {
		t.Fatalf("recorded %d, want the 2 that were there", n)
	}
	if n := count(t, pool, `select count(*) from information_schema.schemata where schema_name = 'identity'`); n != 0 {
		t.Fatal("a file was applied before the newer-database check")
	}
}

func TestRunRollsBackAFileThatFailsInTheMiddle(t *testing.T) {
	pool := pgtest.Start(t)
	files := fstest.MapFS{
		"0002_bad.sql":  {Data: []byte("create table public.b1 (x int); insert into public.b1 values (1); select 1/0; create table public.b2 (x int);")},
		"0001_good.sql": {Data: []byte("create table public.a (x int);")},
		"README.sql":    {Data: []byte("this is not a migration name")},
		"notes.txt":     {Data: []byte("nor is this")},
		"0003_next.sql": {Data: []byte("create table public.c (x int);")},
	}

	applied, err := migrate.Run(context.Background(), pool, files)
	if err == nil || !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("want an error naming the file, got %v", err)
	}
	if !slices.Equal(applied, []string{"0001_good.sql"}) {
		t.Fatalf("applied %v", applied)
	}
	if n := count(t, pool, `select count(*) from pg_tables where schemaname = 'public' and tablename in ('b1', 'b2', 'c')`); n != 0 {
		t.Fatalf("%d tables of the failed file or later ones exist", n)
	}
	if n := count(t, pool, `select count(*) from platform.schema_migrations where name = '0001_good.sql'`); n != 1 {
		t.Fatal("the file before the failure must stay recorded")
	}
	if n := count(t, pool, `select count(*) from platform.schema_migrations`); n != 1 {
		t.Fatalf("recorded %d names", n)
	}
}

func TestRunNeedsNoMigrationFilesToBeUnderSomeDirectory(t *testing.T) {
	pool := pgtest.Start(t)
	applied, err := migrate.Run(context.Background(), pool, fstest.MapFS{})
	if err != nil || len(applied) != 0 {
		t.Fatalf("%v %v", applied, err)
	}
}

func TestConcurrentRunsApplyEachFileExactlyOnce(t *testing.T) {
	address := pgtest.StartURL(t)
	// Slow files, so that the second run really waits on the first one's lock.
	files := fstest.MapFS{}
	for _, name := range []string{"0001_a.sql", "0002_b.sql", "0003_c.sql"} {
		files[name] = &fstest.MapFile{Data: []byte("select pg_sleep(0.3); create table public.t_" + name[:4] + " (x int);")}
	}

	var wg sync.WaitGroup
	results := make([][]string, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool, err := db.Open(context.Background(), address, nil)
			if err != nil {
				errs[i] = err
				return
			}
			defer pool.Close()
			results[i], errs[i] = migrate.Run(context.Background(), pool, files)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	all := slices.Concat(results[0], results[1])
	slices.Sort(all)
	if !slices.Equal(all, []string{"0001_a.sql", "0002_b.sql", "0003_c.sql"}) {
		t.Fatalf("applied %v and %v: every file must be applied exactly once between them", results[0], results[1])
	}
}
