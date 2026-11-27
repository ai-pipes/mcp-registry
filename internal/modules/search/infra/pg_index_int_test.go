//go:build integration

package infra

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func setupIndex(t *testing.T) (*pgxpool.Pool, *PgIndex) {
	t.Helper()
	pool := pgtest.StartMigrated(t)
	return pool, NewPgIndex(pool)
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// must is for values a test needs; a failed call panics, which fails the test with the cause.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func str(s string) *string { return &s }

// unit is the unit vector along dimension i.
func unit(i int) []float32 {
	v := make([]float32, app.Dimensions)
	v[i] = 1
	return v
}

// atCosine is a unit vector whose cosine with unit(0) is c (the rest of its length is on dimension 1).
func atCosine(c float64) []float32 {
	v := make([]float32, app.Dimensions)
	v[0] = float32(c)
	v[1] = float32(math.Sqrt(1 - c*c))
	return v
}

func put(t *testing.T, idx *PgIndex, ref, server string, embedding []float32) {
	t.Helper()
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: ref, Server: server, Description: "d " + ref, Embedding: embedding}))
}

func refsOf(cs []domain.Candidate) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Ref)
	}
	return out
}

func storedEmbedding(t *testing.T, pool *pgxpool.Pool, ref string) []float32 {
	t.Helper()
	var text *string
	mustOK(t, pool.QueryRow(t.Context(), `select embedding::text from search.documents where ref = $1`, ref).Scan(&text))
	if text == nil {
		return nil
	}
	return must(parseVector(*text))
}

func TestTestDatabaseHasPgvectorAndTheSearchSchema(t *testing.T) {
	pool, _ := setupIndex(t)
	var ext string
	mustOK(t, pool.QueryRow(t.Context(), `select extversion from pg_extension where extname = 'vector'`).Scan(&ext))
	var cols int
	mustOK(t, pool.QueryRow(t.Context(), `select count(*) from information_schema.columns where table_schema = 'search' and table_name = 'documents'`).Scan(&cols))
	if cols != 9 {
		t.Fatalf("search.documents has %d columns (pgvector %s)", cols, ext)
	}
}

func TestUpsertStoresTheDocumentAndComputesTsvInSQL(t *testing.T) {
	pool, idx := setupIndex(t)
	mustOK(t, idx.Upsert(t.Context(), app.Document{
		Ref: "gitea.issues_create", Server: "gitea", Title: str("Create issue"),
		Description: "Creates a new issue", Body: "owner repo title", Embedding: unit(3),
	}))
	var server, desc, body, tsv string
	var title *string
	var updated time.Time
	mustOK(t, pool.QueryRow(t.Context(), `select server, title, description, body, tsv::text, updated_at from search.documents where ref = 'gitea.issues_create'`).Scan(&server, &title, &desc, &body, &tsv, &updated))
	if server != "gitea" || title == nil || *title != "Create issue" || desc != "Creates a new issue" || body != "owner repo title" {
		t.Fatalf("columns: %q %v %q %q", server, title, desc, body)
	}
	// The ref's dots and underscores became spaces (A), the english stems of the description are B, the body C.
	for _, want := range []string{"'gitea':1A", "'issues':2A", "'create':3A", "'issu':", "'owner':", "'repo':"} {
		if !strings.Contains(tsv, want) {
			t.Errorf("tsv %s lacks %s", tsv, want)
		}
	}
	if d := time.Since(updated); d < -time.Minute || d > time.Minute {
		t.Errorf("updated_at %v is not now", updated)
	}
	if got := storedEmbedding(t, pool, "gitea.issues_create"); !reflect.DeepEqual(got, unit(3)) {
		t.Errorf("embedding not stored")
	}
}

func TestUpsertNullTitleAndNullEmbedding(t *testing.T) {
	pool, idx := setupIndex(t)
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "a.x", Server: "a", Description: "d"}))
	var title *string
	var hasVec bool
	var body string
	mustOK(t, pool.QueryRow(t.Context(), `select title, embedding is not null, body from search.documents where ref = 'a.x'`).Scan(&title, &hasVec, &body))
	if title != nil || hasVec || body != "" {
		t.Fatalf("title %v, vector %v, body %q", title, hasVec, body)
	}
}

func TestUpsertReplacesEveryColumnByRef(t *testing.T) {
	pool, idx := setupIndex(t)
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "a.x", Server: "a", Title: str("T"), Description: "first", Body: "b1", Embedding: unit(0)}))
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "a.x", Server: "z", Description: "second", Body: "b2", Embedding: unit(1)}))
	var server, desc, body string
	var title *string
	var n int
	mustOK(t, pool.QueryRow(t.Context(), `select server, title, description, body, (select count(*) from search.documents) from search.documents where ref = 'a.x'`).Scan(&server, &title, &desc, &body, &n))
	if n != 1 || server != "z" || title != nil || desc != "second" || body != "b2" {
		t.Fatalf("%d rows: %q %v %q %q", n, server, title, desc, body)
	}
	if got := storedEmbedding(t, pool, "a.x"); !reflect.DeepEqual(got, unit(1)) {
		t.Fatal("embedding not replaced")
	}
}

// Upsert is the "set this vector" path: a nil Embedding stores NULL and keeps the mark. The
// application no longer takes it for a failed re-embedding (decided deviation
// vector-kept-on-failed-reembedding, see UpsertEmbedFailed below).
func TestUpsertWithoutAVectorStoresNullAndKeepsTheMarkAVectorClearsIt(t *testing.T) {
	pool, idx := setupIndex(t)
	put(t, idx, "a.x", "a", unit(0))
	mustOK(t, idx.MarkEmbedFailed(t.Context(), "a.x"))
	put(t, idx, "a.x", "a", nil)
	var hasVec, marked bool
	mustOK(t, pool.QueryRow(t.Context(), `select embedding is not null, embed_failed_at is not null from search.documents where ref = 'a.x'`).Scan(&hasVec, &marked))
	if hasVec || !marked {
		t.Fatalf("vector %v, marked %v; want no vector and a kept mark", hasVec, marked)
	}
	// A vector clears the mark.
	put(t, idx, "a.x", "a", unit(2))
	mustOK(t, pool.QueryRow(t.Context(), `select embedding is not null, embed_failed_at is not null from search.documents where ref = 'a.x'`).Scan(&hasVec, &marked))
	if !hasVec || marked {
		t.Fatalf("vector %v, marked %v; want a vector and no mark", hasVec, marked)
	}
}

