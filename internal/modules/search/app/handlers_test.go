package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
)

func TestEventHandlersCoverEveryCatalogEventType(t *testing.T) {
	h := app.EventHandlers(newWorld(t).d)
	var got []string
	for k := range h {
		got = append(got, k)
	}
	want := catalog.EventTypes()
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("handlers %v, catalog emits %v", got, want)
	}
	if catalog.EventToolsChanged != "catalog.tools_changed" || catalog.EventToolsRemoved != "catalog.tools_removed" || catalog.EventServerChanged != "catalog.server_changed" {
		t.Fatal("event type names changed")
	}
}

func TestToolsChangedIndexesTheRefsAndDropsAToolTheAdminHid(t *testing.T) {
	w := newWorld(t)
	h := app.EventHandlers(w.d)[catalog.EventToolsChanged]
	w.cat.put("gitea", map[string]string{"issue": "create issue", "pr": "open pull request"})
	if err := h(ctx, map[string]any{"server": "gitea", "refs": []any{"gitea.issue", "gitea.pr"}}); err != nil {
		t.Fatal(err)
	}
	if len(w.refs()) != 2 {
		t.Fatalf("refs = %v", w.refs())
	}
	w.cat.hide("gitea", "pr")
	if err := h(ctx, map[string]any{"server": "gitea", "refs": []string{"gitea.pr"}}); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"gitea.issue"}) {
		t.Fatalf("refs = %v", got)
	}
}

func TestToolsChangedIsIdempotentAndConvergesOnTheCurrentCatalog(t *testing.T) {
	w := newWorld(t)
	h := app.EventHandlers(w.d)[catalog.EventToolsChanged]
	w.cat.put("g", map[string]string{"a": "one", "b": "two"})
	ev := map[string]any{"server": "g", "refs": []any{"g.a", "g.b", "g.vanished"}}
	for range 3 {
		if err := h(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.refs(); !slices.Equal(got, []string{"g.a", "g.b"}) {
		t.Fatalf("refs = %v", got)
	}
	// the tool changed in the catalog since the event was written: the CURRENT state is indexed
	w.cat.tool("g", "a").Description = "changed"
	if err := h(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if w.doc("g.a").Description != "changed" {
		t.Fatalf("document = %+v", w.doc("g.a"))
	}
}

func TestToolsRemovedDeletesWhatTheCatalogDoesNotOfferAndKeepsALiveRow(t *testing.T) {
	w := newWorld(t)
	h := app.EventHandlers(w.d)[catalog.EventToolsRemoved]
	w.seed("gitea.a", false)
	w.seed("gitea.b", false)
	if err := h(ctx, map[string]any{"server": "gitea", "refs": []any{"gitea.a"}}); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"gitea.b"}) {
		t.Fatalf("refs = %v", got)
	}
	if w.j.count("catalog") != 1 {
		t.Fatal("the removal did not read the catalog")
	}
	// decided deviation stale-removal-rechecks-catalog: a stale event of a revived tool keeps its row
	w.cat.put("gitea", map[string]string{"b": "back again"})
	if err := h(ctx, map[string]any{"server": "gitea", "refs": []any{"gitea.b"}}); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"gitea.b"}) {
		t.Fatalf("refs = %v", got)
	}
}

func TestServerChangedReindexesTheServerAndRemovesItWhenDisabled(t *testing.T) {
	w := newWorld(t)
	h := app.EventHandlers(w.d)[catalog.EventServerChanged]
	w.cat.put("gitea", map[string]string{"issue": "create issue", "pr": "open pull request"})
	w.cat.servers["gitea"].Name = "Git"
	if err := h(ctx, map[string]any{"server": "gitea"}); err != nil {
		t.Fatal(err)
	}
	if len(w.refs()) != 2 || w.emb.passages[0] != "Git. gitea server\nissue\ncreate issue" {
		t.Fatalf("refs = %v, passages = %q", w.refs(), w.emb.passages)
	}
	before := len(w.emb.passages)
	w.cat.put("gitea", map[string]string{})
	if err := h(ctx, map[string]any{"server": "gitea"}); err != nil {
		t.Fatal(err)
	}
	if len(w.refs()) != 0 || len(w.emb.passages) != before {
		t.Fatalf("refs = %v, embed calls %d -> %d", w.refs(), before, len(w.emb.passages))
	}
}

