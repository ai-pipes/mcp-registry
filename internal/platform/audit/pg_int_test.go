//go:build integration

package audit_test

import (
	"context"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/audit"
	"github.com/ai-pipes/mcp-registry/internal/platform/migrate"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/ai-pipes/mcp-registry/migrations"
)

func TestRecord(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Start(t)
	if _, err := migrate.Run(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	log := audit.NewPgAuditLog(pool)

	if err := log.Record(ctx, kernel.AuditEntry{Action: "a.empty"}); err != nil {
		t.Fatal(err)
	}
	actor, target := "6f1c2a3e-1b2c-4d5e-8f90-123456789abc", "srv"
	if err := log.Record(ctx, kernel.AuditEntry{
		ActorID: &actor, Action: "a.full", Target: &target,
		Meta: map[string]any{"n": 1, "s": "x"},
	}); err != nil {
		t.Fatal(err)
	}

	var meta string
	var actorNull, targetNull bool
	if err := pool.QueryRow(ctx, `select meta::text, actor_id is null, target is null
		from platform.audit_log where action = 'a.empty'`).Scan(&meta, &actorNull, &targetNull); err != nil {
		t.Fatal(err)
	}
	if meta != "{}" || !actorNull || !targetNull {
		t.Fatalf("empty entry: meta=%q actorNull=%v targetNull=%v", meta, actorNull, targetNull)
	}
	var gotActor, gotTarget string
	if err := pool.QueryRow(ctx, `select actor_id::text, target, meta::text from platform.audit_log
		where action = 'a.full'`).Scan(&gotActor, &gotTarget, &meta); err != nil {
		t.Fatal(err)
	}
	if gotActor != actor || gotTarget != target || meta != `{"n": 1, "s": "x"}` {
		t.Fatalf("full entry: %q %q %q", gotActor, gotTarget, meta)
	}
}
