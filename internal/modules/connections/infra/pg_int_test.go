//go:build integration

package infra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA = "11111111-1111-1111-1111-111111111111"
	userB = "22222222-2222-2222-2222-222222222222"
	userC = "33333333-3333-3333-3333-333333333333"
)

var (
	t0 = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	fpOAuth = domain.ServerFingerprint(domain.ServerConfigOrigin{
		URL: "https://mcp.example.com/gitea", AuthKind: "oauth_user",
		OAuth: &domain.OAuthEndpoints{AuthorizeURL: "https://git.example.com/a", TokenURL: "https://git.example.com/t", ClientID: "cid"}, //nolint:gosec // G101: test data, no real credential
	})
)

func setup(t *testing.T) (*pgxpool.Pool, Stores) {
	t.Helper()
	pool := pgtest.StartMigrated(t)
	return pool, NewPg(pool)
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func oauthConn(user, server string, now time.Time) *domain.Connection {
	rev := "https://git.example.com/revoke"
	exp := now.Add(time.Hour)
	return domain.ConnectOAuth(domain.OAuthConnect{
		UserID: user, Server: server, IssuedFor: fpOAuth, RevokeURL: &rev, Now: now,
		Tokens: domain.SealedTokens{Access: domain.Sealed("sealed-access"), Refresh: domain.Sealed("sealed-refresh"), ExpiresAt: &exp},
	})
}

func save(t *testing.T, s Stores, c *domain.Connection) {
	t.Helper()
	err := s.Connections.WithLock(ctxT(t), c.UserID(), c.Server(), func(_ context.Context, _ *domain.Connection) (*domain.Connection, error) { return c, nil })
	if err != nil {
		t.Fatal(err)
	}
}

// Rows written by hand exactly as the TypeScript server writes them (the upsert of
// pg-connections.ts with a Buffer, a Date with milliseconds, nulls) rehydrate.
func TestRowsWrittenLikeTypeScriptRehydrate(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	ins := `insert into connections.connections (user_id, server, kind, access, refresh, expires_at, status, issued_for, revoke_url, updated_at)
	  values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	exp := "2026-11-22 11:00:00.123+00"
	for _, r := range []struct {
		server, kind, status string
		refresh              any
		expires, revoke      any
	}{
		{"oauth-full", "oauth", "ready", []byte("r"), exp, "https://git.example.com/revoke"},
		{"oauth-bare", "oauth", "expired", nil, nil, nil},
		{"oauth-no-revoke", "oauth", "ready", []byte("r"), exp, nil},
		{"pat", "pat", "ready", nil, nil, nil},
	} {
		if _, err := pool.Exec(ctx, ins, userA, r.server, r.kind, []byte("a-"+r.server), r.refresh, r.expires, r.status, fpOAuth, r.revoke, "2026-11-22 10:00:00.456+00"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		server           string
		kind             domain.Kind
		status           domain.Status
		refresh, expires bool
		revoke           bool
	}{
		{"oauth-full", domain.KindOAuth, domain.StatusReady, true, true, true},
		{"oauth-bare", domain.KindOAuth, domain.StatusExpired, false, false, false},
		{"oauth-no-revoke", domain.KindOAuth, domain.StatusReady, true, true, false},
		{"pat", domain.KindPAT, domain.StatusReady, false, false, false},
	} {
		c, err := s.Connections.Get(ctx, userA, tc.server)
		if err != nil || c == nil {
			t.Fatalf("%s: %v %v", tc.server, c, err)
		}
		snap := c.Snapshot()
		if snap.Kind != tc.kind || snap.Status != tc.status || snap.UserID != userA || snap.Server != tc.server || snap.IssuedFor != fpOAuth {
			t.Errorf("%s: %+v", tc.server, snap)
		}
		if !bytes.Equal(snap.Access, []byte("a-"+tc.server)) {
			t.Errorf("%s: access %q", tc.server, []byte(snap.Access))
		}
		if (snap.Refresh != nil) != tc.refresh || (snap.ExpiresAt != nil) != tc.expires || (snap.RevokeURL != nil) != tc.revoke {
			t.Errorf("%s: refresh %v expires %v revoke %v", tc.server, snap.Refresh != nil, snap.ExpiresAt != nil, snap.RevokeURL != nil)
		}
		if want := time.Date(2026, 11, 22, 10, 0, 0, 456e6, time.UTC); !snap.UpdatedAt.Equal(want) {
			t.Errorf("%s: updatedAt %v", tc.server, snap.UpdatedAt)
		}
		if tc.expires && !snap.ExpiresAt.Equal(time.Date(2026, 11, 22, 11, 0, 0, 123e6, time.UTC)) {
			t.Errorf("%s: expiresAt %v", tc.server, snap.ExpiresAt)
		}
	}
	// An empty bytea (not written by either server) reads as "no refresh token".
	if _, err := pool.Exec(ctx, ins, userA, "empty-bytea", "oauth", []byte("a"), []byte{}, nil, "ready", fpOAuth, nil, "2026-11-22 10:00:00+00"); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Connections.Get(ctx, userA, "empty-bytea")
	if c.RefreshToken() != nil {
		t.Error("an empty bytea must read as no refresh token")
	}
}

// Rows written by Go have the column values TypeScript would write.
func TestRowsWrittenByGoHaveTheReferencesColumnValues(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	now := time.Date(2026, 11, 22, 10, 0, 0, 123456789, time.UTC) // nanoseconds: the reference has milliseconds
	save(t, s, oauthConn(userA, "gitea", now))
	pat := domain.ConnectPAT(domain.PATConnect{UserID: userA, Server: "yt", Token: domain.Sealed("sealed-pat"), IssuedFor: `["http://h/?a=1&b=<2>","pat_user",null,null,null]`, Now: now})
	save(t, s, pat)

	type row struct {
		kind, status, issuedFor string
		access, refresh         []byte
		expires                 *time.Time
		revoke                  *string
		updated                 time.Time
		updatedMicros           int64
	}
	read := func(server string) row {
		var r row
		if err := pool.QueryRow(ctx, `select kind, status, issued_for, access, refresh, expires_at, revoke_url, updated_at,
		   (extract(epoch from updated_at) * 1000000)::bigint from connections.connections where user_id = $1 and server = $2`, userA, server).
			Scan(&r.kind, &r.status, &r.issuedFor, &r.access, &r.refresh, &r.expires, &r.revoke, &r.updated, &r.updatedMicros); err != nil {
			t.Fatal(err)
		}
		return r
	}
	o := read("gitea")
	if o.kind != "oauth" || o.status != "ready" || o.issuedFor != fpOAuth || string(o.access) != "sealed-access" || string(o.refresh) != "sealed-refresh" {
		t.Errorf("oauth row: %+v", o)
	}
	if o.expires == nil || !o.expires.Equal(now.Add(time.Hour).Truncate(time.Millisecond)) || o.revoke == nil || *o.revoke != "https://git.example.com/revoke" {
		t.Errorf("oauth expiry/revoke: %v %v", o.expires, o.revoke)
	}
	if o.updatedMicros%1000 != 0 || !o.updated.Equal(now.Truncate(time.Millisecond)) {
		t.Errorf("updated_at kept sub-millisecond digits: %d", o.updatedMicros)
	}
	p := read("yt")
	if p.kind != "pat" || p.status != "ready" || p.refresh != nil || p.expires != nil || p.revoke != nil {
		t.Errorf("pat row: kind %s status %s refresh %v expires %v revoke %v", p.kind, p.status, p.refresh, p.expires, p.revoke)
	}
	if p.issuedFor != `["http://h/?a=1&b=<2>","pat_user",null,null,null]` {
		t.Errorf("issued_for changed on the way: %s", p.issuedFor)
	}
	var nullRefresh bool
	if err := pool.QueryRow(ctx, `select refresh is null from connections.connections where user_id = $1 and server = 'yt'`, userA).Scan(&nullRefresh); err != nil || !nullRefresh {
		t.Errorf("a missing refresh token is NULL, not an empty bytea: %v %v", nullRefresh, err)
	}
	// A connection read back and written again is the same row.
	got, _ := s.Connections.Get(ctx, userA, "gitea")
	if got.UpdatedAt().Nanosecond() != 123000000 || got.UpdatedAt().Location() != time.UTC {
		t.Errorf("updatedAt %v", got.UpdatedAt())
	}
	if !domain.Restore(got.Snapshot()).UpdatedAt().Equal(got.UpdatedAt()) {
		t.Error("restore changed updatedAt")
	}
}

func TestGetListDeleteKeys(t *testing.T) {
	_, s := setup(t)
	ctx := ctxT(t)
	if c, err := s.Connections.Get(ctx, userA, "x"); c != nil || err != nil {
		t.Fatalf("missing: %v %v", c, err)
	}
	if c, err := s.Connections.Delete(ctx, userA, "x"); c != nil || err != nil {
		t.Fatalf("delete missing: %v %v", c, err)
	}
	for _, k := range [][2]string{{userB, "z"}, {userA, "b"}, {userA, "a"}, {userB, "a"}} {
		save(t, s, oauthConn(k[0], k[1], t0))
	}
	list, err := s.Connections.ListForUser(ctx, userA)
	if err != nil || len(list) != 2 || list[0].Server() != "a" || list[1].Server() != "b" {
		t.Fatalf("list: %v %v", list, err)
	}
	if empty, err := s.Connections.ListForUser(ctx, userC); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("a person with none: %#v %v", empty, err)
	}
	keys, err := s.Connections.ListKeys(ctx)
	want := []app.ConnectionKey{{UserID: userA, Server: "a"}, {UserID: userA, Server: "b"}, {UserID: userB, Server: "a"}, {UserID: userB, Server: "z"}}
	if err != nil || fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("keys: %v %v", keys, err)
	}
	old, err := s.Connections.Delete(ctx, userA, "a")
	if err != nil || old == nil || string(old.AccessToken()) != "sealed-access" || old.Server() != "a" {
		t.Fatalf("delete returns the old row: %v %v", old, err)
	}
	if c, _ := s.Connections.Get(ctx, userA, "a"); c != nil {
		t.Fatal("still there")
	}
}

func TestWithLockSeesTheRowAndAppliesWhatTheCallbackReturns(t *testing.T) {
	_, s := setup(t)
	ctx := ctxT(t)
	var seen *domain.Connection
	// absent row: the callback sees nil, a returned connection is an insert
	err := s.Connections.WithLock(ctx, userA, "gitea", func(_ context.Context, cur *domain.Connection) (*domain.Connection, error) {
		seen = cur
		return oauthConn(userA, "gitea", t0), nil
	})
	if err != nil || seen != nil {
		t.Fatalf("absent: %v %v", seen, err)
	}
	// present row: seen as stored; nil writes nothing
	err = s.Connections.WithLock(ctx, userA, "gitea", func(_ context.Context, cur *domain.Connection) (*domain.Connection, error) {
		seen = cur
		return nil, nil
	})
	if err != nil || seen == nil || string(seen.AccessToken()) != "sealed-access" {
		t.Fatalf("present: %v %v", seen, err)
	}
	// update through the lock
	err = s.Connections.WithLock(ctx, userA, "gitea", func(_ context.Context, cur *domain.Connection) (*domain.Connection, error) {
		cur.Expire(t0.Add(time.Minute))
		return cur, nil
	})
	got, _ := s.Connections.Get(ctx, userA, "gitea")
	if err != nil || got.Status() != domain.StatusExpired || !got.UpdatedAt().Equal(t0.Add(time.Minute)) {
		t.Fatalf("update: %v %v", got, err)
	}
}

func TestWithLockRollsBackOnErrorPanicAndWrongKey(t *testing.T) {
	_, s := setup(t)
	ctx := ctxT(t)
	save(t, s, oauthConn(userA, "gitea", t0))
	expire := func(cur *domain.Connection) *domain.Connection { cur.Expire(t0.Add(time.Hour)); return cur }
	boom := errors.New("boom")
	err := s.Connections.WithLock(ctx, userA, "gitea", func(_ context.Context, cur *domain.Connection) (*domain.Connection, error) {
		return expire(cur), boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed")
			}
		}()
		_ = s.Connections.WithLock(ctx, userA, "gitea", func(context.Context, *domain.Connection) (*domain.Connection, error) {
			panic("kaboom")
		})
	}()
	err = s.Connections.WithLock(ctx, userA, "gitea", func(context.Context, *domain.Connection) (*domain.Connection, error) {
		return oauthConn(userA, "other", t0), nil // not the locked key
	})
	if err == nil {
		t.Error("a write for another key must be refused")
	}
	if c, _ := s.Connections.Get(ctx, userA, "other"); c != nil {
		t.Error("the foreign write happened")
	}
	if c, _ := s.Connections.Get(ctx, userA, "gitea"); c.Status() != domain.StatusReady {
		t.Error("a rolled-back change is visible")
	}
	// The panic released the lock: another call is not blocked.
	if err := s.Connections.WithLock(ctx, userA, "gitea", func(context.Context, *domain.Connection) (*domain.Connection, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

// A caller that gives up (its context ends) after the callback returned must not lose the write:
// the provider may have rotated a one-time refresh token.
func TestWithLockCommitsAfterTheCallersContextEnded(t *testing.T) {
	_, s := setup(t)
	save(t, s, oauthConn(userA, "gitea", t0))
	ctx, cancel := context.WithCancel(ctxT(t))
	err := s.Connections.WithLock(ctx, userA, "gitea", func(_ context.Context, cur *domain.Connection) (*domain.Connection, error) {
		cur.Expire(t0.Add(time.Hour))
		cancel() // the caller's bound fires after the provider answered, before commit
		return cur, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	got, _ := s.Connections.Get(ctxT(t), userA, "gitea")
	if got.Status() != domain.StatusExpired {
		t.Fatal("the write was rolled back")
	}
}

func TestWithLockHonoursACancelledContextBeforeTheLock(t *testing.T) {
	_, s := setup(t)
	ctx, cancel := context.WithCancel(ctxT(t))
	cancel()
	called := false
	err := s.Connections.WithLock(ctx, userA, "gitea", func(context.Context, *domain.Connection) (*domain.Connection, error) {
		called = true
		return nil, nil
	})
	if err == nil || called {
		t.Fatalf("err %v called %v", err, called)
	}
}

func blockedBy(ctx context.Context, t *testing.T, pool *pgxpool.Pool, wantWaiting int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		err := pool.QueryRow(ctx, `select count(*)::int from pg_stat_activity
		  where datname = current_database() and cardinality(pg_blocking_pids(pid)) > 0`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n >= wantWaiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend is blocked (want %d)", wantWaiting)
		}
		time.Sleep(10 * time.Millisecond) // polling the server's own view of the lock: there is no event to wait on
	}
}

// Two goroutines on separate pooled connections, one key: the second blocks until the first
// commits (seen through pg_blocking_pids) and then sees the first one's write.
func TestWithLockSerialisesTheSameKeyAcrossConnections(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	save(t, s, oauthConn(userA, "gitea", t0))

	inside := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- s.Connections.WithLock(ctx, userA, "gitea", func(_ context.Context, cur *domain.Connection) (*domain.Connection, error) {
			close(inside)
			<-release
			cur.Expire(t0.Add(time.Minute))
			return cur, nil
		})
	}()
	<-inside
	var second *domain.Connection
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- s.Connections.WithLock(ctx, userA, "gitea", func(_ context.Context, cur *domain.Connection) (*domain.Connection, error) {
			second = cur
			return nil, nil
		})
	}()
	blockedBy(ctx, t, pool, 1)
	select {
	case err := <-secondDone:
		t.Fatalf("the second call ran while the first held the lock: %v", err)
	default:
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if second == nil || second.Status() != domain.StatusExpired {
		t.Fatalf("the second call must see the first one's write, saw %v", second)
	}
}

func TestWithLockDifferentKeysDoNotBlock(t *testing.T) {
	_, s := setup(t)
	ctx := ctxT(t)
	save(t, s, oauthConn(userA, "gitea", t0))
	save(t, s, oauthConn(userB, "gitea", t0))
	save(t, s, oauthConn(userA, "yt", t0))
	inside := make(chan struct{})
	release := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- s.Connections.WithLock(ctx, userA, "gitea", func(context.Context, *domain.Connection) (*domain.Connection, error) {
			close(inside)
			<-release
			return nil, nil
		})
	}()
	<-inside
	for _, k := range [][2]string{{userB, "gitea"}, {userA, "yt"}, {userC, "gitea"}} {
		done := make(chan error, 1)
		go func() {
			done <- s.Connections.WithLock(ctx, k[0], k[1], func(context.Context, *domain.Connection) (*domain.Connection, error) { return nil, nil })
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("%v is blocked by an unrelated lock", k)
		}
	}
	// reads are not blocked either
	if c, err := s.Connections.Get(ctx, userA, "gitea"); c == nil || err != nil {
		t.Fatalf("get under a lock: %v %v", c, err)
	}
	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
}

// Inserts of a missing key race on the primary key: the upsert makes every one succeed (no unique
// violation reaches the caller) and one row is left, as in the reference.
func TestWithLockOnAMissingKeyInsertsDoNotFail(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- s.Connections.WithLock(ctx, userA, "gitea", func(context.Context, *domain.Connection) (*domain.Connection, error) {
				return oauthConn(userA, "gitea", t0.Add(time.Duration(i)*time.Minute)), nil
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `select count(*) from connections.connections`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d rows", n)
	}
}

// The reference's pool trap: every renewal holds one pooled connection for its lock; a pool of 2
// with 3 concurrent renewals for different keys must finish, and so must a pool of 1.
func TestWithLockCompletesOnATinyPool(t *testing.T) {
	migrated := pgtest.StartMigrated(t)
	url := migrated.Config().ConnString()
	for _, size := range []int32{1, 2} {
		cfg, _ := pgxpool.ParseConfig(url)
		cfg.MaxConns = size
		small, err := pgxpool.NewWithConfig(ctxT(t), cfg)
		if err != nil {
			t.Fatal(err)
		}
		s := NewPg(small)
		users := []string{userA, userB, userC}
		var wg sync.WaitGroup
		var finished atomic.Int32
		errs := make(chan error, len(users))
		for _, u := range users {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- s.Connections.WithLock(ctxT(t), u, fmt.Sprintf("gitea-%d", size), func(context.Context, *domain.Connection) (*domain.Connection, error) {
					time.Sleep(50 * time.Millisecond) // the provider's answer takes a while
					finished.Add(1)
					return oauthConn(u, fmt.Sprintf("gitea-%d", size), t0), nil
				})
			}()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("pool of %d: the renewals did not finish (deadlock)", size)
		}
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("pool of %d: %v", size, err)
			}
		}
		if finished.Load() != 3 {
			t.Fatalf("pool of %d: %d finished", size, finished.Load())
		}
		small.Close()
	}
}

func pending(state, user string, expires time.Time) domain.PendingConnect {
	return domain.RestorePendingConnect(domain.PendingSnapshot{State: state, UserID: user, Server: "gitea", CodeVerifier: "verifier-" + state, IssuedFor: fpOAuth, ExpiresAt: expires})
}

func TestPendingInsertAndConsume(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.Pending.Insert(ctx, pending("st1", userA, now.Add(10*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := s.Pending.Insert(ctx, pending("st1", userA, now)); err == nil {
		t.Error("state is unique")
	} else if containsStr(err.Error(), "st1") || containsStr(fmt.Sprintf("%+v", err), "st1") {
		t.Errorf("error quotes the state: %v", err)
	}
	if p, err := s.Pending.Consume(ctx, "unknown", userA); p != nil || err != nil {
		t.Fatalf("unknown: %v %v", p, err)
	}
	p, err := s.Pending.Consume(ctx, "st1", userA)
	if err != nil || p == nil {
		t.Fatalf("consume: %v %v", p, err)
	}
	if p.State() != "st1" || p.UserID() != userA || p.Server() != "gitea" || p.CodeVerifier() != "verifier-st1" || p.IssuedFor() != fpOAuth || !p.ExpiresAt().Equal(now.Add(10*time.Minute)) {
		t.Errorf("row mismatch: %v", p)
	}
	var n int
	_ = pool.QueryRow(ctx, `select count(*) from connections.pending`).Scan(&n)
	if n != 0 {
		t.Fatal("the row is still there")
	}
	if again, _ := s.Pending.Consume(ctx, "st1", userA); again != nil {
		t.Fatal("a second consume got the attempt")
	}
}

func containsStr(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

// Rows written by hand as TypeScript writes them rehydrate.
func TestPendingRowWrittenLikeTypeScript(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	if _, err := pool.Exec(ctx, `insert into connections.pending (state, user_id, server, code_verifier, issued_for, expires_at)
	  values ('ts-state', $1, 'gitea', 'ts-verifier', $2, '2030-01-01 00:00:00.789+00')`, userA, fpOAuth); err != nil {
		t.Fatal(err)
	}
	p, err := s.Pending.Consume(ctx, "ts-state", userA)
	if err != nil || p == nil || p.CodeVerifier() != "ts-verifier" || !p.ExpiresAt().Equal(time.Date(2030, 1, 1, 0, 0, 0, 789e6, time.UTC)) {
		t.Fatalf("%v %v", p, err)
	}
}

func TestPendingConsumeOnlyTakesTheStartersAttempt(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	now := time.Now().UTC()
	_ = s.Pending.Insert(ctx, pending("live", userA, now.Add(10*time.Minute)))
	_ = s.Pending.Insert(ctx, pending("old", userA, now.Add(-time.Minute)))

	for _, state := range []string{"live", "old"} {
		if p, err := s.Pending.Consume(ctx, state, userB); p != nil || err != nil {
			t.Fatalf("another person got %s: %v %v", state, p, err)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `select count(*) from connections.pending`).Scan(&n)
	if n != 2 {
		t.Fatalf("another person's callback removed rows: %d left", n)
	}
	// the starter's expired attempt IS consumed and returned (the domain judges expiry)
	p, err := s.Pending.Consume(ctx, "old", userA)
	if err != nil || p == nil || !domain.IsPendingConnectExpired(p.ExpiresAt(), now) {
		t.Fatalf("expired own attempt: %v %v", p, err)
	}
	if again, _ := s.Pending.Consume(ctx, "old", userA); again != nil {
		t.Fatal("the expired attempt was not consumed")
	}
	// and the live one is still there for its starter
	if p, err := s.Pending.Consume(ctx, "live", userA); p == nil || err != nil {
		t.Fatalf("the starter lost the attempt: %v %v", p, err)
	}
}

func TestPendingConsumeRaces(t *testing.T) {
	_, s := setup(t)
	ctx := ctxT(t)
	for round := range 20 {
		state := fmt.Sprintf("race-%d", round)
		_ = s.Pending.Insert(ctx, pending(state, userA, time.Now().Add(10*time.Minute)))
		var wins atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if p, err := s.Pending.Consume(ctx, state, userA); err != nil {
					t.Error(err)
				} else if p != nil {
					wins.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("round %d: %d callbacks got the attempt", round, wins.Load())
		}
	}
	// the rightful one against another person: the other gets nothing, the rightful one succeeds
	for round := range 20 {
		state := fmt.Sprintf("duel-%d", round)
		_ = s.Pending.Insert(ctx, pending(state, userA, time.Now().Add(10*time.Minute)))
		var wg sync.WaitGroup
		var rightful, other *domain.PendingConnect
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; rightful, _ = s.Pending.Consume(ctx, state, userA) }()
		go func() { defer wg.Done(); <-start; other, _ = s.Pending.Consume(ctx, state, userB) }()
		close(start)
		wg.Wait()
		if other != nil || rightful == nil {
			t.Fatalf("round %d: other %v rightful %v", round, other, rightful)
		}
	}
}

func TestPendingInsertPrunesWhatExpiredMoreThanAnHourAgo(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	now := time.Now().UTC()
	_ = s.Pending.Insert(ctx, pending("ancient", userA, now.Add(-2*time.Hour)))
	// the insert above pruned nothing; this one does
	_ = s.Pending.Insert(ctx, pending("recent-expired", userA, now.Add(-30*time.Minute)))
	_ = s.Pending.Insert(ctx, pending("trigger", userB, now.Add(10*time.Minute)))
	var states []string
	rows, _ := pool.Query(ctx, `select state from connections.pending order by state`)
	for rows.Next() {
		var st string
		_ = rows.Scan(&st)
		states = append(states, st)
	}
	rows.Close()
	if fmt.Sprint(states) != "[recent-expired trigger]" {
		t.Fatalf("left: %v", states)
	}
}