func rowState(t *testing.T, pool *pgxpool.Pool, ref string) (vec *string, desc string, marked bool) {
	t.Helper()
	mustOK(t, pool.QueryRow(t.Context(), `select embedding::text, description, embed_failed_at is not null from search.documents where ref = $1`, ref).Scan(&vec, &desc, &marked))
	return vec, desc, marked
}

func TestUpsertEmbedFailedKeepsTheStoredVectorByteForByteUpdatesTextAndMarksTheRow(t *testing.T) {
	pool, idx := setupIndex(t)
	put(t, idx, "a.x", "a", atCosine(0.6))
	before, _, marked := rowState(t, pool, "a.x")
	if before == nil || marked {
		t.Fatalf("setup: vector %v, marked %v", before, marked)
	}
	mustOK(t, idx.UpsertEmbedFailed(t.Context(), app.Document{Ref: "a.x", Server: "z", Title: str("T"), Description: "new text", Body: "newbody"}))
	after, desc, marked := rowState(t, pool, "a.x")
	if after == nil || *after != *before {
		t.Fatalf("vector %v, want %v", after, before)
	}
	if desc != "new text" || !marked {
		t.Fatalf("description %q, marked %v", desc, marked)
	}
	var server string
	mustOK(t, pool.QueryRow(t.Context(), `select server from search.documents where ref = 'a.x'`).Scan(&server))
	if server != "z" {
		t.Fatalf("server %q", server)
	}
	if got := fts(t, idx, "newbody", "z"); !reflect.DeepEqual(got, []string{"a.x"}) {
		t.Fatalf("tsv not updated: %v", got)
	}
}

func TestUpsertEmbedFailedKeepsAnOlderMarkAndLeavesRowsWithoutAVectorAlone(t *testing.T) {
	pool, idx := setupIndex(t)
	put(t, idx, "a.x", "a", unit(0))
	mustOK(t, idx.UpsertEmbedFailed(t.Context(), app.Document{Ref: "a.x", Server: "a", Description: "one"}))
	_, err := pool.Exec(t.Context(), `update search.documents set embed_failed_at = now() - interval '3 hours' where ref = 'a.x'`)
	mustOK(t, err)
	mustOK(t, idx.UpsertEmbedFailed(t.Context(), app.Document{Ref: "a.x", Server: "a", Description: "two"}))
	var old bool
	mustOK(t, pool.QueryRow(t.Context(), `select embed_failed_at < now() - interval '2 hours' from search.documents where ref = 'a.x'`).Scan(&old))
	if !old {
		t.Fatal("a second failure moved the mark forward: an outage would back the row off for an hour at every attempt")
	}
	// A row without a vector (new, or already NULL) stays as the reference leaves it: no mark added.
	mustOK(t, idx.UpsertEmbedFailed(t.Context(), app.Document{Ref: "n.x", Server: "n", Description: "new"}))
	vec, _, marked := rowState(t, pool, "n.x")
	if vec != nil || marked {
		t.Fatalf("a new row: vector %v, marked %v", vec, marked)
	}
	mustOK(t, idx.UpsertEmbedFailed(t.Context(), app.Document{Ref: "n.x", Server: "n", Description: "again"}))
	if vec, desc, marked := rowState(t, pool, "n.x"); vec != nil || marked || desc != "again" {
		t.Fatalf("an existing NULL-vector row: %v %q %v", vec, desc, marked)
	}
}

func TestTheSweepOffersMarkedRowsThatKeptAVectorAfterTheBackoffAndASuccessfulUpsertClearsTheMark(t *testing.T) {
	pool, idx := setupIndex(t)
	put(t, idx, "s.keep", "s", unit(0))
	put(t, idx, "s.plain", "s", unit(1))
	mustOK(t, idx.UpsertEmbedFailed(t.Context(), app.Document{Ref: "s.keep", Server: "s", Description: "x"}))
	// Marked just now: backed off like any other failed row.
	if got := must(idx.WithoutEmbedding(t.Context(), 10)); len(got) != 0 {
		t.Fatalf("offered inside the back-off: %v", refsOf(got))
	}
	_, err := pool.Exec(t.Context(), `update search.documents set embed_failed_at = now() - interval '2 hours' where ref = 's.keep'`)
	mustOK(t, err)
	got := must(idx.WithoutEmbedding(t.Context(), 10))
	if !reflect.DeepEqual(refsOf(got), []string{"s.keep"}) || got[0].Server != "s" {
		t.Fatalf("got %v; want only the marked row, not the plain vector row", got)
	}
	// Order: never attempted (NULL vector) first, then the marked one.
	put(t, idx, "s.null", "s", nil)
	if got := must(idx.WithoutEmbedding(t.Context(), 10)); !reflect.DeepEqual(refsOf(got), []string{"s.null", "s.keep"}) {
		t.Fatalf("order %v", refsOf(got))
	}
	put(t, idx, "s.keep", "s", unit(5))
	if _, _, marked := rowState(t, pool, "s.keep"); marked {
		t.Fatal("a vector did not clear the mark")
	}
	if got := must(idx.WithoutEmbedding(t.Context(), 10)); !reflect.DeepEqual(refsOf(got), []string{"s.null"}) {
		t.Fatalf("after the refresh: %v", refsOf(got))
	}
}

