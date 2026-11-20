//go:build compat

package compat_test

// Search scenarios: the index follows the catalog (search.documents, the outbox), the recorded
// queries (compared with the reference image on the same machine, see "the recorded queries"),
// limits and filters, access by groups, and the
// reindex command. Everything is reached through MCP (search_tools, describe_tool, list_servers,
// call_tool), REST, SQL and the CLI.
//
// Source cases that are not ported:
//   - search.int.test.ts "embedder outages" (keeps text search working, the sweep fills the
//     embeddings in later, skips a document marked as failed): the embedder cannot be made to
//     fail from outside. "reindexAll rebuilds the index" is ported through the reindex command.
//   - search.int.test.ts uses a bag-of-tokens fake embedder at similarity 0.2; the scenarios
//     here use the real model and the default threshold, so their queries and expected values
//     differ (stated at each case).
//   - embedder.int.test.ts: the embedder itself; judged by TestSearchStoredVectors
//     (the stored vectors against transformers.js on the same machine) and, later, by Go unit tests.

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// ---- search ----

// catalogSearchQueries is the fixed list asked of the stack under test and of the reference image.
// relevant is a literal expectation about the first hit, kept only for queries whose first hit was
// identical in the arm64 golden recorded on an Apple Silicon Mac, in the GitHub arm64 runner's log
// and in the GitHub amd64 run (the CPU-dependent ones, such as "get the contents of a file in a
// repository" and "issue", differ between machines and are only compared with the reference
// image). Literals keep the comparison honest when the reference and the server under test are
// both broken the same way. nothing: no tool at all may be returned.
var catalogSearchQueries = []struct {
	query    string
	relevant string // the ref that must rank first, "" for none
	nothing  bool   // must return no tool at all
}{
	{"create an issue in the tracker", "youtrack.create_issue", false},
	{"list pull requests in a repository", "gitea.list_pull_requests", false},
	{"add a comment to an issue", "youtrack.add_issue_comment", false},
	{"find issues by a search query", "youtrack.search_issues", false},
	{"create a new branch", "gitea.create_branch", false},
	{"get the contents of a file in a repository", "", false},
	{"создать задачу в трекере", "youtrack.create_issue", false},
	{"найти задачу по номеру", "", false},
	{"показать список pull request", "gitea.list_pull_requests", false},
	{"создать ветку в репозитории", "", false},
	{"issue", "", false},
	{"comment", "youtrack.add_issue_comment", false},
	{"search", "", false},
	{"weather in Berlin", "", true},
	{"book a flight to Tokyo", "", true},
	{"dollar exchange rate", "", true},
	{"borscht recipe", "", true},
	{"прогноз погоды на завтра", "", true},
}

