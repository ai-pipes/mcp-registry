// Package db opens the Postgres connection pool.
package db

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolMaxConns is the pool size.
const PoolMaxConns = 10

// connectError replaces the driver's error: pgx messages can quote the connection string, so
// callers get a fixed text and a content-free label (kernel.ErrorLabel), never the original.
type connectError struct{ msg string }

func (e *connectError) Error() string { return e.msg }

// Open builds the pool from databaseURL and pings it. A non-nil password (from PGPASSWORD_FILE)
// overrides the one in the address and in PGPASSWORD; without it pgx's own rules apply. Errors
// never contain the address or any password.
func Open(ctx context.Context, databaseURL string, password *string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, &connectError{"invalid database configuration (" + kernel.ErrorLabel(err) + ")"}
	}
	if password != nil {
		cfg.ConnConfig.Password = *password
	}
	cfg.MaxConns = PoolMaxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, &connectError{"cannot open the database pool (" + kernel.ErrorLabel(err) + ")"}
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, &connectError{"cannot connect to the database (" + kernel.ErrorLabel(err) + ")"}
	}
	return pool, nil
}
