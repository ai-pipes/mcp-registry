package infra

import (
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Stores is every Postgres port implementation of the gateway module.
type Stores struct {
	CallLog app.CallLog
	Quota   app.Quota
}

// NewPg builds the stores on one pool.
func NewPg(pool *pgxpool.Pool) Stores {
	return Stores{CallLog: &pgCallLog{pool: pool}, Quota: &pgQuota{pool: pool}}
}
