//go:build compat

package compat_test

// Catalog, sync, upstream-client and search scenarios. The catalog is driven through REST
// (/api/servers*, /api/catalog), search and the gateway through MCP (search_tools, describe_tool,
// list_servers, call_tool), the index through SQL (search.documents) and the CLI (reindex).
//
// Two stacks: TestCatalogServers and TestCatalogUpstreamCalls share one with a small
// UPSTREAM_MAX_RESPONSE_BYTES (the size limit is tested with 80 KB bodies instead of 20 MiB);
// TestCatalogSearch has its own with the default limit, because the Gitea fixture is ~80 KB.
//
// Source cases that are not ported (they need the app's internals, not its outside):
//   - pg-catalog.int.test.ts "round-trips a server, including the sealed secret bytes" and
//     "stores the sealed bytes verbatim": sealed bytes are internal; the round trip of every
//     field through the REST API and "the stored secret is not the plaintext" are ported.
//   - "writes events to the outbox in the same call", "a taken slug ... writes no event",
//     "save updates ... no event", "saveSyncState writes ... no event", "a second identical
//     snapshot is a no-op" (the event part): the outbox is drained by a worker, its rows are
//     not stable to assert from outside. The effects (index and counts) are ported.
//   - "a stale save is refused, not merged" / "the other way round": two interleaved aggregate
//     reads. Ported through the API as TestCatalogOptimisticConcurrency (a row lock holds the
//     request between its read and its write); the version is not on the wire.
//   - "save after a key rotation re-sealed the secret keeps the new ciphertext (M2)": a race
//     between rotate-secrets-key and a save that cannot be placed from outside.
//   - "saveSyncState ignores an attempt older than the stored one": needs two overlapping
//     attempts with a controlled finish order.
//   - "applySnapshot / saveSyncState / save of an unknown server throws not_found": the REST
//     equivalents (404) are ported.
//   - mcp-upstream-client.int.test.ts "returns an image block byte for byte": the fake upstream
//     has no image mode and the pass-through of results is a gateway scenario.
//   - "a JSON-RPC internal error ... listTools is rejected too": the fake can only reject
//     tools/call.
//   - search.int.test.ts "embedder outages" (keeps text search working, the sweep fills the
//     embeddings in later, skips a document marked as failed): the embedder cannot be made to
//     fail from outside. Search over text-only documents is covered by the Go unit tests of
//     milestone 4. "reindexAll rebuilds the index" is ported through the reindex command.
//   - search.int.test.ts uses a bag-of-tokens fake embedder at similarity 0.2; the scenarios
//     here use the real model and the default threshold, so their queries and expected values
//     differ (stated at each case).
//   - embedder.int.test.ts: the embedder itself; judged by compat/golden/embeddings-*.json in
//     milestone 4.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

const (
	catalogMaxBytes = 65536 // UPSTREAM_MAX_RESPONSE_BYTES of the first stack
	catalogCSRF     = "csrf|cross-origin request rejected"
	catalogAdminErr = "forbidden|admin only"

	catalogMsgNeedsConnection = "An administrator needs to connect this server"
	catalogMsg401             = "Upstream rejected the credentials (HTTP 401)"
	catalogMsgUnreachable     = "Could not reach the upstream"
	catalogMsgTimeout         = "Upstream did not respond in time"
	catalogMsgTooLarge        = "The upstream response exceeds the allowed size"
)

// ---- views and helpers ----

// catalogView is the ServerView contract.
type catalogView struct {
	Slug          string         `json:"slug"`
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	URL           string         `json:"url"`
	Auth          map[string]any `json:"auth"`
	SecretSet     bool           `json:"secretSet"`
	Access        map[string]any `json:"access"`
	TimeoutMs     int            `json:"timeoutMs"`
	DailyLimit    *int           `json:"dailyLimit"`
	Status        string         `json:"status"`
	LastSyncAt    *string        `json:"lastSyncAt"`
	LastSyncError *string        `json:"lastSyncError"`
	ToolCount     int            `json:"toolCount"`
}

func (v catalogView) syncError() string {
	if v.LastSyncError == nil {
		return "<nil>"
	}
	return *v.LastSyncError
}

func (v catalogView) syncedAt(t *testing.T) time.Time {
	t.Helper()
	if v.LastSyncAt == nil {
		t.Fatalf("server %s: lastSyncAt is null", v.Slug)
	}
	at, err := time.Parse(time.RFC3339Nano, *v.LastSyncAt)
	if err != nil {
		t.Fatalf("server %s: lastSyncAt %q: %v", v.Slug, *v.LastSyncAt, err)
	}
	return at
}

// catalogAdminTool is the AdminTool contract.
type catalogAdminTool struct {
	Ref          string         `json:"ref"`
	Title        *string        `json:"title"`
	Description  string         `json:"description"`
	Annotations  map[string]any `json:"annotations"`
	Name         string         `json:"name"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema"`
	Hidden       bool           `json:"hidden"`
}

type catalogSyncResult struct{ Added, Changed, Removed int }

// catalogEntry is the CatalogServer contract.
type catalogEntry struct {
	Slug, Name, Description string
	Tools                   []struct {
		Ref         string  `json:"ref"`
		Title       *string `json:"title"`
		Description string  `json:"description"`
	} `json:"tools"`
}

func catalogExpectError(t *testing.T, what string, r harness.Response, status int, want string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("%s: status %d, want %d (body %.300s)", what, r.Status, status, r.Body)
	}
	if got := errorCode(t, r); got != want {
		t.Fatalf("%s: error %q, want %q", what, got, want)
	}
}

func catalogRegister(t *testing.T, admin *harness.Client, body map[string]any) catalogView {
	t.Helper()
	r := admin.Do(t, "POST", "/api/servers", body)
	if r.Status != 201 {
		t.Fatalf("POST /api/servers (%v): %d %.300s", body["slug"], r.Status, r.Body)
	}
	var v catalogView
	r.JSON(t, "ServerView", &v)
	if v.Slug != body["slug"] {
		t.Fatalf("POST /api/servers: slug %q, want %v", v.Slug, body["slug"])
	}
	return v
}

func catalogGet(t *testing.T, admin *harness.Client, slug string) catalogView {
	t.Helper()
	r := admin.Do(t, "GET", "/api/servers/"+slug, nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/servers/%s: %d %.300s", slug, r.Status, r.Body)
	}
	var v catalogView
	r.JSON(t, "ServerView", &v)
	return v
}

func catalogSyncOK(t *testing.T, admin *harness.Client, slug string) catalogSyncResult {
	t.Helper()
	r := admin.Do(t, "POST", "/api/servers/"+slug+"/sync", nil)
	if r.Status != 200 {
		t.Fatalf("POST /api/servers/%s/sync: %d %.300s", slug, r.Status, r.Body)
	}
	var res catalogSyncResult
	r.JSON(t, "SyncResult", &res)
	return res
}

func catalogTools(t *testing.T, admin *harness.Client, slug string) (raw []byte, tools []catalogAdminTool) {
	t.Helper()
	r := admin.Do(t, "GET", "/api/servers/"+slug+"/tools", nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/servers/%s/tools: %d %.300s", slug, r.Status, r.Body)
	}
	r.JSON(t, "AdminToolList", &tools)
	return r.Body, tools
}

func catalogToolNames(tools []catalogAdminTool) []string {
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func catalogHide(t *testing.T, admin *harness.Client, slug, name string, hidden bool) {
	t.Helper()
	r := admin.Do(t, "PUT", "/api/servers/"+slug+"/tools/"+name+"/hidden", map[string]bool{"hidden": hidden})
	if r.Status != 204 {
		t.Fatalf("PUT hidden %s.%s=%v: %d %.300s", slug, name, hidden, r.Status, r.Body)
	}
}

func catalogCatalog(t *testing.T, c *harness.Client) []catalogEntry {
	t.Helper()
	r := c.Do(t, "GET", "/api/catalog", nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/catalog: %d %.300s", r.Status, r.Body)
	}
	var out []catalogEntry
	r.JSON(t, "CatalogResponse", &out)
	return out
}

func catalogHas(entries []catalogEntry, slug string) bool {
	return slices.ContainsFunc(entries, func(e catalogEntry) bool { return e.Slug == slug })
}

func catalogRefs(entries []catalogEntry, slug string) []string {
	for _, e := range entries {
		if e.Slug == slug {
			refs := make([]string, 0, len(e.Tools))
			for _, tl := range e.Tools {
				refs = append(refs, tl.Ref)
			}
			return refs
		}
	}
	return nil
}

// catalogPutBody is the PUT /api/servers/:slug body that leaves the server as it is, then mut.
func catalogPutBody(v catalogView, mut func(map[string]any)) map[string]any {
	b := map[string]any{
		"name": v.Name, "description": v.Description, "url": v.URL, "auth": v.Auth,
		"access": v.Access, "timeoutMs": v.TimeoutMs, "dailyLimit": v.DailyLimit,
	}
	if mut != nil {
		mut(b)
	}
	return b
}

func catalogWithDescription(desc string) fakes.ServerOpt {
	return func(b map[string]any) { b["description"] = desc }
}

// catalogOAuth turns the body into an oauth_user one; mut edits the oauth object.
func catalogOAuth(mut func(o map[string]any)) fakes.ServerOpt {
	return func(b map[string]any) {
		a, _ := b["auth"].(map[string]any)
		a["kind"] = "oauth_user"
		o := map[string]any{ //nolint:gosec // G101: made-up OAuth endpoints of a test server, not credentials
			"authorizeUrl": "https://git.example.com/authorize", "tokenUrl": "https://git.example.com/token",
			"clientId": "cid", "scopes": []string{"read"},
		}
		if mut != nil {
			mut(o)
		}
		a["oauth"] = o
	}
}

func catalogSet(key string, value any) fakes.ServerOpt {
	return func(b map[string]any) { b[key] = value }
}

func catalogAuthSet(key string, value any) fakes.ServerOpt {
	return func(b map[string]any) {
		a, _ := b["auth"].(map[string]any)
		a[key] = value
	}
}

func catalogBigTools(n, size int) []fakes.Tool {
	out := make([]fakes.Tool, 0, n)
	for i := range n {
		out = append(out, fakes.Tool{
			Name:        fmt.Sprintf("big%02d", i),
			Description: strings.Repeat("y", size),
			InputSchema: map[string]any{"type": "object"},
		})
	}
	return out
}

func catalogSyncUser(t *testing.T, s *harness.Stack, slug string) string {
	t.Helper()
	var id *string
	if err := s.DB.QueryRow(t.Context(), "select sync_user_id::text from catalog.servers where slug = $1", slug).Scan(&id); err != nil {
		t.Fatalf("sync_user_id of %s: %v", slug, err)
	}
	if id == nil {
		return "<null>"
	}
	return *id
}

// catalogWaitDocs waits until search.documents holds exactly want rows for the server and every
// one of them has an embedding, so a search assertion never races the indexer or the embedder.
func catalogWaitDocs(t *testing.T, s *harness.Stack, server string, want int) {
	t.Helper()
	var total, embedded int
	harness.Eventually(t, fmt.Sprintf("%d embedded documents of %s in search.documents", want, server), func() bool {
		err := s.DB.QueryRow(t.Context(), "select count(*), count(embedding) from search.documents where server = $1", server).Scan(&total, &embedded)
		return err == nil && total == want && embedded == want
	})
}

// ---- MCP helpers ----

type catalogHit struct {
	Tool        string          `json:"tool"`
	Title       *string         `json:"title"`
	Description string          `json:"description"`
	Server      string          `json:"server"`
	Score       float64         `json:"score"`
	Annotations map[string]bool `json:"annotations"`
	Connection  string          `json:"connection"`
}

func catalogCallMCP(t *testing.T, c *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s %v: protocol error %v", name, args, err)
	}
	return res
}