func TestUpsertRefusesAVectorThatIsNotFiniteWithoutTouchingTheRow(t *testing.T) {
	pool, idx := setupIndex(t)
	put(t, idx, "a.x", "a", unit(0))
	bad := unit(1)
	bad[5] = float32(math.NaN())
	if err := idx.Upsert(t.Context(), app.Document{Ref: "a.x", Server: "a", Description: "new", Embedding: bad}); err == nil {
		t.Fatal("NaN accepted")
	}
	var desc string
	mustOK(t, pool.QueryRow(t.Context(), `select description from search.documents where ref = 'a.x'`).Scan(&desc))
	if desc != "d a.x" {
		t.Fatalf("row changed: %q", desc)
	}
	if _, err := idx.VectorCandidates(t.Context(), bad, 5, 0, []string{"a"}); err == nil {
		t.Fatal("NaN query vector accepted")
	}
}

func TestUpsertWithAVectorOfTheWrongLengthIsADatabaseError(t *testing.T) {
	_, idx := setupIndex(t)
	if err := idx.Upsert(t.Context(), app.Document{Ref: "a.x", Server: "a", Description: "d", Embedding: []float32{1, 2, 3}}); err == nil {
		t.Fatal("3 dimensions accepted")
	}
}

func TestRowsWrittenByHandAsTypeScriptWritesThemAreRead(t *testing.T) {
	pool, idx := setupIndex(t)
	// The TypeScript test's ghost row, and a row with the vector literal JavaScript produces.
	_, err := pool.Exec(t.Context(), `insert into search.documents (ref, server, description, tsv)
		values ('ghost.x', 'ghost', 'd', to_tsvector('simple', 'x'))`)
	mustOK(t, err)
	var parts []string
	for i := 0; i < app.Dimensions; i++ {
		if i == 7 {
			parts = append(parts, "1")
		} else {
			parts = append(parts, "0")
		}
	}
	_, err = pool.Exec(t.Context(), `insert into search.documents (ref, server, description, tsv, embedding)
		values ('js.y', 'js', 'd', to_tsvector('simple', 'y'), $1::vector)`, "["+strings.Join(parts, ",")+"]")
	mustOK(t, err)

	if n := must(idx.Count(t.Context())); n != 2 {
		t.Fatalf("count %d", n)
	}
	if got := must(idx.Servers(t.Context())); !reflect.DeepEqual(got, []string{"ghost", "js"}) {
		t.Fatalf("servers %v", got)
	}
	if got := must(idx.RefsOfServer(t.Context(), "ghost")); !reflect.DeepEqual(got, []string{"ghost.x"}) {
		t.Fatalf("refs %v", got)
	}
	// The ghost has no vector: a sweep candidate; the JS row has one: a vector candidate.
	if got := must(idx.WithoutEmbedding(t.Context(), 10)); !reflect.DeepEqual(refsOf(got), []string{"ghost.x"}) {
		t.Fatalf("without embedding %v", got)
	}
	got := must(idx.VectorCandidates(t.Context(), unit(7), 5, 0.5, []string{"ghost", "js"}))
	if !reflect.DeepEqual(got, []domain.Candidate{{Ref: "js.y", Server: "js"}}) {
		t.Fatalf("vector candidates %v", got)
	}
	// A hand-written row is replaced by an upsert like any other.
	put(t, idx, "ghost.x", "ghost", unit(1))
	if got := must(idx.WithoutEmbedding(t.Context(), 10)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestRemoveDeletesByRefsIgnoresUnknownAndEmpty(t *testing.T) {
	_, idx := setupIndex(t)
	for _, r := range []string{"a.1", "a.2", "a.3", "b.1"} {
		put(t, idx, r, strings.SplitN(r, ".", 2)[0], nil)
	}
	mustOK(t, idx.Remove(t.Context(), nil))
	mustOK(t, idx.Remove(t.Context(), []string{}))
	if n := must(idx.Count(t.Context())); n != 4 {
		t.Fatalf("an empty list removed rows: %d left", n)
	}
	mustOK(t, idx.Remove(t.Context(), []string{"a.1", "a.3", "nope.x"}))
	if got := must(idx.RefsOfServer(t.Context(), "a")); !reflect.DeepEqual(got, []string{"a.2"}) {
		t.Fatalf("%v", got)
	}
	if n := must(idx.Count(t.Context())); n != 2 {
		t.Fatalf("count %d", n)
	}
}

func TestServersAreDistinctAndRefsOfServerOrdered(t *testing.T) {
	_, idx := setupIndex(t)
	for _, r := range []string{"yt.b", "gitea.z", "yt.a", "gitea.a", "yt.c"} {
		put(t, idx, r, strings.SplitN(r, ".", 2)[0], nil)
	}
	if got := must(idx.Servers(t.Context())); !reflect.DeepEqual(got, []string{"gitea", "yt"}) {
		t.Fatalf("servers %v", got)
	}
	if got := must(idx.RefsOfServer(t.Context(), "yt")); !reflect.DeepEqual(got, []string{"yt.a", "yt.b", "yt.c"}) {
		t.Fatalf("refs %v", got)
	}
	if got := must(idx.RefsOfServer(t.Context(), "none")); got == nil || len(got) != 0 {
		t.Fatalf("an unknown server gives %#v, want an empty list", got)
	}
}

func TestEmptyTableReads(t *testing.T) {
	_, idx := setupIndex(t)
	if n := must(idx.Count(t.Context())); n != 0 {
		t.Fatalf("count %d", n)
	}
	if got := must(idx.Servers(t.Context())); got == nil || len(got) != 0 {
		t.Fatalf("%#v", got)
	}
	if got := must(idx.WithoutEmbedding(t.Context(), 5)); got == nil || len(got) != 0 {
		t.Fatalf("%#v", got)
	}
}

func TestWithoutEmbeddingOrderAndHourlyBackoff(t *testing.T) {
	pool, idx := setupIndex(t)
	for _, r := range []string{"s.d", "s.c", "s.b", "s.a", "s.vec"} {
		var emb []float32
		if r == "s.vec" {
			emb = unit(0)
		}
		put(t, idx, r, "s", emb)
	}
	// s.a failed 3 hours ago, s.b 2 hours ago, s.c a minute ago, s.d never.
	for ref, age := range map[string]string{"s.a": "3 hours", "s.b": "2 hours", "s.c": "1 minute"} {
		_, err := pool.Exec(t.Context(), `update search.documents set embed_failed_at = now() - $2::interval where ref = $1`, ref, age)
		mustOK(t, err)
	}
	got := must(idx.WithoutEmbedding(t.Context(), 10))
	// Never attempted first, then the oldest failure; the one failed a minute ago waits; the vector one is not a candidate.
	if want := []string{"s.d", "s.a", "s.b"}; !reflect.DeepEqual(refsOf(got), want) {
		t.Fatalf("got %v, want %v", refsOf(got), want)
	}
	if got[0].Server != "s" {
		t.Fatalf("server %q", got[0].Server)
	}
	if got := must(idx.WithoutEmbedding(t.Context(), 2)); !reflect.DeepEqual(refsOf(got), []string{"s.d", "s.a"}) {
		t.Fatalf("limit 2: %v", refsOf(got))
	}
	// Never attempted rows are ordered by ref.
	put(t, idx, "s.0", "s", nil)
	if got := must(idx.WithoutEmbedding(t.Context(), 2)); !reflect.DeepEqual(refsOf(got), []string{"s.0", "s.d"}) {
		t.Fatalf("never attempted by ref: %v", refsOf(got))
	}
}

func TestMarkEmbedFailedSkipsTheDocumentForAnHour(t *testing.T) {
	pool, idx := setupIndex(t)
	put(t, idx, "g.one", "g", nil)
	mustOK(t, idx.MarkEmbedFailed(t.Context(), "g.one"))
	if got := must(idx.WithoutEmbedding(t.Context(), 10)); len(got) != 0 {
		t.Fatalf("a document marked just now is offered: %v", got)
	}
	_, err := pool.Exec(t.Context(), `update search.documents set embed_failed_at = now() - interval '2 hours'`)
	mustOK(t, err)
	if got := must(idx.WithoutEmbedding(t.Context(), 10)); len(got) != 1 {
		t.Fatalf("after two hours: %v", got)
	}
	// An unknown ref is not an error.
	mustOK(t, idx.MarkEmbedFailed(t.Context(), "nope"))
}

func fts(t *testing.T, idx *PgIndex, query string, servers ...string) []string {
	t.Helper()
	return refsOf(must(idx.FTSCandidates(t.Context(), query, 50, servers)))
}

func TestFTSNamesWithDotsDashesAndUnderscores(t *testing.T) {
	_, idx := setupIndex(t)
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "gitea.issues.create", Server: "gitea", Description: "x"}))
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "gitea.get-file_contents", Server: "gitea", Description: "y"}))
	if got := fts(t, idx, "issues create", "gitea"); !reflect.DeepEqual(got, []string{"gitea.issues.create"}) {
		t.Fatalf("%v", got)
	}
	if got := fts(t, idx, "file contents", "gitea"); !reflect.DeepEqual(got, []string{"gitea.get-file_contents"}) {
		t.Fatalf("%v", got)
	}
}

