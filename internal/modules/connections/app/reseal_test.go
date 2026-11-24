package app_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

func resealSeed(t *testing.T) (*harness, *fakeBox, *fakeBox) {
	t.Helper()
	h := newHarness(t)
	oldBox, newKey := newBox(1), newBox(2)
	h.store.put(domain.ConnectOAuth(domain.OAuthConnect{
		UserID: "u1", Server: "gitea", IssuedFor: "fp", Now: t0,
		Tokens: domain.SealedTokens{Access: mustSeal(t, oldBox, "A1"), Refresh: mustSeal(t, oldBox, "R1"), ExpiresAt: tp(at(3_600_000))},
	}))
	h.store.put(domain.ConnectOAuth(domain.OAuthConnect{
		UserID: "u2", Server: "youtrack", IssuedFor: "fp", Now: t0,
		Tokens: domain.SealedTokens{Access: mustSeal(t, oldBox, "A2")},
	}))
	h.store.put(domain.ConnectPAT(domain.PATConnect{UserID: "u1", Server: "jira", Token: mustSeal(t, oldBox, "PAT"), IssuedFor: "fp", Now: t0}))
	return h, oldBox, newKey
}

func TestResealConnections(t *testing.T) {
	t.Run("re-seals access, refresh and PAT tokens; the old key opens none of them", func(t *testing.T) {
		h, oldBox, newBox := resealSeed(t)
		sum, err := app.ResealConnections(ctx, h.store, oldBox, newBox)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Resealed != 3 || sum.Current != 0 || len(sum.Failed) != 0 || sum.Failed == nil {
			t.Fatalf("%+v", sum)
		}
		plain := func(user, server string) (string, string) {
			s, _ := h.store.snap(user, server)
			a, err := newBox.Open(s.Access.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			var r string
			if len(s.Refresh.Bytes()) > 0 {
				if r, err = newBox.Open(s.Refresh.Bytes()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := oldBox.Open(s.Access.Bytes()); err == nil {
				t.Fatal("the old key still opens the access token")
			}
			if len(s.Refresh.Bytes()) > 0 {
				if _, err := oldBox.Open(s.Refresh.Bytes()); err == nil {
					t.Fatal("the old key still opens the refresh token")
				}
			}
			if !s.UpdatedAt.Equal(t0) {
				t.Fatal("updatedAt moved")
			}
			return a, r
		}
		if a, r := plain("u1", "gitea"); a != "A1" || r != "R1" {
			t.Fatal(a, r)
		}
		if a, r := plain("u2", "youtrack"); a != "A2" || r != "" {
			t.Fatal(a, r)
		}
		if a, r := plain("u1", "jira"); a != "PAT" || r != "" {
			t.Fatal(a, r)
		}
		// Status and expiry stay.
		s, _ := h.store.snap("u1", "gitea")
		if s.Status != domain.StatusReady || s.ExpiresAt == nil || !s.ExpiresAt.Equal(at(3_600_000)) {
			t.Fatalf("%v", s)
		}
		if len(h.audit.all()) != 0 {
			t.Fatal("the use case audits: the CLI records the rotation")
		}
		h.noLockViolations()
	})

	t.Run("a repeated run changes nothing", func(t *testing.T) {
		h, oldBox, newBox := resealSeed(t)
		if _, err := app.ResealConnections(ctx, h.store, oldBox, newBox); err != nil {
			t.Fatal(err)
		}
		before, _ := h.store.snap("u1", "gitea")
		sum, err := app.ResealConnections(ctx, h.store, oldBox, newBox)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Resealed != 0 || sum.Current != 3 || len(sum.Failed) != 0 {
			t.Fatalf("%+v", sum)
		}
		after, _ := h.store.snap("u1", "gitea")
		if !snapshotsEqual(before, after) {
			t.Fatal("a repeat changed the row")
		}
	})

	t.Run("a mix of current and old tokens re-seals only what needs it", func(t *testing.T) {
		h, oldBox, newBox := resealSeed(t)
		h.store.put(domain.ConnectOAuth(domain.OAuthConnect{
			UserID: "u1", Server: "gitea", IssuedFor: "fp", Now: t0,
			Tokens: domain.SealedTokens{Access: mustSeal(t, newBox, "A1"), Refresh: mustSeal(t, oldBox, "R1")},
		}))
		sum, err := app.ResealConnections(ctx, h.store, oldBox, newBox)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Resealed != 3 || sum.Failed == nil || len(sum.Failed) != 0 {
			t.Fatalf("%+v", sum)
		}
		s, _ := h.store.snap("u1", "gitea")
		if a, _ := newBox.Open(s.Access.Bytes()); a != "A1" {
			t.Fatal("access")
		}
		if r, _ := newBox.Open(s.Refresh.Bytes()); r != "R1" {
			t.Fatal("refresh")
		}
	})

	t.Run("leaves a connection with a token neither key opens, and names it", func(t *testing.T) {
		h, oldBox, newBox := resealSeed(t)
		lost := newBox2(9)
		h.store.put(domain.ConnectOAuth(domain.OAuthConnect{
			UserID: "u1", Server: "gitea", IssuedFor: "fp", Now: t0,
			Tokens: domain.SealedTokens{Access: mustSeal(t, oldBox, "A1"), Refresh: mustSeal(t, lost, "R1")},
		}))
		before, _ := h.store.snap("u1", "gitea")
		sum, err := app.ResealConnections(ctx, h.store, oldBox, newBox)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Resealed != 2 || sum.Current != 0 || !slices.Equal(sum.Failed, []string{"u1/gitea"}) {
			t.Fatalf("%+v", sum)
		}
		after, _ := h.store.snap("u1", "gitea")
		if !snapshotsEqual(before, after) {
			t.Fatal("an unreadable connection was changed")
		}
	})

	t.Run("an unreadable refresh token with a current access token is still unreadable", func(t *testing.T) {
		h, oldBox, newBox := resealSeed(t)
		lost := newBox2(9)
		h.store.put(domain.ConnectOAuth(domain.OAuthConnect{
			UserID: "u1", Server: "gitea", IssuedFor: "fp", Now: t0,
			Tokens: domain.SealedTokens{Access: mustSeal(t, newBox, "A1"), Refresh: mustSeal(t, lost, "R1")},
		}))
		sum, err := app.ResealConnections(ctx, h.store, oldBox, newBox)
		if err != nil || !slices.Equal(sum.Failed, []string{"u1/gitea"}) {
			t.Fatalf("%+v %v", sum, err)
		}
	})

	t.Run("skips a connection deleted meanwhile and works under the lock", func(t *testing.T) {
		h, oldBox, newBox := resealSeed(t)
		ghost := &ghostKeys{fakeStore: h.store}
		sum, err := app.ResealConnections(ctx, ghost, oldBox, newBox)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Resealed != 3 || sum.Current != 0 || len(sum.Failed) != 0 {
			t.Fatalf("%+v", sum)
		}
		got := h.j.all()
		if n := count(got, "lock.begin"); n != 4 {
			t.Fatalf("locks taken: %d, want one per key (4): %v", n, got)
		}
		// One at a time: every begin is closed before the next one.
		depth := 0
		for _, c := range got {
			switch c {
			case "lock.begin":
				depth++
			case "lock.end":
				depth--
			}
			if depth > 1 {
				t.Fatal("two locks held at once")
			}
		}
	})

	t.Run("a store error stops the run", func(t *testing.T) {
		h, oldBox, newBox := resealSeed(t)
		h.store.failLock = errors.New("db down")
		if _, err := app.ResealConnections(ctx, h.store, oldBox, newBox); err == nil {
			t.Fatal("no error")
		}
		failing := &failKeys{fakeStore: h.store}
		if _, err := app.ResealConnections(ctx, failing, oldBox, newBox); err == nil {
			t.Fatal("no error from the listing")
		}
	})
}

func newBox2(id byte) *fakeBox { return newBox(id) }

func count(list []string, what string) int {
	n := 0
	for _, s := range list {
		if s == what {
			n++
		}
	}
	return n
}

// ghostKeys lists one more key than the store holds: a connection deleted after the listing.
type ghostKeys struct{ *fakeStore }

func (g *ghostKeys) ListKeys(c context.Context) ([]app.ConnectionKey, error) {
	keys, err := g.fakeStore.ListKeys(c)
	return append(keys, app.ConnectionKey{UserID: "gone", Server: "gitea"}), err
}

type failKeys struct{ *fakeStore }

func (f *failKeys) ListKeys(context.Context) ([]app.ConnectionKey, error) {
	return nil, errors.New("db down")
}
