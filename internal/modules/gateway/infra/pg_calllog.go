package infra

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgCallLog is app.CallLog on gateway.calls (PgCallLog of the TypeScript server). No column holds
// arguments or results.
type pgCallLog struct{ pool *pgxpool.Pool }

var _ app.CallLog = (*pgCallLog)(nil)

// Record inserts one row. started_at has millisecond precision (the reference stores a JavaScript
// Date; the REST cursor round-trips through a three-decimal string, so a finer stored value would
// skip or repeat rows), duration_ms is an integer of at least zero, a session has no token (NULL).
func (l *pgCallLog) Record(ctx context.Context, e app.CallLogEntry) error {
	var token any
	if e.TokenID != "" {
		token = e.TokenID
	}
	_, err := l.pool.Exec(ctx, `insert into gateway.calls
      (user_id, token_id, server, tool, started_at, duration_ms, outcome)
    values ($1, $2, $3, $4, $5, $6, $7)`,
		e.UserID, token, e.Server, e.Tool, ms(e.StartedAt), durationMs(e.DurationMs), string(e.Outcome))
	return err
}

// durationMs fits the integer column: never negative, never beyond int32.
func durationMs(d int64) int32 {
	return int32(min(max(d, 0), math.MaxInt32)) //nolint:gosec // G115: clamped to the int32 range on the line above
}

// List is newest first: started_at, then the NUMERIC id (the id leaves as text, but the order is
// the column's, qualified so it is not the text alias of the same name). Before alone is strictly
// older; with BeforeID the pair is the exclusive upper cursor, and the redundant started_at <=
// before is what the index can range on.
func (l *pgCallLog) List(ctx context.Context, f app.CallLogFilter) ([]app.CallRecord, error) {
	var (
		sb   strings.Builder
		args []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	sb.WriteString(`select id::text as id, user_id::text, token_id::text, server, tool,
      started_at, duration_ms, outcome
    from gateway.calls
    where true`)
	if f.UserID != nil {
		sb.WriteString(" and user_id = " + arg(*f.UserID))
	}
	if f.Server != nil {
		sb.WriteString(" and server = " + arg(*f.Server))
	}
	if f.Outcome != nil {
		sb.WriteString(" and outcome = " + arg(string(*f.Outcome)))
	}
	if f.Before != nil {
		before := ms(*f.Before)
		if f.BeforeID == nil {
			sb.WriteString(" and started_at < " + arg(before))
		} else {
			p := arg(before)
			sb.WriteString(" and started_at <= " + p + " and (started_at, gateway.calls.id) < (" + p + ", " + arg(*f.BeforeID) + "::bigint)")
		}
	}
	sb.WriteString(" order by started_at desc, gateway.calls.id desc limit " + arg(f.Limit))

	rows, err := l.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []app.CallRecord{}
	for rows.Next() {
		var (
			r       app.CallRecord
			token   *string
			started time.Time
			outcome string
		)
		if err := rows.Scan(&r.ID, &r.UserID, &token, &r.Server, &r.Tool, &started, &r.DurationMs, &outcome); err != nil {
			return nil, err
		}
		o, ok := domain.ParseOutcome(outcome)
		if !ok {
			return nil, errUnknownOutcome
		}
		r.Outcome = o
		if token != nil {
			r.TokenID = *token
		}
		r.StartedAt = ms(started)
		out = append(out, r)
	}
	return out, rows.Err()
}

type fixedError string

func (e fixedError) Error() string { return string(e) }

// errUnknownOutcome cannot happen with the table's CHECK; it names no value.
const errUnknownOutcome = fixedError("unknown call outcome in gateway.calls")

// DeleteOlderThan removes the rows that started before t.
func (l *pgCallLog) DeleteOlderThan(ctx context.Context, t time.Time) (int64, error) {
	tag, err := l.pool.Exec(ctx, `delete from gateway.calls where started_at < $1`, ms(t))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