func TestFTSEnglishAndRussianStems(t *testing.T) {
	_, idx := setupIndex(t)
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "s.en", Server: "s", Description: "Creates a new issue in the tracker"}))
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "s.ru", Server: "s", Description: "Создаёт новую задачу в трекере"}))
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "s.body", Server: "s", Description: "nothing", Body: "assignee priority"}))
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"creating issues", []string{"s.en"}},
		{"задачи", []string{"s.ru"}},
		{"задачей", []string{"s.ru"}},
		{"assignee", []string{"s.body"}},
		{"unrelated words", []string{}},
	} {
		if got := fts(t, idx, tc.query, "s"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestFTSUsesWebsearchSyntaxAndWeights(t *testing.T) {
	_, idx := setupIndex(t)
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "s.cat", Server: "s", Description: "cat tool"}))
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "s.dog", Server: "s", Description: "dog tool"}))
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "s.both", Server: "s", Description: "cat and dog tool"}))
	if got := fts(t, idx, "cat or dog", "s"); len(got) != 3 {
		t.Errorf("or: %v", got)
	}
	if got := fts(t, idx, "cat -dog", "s"); !reflect.DeepEqual(got, []string{"s.cat"}) {
		t.Errorf("not: %v", got)
	}
	if got := fts(t, idx, `"cat and dog"`, "s"); !reflect.DeepEqual(got, []string{"s.both"}) {
		t.Errorf("phrase: %v", got)
	}
	// Weight A (ref/title) outranks B (description) and C (body).
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "w.a", Server: "w", Title: str("widget"), Description: "x"}))
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "w.b", Server: "w", Description: "widget"}))
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "w.c", Server: "w", Description: "x", Body: "widget"}))
	if got := fts(t, idx, "widget", "w"); !reflect.DeepEqual(got, []string{"w.a", "w.b", "w.c"}) {
		t.Errorf("weights: %v", got)
	}
}

func TestFTSTiesAreOrderedByRefAndLimited(t *testing.T) {
	_, idx := setupIndex(t)
	for _, r := range []string{"s.c", "s.a", "s.b", "s.d"} {
		mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: r, Server: "s", Description: "same words here"}))
	}
	if got := fts(t, idx, "same words", "s"); !reflect.DeepEqual(got, []string{"s.a", "s.b", "s.c", "s.d"}) {
		t.Fatalf("%v", got)
	}
	got := must(idx.FTSCandidates(t.Context(), "same words", 2, []string{"s"}))
	if !reflect.DeepEqual(got, []domain.Candidate{{Ref: "s.a", Server: "s"}, {Ref: "s.b", Server: "s"}}) {
		t.Fatalf("limit 2: %v", got)
	}
}