func TestCatalogSearch(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	admin := s.Admin(t)
	c := s.MCP(t, admin.Token(t, "catalog-search"))

	gitea := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.FixtureTools("gitea")})
	youtrack := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.FixtureTools("youtrack")})
	catalogRegister(t, admin, fakes.ServerInput(gitea, "gitea", "none", fakes.WithName("Gitea"), catalogWithDescription("Git hosting: repositories, pull requests")))
	catalogRegister(t, admin, fakes.ServerInput(youtrack, "youtrack", "none", fakes.WithName("YouTrack"), catalogWithDescription("Issue tracker")))
	catalogWaitDocs(t, s, "gitea", 61)
	catalogWaitDocs(t, s, "youtrack", 23)
	both := []string{"gitea", "youtrack"}

	t.Run("the index follows the catalog", func(t *testing.T) {
		var total, embedded int
		if err := s.DB.QueryRow(t.Context(), "select count(*), count(embedding) from search.documents where server in ('gitea', 'youtrack')").Scan(&total, &embedded); err != nil {
			t.Fatal(err)
		}
		if total != 84 || embedded != 84 {
			t.Fatalf("search.documents: %d rows, %d embedded, want 84 one per visible tool", total, embedded)
		}
	})

	// The queries are asked of the stack under test and of a second stack booted from the reference
	// image on the same machine (the same fixtures, the same default floor): the refs and the order
	// must be equal and the scores equal within 1e-6 (a score is a pure function of the rank, so
	// equal refs imply equal scores; the scores are compared anyway as the server's own number).
	// q8 vectors differ between CPUs of the same architecture by up to about 0.015, which moves
	// ranks, so no ranking is committed. When the image under test IS the reference image this is a
	// determinism check between two instances; for another image (the Go server) it is the real
	// comparison. Everything else here does not depend on the CPU.
	t.Run("the recorded queries", func(t *testing.T) {
		harness.RequireReferenceImage(t)
		ref := harness.Start(t, harness.Options{Image: harness.ReferenceImage(), CLI: harness.ReferenceCLI()})
		refAdmin := ref.Admin(t)
		rc := ref.MCP(t, refAdmin.Token(t, "catalog-search-reference"))
		catalogRegister(t, refAdmin, fakes.ServerInput(gitea, "gitea", "none", fakes.WithName("Gitea"), catalogWithDescription("Git hosting: repositories, pull requests")))
		catalogRegister(t, refAdmin, fakes.ServerInput(youtrack, "youtrack", "none", fakes.WithName("YouTrack"), catalogWithDescription("Issue tracker")))
		catalogWaitDocs(t, ref, "gitea", 61)
		catalogWaitDocs(t, ref, "youtrack", 23)

		for _, q := range catalogSearchQueries {
			hits := catalogSearch(t, c, q.query, 10, both)
			refHits := catalogSearch(t, rc, q.query, 10, both)

			// Properties that do not depend on the CPU.
			if len(hits) > 10 {
				t.Errorf("%q: %d hits, limit 10", q.query, len(hits))
			}
			for i, h := range hits {
				if h.Score <= 0 || h.Score > 1 {
					t.Errorf("%q: score %v of %s is outside (0, 1]", q.query, h.Score, h.Tool)
				}
				if i > 0 && h.Score > hits[i-1].Score {
					t.Errorf("%q: scores are not sorted descending at %d", q.query, i)
				}
				if h.Server != strings.SplitN(h.Tool, ".", 2)[0] || (h.Server != "gitea" && h.Server != "youtrack") {
					t.Errorf("%q: hit %s from server %q", q.query, h.Tool, h.Server)
				}
			}
			if q.nothing && len(hits) != 0 {
				t.Errorf("%q must find nothing, got %v", q.query, catalogHitRefs(hits))
			}
			if q.relevant != "" && (len(hits) == 0 || hits[0].Tool != q.relevant) {
				t.Errorf("%q: first hit %v, want %s", q.query, catalogHitRefs(hits), q.relevant)
			}

			// The comparison with the reference image on this machine.
			got, want := catalogHitRefs(hits), catalogHitRefs(refHits)
			if !slices.Equal(got, want) {
				t.Errorf("%q: refs differ from the reference image on this machine\n server under test %v\n reference image   %v", q.query, got, want)
				continue
			}
			for j := range refHits {
				if math.Abs(hits[j].Score-refHits[j].Score) > 1e-6 {
					t.Errorf("%q: score of %s is %.9f, the reference image gives %.9f (tolerance 1e-6)", q.query, want[j], hits[j].Score, refHits[j].Score)
				}
			}
		}
		if len(catalogSearchQueries) < 12 {
			t.Fatalf("only %d queries", len(catalogSearchQueries))
		}
	})

	// Order-dependent by design: subtests run in declaration order and only gitea and youtrack
	// exist on this stack until the later subtests register their own servers.
	t.Run("searching every usable server equals searching both explicitly", func(t *testing.T) {
		for _, q := range []string{"create an issue in the tracker", "list pull requests in a repository", "weather in Berlin"} {
			all := catalogSearch(t, c, q, 10, nil)
			explicit := catalogSearch(t, c, q, 10, both)
			if !reflect.DeepEqual(all, explicit) {
				t.Errorf("%q: without servers %v, with both %v", q, catalogHitRefs(all), catalogHitRefs(explicit))
			}
		}
	})

	t.Run("limit is respected", func(t *testing.T) {
		full := catalogSearch(t, c, "issue", 25, both)
		if len(full) < 5 {
			t.Fatalf("only %d hits for \"issue\"", len(full))
		}
		for _, limit := range []int{1, 3, 5, len(full) - 1} {
			part := catalogSearch(t, c, "issue", limit, both)
			if len(part) != limit {
				t.Fatalf("limit %d returned %d hits", limit, len(part))
			}
			for i, h := range part {
				if math.Abs(h.Score-full[i].Score) > 1e-9 {
					t.Fatalf("limit %d: score %d is %v, with limit 25 it is %v", limit, i, h.Score, full[i].Score)
				}
			}
		}
		if got, want := len(catalogSearch(t, c, "issue", 0, both)), min(10, len(full)); got != want {
			t.Fatalf("without a limit %d hits, want %d (the default limit is 10)", got, want)
		}
		for _, bad := range []int{0, 26, -1} {
			res := catalogCallMCP(t, c, "search_tools", map[string]any{"query": "issue", "limit": bad})
			catalogExpectInputError(t, fmt.Sprintf("limit %d", bad), res, "limit")
		}
	})

	t.Run("the servers filter applies before the limit", func(t *testing.T) {
		unfiltered := catalogSearch(t, c, "create an issue in the tracker", 1, both)
		if len(unfiltered) != 1 || unfiltered[0].Server != "youtrack" {
			t.Fatalf("unfiltered top hit: %v", catalogHitRefs(unfiltered))
		}
		onlyGitea := catalogSearch(t, c, "create an issue in the tracker", 1, []string{"gitea"})
		if len(onlyGitea) != 1 || onlyGitea[0].Server != "gitea" || onlyGitea[0].Score <= 0 || onlyGitea[0].Score > 1 {
			t.Fatalf("limit 1 on gitea: %+v: the better youtrack hits must not eat the limit", onlyGitea)
		}
		for _, h := range catalogSearch(t, c, "create an issue in the tracker", 10, []string{"gitea"}) {
			if h.Server != "gitea" {
				t.Fatalf("a hit of %s with the filter [gitea]", h.Server)
			}
		}
		dup := catalogSearch(t, c, "create an issue in the tracker", 10, []string{"gitea", "gitea"})
		single := catalogSearch(t, c, "create an issue in the tracker", 10, []string{"gitea"})
		if !reflect.DeepEqual(dup, single) {
			t.Fatalf("a repeated slug changes the result: %v vs %v", catalogHitRefs(dup), catalogHitRefs(single))
		}
	})

	t.Run("an unknown server and an empty filter are refused", func(t *testing.T) {
		res := catalogCallMCP(t, c, "search_tools", catalogSearchArgs("issue", 0, []string{"nope"}))
		if !res.IsError || catalogText(res) != `Unknown server "nope". Call list_servers.` {
			t.Fatalf("unknown slug: isError %v %q", res.IsError, catalogText(res))
		}
		res = catalogCallMCP(t, c, "search_tools", catalogSearchArgs("issue", 0, []string{"gitea", "nope"}))
		if !res.IsError || catalogText(res) != `Unknown server "nope". Call list_servers.` {
			t.Fatalf("one unknown among known: isError %v %q", res.IsError, catalogText(res))
		}
		res = catalogCallMCP(t, c, "search_tools", catalogSearchArgs("issue", 0, []string{}))
		// The tool never returns [] for an empty list: its schema refuses it, as an input error that
		// names the field (the validator's wording is not pinned).
		catalogExpectInputError(t, "empty filter", res, "servers")
		if t.Failed() {
			t.FailNow()
		}
		// Control: the same query without a filter works.
		if hits := catalogSearch(t, c, "create an issue in the tracker", 3, nil); len(hits) == 0 {
			t.Fatal("no hits for a relevant query without a filter")
		}
	})

	t.Run("list_servers and describe_tool", func(t *testing.T) {
		list := catalogListServers(t, c)
		if len(list) != 2 || list[0].Slug != "gitea" || list[0].Tools != 61 || list[0].Name != "Gitea" || list[0].Connection != "not_required" ||
			list[1].Slug != "youtrack" || list[1].Tools != 23 || list[0].ConnectURL != "" {
			t.Fatalf("list_servers: %+v", list)
		}
		var d catalogDescribed
		catalogStructured(t, catalogCallMCP(t, c, "describe_tool", map[string]any{"tool": "youtrack.create_issue"}), &d)
		if d.Tool != "youtrack.create_issue" || d.Server != "youtrack" || d.Connection != "not_required" || d.InputSchema["type"] != "object" || d.Description == "" {
			t.Fatalf("describe_tool: %+v", d)
		}
		res := catalogCallMCP(t, c, "describe_tool", map[string]any{"tool": "youtrack.no_such_tool"})
		if !res.IsError || catalogText(res) != catalogNotAvailable("youtrack.no_such_tool") {
			t.Fatalf("unknown tool: %q", catalogText(res))
		}
	})

	t.Run("hiding, removing and changing a tool reach the search results", func(t *testing.T) {
		upstreamTools := []fakes.Tool{
			{Name: "frobnicate_quuxwidget", Description: "Frobnicates the quuxwidget of a repository.", InputSchema: map[string]any{"type": "object"}},
			{Name: "static_item", Description: "Neutralizes the blargle.", InputSchema: map[string]any{"type": "object"}},
			{Name: "dropped_plumbus", Description: "Drops the plumbus.", InputSchema: map[string]any{"type": "object"}},
		}
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: upstreamTools})
		slug := harness.Unique("idx")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		catalogWaitDocs(t, s, slug, 3)
		find := func(query string) []string {
			return catalogHitRefs(catalogSearch(t, c, query, 10, []string{slug}))
		}
		hidden := slug + ".frobnicate_quuxwidget"
		if got := find("frobnicate quuxwidget"); !slices.Contains(got, hidden) {
			t.Fatalf("the tool is not found before it is hidden: %v", got)
		}

		// Hidden: neither searched, nor described, nor callable; still hidden after a sync.
		catalogHide(t, admin, slug, "frobnicate_quuxwidget", true)
		catalogWaitDocs(t, s, slug, 2)
		if got := find("frobnicate quuxwidget"); slices.Contains(got, hidden) {
			t.Fatalf("a hidden tool is found: %v", got)
		}
		notAvailable := func(when string) {
			t.Helper()
			for _, name := range []string{"describe_tool", "call_tool"} {
				res := catalogCallMCP(t, c, name, map[string]any{"tool": hidden})
				if !res.IsError || catalogText(res) != catalogNotAvailable(hidden) {
					t.Fatalf("%s of a hidden tool %s: isError %v %q", name, when, res.IsError, catalogText(res))
				}
			}
			if n := len(up.Calls()); n != 0 {
				t.Fatalf("the upstream was called %d time(s) for a hidden tool %s", n, when)
			}
		}
		notAvailable("after hiding it")
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{}) {
			t.Fatalf("sync with a hidden tool: %+v", res)
		}
		// The sync's events must be delivered before the index is judged: the row count is 2 both
		// before and after, so it cannot tell a re-indexed hidden tool from an untouched index.
		catalogWaitDocs(t, s, slug, 2)
		notAvailable("after a sync") // synchronous: the catalog decides, not the index
		if got := find("frobnicate quuxwidget"); slices.Contains(got, hidden) {
			t.Fatalf("the tool is found again after a sync: %v", got)
		}
		// Unhide: found again.
		catalogHide(t, admin, slug, "frobnicate_quuxwidget", false)
		catalogWaitDocs(t, s, slug, 3)
		if got := find("frobnicate quuxwidget"); !slices.Contains(got, hidden) {
			t.Fatalf("an unhidden tool is not found: %v", got)
		}

		// Removed upstream: gone from the catalog and the search.
		gone := slug + ".dropped_plumbus"
		if got := find("drops the plumbus"); !slices.Contains(got, gone) {
			t.Fatalf("control: %s is not found before it is removed: %v", gone, got)
		}
		upstreamTools = slices.DeleteFunc(upstreamTools, func(tl fakes.Tool) bool { return tl.Name == "dropped_plumbus" })
		up.SetTools(upstreamTools)
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{Removed: 1}) {
			t.Fatalf("sync after the removal: %+v", res)
		}
		catalogWaitDocs(t, s, slug, 2)
		if got := find("drops the plumbus"); slices.Contains(got, gone) {
			t.Fatalf("a removed tool is found: %v", got)
		}
		for _, name := range []string{"describe_tool", "call_tool"} {
			if res := catalogCallMCP(t, c, name, map[string]any{"tool": gone}); !res.IsError || catalogText(res) != catalogNotAvailable(gone) {
				t.Fatalf("%s of a removed tool: isError %v %q", name, res.IsError, catalogText(res))
			}
		}
		if n := len(up.Calls()); n != 0 {
			t.Fatalf("the upstream was called %d time(s) for a removed tool", n)
		}
		if catalogHas(catalogCatalog(t, admin), slug) && slices.Contains(catalogRefs(catalogCatalog(t, admin), slug), gone) {
			t.Fatal("a removed tool is still in the catalog")
		}

		// Changed description: the new words are found, the old ones are not.
		for i := range upstreamTools {
			if upstreamTools[i].Name == "static_item" {
				upstreamTools[i].Description = "Reticulates the splines of a gizmo."
			}
		}
		up.SetTools(upstreamTools)
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{Changed: 1}) {
			t.Fatalf("sync after the change: %+v", res)
		}
		harness.Eventually(t, "the changed description in search.documents", func() bool {
			var n int
			err := s.DB.QueryRow(t.Context(), "select count(*) from search.documents where ref = $1 and description = $2 and embedding is not null", slug+".static_item", "Reticulates the splines of a gizmo.").Scan(&n)
			return err == nil && n == 1
		})
		catalogWaitDelivered(t, s)
		if got := find("reticulates splines gizmo"); !slices.Contains(got, slug+".static_item") {
			t.Fatalf("the new description is not searchable: %v", got)
		}
		if got := find("neutralizes the blargle"); slices.Contains(got, slug+".static_item") {
			t.Fatalf("the old description is still searchable: %v", got)
		}
	})

	t.Run("a disabled server leaves the index; enabling brings it back", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("alpha_gizmo", "beta_gizmo")})
		slug := harness.Unique("dsb")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		catalogWaitDocs(t, s, slug, 2)
		ref := slug + ".alpha_gizmo"
		if got := catalogHitRefs(catalogSearch(t, c, "alpha gizmo", 5, []string{slug})); !slices.Contains(got, ref) {
			t.Fatalf("control: not found while active: %v", got)
		}
		if r := admin.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]any{"status": "disabled"}); r.Status != 204 {
			t.Fatalf("disable: %d", r.Status)
		}
		catalogWaitDocs(t, s, slug, 0)
		if catalogListedHas(catalogListServers(t, c), slug) {
			t.Fatal("list_servers lists a disabled server, even to an admin")
		}
		res := catalogCallMCP(t, c, "search_tools", catalogSearchArgs("alpha gizmo", 5, []string{slug}))
		if !res.IsError || catalogText(res) != fmt.Sprintf("Unknown server %q. Call list_servers.", slug) {
			t.Fatalf("search in a disabled server: isError %v %q", res.IsError, catalogText(res))
		}
		if res := catalogCallMCP(t, c, "describe_tool", map[string]any{"tool": ref}); !res.IsError || catalogText(res) != catalogNotAvailable(ref) {
			t.Fatalf("describe_tool in a disabled server: %q", catalogText(res))
		}
		if res := catalogCallMCP(t, c, "call_tool", map[string]any{"tool": ref}); !res.IsError || catalogText(res) != catalogNotAvailable(ref) {
			t.Fatalf("call_tool in a disabled server: %q", catalogText(res))
		}
		if n := len(up.Calls()); n != 0 {
			t.Fatalf("the upstream was called %d time(s) for a disabled server", n)
		}
		if got := catalogHitRefs(catalogSearch(t, c, "alpha gizmo", 25, nil)); slices.Contains(got, ref) {
			t.Fatalf("a disabled server's tool is found without a filter: %v", got)
		}
		if r := admin.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]any{"status": "active"}); r.Status != 204 {
			t.Fatalf("enable: %d", r.Status)
		}
		catalogWaitDocs(t, s, slug, 2)
		if got := catalogHitRefs(catalogSearch(t, c, "alpha gizmo", 5, []string{slug})); !slices.Contains(got, ref) {
			t.Fatalf("not found after enabling: %v", got)
		}
	})

	t.Run("a change of the server is reindexed", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("one", "two", "three")})
		slug := harness.Unique("ren")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none", catalogWithDescription("Issue tracker")))
		catalogWaitDocs(t, s, slug, 3)
		vectors := func() map[string]string {
			rows, err := s.DB.Query(t.Context(), "select ref, embedding::text from search.documents where server = $1", slug)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			out := map[string]string{}
			for rows.Next() {
				var ref, v string
				if err := rows.Scan(&ref, &v); err != nil {
					t.Fatal(err)
				}
				out[ref] = v
			}
			return out
		}
		before := vectors()
		v := catalogGet(t, admin, slug)
		r := admin.Do(t, "PUT", "/api/servers/"+slug, catalogPutBody(v, func(b map[string]any) { b["description"] = "Bug tracker" }))
		if r.Status != 200 {
			t.Fatalf("PUT: %d %s", r.Status, r.Body)
		}
		// The server's name and description are part of every passage, so every vector changes.
		harness.Eventually(t, "every document of the server to be re-embedded after a description change", func() bool {
			now := vectors()
			if len(now) != 3 {
				return false
			}
			for ref, vec := range now {
				if vec == "" || vec == before[ref] {
					return false
				}
			}
			return true
		})
	})

	t.Run("names with dots and dashes are found by their words", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: []fakes.Tool{
			{Name: "issues.create", Description: "x", InputSchema: map[string]any{"type": "object"}},
			{Name: "get-file_contents", Description: "y", InputSchema: map[string]any{"type": "object"}},
		}})
		slug := harness.Unique("dot")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		catalogWaitDocs(t, s, slug, 2)
		if got := catalogHitRefs(catalogSearch(t, c, "issues create", 5, []string{slug})); len(got) == 0 || got[0] != slug+".issues.create" {
			t.Fatalf("issues create: %v", got)
		}
		if got := catalogHitRefs(catalogSearch(t, c, "file contents", 5, []string{slug})); len(got) == 0 || got[0] != slug+".get-file_contents" {
			t.Fatalf("file contents: %v", got)
		}
		// The ref is split at the first dot: a dotted tool name works with describe_tool.
		var d catalogDescribed
		catalogStructured(t, catalogCallMCP(t, c, "describe_tool", map[string]any{"tool": slug + ".issues.create"}), &d)
		if d.Tool != slug+".issues.create" || d.Server != slug || d.Description != "x" {
			t.Fatalf("describe_tool of a dotted name: %+v", d)
		}
	})

	t.Run("the reindex command rebuilds the index and drops documents of vanished servers", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("ri_one", "ri_two")})
		slug := harness.Unique("ri")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		catalogWaitDocs(t, s, slug, 2)
		var total int
		if err := s.DB.QueryRow(t.Context(), "select count(*) from search.documents").Scan(&total); err != nil {
			t.Fatal(err)
		}
		execSQL(t, s, `insert into search.documents (ref, server, description, tsv) values ('ghost.x', 'ghost', 'd', to_tsvector('simple', 'x'))`)
		execSQL(t, s, "delete from search.documents where server = $1", slug)
		r := s.CLI(t, "", "reindex")
		if r.Code != 0 || r.Stderr != "" {
			t.Fatalf("reindex: code %d stderr %q", r.Code, r.Stderr)
		}
		// The count in the message is the number of documents written (the wording says servers).
		if want := fmt.Sprintf("reindexed %d server(s)\n", total); r.Stdout != want {
			t.Fatalf("reindex stdout %q, want %q", r.Stdout, want)
		}
		catalogWaitDocs(t, s, slug, 2)
		if n := countSQL(t, s, "select count(*) from search.documents where server = 'ghost'"); n != 0 {
			t.Fatalf("%d document(s) of a vanished server survived the reindex", n)
		}
		if n := countSQL(t, s, "select count(*) from search.documents"); n != total {
			t.Fatalf("search.documents has %d rows after the reindex, want %d", n, total)
		}
	})

	t.Run("access by groups", func(t *testing.T) { catalogSearchAccess(t, s, admin) })
}

