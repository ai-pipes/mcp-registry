package app_test

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

var (
	oldBox = fakeBox{id: "old"}
	newBox = fakeBox{id: "new"}
)

func sealedUnder(t *testing.T, b fakeBox, plain string) []byte {
	t.Helper()
	s, err := b.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func resealHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness()
	h.deps.Box = oldBox
	h.mustCreate(t, "alpha", ptr("alpha-key"), func(f *app.ServerFields) { f.Auth = authShared })
	h.mustCreate(t, "beta", ptr("beta-key"), func(f *app.ServerFields) { f.Auth = authShared })
	h.mustCreate(t, "open", nil)
	h.clean()
	return h
}

func TestResealServerSecrets(t *testing.T) {
	t.Run("re-seals every secret under the new key without touching updated_at or events", func(t *testing.T) {
		h := resealHarness(t)
		before := map[string]int64{}
		for slug, st := range h.servers.rows {
			before[slug] = st.UpdatedAt.UnixNano()
		}
		r, err := app.ResealServerSecrets(ctx, h.deps.Servers, oldBox, newBox)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(r, app.ResealSummary{Resealed: 2, Current: 0, Failed: []string{}}) {
			t.Fatalf("summary = %+v", r)
		}
		if len(*h.outbox) != 0 || len(h.servers.writes) != 0 {
			t.Fatal("a re-seal is not an edit: no events, no saves")
		}
		for slug, st := range h.servers.rows {
			if st.UpdatedAt.UnixNano() != before[slug] {
				t.Fatalf("%s: updated_at moved", slug)
			}
			if st.SealedSecret != nil {
				if _, err := oldBox.Open(st.SealedSecret); err == nil {
					t.Fatalf("%s: the old key still opens it", slug)
				}
			}
		}
		deps := h.deps
		deps.Box = newBox
		if got, _, _ := getServerSecret(ctx, deps, "alpha"); got != "alpha-key" {
			t.Fatalf("alpha = %q", got)
		}
		if got, _, _ := getServerSecret(ctx, deps, "beta"); got != "beta-key" {
			t.Fatalf("beta = %q", got)
		}
	})

	t.Run("a repeated run changes nothing", func(t *testing.T) {
		h := resealHarness(t)
		if _, err := app.ResealServerSecrets(ctx, h.deps.Servers, oldBox, newBox); err != nil {
			t.Fatal(err)
		}
		r, _ := app.ResealServerSecrets(ctx, h.deps.Servers, oldBox, newBox)
		if !reflect.DeepEqual(r, app.ResealSummary{Resealed: 0, Current: 2, Failed: []string{}}) {
			t.Fatalf("summary = %+v", r)
		}
	})

	t.Run("a secret neither key opens is reported and left as it is", func(t *testing.T) {
		h := resealHarness(t)
		lost := sealedUnder(t, fakeBox{id: "lost"}, "x")
		h.servers.setRow("beta", func(st *domain.ServerState) { st.SealedSecret = lost })
		r, _ := app.ResealServerSecrets(ctx, h.deps.Servers, oldBox, newBox)
		if !reflect.DeepEqual(r, app.ResealSummary{Resealed: 1, Current: 0, Failed: []string{"beta"}}) {
			t.Fatalf("summary = %+v", r)
		}
		if string(h.servers.row(t, "beta").SealedSecret) != string(lost) {
			t.Fatal("an unreadable secret was changed")
		}
	})

	t.Run("a secret replaced meanwhile (under the new key) is not overwritten", func(t *testing.T) {
		h := resealHarness(t)
		raced := false
		h.servers.replaceHook = func(slug string) {
			if slug == "alpha" && !raced {
				raced = true
				h.servers.setRow("alpha", func(st *domain.ServerState) { st.SealedSecret = sealedUnder(t, newBox, "admin-set-meanwhile") })
			}
		}
		r, _ := app.ResealServerSecrets(ctx, h.deps.Servers, oldBox, newBox)
		if !reflect.DeepEqual(r, app.ResealSummary{Resealed: 1, Current: 1, Failed: []string{}}) {
			t.Fatalf("summary = %+v", r)
		}
		deps := h.deps
		deps.Box = newBox
		if got, _, _ := getServerSecret(ctx, deps, "alpha"); got != "admin-set-meanwhile" {
			t.Fatalf("alpha = %q", got)
		}
	})

	t.Run("gives up on a secret that keeps changing, and skips a server deleted meanwhile", func(t *testing.T) {
		h := resealHarness(t)
		attempts := 0
		h.servers.replaceHook = func(slug string) {
			if slug == "alpha" {
				attempts++
				// Every attempt loses: the stored secret changes (under the old key) before the swap.
				h.servers.setRow("alpha", func(st *domain.ServerState) { st.SealedSecret = sealedUnder(t, oldBox, "churn"+strconv.Itoa(attempts)) })
			}
		}
		r, _ := app.ResealServerSecrets(ctx, h.deps.Servers, oldBox, newBox)
		if attempts != 3 {
			t.Fatalf("attempts = %d, want 3", attempts)
		}
		if !reflect.DeepEqual(r.Failed, []string{"alpha"}) || r.Resealed != 1 {
			t.Fatalf("summary = %+v", r)
		}
	})

	t.Run("a server that vanished after the compare-and-set lost is skipped", func(t *testing.T) {
		h := resealHarness(t)
		h.servers.replaceHook = func(slug string) {
			if slug == "beta" {
				h.servers.mu.Lock()
				delete(h.servers.rows, "beta")
				h.servers.mu.Unlock()
			}
		}
		r, _ := app.ResealServerSecrets(ctx, h.deps.Servers, oldBox, newBox)
		if len(r.Failed) != 0 || r.Resealed != 1 {
			t.Fatalf("summary = %+v", r)
		}
	})
}