func TestFTSServersFilterAppliesBeforeTheLimit(t *testing.T) {
	_, idx := setupIndex(t)
	// 60 documents of another server that rank higher (the term is in their ref, weight A) must not
	// eat the limit of 50.
	for i := 0; i < 60; i++ {
		mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: fmt.Sprintf("other.widget%02d", i), Server: "other", Title: str("widget widget"), Description: "widget"}))
	}
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "mine.low", Server: "mine", Description: "a widget"}))
	got := must(idx.FTSCandidates(t.Context(), "widget", 50, []string{"mine"}))
	if !reflect.DeepEqual(got, []domain.Candidate{{Ref: "mine.low", Server: "mine"}}) {
		t.Fatalf("%v", got)
	}
	if got := must(idx.FTSCandidates(t.Context(), "widget", 50, []string{"mine", "other"})); len(got) != 50 {
		t.Fatalf("two servers, limit 50: %d", len(got))
	}
}

func TestFTSNoServersNoQueryAndRowsWithoutVectorsAreCandidates(t *testing.T) {
	_, idx := setupIndex(t)
	put(t, idx, "s.novec", "s", nil)
	got := must(idx.FTSCandidates(t.Context(), "novec", 5, nil))
	if got == nil || len(got) != 0 {
		t.Fatalf("no servers: %#v", got)
	}
	// A query websearch_to_tsquery cannot parse into anything still works (no error, no hits).
	if got := fts(t, idx, "!!! ---", "s"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// NULL embedding: found by text.
	if got := fts(t, idx, "novec", "s"); !reflect.DeepEqual(got, []string{"s.novec"}) {
		t.Fatalf("%v", got)
	}
}

func vec(t *testing.T, idx *PgIndex, query []float32, limit int, floor float64, servers ...string) []string {
	t.Helper()
	return refsOf(must(idx.VectorCandidates(t.Context(), query, limit, floor, servers)))
}

func TestVectorCandidatesOrderByDistanceAndNeverReturnRowsWithoutVectors(t *testing.T) {
	_, idx := setupIndex(t)
	put(t, idx, "s.far", "s", atCosine(0.6))
	put(t, idx, "s.best", "s", atCosine(0.99))
	put(t, idx, "s.mid", "s", atCosine(0.9))
	put(t, idx, "s.novec", "s", nil)
	got := must(idx.VectorCandidates(t.Context(), unit(0), 10, 0, []string{"s"}))
	if want := []string{"s.best", "s.mid", "s.far"}; !reflect.DeepEqual(refsOf(got), want) {
		t.Fatalf("%v, want %v", refsOf(got), want)
	}
	if got[0].Server != "s" {
		t.Fatalf("server %q", got[0].Server)
	}
	if got := vec(t, idx, unit(0), 2, 0, "s"); !reflect.DeepEqual(got, []string{"s.best", "s.mid"}) {
		t.Fatalf("limit 2: %v", got)
	}
}

func TestVectorTiesAreOrderedByRef(t *testing.T) {
	_, idx := setupIndex(t)
	for _, r := range []string{"s.c", "s.a", "s.d", "s.b"} {
		put(t, idx, r, "s", atCosine(0.9))
	}
	if got := vec(t, idx, unit(0), 10, 0, "s"); !reflect.DeepEqual(got, []string{"s.a", "s.b", "s.c", "s.d"}) {
		t.Fatalf("%v", got)
	}
}

func TestVectorSimilarityFloorBoundary(t *testing.T) {
	pool, idx := setupIndex(t)
	put(t, idx, "s.edge", "s", atCosine(0.9))
	put(t, idx, "s.better", "s", atCosine(0.95))
	// The database's own similarity of the stored (float4) vector, as the query computes it.
	lit := must(vectorLiteral(unit(0)))
	var sim float64
	mustOK(t, pool.QueryRow(t.Context(), `select 1 - (embedding <=> $1::text::vector)::float8 from search.documents where ref = 's.edge'`, lit).Scan(&sim))
	if got := vec(t, idx, unit(0), 10, sim, "s"); !reflect.DeepEqual(got, []string{"s.better", "s.edge"}) {
		t.Errorf("a candidate exactly at the floor must pass: %v", got)
	}
	if got := vec(t, idx, unit(0), 10, math.Nextafter(sim, 2), "s"); !reflect.DeepEqual(got, []string{"s.better"}) {
		t.Errorf("just above the candidate's similarity it must not pass: %v", got)
	}
	if got := vec(t, idx, unit(0), 10, 0.97, "s"); len(got) != 0 {
		t.Errorf("above everything: %v", got)
	}
	// The floor applies after the limit, as in TypeScript: limit 1 takes the nearest, then filters.
	if got := vec(t, idx, unit(0), 1, 0.92, "s"); !reflect.DeepEqual(got, []string{"s.better"}) {
		t.Errorf("limit 1: %v", got)
	}
}

func TestVectorServersFilterAppliesBeforeTheLimit(t *testing.T) {
	_, idx := setupIndex(t)
	for i := 0; i < 60; i++ {
		put(t, idx, fmt.Sprintf("other.t%02d", i), "other", atCosine(0.99))
	}
	put(t, idx, "mine.a", "mine", atCosine(0.8))
	put(t, idx, "mine.b", "mine", atCosine(0.7))
	got := must(idx.VectorCandidates(t.Context(), unit(0), 50, 0.5, []string{"mine"}))
	if want := []string{"mine.a", "mine.b"}; !reflect.DeepEqual(refsOf(got), want) {
		t.Fatalf("%v, want %v", refsOf(got), want)
	}
	if got := vec(t, idx, unit(0), 50, 0.5, "mine", "other"); len(got) != 50 {
		t.Fatalf("both servers: %d", len(got))
	}
}

func TestVectorNoServersNoQuery(t *testing.T) {
	_, idx := setupIndex(t)
	put(t, idx, "s.a", "s", unit(0))
	got := must(idx.VectorCandidates(t.Context(), unit(0), 5, 0, nil))
	if got == nil || len(got) != 0 {
		t.Fatalf("%#v", got)
	}
	// With no servers a bad vector is not even looked at: the answer is empty before any work.
	if got := must(idx.VectorCandidates(t.Context(), []float32{float32(math.NaN())}, 5, 0, []string{})); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

// onePool is a pool of one connection whose planner may not use a sequential scan: the vector
// query then has to go through the HNSW index, where the iterative scan matters.
func onePool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config().Copy()
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["enable_seqscan"] = "off"
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	mustOK(t, err)
	t.Cleanup(p.Close)
	return p
}

// TestVectorIterativeScanSetLocalIsInTheQueryTransaction: 150 documents of "near" are all closer to
// the query than the 5 of "far", and the filter selects "far". Through the HNSW index with the
// default ef_search of 40 the walk meets only "near" documents and finds nothing; the iterative
// scan keeps walking until it has the matching rows. If `set local` ran outside the query's
// transaction (as a plain pool.Exec would: Postgres only warns that SET LOCAL is outside a
// transaction and does nothing) the query would return no rows and this test fails. The pool has
// one connection, so a second connection cannot be the one that carried the setting either: the
// query would then wait for the first for ever and the context deadline fails the test.
func TestVectorIterativeScanSetLocalIsInTheQueryTransaction(t *testing.T) {
	pool, idx := setupIndex(t)
	// Documents spread in random directions (a connected graph), at cosines 0.5-0.9 to the query for
	// "near" and 0.1-0.3 for "far".
	seq := 0 // a deterministic noise source (no math/rand: gosec)
	noise := func() float64 {
		seq++
		y := math.Sin(float64(seq)*12.9898) * 43758.5453
		return y - math.Floor(y) - 0.5
	}
	spread := func(c float64) []float32 {
		v := make([]float32, app.Dimensions)
		var norm float64
		for i := 1; i < app.Dimensions; i++ {
			x := noise()
			v[i] = float32(x)
			norm += x * x
		}
		rest := math.Sqrt(1-c*c) / math.Sqrt(norm)
		for i := 1; i < app.Dimensions; i++ {
			v[i] *= float32(rest)
		}
		v[0] = float32(c)
		return v
	}
	for i := 0; i < 150; i++ {
		put(t, idx, fmt.Sprintf("near.t%03d", i), "near", spread(0.5+0.4*float64(i)/150))
	}
	for i := 0; i < 5; i++ {
		put(t, idx, fmt.Sprintf("far.t%d", i), "far", spread(0.1+0.05*float64(i)))
	}
	// Without the btree index on server the planner has no other ordered path than HNSW (with a
	// selective filter it would pick that index and sort, and the iterative scan would not matter).
	_, err := pool.Exec(t.Context(), `drop index search.documents_server_idx`)
	mustOK(t, err)
	one := onePool(t, pool)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// Control: the planner really goes through the index here.
	rows, err := one.Query(ctx, `explain select ref from search.documents where embedding is not null and server = 'far' order by embedding <=> $1::text::vector limit 50`, must(vectorLiteral(unit(0))))
	mustOK(t, err)
	var plan []string
	for rows.Next() {
		var line string
		mustOK(t, rows.Scan(&line))
		plan = append(plan, line)
	}
	mustOK(t, rows.Err())
	if !strings.Contains(strings.Join(plan, "\n"), "documents_embedding_idx") {
		t.Fatalf("the HNSW index is not used:\n%s", strings.Join(plan, "\n"))
	}

	got, err := NewPgIndex(one).VectorCandidates(ctx, unit(0), 50, 0, []string{"far"})
	mustOK(t, err)
	if want := []string{"far.t4", "far.t3", "far.t2", "far.t1", "far.t0"}; !reflect.DeepEqual(refsOf(got), want) {
		t.Fatalf("%v, want %v", refsOf(got), want)
	}
	// SET LOCAL ends with the transaction: the connection is back to the default.
	var setting string
	mustOK(t, one.QueryRow(ctx, `show hnsw.iterative_scan`).Scan(&setting))
	if setting != "off" {
		t.Fatalf("hnsw.iterative_scan is %q on the pooled connection after the call", setting)
	}
}

// jsVectorComponent is one float32 by bit pattern and the text JavaScript's number-to-string gives
// for it (String(new Float32Array(...)[0]), what `[v.join(',')]` writes in the TypeScript server for
// a vector from tolist()).
var jsVectorComponents = []struct {
	bits uint32
	js   string
}{
	{0x00000000, "0"},
	{0x80000000, "0"},
	{0x00000001, "1.401298464324817e-45"},
	{0x80000001, "-1.401298464324817e-45"},
	{0x007fffff, "1.1754942106924411e-38"},
	{0x00800000, "1.1754943508222875e-38"},
	{0x7f7fffff, "3.4028234663852886e+38"},
	{0xff7fffff, "-3.4028234663852886e+38"},
	{0x3f800000, "1"},
	{0xbf800000, "-1"},
	{0x3dcccccd, "0.10000000149011612"},
	{0xbdcccccd, "-0.10000000149011612"},
	{0x33d6bf95, "1.0000000116860974e-7"},
	{0x358637bd, "9.999999974752427e-7"},
	{0xb3d6bf95, "-1.0000000116860974e-7"},
	{0x3e800000, "0.25"},
	{0x3eaaaaab, "0.3333333432674408"},
	{0x3f7fffff, "0.9999999403953552"},
	{0x3f800001, "1.0000001192092896"},
	{0x4b800000, "16777216"},
	{0x49742400, "1000000"},
	{0x4cbebc20, "100000000"},
	{0x5f5e0ff0, "16001271883861328000"},
	{0x2edbe6ff, "1.000000013351432e-10"},
	{0x1a000000, "2.6469779601696886e-23"},
	{0x7e967699, "9.999999680285692e+37"},
	{0x0ca2b3f1, "2.5068350373799834e-31"},
	{0x05391c44, "8.703849771477109e-36"},
	{0x043c7ad3, "2.2155691910804625e-36"},
	{0x8b0c4216, "-2.701273675498933e-32"},
	{0xa289127d, "-3.7153499636824594e-18"},
	{0xe8f7b1b8, "-9.357622828537215e+24"},
	{0x1cca49b7, "1.3386293539359232e-21"},
	{0x7ef29baa, "1.6124071450132758e+38"},
	{0x8c609701, "-1.730177338682937e-31"},
	{0x989a046c, "-3.981253193183801e-24"},
	{0xc89034db, "-295334.84375"},
	{0x0a33e47e, "8.661511105231005e-33"},
	{0xbe69c1c5, "-0.2282782346010208"},
	{0xf73f4860, "-3.879674372559439e+33"},
	{0x12dbe03f, "1.3876123989630846e-27"},
	{0xb3197092, "-3.5725470581837726e-8"},
	{0x75d5d6c9, "5.4214628485535406e+32"},
	{0x657ff194, "7.554123643609007e+22"},
	{0xf5a32fe3, "-4.137283427663278e+32"},
	{0x67f7d3e6, "2.3406667150299676e+24"},
	{0xf3e85a0d, "-3.6817606140457805e+31"},
	{0xfe61b408, "-7.500276868635479e+37"},
	{0xa9e847c7, "-1.0315321052846091e-13"},
	{0x6c42e27a, "9.424044573325744e+26"},
	{0x498b83bc, "1142903.5"},
	{0x6c5d8beb, "1.071333400243237e+27"},
	{0xace1b04e, "-6.414458374182086e-12"},
	{0x27c1fb55, "5.384075567246025e-15"},
	{0x16b994b0, "2.9982196981689735e-25"},
	{0xda49a04f, "-14188182870425600"},
	{0x4fc29162, "6528615424"},
	{0x01cf6159, "7.617947785493448e-38"},
	{0x76715ae4, "1.2238154529221439e+33"},
	{0x4aab68f3, "5616761.5"},
	{0x92af19b6, "-1.1050390908541144e-27"},
	{0xf657c59d, "-1.0940936504867403e+33"},
	{0x03f58a58, "1.443156796487704e-36"},
	{0xc79e09d7, "-80915.6796875"},
}

// TestGoVectorLiteralStoresTheSameFloat4AsJavaScripts writes 384 awkward components through Upsert
// (Go's shortest-float32 literal) and the same values through the literal the TypeScript server
// would send (the JavaScript number-to-string of each double, computed with node and pasted above),
// and compares what pgvector stored.
func TestGoVectorLiteralStoresTheSameFloat4AsJavaScripts(t *testing.T) {
	pool, idx := setupIndex(t)
	n := len(jsVectorComponents)
	goVec := make([]float32, app.Dimensions)
	var js []string
	for i := 0; i < app.Dimensions; i++ {
		c := jsVectorComponents[(i*7)%n] // a different arrangement from the table's, every entry used (7 and 64 are coprime)
		goVec[i] = math.Float32frombits(c.bits)
		js = append(js, c.js)
	}
	mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: "go.v", Server: "x", Description: "d", Embedding: goVec}))
	_, err := pool.Exec(t.Context(), `insert into search.documents (ref, server, description, tsv, embedding)
		values ('js.v', 'x', 'd', to_tsvector('simple', 'x'), $1::vector)`, "["+strings.Join(js, ",")+"]")
	mustOK(t, err)
	var same bool
	var goText, jsText string
	mustOK(t, pool.QueryRow(t.Context(), `select a.embedding = b.embedding, a.embedding::text, b.embedding::text
		from search.documents a, search.documents b where a.ref = 'go.v' and b.ref = 'js.v'`).Scan(&same, &goText, &jsText))
	if goText != jsText || !same {
		t.Fatalf("stored vectors differ:\ngo %.200s\njs %.200s", goText, jsText)
	}
	// And both are the values that went in (negative zero is stored as zero by both).
	stored := must(parseVector(goText))
	for i, x := range goVec {
		want := x
		if want == 0 {
			want = 0 // -0 -> +0
		}
		if math.Float32bits(stored[i]) != math.Float32bits(want) {
			t.Fatalf("component %d: %08x went in, %08x stored", i, math.Float32bits(x), math.Float32bits(stored[i]))
		}
	}
	// Every table entry was used.
	used := map[int]bool{}
	for i := 0; i < app.Dimensions; i++ {
		used[(i*7)%n] = true
	}
	if len(used) != n {
		t.Fatalf("only %d of %d patterns used", len(used), n)
	}
}

