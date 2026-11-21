//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5"
)

func rowCount(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
},
) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `select count(*) from intx_rows`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInTxAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Start(t)
	if _, err := pool.Exec(ctx, `create table intx_rows (id int primary key)`); err != nil {
		t.Fatal(err)
	}
	insert := func(id int) func(pgx.Tx) error {
		return func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `insert into intx_rows values ($1)`, id)
			return err
		}
	}

	t.Run("commits on nil", func(t *testing.T) {
		if err := db.InTx(ctx, pool, insert(1)); err != nil {
			t.Fatal(err)
		}
		if n := rowCount(t, pool); n != 1 {
			t.Fatalf("rows = %d", n)
		}
	})

	t.Run("rolls back on an error and returns it", func(t *testing.T) {
		boom := errors.New("boom")
		err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
			if err := insert(2)(tx); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
		if n := rowCount(t, pool); n != 1 {
			t.Fatalf("rows = %d, the insert must be rolled back", n)
		}
	})

	t.Run("rolls back on a panic and passes it on", func(t *testing.T) {
		var got any
		func() {
			defer func() { got = recover() }()
			_ = db.InTx(ctx, pool, func(tx pgx.Tx) error {
				if err := insert(3)(tx); err != nil {
					return err
				}
				panic("kaboom")
			})
		}()
		if got != "kaboom" {
			t.Fatalf("recovered %v", got)
		}
		if n := rowCount(t, pool); n != 1 {
			t.Fatalf("rows = %d, the insert must be rolled back", n)
		}
	})

	t.Run("rolls back when the context is cancelled mid-transaction", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		err := db.InTx(cctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(cctx, `insert into intx_rows values (4)`); err != nil {
				return err
			}
			cancel()
			_, err := tx.Exec(cctx, `insert into intx_rows values (5)`)
			return err
		})
		if err == nil {
			t.Fatal("want the cancellation error")
		}
		if n := rowCount(t, pool); n != 1 {
			t.Fatalf("rows = %d, nothing of a cancelled transaction may stay", n)
		}
		// The connection went back to the pool in a usable state.
		if err := db.InTx(ctx, pool, insert(6)); err != nil {
			t.Fatal(err)
		}
		if n := rowCount(t, pool); n != 2 {
			t.Fatalf("rows = %d", n)
		}
	})
}
