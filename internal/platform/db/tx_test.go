package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

// fakeTx records what InTx does to it. The embedded nil pgx.Tx makes any other method a loud failure.
type fakeTx struct {
	pgx.Tx
	commits, rollbacks int
	commitErr          error
	rollbackErr        error
	rollbackCtxErr     error
}

func (f *fakeTx) Commit(context.Context) error { f.commits++; return f.commitErr }
func (f *fakeTx) Rollback(ctx context.Context) error {
	f.rollbacks++
	f.rollbackCtxErr = ctx.Err()
	return f.rollbackErr
}

type fakePool struct {
	tx       *fakeTx
	beginErr error
}

func (p *fakePool) Begin(context.Context) (pgx.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	return p.tx, nil
}

func TestInTxCommitsOnNil(t *testing.T) {
	p := &fakePool{tx: &fakeTx{}}
	if err := db.InTx(context.Background(), p, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if p.tx.commits != 1 || p.tx.rollbacks != 0 {
		t.Fatalf("commits=%d rollbacks=%d", p.tx.commits, p.tx.rollbacks)
	}
}

func TestInTxRollsBackAndReturnsTheOriginalError(t *testing.T) {
	boom := errors.New("boom")
	p := &fakePool{tx: &fakeTx{rollbackErr: errors.New("rollback failed")}}
	err := db.InTx(context.Background(), p, func(pgx.Tx) error { return boom })
	if !errors.Is(err, boom) || err.Error() != "boom" {
		t.Fatalf("got %v, want the function's own error unchanged (a failed rollback is dropped, not joined)", err)
	}
	if p.tx.commits != 0 || p.tx.rollbacks != 1 {
		t.Fatalf("commits=%d rollbacks=%d", p.tx.commits, p.tx.rollbacks)
	}
}

func TestInTxRollsBackOnPanicAndRepanicsWithTheSameValue(t *testing.T) {
	p := &fakePool{tx: &fakeTx{}}
	type marker struct{ n int }
	want := &marker{1}
	var got any
	func() {
		defer func() { got = recover() }()
		_ = db.InTx(context.Background(), p, func(pgx.Tx) error { panic(want) })
	}()
	if got != any(want) {
		t.Fatalf("recovered %v, want the original panic value", got)
	}
	if p.tx.commits != 0 || p.tx.rollbacks != 1 {
		t.Fatalf("commits=%d rollbacks=%d", p.tx.commits, p.tx.rollbacks)
	}
}

func TestInTxRollsBackWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &fakePool{tx: &fakeTx{}}
	err := db.InTx(ctx, p, func(pgx.Tx) error {
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if p.tx.rollbacks != 1 || p.tx.rollbackCtxErr != nil {
		t.Fatalf("the rollback must run, and on a context that is not cancelled: rollbacks=%d ctxErr=%v", p.tx.rollbacks, p.tx.rollbackCtxErr)
	}
}

func TestInTxReturnsTheCommitErrorAndEndsTheTransaction(t *testing.T) {
	commitErr := errors.New("commit failed")
	p := &fakePool{tx: &fakeTx{commitErr: commitErr}}
	if err := db.InTx(context.Background(), p, func(pgx.Tx) error { return nil }); !errors.Is(err, commitErr) {
		t.Fatalf("got %v", err)
	}
	if p.tx.rollbacks != 1 {
		t.Fatalf("a failed commit is followed by one rollback to release the connection, got %d", p.tx.rollbacks)
	}
}

func TestInTxReturnsTheBeginError(t *testing.T) {
	beginErr := errors.New("no connection")
	called := false
	err := db.InTx(context.Background(), &fakePool{beginErr: beginErr}, func(pgx.Tx) error { called = true; return nil })
	if !errors.Is(err, beginErr) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}