func TestVectorCandidatesAgreeWithAnUpsertedVectorAcrossFormats(t *testing.T) {
	_, idx := setupIndex(t)
	q := make([]float32, app.Dimensions)
	for i := range q {
		q[i] = float32(math.Sin(float64(i))) / 20
	}
	put(t, idx, "s.same", "s", q)
	if got := vec(t, idx, q, 5, 0.999999, "s"); !slices.Equal(got, []string{"s.same"}) {
		t.Fatalf("a vector is not its own nearest neighbour: %v", got)
	}
}

// referenceFTS is the TypeScript statement of ftsCandidates (v1.0.1:apps/server/src/modules/search/infrastructure/pg-search-index.ts), verbatim except
// for the rank expression, which the test substitutes to prove its data tells the variants apart.
const referenceFTS = `
with q as (
  select websearch_to_tsquery('russian', $1::text)
    || websearch_to_tsquery('english', $1::text)
    || websearch_to_tsquery('simple', $1::text) as q
)
select d.ref from search.documents d, q
where d.tsv @@ q.q and d.server = any($2::text[])
order by %s desc, d.ref
limit 50`

func orderBy(t *testing.T, pool *pgxpool.Pool, rank, query string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), strings.Replace(referenceFTS, "%s", rank, 1), query, []string{"s"})
	mustOK(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	mustOK(t, err)
	return out
}

