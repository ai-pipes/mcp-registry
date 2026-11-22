//go:build compat

package compat_test

// Two decided deviations of the search index (compat/harness/deviations.go). Neither needs /mcp:
// the index is observed through SQL on search.documents, the registration and the change go through
// REST, the recovery through the CLI. Both run the reference branch on the reference image and the
// decided one on the Go server.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// indexedDoc is the part of a search.documents row these scenarios watch.
type indexedDoc struct {
	vector  *string // embedding::text, nil for NULL
	marked  bool    // embed_failed_at is set
	updated time.Time
}

func indexedDocs(t *testing.T, s *harness.Stack, server string) map[string]indexedDoc {
	t.Helper()
	rows, err := s.DB.Query(t.Context(), "select ref, embedding::text, embed_failed_at is not null, updated_at from search.documents where server = $1", server)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]indexedDoc{}
	for rows.Next() {
		var ref string
		var d indexedDoc
		if err := rows.Scan(&ref, &d.vector, &d.marked, &d.updated); err != nil {
			t.Fatal(err)
		}
		out[ref] = d
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func startSearchStackWithFakeEmbedder(t *testing.T) (*harness.Stack, *fakes.Embedder) {
	t.Helper()
	embedder := fakes.StartEmbedder(t)
	s := harness.Start(t, harness.Options{Env: map[string]string{
		"EMBEDDER": "openai-compatible", "EMBEDDER_URL": embedder.URL, "EMBEDDER_MODEL": "compat-e5-model",
		"SEARCH_MIN_SIMILARITY": "0.1",
	}})
	return s, embedder
}

// TestSearchFailedReembedding judges vector-kept-on-failed-reembedding: the embedder is down while
// a change of the server re-indexes every document of it. Reference: the upsert writes NULL
// vectors, so the documents lose vector search. Decided: the text is written, the vectors stay
// byte for byte and the rows are marked (embed_failed_at). Then the embedder comes back and
// `reindex` refreshes the vectors in both branches. That the sweep alone (every five minutes, the
// mark must be an hour old) refreshes a marked row is judged by the Go integration test
// TestAFailedReembeddingKeepsTheVectorsAndTheSweepRefreshesThem, not here: a scenario does not
// wait minutes.
func TestSearchFailedReembedding(t *testing.T) {
	s, embedder := startSearchStackWithFakeEmbedder(t)
	admin := s.Admin(t)
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("one", "two", "three")})
	slug := harness.Unique("fre")
	catalogRegister(t, admin, fakes.ServerInput(up, slug, "none", catalogWithDescription("Issue tracker")))
	catalogWaitDocs(t, s, slug, 3)
	before := indexedDocs(t, s, slug)
	var latest time.Time
	for ref, d := range before {
		if d.vector == nil || d.marked {
			t.Fatalf("setup: %s has vector %v, marked %v", ref, d.vector != nil, d.marked)
		}
		if d.updated.After(latest) {
			latest = d.updated
		}
	}

	embedder.SetDown(true)
	v := catalogGet(t, admin, slug)
	r := admin.Do(t, "PUT", "/api/servers/"+slug, catalogPutBody(v, func(b map[string]any) { b["description"] = "Bug tracker" }))
	if r.Status != 200 {
		t.Fatalf("PUT: %d %s", r.Status, r.Body)
	}
	// Every document is rewritten (updated_at moves) although the embedder fails.
	harness.Eventually(t, "every document of the server to be rewritten while the embedder is down", func() bool {
		now := indexedDocs(t, s, slug)
		if len(now) != 3 {
			return false
		}
		for _, d := range now {
			if !d.updated.After(latest) {
				return false
			}
		}
		return true
	})
	after := indexedDocs(t, s, slug)
	for ref, d := range after {
		if s.Fixed(harness.DevVectorKeptOnFailedReembedding) {
			// Decided: the vector is the old one, byte for byte, and the row is marked for a new one.
			if d.vector == nil || *d.vector != *before[ref].vector {
				t.Errorf("%s: the stored vector did not survive the failed re-embedding", ref)
			}
			if !d.marked {
				t.Errorf("%s: a row that kept a stale vector is not marked (embed_failed_at is NULL)", ref)
			}
		} else if d.vector != nil {
			// Reference: the failed re-embedding nulls the vector.
			t.Errorf("%s: the reference keeps a vector (%.30s...) after a failed re-embedding", ref, *d.vector)
		}
	}

	embedder.SetDown(false)
	if res := s.CLI(t, "", "reindex"); res.Code != 0 {
		t.Fatalf("reindex: code %d stdout %q stderr %q", res.Code, res.Stdout, res.Stderr)
	}
	harness.Eventually(t, "every document to hold a fresh vector of the changed server description, unmarked", func() bool {
		now := indexedDocs(t, s, slug)
		if len(now) != 3 {
			return false
		}
		for ref, d := range now {
			if d.vector == nil || *d.vector == *before[ref].vector || d.marked {
				return false
			}
		}
		return true
	})
}

// TestSearchStaleRemoval judges stale-removal-rechecks-catalog. A catalog.tools_removed event for
// a tool the catalog offers is written by hand (the payload is what the catalog writes:
// {server, refs}); it stands for a removal delivered late, after the tool was offered again.
// Reference: the live row is deleted and only `reindex` restores it. Decided: it stays. A listed
// ref the catalog does not offer is deleted in both. The hand-written outbox row is the only
// way to replay a stale event from outside.
func TestSearchStaleRemoval(t *testing.T) {
	s, _ := startSearchStackWithFakeEmbedder(t)
	admin := s.Admin(t)
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("one", "two", "three")})
	slug := harness.Unique("sre")
	catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
	catalogWaitDocs(t, s, slug, 3)

	live, ghost := slug+".two", slug+".ghost"
	execSQL(t, s, "insert into search.documents (ref, server, description, tsv) values ($1, $2, 'd', to_tsvector('simple', 'ghost'))", ghost, slug)
	payload, err := json.Marshal(map[string]any{"server": slug, "refs": []string{live, ghost}})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, "insert into platform.outbox (type, payload) values ('catalog.tools_removed', $1::jsonb)", string(payload))
	catalogWaitDelivered(t, s)

	if n := countSQL(t, s, "select count(*) from search.documents where ref = $1", ghost); n != 0 {
		t.Errorf("a listed ref the catalog does not offer survived its removal event")
	}
	liveRows := countSQL(t, s, "select count(*) from search.documents where ref = $1", live)
	if s.Fixed(harness.DevStaleRemovalRechecksCatalog) {
		if liveRows != 1 {
			t.Fatalf("the stale removal deleted the live row of %s although the catalog offers it", live)
		}
		catalogWaitDocs(t, s, slug, 3)
		return
	}
	if liveRows != 0 {
		t.Fatalf("the reference kept %s after a removal event that named it", live)
	}
	// Reference: nothing restores it but the next event for the server or a reindex.
	if n := countSQL(t, s, "select count(*) from search.documents where server = $1", slug); n != 2 {
		t.Fatalf("%d rows of the server after the stale removal, want 2", n)
	}
	if res := s.CLI(t, "", "reindex"); res.Code != 0 {
		t.Fatalf("reindex: code %d stderr %q", res.Code, res.Stderr)
	}
	catalogWaitDocs(t, s, slug, 3)
}
