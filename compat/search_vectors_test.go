//go:build compat

package compat_test

// Requirement 4 of the migration plan: the vectors a Go server STORES for the same texts match the
// TypeScript reference within 1e-6. TestCatalogSearch compares search SCORES with 1e-6, but a score
// is a pure function of the rank (see there), so it says nothing about vectors; this file compares
// the vectors themselves.
//
// Approach (a): the scenario registers the `gitea` and `youtrack` fixture servers with exactly the
// name and description the passage texts of compat/golden/embeddings-<arch>.json were generated
// from (compat/golden/gen-embeddings.mjs: "passage: <slug>. <slug> server\n<tool> <title>\n<description>",
// the document text of CLAUDE.md "Search / index" with server name "<slug>" and description
// "<slug> server"), waits for the index, reads search.documents.embedding through SQL and compares
// every vector component-wise with the reference vector of the same text. The reference comes from
// @huggingface/transformers run directly (gen-embeddings.mjs), not from the image, so the
// comparison judges the image against something it did not produce.
//
// What each golden file is for: embeddings-<arch>.json holds the reference vectors of the passage
// texts compared here and also of the query texts, which nothing in the product exposes (the
// query vector is never stored and search returns ranks and scores only): the query side stays the
// milestone-4 reference for the Go embedder's unit tests.
//
// The same stack serves the SEARCH_MIN_SIMILARITY scenario (configuration, requirement 7): the
// index is built once and the app is restarted with other floors.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// vectorTolerance is the component-wise tolerance of the requirement.
const vectorTolerance = 1e-6

type referenceEmbedding struct {
	Text string    `json:"text"`
	Vec  []float64 `json:"vec"`
}

// loadReferenceEmbeddings reads compat/golden/embeddings-<arch>.json keyed by text. A missing file
// is a clear failure: the vectors of an architecture can only be compared with the reference
// recorded on it.
func loadReferenceEmbeddings(t *testing.T, arch string) map[string][]float64 {
	t.Helper()
	path := filepath.Join("golden", "embeddings-"+arch+".json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("there is no reference %s for the architecture %s of the image under test: record it with compat/golden/gen-embeddings.mjs in a node container of that architecture (see the header of the script)", path, arch)
	}
	if err != nil {
		t.Fatal(err)
	}
	var rows []referenceEmbedding
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := make(map[string][]float64, len(rows))
	for _, r := range rows {
		out[r.Text] = r.Vec
	}
	return out
}

// fixturePassage is the passage text gen-embeddings.mjs builds for a fixture tool: the product's
// document text with the server name and description the generator assumed. A tool without a title
// keeps the generator's trailing space ("name "); the tokenizer drops whitespace, so it does not
// change the vector.
func fixturePassage(server string, tool fakes.Tool) string {
	return fmt.Sprintf("passage: %s. %s server\n%s %s\n%s", server, server, tool.Name, tool.Title, tool.Description)
}

// parseVector reads pgvector's text form "[0.1,0.2,...]".
func parseVector(t *testing.T, text string) []float64 {
	t.Helper()
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "["), "]")
	parts := strings.Split(inner, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		x, err := strconv.ParseFloat(p, 64)
		if err != nil {
			t.Fatalf("vector %.40q: %v", text, err)
		}
		out = append(out, x)
	}
	return out
}

// compareVectors lists every difference between the stored vectors and the reference (by ref):
// refs on one side only, lengths, and components further than tol apart (with the worst one).
func compareVectors(stored, want map[string][]float64, tol float64) []string {
	var problems []string
	for ref := range want {
		if _, ok := stored[ref]; !ok {
			problems = append(problems, ref+": no stored vector")
		}
	}
	for ref, got := range stored {
		w, ok := want[ref]
		if !ok {
			problems = append(problems, ref+": stored but not in the reference")
			continue
		}
		if len(got) != len(w) {
			problems = append(problems, fmt.Sprintf("%s: %d components, reference %d", ref, len(got), len(w)))
			continue
		}
		worst, at := 0.0, 0
		for i := range got {
			if d := math.Abs(got[i] - w[i]); d > worst {
				worst, at = d, i
			}
		}
		if worst > tol {
			problems = append(problems, fmt.Sprintf("%s: component %d differs by %.3g (> %.0e)", ref, at, worst, tol))
		}
	}
	slices.Sort(problems)
	return problems
}

