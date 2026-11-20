// Package pgtest starts a throwaway Postgres for integration tests.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// image is the same pinned pgvector image as dbImage in compat/harness/stack.go (that package is
// behind the compat build tag and cannot be imported here). TestImageMatchesCompatHarness fails
// when the two differ: bump both together.
const image = "pgvector/pgvector:pg17@sha256:b513c48566e4777cd3370451400000c78eda6a08131d68f4c31d4a76f27bd8fd"

const (
	adminUser     = "postgres"
	adminPassword = "pgtest"
	adminDB       = "postgres"
)

var (
	once     sync.Once
	adminURL string
	startErr error
)

// server starts the container once per test binary; the reaper removes it when the binary exits.
func server(t *testing.T) string {
	t.Helper()
	once.Do(func() {
		ctx := context.Background()
		c, err := postgres.Run(ctx, image,
			postgres.WithDatabase(adminDB),
			postgres.WithUsername(adminUser),
			postgres.WithPassword(adminPassword),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			startErr = err
			return
		}
		adminURL, startErr = c.ConnectionString(ctx, "sslmode=disable")
	})
	if startErr != nil {
		t.Fatalf("pgtest: cannot start Postgres (is Docker running?): %v", startErr)
	}
	return adminURL
}

// StartURL gives the test its own empty database on the shared container and returns its
// connection string. The database is dropped when the test ends.
func StartURL(t *testing.T) string {
	t.Helper()
	base := server(t)
	ctx := context.Background()

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "t_" + hex.EncodeToString(b[:])

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("pgtest: admin connection: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, `create database `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Logf("pgtest: cannot drop %s: %v", name, err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(context.Background(), `drop database if exists `+pgx.Identifier{name}.Sanitize()+` with (force)`); err != nil {
			t.Logf("pgtest: cannot drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// Start gives the test its own empty database and a pool on it, closed when the test ends.
func Start(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), StartURL(t))
	if err != nil {
		t.Fatalf("pgtest: pool: %v", err)
	}
	t.Cleanup(pool.Close) // runs before the drop registered inside StartURL
	return pool
}
