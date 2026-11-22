//go:build integration

package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	t0 = time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	t1 = time.Date(2026, 10, 30, 11, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
)

const (
	adminA = "22222222-2222-2222-2222-222222222222"
	adminB = "44444444-4444-4444-4444-444444444444"
)

func setup(t *testing.T) (*pgxpool.Pool, Repos) {
	t.Helper()
	pool := pgtest.StartMigrated(t)
	return pool, NewPg(pool)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func code(err error) string {
	if de, ok := kernel.AsDomainError(err); ok {
		return de.Code
	}
	return ""
}

func wantCode(t *testing.T, err error, want, msg string) {
	t.Helper()
	de, ok := kernel.AsDomainError(err)
	if !ok || de.Code != want || (msg != "" && de.Message != msg) {
		t.Fatalf("error = %v, want %s %q", err, want, msg)
	}
}

var oauthAuth = domain.AuthMode{
	Kind: domain.AuthOAuthUser, Header: "Authorization", Template: "Bearer {secret}",
	OAuth: oauthCfg("https://git.example.com/authorize", "https://git.example.com/token", "cid", []string{"read", "write"}, nil,
		[]domain.AuthorizeParam{{Key: "access_type", Value: "offline"}}),
}

var sharedAuth = domain.AuthMode{Kind: domain.AuthSharedSecret, Header: "X-Key", Template: "{secret}"}

func newServer(t *testing.T, slug string, mut ...func(*domain.NewServerParams)) *domain.Server {
	t.Helper()
	limit := 10
	timeout := 30_000
	p := domain.NewServerParams{
		Slug: slug, Name: "Gitea", Description: "Git hosting", URL: "https://mcp.example.com/gitea", Auth: oauthAuth,
		Access: domain.Access{Kind: domain.AccessGroups, Groups: []string{"dev"}}, TimeoutMs: &timeout, DailyLimit: &limit, Now: t0,
	}
	for _, m := range mut {
		m(&p)
	}
	s, err := domain.NewServer(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func withAuth(a domain.AuthMode) func(*domain.NewServerParams) {
	return func(p *domain.NewServerParams) { p.Auth = a }
}

func create(t *testing.T, r Repos, s *domain.Server) {
	t.Helper()
	mustOK(t, r.Servers.Create(context.Background(), s, s.PullEvents()))
}

func save(t *testing.T, r Repos, s *domain.Server) error {
	t.Helper()
	return r.Servers.Save(context.Background(), s, s.PullEvents())
}

func load(t *testing.T, r Repos, slug string) *domain.Server {
	t.Helper()
	s, err := r.Servers.FindBySlug(context.Background(), slug)
	if err != nil || s == nil {
		t.Fatalf("FindBySlug(%s) = %v, %v", slug, s, err)
	}
	return s
}

type outboxRow struct {
	Type    string
	Payload map[string]any
}

func outboxRows(t *testing.T, pool *pgxpool.Pool) []outboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select type, payload from platform.outbox order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []outboxRow{}
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.Type, &r.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	mustOK(t, rows.Err())
	return out
}

func outboxTypes(t *testing.T, pool *pgxpool.Pool) []string {
	types := []string{}
	for _, r := range outboxRows(t, pool) {
		types = append(types, r.Type)
	}
	return types
}

func clearOutbox(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `truncate platform.outbox`)
	mustOK(t, err)
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	mustOK(t, err)
}

// ---- servers ----

func TestServerRoundTripsEveryAuthKind(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	revoke := "https://git.example.com/revoke"
	full := oauthAuth.Clone()
	full.OAuth.RevokeURL = &revoke
	cases := map[string]domain.AuthMode{
		"none":   {Kind: domain.AuthNone},
		"shared": sharedAuth,
		"pat":    {Kind: domain.AuthPATUser, Header: "Authorization", Template: "Bearer {secret}"},
		"oauth":  oauthAuth,
		"full":   full,
	}
	for slug, a := range cases {
		s := newServer(t, slug, withAuth(a))
		if a.HoldsSecret() {
			mustOK(t, s.SetSealedSecret([]byte{0, 1, 2, 255}, t0))
		}
		create(t, r, s)
		got := load(t, r, slug)
		if !reflect.DeepEqual(got.Auth(), a) {
			t.Fatalf("%s: auth = %+v, want %+v", slug, got.Auth(), a)
		}
		if got.Version() != 0 || got.Name() != "Gitea" || got.Description() != "Git hosting" || got.URL() != "https://mcp.example.com/gitea" ||
			got.TimeoutMs() != 30_000 || got.DailyLimit() == nil || *got.DailyLimit() != 10 || got.Status() != domain.StatusActive ||
			got.LastSyncAt() != nil || got.LastSyncError() != nil || got.SyncUserID() != nil ||
			!got.CreatedAt().Equal(t0) || !got.UpdatedAt().Equal(t0) || got.CreatedAt().Location() != time.UTC {
			t.Fatalf("%s: %+v", slug, got)
		}
		if a.HoldsSecret() && !bytes.Equal(got.SealedSecret(), []byte{0, 1, 2, 255}) {
			t.Fatalf("%s: secret = %v", slug, got.SealedSecret())
		}
		if !a.HoldsSecret() && got.SecretSet() {
			t.Fatalf("%s: secret set", slug)
		}
		if got.SecretChanged() {
			t.Fatal("a loaded server must not report a changed secret")
		}
	}
	// access
	all := newServer(t, "open", func(p *domain.NewServerParams) { p.Access = domain.Access{Kind: domain.AccessAll}; p.DailyLimit = nil })
	create(t, r, all)
	got := load(t, r, "open")
	if got.Access().Kind != domain.AccessAll || got.DailyLimit() != nil {
		t.Fatalf("%+v", got)
	}
	if g := load(t, r, "oauth").Access(); !g.Equal(domain.Access{Kind: domain.AccessGroups, Groups: []string{"dev"}}) {
		t.Fatalf("%+v", g)
	}
	if s, err := r.Servers.FindBySlug(ctx, "nope"); s != nil || err != nil {
		t.Fatalf("unknown: %v %v", s, err)
	}
	_ = pool
}

func TestSealedBytesAreStoredVerbatim(t *testing.T) {
	pool, r := setup(t)
	s := newServer(t, "gitea")
	mustOK(t, s.SetSealedSecret([]byte("opaque-bytes"), t0))
	create(t, r, s)
	var b []byte
	mustOK(t, pool.QueryRow(context.Background(), `select secret from catalog.servers`).Scan(&b))
	if string(b) != "opaque-bytes" {
		t.Fatalf("%q", b)
	}
}

func TestCreateWritesItsEventsAndAKeyedDuplicateWritesNone(t *testing.T) {
	pool, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	if got := outboxRows(t, pool); !reflect.DeepEqual(got, []outboxRow{{"catalog.server_changed", map[string]any{"server": "gitea"}}}) {
		t.Fatalf("%+v", got)
	}
	dup := newServer(t, "gitea")
	err := r.Servers.Create(context.Background(), dup, dup.PullEvents())
	wantCode(t, err, domain.CodeServerExists, `server "gitea" already exists`)
	if n := len(outboxRows(t, pool)); n != 1 {
		t.Fatalf("outbox has %d rows", n)
	}
}

func TestSaveUpdatesFieldsButLeavesSyncStateAlone(t *testing.T) {
	pool, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	exec(t, pool, `update catalog.servers set last_sync_at = $1, last_sync_error = 'boom',
      sync_user_id = '11111111-1111-1111-1111-111111111111'`, t1)
	s := load(t, r, "gitea")
	_, err := s.Update(domain.Changes{Name: domain.Some("Gitea 2"), DailyLimit: domain.Some[*int](nil)}, t1)
	mustOK(t, err)
	s.SetStatus(domain.StatusDisabled, t1)
	mustOK(t, s.SetSealedSecret([]byte{7}, t1))
	mustOK(t, save(t, r, s))
	got := load(t, r, "gitea")
	if got.Name() != "Gitea 2" || got.DailyLimit() != nil || got.Status() != domain.StatusDisabled || got.Version() != 1 ||
		got.LastSyncAt() == nil || !got.LastSyncAt().Equal(t1) || got.LastSyncError() == nil || *got.LastSyncError() != "boom" ||
		got.SyncUserID() == nil || *got.SyncUserID() != "11111111-1111-1111-1111-111111111111" || !got.UpdatedAt().Equal(t1) ||
		!bytes.Equal(got.SealedSecret(), []byte{7}) {
		t.Fatalf("%+v", got)
	}
	if got := outboxTypes(t, pool); !slices.Equal(got, []string{"catalog.server_changed", "catalog.server_changed"}) {
		t.Fatalf("%v", got)
	}
}

func TestAStaleSaveIsRefusedNotMerged(t *testing.T) {
	_, r := setup(t)
	s := newServer(t, "yt", withAuth(sharedAuth))
	mustOK(t, s.SetSealedSecret([]byte{1}, t0))
	create(t, r, s)
	// A (new URL) and B (new secret) both read; B commits first.
	a, b := load(t, r, "yt"), load(t, r, "yt")
	mustOK(t, b.SetSealedSecret([]byte{2}, t1))
	mustOK(t, save(t, r, b))
	_, err := a.Update(domain.Changes{URL: domain.Some("https://elsewhere.example.com/mcp")}, t1)
	mustOK(t, err)
	wantCode(t, save(t, r, a), domain.CodeServerConflict, `server "yt" was changed meanwhile; reload it and try again`)
	got := load(t, r, "yt")
	if got.URL() != "https://mcp.example.com/gitea" || !bytes.Equal(got.SealedSecret(), []byte{2}) {
		t.Fatalf("%s %v", got.URL(), got.SealedSecret())
	}
}

func TestASecretForTheOldURLIsRefusedAfterTheURLChanged(t *testing.T) {
	_, r := setup(t)
	s := newServer(t, "yt", withAuth(sharedAuth))
	mustOK(t, s.SetSealedSecret([]byte{1}, t0))
	create(t, r, s)
	b, a := load(t, r, "yt"), load(t, r, "yt")
	_, err := a.Update(domain.Changes{URL: domain.Some("https://elsewhere.example.com/mcp")}, t1)
	mustOK(t, err)
	mustOK(t, save(t, r, a))
	mustOK(t, b.SetSealedSecret([]byte{2}, t1))
	wantCode(t, save(t, r, b), domain.CodeServerConflict, "")
	got := load(t, r, "yt")
	if got.URL() != "https://elsewhere.example.com/mcp" || got.SecretSet() {
		t.Fatalf("%s %v", got.URL(), got.SecretSet())
	}
	fresh := load(t, r, "yt")
	mustOK(t, fresh.SetSealedSecret([]byte{3}, t1))
	mustOK(t, save(t, r, fresh))
	if !bytes.Equal(load(t, r, "yt").SealedSecret(), []byte{3}) {
		t.Fatal("reloaded save must go through")
	}
}

func TestSaveAfterAReSealKeepsTheNewCiphertext(t *testing.T) {
	ctx := context.Background()
	_, r := setup(t)
	s := newServer(t, "yt", withAuth(sharedAuth))
	mustOK(t, s.SetSealedSecret([]byte{1}, t0))
	create(t, r, s)
	stale := load(t, r, "yt")
	ok, err := r.Servers.ReplaceSealedSecret(ctx, "yt", []byte{1}, []byte{9})
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	stale.SetStatus(domain.StatusDisabled, t1)
	mustOK(t, save(t, r, stale))
	if got := load(t, r, "yt"); !bytes.Equal(got.SealedSecret(), []byte{9}) || got.Status() != domain.StatusDisabled {
		t.Fatalf("%v %v", got.SealedSecret(), got.Status())
	}
}

func TestAChangeOfAuthKindClearsTheStoredSecretOnSave(t *testing.T) {
	_, r := setup(t)
	s := newServer(t, "yt", withAuth(sharedAuth))
	mustOK(t, s.SetSealedSecret([]byte{1}, t0))
	create(t, r, s)
	l := load(t, r, "yt")
	cleared, err := l.Update(domain.Changes{Auth: domain.Some(domain.AuthMode{Kind: domain.AuthNone})}, t1)
	if err != nil || !cleared {
		t.Fatalf("%v %v", cleared, err)
	}
	mustOK(t, save(t, r, l))
	if got := load(t, r, "yt"); got.SecretSet() || got.Auth().Kind != domain.AuthNone {
		t.Fatalf("%+v", got)
	}
}

func TestSaveOfAnUnknownServerIsNotFound(t *testing.T) {
	_, r := setup(t)
	s := newServer(t, "ghost")
	wantCode(t, r.Servers.Save(context.Background(), s, nil), domain.CodeNotFound, `server "ghost" not found`)
}

func TestSaveSyncState(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	clearOutbox(t, pool)
	mustOK(t, r.Servers.SaveSyncState(ctx, "gitea", domain.SyncSuccess(strPtr(adminA), t1)))
	got := load(t, r, "gitea")
	if got.Name() != "Gitea" || got.LastSyncAt() == nil || !got.LastSyncAt().Equal(t1) || got.LastSyncError() != nil ||
		got.SyncUserID() == nil || *got.SyncUserID() != adminA || !got.UpdatedAt().Equal(t0) || got.Version() != 0 {
		t.Fatalf("%+v", got)
	}
	// a later failure and a scheduler success keep the admin
	mustOK(t, r.Servers.SaveSyncState(ctx, "gitea", domain.SyncFailureRecord(domain.FailureNeedsConnection, t2)))
	got = load(t, r, "gitea")
	if got.LastSyncAt() == nil || !got.LastSyncAt().Equal(t2) || got.LastSyncError() == nil ||
		*got.LastSyncError() != "An administrator needs to connect this server" || *got.SyncUserID() != adminA {
		t.Fatalf("%+v", got)
	}
	mustOK(t, r.Servers.SaveSyncState(ctx, "gitea", domain.SyncSuccess(nil, t2)))
	if got = load(t, r, "gitea"); got.LastSyncError() != nil || *got.SyncUserID() != adminA {
		t.Fatalf("%+v", got)
	}
	if n := len(outboxRows(t, pool)); n != 0 {
		t.Fatalf("%d events", n)
	}
}

func strPtr(s string) *string { return &s }

func TestSaveSyncStateIgnoresAnOlderAttempt(t *testing.T) {
	ctx := context.Background()
	_, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	mustOK(t, r.Servers.SaveSyncState(ctx, "gitea", domain.SyncSuccess(strPtr(adminB), t1)))
	mustOK(t, r.Servers.SaveSyncState(ctx, "gitea", domain.SyncSuccess(strPtr(adminA), t0)))
	mustOK(t, r.Servers.SaveSyncState(ctx, "gitea", domain.SyncFailureRecord(domain.FailureTimeout, t0)))
	got := load(t, r, "gitea")
	if !got.LastSyncAt().Equal(t1) || got.LastSyncError() != nil || *got.SyncUserID() != adminB {
		t.Fatalf("%+v", got)
	}
	// an equal start is accepted
	mustOK(t, r.Servers.SaveSyncState(ctx, "gitea", domain.SyncFailureRecord(domain.FailureTimeout, t1)))
	if load(t, r, "gitea").LastSyncError() == nil {
		t.Fatal("an equal start must be written")
	}
}

func TestSaveSyncStateStoresMicrosecondsSoTheGuardAgrees(t *testing.T) {
	ctx := context.Background()
	_, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	started := time.Date(2026, 10, 30, 10, 0, 0, 123456789, time.UTC)
	mustOK(t, r.Servers.SaveSyncState(ctx, "gitea", domain.SyncSuccess(nil, started)))
	// the same instant again: with nanoseconds kept in one place and truncated in the other, this
	// would look older than the stored value
	mustOK(t, r.Servers.SaveSyncState(ctx, "gitea", domain.SyncFailureRecord(domain.FailureTimeout, started)))
	got := load(t, r, "gitea")
	if got.LastSyncError() == nil || !got.LastSyncAt().Equal(started.Truncate(time.Microsecond)) {
		t.Fatalf("%v %v", got.LastSyncAt(), got.LastSyncError())
	}
}

func TestSaveSyncStateOfAnUnknownServerIsNotFound(t *testing.T) {
	_, r := setup(t)
	wantCode(t, r.Servers.SaveSyncState(context.Background(), "nope", domain.SyncSuccess(nil, t0)), domain.CodeNotFound, `server "nope" not found`)
}

func TestReplaceSealedSecretIsCompareAndSetAndQuiet(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	s := newServer(t, "yt", withAuth(sharedAuth))
	mustOK(t, s.SetSealedSecret([]byte{1}, t0))
	create(t, r, s)
	clearOutbox(t, pool)
	if ok, err := r.Servers.ReplaceSealedSecret(ctx, "yt", []byte{5}, []byte{9}); err != nil || ok {
		t.Fatalf("a wrong old value must not replace: %v %v", ok, err)
	}
	if ok, err := r.Servers.ReplaceSealedSecret(ctx, "nope", []byte{1}, []byte{9}); err != nil || ok {
		t.Fatalf("unknown server: %v %v", ok, err)
	}
	if ok, err := r.Servers.ReplaceSealedSecret(ctx, "yt", []byte{1}, []byte{9}); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, _ := r.Servers.ReplaceSealedSecret(ctx, "yt", []byte{1}, []byte{8}); ok {
		t.Fatal("a repeat with the old value must not replace")
	}
	got := load(t, r, "yt")
	if !bytes.Equal(got.SealedSecret(), []byte{9}) || got.Version() != 0 || !got.UpdatedAt().Equal(t0) || len(outboxRows(t, pool)) != 0 {
		t.Fatalf("%v v%d %v", got.SealedSecret(), got.Version(), got.UpdatedAt())
	}
}

func TestListIsOrderedBySlugAndCountsVisibleToolsOnly(t *testing.T) {
	ctx := context.Background()
	_, r := setup(t)
	create(t, r, newServer(t, "beta"))
	create(t, r, newServer(t, "alpha"))
	_, err := r.Tools.ApplySnapshot(ctx, "alpha", []domain.ToolSnapshot{snap("a"), snap("b"), snap("c")}, t0)
	mustOK(t, err)
	hidden := mustTool(t, r, "alpha", "b")
	_, err = hidden.SetHidden(true, t1)
	mustOK(t, err)
	mustOK(t, r.Tools.Save(ctx, hidden, hidden.PullEvents()))
	_, err = r.Tools.ApplySnapshot(ctx, "alpha", []domain.ToolSnapshot{snap("a"), snap("b")}, t1) // c removed
	mustOK(t, err)
	list, err := r.Servers.List(ctx)
	mustOK(t, err)
	got := []string{}
	for _, l := range list {
		got = append(got, l.Server.Slug()+":"+strconv.Itoa(l.ToolCount))
	}
	if !slices.Equal(got, []string{"alpha:1", "beta:0"}) {
		t.Fatalf("%v", got)
	}
	if list[0].Server.Version() != 0 || list[0].Server.Auth().Kind != domain.AuthOAuthUser {
		t.Fatalf("%+v", list[0].Server)
	}
}

// ---- transactions and locks ----

func TestEventsAreInvisibleUntilCommitAndRolledBackWithTheChange(t *testing.T) {
	ctx := context.Background()
	pool, _ := setup(t)
	boom := errors.New("boom")

	t.Run("Create: visible from another connection only after commit", func(t *testing.T) {
		var seen int
		repo := &pgServers{pool: pool, afterEvents: func() error {
			seen = len(outboxRows(t, pool)) // another connection, while the transaction is open
			return nil
		}}
		s := newServer(t, "one")
		mustOK(t, repo.Create(ctx, s, s.PullEvents()))
		if seen != 0 || len(outboxRows(t, pool)) != 1 {
			t.Fatalf("seen %d, after %d", seen, len(outboxRows(t, pool)))
		}
	})
	t.Run("Create: an error after the events rolls both back", func(t *testing.T) {
		clearOutbox(t, pool)
		repo := &pgServers{pool: pool, afterEvents: func() error { return boom }}
		s := newServer(t, "two")
		if err := repo.Create(ctx, s, s.PullEvents()); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		var n int
		mustOK(t, pool.QueryRow(ctx, `select count(*) from catalog.servers where slug = 'two'`).Scan(&n))
		if n != 0 || len(outboxRows(t, pool)) != 0 {
			t.Fatalf("server rows %d, events %d", n, len(outboxRows(t, pool)))
		}
	})
	t.Run("Save: the change, the secret and the events roll back together", func(t *testing.T) {
		clearOutbox(t, pool)
		good := NewPg(pool)
		s := newServer(t, "three", withAuth(sharedAuth))
		mustOK(t, s.SetSealedSecret([]byte{1}, t0))
		create(t, good, s)
		clearOutbox(t, pool)
		l := load(t, good, "three")
		l.SetStatus(domain.StatusDisabled, t1)
		l.ClearSecret(t1)
		repo := &pgServers{pool: pool, afterEvents: func() error { return boom }}
		if err := repo.Save(ctx, l, l.PullEvents()); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		got := load(t, good, "three")
		if got.Status() != domain.StatusActive || !bytes.Equal(got.SealedSecret(), []byte{1}) || got.Version() != 0 || len(outboxRows(t, pool)) != 0 {
			t.Fatalf("%+v events %d", got, len(outboxRows(t, pool)))
		}
	})
	t.Run("Tool.Save", func(t *testing.T) {
		clearOutbox(t, pool)
		good := NewPg(pool)
		create(t, good, newServer(t, "four"))
		_, err := good.Tools.ApplySnapshot(ctx, "four", []domain.ToolSnapshot{snap("a")}, t0)
		mustOK(t, err)
		clearOutbox(t, pool)
		tool := mustTool(t, good, "four", "a")
		_, err = tool.SetHidden(true, t1)
		mustOK(t, err)
		repo := &pgTools{pool: pool, afterEvents: func() error { return boom }}
		if err := repo.Save(ctx, tool, tool.PullEvents()); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if mustTool(t, good, "four", "a").Hidden() || len(outboxRows(t, pool)) != 0 {
			t.Fatal("hidden or events survived the rollback")
		}
	})
	t.Run("ApplySnapshot", func(t *testing.T) {
		clearOutbox(t, pool)
		good := NewPg(pool)
		create(t, good, newServer(t, "five"))
		clearOutbox(t, pool)
		repo := &pgTools{pool: pool, afterEvents: func() error { return boom }}
		if _, err := repo.ApplySnapshot(ctx, "five", []domain.ToolSnapshot{snap("a")}, t0); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		list, err := good.Tools.ListByServer(ctx, "five")
		mustOK(t, err)
		if len(list) != 0 || len(outboxRows(t, pool)) != 0 {
			t.Fatalf("tools %d events %d", len(list), len(outboxRows(t, pool)))
		}
	})
}

// gate starts the functions together: each waits on the same channel before running.
func gate(fns ...func()) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, f := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			f()
		}()
	}
	close(start)
	wg.Wait()
}

