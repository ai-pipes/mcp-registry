package app_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
)

// sweepWorld indexes s.a, s.b, s.c while the embedder is down, then brings it back.
func sweepWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "tool a", "b": "tool b", "c": "tool c"})
	w.failAll()
	_ = app.IndexTools(ctx, w.d, "s")
	w.emb.fail = nil
	w.emb.passages = nil
	w.j.mu.Lock()
	w.j.calls = nil
	w.j.mu.Unlock()
	return w
}

func poison(substr string) func(int, string) error {
	return func(_ int, text string) error {
		if len(text) > 0 && contains(text, substr) {
			return errors.New("poison")
		}
		return nil
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSweepReindexesAtMostLimitPerRun(t *testing.T) {
	w := sweepWorld(t)
	if w.missing() != 3 {
		t.Fatalf("missing = %d", w.missing())
	}
	r, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 2})
	if err != nil || !slices.Equal(r.Indexed, []string{"s.a", "s.b"}) || w.missing() != 1 {
		t.Fatalf("result = %+v, err = %v, missing = %d", r, err, w.missing())
	}
	r, _ = app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 2})
	if !slices.Equal(r.Indexed, []string{"s.c"}) {
		t.Fatalf("result = %+v", r)
	}
	r, _ = app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 2})
	if len(r.Indexed) != 0 || len(r.Failed) != 0 || r.StoppedEarly != "" {
		t.Fatalf("result = %+v", r)
	}
}

func TestSweepDoesNothingWhileTheEmbedderIsNotReady(t *testing.T) {
	w := sweepWorld(t)
	w.emb.notReady = true
	r, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
	if err != nil || len(r.Indexed) != 0 || len(w.emb.passages) != 0 {
		t.Fatalf("result = %+v, err = %v", r, err)
	}
	if got := w.j.all(); !slices.Equal(got, []string{"ready"}) {
		t.Fatalf("calls = %v", got)
	}
}

func TestSweepMarksOnlyAnIsolatedPoisonDocumentAndThenSkipsIt(t *testing.T) {
	w := sweepWorld(t)
	w.emb.fail = poison("tool a")
	r, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
	if err != nil || !slices.Equal(r.Indexed, []string{"s.b", "s.c"}) || !slices.Equal(r.Failed, []string{"s.a"}) || r.StoppedEarly != "" {
		t.Fatalf("result = %+v, err = %v", r, err)
	}
	if !w.idx.failed["s.a"] || len(w.idx.failed) != 1 {
		t.Fatalf("marked = %v", w.idx.failed)
	}
	before := len(w.emb.passages)
	r, _ = app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
	if len(r.Indexed) != 0 || len(w.emb.passages) != before {
		t.Fatalf("a marked document was tried again: %+v", r)
	}
}

func TestSweepThreeFailuresInARowAreAnOutageWhenTheProbeFails(t *testing.T) {
	w := sweepWorld(t)
	w.failAll()
	r, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
	if err != nil || !slices.Equal(r.Failed, []string{"s.a", "s.b", "s.c"}) || r.StoppedEarly != app.StoppedByOutage || len(r.Indexed) != 0 {
		t.Fatalf("result = %+v, err = %v", r, err)
	}
	if len(w.emb.passages) != 4 || w.emb.passages[3] != "health probe" { // 3 documents and the probe
		t.Fatalf("passages = %q", w.emb.passages)
	}
	if len(w.idx.failed) != 0 {
		t.Fatalf("marked = %v", w.idx.failed)
	}
}

func TestSweepAPoisonStreakIsMarkedWhenTheProbeSucceedsAndTheSweepGoesOn(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"p1": "poison", "p2": "poison", "p3": "poison", "zok": "fine"})
	w.failAll()
	_ = app.IndexTools(ctx, w.d, "s")
	w.emb.fail = poison("poison")
	r, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
	if err != nil || !slices.Equal(r.Indexed, []string{"s.zok"}) || !slices.Equal(r.Failed, []string{"s.p1", "s.p2", "s.p3"}) || r.StoppedEarly != "" {
		t.Fatalf("result = %+v, err = %v", r, err)
	}
	for _, ref := range []string{"s.p1", "s.p2", "s.p3"} {
		if !w.idx.failed[ref] {
			t.Errorf("%s not marked", ref)
		}
	}
}

func TestSweepStopsWhenTheBudgetIsSpentAndNeverMarksWhatItDidNotTry(t *testing.T) {
	w := sweepWorld(t)
	// The deadline is read at once (6 ms in), then once per document: 12 ms and 18 ms.
	w.d.Clock = &stepClock{t: time.Unix(0, 0), step: 6 * time.Millisecond}
	r, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20, Budget: 10 * time.Millisecond})
	if err != nil || r.StoppedEarly != app.StoppedByBudget || !slices.Equal(r.Indexed, []string{"s.a"}) {
		t.Fatalf("result = %+v, err = %v", r, err)
	}
	if len(w.idx.failed) != 0 {
		t.Fatalf("marked = %v", w.idx.failed)
	}
}

