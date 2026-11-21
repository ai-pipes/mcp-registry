package infra

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgTokenRepository is app.TokenRepository on identity.tokens.
type pgTokenRepository struct{ pool *pgxpool.Pool }

var _ app.TokenRepository = (*pgTokenRepository)(nil)

const apiRowColumns = `id::text, user_id::text, name, prefix, hash, created_at, last_used_at, revoked_at`

func scanToken(row pgx.Row) (app.TokenRecord, error) {
	var t app.TokenRecord
	var created time.Time
	if err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.Hash, &created, &t.LastUsedAt, &t.RevokedAt); err != nil {
		return app.TokenRecord{}, err
	}
	t.CreatedAt = created.UTC()
	t.LastUsedAt = utcPtr(t.LastUsedAt)
	t.RevokedAt = utcPtr(t.RevokedAt)
	return t, nil
}

func (r *pgTokenRepository) Insert(ctx context.Context, t app.NewToken) (app.TokenRecord, error) {
	return scanToken(r.pool.QueryRow(ctx, `
      insert into identity.tokens (user_id, name, prefix, hash, created_at)
      values ($1, $2, $3, $4, $5) returning `+apiRowColumns,
		t.UserID, t.Name, t.Prefix, t.Hash, us(t.CreatedAt)))
}

func (r *pgTokenRepository) FindByHash(ctx context.Context, hash string) (*app.TokenRecord, error) {
	return one(scanToken(r.pool.QueryRow(ctx, `select `+apiRowColumns+` from identity.tokens where hash = $1`, hash)))
}

func (r *pgTokenRepository) FindByID(ctx context.Context, id string) (*app.TokenRecord, error) {
	if !isUUID(id) {
		return nil, nil
	}
	return one(scanToken(r.pool.QueryRow(ctx, `select `+apiRowColumns+` from identity.tokens where id = $1`, id)))
}

func (r *pgTokenRepository) ListByUser(ctx context.Context, userID string) ([]app.TokenRecord, error) {
	out := []app.TokenRecord{}
	rows, err := r.pool.Query(ctx, `select `+apiRowColumns+` from identity.tokens where user_id = $1 order by created_at desc`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *pgTokenRepository) Revoke(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `update identity.tokens set revoked_at = $1 where id = $2 and revoked_at is null`, us(at), id)
	return err
}

func (r *pgTokenRepository) RevokeAllByUser(ctx context.Context, userID string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `update identity.tokens set revoked_at = $1 where user_id = $2 and revoked_at is null`, us(at), userID)
	return err
}

func (r *pgTokenRepository) Touch(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `update identity.tokens set last_used_at = $1 where id = $2`, us(at), id)
	return err
}