func catalogText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func catalogStructured(t *testing.T, res *mcp.CallToolResult, into any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", catalogText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("structured content %.300s: %v", raw, err)
	}
}

func catalogSearchArgs(query string, limit int, servers []string) map[string]any {
	args := map[string]any{"query": query}
	if limit > 0 {
		args["limit"] = limit
	}
	if servers != nil {
		args["servers"] = servers
	}
	return args
}

func catalogSearch(t *testing.T, c *mcp.ClientSession, query string, limit int, servers []string) []catalogHit {
	t.Helper()
	res := catalogCallMCP(t, c, "search_tools", catalogSearchArgs(query, limit, servers))
	var out struct {
		Results []catalogHit `json:"results"`
	}
	catalogStructured(t, res, &out)
	return out.Results
}

func catalogHitRefs(hits []catalogHit) []string {
	refs := make([]string, 0, len(hits))
	for _, h := range hits {
		refs = append(refs, h.Tool)
	}
	return refs
}

type catalogListed struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Tools       int    `json:"tools"`
	Connection  string `json:"connection"`
	ConnectURL  string `json:"connectUrl"`
}

func catalogListServers(t *testing.T, c *mcp.ClientSession) []catalogListed {
	t.Helper()
	var out struct {
		Servers []catalogListed `json:"servers"`
	}
	catalogStructured(t, catalogCallMCP(t, c, "list_servers", map[string]any{}), &out)
	return out.Servers
}

func catalogListedHas(list []catalogListed, slug string) bool {
	return slices.ContainsFunc(list, func(l catalogListed) bool { return l.Slug == slug })
}

func catalogNotAvailable(ref string) string {
	return fmt.Sprintf("Tool %q is not available. Use search_tools to find another.", ref)
}

// catalogRaceWrite sends a write that reads the server first and writes it later while another
// admin's change commits in between: a row lock holds the request's UPDATE until the other
// change is in. It returns the request's answer.
func catalogRaceWrite(t *testing.T, s *harness.Stack, slug string, send func() harness.Response) harness.Response {
	t.Helper()
	ctx := t.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	if _, err := tx.Exec(ctx, "select 1 from catalog.servers where slug = $1 for update", slug); err != nil {
		t.Fatal(err)
	}
	answer := make(chan harness.Response, 1)
	go func() { answer <- send() }()
	harness.Eventually(t, "the request to wait for the row lock of its UPDATE", func() bool {
		var n int
		err := s.DB.QueryRow(ctx, `select count(*) from pg_stat_activity
			where wait_event_type = 'Lock' and query ilike 'update catalog.servers%'`).Scan(&n)
		return err == nil && n > 0
	})
	// The other admin's change commits first: it bumps the version the request read.
	if _, err := tx.Exec(ctx, "update catalog.servers set version = version + 1, description = 'theirs' where slug = $1", slug); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-answer:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("the request did not finish after the row lock was released")
		return harness.Response{}
	}
}

// ---- TestCatalogServers ----

func catalogStackSmallLimit(t *testing.T) *harness.Stack {
	t.Helper()
	return harness.Start(t, harness.Options{Env: map[string]string{"UPSTREAM_MAX_RESPONSE_BYTES": fmt.Sprint(catalogMaxBytes)}})
}

