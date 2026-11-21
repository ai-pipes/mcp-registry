//go:build integration

package infra

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)

func setup(t *testing.T) (*pgxpool.Pool, Adapters) {
	t.Helper()
	pool := pgtest.StartMigrated(t)
	return pool, NewPg(pool, kernel.FixedClock{T: t0})
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func newUser(t *testing.T, a Adapters, sub string) domain.User {
	t.Helper()
	email := sub + "@example.com"
	u, err := a.Users.UpsertFromLogin(context.Background(), app.UpsertLogin{
		Claims: domain.OIDCClaims{Sub: sub, Name: "N " + sub, Email: &email, Groups: []string{"g1", "g2"}},
		Role:   domain.RoleMember, At: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

const noSuchID = "00000000-0000-4000-8000-000000000000"

func TestUsers(t *testing.T) {
	ctx := context.Background()
	_, a := setup(t)

	u := newUser(t, a, "sub-1")
	if len(u.ID) != 36 || u.ID != strings.ToLower(u.ID) {
		t.Fatalf("id = %q", u.ID)
	}
	if u.OIDCSub != "sub-1" || u.Name != "N sub-1" || u.Email == nil || *u.Email != "sub-1@example.com" ||
		u.Role != domain.RoleMember || u.Disabled || !u.CreatedAt.Equal(t0) ||
		u.LastLoginAt == nil || !u.LastLoginAt.Equal(t0) || len(u.Groups) != 2 {
		t.Fatalf("user = %+v", u)
	}
	if u.CreatedAt.Location() != time.UTC {
		t.Fatal("times must be UTC")
	}

	byID := must(a.Users.FindByID(ctx, u.ID))
	bySub := must(a.Users.FindBySub(ctx, "sub-1"))
	if byID == nil || bySub == nil || byID.ID != u.ID || bySub.ID != u.ID {
		t.Fatalf("lookups: %+v %+v", byID, bySub)
	}
	if v := must(a.Users.FindByID(ctx, noSuchID)); v != nil {
		t.Fatal("unknown id must be nil")
	}
	if v := must(a.Users.FindBySub(ctx, "nope")); v != nil {
		t.Fatal("unknown sub must be nil")
	}

	// a malformed id is "not found" without a query: it works even on a closed pool
	pool2 := pgtest.StartMigrated(t)
	pool2.Close()
	closed := NewPg(pool2, kernel.SystemClock{})
	if v, err := closed.Users.FindByID(ctx, "not-a-uuid"); v != nil || err != nil {
		t.Fatalf("FindByID(bad) = %v, %v", v, err)
	}
	if ok, err := closed.Users.SetDisabled(ctx, "not-a-uuid", true); ok || err != nil {
		t.Fatalf("SetDisabled(bad) = %v, %v", ok, err)
	}
	if err := closed.Users.TouchLastLogin(ctx, "../x", t0); err != nil {
		t.Fatalf("TouchLastLogin(bad) = %v", err)
	}
	if v, err := closed.Tokens.FindByID(ctx, "zzz"); v != nil || err != nil {
		t.Fatalf("Tokens.FindByID(bad) = %v, %v", v, err)
	}
	// ... and a well-formed one does hit the database
	if _, err := closed.Users.FindByID(ctx, noSuchID); err == nil {
		t.Fatal("a well-formed id must reach the database")
	}

	// upsert refreshes everything but disabled; created_at stays
	if ok := must(a.Users.SetDisabled(ctx, u.ID, true)); !ok {
		t.Fatal("SetDisabled existing = false")
	}
	later := t0.Add(time.Hour)
	u2, err := a.Users.UpsertFromLogin(ctx, app.UpsertLogin{
		Claims: domain.OIDCClaims{Sub: "sub-1", Name: "Renamed", Email: nil, Groups: nil},
		Role:   domain.RoleAdmin, At: later,
	})
	if err != nil {
		t.Fatal(err)
	}
	if u2.ID != u.ID || u2.Name != "Renamed" || u2.Email != nil || u2.Groups == nil || len(u2.Groups) != 0 ||
		u2.Role != domain.RoleAdmin || !u2.Disabled || !u2.CreatedAt.Equal(t0) || !u2.LastLoginAt.Equal(later) {
		t.Fatalf("after upsert = %+v", u2)
	}
	if ok := must(a.Users.SetDisabled(ctx, u.ID, false)); !ok {
		t.Fatal("re-enable")
	}
	if ok := must(a.Users.SetDisabled(ctx, noSuchID, true)); ok {
		t.Fatal("SetDisabled unknown = true")
	}

	if err := a.Users.TouchLastLogin(ctx, u.ID, later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := must(a.Users.FindByID(ctx, u.ID))
	if !got.LastLoginAt.Equal(later.Add(time.Minute)) || got.Name != "Renamed" || got.Role != domain.RoleAdmin {
		t.Fatalf("touch changed more than last login: %+v", got)
	}

	// list: creation time, then name
	b := newUserAt(t, a, "sub-b", "Bravo", t0.Add(time.Second))
	_ = newUserAt(t, a, "sub-a", "Alpha", t0.Add(time.Second))
	list := must(a.Users.List(ctx))
	var names []string
	for _, x := range list {
		names = append(names, x.Name)
	}
	if strings.Join(names, ",") != "Renamed,Alpha,Bravo" {
		t.Fatalf("order = %v", names)
	}
	_ = b
}

func newUserAt(t *testing.T, a Adapters, sub, name string, at time.Time) domain.User {
	t.Helper()
	u, err := a.Users.UpsertFromLogin(context.Background(), app.UpsertLogin{
		Claims: domain.OIDCClaims{Sub: sub, Name: name, Groups: []string{}}, Role: domain.RoleMember, At: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	_, a := setup(t)
	u := newUser(t, a, "tok-user")
	other := newUser(t, a, "tok-other")
	h := func(c string) string { return strings.Repeat(c, 64) }

	t1 := must(a.Tokens.Insert(ctx, app.NewToken{UserID: u.ID, Name: "one", Prefix: "sk_abcdefg", Hash: h("a"), CreatedAt: t0}))
	t2 := must(a.Tokens.Insert(ctx, app.NewToken{UserID: u.ID, Name: "two", Prefix: "sk_hijklmn", Hash: h("b"), CreatedAt: t0.Add(time.Second)}))
	t3 := must(a.Tokens.Insert(ctx, app.NewToken{UserID: other.ID, Name: "three", Prefix: "sk_x", Hash: h("c"), CreatedAt: t0}))
	if t1.ID == "" || t1.UserID != u.ID || t1.Name != "one" || t1.Hash != h("a") || !t1.CreatedAt.Equal(t0) ||
		t1.LastUsedAt != nil || t1.RevokedAt != nil {
		t.Fatalf("inserted = %+v", t1)
	}
	if _, err := a.Tokens.Insert(ctx, app.NewToken{UserID: u.ID, Name: "dup", Prefix: "p", Hash: h("a"), CreatedAt: t0}); err == nil {
		t.Fatal("duplicate hash must fail")
	}

	if g := must(a.Tokens.FindByHash(ctx, h("b"))); g == nil || g.ID != t2.ID || len(g.Hash) != 64 {
		t.Fatalf("FindByHash = %+v", g)
	}
	if g := must(a.Tokens.FindByHash(ctx, h("z"))); g != nil {
		t.Fatal("unknown hash")
	}
	if g := must(a.Tokens.FindByID(ctx, t1.ID)); g == nil || g.Name != "one" {
		t.Fatalf("FindByID = %+v", g)
	}
	if g := must(a.Tokens.FindByID(ctx, noSuchID)); g != nil {
		t.Fatal("unknown id")
	}

	list := must(a.Tokens.ListByUser(ctx, u.ID))
	if len(list) != 2 || list[0].ID != t2.ID || list[1].ID != t1.ID {
		t.Fatalf("list newest first: %+v", list)
	}
	if l := must(a.Tokens.ListByUser(ctx, noSuchID)); l == nil || len(l) != 0 {
		t.Fatalf("empty list = %#v", l)
	}

	if err := a.Tokens.Touch(ctx, t1.ID, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if g := must(a.Tokens.FindByID(ctx, t1.ID)); g.LastUsedAt == nil || !g.LastUsedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("touch: %+v", g)
	}

	// revoke sets once; a second revoke keeps the first time
	if err := a.Tokens.Revoke(ctx, t1.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := a.Tokens.Revoke(ctx, t1.ID, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if g := must(a.Tokens.FindByID(ctx, t1.ID)); g.RevokedAt == nil || !g.RevokedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("revoke: %+v", g)
	}
	// revoke-all keeps earlier revocations and does not touch other people
	if err := a.Tokens.RevokeAllByUser(ctx, u.ID, t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if g := must(a.Tokens.FindByID(ctx, t1.ID)); !g.RevokedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("earlier revocation overwritten: %+v", g)
	}
	if g := must(a.Tokens.FindByID(ctx, t2.ID)); g.RevokedAt == nil || !g.RevokedAt.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("t2: %+v", g)
	}
	if g := must(a.Tokens.FindByID(ctx, t3.ID)); g.RevokedAt != nil {
		t.Fatalf("other person's token revoked: %+v", g)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	_, a := setup(t)
	u := newUser(t, a, "sess-user")
	o := newUser(t, a, "sess-other")
	h := func(c string) string { return strings.Repeat(c, 64) }
	rec := func(hash, uid string, created, seen, exp time.Time) app.SessionRecord {
		return app.SessionRecord{IDHash: hash, UserID: uid, CreatedAt: created, LastSeenAt: seen, ExpiresAt: exp}
	}
	s := rec(h("1"), u.ID, t0, t0, t0.Add(30*24*time.Hour))
	if err := a.Sessions.Insert(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := a.Sessions.Insert(ctx, s); err == nil {
		t.Fatal("duplicate id must fail")
	}
	got := must(a.Sessions.FindByHash(ctx, h("1")))
	if got == nil || *got != s {
		t.Fatalf("got %+v want %+v", got, s)
	}
	if g := must(a.Sessions.FindByHash(ctx, h("9"))); g != nil {
		t.Fatal("unknown")
	}
	if err := a.Sessions.Touch(ctx, h("1"), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if g := must(a.Sessions.FindByHash(ctx, h("1"))); !g.LastSeenAt.Equal(t0.Add(time.Minute)) || !g.CreatedAt.Equal(t0) {
		t.Fatalf("touch: %+v", g)
	}
	if err := a.Sessions.Delete(ctx, h("1")); err != nil {
		t.Fatal(err)
	}
	if g := must(a.Sessions.FindByHash(ctx, h("1"))); g != nil {
		t.Fatal("deleted")
	}

	// DeleteByUser
	for i, c := range []string{"2", "3"} {
		_ = i
		if err := a.Sessions.Insert(ctx, rec(h(c), u.ID, t0, t0, t0.Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Sessions.Insert(ctx, rec(h("4"), o.ID, t0, t0, t0.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := a.Sessions.DeleteByUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if must(a.Sessions.FindByHash(ctx, h("2"))) != nil || must(a.Sessions.FindByHash(ctx, h("4"))) == nil {
		t.Fatal("DeleteByUser scope")
	}

	// DeleteDead: expires_at <= cutoff or last_seen_at < cutoff
	now := t0.Add(100 * time.Hour)
	cut := domain.DeadSessionCutoffs(now)
	alive := rec(h("a"), o.ID, now.Add(-time.Hour), now.Add(-time.Hour), now.Add(time.Hour))
	expiredExactly := rec(h("b"), o.ID, t0, now.Add(-time.Hour), now) // expires_at == cutoff: dead
	idleJustOver := rec(h("c"), o.ID, t0, cut.LastSeenBefore.Add(-time.Microsecond), now.Add(time.Hour))
	idleExactly := rec(h("d"), o.ID, t0, cut.LastSeenBefore, now.Add(time.Hour)) // last_seen == cutoff: alive
	for _, r := range []app.SessionRecord{alive, expiredExactly, idleJustOver, idleExactly} {
		if err := a.Sessions.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	n, err := a.Sessions.DeleteDead(ctx, cut)
	if err != nil {
		t.Fatal(err)
	}
	// h("4") (expires t0+1h, well before now) is dead too
	if n != 3 {
		t.Fatalf("deleted %d, want 3", n)
	}
	for _, c := range []string{"a", "d"} {
		if must(a.Sessions.FindByHash(ctx, h(c))) == nil {
			t.Fatalf("%s must survive", c)
		}
	}
	for _, c := range []string{"b", "c", "4"} {
		if must(a.Sessions.FindByHash(ctx, h(c))) != nil {
			t.Fatalf("%s must be gone", c)
		}
	}
}

func TestPendingLogins(t *testing.T) {
	ctx := context.Background()
	pool, a := setup(t)
	p := app.PendingLogin{State: "st1", CodeVerifier: "cv", Nonce: "no", ReturnTo: "/x?y=1", ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Truncate(time.Microsecond)}
	if err := a.PendingLogins.Insert(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := a.PendingLogins.Insert(ctx, p); err == nil {
		t.Fatal("duplicate state must fail")
	}
	got := must(a.PendingLogins.Consume(ctx, "st1"))
	if got == nil || *got != p {
		t.Fatalf("got %+v want %+v", got, p)
	}
	if must(a.PendingLogins.Consume(ctx, "st1")) != nil {
		t.Fatal("second consume must be nil")
	}

	// Insert removes rows expired for more than a minute (database clock) and nothing else.
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`insert into identity.oidc_pending (state, code_verifier, nonce, return_to, expires_at) values
		('old','v','n','/', now() - interval '2 minutes'),
		('recent','v','n','/', now() - interval '30 seconds'),
		('live','v','n','/', now() + interval '5 minutes')`)
	if err := a.PendingLogins.Insert(ctx, app.PendingLogin{State: "new", CodeVerifier: "v", Nonce: "n", ReturnTo: "/", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	for state, want := range map[string]bool{"old": false, "recent": true, "live": true, "new": true} {
		if (must(a.PendingLogins.Consume(ctx, state)) != nil) != want {
			t.Fatalf("state %s present = %v, want %v", state, !want, want)
		}
	}
}

func TestLoginLinks(t *testing.T) {
	ctx := context.Background()
	_, a := setup(t)
	u := newUser(t, a, "link-user")
	o := newUser(t, a, "link-other")
	l := app.LoginLinkRecord{CodeHash: strings.Repeat("e", 64), UserID: u.ID, CreatedAt: t0, ExpiresAt: t0.Add(15 * time.Minute)}
	if err := a.LoginLinks.Insert(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := a.LoginLinks.Insert(ctx, l); err == nil {
		t.Fatal("duplicate must fail")
	}
	got := must(a.LoginLinks.Consume(ctx, l.CodeHash))
	if got == nil || *got != l {
		t.Fatalf("got %+v want %+v", got, l)
	}
	if must(a.LoginLinks.Consume(ctx, l.CodeHash)) != nil {
		t.Fatal("second consume")
	}

	mk := func(c string, uid string, exp time.Time) app.LoginLinkRecord {
		r := app.LoginLinkRecord{CodeHash: strings.Repeat(c, 64), UserID: uid, CreatedAt: t0, ExpiresAt: exp}
		if err := a.LoginLinks.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	mk("1", u.ID, t0.Add(time.Hour))
	mk("2", o.ID, t0.Add(time.Hour))
	if err := a.LoginLinks.DeleteByUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if must(a.LoginLinks.Consume(ctx, strings.Repeat("1", 64))) != nil || must(a.LoginLinks.Consume(ctx, strings.Repeat("2", 64))) == nil {
		t.Fatal("DeleteByUser scope")
	}

	now := t0.Add(time.Hour)
	mk("3", u.ID, now) // == now: deleted
	mk("4", u.ID, now.Add(-time.Second))
	mk("5", o.ID, now.Add(time.Microsecond)) // alive
	n, err := a.LoginLinks.DeleteExpired(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("DeleteExpired = %d, %v", n, err)
	}
	if must(a.LoginLinks.Consume(ctx, strings.Repeat("5", 64))) == nil {
		t.Fatal("live link removed")
	}
}

// race runs fn in n goroutines released together and returns how many got a non-nil result.
func race[T any](n int, fn func() (*T, error)) (winners int, err error) {
	gate := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			r, e := fn()
			mu.Lock()
			defer mu.Unlock()
			if e != nil && err == nil {
				err = e
			}
			if r != nil {
				winners++
			}
		}()
	}
	close(gate)
	wg.Wait()
	return winners, err
}

func TestConcurrentConsumeGivesTheRowToExactlyOne(t *testing.T) {
	ctx := context.Background()
	_, a := setup(t)
	u := newUser(t, a, "race-user")
	for round := 0; round < 10; round++ {
		hash := strings.Repeat(string(rune('a'+round)), 64)
		if err := a.LoginLinks.Insert(ctx, app.LoginLinkRecord{CodeHash: hash, UserID: u.ID, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		w, err := race(8, func() (*app.LoginLinkRecord, error) { return a.LoginLinks.Consume(ctx, hash) })
		if err != nil || w != 1 {
			t.Fatalf("link round %d: winners = %d, err = %v", round, w, err)
		}
		state := "state-" + hash[:4]
		if err := a.PendingLogins.Insert(ctx, app.PendingLogin{State: state, CodeVerifier: "v", Nonce: "n", ReturnTo: "/", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		w, err = race(8, func() (*app.PendingLogin, error) { return a.PendingLogins.Consume(ctx, state) })
		if err != nil || w != 1 {
			t.Fatalf("pending round %d: winners = %d, err = %v", round, w, err)
		}
	}
}

// Rows in the shape the TypeScript server writes them (defaults, text[] groups, char(64) hashes).
func TestReadsRowsWrittenByTheTypeScriptServer(t *testing.T) {
	ctx := context.Background()
	pool, a := setup(t)
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`insert into identity.users (id, oidc_sub, name, email, groups, role, disabled, created_at, last_login_at)
		values ('AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA', 'ts-sub', 'Ts User', null, '{}', 'admin', true, '2026-01-02T03:04:05.678Z', null),
		       ('bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb', 'ts-sub2', 'Ts Two', 'a@b.c', '{"x y","z\"q","","a,b","ünï"}', 'member', false, '2026-01-02T03:04:05.678Z', '2026-01-03T00:00:00Z')`)
	hash := strings.Repeat("0123456789abcdef", 4)
	exec(`insert into identity.tokens (user_id, name, prefix, hash) values ('bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb', 'n', 'sk_1234567', $1)`, hash)
	exec(`insert into identity.sessions (id_hash, user_id, expires_at) values ($1, 'bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb', now() + interval '1 day')`, hash)

	u := must(a.Users.FindBySub(ctx, "ts-sub"))
	if u == nil || u.ID != "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa" || u.Email != nil || u.Groups == nil || len(u.Groups) != 0 ||
		u.Role != domain.RoleAdmin || !u.Disabled || u.LastLoginAt != nil ||
		!u.CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 678000000, time.UTC)) {
		t.Fatalf("ts user = %+v", u)
	}
	// an upper-case id from outside finds the same row (uuid compare, like TypeScript)
	if v := must(a.Users.FindByID(ctx, "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA")); v == nil || v.ID != u.ID {
		t.Fatalf("upper-case id: %+v", v)
	}
	u2 := must(a.Users.FindBySub(ctx, "ts-sub2"))
	if strings.Join(u2.Groups, "|") != `x y|z"q||a,b|ünï` || u2.Email == nil || *u2.Email != "a@b.c" || u2.LastLoginAt == nil {
		t.Fatalf("ts user 2 = %+v", u2)
	}
	tok := must(a.Tokens.FindByHash(ctx, hash))
	if tok == nil || tok.Hash != hash || tok.UserID != u2.ID || tok.CreatedAt.IsZero() {
		t.Fatalf("ts token = %+v", tok)
	}
	s := must(a.Sessions.FindByHash(ctx, hash))
	if s == nil || s.IDHash != hash || s.UserID != u2.ID {
		t.Fatalf("ts session = %+v", s)
	}
}

// Where TypeScript has no id-shape guard a malformed id reaches the database and fails: a silent
// no-op would make "revoke everything of this person" succeed without revoking anything.
func TestMalformedIDsFailWhereTypeScriptHasNoGuard(t *testing.T) {
	ctx := context.Background()
	_, a := setup(t)
	const bad = "not-a-uuid"
	checks := map[string]func() error{
		"Tokens.Revoke":           func() error { return a.Tokens.Revoke(ctx, bad, t0) },
		"Tokens.RevokeAllByUser":  func() error { return a.Tokens.RevokeAllByUser(ctx, bad, t0) },
		"Tokens.Touch":            func() error { return a.Tokens.Touch(ctx, bad, t0) },
		"Tokens.ListByUser":       func() error { _, err := a.Tokens.ListByUser(ctx, bad); return err },
		"Sessions.DeleteByUser":   func() error { return a.Sessions.DeleteByUser(ctx, bad) },
		"LoginLinks.DeleteByUser": func() error { return a.LoginLinks.DeleteByUser(ctx, bad) },
	}
	for name, fn := range checks {
		if fn() == nil {
			t.Errorf("%s accepted a malformed id", name)
		}
	}
}
