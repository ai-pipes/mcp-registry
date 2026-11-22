package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
)

func TestIndexToolsIndexesEveryVisibleToolWithBodyAndVector(t *testing.T) {
	w := newWorld(t)
	w.cat.put("gitea", map[string]string{"issue": "create issue", "pr": "open pull request"})
	if err := app.IndexTools(ctx, w.d, "gitea"); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"gitea.issue", "gitea.pr"}) {
		t.Fatalf("refs = %v", got)
	}
	d := w.doc("gitea.issue")
	if d.Server != "gitea" || d.Description != "create issue" || d.Body != "query search words" || len(d.Embedding) != app.Dimensions {
		t.Fatalf("document = %+v", d)
	}
	// The embedder gets the bare passage (the prefix is the embedder's), in catalog order.
	want := []string{
		"gitea. gitea server\nissue\ncreate issue",
		"gitea. gitea server\npr\nopen pull request",
	}
	if !slices.Equal(w.emb.passages, want) {
		t.Fatalf("passages = %q", w.emb.passages)
	}
}

func TestIndexToolsPassageCarriesTheTitle(t *testing.T) {
	w := newWorld(t)
	w.cat.put("g", map[string]string{"a": "desc"})
	title := "Nice title"
	w.cat.tool("g", "a").Title = &title
	if err := app.IndexTools(ctx, w.d, "g"); err != nil {
		t.Fatal(err)
	}
	if got := w.emb.passages[0]; got != "g. g server\na Nice title\ndesc" {
		t.Fatalf("passage = %q", got)
	}
	if d := w.doc("g.a"); d.Title == nil || *d.Title != title {
		t.Fatalf("title = %v", d.Title)
	}
}

func TestIndexToolRefsLeavesTheRestOfTheServerAlone(t *testing.T) {
	w := newWorld(t)
	w.cat.put("gitea", map[string]string{"issue": "create issue", "pr": "open pull request"})
	if err := app.IndexToolRefs(ctx, w.d, "gitea", []string{"gitea.pr"}); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"gitea.pr"}) {
		t.Fatalf("refs = %v", got)
	}
}

func TestIndexToolRefsRemovesARefTheCatalogNoLongerOffers(t *testing.T) {
	w := newWorld(t)
	w.cat.put("gitea", map[string]string{"issue": "create issue", "pr": "open pull request"})
	if err := app.IndexTools(ctx, w.d, "gitea"); err != nil {
		t.Fatal(err)
	}
	w.cat.hide("gitea", "pr")
	if err := app.IndexToolRefs(ctx, w.d, "gitea", []string{"gitea.pr"}); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"gitea.issue"}) {
		t.Fatalf("refs = %v", got)
	}
}

func TestIndexToolRefsAnEmptyListDoesNothing(t *testing.T) {
	w := newWorld(t)
	w.cat.put("g", map[string]string{"a": "x"})
	if err := app.IndexToolRefs(ctx, w.d, "g", []string{}); err != nil {
		t.Fatal(err)
	}
	if got := w.j.all(); !slices.Equal(got, []string{"catalog g"}) {
		t.Fatalf("calls = %v", got)
	}
}

func TestIndexToolsWholeServerRemovesWhatItNoLongerOffers(t *testing.T) {
	w := newWorld(t)
	w.cat.put("g", map[string]string{"a": "x", "b": "y"})
	if err := app.IndexTools(ctx, w.d, "g"); err != nil {
		t.Fatal(err)
	}
	w.cat.hide("g", "a")
	if err := app.IndexTools(ctx, w.d, "g"); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"g.b"}) {
		t.Fatalf("refs = %v", got)
	}
}

func TestIndexToolsDropsDocumentsOfADisabledEmptiedOrUnknownServer(t *testing.T) {
	w := newWorld(t)
	w.cat.put("gitea", map[string]string{"issue": "create issue"})
	if err := app.IndexTools(ctx, w.d, "gitea"); err != nil {
		t.Fatal(err)
	}
	w.cat.put("gitea", map[string]string{}) // disabled: no tools
	before := len(w.emb.passages)
	if err := app.IndexTools(ctx, w.d, "gitea"); err != nil {
		t.Fatal(err)
	}
	if len(w.refs()) != 0 || len(w.emb.passages) != before {
		t.Fatalf("refs = %v, embed calls %d -> %d", w.refs(), before, len(w.emb.passages))
	}
	w.seed("ghost.x", false)
	if err := app.IndexTools(ctx, w.d, "ghost"); err != nil { // unknown: the catalog answers nil
		t.Fatal(err)
	}
	if len(w.refs()) != 0 {
		t.Fatalf("refs = %v", w.refs())
	}
}