func TestCatalogServers(t *testing.T) {
	s := catalogStackSmallLimit(t)
	admin := s.Admin(t)
	adminID := whoami(t, admin).UserID

	t.Run("registering a server synchronises it at once", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("list_repos", "delete_repo")})
		slug := harness.Unique("reg")
		v := catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		if v.ToolCount != 2 || v.LastSyncError != nil || v.LastSyncAt == nil || v.SecretSet || v.Status != "active" {
			t.Fatalf("created view: toolCount %d, lastSyncError %q, lastSyncAt %v, secretSet %v, status %s", v.ToolCount, v.syncError(), v.LastSyncAt, v.SecretSet, v.Status)
		}
		if n := len(up.Requests()); n == 0 || up.ListRequests() != 1 {
			t.Fatalf("upstream saw %d requests, %d tools/list: registering must list the tools exactly once", n, up.ListRequests())
		}
		_, tools := catalogTools(t, admin, slug)
		if got := catalogToolNames(tools); !slices.Equal(got, []string{"delete_repo", "list_repos"}) || tools[0].Ref != slug+"."+tools[0].Name {
			t.Fatalf("tools %v (first ref %q)", got, tools[0].Ref)
		}
		if got := catalogGet(t, admin, slug); got.ToolCount != 2 || got.syncedAt(t) != v.syncedAt(t) {
			t.Fatalf("GET after create: toolCount %d, lastSyncAt %v vs %v", got.ToolCount, got.LastSyncAt, v.LastSyncAt)
		}
		entries := auditAll(t, admin)
		for _, action := range []string{"server.create", "server.sync"} {
			if len(auditWhere(entries, action, func(e auditEntry) bool { return ptrIs(e.Target, slug) && ptrIs(e.ActorID, adminID) })) == 0 {
				t.Errorf("no %s audit record for %s by the admin", action, slug)
			}
		}
		// Negative control: an unknown server has no tools to list.
		catalogExpectError(t, "GET tools of an unknown server", admin.Do(t, "GET", "/api/servers/nope-x/tools", nil), 404, `not_found|server "nope-x" not found`)
	})

	t.Run("a failing first sync does not undo the creation", func(t *testing.T) {
		unreachable := func(b map[string]any) { b["url"] = "http://127.0.0.1:1/mcp" }
		cases := []struct {
			name   string
			arm    func(*fakes.Upstream)
			auth   string
			opts   []fakes.ServerOpt
			wantMs string
		}{
			{"upstream answers 401", func(u *fakes.Upstream) { u.FailWith(401) }, "none", nil, catalogMsg401},
			{"upstream answers 500", func(u *fakes.Upstream) { u.FailWith(500) }, "none", nil, catalogMsgUnreachable},
			{"upstream unreachable", func(*fakes.Upstream) {}, "none", []fakes.ServerOpt{unreachable}, catalogMsgUnreachable},
			{"pat_user has no connection", func(*fakes.Upstream) {}, "pat_user", nil, catalogMsgNeedsConnection},
			{"oauth_user has no connection", func(*fakes.Upstream) {}, "oauth_user", []fakes.ServerOpt{catalogOAuth(nil)}, catalogMsgNeedsConnection},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a", "b")})
				c.arm(up)
				slug := harness.Unique("fail")
				v := catalogRegister(t, admin, fakes.ServerInput(up, slug, c.auth, c.opts...))
				if v.ToolCount != 0 || v.LastSyncError == nil || *v.LastSyncError != c.wantMs || v.LastSyncAt == nil {
					t.Fatalf("created view: toolCount %d, lastSyncError %q (want %q), lastSyncAt %v", v.ToolCount, v.syncError(), c.wantMs, v.LastSyncAt)
				}
				if got := catalogGet(t, admin, slug); got.LastSyncError == nil || *got.LastSyncError != c.wantMs {
					t.Fatalf("GET: lastSyncError %q, want %q", got.syncError(), c.wantMs)
				}
				if _, tools := catalogTools(t, admin, slug); len(tools) != 0 {
					t.Fatalf("tools after a failed first sync: %v", catalogToolNames(tools))
				}
			})
		}
	})

	t.Run("a failed sync keeps the tools and records the reason", func(t *testing.T) {
		other := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("z")})
		cases := []struct {
			name    string
			page    int
			arm     func(u *fakes.Upstream)
			status  int
			code    string
			message string
			within  time.Duration
		}{
			{name: "HTTP 401", arm: func(u *fakes.Upstream) { u.FailWith(401) }, status: 502, code: "sync_failed", message: catalogMsg401},
			{name: "HTTP 500", arm: func(u *fakes.Upstream) { u.FailWith(500) }, status: 502, code: "sync_failed", message: catalogMsgUnreachable},
			{name: "HTTP 403", arm: func(u *fakes.Upstream) { u.FailWith(403) }, status: 502, code: "sync_failed", message: catalogMsgUnreachable},
			{name: "HTTP 404", arm: func(u *fakes.Upstream) { u.FailWith(404) }, status: 502, code: "sync_failed", message: catalogMsgUnreachable},
			{name: "a redirect is not followed", arm: func(u *fakes.Upstream) { u.RedirectTo(other.URL) }, status: 502, code: "sync_failed", message: catalogMsgUnreachable},
			{name: "slower than timeoutMs", arm: func(u *fakes.Upstream) { u.Delay(5 * time.Second) }, status: 502, code: "sync_failed", message: catalogMsgTimeout, within: 4 * time.Second},
			{name: "one answer above UPSTREAM_MAX_RESPONSE_BYTES", arm: func(u *fakes.Upstream) { u.SetTools(catalogBigTools(40, 2000)) }, status: 502, code: "sync_failed", message: catalogMsgTooLarge},
			{name: "many small pages add up above the limit", page: 1, arm: func(u *fakes.Upstream) { u.SetTools(catalogBigTools(5, 20000)) }, status: 502, code: "sync_failed", message: catalogMsgTooLarge},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a", "b"), PageSize: c.page})
				slug := harness.Unique("keep")
				created := catalogRegister(t, admin, fakes.ServerInput(up, slug, "none", fakes.WithTimeoutMs(1000)))
				if created.ToolCount != 2 || created.LastSyncError != nil {
					t.Fatalf("setup: toolCount %d, lastSyncError %q", created.ToolCount, created.syncError())
				}
				// A successful sync by the admin records them as the sync owner.
				catalogSyncOK(t, admin, slug)
				if got := catalogSyncUser(t, s, slug); got != adminID {
					t.Fatalf("sync_user_id %s, want the admin %s", got, adminID)
				}
				beforeRaw, _ := catalogTools(t, admin, slug)
				before := catalogGet(t, admin, slug)
				other.Reset()

				c.arm(up)
				started := time.Now()
				r := admin.Do(t, "POST", "/api/servers/"+slug+"/sync", nil)
				catalogExpectError(t, "sync", r, c.status, c.code+"|"+c.message)
				if c.within > 0 && time.Since(started) > c.within {
					t.Fatalf("the sync took %s, timeoutMs is 1000", time.Since(started))
				}

				after := catalogGet(t, admin, slug)
				if after.LastSyncError == nil || *after.LastSyncError != c.message || after.ToolCount != 2 {
					t.Fatalf("view after: lastSyncError %q (want %q), toolCount %d (want 2)", after.syncError(), c.message, after.ToolCount)
				}
				if !after.syncedAt(t).After(before.syncedAt(t)) {
					t.Fatalf("lastSyncAt did not move on a failed attempt: %v -> %v", before.LastSyncAt, after.LastSyncAt)
				}
				if rawAfter, _ := catalogTools(t, admin, slug); !bytes.Equal(beforeRaw, rawAfter) {
					t.Fatalf("the tool list changed after a failed sync:\n%s\n%s", beforeRaw, rawAfter)
				}
				if got := catalogSyncUser(t, s, slug); got != adminID {
					t.Fatalf("a failed sync changed sync_user_id to %s", got)
				}
				if n := len(other.Requests()); n != 0 {
					t.Fatalf("the redirect target received %d request(s): credentials must not follow a redirect", n)
				}

				// Recovery (negative control): the same server syncs again once the upstream is fine.
				up.Reset()
				up.SetTools(fakes.Tools("a", "b"))
				if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{}) {
					t.Fatalf("sync after recovery: %+v, want nothing changed", res)
				}
				if v := catalogGet(t, admin, slug); v.LastSyncError != nil || v.ToolCount != 2 {
					t.Fatalf("view after recovery: lastSyncError %q, toolCount %d", v.syncError(), v.ToolCount)
				}
			})
		}
	})

	t.Run("an answer below the limit is accepted", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: catalogBigTools(20, 2000)})
		slug := harness.Unique("under")
		if v := catalogRegister(t, admin, fakes.ServerInput(up, slug, "none")); v.ToolCount != 20 || v.LastSyncError != nil {
			t.Fatalf("toolCount %d, lastSyncError %q: a 40 KB answer is below the %d byte limit", v.ToolCount, v.syncError(), catalogMaxBytes)
		}
	})

	t.Run("shared_secret: every page of tools/list is fetched with the secret header", func(t *testing.T) {
		tools := fakes.Tools("a", "b", "c.d", "e", "f", "g", "h")
		tools[1].Annotations = map[string]any{"readOnlyHint": true}
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: tools, PageSize: 2, RequireHeader: [2]string{"X-API-Key", "key k-1"}})
		slug := harness.Unique("page")
		v := catalogRegister(t, admin, fakes.ServerInput(up, slug, "shared_secret", fakes.WithHeader("X-API-Key", "key {secret}"), fakes.WithSecret("k-1")))
		if v.ToolCount != 7 || v.LastSyncError != nil || !v.SecretSet {
			t.Fatalf("toolCount %d, lastSyncError %q, secretSet %v", v.ToolCount, v.syncError(), v.SecretSet)
		}
		if n := up.ListRequests(); n != 4 {
			t.Fatalf("tools/list requests: %d, want 4 pages (7 tools, 2 per page)", n)
		}
		methods := map[string]int{}
		for _, req := range up.Requests() {
			methods[req.Method]++
			if got := req.Header.Get("X-API-Key"); got != "key k-1" {
				t.Errorf("a %q request carried X-API-Key %q, want %q", req.Method, got, "key k-1")
			}
		}
		if methods["tools/list"] != 4 {
			t.Fatalf("requests by method: %v", methods)
		}
		_, got := catalogTools(t, admin, slug)
		if names := catalogToolNames(got); !slices.Equal(names, []string{"a", "b", "c.d", "e", "f", "g", "h"}) {
			t.Fatalf("tools: %v", names)
		}
		for _, tl := range got {
			if tl.Name == "b" && tl.Annotations["readOnlyHint"] != true {
				t.Errorf("annotations of b: %v", tl.Annotations)
			}
			if tl.Ref != slug+"."+tl.Name {
				t.Errorf("ref %q of tool %q: the ref is <slug>.<name>, split at the first dot", tl.Ref, tl.Name)
			}
		}
		// A dotted upstream name is addressed by the part after the slug.
		catalogHide(t, admin, slug, "c.d", true)
		if v := catalogGet(t, admin, slug); v.ToolCount != 6 {
			t.Fatalf("toolCount after hiding c.d: %d, want 6", v.ToolCount)
		}
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{}) {
			t.Fatalf("second sync: %+v, want nothing changed", res)
		}
	})

	t.Run("the secret header changes with the secret and a wrong one is refused", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a", "b"), RequireHeader: [2]string{"X-API-Key", "key good"}})
		slug := harness.Unique("hdr")
		v := catalogRegister(t, admin, fakes.ServerInput(up, slug, "shared_secret", fakes.WithHeader("X-API-Key", "key {secret}"), fakes.WithSecret("bad")))
		if v.ToolCount != 0 || v.LastSyncError == nil || *v.LastSyncError != catalogMsg401 {
			t.Fatalf("with a wrong secret: toolCount %d, lastSyncError %q", v.ToolCount, v.syncError())
		}
		for _, req := range up.Requests() {
			if got := req.Header.Get("X-API-Key"); got != "key bad" {
				t.Errorf("a %q request carried X-API-Key %q, want %q", req.Method, got, "key bad")
			}
		}
		if r := admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{"secret": "good"}); r.Status != 204 || len(r.Body) != 0 {
			t.Fatalf("PUT secret: %d %s", r.Status, r.Body)
		}
		up.Reset() // forget the requests made with the old secret
		if res := catalogSyncOK(t, admin, slug); res.Added != 2 {
			t.Fatalf("sync with the right secret: %+v", res)
		}
		reqs := up.Requests()
		if len(reqs) == 0 {
			t.Fatal("the sync reached the upstream with no request")
		}
		for _, req := range reqs {
			if got := req.Header.Get("X-API-Key"); got != "key good" {
				t.Errorf("a %q request after the change carried X-API-Key %q: the old target's header must not be reused", req.Method, got)
			}
		}
	})

	t.Run("personal credentials: sync says needs_connection and the upstream is not called", func(t *testing.T) {
		for _, kind := range []string{"pat_user", "oauth_user"} {
			t.Run(kind, func(t *testing.T) {
				up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
				var opts []fakes.ServerOpt
				if kind == "oauth_user" {
					opts = append(opts, catalogOAuth(nil))
				}
				slug := harness.Unique("pers")
				catalogRegister(t, admin, fakes.ServerInput(up, slug, kind, opts...))
				before := catalogGet(t, admin, slug)
				r := admin.Do(t, "POST", "/api/servers/"+slug+"/sync", nil)
				catalogExpectError(t, "sync", r, 400, "needs_connection|"+catalogMsgNeedsConnection)
				if n := len(up.Requests()); n != 0 {
					t.Fatalf("the upstream received %d request(s) without any credential to send", n)
				}
				after := catalogGet(t, admin, slug)
				if after.LastSyncError == nil || *after.LastSyncError != catalogMsgNeedsConnection || !after.syncedAt(t).After(before.syncedAt(t)) {
					t.Fatalf("view: lastSyncError %q, lastSyncAt %v -> %v", after.syncError(), before.LastSyncAt, after.LastSyncAt)
				}
			})
		}
	})

	t.Run("sync counts what changed", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("list_repos", "delete_repo")})
		slug := harness.Unique("cnt")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{}) {
			t.Fatalf("identical snapshot: %+v, want {0 0 0}", res)
		}
		changed := fakes.Tools("list_repos", "new_tool")
		changed[0].Description = "Lists repositories, now with paging."
		up.SetTools(changed)
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{Added: 1, Changed: 1, Removed: 1}) {
			t.Fatalf("one added, one changed, one removed: %+v", res)
		}
		_, tools := catalogTools(t, admin, slug)
		if got := catalogToolNames(tools); !slices.Equal(got, []string{"list_repos", "new_tool"}) {
			t.Fatalf("tools: %v", got)
		}
		for _, tl := range tools {
			if tl.Name == "list_repos" && tl.Description != "Lists repositories, now with paging." {
				t.Errorf("description of list_repos: %q", tl.Description)
			}
		}
	})

	t.Run("the snapshot keeps title, schemas and annotations", func(t *testing.T) {
		in := map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}, "a": map[string]any{"type": "number"}}, "required": []any{"a"}}
		out := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: []fakes.Tool{
			{Name: "issues.create", Title: "Create issue", Description: "Creates an issue.", InputSchema: in, OutputSchema: out, Annotations: map[string]any{"readOnlyHint": true, "idempotentHint": false}},
			{Name: "plain", Description: "Plain.", InputSchema: map[string]any{"type": "object"}},
		}})
		slug := harness.Unique("snap")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		_, tools := catalogTools(t, admin, slug)
		byName := map[string]catalogAdminTool{}
		for _, tl := range tools {
			byName[tl.Name] = tl
		}
		tl := byName["issues.create"]
		if tl.Ref != slug+".issues.create" || tl.Title == nil || *tl.Title != "Create issue" || tl.Description != "Creates an issue." || tl.Hidden {
			t.Fatalf("issues.create: %+v", tl)
		}
		if !reflect.DeepEqual(tl.InputSchema, in) || !reflect.DeepEqual(tl.OutputSchema, out) {
			t.Fatalf("schemas: input %v output %v", tl.InputSchema, tl.OutputSchema)
		}
		if !reflect.DeepEqual(tl.Annotations, map[string]any{"readOnlyHint": true, "idempotentHint": false}) {
			t.Fatalf("annotations: %v", tl.Annotations)
		}
		if p := byName["plain"]; p.Title != nil || p.OutputSchema != nil || len(p.Annotations) != 0 {
			t.Fatalf("plain: %+v", p)
		}
	})

	t.Run("hidden survives sync, removal and return; other servers are untouched", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a", "drop")})
		slug := harness.Unique("hid")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		neighbour := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("z")})
		nslug := harness.Unique("nb")
		catalogRegister(t, admin, fakes.ServerInput(neighbour, nslug, "none"))

		catalogHide(t, admin, slug, "drop", true)
		changed := fakes.Tools("a", "drop")
		changed[0].Description = "A, changed."
		up.SetTools(changed)
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{Changed: 1}) {
			t.Fatalf("sync with a hidden tool: %+v", res)
		}
		if _, tools := catalogTools(t, admin, slug); !slices.ContainsFunc(tools, func(tl catalogAdminTool) bool { return tl.Name == "drop" && tl.Hidden }) {
			t.Fatalf("drop is no longer hidden after a sync: %+v", tools)
		}
		if v := catalogGet(t, admin, slug); v.ToolCount != 1 {
			t.Fatalf("toolCount counts visible tools only: %d, want 1", v.ToolCount)
		}

		up.SetTools(fakes.Tools("a"))
		if res := catalogSyncOK(t, admin, slug); res.Removed != 1 {
			t.Fatalf("a vanished tool: %+v", res)
		}
		_, tools := catalogTools(t, admin, slug)
		if got := catalogToolNames(tools); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("tools after the removal: %v", got)
		}
		up.SetTools(fakes.Tools("a", "drop"))
		if res := catalogSyncOK(t, admin, slug); res.Added != 1 {
			t.Fatalf("the tool returns: %+v", res)
		}
		_, tools = catalogTools(t, admin, slug)
		if !slices.ContainsFunc(tools, func(tl catalogAdminTool) bool { return tl.Name == "drop" && tl.Hidden }) {
			t.Fatalf("a tool that came back lost its hidden flag: %+v", tools)
		}
		catalogHide(t, admin, slug, "drop", false)
		if v := catalogGet(t, admin, slug); v.ToolCount != 2 {
			t.Fatalf("toolCount after unhiding: %d, want 2", v.ToolCount)
		}

		// An upstream with no tools leaves the other server's tools alone.
		up.SetTools(nil)
		if res := catalogSyncOK(t, admin, slug); res.Removed != 2 {
			t.Fatalf("empty snapshot: %+v", res)
		}
		if v := catalogGet(t, admin, nslug); v.ToolCount != 1 {
			t.Fatalf("the neighbour server has %d tools, want 1", v.ToolCount)
		}
	})

	t.Run("the server list is ordered by slug and counts visible tools only", func(t *testing.T) {
		suffix := harness.Unique("ord")
		beta, alpha := "b"+suffix, "a"+suffix
		ub := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("x")})
		ua := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a", "b", "c")})
		catalogRegister(t, admin, fakes.ServerInput(ub, beta, "none"))
		catalogRegister(t, admin, fakes.ServerInput(ua, alpha, "none"))
		catalogHide(t, admin, alpha, "b", true)
		ua.SetTools(fakes.Tools("a", "b"))
		catalogSyncOK(t, admin, alpha) // c is removed
		r := admin.Do(t, "GET", "/api/servers", nil)
		var list []catalogView
		r.JSON(t, "ServerList", &list)
		var order []string
		counts := map[string]int{}
		for _, v := range list {
			counts[v.Slug] = v.ToolCount
			if v.Slug == alpha || v.Slug == beta {
				order = append(order, v.Slug)
			}
		}
		if !slices.Equal(order, []string{alpha, beta}) || counts[alpha] != 1 || counts[beta] != 1 {
			t.Fatalf("order %v counts %v, want [%s %s] with 1 and 1", order, counts, alpha, beta)
		}
		if !slices.IsSortedFunc(list, func(a, b catalogView) int { return strings.Compare(a.Slug, b.Slug) }) {
			t.Fatalf("GET /api/servers is not ordered by slug")
		}
	})

	t.Run("no response, log or stored value contains a secret", func(t *testing.T) {
		const secret, clientSecret = "sk-live-TOPSECRET-7f3a9c", "oauth-client-SECRET-91bd" //nolint:gosec // G101: made-up secrets that the test hunts for
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("list_repos")})
		shared, hub := harness.Unique("sec"), harness.Unique("hub")
		catalogRegister(t, admin, fakes.ServerInput(up, shared, "shared_secret", fakes.WithSecret(secret)))
		catalogRegister(t, admin, fakes.ServerInput(up, hub, "oauth_user", catalogOAuth(nil), fakes.WithSecret(clientSecret)))
		var bodies []string
		record := func(r harness.Response) { bodies = append(bodies, string(r.Body)) }
		record(admin.Do(t, "GET", "/api/servers", nil))
		record(admin.Do(t, "GET", "/api/servers/"+shared, nil))
		record(admin.Do(t, "GET", "/api/servers/"+hub, nil))
		record(admin.Do(t, "GET", "/api/servers/"+shared+"/tools", nil))
		record(admin.Do(t, "PUT", "/api/servers/"+shared+"/secret", map[string]any{"secret": secret + "-2"}))
		record(admin.Do(t, "POST", "/api/servers/"+shared+"/sync", nil))
		record(admin.Do(t, "PUT", "/api/servers/"+shared, catalogPutBody(catalogGet(t, admin, shared), func(b map[string]any) { b["description"] = "changed" })))
		record(admin.Do(t, "POST", "/api/servers/"+shared+"/status", map[string]any{"status": "disabled"}))
		record(admin.Do(t, "POST", "/api/servers/"+shared+"/status", map[string]any{"status": "active"}))
		record(admin.Do(t, "GET", "/api/audit?limit=500", nil))
		record(admin.Do(t, "GET", "/api/catalog", nil))
		record(admin.Do(t, "GET", "/api/calls", nil))
		record(admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, "Bad Slug", "shared_secret", fakes.WithSecret(secret))))
		all := strings.Join(bodies, "\n") + s.Logs(t)
		for _, sec := range []string{secret, secret + "-2", clientSecret} {
			if strings.Contains(all, sec) {
				t.Errorf("a response or the app log contains the secret %s", harness.Redact(sec))
			}
		}
		if !strings.Contains(bodies[1], `"secretSet":true`) || !strings.Contains(bodies[2], `"secretSet":true`) {
			t.Errorf("secretSet is not true for a server with a secret: %.200s", bodies[1])
		}
		var stored []byte
		if err := s.DB.QueryRow(t.Context(), "select secret from catalog.servers where slug = $1", shared).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if len(stored) == 0 || bytes.Contains(stored, []byte(secret)) {
			t.Fatalf("the stored secret is %d bytes and must be sealed, not the plaintext", len(stored))
		}
	})

	t.Run("PUT /secret sets and clears; a change of auth kind clears the stored secret", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
		slug := harness.Unique("sk")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "shared_secret"))
		if catalogGet(t, admin, slug).SecretSet {
			t.Fatal("secretSet before any secret")
		}
		if r := admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{"secret": "s3"}); r.Status != 204 {
			t.Fatalf("set: %d %s", r.Status, r.Body)
		}
		if !catalogGet(t, admin, slug).SecretSet {
			t.Fatal("secretSet is false after setting a secret")
		}
		if r := admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{"secret": nil}); r.Status != 204 {
			t.Fatalf("clear: %d %s", r.Status, r.Body)
		}
		if catalogGet(t, admin, slug).SecretSet {
			t.Fatal("secretSet is true after clearing the secret")
		}
		admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{"secret": "s4"})
		v := catalogGet(t, admin, slug)
		r := admin.Do(t, "PUT", "/api/servers/"+slug, catalogPutBody(v, func(b map[string]any) { b["auth"] = map[string]any{"kind": "none"} }))
		var after catalogView
		if r.Status != 200 {
			t.Fatalf("PUT to auth none: %d %s", r.Status, r.Body)
		}
		r.JSON(t, "ServerView", &after)
		if after.SecretSet || catalogGet(t, admin, slug).SecretSet {
			t.Fatal("the secret survived a change of the auth kind to none")
		}
		var n int
		if err := s.DB.QueryRow(t.Context(), "select count(*) from catalog.servers where slug = $1 and secret is not null", slug).Scan(&n); err != nil || n != 0 {
			t.Fatalf("a secret is still stored after the change of the auth kind (rows %d, err %v)", n, err)
		}
		// Negative control: the same change of a field other than the kind keeps the secret.
		other := harness.Unique("sk")
		catalogRegister(t, admin, fakes.ServerInput(up, other, "shared_secret", fakes.WithSecret("keep")))
		ov := catalogGet(t, admin, other)
		if r := admin.Do(t, "PUT", "/api/servers/"+other, catalogPutBody(ov, func(b map[string]any) { b["name"] = "Renamed" })); r.Status != 200 || !catalogGet(t, admin, other).SecretSet {
			t.Fatalf("a rename dropped the secret: %d", r.Status)
		}
		catalogExpectError(t, "PUT secret of an unknown server", admin.Do(t, "PUT", "/api/servers/nope-x/secret", map[string]any{"secret": "x"}), 404, `not_found|server "nope-x" not found`)
	})

	t.Run("every editable field round-trips through the API", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
		slug := harness.Unique("rt")
		body := fakes.ServerInput(up, slug, "oauth_user", catalogOAuth(func(o map[string]any) {
			o["scopes"] = []string{"read", "write"}
			o["revokeUrl"] = "https://git.example.com/revoke"
			o["extraAuthorizeParams"] = map[string]string{"access_type": "offline"}
		}), fakes.WithGroups("dev", "ops"), fakes.WithTimeoutMs(30_000), fakes.WithDailyLimit(10), fakes.WithName("Gitea"), catalogWithDescription("Git hosting"))
		got := catalogRegister(t, admin, body)
		wantAuth := map[string]any{
			"kind": "oauth_user", "header": "Authorization", "template": "Bearer {secret}",
			"oauth": map[string]any{ //nolint:gosec // G101: made-up OAuth endpoints of a test server, not credentials
				"authorizeUrl": "https://git.example.com/authorize", "tokenUrl": "https://git.example.com/token",
				"revokeUrl": "https://git.example.com/revoke", "clientId": "cid", "scopes": []any{"read", "write"},
				"extraAuthorizeParams": map[string]any{"access_type": "offline"},
			},
		}
		check := func(v catalogView) {
			t.Helper()
			if v.Name != "Gitea" || v.Description != "Git hosting" || v.URL != up.URL || v.TimeoutMs != 30_000 || v.DailyLimit == nil || *v.DailyLimit != 10 || v.Status != "active" || v.SecretSet {
				t.Fatalf("view: %+v", v)
			}
			if !reflect.DeepEqual(v.Auth, wantAuth) || !reflect.DeepEqual(v.Access, map[string]any{"kind": "groups", "groups": []any{"dev", "ops"}}) {
				t.Fatalf("auth %v access %v", v.Auth, v.Access)
			}
		}
		check(got)
		check(catalogGet(t, admin, slug))
		// Defaults: the timeout is 120000 ms and there is no daily limit.
		plain := catalogRegister(t, admin, fakes.ServerInput(up, harness.Unique("df"), "none"))
		if plain.TimeoutMs != 120_000 || plain.DailyLimit != nil {
			t.Fatalf("defaults: timeoutMs %d dailyLimit %v", plain.TimeoutMs, plain.DailyLimit)
		}
		// A taken slug is refused with 409 and changes nothing.
		catalogExpectError(t, "duplicate slug", admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, slug, "none")), 409, fmt.Sprintf(`server_exists|server %q already exists`, slug))
		check(catalogGet(t, admin, slug))
	})

	t.Run("an update changes access, keeps the sync state and is audited", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
		slug := harness.Unique("upd")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		up.FailWith(500)
		catalogExpectError(t, "failing sync", admin.Do(t, "POST", "/api/servers/"+slug+"/sync", nil), 502, "sync_failed|"+catalogMsgUnreachable)
		before := catalogGet(t, admin, slug)
		r := admin.Do(t, "PUT", "/api/servers/"+slug, catalogPutBody(before, func(b map[string]any) {
			b["access"] = map[string]any{"kind": "groups", "groups": []string{"devs"}}
		}))
		if r.Status != 200 {
			t.Fatalf("PUT: %d %s", r.Status, r.Body)
		}
		var v catalogView
		r.JSON(t, "ServerView", &v)
		if !reflect.DeepEqual(v.Access, map[string]any{"kind": "groups", "groups": []any{"devs"}}) {
			t.Fatalf("access %v", v.Access)
		}
		if v.LastSyncError == nil || *v.LastSyncError != catalogMsgUnreachable || v.LastSyncAt == nil || *v.LastSyncAt != *before.LastSyncAt || v.ToolCount != before.ToolCount {
			t.Fatalf("an update touched the sync state: %+v vs %+v", v, before)
		}
		entries := auditWhere(auditAll(t, admin), "server.update", func(e auditEntry) bool { return ptrIs(e.Target, slug) })
		if len(entries) != 1 || !ptrIs(entries[0].ActorID, adminID) {
			t.Fatalf("server.update audit records: %+v", entries)
		}
		// The record lists every field the request carried, not only those whose value changed.
		wantFields := []any{"name", "description", "url", "access", "timeoutMs", "dailyLimit", "auth"}
		if fields, _ := entries[0].Meta["fields"].([]any); !reflect.DeepEqual(fields, wantFields) {
			t.Fatalf("audit meta fields %v, want %v", entries[0].Meta["fields"], wantFields)
		}
		catalogExpectError(t, "PUT of an unknown server", admin.Do(t, "PUT", "/api/servers/nope-x", catalogPutBody(before, nil)), 404, `not_found|server "nope-x" not found`)
	})

	t.Run("status disabled hides the server; sync of a disabled one is not found", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
		slug := harness.Unique("dis")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		member := s.Member(t, harness.Unique("m"))
		for who, c := range map[string]*harness.Client{"member": member, "admin": admin} {
			if !catalogHas(catalogCatalog(t, c), slug) {
				t.Fatalf("an active server is missing from the %s's catalog", who)
			}
		}
		if r := admin.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]any{"status": "disabled"}); r.Status != 204 || len(r.Body) != 0 {
			t.Fatalf("disable: %d %s", r.Status, r.Body)
		}
		for who, c := range map[string]*harness.Client{"member": member, "admin": admin} {
			if catalogHas(catalogCatalog(t, c), slug) {
				t.Errorf("a disabled server is in the %s's catalog", who)
			}
		}
		catalogExpectError(t, "sync of a disabled server", admin.Do(t, "POST", "/api/servers/"+slug+"/sync", nil), 404, fmt.Sprintf("not_found|active server %q not found", slug))
		if v := catalogGet(t, admin, slug); v.Status != "disabled" {
			t.Fatalf("admin view status %q", v.Status)
		}
		if r := admin.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]any{"status": "active"}); r.Status != 204 {
			t.Fatalf("enable: %d %s", r.Status, r.Body)
		}
		if !catalogHas(catalogCatalog(t, member), slug) || catalogSyncOK(t, admin, slug) != (catalogSyncResult{}) {
			t.Fatal("a re-enabled server is not back in the catalog or does not sync")
		}
	})

	t.Run("the catalog shows a member the servers open to their groups and the visible tools", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("list_repos", "delete_repo")})
		open, grp, closed := harness.Unique("co"), harness.Unique("cg"), harness.Unique("cx")
		catalogRegister(t, admin, fakes.ServerInput(up, open, "none"))
		catalogRegister(t, admin, fakes.ServerInput(up, grp, "none", fakes.WithGroups("mcp-users")))
		catalogRegister(t, admin, fakes.ServerInput(up, closed, "none", fakes.WithGroups("ops")))
		catalogHide(t, admin, open, "delete_repo", true)
		member := s.Member(t, harness.Unique("m"))
		if g := whoami(t, member).Groups; !slices.Equal(g, []string{"mcp-users"}) {
			t.Fatalf("a member created by the CLI has groups %v, want [mcp-users]", g)
		}
		r := member.Do(t, "GET", "/api/catalog", nil)
		var seen []catalogEntry
		r.JSON(t, "CatalogResponse", &seen)
		if !catalogHas(seen, open) || !catalogHas(seen, grp) || catalogHas(seen, closed) {
			t.Fatalf("the member's catalog: open %v group %v closed %v, want true true false", catalogHas(seen, open), catalogHas(seen, grp), catalogHas(seen, closed))
		}
		if got := catalogRefs(seen, open); !slices.Equal(got, []string{open + ".list_repos"}) {
			t.Fatalf("visible tools of %s: %v (a hidden tool is not listed)", open, got)
		}
		if strings.Contains(string(r.Body), up.URL) {
			t.Fatal("the catalog leaks the upstream URL")
		}
		adminSeen := catalogCatalog(t, admin)
		if !catalogHas(adminSeen, closed) || !catalogHas(adminSeen, grp) || !catalogHas(adminSeen, open) {
			t.Fatal("an admin must see every active server")
		}
		if got := catalogRefs(adminSeen, open); !slices.Equal(got, []string{open + ".list_repos"}) {
			t.Fatalf("the admin's catalog lists hidden tools: %v", got)
		}
		// The admin's own list still shows the hidden tool, flagged.
		if _, tools := catalogTools(t, admin, open); !slices.ContainsFunc(tools, func(tl catalogAdminTool) bool { return tl.Name == "delete_repo" && tl.Hidden }) {
			t.Fatal("the admin tool list does not show the hidden tool")
		}
		catalogExpectError(t, "hide an unknown tool", admin.Do(t, "PUT", "/api/servers/"+open+"/tools/nope/hidden", map[string]bool{"hidden": true}), 404, fmt.Sprintf(`not_found|tool "%s.nope" not found`, open))
		catalogExpectError(t, "hide a tool of an unknown server", admin.Do(t, "PUT", "/api/servers/nope-x/tools/a/hidden", map[string]bool{"hidden": true}), 404, `not_found|tool "nope-x.a" not found`)
		catalogExpectError(t, "GET an unknown server", admin.Do(t, "GET", "/api/servers/nope-x", nil), 404, "not_found|server not found")
	})

	t.Run("admin routes refuse members before the body is read", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("list_repos")})
		slug := harness.Unique("adm")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		member := s.Member(t, harness.Unique("m"))
		routes := []struct{ method, path string }{
			{"GET", "/api/servers"},
			{"POST", "/api/servers"},
			{"GET", "/api/servers/" + slug},
			{"PUT", "/api/servers/" + slug},
			{"PUT", "/api/servers/" + slug + "/secret"},
			{"POST", "/api/servers/" + slug + "/status"},
			{"POST", "/api/servers/" + slug + "/sync"},
			{"GET", "/api/servers/" + slug + "/tools"},
			{"PUT", "/api/servers/" + slug + "/tools/list_repos/hidden"},
			{"GET", "/api/calls"},
			{"GET", "/api/audit"},
		}
		for _, rt := range routes {
			// A body that is not JSON: an admin would get 422, a member must get 403 first.
			catalogExpectError(t, "member "+rt.method+" "+rt.path, member.Do(t, rt.method, rt.path, "{bad"), 403, catalogAdminErr)
		}
		// Negative controls: the admin reaches the body, and a member may read the catalog.
		catalogExpectError(t, "admin POST with a broken body", admin.Do(t, "POST", "/api/servers", "{bad"), 422, "invalid_request|body is not valid JSON")
		if r := member.Do(t, "GET", "/api/catalog", nil); r.Status != 200 {
			t.Fatalf("member GET /api/catalog: %d", r.Status)
		}
		// No session: 401 on every route that needs one.
		anon := s.Anonymous()
		for _, path := range []string{"/api/servers", "/api/catalog", "/api/calls", "/api/audit"} {
			catalogExpectError(t, "anonymous GET "+path, anon.Do(t, "GET", path, nil), 401, identityUnauthenticated)
		}
		// A bearer token never opens the web API, even next to a valid session cookie.
		catalogExpectError(t, "Bearer next to a cookie", admin.Do(t, "GET", "/api/servers", nil, "Authorization: Bearer sk_abcdef"), 401, "unauthenticated|tokens do not work on the web API")
		// A mutation from another origin is refused.
		catalogExpectError(t, "POST with a foreign Origin", admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, harness.Unique("csrf"), "none"), "Origin: http://evil.test"), 403, catalogCSRF)
		catalogExpectError(t, "POST without Origin", admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, harness.Unique("csrf"), "none"), "Origin:"), 403, catalogCSRF)
	})
}

