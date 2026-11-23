package infra

import (
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Stores is every Postgres port implementation of the connections module (createPgConnections of
// the TypeScript server).
type Stores struct {
	Connections app.ConnectionStore
	Pending     app.PendingConnects
}

// NewPg builds the stores on one pool.
func NewPg(pool *pgxpool.Pool) Stores {
	return Stores{Connections: &pgConnections{pool: pool}, Pending: &pgPending{pool: pool}}
}
