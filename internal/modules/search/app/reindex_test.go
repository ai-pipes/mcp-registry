package app_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
)

func TestReindexAllCleansUpAndReturnsTheTableCount(t *testing.T) {
	w := newWorld(t)
	w.cat.put("a", map[string]string{"x": "one"})
	w.cat.put("b", map[string]string{}) // disabled
	w.seed("stale.t", false)            // server gone from the catalog
	w.seed("b.y", false)
	n, err := app.ReindexAll(ctx, w.d)
	if err != nil || n != 1 {
		t.Fatalf("n = %d, err = %v", n, err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"a.x"}) {
		t.Fatalf("refs = %v", got)
	}
}

func TestReindexAllCountsTheWholeTable(t *testing.T) {
	w := newWorld(t)
	w.cat.put("a", map[string]string{"x": "one", "y": "two"})
	n, err := app.ReindexAll(ctx, w.d)
	if err != nil || n != 2 {
		t.Fatalf("n = %d, err = %v", n, err)
	}
}

func TestReindexAllVisitsCatalogServersThenIndexOnlyServers(t *testing.T) {
	w := newWorld(t)
	w.cat.put("b", map[string]string{})
	w.cat.put("a", map[string]string{})
	w.seed("zzz.t", false)
	w.seed("a.t", false)
	if _, err := app.ReindexAll(ctx, w.d); err != nil {
		t.Fatal(err)
	}
	var visited []string
	for _, c := range w.j.all() {
		if len(c) > 8 && c[:8] == "catalog " {
			visited = append(visited, c[8:])
		}
	}
	// catalog slugs in the catalog's order (a server of both lists once), then the rest
	if want := []string{"b", "a", "zzz"}; !slices.Equal(visited, want) {
		t.Fatalf("visited = %v, want %v", visited, want)
	}
}

func TestReindexAllWritesEveryTextRowBeforeReturningTheFirstEmbedderError(t *testing.T) {
	w := newWorld(t)
	w.cat.put("a", map[string]string{"x": "one"})
	w.cat.put("b", map[string]string{"y": "two"})
	w.failAll()
	_, err := app.ReindexAll(ctx, w.d)
	if !errors.Is(err, errEmbedder) {
		t.Fatalf("err = %v", err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"a.x", "b.y"}) {
		t.Fatalf("refs = %v", got)
	}
}

func TestReindexAllKeepsGoingAfterAnyServerFailsAndReturnsTheFirstError(t *testing.T) {
	w := newWorld(t)
	first := errors.New("first")
	w.cat.put("a", map[string]string{"x": "one"})
	w.cat.put("b", map[string]string{"y": "two"})
	w.idx.errRefs = first // every whole-server run fails when listing the refs
	if _, err := app.ReindexAll(ctx, w.d); !errors.Is(err, first) {
		t.Fatalf("err = %v", err)
	}
	if got := w.j.count("catalog "); got != 2 {
		t.Fatalf("servers visited = %d, want 2", got)
	}
}

func TestReindexAllPortErrors(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(w *world){
		"slugs":   func(w *world) { w.cat.errList = boom },
		"servers": func(w *world) { w.idx.errServers = boom },
		"count":   func(w *world) { w.idx.errCount = boom },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			set(w)
			if _, err := app.ReindexAll(ctx, w.d); !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestReindexAllACancelledContextStopsTheRun(t *testing.T) {
	w := newWorld(t)
	w.cat.put("a", map[string]string{"x": "one"})
	w.cat.put("b", map[string]string{"y": "two"})
	c, cancel := contextWithCancel()
	w.emb.fail = func(int, string) error { cancel(); return c.Err() }
	if _, err := app.ReindexAll(c, w.d); !errors.Is(err, c.Err()) {
		t.Fatalf("err = %v", err)
	}
	if w.j.count("catalog ") != 1 {
		t.Fatalf("went on after the cancellation: %v", w.j.all())
	}
}