func TestHandlerFailureRule(t *testing.T) {
	changed := map[string]any{"server": "g", "refs": []any{"g.a"}}
	t.Run("embedder down: rows written, handler fails", func(t *testing.T) {
		w := newWorld(t)
		w.cat.put("g", map[string]string{"a": "x"})
		w.failAll()
		for _, typ := range []string{catalog.EventToolsChanged, catalog.EventServerChanged} {
			if err := app.EventHandlers(w.d)[typ](ctx, changed); !errors.Is(err, errEmbedder) {
				t.Fatalf("%s: err = %v", typ, err)
			}
		}
		if d := w.doc("g.a"); d.Ref == "" || d.Embedding != nil {
			t.Fatalf("document = %+v", d)
		}
	})
	t.Run("nothing to embed: no failure although the embedder is down", func(t *testing.T) {
		w := newWorld(t)
		w.cat.put("g", map[string]string{})
		w.failAll()
		if err := app.EventHandlers(w.d)[catalog.EventToolsChanged](ctx, changed); err != nil {
			t.Fatal(err)
		}
		w.seed("g.a", true)
		if err := app.EventHandlers(w.d)[catalog.EventToolsRemoved](ctx, changed); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a catalog error fails the removal and keeps the row", func(t *testing.T) {
		boom := errors.New("boom")
		w := newWorld(t)
		w.seed("g.a", true)
		w.cat.errFor = boom
		if err := app.EventHandlers(w.d)[catalog.EventToolsRemoved](ctx, changed); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if got := w.refs(); !slices.Equal(got, []string{"g.a"}) {
			t.Fatalf("refs = %v", got)
		}
	})
	t.Run("embedder down does not make removal fail", func(t *testing.T) {
		w := newWorld(t)
		w.failAll()
		w.seed("g.a", false)
		if err := app.EventHandlers(w.d)[catalog.EventToolsRemoved](ctx, changed); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("port errors", func(t *testing.T) {
		boom := errors.New("boom")
		w := newWorld(t)
		w.cat.errFor = boom
		if err := app.EventHandlers(w.d)[catalog.EventServerChanged](ctx, map[string]any{"server": "g"}); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		w.cat.errFor = nil
		w.idx.errRemove = boom
		if err := app.EventHandlers(w.d)[catalog.EventToolsRemoved](ctx, changed); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestToolsChangedWithNoRefsIndexesNothingNotTheWholeServer(t *testing.T) {
	w := newWorld(t)
	w.cat.put("g", map[string]string{"a": "1", "b": "2"})
	w.seed("g.stale", true)
	h := app.EventHandlers(w.d)[catalog.EventToolsChanged]
	for _, refs := range []any{[]any{}, []string{}} {
		if err := h(ctx, map[string]any{"server": "g", "refs": refs}); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.emb.passages) != 0 || w.j.count("upsert") != 0 || w.j.count("remove") != 0 {
		t.Fatalf("calls = %v", w.j.all())
	}
	if got := w.refs(); !slices.Equal(got, []string{"g.stale"}) {
		t.Fatalf("refs = %v", got)
	}
}

// The payloads below are the documented shapes of what the catalog writes ({server, refs} and
// {server}), sent through encoding/json as the outbox stores and returns them. The catalog's own
// constructors are not reachable from search/app (modules talk through the root package only),
// and its parsers are what the handlers use, so a key rename that breaks the parsers fails here.
func TestHandlersAcceptThePayloadsAsTheOutboxReturnsThem(t *testing.T) {
	w := newWorld(t)
	w.cat.put("gitea", map[string]string{"issue": "create issue"})
	h := app.EventHandlers(w.d)
	roundTrip := func(p map[string]any) map[string]any {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	changed := roundTrip(map[string]any{"server": "gitea", "refs": []string{"gitea.issue"}})
	if ev, ok := catalog.ParseToolsEvent(changed); !ok || ev.Server != "gitea" || !slices.Equal(ev.Refs, []string{"gitea.issue"}) {
		t.Fatalf("the catalog's own parser refuses the payload: %v", changed)
	}
	if err := h[catalog.EventToolsChanged](ctx, changed); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"gitea.issue"}) {
		t.Fatalf("refs = %v", got)
	}
	if err := h[catalog.EventServerChanged](ctx, roundTrip(map[string]any{"server": "gitea"})); err != nil {
		t.Fatal(err)
	}
	w.cat.hide("gitea", "issue") // removed from the catalog: its removal event now takes effect
	if err := h[catalog.EventToolsRemoved](ctx, roundTrip(map[string]any{"server": "gitea", "refs": []string{"gitea.issue"}})); err != nil {
		t.Fatal(err)
	}
	if len(w.refs()) != 0 {
		t.Fatalf("refs = %v", w.refs())
	}
}

func TestMalformedPayloadsFail(t *testing.T) {
	h := app.EventHandlers(newWorld(t).d)
	cases := []struct {
		name    string
		handler func(context.Context, map[string]any) error
		payload map[string]any
	}{
		{"changed, no server", h[catalog.EventToolsChanged], map[string]any{"refs": []any{}}},
		{"changed, server not a string", h[catalog.EventToolsChanged], map[string]any{"server": 1, "refs": []any{}}},
		{"changed, no refs", h[catalog.EventToolsChanged], map[string]any{"server": "g"}},
		{"changed, refs a string", h[catalog.EventToolsChanged], map[string]any{"server": "g", "refs": "x"}},
		{"changed, a ref is a number", h[catalog.EventToolsChanged], map[string]any{"server": "g", "refs": []any{1}}},
		{"removed, no server (the catalog never writes that)", h[catalog.EventToolsRemoved], map[string]any{"refs": []any{"g.a"}}},
		{"removed, refs missing", h[catalog.EventToolsRemoved], map[string]any{"server": "g"}},
		{"removed, a ref is a number", h[catalog.EventToolsRemoved], map[string]any{"server": "g", "refs": []any{1}}},
		{"server changed, empty", h[catalog.EventServerChanged], map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.handler(ctx, c.payload)
			if !errors.Is(err, app.ErrBadPayload) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
