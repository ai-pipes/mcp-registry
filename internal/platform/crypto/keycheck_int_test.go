//go:build integration

package crypto_test

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
	"github.com/ai-pipes/mcp-registry/internal/platform/migrate"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/ai-pipes/mcp-registry/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func fresh(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.Start(t)
	if _, err := migrate.Run(context.Background(), pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return pool
}

func stored(t *testing.T, pool *pgxpool.Pool) []byte {
	t.Helper()
	var b []byte
	if err := pool.QueryRow(context.Background(), `select sealed from platform.key_check`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func rows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `select count(*) from platform.key_check`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustVerify(t *testing.T, pool *pgxpool.Pool, k crypto.Keys) string {
	t.Helper()
	v, err := crypto.Verify(context.Background(), pool, k)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyCreatesThenConfirms(t *testing.T) {
	pool := fresh(t)
	k := crypto.NewKeys(randKey(t), nil)
	if v := mustVerify(t, pool, k); v != crypto.KeyCheckCreated {
		t.Fatalf("first: %q", v)
	}
	if v := mustVerify(t, pool, k); v != crypto.KeyCheckOK {
		t.Fatalf("second: %q", v)
	}
	if n := rows(t, pool); n != 1 {
		t.Fatalf("rows %d", n)
	}
}

func TestVerifyForeignKeyIsMismatchAndLeavesTheRow(t *testing.T) {
	pool := fresh(t)
	mustVerify(t, pool, crypto.NewKeys(randKey(t), nil))
	before := stored(t, pool)
	if v := mustVerify(t, pool, crypto.NewKeys(randKey(t), randKey(t))); v != crypto.KeyCheckMismatch {
		t.Fatalf("got %q", v)
	}
	if !bytes.Equal(before, stored(t, pool)) {
		t.Fatal("row changed")
	}
}

func TestVerifyOnlyPreviousKeyNeedsRotation(t *testing.T) {
	pool := fresh(t)
	old := randKey(t)
	mustVerify(t, pool, crypto.NewKeys(old, nil))
	before := stored(t, pool)
	if v := mustVerify(t, pool, crypto.NewKeys(randKey(t), old)); v != crypto.KeyCheckNeedsRotation {
		t.Fatalf("got %q", v)
	}
	if !bytes.Equal(before, stored(t, pool)) {
		t.Fatal("row changed")
	}
}

func TestVerifyConcurrentFirstStartsOneCreatesOtherConfirms(t *testing.T) {
	pool := fresh(t)
	k := crypto.NewKeys(randKey(t), nil)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	verdicts := make([]string, 2)
	errs := make([]error, 2)
	for i := range verdicts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			verdicts[i], errs[i] = crypto.Verify(context.Background(), pool, k)
		}()
	}
	close(gate)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	created, ok := 0, 0
	for _, v := range verdicts {
		switch v {
		case crypto.KeyCheckCreated:
			created++
		case crypto.KeyCheckOK:
			ok++
		}
	}
	if created != 1 || ok != 1 {
		t.Fatalf("verdicts %v", verdicts)
	}
	if n := rows(t, pool); n != 1 {
		t.Fatalf("rows %d", n)
	}
}

func TestInspectNeverCreates(t *testing.T) {
	pool := fresh(t)
	k := crypto.NewKeys(randKey(t), nil)
	v, err := crypto.Inspect(context.Background(), pool, k)
	if err != nil || v != crypto.KeyCheckAbsent {
		t.Fatalf("%q %v", v, err)
	}
	if n := rows(t, pool); n != 0 {
		t.Fatalf("rows %d", n)
	}
	mustVerify(t, pool, k)
	if v, _ := crypto.Inspect(context.Background(), pool, k); v != crypto.KeyCheckOK {
		t.Fatalf("got %q", v)
	}
}

func TestResealCheck(t *testing.T) {
	ctx := context.Background()
	pool := fresh(t)
	old, cur := randKey(t), randKey(t)
	rotated := crypto.NewKeys(cur, old)

	if v, err := crypto.ResealCheck(ctx, pool, rotated); err != nil || v != "absent" {
		t.Fatalf("absent: %q %v", v, err)
	}
	mustVerify(t, pool, crypto.NewKeys(old, nil))
	if v, err := crypto.ResealCheck(ctx, pool, crypto.NewKeys(randKey(t), randKey(t))); err != nil || v != "unreadable" {
		t.Fatalf("unreadable: %q %v", v, err)
	}
	if v, err := crypto.ResealCheck(ctx, pool, rotated); err != nil || v != "resealed" {
		t.Fatalf("resealed: %q %v", v, err)
	}
	if v := crypto.Judge(stored(t, pool), crypto.NewKeys(cur, nil)); v != crypto.KeyCheckOK {
		t.Fatalf("after reseal: %q", v)
	}
	if v, err := crypto.ResealCheck(ctx, pool, rotated); err != nil || v != "current" {
		t.Fatalf("repeat: %q %v", v, err)
	}
	// Without a previous key: current opens it, a foreign key does not.
	if v, err := crypto.ResealCheck(ctx, pool, crypto.NewKeys(cur, nil)); err != nil || v != "current" {
		t.Fatalf("no previous, right key: %q %v", v, err)
	}
	if v, err := crypto.ResealCheck(ctx, pool, crypto.NewKeys(randKey(t), nil)); err != nil || v != "unreadable" {
		t.Fatalf("no previous, foreign key: %q %v", v, err)
	}
}

func TestRemove(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	if ok, err := crypto.Remove(ctx, pool); err != nil || ok {
		t.Fatalf("empty: %v %v", ok, err)
	}
	mustVerify(t, pool, crypto.NewKeys(randKey(t), nil))
	if ok, err := crypto.Remove(ctx, pool); err != nil || !ok {
		t.Fatalf("stored: %v %v", ok, err)
	}
	if n := rows(t, pool); n != 0 {
		t.Fatalf("rows %d", n)
	}
}
