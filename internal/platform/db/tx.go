package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// TxBeginner is what InTx needs from a pool (*pgxpool.Pool satisfies it).
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// rollbackTimeout bounds every rollback InTx issues.
const rollbackTimeout = 5 * time.Second

func rollback(ctx context.Context, tx pgx.Tx) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(rctx)
}

// InTx runs fn in a transaction: it commits when fn returns nil, and rolls back when fn returns an
// error, panics, or when the commit itself fails.
//
//   - The error of fn is returned unchanged (the same value, not wrapped). A rollback that fails after it is
//     dropped, not joined: the caller needs the cause, and a transaction that cannot be rolled back on a broken
//     connection is gone with that connection anyway.
//   - A panic in fn rolls back first and then continues unchanged (the same value, the same
//     stack): InTx never recovers it.
//   - The rollback runs on a context detached from ctx's cancellation with its own 5 s limit
//     (rollbackTimeout), so a cancelled request does not leave the transaction open, and a dead
//     connection cannot block the caller for ever.
//
// pool may also be a pgx.Tx (it has the same Begin): the "transaction" is then a savepoint inside
// that transaction, so InTx nests; a rollback undoes only the inner work. fn must not commit or roll
// back tx itself.
func InTx(ctx context.Context, pool TxBeginner, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished { // a panic (or runtime.Goexit) is unwinding through fn
			rollback(ctx, tx)
		}
	}()
	if err := fn(tx); err != nil {
		finished = true
		rollback(ctx, tx)
		return err
	}
	finished = true
	if err := tx.Commit(ctx); err != nil {
		// Best effort, result dropped. After a commit that reached the server and failed, the
		// transaction is already over and this only reports that; it does not undo anything.
		// It is for a commit that never got sent (cancelled context), where the transaction may
		// still be open and the rollback ends it.
		rollback(ctx, tx)
		return err
	}
	return nil
}
