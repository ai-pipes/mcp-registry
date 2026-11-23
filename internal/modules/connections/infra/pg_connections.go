package infra

import (
	"context"
	"errors"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// commitTimeout bounds what runs after the callback of WithLock returned a write: the upsert and
// the commit. They run on a context that outlives the caller's cancellation (see WithLock).
const commitTimeout = 5 * time.Second

// rollbackTimeout bounds a rollback, which also runs detached.
const rollbackTimeout = 5 * time.Second

// pgConnections is app.ConnectionStore on connections.connections (PgConnectionStore of the
// TypeScript server). Every statement is the reference's, column for column.
type pgConnections struct{ pool *pgxpool.Pool }

var _ app.ConnectionStore = (*pgConnections)(nil)

const connectionColumns = `user_id::text, server, kind, access, refresh, expires_at, status, issued_for, revoke_url, updated_at`

// ms is a timestamp as the reference stores and reads one: a JavaScript Date has milliseconds, so
// anything finer is cut (the domain compares milliseconds), in UTC and without a monotonic reading.
func ms(t time.Time) time.Time { return t.Round(0).UTC().Truncate(time.Millisecond) }

func msPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := ms(*t)
	return &v
}

func scanConnection(row pgx.Row) (*domain.Connection, error) {
	var (
		s               domain.Snapshot
		kind, status    string
		access, refresh []byte
		expires         *time.Time
		revokeURL       *string
		updated         time.Time
	)
	if err := row.Scan(&s.UserID, &s.Server, &kind, &access, &refresh, &expires, &status, &s.IssuedFor, &revokeURL, &updated); err != nil {
		return nil, err
	}
	s.Kind, s.Status = domain.Kind(kind), domain.Status(status)
	s.Access = domain.Sealed(access)
	if len(refresh) > 0 {
		s.Refresh = domain.Sealed(refresh)
	}
	s.ExpiresAt, s.RevokeURL, s.UpdatedAt = msPtr(expires), revokeURL, ms(updated)
	return domain.Restore(s), nil
}

const upsertSQL = `insert into connections.connections
    (user_id, server, kind, access, refresh, expires_at, status, issued_for, revoke_url, updated_at)
  values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
  on conflict (user_id, server) do update set
    kind = excluded.kind, access = excluded.access, refresh = excluded.refresh,
    expires_at = excluded.expires_at, status = excluded.status,
    issued_for = excluded.issued_for, revoke_url = excluded.revoke_url,
    updated_at = excluded.updated_at`

func upsert(ctx context.Context, tx pgx.Tx, c *domain.Connection) error {
	s := c.Snapshot()
	var refresh any // NULL, never an empty bytea
	if len(s.Refresh) > 0 {
		refresh = []byte(s.Refresh)
	}
	var expires any
	if e := msPtr(s.ExpiresAt); e != nil {
		expires = *e
	}
	var revoke any
	if s.RevokeURL != nil {
		revoke = *s.RevokeURL
	}
	_, err := tx.Exec(ctx, upsertSQL, s.UserID, s.Server, string(s.Kind), []byte(s.Access), refresh, expires,
		string(s.Status), s.IssuedFor, revoke, ms(s.UpdatedAt))
	return err
}

func one[T any](v T, err error) (*T, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *pgConnections) Get(ctx context.Context, userID, server string) (*domain.Connection, error) {
	c, err := one(scanConnection(s.pool.QueryRow(ctx, `select `+connectionColumns+` from connections.connections
      where user_id = $1 and server = $2`, userID, server)))
	if err != nil || c == nil {
		return nil, err
	}
	return *c, nil
}

func (s *pgConnections) ListForUser(ctx context.Context, userID string) ([]*domain.Connection, error) {
	rows, err := s.pool.Query(ctx, `select `+connectionColumns+` from connections.connections
      where user_id = $1 order by server`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*domain.Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

var errWrongKey = errors.New("the connection to write is not the locked one")

// WithLock is `select ... for update` in one transaction on one pooled connection: a second
// process asking for the same row waits until the first commits and then sees its result. A missing
// row locks nothing; inserts of the same key still serialise on the primary key, and the later one
// updates (the statement is an upsert, as in the reference).
//
// fn gets the row as read under the lock and nothing else, so it cannot reach for a second pooled
// connection while this one is held. Acquiring the connection and the select respect ctx. Once fn
// has returned, the write and the commit run on a context detached from ctx's cancellation with
// their own timeout: fn's provider call may have rotated a one-time refresh token, and a caller
// that gave up in the meantime must not roll that token's storage back. An error from fn, a write
// for another key, or a panic rolls back (also detached).
func (s *pgConnections) WithLock(ctx context.Context, userID, server string, fn app.LockedFunc) (err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done { // an error return or a panic unwinding through fn
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
			defer cancel()
			_ = tx.Rollback(rctx)
		}
	}()
	cur, err := one(scanConnection(tx.QueryRow(ctx, `select `+connectionColumns+` from connections.connections
      where user_id = $1 and server = $2 for update`, userID, server)))
	if err != nil {
		return err
	}
	var current *domain.Connection
	if cur != nil {
		current = *cur
	}
	write, err := fn(ctx, current)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	if write != nil {
		if write.UserID() != userID || write.Server() != server {
			return errWrongKey
		}
		if err := upsert(cctx, tx, write); err != nil {
			return err
		}
	}
	done = true
	if err := tx.Commit(cctx); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rctx)
		return err
	}
	return nil
}

func (s *pgConnections) Delete(ctx context.Context, userID, server string) (*domain.Connection, error) {
	c, err := one(scanConnection(s.pool.QueryRow(ctx, `delete from connections.connections
      where user_id = $1 and server = $2 returning `+connectionColumns, userID, server)))
	if err != nil || c == nil {
		return nil, err
	}
	return *c, nil
}

func (s *pgConnections) ListKeys(ctx context.Context) ([]app.ConnectionKey, error) {
	rows, err := s.pool.Query(ctx, `select user_id::text, server from connections.connections order by user_id, server`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []app.ConnectionKey{}
	for rows.Next() {
		var k app.ConnectionKey
		if err := rows.Scan(&k.UserID, &k.Server); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
