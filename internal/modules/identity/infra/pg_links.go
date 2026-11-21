package infra

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgLoginLinkRepository is app.LoginLinkRepository on identity.login_links.
type pgLoginLinkRepository struct{ pool *pgxpool.Pool }

var _ app.LoginLinkRepository = (*pgLoginLinkRepository)(nil)

func (r *pgLoginLinkRepository) Insert(ctx context.Context, l app.LoginLinkRecord) error {
	_, err := r.pool.Exec(ctx, `insert into identity.login_links (code_hash, user_id, created_at, expires_at)
      values ($1, $2, $3, $4)`, l.CodeHash, l.UserID, us(l.CreatedAt), us(l.ExpiresAt))
	return err
}

// Consume is a single delete ... returning, so concurrent callers give the row to exactly one.
func (r *pgLoginLinkRepository) Consume(ctx context.Context, codeHash string) (*app.LoginLinkRecord, error) {
	var l app.LoginLinkRecord
	var created, exp time.Time
	err := r.pool.QueryRow(ctx, `delete from identity.login_links where code_hash = $1
      returning code_hash, user_id::text, created_at, expires_at`, codeHash).Scan(&l.CodeHash, &l.UserID, &created, &exp)
	l.CreatedAt, l.ExpiresAt = created.UTC(), exp.UTC()
	return one(l, err)
}

func (r *pgLoginLinkRepository) DeleteByUser(ctx context.Context, userID string) error {
	if !isUUID(userID) {
		return nil
	}
	_, err := r.pool.Exec(ctx, `delete from identity.login_links where user_id = $1`, userID)
	return err
}

func (r *pgLoginLinkRepository) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	tag, err := r.pool.Exec(ctx, `delete from identity.login_links where expires_at <= $1`, us(now))
	return int(tag.RowsAffected()), err
}
