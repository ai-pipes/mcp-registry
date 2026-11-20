// Package outbox is the transactional outbox (platform.outbox): events are appended in the
// transaction of the change and delivered at least once by a polling worker.
package outbox

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxAttempts is the number of failed deliveries after which an event is left alone (see the recovery sweep).
const MaxAttempts = 10

// leaseSeconds: a claimed row stays invisible to other claimers for this long; a failure
// replaces the lease with backoff.
const leaseSeconds = 60

type row struct {
	id       int64
	typ      string
	payload  map[string]any
	attempts int
}

// Append writes the events in tx, so they commit or roll back with the change. No events, no query.
func Append(ctx context.Context, tx pgx.Tx, events []kernel.Event) error {
	if len(events) == 0 {
		return nil
	}
	type item struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	items := make([]item, len(events))
	for i, e := range events {
		p := e.Payload
		if p == nil {
			p = map[string]any{}
		}
		items[i] = item{Type: e.Type, Payload: p}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `insert into platform.outbox (type, payload)
		select type, payload from jsonb_to_recordset($1::text::jsonb) as x(type text, payload jsonb)`, string(raw))
	return err
}

// claim takes up to limit due rows in one transaction (for update skip locked), counts the
// attempt and leases them, and returns them in id order.
func claim(ctx context.Context, pool *pgxpool.Pool, limit int) ([]row, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	picked, err := tx.Query(ctx, `select id from platform.outbox
		where processed_at is null and next_attempt_at <= now() and attempts < $1
		order by id
		limit $2
		for update skip locked`, MaxAttempts, limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(picked, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	claimed, err := tx.Query(ctx, `update platform.outbox
		set attempts = attempts + 1,
		    next_attempt_at = now() + make_interval(secs => $2)
		where id = any($1::bigint[])
		returning id, type, payload, attempts`, ids, leaseSeconds)
	if err != nil {
		return nil, err
	}
	rows, err := pgx.CollectRows(claimed, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.typ, &x.payload, &x.attempts)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b row) int {
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		}
		return 0
	})
	return rows, nil
}

func markProcessed(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx, `update platform.outbox set processed_at = now(), last_error = null
		where id = $1`, id)
	return err
}

// markFailed schedules a retry with backoff. last_error gets only a content-free label: the
// message can quote values.
func markFailed(ctx context.Context, pool *pgxpool.Pool, id int64, err error) error {
	_, e := pool.Exec(ctx, `update platform.outbox
		set last_error = $2,
		    next_attempt_at = now() + make_interval(secs => least(power(2, attempts), 300))
		where id = $1`, id, kernel.ErrorLabel(err))
	return e
}

// release gives back rows that were claimed but not delivered because the worker stopped: the
// attempt is not counted and they are due at once.
func release(ctx context.Context, pool *pgxpool.Pool, ids []int64) error {
	_, err := pool.Exec(ctx, `update platform.outbox
		set attempts = attempts - 1, next_attempt_at = now()
		where id = any($1::bigint[]) and processed_at is null`, ids)
	return err
}