// TestFTSRankIsCoverDensityWithoutNormalisation pins ts_rank_cd(tsv, q) with no flags. The three
// documents are ordered differently by ts_rank and by ts_rank_cd with normalisation 1 (length), so
// either substitution in the repository's SQL is noticed; the expected order is what the
// TypeScript statement returns, run here as a literal.
func TestFTSRankIsCoverDensityWithoutNormalisation(t *testing.T) {
	pool, idx := setupIndex(t)
	filler := strings.Repeat("lorem ipsum dolor sit amet ", 12)
	for ref, desc := range map[string]string{
		"s.short":   "widget lorem gadget",
		"s.long":    filler + "widget gadget " + filler,
		"s.scatter": "widget " + filler + "gadget " + filler + "widget " + filler + "gadget",
	} {
		mustOK(t, idx.Upsert(t.Context(), app.Document{Ref: ref, Server: "s", Description: desc}))
	}
	const q = "widget gadget"
	reference := orderBy(t, pool, "ts_rank_cd(d.tsv, q.q)", q)
	for _, variant := range []string{"ts_rank(d.tsv, q.q)", "ts_rank_cd(d.tsv, q.q, 1)"} {
		if slices.Equal(orderBy(t, pool, variant, q), reference) {
			t.Fatalf("the data does not tell %s from the reference: %v", variant, reference)
		}
	}
	if !slices.Equal(reference, []string{"s.long", "s.short", "s.scatter"}) {
		t.Fatalf("reference order %v", reference)
	}
	if got := fts(t, idx, q, "s"); !slices.Equal(got, reference) {
		t.Fatalf("FTSCandidates %v, the TypeScript statement %v", got, reference)
	}
}