// ---- optimistic concurrency ----

func TestCatalogOptimisticConcurrency(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})

	version := func(t *testing.T, slug string) int {
		t.Helper()
		var v int
		if err := s.DB.QueryRow(t.Context(), "select version from catalog.servers where slug = $1", slug).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	description := func(t *testing.T, slug string) string {
		t.Helper()
		var d string
		if err := s.DB.QueryRow(t.Context(), "select description from catalog.servers where slug = $1", slug).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	secretBytes := func(t *testing.T, slug string) []byte {
		t.Helper()
		var b []byte
		if err := s.DB.QueryRow(t.Context(), "select secret from catalog.servers where slug = $1", slug).Scan(&b); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		return b
	}

	writes := []struct {
		name string
		send func(slug string, v catalogView) harness.Response
	}{
		{"PUT /api/servers/:slug", func(slug string, v catalogView) harness.Response {
			return admin.Do(t, "PUT", "/api/servers/"+slug, catalogPutBody(v, func(b map[string]any) { b["description"] = "mine" }))
		}},
		{"PUT /api/servers/:slug/secret", func(slug string, _ catalogView) harness.Response {
			return admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{"secret": "for-the-old-state"})
		}},
		{"POST /api/servers/:slug/status", func(slug string, _ catalogView) harness.Response {
			return admin.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]any{"status": "disabled"})
		}},
	}
	for _, w := range writes {
		t.Run(w.name+" answers 409 server_conflict when another change commits meanwhile", func(t *testing.T) {
			slug := harness.Unique("race")
			v := catalogRegister(t, admin, fakes.ServerInput(up, slug, "shared_secret", fakes.WithSecret("orig"), catalogWithDescription("original")))
			secretBefore := secretBytes(t, slug)
			versionBefore := version(t, slug)

			r := catalogRaceWrite(t, s, slug, func() harness.Response { return w.send(slug, v) })
			catalogExpectError(t, w.name+" racing a change", r, 409, fmt.Sprintf("server_conflict|server %q was changed meanwhile; reload it and try again", slug))
			// Nothing of the losing request was written; the other admin's change stands.
			if got := description(t, slug); got != "theirs" {
				t.Fatalf("description %q: the refused request wrote, or the other change was lost", got)
			}
			if got := version(t, slug); got != versionBefore+1 {
				t.Fatalf("version %d, want %d (only the other change counts)", got, versionBefore+1)
			}
			if !bytes.Equal(secretBytes(t, slug), secretBefore) {
				t.Fatal("the secret changed although the request was refused")
			}
			if got := catalogGet(t, admin, slug); got.Status != "active" {
				t.Fatalf("status %q after a refused change", got.Status)
			}

			// Control: with no change in between the same request goes through and bumps the version.
			fresh := catalogGet(t, admin, slug)
			if r := w.send(slug, fresh); r.Status != 200 && r.Status != 204 {
				t.Fatalf("the same request without a race: %d %s", r.Status, r.Body)
			}
			if got := version(t, slug); got != versionBefore+2 {
				t.Fatalf("version %d after a clean write, want %d", got, versionBefore+2)
			}
		})
	}

	t.Run("the version is not part of the API", func(t *testing.T) {
		slug := harness.Unique("ver")
		v := catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		// ServerView is validated against a strict schema: a `version` field would have failed it.
		body := catalogPutBody(v, func(b map[string]any) { b["version"] = 7 })
		if r := admin.Do(t, "PUT", "/api/servers/"+slug, body); r.Status != 200 {
			t.Fatalf("PUT with an unknown version field: %d %s", r.Status, r.Body)
		}
		if got := version(t, slug); got == 7 {
			t.Fatal("a version sent by the client was stored")
		}
	})
}

