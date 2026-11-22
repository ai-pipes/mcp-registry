package infra

import (
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repos is every Postgres port implementation of the catalog module (createPgCatalog of the
// TypeScript server).
type Repos struct {
	Servers app.ServerRepository
	Tools   app.ToolRepository
}

// NewPg builds the repositories on one pool.
func NewPg(pool *pgxpool.Pool) Repos {
	return Repos{Servers: &pgServers{pool: pool}, Tools: &pgTools{pool: pool}}
}