// referenceUpsert is the TypeScript upsert (v1.0.1:apps/server/src/modules/search/infrastructure/pg-search-index.ts) with postgres.js numbering: one
// parameter per interpolation (ref, server, title, description, body, then the tsv's own).
const referenceUpsert = `
insert into search.documents
  (ref, server, title, description, body, tsv, embedding, updated_at)
values (
  $1, $2, $3, $4, $5,
  setweight(to_tsvector('simple', translate($6, '._-/', '    ')
      || ' ' || coalesce($7::text, '')), 'A')
    || setweight(to_tsvector('russian', $8), 'B')
    || setweight(to_tsvector('english', $9), 'B')
    || setweight(to_tsvector('russian', $10), 'C')
    || setweight(to_tsvector('english', $11), 'C'),
  $12::vector,
  now()
)
on conflict (ref) do update set
  server = excluded.server,
  title = excluded.title,
  description = excluded.description,
  body = excluded.body,
  tsv = excluded.tsv,
  embedding = excluded.embedding,
  embed_failed_at = case when excluded.embedding is null
    then search.documents.embed_failed_at else null end,
  updated_at = excluded.updated_at`

// TestTsvEqualsTheTypeScriptUpsertsForEveryKindOfText writes documents through Upsert, reads their
// tsv, overwrites them with the TypeScript statement run by hand and reads again: equal text means
// the same configurations, weights, translate set and title handling.
func TestTsvEqualsTheTypeScriptUpsertsForEveryKindOfText(t *testing.T) {
	pool, idx := setupIndex(t)
	docs := []app.Document{
		{Ref: "gitea.issues_create", Server: "gitea", Title: str("Create issue"), Description: "Creates a new issue in the repository", Body: "owner repo title labels"},
		{Ref: "yt.создать-задачу", Server: "yt", Title: str("Создать задачу"), Description: "Создаёт новую задачу в трекере проекта", Body: "проект описание приоритет"},
		{Ref: "a.b-c_d/e.f", Server: "a", Description: "dots, dashes, underscores and a slash in the ref", Body: ""},
		{Ref: "nulltitle.x", Server: "nulltitle", Title: nil, Description: "no title here", Body: "b"},
		{Ref: "punct.mix", Server: "punct", Title: str("E-mail: me@example.com, v1.2.3"), Description: "Send mail to admin@example.org (v2.0) at 3.14159 or 1,000 now!", Body: "http://host:8080/path?x=1&y=2 foo_bar-baz 42"},
		{Ref: "both.langs", Server: "both", Title: str("Mixed"), Description: "Running runners ran; бегущие бегуны бежали", Body: "parameters параметры"},
	}
	for _, d := range docs {
		mustOK(t, idx.Upsert(t.Context(), d))
	}
	read := func() map[string]string {
		rows, err := pool.Query(t.Context(), `select ref, tsv::text from search.documents`)
		mustOK(t, err)
		out := map[string]string{}
		for rows.Next() {
			var r, v string
			mustOK(t, rows.Scan(&r, &v))
			out[r] = v
		}
		mustOK(t, rows.Err())
		return out
	}
	ours := read()
	for _, d := range docs {
		_, err := pool.Exec(t.Context(), referenceUpsert, d.Ref, d.Server, d.Title, d.Description, d.Body,
			d.Ref, d.Title, d.Description, d.Description, d.Body, d.Body, nil)
		mustOK(t, err)
	}
	ref := read()
	for _, d := range docs {
		if ours[d.Ref] != ref[d.Ref] {
			t.Errorf("%s:\n ours      %s\n reference %s", d.Ref, ours[d.Ref], ref[d.Ref])
		}
	}
	if len(ours) != len(docs) || len(ref) != len(docs) {
		t.Fatalf("%d / %d rows", len(ours), len(ref))
	}
}