// ---- validation goldens ----

// catalogGoldenCase records the status and body of a refused request under
// compat/golden/validation/<name>.json.
func catalogGoldenCase(t *testing.T, name string, r harness.Response, wantStatus int) {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("%s: status %d, want %d (body %.300s)", name, r.Status, wantStatus, r.Body)
	}
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	r.JSON(t, "ErrorBody", &e)
	doc, err := json.MarshalIndent(struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}{r.Status, r.Body}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	harness.Golden(t, "validation/"+name+".json", append(doc, '\n'))
}

func TestCatalogValidation(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
	listLen := func() int {
		var list []catalogView
		admin.Do(t, "GET", "/api/servers", nil).JSON(t, "ServerList", &list)
		return len(list)
	}
	long := func(n int) string { return strings.Repeat("x", n) }
	groups := func(g ...string) fakes.ServerOpt {
		if g == nil {
			g = []string{}
		}
		return fakes.WithGroups(g...)
	}
	extra := func(key string) fakes.ServerOpt {
		return catalogOAuth(func(o map[string]any) { o["extraAuthorizeParams"] = map[string]string{key: "x"} })
	}

	// Every case is a create request that the web form can produce; the golden holds the exact
	// status and body the form's field mapping (formModel.ts serverErrorField) works from.
	creates := []struct {
		name string
		kind string
		opts []fakes.ServerOpt
		want int
	}{
		{"slug_uppercase_and_space", "none", []fakes.ServerOpt{catalogSet("slug", "Bad Slug")}, 422},
		{"slug_empty", "none", []fakes.ServerOpt{catalogSet("slug", "")}, 422},
		{"slug_too_short", "none", []fakes.ServerOpt{catalogSet("slug", "a")}, 422},
		{"slug_starts_with_digit", "none", []fakes.ServerOpt{catalogSet("slug", "1abc")}, 422},
		{"slug_33_characters", "none", []fakes.ServerOpt{catalogSet("slug", "a"+long(32))}, 422},
		{"slug_too_long", "none", []fakes.ServerOpt{catalogSet("slug", "a"+long(64))}, 422},
		{"slug_underscore", "none", []fakes.ServerOpt{catalogSet("slug", "my_server")}, 422},
		{"name_empty", "none", []fakes.ServerOpt{fakes.WithName("")}, 422},
		{"name_too_long", "none", []fakes.ServerOpt{fakes.WithName(long(101))}, 422},
		{"description_too_long", "none", []fakes.ServerOpt{catalogWithDescription(long(501))}, 422},
		{"url_ftp", "none", []fakes.ServerOpt{fakes.WithURL("ftp://example.com/mcp")}, 422},
		{"url_not_a_url", "none", []fakes.ServerOpt{fakes.WithURL("not a url")}, 422},
		{"url_empty", "none", []fakes.ServerOpt{fakes.WithURL("")}, 422},
		{"url_too_long", "none", []fakes.ServerOpt{fakes.WithURL("http://example.com/" + long(2048))}, 422},
		{"auth_unknown_kind", "shared_secret", []fakes.ServerOpt{catalogAuthSet("kind", "magic")}, 422},
		{"auth_missing", "none", []fakes.ServerOpt{catalogSet("auth", nil)}, 422},
		{"oauth_without_oauth_block", "shared_secret", []fakes.ServerOpt{catalogAuthSet("kind", "oauth_user")}, 422},
		{"oauth_client_id_empty", "oauth_user", []fakes.ServerOpt{catalogOAuth(func(o map[string]any) { o["clientId"] = "" })}, 422},
		{"oauth_client_id_blank", "oauth_user", []fakes.ServerOpt{catalogOAuth(func(o map[string]any) { o["clientId"] = "   " })}, 422},
		{"oauth_authorize_url_ftp", "oauth_user", []fakes.ServerOpt{catalogOAuth(func(o map[string]any) { o["authorizeUrl"] = "ftp://x.example/a" })}, 422},
		{"oauth_token_url_not_a_url", "oauth_user", []fakes.ServerOpt{catalogOAuth(func(o map[string]any) { o["tokenUrl"] = "nope" })}, 422},
		{"oauth_revoke_url_bad", "oauth_user", []fakes.ServerOpt{catalogOAuth(func(o map[string]any) { o["revokeUrl"] = "nope" })}, 422},
		{"oauth_scope_openid", "oauth_user", []fakes.ServerOpt{catalogOAuth(func(o map[string]any) { o["scopes"] = []string{"read", "openid"} })}, 422},
		{"oauth_scope_openid_uppercase", "oauth_user", []fakes.ServerOpt{catalogOAuth(func(o map[string]any) { o["scopes"] = []string{"OpenID"} })}, 422},
		{"oauth_scope_empty_string", "oauth_user", []fakes.ServerOpt{catalogOAuth(func(o map[string]any) { o["scopes"] = []string{""} })}, 422},
		{"oauth_extra_param_state", "oauth_user", []fakes.ServerOpt{extra("state")}, 422},
		{"oauth_extra_param_redirect_uri", "oauth_user", []fakes.ServerOpt{extra("redirect_uri")}, 422},
		{"oauth_extra_param_scope", "oauth_user", []fakes.ServerOpt{extra("scope")}, 422},
		{"oauth_extra_param_client_id", "oauth_user", []fakes.ServerOpt{extra("client_id")}, 422},
		{"oauth_extra_param_response_type", "oauth_user", []fakes.ServerOpt{extra("response_type")}, 422},
		{"oauth_extra_param_code_challenge", "oauth_user", []fakes.ServerOpt{extra("code_challenge")}, 422},
		{"oauth_extra_param_code_challenge_method", "oauth_user", []fakes.ServerOpt{extra("code_challenge_method")}, 422},
		{"oauth_extra_param_bad_name", "oauth_user", []fakes.ServerOpt{extra("a b")}, 422},
		{"access_groups_empty", "none", []fakes.ServerOpt{groups()}, 422},
		{"access_groups_blank_name", "none", []fakes.ServerOpt{groups("")}, 422},
		{"access_unknown_kind", "none", []fakes.ServerOpt{catalogSet("access", map[string]any{"kind": "some"})}, 422},
		{"timeout_below_minimum", "none", []fakes.ServerOpt{fakes.WithTimeoutMs(999)}, 422},
		{"timeout_above_maximum", "none", []fakes.ServerOpt{fakes.WithTimeoutMs(600_001)}, 422},
		{"timeout_not_an_integer", "none", []fakes.ServerOpt{catalogSet("timeoutMs", 1500.5)}, 422},
		{"timeout_not_a_number", "none", []fakes.ServerOpt{catalogSet("timeoutMs", "abc")}, 422},
		{"daily_limit_negative", "none", []fakes.ServerOpt{fakes.WithDailyLimit(-1)}, 422},
		{"daily_limit_zero", "none", []fakes.ServerOpt{fakes.WithDailyLimit(0)}, 422},
		{"daily_limit_above_maximum", "none", []fakes.ServerOpt{fakes.WithDailyLimit(100_001)}, 422},
		{"daily_limit_not_a_number", "none", []fakes.ServerOpt{catalogSet("dailyLimit", "ten")}, 422},
		{"header_empty", "shared_secret", []fakes.ServerOpt{catalogAuthSet("header", "")}, 422},
		{"header_with_space", "shared_secret", []fakes.ServerOpt{catalogAuthSet("header", "X Key")}, 422},
		{"header_with_colon", "shared_secret", []fakes.ServerOpt{catalogAuthSet("header", "X:Key")}, 422},
		{"header_too_long", "shared_secret", []fakes.ServerOpt{catalogAuthSet("header", long(101))}, 422},
		{"template_empty", "shared_secret", []fakes.ServerOpt{catalogAuthSet("template", "")}, 422},
		{"template_without_secret", "shared_secret", []fakes.ServerOpt{catalogAuthSet("template", "Bearer token")}, 422},
		{"template_with_newline", "shared_secret", []fakes.ServerOpt{catalogAuthSet("template", "Bearer\n{secret}")}, 422},
		{"template_pat_user_without_secret", "pat_user", []fakes.ServerOpt{catalogAuthSet("template", "Bearer")}, 422},
		{"template_oauth_user_without_secret", "oauth_user", []fakes.ServerOpt{catalogOAuth(nil), catalogAuthSet("template", "Bearer")}, 422},
		{"secret_empty", "shared_secret", []fakes.ServerOpt{catalogSet("secret", "")}, 422},
		{"secret_too_long", "shared_secret", []fakes.ServerOpt{catalogSet("secret", long(4097))}, 422},
		{"secret_not_a_string", "shared_secret", []fakes.ServerOpt{catalogSet("secret", 5)}, 422},
	}
	before := listLen()
	for _, c := range creates {
		t.Run("create_"+c.name, func(t *testing.T) {
			body := fakes.ServerInput(up, harness.Unique("val"), c.kind, c.opts...)
			catalogGoldenCase(t, "create_"+c.name, admin.Do(t, "POST", "/api/servers", body), c.want)
		})
	}
	// Bodies that are not a server at all.
	for name, body := range map[string]string{
		"body_not_json":        "{bad",
		"body_empty_object":    `{}`,
		"body_missing_name":    `{"slug":"abc"}`,
		"body_array":           `[]`,
		"body_null":            `null`,
		"body_unknown_auth":    `{"slug":"abc","name":"x","description":"","url":"http://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"timeoutMs":"x"}`,
		"body_access_missing":  `{"slug":"abc","name":"x","description":"","url":"http://x.example","auth":{"kind":"none"}}`,
		"body_description_nil": `{"slug":"abc","name":"x","url":"http://x.example","auth":{"kind":"none"},"access":{"kind":"all"}}`,
	} {
		t.Run("create_"+name, func(t *testing.T) {
			catalogGoldenCase(t, "create_"+name, admin.Do(t, "POST", "/api/servers", body), 422)
		})
	}
	if after := listLen(); after != before {
		t.Fatalf("refused requests created %d server(s)", after-before)
	}

	t.Run("a taken slug is a 409, not a 422", func(t *testing.T) {
		slug := harness.Unique("taken")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		r := admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, slug, "none"))
		catalogExpectError(t, "second POST", r, 409, fmt.Sprintf("server_exists|server %q already exists", slug))
	})

	// The edit form sends the same fields (without slug and secret) to PUT.
	slug := harness.Unique("edit")
	existing := catalogRegister(t, admin, fakes.ServerInput(up, slug, "shared_secret", fakes.WithSecret("keep")))
	updates := []struct {
		name string
		mut  func(map[string]any)
	}{
		{"name_empty", func(b map[string]any) { b["name"] = "" }},
		{"url_ftp", func(b map[string]any) { b["url"] = "ftp://example.com" }},
		{"auth_unknown_kind", func(b map[string]any) { b["auth"] = map[string]any{"kind": "magic"} }},
		{"template_without_secret", func(b map[string]any) {
			b["auth"] = map[string]any{"kind": "shared_secret", "header": "Authorization", "template": "Bearer"}
		}},
		{"oauth_scope_openid", func(b map[string]any) {
			b["auth"] = map[string]any{"kind": "oauth_user", "header": "Authorization", "template": "Bearer {secret}", "oauth": map[string]any{ //nolint:gosec // G101: made-up OAuth endpoints of a test server, not credentials
				"authorizeUrl": "https://a.example/a", "tokenUrl": "https://a.example/t", "clientId": "c", "scopes": []string{"openid"},
			}}
		}},
		{"access_groups_empty", func(b map[string]any) { b["access"] = map[string]any{"kind": "groups", "groups": []string{}} }},
		{"timeout_below_minimum", func(b map[string]any) { b["timeoutMs"] = 10 }},
		{"daily_limit_zero", func(b map[string]any) { b["dailyLimit"] = 0 }},
	}
	for _, c := range updates {
		t.Run("update_"+c.name, func(t *testing.T) {
			r := admin.Do(t, "PUT", "/api/servers/"+slug, catalogPutBody(existing, c.mut))
			catalogGoldenCase(t, "update_"+c.name, r, 422)
			if got := catalogGet(t, admin, slug); !reflect.DeepEqual(got, existing) {
				t.Fatalf("a refused update changed the server: %+v vs %+v", got, existing)
			}
		})
	}
	t.Run("update_body_not_json", func(t *testing.T) {
		catalogGoldenCase(t, "update_body_not_json", admin.Do(t, "PUT", "/api/servers/"+slug, "{bad"), 422)
	})

	// The other server forms: secret, status, hidden.
	t.Run("secret_empty_string", func(t *testing.T) {
		catalogGoldenCase(t, "put_secret_empty_string", admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{"secret": ""}), 422)
	})
	t.Run("secret_missing", func(t *testing.T) {
		catalogGoldenCase(t, "put_secret_missing", admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{}), 422)
	})
	t.Run("status_unknown", func(t *testing.T) {
		catalogGoldenCase(t, "status_unknown_value", admin.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]any{"status": "bogus"}), 422)
	})
	t.Run("hidden_not_a_boolean", func(t *testing.T) {
		catalogGoldenCase(t, "hidden_not_a_boolean", admin.Do(t, "PUT", "/api/servers/"+slug+"/tools/a/hidden", map[string]any{"hidden": "yes"}), 422)
	})
	t.Run("hidden_missing", func(t *testing.T) {
		catalogGoldenCase(t, "hidden_missing", admin.Do(t, "PUT", "/api/servers/"+slug+"/tools/a/hidden", map[string]any{}), 422)
	})

	// Negative control: a valid body for every variant is accepted, so the goldens above come
	// from the one broken field and not from a broken base body.
	t.Run("the base bodies are valid", func(t *testing.T) {
		for _, kind := range []string{"none", "shared_secret", "pat_user"} {
			catalogRegister(t, admin, fakes.ServerInput(up, harness.Unique("ok"), kind))
		}
		catalogRegister(t, admin, fakes.ServerInput(up, harness.Unique("ok"), "oauth_user", catalogOAuth(nil)))
		if r := admin.Do(t, "PUT", "/api/servers/"+slug, catalogPutBody(existing, nil)); r.Status != 200 {
			t.Fatalf("PUT of the unchanged server: %d %s", r.Status, r.Body)
		}
	})
}

