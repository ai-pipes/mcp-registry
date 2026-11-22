package infra

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgTools is app.ToolRepository on catalog.tools.
type pgTools struct {
	pool *pgxpool.Pool
	// afterEvents: see pgServers.
	afterEvents func() error
}

var _ app.ToolRepository = (*pgTools)(nil)

const toolColumns = `server, name, title, description, input_schema, output_schema, annotations, hidden, removed_at, synced_at`

func scanTool(row pgx.Row) (*domain.Tool, error) {
	var st domain.ToolState
	var input, output, ann []byte
	var removed *time.Time
	var synced time.Time
	if err := row.Scan(&st.Server, &st.Name, &st.Title, &st.Description, &input, &output, &ann, &st.Hidden, &removed, &synced); err != nil {
		return nil, err
	}
	st.InputSchema = json.RawMessage(input)
	if o := storedJSON(output); o != nil {
		st.OutputSchema = json.RawMessage(o)
	}
	st.Annotations = decodeAnnotations(ann)
	st.RemovedAt = utcPtr(removed)
	st.SyncedAt = synced.UTC()
	return domain.RehydrateTool(st), nil
}

func (r *pgTools) Find(ctx context.Context, server, name string) (*domain.Tool, error) {
	t, err := one(scanTool(r.pool.QueryRow(ctx, `select `+toolColumns+` from catalog.tools where server = $1 and name = $2`, server, name)))
	if err != nil || t == nil {
		return nil, err
	}
	return *t, nil
}

func (r *pgTools) ListByServer(ctx context.Context, server string) ([]*domain.Tool, error) {
	return listTools(ctx, r.pool, `select `+toolColumns+` from catalog.tools where server = $1 order by name`, server)
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func listTools(ctx context.Context, q queryer, sql string, args ...any) ([]*domain.Tool, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*domain.Tool{}
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *pgTools) appendEvents(ctx context.Context, tx pgx.Tx, events []kernel.Event) error {
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

// Save writes the hidden flag (nothing else is the admin's to change) and the events.
func (r *pgTools) Save(ctx context.Context, t *domain.Tool, events []kernel.Event) error {
	return db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update catalog.tools set hidden = $3 where server = $1 and name = $2`, t.Server(), t.Name(), t.Hidden())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domainErr(domain.CodeNotFound, "tool %q not found", t.Ref())
		}
		return r.appendEvents(ctx, tx, events)
	})
}

// upsertRow is one element of the jsonb batch of ApplySnapshot, key for key as the TypeScript
// repository builds it. A nil RawMessage marshals as null, which jsonb_to_recordset turns into
// SQL NULL for output_schema.
type upsertRow struct {
	Server       string          `json:"server"`
	Name         string          `json:"name"`
	Title        *string         `json:"title"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Annotations  json.RawMessage `json:"annotations"`
	Hidden       bool            `json:"hidden"`
	RemovedAt    *time.Time      `json:"removed_at"`
	SyncedAt     time.Time       `json:"synced_at"`
}

// ApplySnapshot, in ONE transaction: locks the server row (this serialises concurrent
// synchronisations of one server and fails early for an unknown one), reads its tools,
// reconciles them with the snapshot, upserts the result without touching hidden, and appends the
// events. The stored tools are read in name order (the TypeScript query has no ORDER BY, so its
// order is the heap's; the domain's "newly removed in stored order" is then deterministic here).
func (r *pgTools) ApplySnapshot(ctx context.Context, server string, snapshot []domain.ToolSnapshot, now time.Time) (app.SnapshotResult, error) {
	var res app.SnapshotResult
	err := db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var locked string
		err := tx.QueryRow(ctx, `select slug from catalog.servers where slug = $1 for update`, server).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return domainErr(domain.CodeNotFound, "server %q not found", server)
		}
		if err != nil {
			return err
		}
		existing, err := listTools(ctx, tx, `select `+toolColumns+` from catalog.tools where server = $1 order by name`, server)
		if err != nil {
			return err
		}
		rec := domain.ReconcileTools(server, existing, snapshot, now)
		if len(rec.Upserts) > 0 {
			batch := make([]upsertRow, len(rec.Upserts))
			for i, t := range rec.Upserts {
				ann, err := json.Marshal(t.Annotations())
				if err != nil {
					return err
				}
				removed := usPtr(t.RemovedAt())
				batch[i] = upsertRow{
					Server: t.Server(), Name: t.Name(), Title: t.Title(), Description: t.Description(),
					InputSchema: t.InputSchema(), OutputSchema: t.OutputSchema(), Annotations: ann, Hidden: t.Hidden(),
					RemovedAt: removed, SyncedAt: us(t.SyncedAt()),
				}
			}
			text, err := json.Marshal(batch)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `insert into catalog.tools (server, name, title, description, input_schema,
                output_schema, annotations, hidden, removed_at, synced_at)
              select server, name, title, description, input_schema, output_schema, annotations,
                hidden, removed_at, synced_at
              from jsonb_to_recordset($1::text::jsonb) as x(
                server text, name text, title text, description text, input_schema jsonb,
                output_schema jsonb, annotations jsonb, hidden boolean, removed_at timestamptz,
                synced_at timestamptz)
              on conflict (server, name) do update set
                title = excluded.title, description = excluded.description,
                input_schema = excluded.input_schema, output_schema = excluded.output_schema,
                annotations = excluded.annotations, removed_at = excluded.removed_at,
                synced_at = excluded.synced_at`, string(text)); err != nil {
				return err
			}
		}
		if err := r.appendEvents(ctx, tx, rec.Events); err != nil {
			return err
		}
		res = app.SnapshotResult{ChangedRefs: rec.ChangedRefs, AddedRefs: rec.AddedRefs, RemovedRefs: rec.RemovedRefs}
		return nil
	})
	return res, err
}