func TestIndexToolsWritesTextRowsAndReturnsTheEmbedderError(t *testing.T) {
	w := newWorld(t)
	w.cat.put("gitea", map[string]string{"issue": "create issue"})
	w.failAll()
	err := app.IndexTools(ctx, w.d, "gitea")
	if !errors.Is(err, errEmbedder) {
		t.Fatalf("err = %v", err)
	}
	if d := w.doc("gitea.issue"); d.Ref == "" || d.Embedding != nil || d.Body == "" {
		t.Fatalf("document = %+v", d)
	}
}

func TestIndexToolsEmbedsOnePassagePerCall(t *testing.T) {
	w := newWorld(t)
	tools := map[string]string{}
	for i := range 40 {
		tools[fmt.Sprintf("t%d", i)] = "x"
	}
	w.cat.put("big", tools)
	if err := app.IndexTools(ctx, w.d, "big"); err != nil {
		t.Fatal(err)
	}
	if len(w.refs()) != 40 || len(w.emb.passages) != 40 {
		t.Fatalf("docs %d, embedder calls %d", len(w.refs()), len(w.emb.passages))
	}
}

func TestIndexToolsStopsCallingTheEmbedderAfterTheFirstFailure(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"})
	w.emb.fail = func(n int, _ string) error {
		if n >= 3 { // the third call fails; a fourth would too, but must not happen
			return errEmbedder
		}
		return nil
	}
	if err := app.IndexTools(ctx, w.d, "s"); !errors.Is(err, errEmbedder) {
		t.Fatalf("err = %v", err)
	}
	if len(w.emb.passages) != 3 {
		t.Fatalf("embedder calls = %d, want 3", len(w.emb.passages))
	}
	for ref, want := range map[string]bool{"s.a": true, "s.b": true, "s.c": false, "s.d": false} {
		if got := w.doc(ref).Embedding != nil; got != want {
			t.Errorf("%s has vector = %v, want %v", ref, got, want)
		}
	}
	if len(w.refs()) != 4 {
		t.Fatalf("every document is still written: %v", w.refs())
	}
}

// Decided deviation vector-kept-on-failed-reembedding: the TypeScript server nulls the vector.
func TestIndexToolsAFailedRecomputeKeepsAnExistingVectorAndMarksTheRow(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "1", "b": "2"})
	if err := app.IndexTools(ctx, w.d, "s"); err != nil {
		t.Fatal(err)
	}
	before := w.doc("s.a").Embedding
	if before == nil {
		t.Fatal("no vector to begin with")
	}
	w.cat.tool("s", "a").Description = "changed text"
	w.failAll()
	if err := app.IndexTools(ctx, w.d, "s"); !errors.Is(err, errEmbedder) {
		t.Fatalf("err = %v", err)
	}
	d := w.doc("s.a")
	if !slices.Equal(d.Embedding, before) {
		t.Fatal("the old vector was not kept")
	}
	if d.Description != "changed text" {
		t.Fatalf("the text columns are not updated: %+v", d)
	}
	if !w.isStale("s.a") || !w.isStale("s.b") {
		t.Fatal("the rows are not marked as needing a vector")
	}
}

func TestIndexToolsAFailedFirstEmbeddingStoresTheRowWithoutAVectorAndWithoutTheKeepPath(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "1"})
	w.failAll()
	_ = app.IndexTools(ctx, w.d, "s")
	if d := w.doc("s.a"); d.Ref == "" || d.Embedding != nil {
		t.Fatalf("document = %+v", d)
	}
	if w.isStale("s.a") {
		t.Fatal("a row that never had a vector is not stale")
	}
}