// ---- the upstream client seen through call_tool ----

func TestCatalogUpstreamCalls(t *testing.T) {
	const secret = "k-live-7f3a9c" //nolint:gosec // G101: a made-up secret that the test hunts for
	s := catalogStackSmallLimit(t)
	admin := s.Admin(t)
	c := s.MCP(t, admin.Token(t, "catalog-calls"))
	other := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("z")})
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	slug := harness.Unique("up")
	catalogRegister(t, admin, fakes.ServerInput(up, slug, "shared_secret", fakes.WithHeader("X-API-Key", "key {secret}"), fakes.WithSecret(secret), fakes.WithTimeoutMs(1000)))
	ref := slug + ".echo"
	outcomes := func() []string {
		r := admin.Do(t, "GET", "/api/calls?server="+slug, nil)
		var list []struct{ Outcome string }
		r.JSON(t, "CallList", &list)
		out := make([]string, 0, len(list))
		for _, e := range list {
			out = append(out, e.Outcome)
		}
		slices.Reverse(out) // oldest first
		return out
	}

	t.Run("the credential header is sent on every request of a call", func(t *testing.T) {
		up.Reset()
		res := catalogCallMCP(t, c, "call_tool", map[string]any{"tool": ref, "arguments": map[string]any{"text": "hi"}})
		if res.IsError || catalogText(res) != `{"text":"hi"}` {
			t.Fatalf("call: isError %v, text %q", res.IsError, catalogText(res))
		}
		sc, _ := res.StructuredContent.(map[string]any)
		if !reflect.DeepEqual(sc, map[string]any{"echo": map[string]any{"text": "hi"}}) {
			t.Fatalf("structuredContent %v: the upstream's result must come back unchanged", res.StructuredContent)
		}
		calls := up.Calls()
		if len(calls) != 1 || calls[0].Tool != "echo" || string(calls[0].Arguments) != `{"text":"hi"}` {
			t.Fatalf("the upstream saw %+v", calls)
		}
		methods := map[string]bool{}
		for _, req := range up.Requests() {
			methods[req.Method] = true
			if got := req.Header.Get("X-API-Key"); got != "key "+secret {
				t.Errorf("a %q request carried X-API-Key %q", req.Method, got)
			}
		}
		if !methods["tools/call"] {
			t.Fatalf("methods seen: %v", methods)
		}
		if !slices.Equal(outcomes(), []string{"ok"}) {
			t.Fatalf("call log: %v", outcomes())
		}
	})

	t.Run("a new secret replaces the header of the next call", func(t *testing.T) {
		up.Reset()
		if r := admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{"secret": "k-second"}); r.Status != 204 {
			t.Fatalf("PUT secret: %d", r.Status)
		}
		catalogCallMCP(t, c, "call_tool", map[string]any{"tool": ref})
		reqs := up.Requests()
		if len(reqs) == 0 {
			t.Fatal("the upstream saw nothing")
		}
		for _, req := range reqs {
			if got := req.Header.Get("X-API-Key"); got != "key k-second" {
				t.Errorf("a %q request carried X-API-Key %q after the secret changed", req.Method, got)
			}
		}
		if r := admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{"secret": secret}); r.Status != 204 {
			t.Fatalf("PUT secret: %d", r.Status)
		}
	})

	failures := []struct {
		name    string
		arm     func()
		text    string
		outcome string
		within  time.Duration
	}{
		{"HTTP 401", func() { up.FailWith(401) }, slug + " rejected the gateway credentials. Ask the administrator to check the server key.", "transport_error", 0},
		{"HTTP 500", func() { up.FailWith(500) }, slug + " is unavailable.", "transport_error", 0},
		{"slower than timeoutMs", func() { up.Delay(5 * time.Second) }, slug + " did not respond in 1 s.", "timeout", 4 * time.Second},
		{"a Content-Length above the limit", func() { up.RespondBytes(100_000) }, slug + " returned a response larger than 0.06 MiB.", "transport_error", 0},
		{"a streamed answer above the limit", func() { up.RespondChunked(100_000) }, slug + " returned a response larger than 0.06 MiB.", "transport_error", 0},
		{"a JSON-RPC error", func() { up.RejectCalls(-32602, "Invalid params: owner is required") }, slug + " rejected the call (JSON-RPC -32602): Invalid params: owner is required", "upstream_error", 0},
		{"a redirect is not followed", func() { up.RedirectTo(other.URL) }, slug + " is unavailable.", "transport_error", 0},
	}
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			up.Reset()
			other.Reset()
			f.arm()
			started := time.Now()
			res := catalogCallMCP(t, c, "call_tool", map[string]any{"tool": ref})
			if !res.IsError || catalogText(res) != f.text {
				t.Fatalf("isError %v, text %q, want an error with %q", res.IsError, catalogText(res), f.text)
			}
			if f.within > 0 && time.Since(started) > f.within {
				t.Fatalf("the call took %s, timeoutMs is 1000", time.Since(started))
			}
			if got := outcomes(); got[len(got)-1] != f.outcome {
				t.Fatalf("call log outcome %q, want %q (all: %v)", got[len(got)-1], f.outcome, got)
			}
			if n := len(other.Requests()); n != 0 {
				t.Fatalf("the redirect target received %d request(s)", n)
			}
			// Negative control: the same server answers once the failure is gone.
			up.Reset()
			if res := catalogCallMCP(t, c, "call_tool", map[string]any{"tool": ref}); res.IsError {
				t.Fatalf("the server does not recover: %s", catalogText(res))
			}
		})
	}

	t.Run("an answer within the limit passes unchanged", func(t *testing.T) {
		up.Reset()
		up.RespondBytes(30_000)
		res := catalogCallMCP(t, c, "call_tool", map[string]any{"tool": ref})
		text := catalogText(res)
		if res.IsError || len(text) < 29_900 || strings.Trim(text, "x") != "" {
			t.Fatalf("isError %v, %d chars of text (want ~30000 x)", res.IsError, len(text))
		}
		up.Reset()
	})

	t.Run("the secret is nowhere in the answers or the log", func(t *testing.T) {
		up.Reset()
		up.FailWith(401)
		text := catalogText(catalogCallMCP(t, c, "call_tool", map[string]any{"tool": ref}))
		up.Reset()
		if strings.Contains(text, secret) || strings.Contains(s.Logs(t), secret) || strings.Contains(s.Logs(t), "k-second") {
			t.Fatal("a secret leaked into an error text or the app log")
		}
	})
}

