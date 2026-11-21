package infra

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgSessionRepository is app.SessionRepository on identity.sessions.
type pgSessionRepository struct{ pool *pgxpool.Pool }

var _ app.SessionRepository = (*pgSessionRepository)(nil)

func (r *pgSessionRepository) Insert(ctx context.Context, s app.SessionRecord) error {
	_, err := r.pool.Exec(ctx, `insert into identity.sessions (id_hash, user_id, created_at, last_seen_at, expires_at)
      values ($1, $2, $3, $4, $5)`, s.IDHash, s.UserID, us(s.CreatedAt), us(s.LastSeenAt), us(s.ExpiresAt))
	return err
}

func (r *pgSessionRepository) FindByHash(ctx context.Context, idHash string) (*app.SessionRecord, error) {
	var s app.SessionRecord
	var created, seen, exp time.Time
	err := r.pool.QueryRow(ctx, `select id_hash, user_id::text, created_at, last_seen_at, expires_at
      from identity.sessions where id_hash = $1`, idHash).Scan(&s.IDHash, &s.UserID, &created, &seen, &exp)
	s.CreatedAt, s.LastSeenAt, s.ExpiresAt = created.UTC(), seen.UTC(), exp.UTC()
	return one(s, err)
}

func (r *pgSessionRepository) Touch(ctx context.Context, idHash string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `update identity.sessions set last_seen_at = $1 where id_hash = $2`, us(at), idHash)
	return err
}

func (r *pgSessionRepository) Delete(ctx context.Context, idHash string) error {
	_, err := r.pool.Exec(ctx, `delete from identity.sessions where id_hash = $1`, idHash)
	return err
}

func (r *pgSessionRepository) DeleteByUser(ctx context.Context, userID string) error {
	if !isUUID(userID) {
		return nil
	}
	_, err := r.pool.Exec(ctx, `delete from identity.sessions where user_id = $1`, userID)
	return err
}

func (r *pgSessionRepository) DeleteDead(ctx context.Context, c domain.SessionCutoffs) (int, error) {
	tag, err := r.pool.Exec(ctx, `delete from identity.sessions where expires_at <= $1 or last_seen_at < $2`,
		us(c.ExpiresAtOrBefore), us(c.LastSeenBefore))
	return int(tag.RowsAffected()), err
}