func TestSweepDefaultBudgetIsThirtySeconds(t *testing.T) {
	w := sweepWorld(t)
	w.d.Clock = &stepClock{t: time.Unix(0, 0), step: 29 * time.Second}
	r, _ := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
	// deadline = 29 s + 30 s = 59 s; the first document is checked at 58 s, the second at 87 s
	if r.StoppedEarly != app.StoppedByBudget || !slices.Equal(r.Indexed, []string{"s.a"}) {
		t.Fatalf("result = %+v", r)
	}
	if app.DefaultSweepBudget != 30*time.Second {
		t.Fatal(app.DefaultSweepBudget)
	}
}

func TestSweepAtTheDeadlineExactlyStops(t *testing.T) {
	w := sweepWorld(t)
	w.d.Clock = &stepClock{t: time.Unix(0, 0), step: 10 * time.Millisecond}
	// deadline = 10 + 10 = 20 ms; the first check reads 20 ms: now >= deadline
	r, _ := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20, Budget: 10 * time.Millisecond})
	if r.StoppedEarly != app.StoppedByBudget || len(r.Indexed) != 0 {
		t.Fatalf("result = %+v", r)
	}
}

func TestSweepAFailedRunReKeepsTheDocumentWithoutAVector(t *testing.T) {
	w := sweepWorld(t)
	w.emb.fail = poison("tool b")
	_, _ = app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
	if d := w.doc("s.b"); d.Ref == "" || d.Embedding != nil {
		t.Fatalf("document = %+v", d)
	}
}

func TestSweepLogsNothing(t *testing.T) {
	w := sweepWorld(t)
	w.failAll()
	_, _ = app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
	if len(w.log.logs) != 0 {
		t.Fatalf("logs = %v", w.log.logs)
	}
}

func TestSweepDocumentsTheCatalogNoLongerOffersAreRemovedAndCountAsIndexed(t *testing.T) {
	w := sweepWorld(t)
	w.cat.hide("s", "a")
	r, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
	if err != nil || !slices.Equal(r.Indexed, []string{"s.a", "s.b", "s.c"}) {
		t.Fatalf("result = %+v, err = %v", r, err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"s.b", "s.c"}) {
		t.Fatalf("refs = %v", got)
	}
}

func TestSweepErrors(t *testing.T) {
	boom := errors.New("boom")
	t.Run("listing", func(t *testing.T) {
		w := sweepWorld(t)
		w.idx.errWithout = boom
		if _, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20}); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("marking a poison streak", func(t *testing.T) {
		w := sweepWorld(t)
		w.emb.fail = func(_ int, text string) error {
			if text == "health probe" {
				return nil
			}
			return errEmbedder
		}
		w.idx.errMark = boom
		if _, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20}); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("marking after a success counts as that document's failure", func(t *testing.T) {
		w := sweepWorld(t)
		w.emb.fail = poison("tool a")
		w.idx.errMark = boom
		r, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
		if err != nil || !slices.Equal(r.Failed, []string{"s.a", "s.b", "s.c"}) || !slices.Equal(r.Indexed, []string{"s.b", "s.c"}) {
			t.Fatalf("result = %+v, err = %v", r, err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		w := sweepWorld(t)
		c, cancel := contextWithCancel()
		cancel()
		if _, err := app.IndexMissingEmbeddings(c, w.d, app.SweepOptions{Limit: 20}); !errors.Is(err, c.Err()) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cancelled mid run", func(t *testing.T) {
		w := sweepWorld(t)
		c, cancel := contextWithCancel()
		w.emb.fail = func(int, string) error { cancel(); return c.Err() }
		r, err := app.IndexMissingEmbeddings(c, w.d, app.SweepOptions{Limit: 20})
		if !errors.Is(err, c.Err()) || len(r.Failed) != 0 || len(w.idx.failed) != 0 {
			t.Fatalf("result = %+v, err = %v", r, err)
		}
	})
}

// Decided deviation vector-kept-on-failed-reembedding: a row that kept its old vector after a
// failed re-embedding is offered by the sweep and gets a fresh vector; the mark goes.
func TestSweepRefreshesARowThatKeptAStaleVector(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "old text"})
	if err := app.IndexTools(ctx, w.d, "s"); err != nil {
		t.Fatal(err)
	}
	old := w.doc("s.a").Embedding
	w.cat.tool("s", "a").Description = "completely different words"
	w.failAll()
	_ = app.IndexTools(ctx, w.d, "s")
	w.emb.fail = nil
	r, err := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20})
	if err != nil || !slices.Equal(r.Indexed, []string{"s.a"}) {
		t.Fatalf("result = %+v, err %v", r, err)
	}
	if slices.Equal(w.doc("s.a").Embedding, old) || w.isStale("s.a") {
		t.Fatalf("the vector was not refreshed or the mark stayed (stale %v)", w.isStale("s.a"))
	}
	// nothing left to do
	if r, _ := app.IndexMissingEmbeddings(ctx, w.d, app.SweepOptions{Limit: 20}); len(r.Indexed) != 0 {
		t.Fatalf("second sweep indexed %v", r.Indexed)
	}
}