// ---- search ----

// catalogGoldenQuery is one recorded search.
type catalogGoldenQuery struct {
	Query   string    `json:"query"`
	Servers []string  `json:"servers"`
	Limit   int       `json:"limit"`
	Refs    []string  `json:"refs"`
	Scores  []float64 `json:"scores"`
}

type catalogGoldenFile struct {
	Arch    string               `json:"arch"`
	Model   string               `json:"model"`
	Queries []catalogGoldenQuery `json:"queries"`
}

func catalogUpdating() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

// catalogSearchQueries is the fixed list recorded in compat/golden/search-<arch>.json.
var catalogSearchQueries = []struct {
	query    string
	relevant string // the ref that must rank first, "" for none
	nothing  bool   // must return no tool at all
}{
	{"create an issue in the tracker", "youtrack.create_issue", false},
	{"list pull requests in a repository", "gitea.list_pull_requests", false},
	{"add a comment to an issue", "", false},
	{"find issues by a search query", "", false},
	{"create a new branch", "", false},
	{"get the contents of a file in a repository", "", false},
	{"создать задачу в трекере", "", false},
	{"найти задачу по номеру", "", false},
	{"показать список pull request", "", false},
	{"создать ветку в репозитории", "", false},
	{"issue", "", false},
	{"comment", "", false},
	{"search", "", false},
	{"weather in Berlin", "", true},
	{"book a flight to Tokyo", "", true},
	{"dollar exchange rate", "", true},
	{"borscht recipe", "", true},
	{"прогноз погоды на завтра", "", true},
}

