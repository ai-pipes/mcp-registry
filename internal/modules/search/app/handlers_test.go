package app_test

import (
	"context"
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

func TestToolsRemovedDeletesByRefWithoutConsultingTheCatalog(t *testing.T) {
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
	if w.j.count("catalog") != 0 {
		t.Fatal("the removal consulted the catalog")
	}
	// the stale-event peculiarity: a live document of a revived tool is deleted all the same
	w.cat.put("gitea", map[string]string{"b": "back again"})
	if err := h(ctx, map[string]any{"refs": []any{"gitea.b"}}); err != nil {
		t.Fatal(err)
	}
	if len(w.refs()) != 0 {
		t.Fatalf("refs = %v", w.refs())
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
		w.idx.errRemove = boom
		if err := app.EventHandlers(w.d)[catalog.EventToolsRemoved](ctx, changed); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMalformedPayloadsFail(t *testing.T) {
	h := app.EventHandlers(newWorld(t).d)
	cases := []struct {
		name    string
		handler func(context.Context, map[string]any) error
		payload map[string]any
		want    string
	}{
		{"changed, no server", h[catalog.EventToolsChanged], map[string]any{"refs": []any{}}, "event payload has no server"},
		{"changed, server not a string", h[catalog.EventToolsChanged], map[string]any{"server": 1, "refs": []any{}}, "event payload has no server"},
		{"changed, no refs", h[catalog.EventToolsChanged], map[string]any{"server": "g"}, "event payload has no refs"},
		{"changed, refs a string", h[catalog.EventToolsChanged], map[string]any{"server": "g", "refs": "x"}, "event payload has no refs"},
		{"changed, a ref is a number", h[catalog.EventToolsChanged], map[string]any{"server": "g", "refs": []any{1}}, "event payload has no refs"},
		{"removed, refs missing", h[catalog.EventToolsRemoved], map[string]any{"server": "g"}, "event payload has no refs"},
		{"removed, a ref is a number", h[catalog.EventToolsRemoved], map[string]any{"server": "g", "refs": []any{1}}, "event payload has no refs"},
		{"server changed, empty", h[catalog.EventServerChanged], map[string]any{}, "event payload has no server"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.handler(ctx, c.payload)
			if err == nil || err.Error() != c.want {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}
