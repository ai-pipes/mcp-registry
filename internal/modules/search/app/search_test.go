package app_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

func searchWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.cat.put("gitea", map[string]string{"pdf": "extract text from pdf", "review": "code review before merge"})
	w.cat.put("youtrack", map[string]string{"borscht": "cooking recipe", "pdf": "pdf report export"})
	if _, err := app.ReindexAll(ctx, w.d); err != nil {
		t.Fatal(err)
	}
	w.j.mu.Lock()
	w.j.calls = nil
	w.j.mu.Unlock()
	w.emb.passages = nil
	return w
}

func refsOf(hits []domain.ToolHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Ref
	}
	return out
}

func TestSearchToolsNothingToSearchTouchesNothing(t *testing.T) {
	w := searchWorld(t)
	for name, c := range map[string]struct {
		query   string
		limit   int
		servers []string
	}{
		"no servers":     {"pdf", 10, nil},
		"empty servers":  {"pdf", 10, []string{}},
		"blank":          {"   ", 10, []string{"gitea"}},
		"js whitespace":  {"\ufeff\u00a0\n", 10, []string{"gitea"}},
		"limit zero":     {"pdf", 0, []string{"gitea"}},
		"negative limit": {"pdf", -1, []string{"gitea"}},
	} {
		t.Run(name, func(t *testing.T) {
			hits, err := app.SearchTools(ctx, w.d, c.query, c.limit, c.servers)
			if err != nil || len(hits) != 0 || hits == nil {
				t.Fatalf("hits = %#v (want an empty non-nil slice), err = %v", hits, err)
			}
		})
	}
	if got := w.j.all(); len(got) != 0 {
		t.Fatalf("ports were called: %v", got)
	}
}