// ---- access to search results ----

// catalogSearchAccess is the last group of TestCatalogSearch (same stack): its servers are made
// here and found by words no other group uses.
func catalogSearchAccess(t *testing.T, s *harness.Stack, admin *harness.Client) {
	adminMCP := s.MCP(t, admin.Token(t, "access-admin"))
	member := s.Member(t, harness.Unique("m"))
	memberMCP := s.MCP(t, member.Token(t, "access-member"))

	type srv struct {
		slug, tool, text string
		up               *fakes.Upstream
	}
	mk := func(prefix, tool, text string, opts ...fakes.ServerOpt) srv {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: []fakes.Tool{{Name: tool, Description: text, InputSchema: map[string]any{"type": "object"}}}})
		slug := harness.Unique(prefix)
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none", opts...))
		catalogWaitDocs(t, s, slug, 1)
		return srv{slug: slug, tool: slug + "." + tool, text: text, up: up}
	}
	open := mk("acco", "open_flux", "Calibrates the open flux capacitor.")
	grp := mk("accg", "group_flux", "Calibrates the group flux capacitor.", fakes.WithGroups("mcp-users"))
	closed := mk("accx", "closed_flux", "Calibrates the closed flux capacitor.", fakes.WithGroups("ops"))

	refsFor := func(c *mcp.ClientSession, query string) []string {
		return catalogHitRefs(catalogSearch(t, c, query, 25, nil))
	}

	t.Run("a member finds what their groups open and an admin finds everything", func(t *testing.T) {
		for _, q := range []string{"flux capacitor", "closed flux", "Calibrates the closed flux capacitor.", "closed_flux"} {
			if got := refsFor(memberMCP, q); slices.Contains(got, closed.tool) {
				t.Errorf("the member finds %s with %q: %v", closed.tool, q, got)
			}
		}
		// The admin sees every server and its tools (the control that makes the member's
		// emptiness mean something).
		adminGot := refsFor(adminMCP, "Calibrates the flux capacitor.")
		for _, want := range []string{open.tool, grp.tool, closed.tool} {
			if !slices.Contains(adminGot, want) {
				t.Fatalf("the admin does not find %s: %v", want, adminGot)
			}
		}
		for _, slug := range []string{open.slug, grp.slug, closed.slug} {
			if !catalogListedHas(catalogListServers(t, adminMCP), slug) {
				t.Fatalf("the admin's list_servers misses %s", slug)
			}
		}
		memberGot := refsFor(memberMCP, "flux capacitor")
		if !slices.Contains(memberGot, open.tool) || !slices.Contains(memberGot, grp.tool) {
			t.Fatalf("the member does not find tools open to them: %v", memberGot)
		}
	})

	t.Run("list_servers, filters, describe and call treat a closed server as unknown", func(t *testing.T) {
		list := catalogListServers(t, memberMCP)
		if !catalogListedHas(list, open.slug) || !catalogListedHas(list, grp.slug) || catalogListedHas(list, closed.slug) {
			t.Fatalf("the member's list_servers: %+v", list)
		}
		if !catalogListedHas(catalogListServers(t, adminMCP), closed.slug) {
			t.Fatal("the admin's list_servers misses the closed server")
		}
		res := catalogCallMCP(t, memberMCP, "search_tools", catalogSearchArgs("flux", 0, []string{closed.slug}))
		if !res.IsError || catalogText(res) != fmt.Sprintf("Unknown server %q. Call list_servers.", closed.slug) {
			t.Fatalf("search in a closed server: %q", catalogText(res))
		}
		nope := catalogCallMCP(t, memberMCP, "search_tools", catalogSearchArgs("flux", 0, []string{"no-such-server"}))
		if catalogText(nope) != fmt.Sprintf("Unknown server %q. Call list_servers.", "no-such-server") {
			t.Fatalf("search in an unknown server: %q", catalogText(nope))
		}
		for _, name := range []string{"describe_tool", "call_tool"} {
			res := catalogCallMCP(t, memberMCP, name, map[string]any{"tool": closed.tool})
			if !res.IsError || catalogText(res) != catalogNotAvailable(closed.tool) {
				t.Fatalf("%s of a closed tool: isError %v %q", name, res.IsError, catalogText(res))
			}
			if want := catalogNotAvailable(slugOf(closed.tool) + ".nope"); catalogText(catalogCallMCP(t, memberMCP, name, map[string]any{"tool": slugOf(closed.tool) + ".nope"})) != want {
				t.Fatalf("%s tells a closed server's missing tool apart from its existing one", name)
			}
		}
		if n := len(closed.up.Calls()); n != 0 {
			t.Fatalf("the upstream of a closed server was called %d time(s) by a member", n)
		}
		// Control: the admin may describe and call it.
		if res := catalogCallMCP(t, adminMCP, "call_tool", map[string]any{"tool": closed.tool}); res.IsError {
			t.Fatalf("the admin cannot call the closed tool: %s", catalogText(res))
		}
		if n := len(closed.up.Calls()); n != 1 {
			t.Fatalf("the upstream received %d call(s) from the admin, want 1", n)
		}
	})

	t.Run("changing the access takes effect at once", func(t *testing.T) {
		v := catalogGet(t, admin, closed.slug)
		setGroups := func(groups ...string) {
			t.Helper()
			r := admin.Do(t, "PUT", "/api/servers/"+closed.slug, catalogPutBody(v, func(b map[string]any) {
				b["access"] = map[string]any{"kind": "groups", "groups": groups}
			}))
			if r.Status != 200 {
				t.Fatalf("PUT access %v: %d %s", groups, r.Status, r.Body)
			}
		}
		setGroups("mcp-users")
		if got := refsFor(memberMCP, "Calibrates the closed flux capacitor."); !slices.Contains(got, closed.tool) {
			t.Fatalf("the member does not find the tool after the access was opened: %v", got)
		}
		if !catalogHas(catalogCatalog(t, member), closed.slug) {
			t.Fatal("the member's catalog misses the server after the access was opened")
		}
		setGroups("ops")
		if got := refsFor(memberMCP, "Calibrates the closed flux capacitor."); slices.Contains(got, closed.tool) {
			t.Fatalf("the member still finds the tool after the access was closed: %v", got)
		}
		if catalogHas(catalogCatalog(t, member), closed.slug) {
			t.Fatal("the member's catalog still has the server after the access was closed")
		}
	})
}