func TestTwoSavesAtOneVersionExactlyOneWins(t *testing.T) {
	_, r := setup(t)
	for round := range 15 {
		slug := "race" + string(rune('a'+round))
		create(t, r, newServer(t, slug))
		a, b := load(t, r, slug), load(t, r, slug)
		_, err := a.Update(domain.Changes{Description: domain.Some("from a")}, t1)
		mustOK(t, err)
		_, err = b.Update(domain.Changes{Description: domain.Some("from b")}, t1)
		mustOK(t, err)
		var errA, errB error
		gate(func() { errA = save(t, r, a) }, func() { errB = save(t, r, b) })
		if (errA == nil) == (errB == nil) {
			t.Fatalf("round %d: errors %v / %v: exactly one must succeed", round, errA, errB)
		}
		loser := errA
		want := "from b"
		if errA == nil {
			loser, want = errB, "from a"
		}
		wantCode(t, loser, domain.CodeServerConflict, "")
		got := load(t, r, slug)
		if got.Description() != want || got.Version() != 1 {
			t.Fatalf("round %d: %q v%d, want %q v1", round, got.Description(), got.Version(), want)
		}
	}
}

// eventually polls a database condition until the deadline.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

func blockedBackends(pool *pgxpool.Pool) int {
	var n int
	if err := pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity
      where datname = current_database() and cardinality(pg_blocking_pids(pid)) > 0`).Scan(&n); err != nil {
		panic(err)
	}
	return n
}

func TestSaveWaitsForARowLockThenAppliesItsVersionRule(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		holderBump bool
		wantErr    bool
	}{{"holder commits unchanged: the save goes through", false, false}, {"holder bumps the version: conflict", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			pool, r := setup(t)
			create(t, r, newServer(t, "gitea"))
			s := load(t, r, "gitea")
			_, err := s.Update(domain.Changes{Description: domain.Some("mine")}, t1)
			mustOK(t, err)

			holder, err := pool.Begin(ctx)
			mustOK(t, err)
			defer func() { _ = holder.Rollback(context.WithoutCancel(ctx)) }()
			var slug string
			mustOK(t, holder.QueryRow(ctx, `select slug from catalog.servers where slug = 'gitea' for update`).Scan(&slug))

			// reads take no lock and are not blocked by the holder
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = r.Servers.FindBySlug(ctx, "gitea")
				_, _ = r.Servers.List(ctx)
				_, _ = r.Tools.ListByServer(ctx, "gitea")
			}()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("a read was blocked by another transaction's row lock")
			}

			result := make(chan error, 1)
			go func() { result <- save(t, r, s) }()
			eventually(t, "the save to block on the row lock", func() bool { return blockedBackends(pool) >= 1 })
			select {
			case err := <-result:
				t.Fatalf("the save finished while the row was locked: %v", err)
			default:
			}
			if tc.holderBump {
				_, err = holder.Exec(ctx, `update catalog.servers set version = version + 1 where slug = 'gitea'`)
				mustOK(t, err)
			}
			mustOK(t, holder.Commit(ctx))
			err = <-result
			if tc.wantErr {
				wantCode(t, err, domain.CodeServerConflict, "")
			} else {
				mustOK(t, err)
				if load(t, r, "gitea").Description() != "mine" {
					t.Fatal("the save was not applied")
				}
			}
		})
	}
}

func TestReplaceSealedSecretRacingASaveThatClearsItNeverBringsTheOldOneBack(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []string{"replace first", "save first", "together"} {
		t.Run(tc, func(t *testing.T) {
			_, r := setup(t)
			s := newServer(t, "yt", withAuth(sharedAuth))
			mustOK(t, s.SetSealedSecret([]byte{1}, t0))
			create(t, r, s)
			stale := load(t, r, "yt")
			stale.ClearSecret(t1)
			replace := func() { _, _ = r.Servers.ReplaceSealedSecret(ctx, "yt", []byte{1}, []byte{9}) }
			switch tc {
			case "replace first":
				replace()
				mustOK(t, save(t, r, stale))
			case "save first":
				mustOK(t, save(t, r, stale))
				replace()
			default:
				gate(replace, func() { mustOK(t, save(t, r, stale)) })
			}
			if got := load(t, r, "yt"); got.SecretSet() {
				t.Fatalf("secret %v is back", got.SealedSecret())
			}
		})
	}
}

// ---- tools ----

func snap(name string) domain.ToolSnapshot {
	title := "T " + name
	yes := true
	return domain.ToolSnapshot{
		Name: name, Title: &title, Description: "does " + name,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"number"}}}`),
		Annotations: domain.ToolAnnotations{ReadOnlyHint: &yes},
	}
}

