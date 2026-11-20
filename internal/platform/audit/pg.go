// Package audit writes the audit log (platform.audit_log).
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pgAuditLog struct{ pool *pgxpool.Pool }

// NewPgAuditLog returns the Postgres implementation of kernel.AuditLog. An entry without meta
// is stored with meta = {}.
func NewPgAuditLog(pool *pgxpool.Pool) kernel.AuditLog { return &pgAuditLog{pool: pool} }

func (a *pgAuditLog) Record(ctx context.Context, e kernel.AuditEntry) error {
	meta := e.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("audit: meta is not JSON: %w", err)
	}
	_, err = a.pool.Exec(ctx, `insert into platform.audit_log (actor_id, action, target, meta)
		values ($1::text::uuid, $2, $3, $4::text::jsonb)`, e.ActorID, e.Action, e.Target, string(raw))
	return err
}