func catalogGoldenPath(arch string) string { return filepath.Join("golden", "search-"+arch+".json") }

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
		if err := s.DB.QueryRow(t.Context(), "select count(*), count(embedding) from search.documents").Scan(&total, &embedded); err != nil {
			t.Fatal(err)
		}
		if total != 84 || embedded != 84 {
			t.Fatalf("search.documents: %d rows, %d embedded, want 84 one per visible tool", total, embedded)
		}
	})

	t.Run("the recorded queries", func(t *testing.T) {
		got := catalogGoldenFile{Arch: s.Arch, Model: "Xenova/multilingual-e5-small"}
		for _, q := range catalogSearchQueries {
			hits := catalogSearch(t, c, q.query, 10, both)
			rec := catalogGoldenQuery{Query: q.query, Servers: both, Limit: 10, Refs: catalogHitRefs(hits), Scores: make([]float64, 0, len(hits))}
			for _, h := range hits {
				rec.Scores = append(rec.Scores, h.Score)
			}
			got.Queries = append(got.Queries, rec)

			// Properties that do not depend on the golden.
			if len(hits) > 10 {
				t.Errorf("%q: %d hits, limit 10", q.query, len(hits))
			}
			for i, h := range hits {
				if h.Score <= 0 || h.Score > 1 {
					t.Errorf("%q: score %v of %s is outside (0, 1]", q.query, h.Score, h.Tool)
				}
				if i > 0 && h.Score > hits[i-1].Score {
					t.Errorf("%q: scores are not sorted descending at %d: %v", q.query, i, rec.Scores)
				}
				if h.Server != strings.SplitN(h.Tool, ".", 2)[0] || (h.Server != "gitea" && h.Server != "youtrack") {
					t.Errorf("%q: hit %s from server %q", q.query, h.Tool, h.Server)
				}
			}
			if q.nothing && len(hits) != 0 {
				t.Errorf("%q must find nothing, got %v", q.query, rec.Refs)
			}
			if q.relevant != "" && (len(hits) == 0 || hits[0].Tool != q.relevant) {
				t.Errorf("%q: first hit %v, want %s", q.query, rec.Refs, q.relevant)
			}
		}
		if t.Failed() {
			return
		}
		if len(got.Queries) < 12 {
			t.Fatalf("only %d queries", len(got.Queries))
		}
		doc, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(catalogGoldenPath(s.Arch))
		if catalogUpdating() {
			harness.Golden(t, name, append(doc, '\n')) // writes, and refuses without COMPAT_REFERENCE=1
			return
		}
		raw, err := os.ReadFile(catalogGoldenPath(s.Arch))
		if errors.Is(err, os.ErrNotExist) {
			t.Fatalf("there is no golden %s for the architecture %s of the image under test: record it with COMPAT_REFERENCE=1 and -update on a %s runner against the reference image", name, s.Arch, s.Arch)
		}
		if err != nil {
			t.Fatal(err)
		}
		var want catalogGoldenFile
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatalf("golden %s: %v", name, err)
		}
		if want.Arch != got.Arch || want.Model != got.Model || len(want.Queries) != len(got.Queries) {
			t.Fatalf("golden %s is for %s/%s with %d queries; this run has %s/%s with %d", name, want.Arch, want.Model, len(want.Queries), got.Arch, got.Model, len(got.Queries))
		}
		for i, w := range want.Queries {
			g := got.Queries[i]
			if w.Query != g.Query || w.Limit != g.Limit || !slices.Equal(w.Servers, g.Servers) {
				t.Fatalf("golden query %d is %q, the test asks %q", i, w.Query, g.Query)
			}
			if !slices.Equal(w.Refs, g.Refs) {
				t.Errorf("%q: refs\n got  %v\n want %v", w.Query, g.Refs, w.Refs)
				continue
			}
			for j := range w.Scores {
				if math.Abs(w.Scores[j]-g.Scores[j]) > 1e-6 {
					t.Errorf("%q: score of %s is %.9f, golden %.9f (tolerance 1e-6)", w.Query, w.Refs[j], g.Scores[j], w.Scores[j])
				}
			}
		}
	})

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
		if len(catalogSearch(t, c, "issue", 0, both)) > 10 {
			t.Fatal("the default limit is 10")
		}
		for _, bad := range []int{0, 26, -1} {
			res := catalogCallMCP(t, c, "search_tools", map[string]any{"query": "issue", "limit": bad})
			if !res.IsError || !strings.HasPrefix(catalogText(res), "Input validation error: Invalid arguments for tool search_tools: limit: ") {
				t.Errorf("limit %d: isError %v %q", bad, res.IsError, catalogText(res))
			}
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
		if !res.IsError || catalogText(res) != "Input validation error: Invalid arguments for tool search_tools: servers: Too small: expected array to have >=1 items" {
			t.Fatalf("empty filter: isError %v %q (the tool never returns [] for an empty list: its schema refuses it)", res.IsError, catalogText(res))
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
		var d struct {
			Tool        string          `json:"tool"`
			Title       *string         `json:"title"`
			Description string          `json:"description"`
			InputSchema map[string]any  `json:"inputSchema"`
			Output      map[string]any  `json:"outputSchema"`
			Annotations map[string]bool `json:"annotations"`
			Server      string          `json:"server"`
			Connection  string          `json:"connection"`
		}
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
		for _, name := range []string{"describe_tool", "call_tool"} {
			res := catalogCallMCP(t, c, name, map[string]any{"tool": hidden})
			if !res.IsError || catalogText(res) != catalogNotAvailable(hidden) {
				t.Fatalf("%s of a hidden tool: isError %v %q", name, res.IsError, catalogText(res))
			}
		}
		if n := len(up.Calls()); n != 0 {
			t.Fatalf("the upstream was called %d time(s) for a hidden tool", n)
		}
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{}) {
			t.Fatalf("sync with a hidden tool: %+v", res)
		}
		catalogWaitDocs(t, s, slug, 2)
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
		if res := catalogSyncOK(t, admin, slug); res.Removed != 1 {
			t.Fatalf("sync after the removal: %+v", res)
		}
		catalogWaitDocs(t, s, slug, 2)
		if got := find("drops the plumbus"); slices.Contains(got, gone) {
			t.Fatalf("a removed tool is found: %v", got)
		}
		if res := catalogCallMCP(t, c, "describe_tool", map[string]any{"tool": gone}); !res.IsError {
			t.Fatal("a removed tool can still be described")
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
		if res := catalogSyncOK(t, admin, slug); res.Changed != 1 {
			t.Fatalf("sync after the change: %+v", res)
		}
		harness.Eventually(t, "the changed description in search.documents", func() bool {
			var n int
			err := s.DB.QueryRow(t.Context(), "select count(*) from search.documents where ref = $1 and description = $2 and embedding is not null", slug+".static_item", "Reticulates the splines of a gizmo.").Scan(&n)
			return err == nil && n == 1
		})
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
		var d struct{ Tool, Server string }
		res := catalogCallMCP(t, c, "describe_tool", map[string]any{"tool": slug + ".issues.create"})
		if res.IsError {
			t.Fatalf("describe_tool of a dotted name: %s", catalogText(res))
		}
		raw, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(raw, &d)
		if d.Tool != slug+".issues.create" || d.Server != slug {
			t.Fatalf("describe_tool: %s", raw)
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
}

// ---- access to search results ----

func TestCatalogSearchAccess(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	admin := s.Admin(t)
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
			for _, name := range []string{"admin", "member"} {
				c := adminMCP
				if name == "member" {
					c = memberMCP
				}
				got := refsFor(c, q)
				if name == "member" && slices.Contains(got, closed.tool) {
					t.Errorf("the member finds %s with %q: %v", closed.tool, q, got)
				}
			}
		}
		adminGot := refsFor(adminMCP, "Calibrates the closed flux capacitor.")
		if !slices.Contains(adminGot, closed.tool) {
			t.Fatalf("control: the admin does not find the closed tool: %v", adminGot)
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

func slugOf(ref string) string {
	slug, _, _ := strings.Cut(ref, ".")
	return slug
}