// catalogExpectInputError checks that a tool result is an input-validation error naming the field
// as a path token: an error that is neither a success nor the not_found text.
func catalogExpectInputError(t *testing.T, what string, res *mcp.CallToolResult, field string) {
	t.Helper()
	text := catalogText(res)
	if !res.IsError || text == "" || strings.Contains(text, "is not available") || !mcpFieldNamed(text, field) {
		t.Errorf("%s: isError %v %q, want an input-validation error that names the field %q", what, res.IsError, text, field)
	}
}

func slugOf(ref string) string {
	slug, _, _ := strings.Cut(ref, ".")
	return slug
}

// searchReadyProblem judges deviation ready-before-serving: after a start /healthz never answered
// 503 (healthz holds every status seen), and each query's first answer (made at once after the app
// became healthy) holds the same refs as its settled answer (made after polling). settled must be
// non-empty for at least one query, so that "equal" is not two empty answers. It returns "" when
// all of that holds.
func searchReadyProblem(healthz []int, first, settled map[string][]string) string {
	for _, code := range healthz {
		if code == 503 {
			return "/healthz answered 503 while the database was up"
		}
	}
	nonEmpty := false
	for query, want := range settled {
		if len(want) > 0 {
			nonEmpty = true
		}
		if got := first[query]; !slices.Equal(got, want) {
			return fmt.Sprintf("search %q answered %v right after the start and %v once settled", query, got, want)
		}
	}
	if !nonEmpty {
		return "every settled answer is empty: nothing was compared"
	}
	return ""
}