func TestSearchStoredVectors(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	admin := s.Admin(t)
	token := admin.Token(t, "stored-vectors")
	reference := loadReferenceEmbeddings(t, s.Arch)

	gitea := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.FixtureTools("gitea")})
	youtrack := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.FixtureTools("youtrack")})
	for _, srv := range []struct {
		slug string
		up   *fakes.Upstream
	}{{"gitea", gitea}, {"youtrack", youtrack}} {
		catalogRegister(t, admin, fakes.ServerInput(srv.up, srv.slug, "none", fakes.WithName(srv.slug), catalogWithDescription(srv.slug+" server")))
	}
	catalogWaitDocs(t, s, "gitea", 61)
	catalogWaitDocs(t, s, "youtrack", 23)

	// expected: the reference vector of every fixture tool's passage, keyed by ref.
	expected := map[string][]float64{}
	for _, srv := range []string{"gitea", "youtrack"} {
		for _, tool := range fakes.FixtureTools(srv) {
			vec, ok := reference[fixturePassage(srv, tool)]
			if !ok {
				t.Fatalf("the reference has no passage for %s.%s: the document text the scenario expects differs from the generator's", srv, tool.Name)
			}
			expected[srv+"."+tool.Name] = vec
		}
	}
	if len(expected) != 84 {
		t.Fatalf("%d expected vectors, want 84", len(expected))
	}

	stored := func() map[string][]float64 {
		rows, err := s.DB.Query(t.Context(), "select ref, embedding::text from search.documents where server in ('gitea', 'youtrack')")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string][]float64{}
		for rows.Next() {
			var ref, text string
			if err := rows.Scan(&ref, &text); err != nil {
				t.Fatal(err)
			}
			out[ref] = parseVector(t, text)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("every stored vector matches the reference within 1e-6", func(t *testing.T) {
		got := stored()
		if len(got) != 84 {
			t.Fatalf("%d stored vectors, want 84", len(got))
		}
		var worst float64
		for ref, vec := range got {
			for i, x := range vec {
				worst = math.Max(worst, math.Abs(x-expected[ref][i]))
			}
		}
		t.Logf("largest component difference of the 84 vectors: %.3g", worst)
		if problems := compareVectors(got, expected, vectorTolerance); len(problems) != 0 {
			t.Fatalf("%d of 84 stored vectors differ from the %s reference:\n%s", len(problems), s.Arch, strings.Join(problems[:min(len(problems), 10)], "\n"))
		}
	})

	t.Run("the comparison fails when one stored vector is off by 1e-4 (not vacuous)", func(t *testing.T) {
		const victim = "gitea.list_pull_requests"
		var original string
		if err := s.DB.QueryRow(t.Context(), "select embedding::text from search.documents where ref = $1", victim).Scan(&original); err != nil {
			t.Fatal(err)
		}
		defer execSQL(t, s, "update search.documents set embedding = $2::vector where ref = $1", victim, original)
		// Shift the first component by 1e-4 (pgvector stores float4: the shift survives).
		vec := parseVector(t, original)
		vec[0] += 1e-4
		parts := make([]string, len(vec))
		for i, x := range vec {
			parts[i] = strconv.FormatFloat(x, 'g', -1, 32)
		}
		execSQL(t, s, "update search.documents set embedding = $2::vector where ref = $1", victim, "["+strings.Join(parts, ",")+"]")
		problems := compareVectors(stored(), expected, vectorTolerance)
		if len(problems) != 1 || !strings.HasPrefix(problems[0], victim+": component 0 differs") {
			t.Fatalf("a vector off by 1e-4 is not caught: %v", problems)
		}
	})

	t.Run("SEARCH_MIN_SIMILARITY sets the floor of vector candidates", func(t *testing.T) {
		both := []string{"gitea", "youtrack"}
		// after restarts the app searches the full text only until its embedding model has loaded in
		// the background (about a second: the image falls back silently, see the report), so a
		// result that must contain vector hits is polled until it holds more than the floor above it.
		refsAt := func(floor, query string, moreThan int) []string {
			t.Helper()
			env := map[string]string{}
			if floor != "" {
				env["SEARCH_MIN_SIMILARITY"] = floor
			}
			s.SwapImage(t, harness.DefaultImage(), "", env)
			c := s.MCP(t, token)
			var refs []string
			harness.Eventually(t, fmt.Sprintf("floor %q: more than %d hits for %q (the vector search is ready)", floor, moreThan, query), func() bool {
				refs = catalogHitRefs(catalogSearch(t, c, query, 25, both))
				return len(refs) > moreThan
			})
			return refs
		}
		isSubset := func(small, big []string) bool {
			return !slices.ContainsFunc(small, func(r string) bool { return !slices.Contains(big, r) })
		}
		for _, query := range []string{
			"create an issue in the tracker",     // no full-text hit at all: every hit is a vector hit
			"list pull requests in a repository", // a couple of full-text hits, and the vectors add more
		} {
			// The highest floor lets no vector candidate through: only the full-text hits stay (none
			// or few). It never needs a ready model, so it is not polled.
			s.SwapImage(t, harness.DefaultImage(), "", map[string]string{"SEARCH_MIN_SIMILARITY": "0.999"})
			high := catalogHitRefs(catalogSearch(t, s.MCP(t, token), query, 25, both))
			def := refsAt("", query, len(high))
			low := refsAt("0", query, len(def))
			t.Logf("%q: floor 0.999 -> %d hits, default -> %d, 0 -> %d", query, len(high), len(def), len(low))
			if !isSubset(high, def) || !isSubset(def, low) {
				t.Errorf("%q: the results do not grow with a lower floor: 0.999 %v, default %v, 0 %v", query, high, def, low)
			}
		}
		// The floor leaves full-text hits alone: with the highest floor an exact tool name is still found.
		s.SwapImage(t, harness.DefaultImage(), "", map[string]string{"SEARCH_MIN_SIMILARITY": "0.999"})
		if got := catalogHitRefs(catalogSearch(t, s.MCP(t, token), "create_issue", 25, both)); !slices.Contains(got, "youtrack.create_issue") {
			t.Errorf("a full-text match disappears with a high floor: %v", got)
		}
		s.SwapImage(t, harness.DefaultImage(), "") // the stack ends healthy on the default floor
	})
}
