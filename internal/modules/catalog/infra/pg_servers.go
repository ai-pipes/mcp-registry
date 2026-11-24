package infra

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const uniqueViolation = "23505"

// pgServers is app.ServerRepository on catalog.servers.
type pgServers struct {
	pool *pgxpool.Pool
	// afterEvents, when set, runs inside the transaction right after the events were appended; a
	// test uses it to fail or to look at the database at that moment. nil in production.
	afterEvents func() error
}

var _ app.ServerRepository = (*pgServers)(nil)

func domainErr(code, format string, args ...any) error {
	return kernel.NewDomainError(code, fmt.Sprintf(format, args...))
}

const serverColumns = `slug, name, description, url, auth, secret, version, access, timeout_ms,
  daily_limit, status, last_sync_at, last_sync_error, sync_user_id::text, created_at, updated_at`

func scanServer(row pgx.Row, extra ...any) (*domain.Server, error) {
	var st domain.ServerState
	var auth, access, sealed []byte
	var status string
	var created, updated time.Time
	var last *time.Time
	dest := append([]any{
		&st.Slug, &st.Name, &st.Description, &st.URL, &auth, &sealed, &st.Version, &access, &st.TimeoutMs,
		&st.DailyLimit, &status, &last, &st.LastSyncError, &st.SyncUserID, &created, &updated,
	}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	var err error
	if st.Auth, err = decodeAuth(auth); err != nil {
		return nil, fmt.Errorf("catalog server %q: %w", st.Slug, err)
	}
	if st.Access, err = decodeAccess(access); err != nil {
		return nil, fmt.Errorf("catalog server %q: %w", st.Slug, err)
	}
	st.SealedSecret = domain.SealedSecretOf(sealed)
	st.Status = domain.ServerStatus(status)
	st.LastSyncAt = utcPtr(last)
	st.CreatedAt, st.UpdatedAt = created.UTC(), updated.UTC()
	return domain.RehydrateServer(st), nil
}

func (r *pgServers) FindBySlug(ctx context.Context, slug string) (*domain.Server, error) {
	s, err := one(scanServer(r.pool.QueryRow(ctx, `select `+serverColumns+` from catalog.servers where slug = $1`, slug)))
	if err != nil || s == nil {
		return nil, err
	}
	return *s, nil
}

func (r *pgServers) List(ctx context.Context) ([]app.ServerListing, error) {
	rows, err := r.pool.Query(ctx, `select s.slug, s.name, s.description, s.url, s.auth, s.secret, s.version, s.access, s.timeout_ms,
        s.daily_limit, s.status, s.last_sync_at, s.last_sync_error, s.sync_user_id::text, s.created_at, s.updated_at,
        count(t.name)::int as tool_count
      from catalog.servers s
      left join catalog.tools t on t.server = s.slug and not t.hidden and t.removed_at is null
      group by s.slug
      order by s.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []app.ServerListing{}
	for rows.Next() {
		var count int
		s, err := scanServer(rows, &count)
		if err != nil {
			return nil, err
		}
		out = append(out, app.ServerListing{Server: s, ToolCount: count})
	}
	return out, rows.Err()
}

// Create: the insert (version defaults to 0) and the events in one transaction.
func (r *pgServers) Create(ctx context.Context, s *domain.Server, events []kernel.Event) error {
	auth, err := encodeAuth(s.Auth())
	if err != nil {
		return err
	}
	access, err := encodeAccess(s.Access())
	if err != nil {
		return err
	}
	err = db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into catalog.servers (slug, name, description, url, auth, secret, access,
            timeout_ms, daily_limit, status, last_sync_at, last_sync_error, sync_user_id, created_at, updated_at)
          values ($1, $2, $3, $4, $5::text::jsonb, $6, $7::text::jsonb, $8, $9, $10, $11, $12, $13::uuid, $14, $15)`,
			s.Slug(), s.Name(), s.Description(), s.URL(), string(auth), s.SealedSecret(), string(access),
			s.TimeoutMs(), s.DailyLimit(), string(s.Status()), usPtr(s.LastSyncAt()), s.LastSyncError(), s.SyncUserID(),
			us(s.CreatedAt()), us(s.UpdatedAt())); err != nil {
			return err
		}
		return r.appendEvents(ctx, tx, events)
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == uniqueViolation {
		return domainErr(domain.CodeServerExists, "server %q already exists", s.Slug())
	}
	return err
}

func (r *pgServers) appendEvents(ctx context.Context, tx pgx.Tx, events []kernel.Event) error {
	if err := checkEventTypes(events); err != nil {
		return err
	}
	if err := outbox.Append(ctx, tx, events); err != nil {
		return err
	}
	if r.afterEvents != nil {
		return r.afterEvents()
	}
	return nil
}

// Save is optimistic concurrency: the row is written only if it is still at the version the
// aggregate was read at (and the version is bumped); otherwise server_conflict and nothing is
// written, so an edit based on a stale read never undoes or mixes with another admin's change.
// The UPDATE waits on a row lock held by another transaction (no NOWAIT, no SKIP LOCKED), then
// Postgres re-checks the version on the row as that transaction left it. Synchronisation state
// is not the aggregate's to change here. The secret is written only when the aggregate changed
// it (SecretChanged): a key-rotation re-seal (compare-and-set, no version bump) since the read
// is kept.
func (r *pgServers) Save(ctx context.Context, s *domain.Server, events []kernel.Event) error {
	auth, err := encodeAuth(s.Auth())
	if err != nil {
		return err
	}
	access, err := encodeAccess(s.Access())
	if err != nil {
		return err
	}
	return db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update catalog.servers set
          name = $2, description = $3, url = $4, auth = $5::text::jsonb, access = $6::text::jsonb, timeout_ms = $7,
          daily_limit = $8, status = $9, updated_at = $10, version = version + 1
        where slug = $1 and version = $11`,
			s.Slug(), s.Name(), s.Description(), s.URL(), string(auth), string(access), s.TimeoutMs(),
			s.DailyLimit(), string(s.Status()), us(s.UpdatedAt()), s.Version())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			var exists int
			err := tx.QueryRow(ctx, `select 1 from catalog.servers where slug = $1`, s.Slug()).Scan(&exists)
			if errors.Is(err, pgx.ErrNoRows) {
				return domainErr(domain.CodeNotFound, "server %q not found", s.Slug())
			}
			if err != nil {
				return err
			}
			return domainErr(domain.CodeServerConflict, "server %q was changed meanwhile; reload it and try again", s.Slug())
		}
		if s.SecretChanged() {
			if _, err := tx.Exec(ctx, `update catalog.servers set secret = $2 where slug = $1`, s.Slug(), s.SealedSecret()); err != nil {
				return err
			}
		}
		return r.appendEvents(ctx, tx, events)
	})
}

// SaveSyncState is one UPDATE; the guard is in its own WHERE, so Postgres re-checks it on the
// locked row and overlapping runs cannot slip past each other. Both sides of the comparison are
// stored at microsecond precision.
func (r *pgServers) SaveSyncState(ctx context.Context, slug string, rec domain.SyncRecord) error {
	started := us(rec.StartedAt)
	tag, err := r.pool.Exec(ctx, `update catalog.servers set
        last_sync_at = $2, last_sync_error = $3, sync_user_id = coalesce($4::uuid, sync_user_id)
      where slug = $1 and (last_sync_at is null or last_sync_at <= $2)`, slug, started, rec.Error, rec.SyncedBy)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists int
	err = r.pool.QueryRow(ctx, `select 1 from catalog.servers where slug = $1`, slug).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return domainErr(domain.CodeNotFound, "server %q not found", slug)
	}
	return err
}

func (r *pgServers) ReplaceSealedSecret(ctx context.Context, slug string, old, next []byte) (bool, error) {
	tag, err := r.pool.Exec(ctx, `update catalog.servers set secret = $3 where slug = $1 and secret = $2`, slug, old, next)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
