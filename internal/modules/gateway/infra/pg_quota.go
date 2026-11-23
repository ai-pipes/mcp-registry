package infra

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgQuota is app.Quota on gateway.quota (PgQuota of the TypeScript server).
type pgQuota struct{ pool *pgxpool.Pool }

var _ app.Quota = (*pgQuota)(nil)

// TryConsume is the whole atomicity story: one statement, no read before the write. The first call
// of a (person, server, day) inserts count = 1 and returns a row whatever the limit; later calls
// conflict and update only while count < limit, and no returned row means refused (nothing counted).
// The row lock of the conflict serialises concurrent callers.
func (q *pgQuota) TryConsume(ctx context.Context, r app.QuotaRequest) (bool, error) {
	rows, err := q.pool.Query(ctx, `insert into gateway.quota (user_id, server, day, count)
    values ($1, $2, $3, 1)
    on conflict (user_id, server, day) do update set count = gateway.quota.count + 1
      where gateway.quota.count < $4
    returning count`, r.UserID, r.Server, r.Day, r.Limit)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	granted := rows.Next()
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	return granted, nil
}

// DeleteDaysBefore removes the counters of days before day (YYYY-MM-DD).
func (q *pgQuota) DeleteDaysBefore(ctx context.Context, day string) (int64, error) {
	tag, err := q.pool.Exec(ctx, `delete from gateway.quota where day < $1`, day)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ms is a timestamp as the reference stores and reads one: a JavaScript Date has milliseconds, so
// anything finer is cut (the log's cursor round-trips through a three-decimal string), in UTC
// and without a monotonic reading.
func ms(t time.Time) time.Time { return t.Round(0).UTC().Truncate(time.Millisecond) }