func TestIndexToolsOrderRemovalThenAllEmbeddingsThenAllUpserts(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "1", "b": "2"})
	w.seed("s.old", true)
	if err := app.IndexTools(ctx, w.d, "s"); err != nil {
		t.Fatal(err)
	}
	want := []string{"catalog s", "remove s.old", "embed 1", "embed 2", "upsert s.a", "upsert s.b"}
	if got := w.j.all(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v", got)
	}
}

func TestIndexToolsRemovalIsNotBlockedByAnEmbedderFailure(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "1"})
	w.seed("s.old", true)
	w.failAll()
	_ = app.IndexTools(ctx, w.d, "s")
	if got := w.refs(); !slices.Equal(got, []string{"s.a"}) {
		t.Fatalf("refs = %v", got)
	}
}

func TestIndexToolsAWrongLengthVectorIsAnEmbedderFailure(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "1"})
	w.emb.short = true
	if err := app.IndexTools(ctx, w.d, "s"); !errors.Is(err, app.ErrBadDimensions) {
		t.Fatalf("err = %v", err)
	}
	if w.doc("s.a").Ref == "" || w.doc("s.a").Embedding != nil {
		t.Fatalf("document = %+v", w.doc("s.a"))
	}
}

func TestIndexToolsPortErrors(t *testing.T) {
	boom := errors.New("boom")
	t.Run("catalog", func(t *testing.T) {
		w := newWorld(t)
		w.cat.errFor = boom
		if err := app.IndexTools(ctx, w.d, "s"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("refs of the server", func(t *testing.T) {
		w := newWorld(t)
		w.cat.put("s", map[string]string{"a": "1"})
		w.idx.errRefs = boom
		if err := app.IndexTools(ctx, w.d, "s"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("remove stops before embedding", func(t *testing.T) {
		w := newWorld(t)
		w.cat.put("s", map[string]string{"a": "1"})
		w.seed("s.old", true)
		w.idx.errRemove = boom
		if err := app.IndexTools(ctx, w.d, "s"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if len(w.emb.passages) != 0 {
			t.Fatal("embedded after a failed removal")
		}
	})
	t.Run("upsert wins over the embedder error", func(t *testing.T) {
		w := newWorld(t)
		w.cat.put("s", map[string]string{"a": "1"})
		w.failAll()
		w.idx.errUpsert = boom
		if err := app.IndexTools(ctx, w.d, "s"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestIndexToolsACancelledRunWritesNothing(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "1"})
	if err := app.IndexTools(ctx, w.d, "s"); err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(ctx)
	w.emb.fail = func(int, string) error { cancel(); return c.Err() }
	if err := app.IndexTools(c, w.d, "s"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if w.doc("s.a").Embedding == nil {
		t.Fatal("a cancelled run nulled a good vector")
	}
}

func TestIndexToolsAnInputSchemaThatIsNotJSONIsAnError(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "1"})
	w.cat.tool("s", "a").InputSchema = json.RawMessage(`{"properties":`)
	if err := app.IndexTools(ctx, w.d, "s"); err == nil {
		t.Fatal("want an error")
	}
	if len(w.emb.passages) != 0 {
		t.Fatal("embedded a tool with a broken schema")
	}
}

func TestIndexToolsLongPassageIsCutAtTwoThousandUnits(t *testing.T) {
	w := newWorld(t)
	long := make([]byte, 3000)
	for i := range long {
		long[i] = 'x'
	}
	w.cat.put("s", map[string]string{"a": string(long)})
	if err := app.IndexTools(ctx, w.d, "s"); err != nil {
		t.Fatal(err)
	}
	if n := len(w.emb.passages[0]); n != 2000 {
		t.Fatalf("passage length = %d", n)
	}
}

func TestIndexToolRefsKeepsTheCatalogOrderAndIgnoresUnofferedRefs(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "1", "b": "2"})
	w.seed("s.gone", true)
	if err := app.IndexToolRefs(ctx, w.d, "s", []string{"s.b", "s.gone", "s.a"}); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"s.a", "s.b"}) {
		t.Fatalf("refs = %v", got)
	}
	if want := []string{"catalog s", "remove s.gone", "embed 1", "embed 2", "upsert s.a", "upsert s.b"}; !slices.Equal(w.j.all(), want) {
		t.Fatalf("calls = %v", w.j.all())
	}
}

// Decided deviation stale-removal-rechecks-catalog: the TypeScript server deletes without asking.
func TestRemoveToolsDeletesOnlyWhatTheCatalogDoesNotOffer(t *testing.T) {
	w := newWorld(t)
	w.cat.put("g", map[string]string{"b": "back again"})
	w.seed("g.a", false)
	w.seed("g.b", true)
	if err := app.RemoveTools(ctx, w.d, "g", []string{"g.a", "g.b"}); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"g.b"}) {
		t.Fatalf("refs = %v: the live row must stay, the gone one go", got)
	}
	// replaying it changes nothing
	if err := app.RemoveTools(ctx, w.d, "g", []string{"g.a", "g.b"}); err != nil {
		t.Fatal(err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"g.b"}) {
		t.Fatalf("refs = %v", got)
	}
}

func TestRemoveToolsAnUnknownOrDisabledServerLosesEveryListedRef(t *testing.T) {
	w := newWorld(t)
	w.seed("ghost.a", true)
	w.seed("off.a", true)
	w.cat.put("off", map[string]string{}) // disabled: no tools
	if err := app.RemoveTools(ctx, w.d, "ghost", []string{"ghost.a"}); err != nil {
		t.Fatal(err)
	}
	if err := app.RemoveTools(ctx, w.d, "off", []string{"off.a"}); err != nil {
		t.Fatal(err)
	}
	if len(w.refs()) != 0 {
		t.Fatalf("refs = %v", w.refs())
	}
}

func TestRemoveToolsEmptyListDoesNotAskTheCatalog(t *testing.T) {
	w := newWorld(t)
	if err := app.RemoveTools(ctx, w.d, "g", nil); err != nil || len(w.j.all()) != 0 {
		t.Fatalf("err %v, calls %v", err, w.j.all())
	}
}

func TestRemoveToolsACatalogErrorFailsTheHandlerAndRemovesNothing(t *testing.T) {
	w := newWorld(t)
	w.seed("g.a", true)
	boom := errors.New("boom")
	w.cat.errFor = boom
	if err := app.RemoveTools(ctx, w.d, "g", []string{"g.a"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := w.refs(); !slices.Equal(got, []string{"g.a"}) {
		t.Fatalf("refs = %v", got)
	}
}

// keep the catalog import used even if the types above change
var _ = catalog.IndexedTool{}

func TestIndexToolsATitleIsStoredAsGivenNilStaysNilAndEmptyStaysEmpty(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"none": "x", "empty": "y", "some": "z"})
	empty, some := "", "T"
	w.cat.tool("s", "empty").Title = &empty
	w.cat.tool("s", "some").Title = &some
	if err := app.IndexTools(ctx, w.d, "s"); err != nil {
		t.Fatal(err)
	}
	if d := w.doc("s.none"); d.Title != nil {
		t.Fatalf("nil title stored as %q", *d.Title)
	}
	if d := w.doc("s.empty"); d.Title == nil || *d.Title != "" {
		t.Fatalf("an empty title (TypeScript keeps it as it is) = %v", d.Title)
	}
	if d := w.doc("s.some"); d.Title == nil || *d.Title != "T" {
		t.Fatalf("title = %v", d.Title)
	}
	// and the passage treats nil and "" alike
	if w.emb.passages[0] != "s. s server\nempty\ny" || w.emb.passages[1] != "s. s server\nnone\nx" {
		t.Fatalf("passages = %q", w.emb.passages)
	}
}

func TestIndexToolsASchemaErrorUpsertsNothingOfThatServerAndIsReturned(t *testing.T) {
	w := newWorld(t)
	w.cat.put("s", map[string]string{"a": "1", "b": "2", "c": "3"})
	w.cat.tool("s", "b").InputSchema = json.RawMessage(`{"properties":`)
	err := app.IndexTools(ctx, w.d, "s")
	if err == nil || errors.Is(err, errEmbedder) {
		t.Fatalf("err = %v", err)
	}
	if w.j.count("upsert") != 0 || len(w.refs()) != 0 {
		t.Fatalf("calls = %v", w.j.all())
	}
}