func TestSearchToolsNothingMatchesAndNothingClearsTheFloor(t *testing.T) {
	w := searchWorld(t)
	hits, err := app.SearchTools(ctx, w.d, "weather tomorrow", 10, []string{"gitea"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
}

func TestSearchToolsScoresATextOnlyHitHalfAndAFirstInBothHitOne(t *testing.T) {
	w := searchWorld(t)
	hits, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"})
	if err != nil || !slices.Equal(hits, []domain.ToolHit{{Ref: "gitea.review", Server: "gitea", Score: 0.5}}) {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
	low := w.d
	low.MinSimilarity = 0.3
	hits, err = app.SearchTools(ctx, low, "cooking recipe", 10, []string{"youtrack"})
	if err != nil || !slices.Equal(hits, []domain.ToolHit{{Ref: "youtrack.borscht", Server: "youtrack", Score: 1}}) {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
}

func TestSearchToolsTheQueryIsTrimmedOnceAndEmbeddedAsOneCall(t *testing.T) {
	w := searchWorld(t)
	if _, err := app.SearchTools(ctx, w.d, " \t merge ", 10, []string{"gitea"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.emb.queries, []string{"merge"}) {
		t.Fatalf("queries = %q", w.emb.queries)
	}
	if w.idx.lastLimit != domain.Candidates || w.idx.lastFloor != 0.5 {
		t.Fatalf("vector query got limit %d floor %v", w.idx.lastLimit, w.idx.lastFloor)
	}
}

func TestSearchToolsAnFTSOnlyHitScoresTheSameWhetherOrNotTheEmbedderIsReady(t *testing.T) {
	w := searchWorld(t)
	ready, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"})
	if err != nil {
		t.Fatal(err)
	}
	w.emb.notReady = true
	before := w.j.count("embedq")
	cold, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"})
	if err != nil || !slices.Equal(ready, cold) {
		t.Fatalf("ready %v, cold %v, err %v", ready, cold, err)
	}
	if w.j.count("embedq") != before || w.j.count("vector") != 1 || len(w.log.logs) != 0 {
		t.Fatalf("not ready must skip the vector side without a warning: %v", w.j.all())
	}
}

func TestSearchToolsEmbedderFailureFallsBackToTextAndWarnsWithTheLabelOnly(t *testing.T) {
	w := searchWorld(t)
	w.emb.failQuery = errEmbedder
	hits, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"})
	if err != nil || !slices.Equal(refsOf(hits), []string{"gitea.review"}) {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
	if len(w.log.logs) != 1 {
		t.Fatalf("warnings = %v", w.log.logs)
	}
	rec := w.log.logs[0]
	if rec.msg != "search: vector search failed, text-only fallback" || !slices.Equal(rec.args, []any{"err", kernel.ErrorLabel(errEmbedder)}) {
		t.Fatalf("record = %+v", rec)
	}
	if strings.Contains(fmt.Sprint(w.log.logs), "merge") {
		t.Fatal("the warning contains the query")
	}
}

func TestSearchToolsVectorQueryErrorAlsoFallsBackToText(t *testing.T) {
	w := searchWorld(t)
	w.idx.errVec = errors.New("pg down")
	hits, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"})
	if err != nil || len(hits) != 1 || hits[0].Score != 0.5 || len(w.log.logs) != 1 {
		t.Fatalf("hits = %v, err = %v, logs = %v", hits, err, w.log.logs)
	}
}

func TestSearchToolsFailureWithoutALoggerIsFine(t *testing.T) {
	w := searchWorld(t)
	w.d.Logger = nil
	w.emb.failQuery = errEmbedder
	if _, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchToolsAnOverlongQueryIsTextOnlyWithoutAWarningOrEmbedderCall(t *testing.T) {
	w := searchWorld(t)
	w.emb.readyN = 0
	hits, err := app.SearchTools(ctx, w.d, strings.Repeat("z", 2001), 10, []string{"gitea"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
	if w.emb.readyN != 0 || len(w.emb.queries) != 0 || len(w.log.logs) != 0 || w.j.count("vector") != 0 {
		t.Fatalf("calls = %v", w.j.all())
	}
	if w.j.count("fts") != 1 {
		t.Fatal("the text side must still run")
	}
}

func TestSearchToolsTheLengthLimitCountsUTF16Units(t *testing.T) {
	w := searchWorld(t)
	fits := strings.Repeat("😀", 1000)    // 2000 units
	tooLong := strings.Repeat("😀", 1001) // 2002 units, 1001 runes
	if _, err := app.SearchTools(ctx, w.d, fits, 10, []string{"gitea"}); err != nil {
		t.Fatal(err)
	}
	if len(w.emb.queries) != 1 {
		t.Fatalf("2000 units must be embedded: %d", len(w.emb.queries))
	}
	if _, err := app.SearchTools(ctx, w.d, tooLong, 10, []string{"gitea"}); err != nil {
		t.Fatal(err)
	}
	if len(w.emb.queries) != 1 {
		t.Fatalf("2002 units must not be embedded: %d", len(w.emb.queries))
	}
}

func TestSearchToolsTheServersFilterReachesBothQueriesAndWinsOverRelevance(t *testing.T) {
	w := newWorld(t)
	w.cat.put("youtrack", map[string]string{"pdf": "pdf report export"})
	w.cat.put("gitea", map[string]string{"pdf": "extract text from pdf"})
	if _, err := app.ReindexAll(ctx, w.d); err != nil {
		t.Fatal(err)
	}
	both, err := app.SearchTools(ctx, w.d, "pdf", 1, []string{"gitea", "youtrack"})
	if err != nil || len(both) != 1 {
		t.Fatalf("hits = %v, err = %v", both, err)
	}
	only, err := app.SearchTools(ctx, w.d, "pdf", 1, []string{"gitea"})
	if err != nil || !slices.Equal(refsOf(only), []string{"gitea.pdf"}) {
		t.Fatalf("hits = %v, err = %v", only, err)
	}
	if !slices.Equal(w.idx.ftsServers[len(w.idx.ftsServers)-1], []string{"gitea"}) || !slices.Equal(w.idx.vecServers[len(w.idx.vecServers)-1], []string{"gitea"}) {
		t.Fatalf("fts %v vector %v", w.idx.ftsServers, w.idx.vecServers)
	}
}

func TestSearchToolsRespectsTheLimit(t *testing.T) {
	w := searchWorld(t)
	hits, err := app.SearchTools(ctx, w.d, "pdf", 1, []string{"gitea", "youtrack"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
}

func TestSearchToolsKeepsDottedToolNamesIntact(t *testing.T) {
	w := searchWorld(t)
	w.cat.put("gitea", map[string]string{"issues.create": "create an issue"})
	if err := app.IndexTools(ctx, w.d, "gitea"); err != nil {
		t.Fatal(err)
	}
	hits, err := app.SearchTools(ctx, w.d, "create an issue", 5, []string{"gitea"})
	if err != nil || len(hits) == 0 || hits[0].Ref != "gitea.issues.create" || hits[0].Server != "gitea" {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
}

func TestSearchToolsAFullTextErrorIsReturned(t *testing.T) {
	w := searchWorld(t)
	boom := errors.New("boom")
	w.idx.errFTS = boom
	if _, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestSearchToolsBothHalvesAreStartedBeforeEitherIsAwaited(t *testing.T) {
	w := searchWorld(t)
	if _, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"}); err != nil {
		t.Fatal(err)
	}
	// Both ran; their relative order is not part of the contract.
	if w.j.count("fts") != 1 || w.j.count("ready") != 1 || w.j.count("embedq") != 1 || w.j.count("vector") != 1 {
		t.Fatalf("calls = %v", w.j.all())
	}
}

func TestSearchToolsAPanicInTheFullTextQueryBecomesAnError(t *testing.T) {
	w := searchWorld(t)
	w.idx.panicFTS = true
	hits, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"})
	if !errors.Is(err, app.ErrPanicked) || hits != nil {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
	if strings.Contains(err.Error(), "merge") || strings.Contains(fmt.Sprint(w.log.logs), "merge") {
		t.Fatal("the panic value leaked")
	}
}

func TestSearchToolsAPanicInTheVectorHalfIsTheTextOnlyFallbackWithTheUsualWarning(t *testing.T) {
	w := searchWorld(t)
	w.emb.panicQuery = true
	hits, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"})
	if err != nil || len(hits) != 1 || hits[0].Score != 0.5 {
		t.Fatalf("hits = %v, err = %v", hits, err)
	}
	if len(w.log.logs) != 1 || w.log.logs[0].msg != app.MsgVectorFallback ||
		!slices.Equal(w.log.logs[0].args, []any{"err", kernel.ErrorLabel(app.ErrPanicked)}) {
		t.Fatalf("logs = %v", w.log.logs)
	}
	if strings.Contains(fmt.Sprint(w.log.logs), "merge") {
		t.Fatal("the panic value leaked")
	}
}

func TestSearchToolsAPanicInTheVectorHalfStillWaitsForTheFullTextHalf(t *testing.T) {
	w := searchWorld(t)
	w.emb.panicQuery = true
	w.idx.panicFTS = true
	if _, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"}); !errors.Is(err, app.ErrPanicked) {
		t.Fatalf("err = %v", err)
	}
}

func TestSearchToolsTheHalvesReallyRunAtTheSameTime(t *testing.T) {
	w := searchWorld(t)
	ftsIn, vecIn := make(chan struct{}), make(chan struct{})
	timeout := func() <-chan time.Time { return time.After(10 * time.Second) }
	var failures []string
	var mu sync.Mutex
	fail := func(s string) { mu.Lock(); failures = append(failures, s); mu.Unlock() }
	// each half is in flight and waits until the other one has started
	w.idx.onFTS = func() {
		close(ftsIn)
		select {
		case <-vecIn:
		case <-timeout():
			fail("the vector half never started while the full-text query was in flight")
		}
	}
	w.emb.onQuery = func() {
		close(vecIn)
		select {
		case <-ftsIn:
		case <-timeout():
			fail("the full-text query never started while the vector half was in flight")
		}
	}
	if _, err := app.SearchTools(ctx, w.d, "merge", 10, []string{"gitea"}); err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatal(failures)
	}
}