// TestSearchReadyBeforeServing: the app restarts with an indexed catalog. 1.0.0 loads its embedding
// model after it first answers healthy: until then /healthz can answer 503 and a search silently
// uses full text only, so an answer from the first instants can differ from the settled one. The
// decided behaviour (deviation ready-before-serving): once the app is healthy, /healthz never
// answers 503 while the database is up, and a search made at once returns the refs that the same
// search returns 5 s of polling later. The 1.0.0 branch asserts nothing about the first instants
// (the deviation), only that the settled search works.
func TestSearchReadyBeforeServing(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	admin := s.Admin(t)
	token := admin.Token(t, "search-ready")
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.FixtureTools("youtrack")})
	catalogRegister(t, admin, fakes.ServerInput(up, "youtrack", "none", fakes.WithName("YouTrack"), catalogWithDescription("Issue tracker")))
	catalogWaitDocs(t, s, "youtrack", 23)

	queries := []string{"create an issue in the tracker", "создать задачу в трекере"}
	search := func(c *mcp.ClientSession) map[string][]string {
		out := map[string][]string{}
		for _, q := range queries {
			out[q] = catalogHitRefs(catalogSearch(t, c, q, 10, []string{"youtrack"}))
		}
		return out
	}

	s.SwapImage(t, harness.DefaultImage(), "") // the restart; the catalog and its vectors are in the database
	c := s.MCP(t, token)
	if !s.Fixed(harness.DevReadyBeforeServing) {
		// 1.0.0: nothing is asserted about the first instants. The settled search must work.
		harness.Eventually(t, "the settled search to find youtrack.create_issue first", func() bool {
			hits := catalogHitRefs(catalogSearch(t, c, queries[0], 10, []string{"youtrack"}))
			return len(hits) > 0 && hits[0] == "youtrack.create_issue"
		})
		return
	}
	var healthz []int
	probe := func() { healthz = append(healthz, s.Anonymous().Do(t, "GET", "/healthz", nil).Status) }
	probe()
	first := search(c)
	probe()
	until := time.Now().Add(5 * time.Second)
	harness.Eventually(t, "5 s of polling", func() bool {
		probe()
		return time.Now().After(until)
	})
	settled := search(c)
	if problem := searchReadyProblem(healthz, first, settled); problem != "" {
		t.Error(problem)
	}
	if got := settled[queries[0]]; len(got) == 0 || got[0] != "youtrack.create_issue" {
		t.Errorf("the settled search %q: %v, want youtrack.create_issue first", queries[0], got)
	}
}
