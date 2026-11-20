// Package migrate applies the versioned SQL files embedded from apps/server/migrations, the same
// files and the same bookkeeping (platform.schema_migrations) as the TypeScript server.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockKey is the advisory lock key the TypeScript server uses too, so two servers of different
// versions on one database still take turns.
const lockKey = 727002

var migrationFile = regexp.MustCompile(`^\d{4}_.+\.sql$`)

// NewerDatabaseError means the database holds a migration this build does not ship: it was
// migrated by a newer version.
type NewerDatabaseError struct{ Migration string }

func (e *NewerDatabaseError) Error() string {
	return fmt.Sprintf("the database was migrated by a newer version of mcp-registry (unknown migration: %s); start that version or restore the backup taken before the upgrade", e.Migration)
}

// MigrationError is a failed migration step. Its text names the file and the content-free label
// of the cause (type and SQLSTATE), never the driver's message, which can quote data; Unwrap
// reaches the driver error.
type MigrationError struct {
	File string
	Err  error
}

func (e *MigrationError) Error() string {
	return "migration " + e.File + " failed (" + kernel.ErrorLabel(e.Err) + ")"
}

func (e *MigrationError) Unwrap() error { return e.Err }

// Run applies every migration file of files (read at its root) that platform.schema_migrations
// does not list, in name order, and returns the names it applied. A session advisory lock on one
// dedicated connection spans the whole run; each file runs in its own transaction together with
// its bookkeeping row, so a failed file leaves nothing behind. When the run fails, applied lists
// the files that were committed before it.
func Run(ctx context.Context, pool *pgxpool.Pool, files fs.FS) (applied []string, err error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && migrationFile.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, &MigrationError{File: "(connection)", Err: err}
	}
	// A connection whose lock could not be released must not go back to the pool still holding it.
	release := func() { conn.Release() }
	defer func() { release() }()

	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, int64(lockKey)); err != nil {
		return nil, &MigrationError{File: "(bookkeeping)", Err: err}
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, uerr := conn.Exec(uctx, `select pg_advisory_unlock($1)`, int64(lockKey)); uerr != nil {
			release = func() { _ = conn.Hijack().Close(context.WithoutCancel(ctx)) }
		}
	}()

	if _, err := conn.Exec(ctx, `create schema if not exists platform`); err != nil {
		return nil, &MigrationError{File: "(bookkeeping)", Err: err}
	}
	if _, err := conn.Exec(ctx, `create table if not exists platform.schema_migrations (
		name text primary key,
		applied_at timestamptz not null default now()
	)`); err != nil {
		return nil, &MigrationError{File: "(bookkeeping)", Err: err}
	}

	rows, err := conn.Query(ctx, `select name from platform.schema_migrations`)
	if err != nil {
		return nil, &MigrationError{File: "(bookkeeping)", Err: err}
	}
	done, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, &MigrationError{File: "(bookkeeping)", Err: err}
	}
	// Sorted here, not by the database: its collation is not byte order.
	slices.Sort(done)
	for _, name := range done {
		if !slices.Contains(names, name) {
			return nil, &NewerDatabaseError{Migration: name}
		}
	}

	for _, name := range names {
		if slices.Contains(done, name) {
			continue
		}
		text, err := fs.ReadFile(files, name)
		if err != nil {
			return applied, fmt.Errorf("read migration %s: %w", name, err)
		}
		if err := apply(ctx, conn, name, string(text)); err != nil {
			return applied, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}

// apply runs one file as a single multi-statement query (pgx uses the simple protocol when a
// query has no arguments, like conn.unsafe in the Node client) plus its bookkeeping row.
func apply(ctx context.Context, conn *pgxpool.Conn, name, text string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return &MigrationError{File: name, Err: err}
	}
	if _, err := tx.Exec(ctx, text); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return &MigrationError{File: name, Err: err}
	}
	if _, err := tx.Exec(ctx, `insert into platform.schema_migrations (name) values ($1)`, name); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return &MigrationError{File: name, Err: err}
	}
	if err := tx.Commit(ctx); err != nil {
		return &MigrationError{File: name, Err: err}
	}
	return nil
}
