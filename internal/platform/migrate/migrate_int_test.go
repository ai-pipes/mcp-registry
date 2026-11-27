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
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/ai-pipes/mcp-registry/internal/platform/migrate"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/ai-pipes/mcp-registry/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
		"0002_bad.sql":  {Data: []byte("create table public.b1 (x int); insert into public.b1 values (1); select 1/0 as marker_value_Q7; create table public.b2 (x int); -- marker_value_Q7")},
		"0001_good.sql": {Data: []byte("create table public.a (x int);")},
		"README.sql":    {Data: []byte("this is not a migration name")},
		"notes.txt":     {Data: []byte("nor is this")},
		"0003_next.sql": {Data: []byte("create table public.c (x int);")},
	}

	applied, err := migrate.Run(context.Background(), pool, files)
	if err == nil || !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("want an error naming the file, got %v", err)
	}
	var me *migrate.MigrationError
	if !errors.As(err, &me) || me.File != "0002_bad.sql" {
		t.Fatalf("want *MigrationError for the file, got %T", err)
	}
	if !strings.Contains(err.Error(), "22012") || strings.Contains(err.Error(), "marker_value_Q7") || strings.Contains(err.Error(), "division") {
		t.Fatalf("message must carry the file and SQLSTATE only: %q", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22012" {
		t.Fatalf("errors.As must reach the driver error, got %v", err)
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
	if n := count(t, pool, `select count(*) from pg_locks where locktype = 'advisory' and objid = 727002`); n != 0 {
		t.Fatalf("%d advisory locks with key 727002 remain after the failed run", n)
	}
	// The lock is free: a following run with a short deadline gets it and applies the rest.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	delete(files, "0002_bad.sql")
	applied, err = migrate.Run(ctx, pool, files)
	if err != nil || !slices.Equal(applied, []string{"0003_next.sql"}) {
		t.Fatalf("follow-up run: %v %v", applied, err)
	}
}

func TestRunReportsTheFirstUnknownNameInByteOrder(t *testing.T) {
	pool := pgtest.Start(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `create schema platform;
		create table platform.schema_migrations (name text primary key, applied_at timestamptz not null default now());
		insert into platform.schema_migrations (name) values ('0011_a.sql'), ('0011_B.sql')`); err != nil {
		t.Fatal(err)
	}
	_, err := migrate.Run(ctx, pool, migrations.FS)
	var newer *migrate.NewerDatabaseError
	if !errors.As(err, &newer) || newer.Migration != "0011_B.sql" {
		t.Fatalf("want 0011_B.sql (byte order), got %v", err)
	}
}

func TestRunAppliesANewMigrationOnTopOfTheKnownOnes(t *testing.T) {
	pool := pgtest.Start(t)
	ctx := context.Background()
	if applied, err := migrate.Run(ctx, pool, migrations.FS); err != nil || len(applied) != 10 {
		t.Fatalf("%v %v", applied, err)
	}
	merged := fstest.MapFS{"0011_extra.sql": {Data: []byte("create table public.extra (x int);")}}
	for _, name := range bundled(t) {
		b, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		merged[name] = &fstest.MapFile{Data: b}
	}
	applied, err := migrate.Run(ctx, pool, merged)
	if err != nil || !slices.Equal(applied, []string{"0011_extra.sql"}) {
		t.Fatalf("%v %v", applied, err)
	}
	if n := count(t, pool, `select count(*) from platform.schema_migrations`); n != 11 {
		t.Fatalf("recorded %d", n)
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
	// The lock spans the whole run, not each file: one run did everything, the other nothing.
	if len(results[0])+len(results[1]) != 3 || (len(results[0]) != 0 && len(results[1]) != 0) {
		t.Fatalf("one run must apply all files and the other none, got %v and %v", results[0], results[1])
	}
	all := slices.Concat(results[0], results[1])
	slices.Sort(all)
	if !slices.Equal(all, []string{"0001_a.sql", "0002_b.sql", "0003_c.sql"}) {
		t.Fatalf("applied %v and %v: every file must be applied exactly once between them", results[0], results[1])
	}
}

// holdLock takes the migration advisory lock on its own connection, as another server would.
func holdLock(t *testing.T, address string) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(727002)`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_, _ = conn.Exec(ctx, `select pg_advisory_unlock(727002)`)
			_ = conn.Close(ctx)
		})
	}
	t.Cleanup(release)
	return release
}

// waitForQuery polls pg_stat_activity until a backend runs a query containing needle.
func waitForQuery(t *testing.T, pool *pgxpool.Pool, needle string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if count(t, pool, `select count(*) from pg_stat_activity where state = 'active' and query like '%`+needle+`%' and pid <> pg_backend_pid()`) > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("no backend ran %q", needle)
		case <-tick.C:
		}
	}
}

func TestRunStopsWaitingForTheLockWhenCancelled(t *testing.T) {
	address := pgtest.StartURL(t)
	release := holdLock(t, address)
	pool, err := db.Open(context.Background(), address, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		applied []string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		a, err := migrate.Run(ctx, pool, migrations.FS)
		done <- result{a, err}
	}()
	waitForQuery(t, pool, "pg_advisory_lock")
	cancel()
	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) || len(r.applied) != 0 {
			t.Fatalf("applied %v, err %v", r.applied, r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run kept waiting for the lock after the context was cancelled")
	}

	// Nothing was applied, and the interrupted waiter does not keep the lock once the holder lets go.
	release()
	again, err := migrate.Run(context.Background(), pool, migrations.FS)
	if err != nil || len(again) != 10 {
		t.Fatalf("run after the holder released: %v %v", again, err)
	}
}

func TestRunAlreadyCancelledAppliesNothing(t *testing.T) {
	pool := pgtest.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	applied, err := migrate.Run(ctx, pool, migrations.FS)
	if !errors.Is(err, context.Canceled) || len(applied) != 0 {
		t.Fatalf("applied %v, err %v", applied, err)
	}
}

func TestRunFinishesWhatItBeganWhenCancelledWhileApplying(t *testing.T) {
	address := pgtest.StartURL(t)
	pool, err := db.Open(context.Background(), address, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	files := fstest.MapFS{
		"0001_slow.sql": {Data: []byte("select pg_sleep(1); create table public.t_slow (x int);")},
		"0002_next.sql": {Data: []byte("create table public.t_next (x int);")},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		applied []string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		a, err := migrate.Run(ctx, pool, files)
		done <- result{a, err}
	}()
	waitForQuery(t, pool, "pg_sleep")
	cancel()
	r := <-done
	if r.err != nil || !slices.Equal(r.applied, []string{"0001_slow.sql", "0002_next.sql"}) {
		t.Fatalf("a run that began must finish: applied %v, err %v", r.applied, r.err)
	}
}