func mustTool(t *testing.T, r Repos, server, name string) *domain.Tool {
	t.Helper()
	tool, err := r.Tools.Find(context.Background(), server, name)
	if err != nil || tool == nil {
		t.Fatalf("Find(%s, %s) = %v, %v", server, name, tool, err)
	}
	return tool
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	d := json.NewDecoder(bytes.NewReader(a))
	d.UseNumber()
	if d.Decode(&x) != nil {
		return false
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func TestToolSnapshotRoundTrips(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	clearOutbox(t, pool)
	res, err := r.Tools.ApplySnapshot(ctx, "gitea", []domain.ToolSnapshot{snap("issues.create"), snap("list")}, t0)
	mustOK(t, err)
	want := app.SnapshotResult{
		ChangedRefs: []string{"gitea.issues.create", "gitea.list"}, AddedRefs: []string{"gitea.issues.create", "gitea.list"}, RemovedRefs: []string{},
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("%+v", res)
	}
	tool := mustTool(t, r, "gitea", "issues.create")
	if tool.Ref() != "gitea.issues.create" || tool.Title() == nil || *tool.Title() != "T issues.create" || tool.OutputSchema() != nil ||
		tool.Annotations().ReadOnlyHint == nil || !*tool.Annotations().ReadOnlyHint || tool.Hidden() || tool.RemovedAt() != nil ||
		!tool.SyncedAt().Equal(t0) || !jsonEqual(tool.InputSchema(), snap("x").InputSchema) {
		t.Fatalf("%+v", tool)
	}
	list, err := r.Tools.ListByServer(ctx, "gitea")
	mustOK(t, err)
	if len(list) != 2 || list[0].Name() != "issues.create" || list[1].Name() != "list" {
		t.Fatalf("%v", list)
	}
	if nope, err := r.Tools.Find(ctx, "gitea", "zz"); nope != nil || err != nil {
		t.Fatalf("%v %v", nope, err)
	}
	wantEv := []outboxRow{{"catalog.tools_changed", map[string]any{"server": "gitea", "refs": []any{"gitea.issues.create", "gitea.list"}}}}
	if got := outboxRows(t, pool); !reflect.DeepEqual(got, wantEv) {
		t.Fatalf("%+v", got)
	}
}

func TestToolJSONIsKeptWithoutLoss(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	no, yes := false, true
	empty := ""
	in := domain.ToolSnapshot{
		Name: "big", Title: &empty, Description: "",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","maximum":12345678901234567890123,"minimum":-0.1000000000000000000001,"default":[1,2,{"x":null}]},` +
			`"nested":{"type":"object","properties":{"deep":{"anyOf":[{"const":"éé \" \\ /"},{"const":true},{"const":null}]}}}},"required":["id"],"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"const":9007199254740993}}}`),
		Annotations:  domain.ToolAnnotations{ReadOnlyHint: &no, DestructiveHint: &yes},
	}
	bare := domain.ToolSnapshot{Name: "bare", InputSchema: json.RawMessage(`{}`)}
	_, err := r.Tools.ApplySnapshot(ctx, "gitea", []domain.ToolSnapshot{in, bare}, t0)
	mustOK(t, err)

	got := mustTool(t, r, "gitea", "big")
	if !jsonEqual(got.InputSchema(), in.InputSchema) || !jsonEqual(got.OutputSchema(), in.OutputSchema) {
		t.Fatalf("schemas changed:\n%s\n%s", got.InputSchema(), got.OutputSchema())
	}
	if !strings.Contains(string(got.InputSchema()), "12345678901234567890123") || !strings.Contains(string(got.OutputSchema()), "9007199254740993") ||
		!strings.Contains(string(got.InputSchema()), "-0.1000000000000000000001") {
		t.Fatalf("a number lost precision: %s", got.InputSchema())
	}
	a := got.Annotations()
	if a.ReadOnlyHint == nil || *a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint || a.IdempotentHint != nil || a.OpenWorldHint != nil {
		t.Fatalf("annotations %+v: an explicit false must stay false and an absent hint absent", a)
	}
	if got.Title() == nil || *got.Title() != "" || got.Description() != "" {
		t.Fatal("an empty title must not become absent")
	}
	b := mustTool(t, r, "gitea", "bare")
	if b.Title() != nil || b.OutputSchema() != nil || b.Annotations() != (domain.ToolAnnotations{}) || string(b.InputSchema()) != "{}" {
		t.Fatalf("%+v", b)
	}
	var out *[]byte
	var ann []byte
	mustOK(t, pool.QueryRow(ctx, `select output_schema, annotations from catalog.tools where name = 'bare'`).Scan(&out, &ann))
	if out != nil || string(ann) != "{}" {
		t.Fatalf("absent output schema must be SQL NULL and annotations {}: %v %s", out, ann)
	}
}

func TestASecondIdenticalSnapshotIsANoop(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	_, err := r.Tools.ApplySnapshot(ctx, "gitea", []domain.ToolSnapshot{snap("a")}, t0)
	mustOK(t, err)
	clearOutbox(t, pool)
	res, err := r.Tools.ApplySnapshot(ctx, "gitea", []domain.ToolSnapshot{snap("a")}, t1)
	mustOK(t, err)
	if len(res.ChangedRefs)+len(res.AddedRefs)+len(res.RemovedRefs) != 0 || len(outboxRows(t, pool)) != 0 || !mustTool(t, r, "gitea", "a").SyncedAt().Equal(t0) {
		t.Fatalf("%+v", res)
	}
}

func TestHiddenSurvivesSyncRemovalAndReturn(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	_, err := r.Tools.ApplySnapshot(ctx, "gitea", []domain.ToolSnapshot{snap("a"), snap("b")}, t0)
	mustOK(t, err)
	clearOutbox(t, pool)
	a := mustTool(t, r, "gitea", "a")
	changed, err := a.SetHidden(true, t0)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	mustOK(t, r.Tools.Save(ctx, a, a.PullEvents()))
	if got := outboxTypes(t, pool); !slices.Equal(got, []string{"catalog.tools_changed"}) {
		t.Fatalf("%v", got)
	}
	if !mustTool(t, r, "gitea", "a").Hidden() {
		t.Fatal("not hidden")
	}
	clearOutbox(t, pool)
	mustOK(t, r.Tools.Save(ctx, a, a.PullEvents())) // already hidden: nothing new
	if len(outboxRows(t, pool)) != 0 {
		t.Fatal("events for nothing")
	}

	changedA := snap("a")
	changedA.Description = "changed"
	_, err = r.Tools.ApplySnapshot(ctx, "gitea", []domain.ToolSnapshot{changedA}, t1)
	mustOK(t, err)
	if got := mustTool(t, r, "gitea", "a"); !got.Hidden() || got.Description() != "changed" {
		t.Fatalf("%+v", got)
	}
	if got := mustTool(t, r, "gitea", "b"); got.RemovedAt() == nil || !got.RemovedAt().Equal(t1) {
		t.Fatalf("%+v", got)
	}
	if got := outboxTypes(t, pool); !slices.Equal(got, []string{"catalog.tools_changed", "catalog.tools_removed"}) {
		t.Fatalf("%v", got)
	}
	_, err = r.Tools.ApplySnapshot(ctx, "gitea", []domain.ToolSnapshot{snap("a"), snap("b")}, t1)
	mustOK(t, err)
	if got := mustTool(t, r, "gitea", "b"); got.RemovedAt() != nil || got.Hidden() {
		t.Fatalf("%+v", got)
	}
	if !mustTool(t, r, "gitea", "a").Hidden() {
		t.Fatal("hidden lost on return")
	}
}

func TestToolSaveOfAGoneToolIsNotFound(t *testing.T) {
	_, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	ghost := domain.RehydrateTool(domain.ToolState{ToolSnapshot: snap("ghost"), Server: "gitea", SyncedAt: t0})
	wantCode(t, r.Tools.Save(context.Background(), ghost, nil), domain.CodeNotFound, `tool "gitea.ghost" not found`)
}

func TestApplySnapshotForAnUnknownServerIsNotFound(t *testing.T) {
	_, r := setup(t)
	_, err := r.Tools.ApplySnapshot(context.Background(), "nope", []domain.ToolSnapshot{snap("a")}, t0)
	wantCode(t, err, domain.CodeNotFound, `server "nope" not found`)
}

func TestSnapshotsOfOtherServersAreUntouched(t *testing.T) {
	ctx := context.Background()
	_, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	create(t, r, newServer(t, "other"))
	_, err := r.Tools.ApplySnapshot(ctx, "other", []domain.ToolSnapshot{snap("z")}, t0)
	mustOK(t, err)
	_, err = r.Tools.ApplySnapshot(ctx, "gitea", nil, t1)
	mustOK(t, err)
	if mustTool(t, r, "other", "z").RemovedAt() != nil {
		t.Fatal("another server's tool was touched")
	}
}

func TestConcurrentSnapshotsOfOneServerAreSerialised(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	mk := func(name, desc string) domain.ToolSnapshot { s := snap(name); s.Description = desc; return s }
	a := []domain.ToolSnapshot{mk("x", "x"), mk("y", "y")}
	b := []domain.ToolSnapshot{mk("y", "y changed"), mk("z", "z")}
	for round := range 10 {
		slug := "snap" + string(rune('a'+round))
		create(t, r, newServer(t, slug))
		clearOutbox(t, pool)
		gate(
			func() { _, err := r.Tools.ApplySnapshot(ctx, slug, a, t0); mustOK(t, err) },
			func() { _, err := r.Tools.ApplySnapshot(ctx, slug, b, t1); mustOK(t, err) },
		)
		list, err := r.Tools.ListByServer(ctx, slug)
		mustOK(t, err)
		state := map[string]string{}
		for _, tl := range list {
			state[tl.Name()] = tl.Description()
			if tl.RemovedAt() != nil {
				state[tl.Name()] += " (removed)"
			}
		}
		var wantState map[string]string
		var wantEvents []outboxRow
		ref := func(n ...string) []any {
			out := []any{}
			for _, x := range n {
				out = append(out, slug+"."+x)
			}
			return out
		}
		ev := func(typ string, n ...string) outboxRow {
			return outboxRow{typ, map[string]any{"server": slug, "refs": ref(n...)}}
		}
		if state["x"] == "x" { // A ran after B (A's set is the live one)
			wantState = map[string]string{"x": "x", "y": "y", "z": "z (removed)"}
			wantEvents = []outboxRow{ev("catalog.tools_changed", "y", "z"), ev("catalog.tools_changed", "x", "y"), ev("catalog.tools_removed", "z")}
		} else {
			wantState = map[string]string{"x": "x (removed)", "y": "y changed", "z": "z"}
			wantEvents = []outboxRow{ev("catalog.tools_changed", "x", "y"), ev("catalog.tools_changed", "y", "z"), ev("catalog.tools_removed", "x")}
		}
		if !reflect.DeepEqual(state, wantState) {
			t.Fatalf("round %d: state %v, want %v", round, state, wantState)
		}
		if got := outboxRows(t, pool); !reflect.DeepEqual(got, wantEvents) {
			t.Fatalf("round %d: events %+v\nwant %+v", round, got, wantEvents)
		}
	}
}

// ---- rows written by the TypeScript server ----

func TestRowsWrittenByTheTypeScriptServerAreRead(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	const uid = "11111111-1111-1111-1111-111111111111"
	insert := func(slug, auth, access string, secret []byte, daily any, status string, syncAt any, syncErr any, syncUser any) {
		exec(t, pool, `insert into catalog.servers (slug, name, description, url, auth, secret, access, timeout_ms, daily_limit, status,
          last_sync_at, last_sync_error, sync_user_id, created_at, updated_at, version)
        values ($1, 'N', '', 'https://u.example.com/mcp', $2::text::jsonb, $3, $4::text::jsonb, 120000, $5, $6, $7, $8, $9::uuid, $10, $10, 3)`,
			slug, auth, secret, access, daily, status, syncAt, syncErr, syncUser, t0)
	}
	insert("none", `{"kind":"none"}`, `{"kind":"all"}`, nil, nil, "active", nil, nil, nil)
	insert("shared", `{"kind":"shared_secret","header":"X-Key","template":"{secret}"}`, `{"kind":"groups","groups":["dev","ops"]}`,
		[]byte{0xde, 0xad}, 5, "disabled", t1, "boom", uid)
	insert("pat", `{"kind":"pat_user","header":"Authorization","template":"Token {secret}"}`, `{"kind":"all"}`, nil, nil, "active", nil, nil, nil)
	insert("oauth", `{"kind":"oauth_user","oauth":{"authorizeUrl":"https://a/authorize","tokenUrl":"https://a/token","clientId":"cid","scopes":["read"],"extraAuthorizeParams":{"zz":"1","a":"2","bb":"3"}},"header":"Authorization","template":"Bearer {secret}"}`,
		`{"kind":"all"}`, []byte{1}, nil, "active", nil, nil, nil)
	insert("oauth2", `{"kind":"oauth_user","oauth":{"authorizeUrl":"https://a/authorize","tokenUrl":"https://a/token","clientId":"cid","scopes":[],"revokeUrl":"https://a/revoke","extraAuthorizeParams":{}},"header":"Authorization","template":"Bearer {secret}"}`,
		`{"kind":"all"}`, nil, nil, "active", nil, nil, nil)

	got := load(t, r, "shared")
	if got.Version() != 3 || got.Auth().Kind != domain.AuthSharedSecret || got.Auth().Header != "X-Key" ||
		!got.Access().Equal(domain.Access{Kind: domain.AccessGroups, Groups: []string{"dev", "ops"}}) ||
		!bytes.Equal(got.SealedSecret(), []byte{0xde, 0xad}) || got.DailyLimit() == nil || *got.DailyLimit() != 5 ||
		got.Status() != domain.StatusDisabled || !got.LastSyncAt().Equal(t1) || *got.LastSyncError() != "boom" || *got.SyncUserID() != uid {
		t.Fatalf("%+v", got)
	}
	if n := load(t, r, "none"); n.Auth().Kind != domain.AuthNone || n.Access().Kind != domain.AccessAll || n.SecretSet() || n.DailyLimit() != nil {
		t.Fatalf("%+v", n)
	}
	if p := load(t, r, "pat").Auth(); p.Kind != domain.AuthPATUser || p.Template != "Token {secret}" || p.OAuth != nil {
		t.Fatalf("%+v", p)
	}
	o := load(t, r, "oauth").Auth().OAuth
	// jsonb key order: shorter keys first, then bytewise; TypeScript reads the same order
	wantParams := []domain.AuthorizeParam{{Key: "a", Value: "2"}, {Key: "bb", Value: "3"}, {Key: "zz", Value: "1"}}
	if o == nil || !reflect.DeepEqual(o.ExtraAuthorizeParams, wantParams) || o.RevokeURL != nil || !slices.Equal(o.Scopes, []string{"read"}) {
		t.Fatalf("%+v", o)
	}
	o2 := load(t, r, "oauth2").Auth().OAuth
	if o2.RevokeURL == nil || *o2.RevokeURL != "https://a/revoke" || o2.ExtraAuthorizeParams == nil || len(o2.ExtraAuthorizeParams) != 0 || len(o2.Scopes) != 0 {
		t.Fatalf("`{}` and absent must stay apart: %+v", o2)
	}

	// tools: every optional field absent / everything set / removed / hidden
	exec(t, pool, `insert into catalog.tools (server, name, title, description, input_schema, output_schema, annotations, hidden, removed_at, synced_at) values
      ('none', 'bare', null, '', '{}', null, '{}', false, null, $1),
      ('none', 'full', 'Full', 'does it', '{"type":"object","properties":{"n":{"maximum":12345678901234567890}}}', '{"type":"object"}',
         '{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false}', false, null, $1),
      ('none', 'gone', 'Gone', 'was here', '{"type":"object"}', null, '{}', false, $2, $1),
      ('none', 'secret', null, 'hidden one', '{"type":"object"}', null, '{"readOnlyHint":true}', true, null, $1),
      ('none', 'nulls', null, 'jsonb null', '{"type":"object"}', 'null'::jsonb, '{}', false, null, $1)`, t0, t1)
	tools, err := r.Tools.ListByServer(ctx, "none")
	mustOK(t, err)
	names := []string{}
	for _, tl := range tools {
		names = append(names, tl.Name())
	}
	if !slices.Equal(names, []string{"bare", "full", "gone", "nulls", "secret"}) {
		t.Fatalf("%v", names)
	}
	bare := mustTool(t, r, "none", "bare")
	if bare.Title() != nil || bare.Description() != "" || bare.OutputSchema() != nil || bare.Annotations() != (domain.ToolAnnotations{}) {
		t.Fatalf("%+v", bare)
	}
	full := mustTool(t, r, "none", "full")
	fa := full.Annotations()
	if *full.Title() != "Full" || fa.ReadOnlyHint == nil || !*fa.ReadOnlyHint || fa.DestructiveHint == nil || *fa.DestructiveHint ||
		!strings.Contains(string(full.InputSchema()), "12345678901234567890") || !jsonEqual(full.OutputSchema(), []byte(`{"type":"object"}`)) {
		t.Fatalf("%+v", full)
	}
	if g := mustTool(t, r, "none", "gone"); g.RemovedAt() == nil || !g.RemovedAt().Equal(t1) || g.Visible() {
		t.Fatalf("%+v", g)
	}
	if h := mustTool(t, r, "none", "secret"); !h.Hidden() || h.Visible() {
		t.Fatalf("%+v", h)
	}
	if n := mustTool(t, r, "none", "nulls"); n.OutputSchema() != nil {
		t.Fatalf("a jsonb null output schema is absent: %s", n.OutputSchema())
	}
	list, err := r.Servers.List(ctx)
	mustOK(t, err)
	counts := map[string]int{}
	for _, l := range list {
		counts[l.Server.Slug()] = l.ToolCount
	}
	if counts["none"] != 3 { // bare, full, nulls: not removed, not hidden
		t.Fatalf("%v", counts)
	}
}

func TestWhatWeWriteHasTheKeysTypeScriptReads(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	revoke := "https://git.example.com/revoke"
	full := oauthAuth.Clone()
	full.OAuth.RevokeURL = &revoke
	for slug, a := range map[string]domain.AuthMode{"none": {Kind: domain.AuthNone}, "shared": sharedAuth, "oauth": oauthAuth, "full": full} {
		create(t, r, newServer(t, slug, withAuth(a)))
	}
	create(t, r, newServer(t, "open", func(p *domain.NewServerParams) { p.Access = domain.Access{Kind: domain.AccessAll} }))
	read := func(slug, col string) any {
		var raw []byte
		mustOK(t, pool.QueryRow(ctx, `select `+col+` from catalog.servers where slug = $1`, slug).Scan(&raw))
		var v any
		mustOK(t, json.Unmarshal(raw, &v))
		return v
	}
	parse := func(s string) any { var v any; mustOK(t, json.Unmarshal([]byte(s), &v)); return v }
	for slug, want := range map[string]string{
		"none":   `{"kind":"none"}`,
		"shared": `{"kind":"shared_secret","header":"X-Key","template":"{secret}"}`,
		"oauth":  `{"kind":"oauth_user","oauth":{"authorizeUrl":"https://git.example.com/authorize","tokenUrl":"https://git.example.com/token","clientId":"cid","scopes":["read","write"],"extraAuthorizeParams":{"access_type":"offline"}},"header":"Authorization","template":"Bearer {secret}"}`,
		"full":   `{"kind":"oauth_user","oauth":{"authorizeUrl":"https://git.example.com/authorize","tokenUrl":"https://git.example.com/token","clientId":"cid","scopes":["read","write"],"revokeUrl":"https://git.example.com/revoke","extraAuthorizeParams":{"access_type":"offline"}},"header":"Authorization","template":"Bearer {secret}"}`,
	} {
		if got := read(slug, "auth"); !reflect.DeepEqual(got, parse(want)) {
			t.Fatalf("%s: auth = %v", slug, got)
		}
	}
	if got := read("open", "access"); !reflect.DeepEqual(got, parse(`{"kind":"all"}`)) {
		t.Fatalf("%v", got)
	}
	if got := read("shared", "access"); !reflect.DeepEqual(got, parse(`{"kind":"groups","groups":["dev"]}`)) {
		t.Fatalf("%v", got)
	}
	// tools: annotations holds only the present hints; an absent output schema is SQL NULL
	_, err := r.Tools.ApplySnapshot(ctx, "none", []domain.ToolSnapshot{snap("a")}, t0)
	mustOK(t, err)
	var ann []byte
	var out []byte
	mustOK(t, pool.QueryRow(ctx, `select annotations, output_schema from catalog.tools`).Scan(&ann, &out))
	if !reflect.DeepEqual(parse(string(ann)), parse(`{"readOnlyHint":true}`)) || out != nil {
		t.Fatalf("%s %v", ann, out)
	}
}

func TestExtraParamsRoundTripInJSONBOrder(t *testing.T) {
	_, r := setup(t)
	a := oauthAuth.Clone()
	a.OAuth.ExtraAuthorizeParams = []domain.AuthorizeParam{{Key: "zz", Value: "1"}, {Key: "a", Value: "2"}, {Key: "bb", Value: "3"}}
	create(t, r, newServer(t, "gg", withAuth(a)))
	got := load(t, r, "gg").Auth().OAuth.ExtraAuthorizeParams
	want := []domain.AuthorizeParam{{Key: "a", Value: "2"}, {Key: "bb", Value: "3"}, {Key: "zz", Value: "1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestAMalformedStoredRowIsAnErrorNotAPanic(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	exec(t, pool, `insert into catalog.servers (slug, name, url, auth, access, timeout_ms, status, created_at, updated_at)
      values ('bad', 'N', 'https://u', '{"kind":"mystery"}', '{"kind":"all"}', 1000, 'active', $1, $1)`, t0)
	if s, err := r.Servers.FindBySlug(ctx, "bad"); err == nil || s != nil || code(err) != "" {
		t.Fatalf("%v %v", s, err)
	}
	if _, err := r.Servers.List(ctx); err == nil {
		t.Fatal("List must report the unreadable row")
	}
}

var _ = pgx.ErrNoRows

func TestAHideCommittedDuringASnapshotSurvivesIt(t *testing.T) {
	ctx := context.Background()
	pool, r := setup(t)
	create(t, r, newServer(t, "gitea"))
	_, err := r.Tools.ApplySnapshot(ctx, "gitea", []domain.ToolSnapshot{snap("x")}, t0)
	mustOK(t, err)

	// Another transaction hides x and stays open: the snapshot reads x as visible (no lock on
	// reads), then its upsert waits for the row.
	hider, err := pool.Begin(ctx)
	mustOK(t, err)
	defer func() { _ = hider.Rollback(context.WithoutCancel(ctx)) }()
	_, err = hider.Exec(ctx, `update catalog.tools set hidden = true where server = 'gitea' and name = 'x'`)
	mustOK(t, err)

	changed := snap("x")
	changed.Description = "changed by the snapshot"
	result := make(chan error, 1)
	go func() {
		_, err := r.Tools.ApplySnapshot(ctx, "gitea", []domain.ToolSnapshot{changed}, t1)
		result <- err
	}()
	eventually(t, "the snapshot to block on the tool row", func() bool { return blockedBackends(pool) >= 1 })
	mustOK(t, hider.Commit(ctx))
	mustOK(t, <-result)

	got := mustTool(t, r, "gitea", "x")
	if !got.Hidden() || got.Description() != "changed by the snapshot" {
		t.Fatalf("hidden %v, description %q: the hide was lost or the snapshot not applied", got.Hidden(), got.Description())
	}
}
