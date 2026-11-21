package db

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// TxBeginner is what InTx needs from a pool (*pgxpool.Pool satisfies it).
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// InTx runs fn in a transaction: it commits when fn returns nil, and rolls back when fn returns an
// error, panics, or when the commit itself fails.
//
//   - The error of fn is returned as it is. A rollback that fails after it is dropped, not joined:
//     the caller needs the cause, and a transaction that cannot be rolled back on a broken
//     connection is gone with that connection anyway.
//   - A panic in fn rolls back first and then continues unchanged (the same value, the same
//     stack): InTx never recovers it.
//   - The rollback runs on a context detached from ctx's cancellation, so a cancelled request
//     does not leave the transaction open until the connection is reclaimed.
//
// fn must not commit or roll back tx itself.
func InTx(ctx context.Context, pool TxBeginner, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished { // a panic (or runtime.Goexit) is unwinding through fn
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err := fn(tx); err != nil {
		finished = true
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	finished = true
	if err := tx.Commit(ctx); err != nil {
		// A commit that did not complete (cancelled context, lost connection) leaves the
		// transaction to be ended explicitly; after a completed one this is a no-op.
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return nil
}
