//go:build compat

package compat_test

// Catalog scenarios: server administration over REST, sync, the upstream client as seen from the
// outside, optimistic concurrency and the validation goldens. Search is in search_test.go; the
// helpers prefixed catalog here are shared by both files.
//
// One stack with a small UPSTREAM_MAX_RESPONSE_BYTES (the size limit is tested with 80 KB bodies
// instead of 20 MiB) serves TestCatalogServers, whose last subtest group ("upstream calls") is the
// upstream client seen through call_tool; TestCatalogOptimisticConcurrency and
// TestCatalogValidation use the shared stack. search_test.go starts one more stack (default limit,
// the Gitea fixture is 80 KB). Three stacks in all.
//
// Source cases that are not ported (they need the app's internals, not its outside):
//   - v1.0.1:apps/server/test/catalog/pg-catalog.int.test.ts "round-trips a server, including the sealed secret bytes" and
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
//   - v1.0.1:apps/server/test/upstream/mcp-upstream-client.int.test.ts "returns an image block byte for byte": the fake upstream
//     has no image mode and the pass-through of results is a gateway scenario.
//   - "a JSON-RPC internal error ... is rejected too": its tools/call half is ported (-32603 in
//     TestCatalogServers, subtest "upstream calls"); the listTools half is not, the fake can only reject tools/call.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// catalogDeliveryLimit bounds catalogWaitDelivered.
const catalogDeliveryLimit = 60 * time.Second

// catalogWaitDelivered waits until every event of the outbox has been delivered to its
// subscribers (platform.outbox.processed_at is set). A search assertion that follows a change
// which does not alter the number of documents (a changed description, a hide that is followed by
// a sync, an access change) cannot use the row count as its signal: without this wait it could
// run against the index as it was before the change, and a negative assertion would pass for the
// wrong reason. The events of the change are already written when its API call returns.
//
// The bound is a minute, not Eventually's 15 s: indexing a whole recorded catalog (84 tools, one
// embedding each) on a loaded host ran out of 15 s on either image and passed on a rerun. It is a
// wait bound, not an expectation: a stuck outbox is only reported later.
func catalogWaitDelivered(t *testing.T, s *harness.Stack) {
	t.Helper()
	harness.EventuallyWithin(t, "every outbox event to be delivered (platform.outbox.processed_at set)", catalogDeliveryLimit, func() bool {
		var n int
		err := s.DB.QueryRow(t.Context(), "select count(*) from platform.outbox where processed_at is null").Scan(&n)
		return err == nil && n == 0
	})
}

// catalogWaitDocs waits until the outbox is drained and search.documents holds exactly want rows
// for the server, every one of them with an embedding, so a search assertion never races the
// indexer or the embedder.
func catalogWaitDocs(t *testing.T, s *harness.Stack, server string, want int) {
	t.Helper()
	catalogWaitDelivered(t, s)
	harness.Eventually(t, fmt.Sprintf("%d embedded documents of %s in search.documents", want, server), func() bool {
		var total, embedded int
		err := s.DB.QueryRow(t.Context(), "select count(*), count(embedding) from search.documents where server = $1", server).Scan(&total, &embedded)
		return err == nil && total == want && embedded == want
	})
}

// catalogDescribed is the structured result of describe_tool.
type catalogDescribed struct {
	Tool         string          `json:"tool"`
	Title        *string         `json:"title"`
	Description  string          `json:"description"`
	InputSchema  map[string]any  `json:"inputSchema"`
	OutputSchema map[string]any  `json:"outputSchema"`
	Annotations  map[string]bool `json:"annotations"`
	Server       string          `json:"server"`
	Connection   string          `json:"connection"`
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
// admin's change commits in between. A transaction of the test holds the row lock; the request
// is held at its write, recognised by the fact that some backend is blocked by the test's
// transaction (pg_blocking_pids), not by any SQL text. Then the other change commits, bumping
// the version column the optimistic check compares (catalog.servers.version, see CLAUDE.md
// "Catalog and admin REST"), and the lock is released. It returns the request's answer.
func catalogRaceWrite(t *testing.T, s *harness.Stack, slug string, send func() harness.Response) harness.Response {
	t.Helper()
	ctx := t.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	var pid int
	if err := tx.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "select 1 from catalog.servers where slug = $1 for update", slug); err != nil {
		t.Fatal(err)
	}
	// The request runs in this goroutine (Client.Do may fail the test); the other admin's change
	// runs in a helper that only talks to the database and reports an error through the channel.
	done := make(chan error, 1)
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		deadline := time.After(15 * time.Second)
		for {
			var n int
			if err := s.DB.QueryRow(ctx, "select count(*) from pg_stat_activity where $1 = any(pg_blocking_pids(pid))", pid).Scan(&n); err != nil {
				_ = tx.Rollback(ctx)
				done <- err
				return
			}
			if n > 0 {
				break
			}
			select {
			case <-tick.C:
			case <-deadline:
				_ = tx.Rollback(ctx) // let the request finish so that the test can fail with a message
				done <- errors.New("no backend was ever blocked by the row lock: the request did not wait for it")
				return
			}
		}
		if _, err := tx.Exec(ctx, "update catalog.servers set version = version + 1, description = 'theirs' where slug = $1", slug); err != nil {
			_ = tx.Rollback(ctx)
			done <- err
			return
		}
		done <- tx.Commit(ctx)
	}()
	r := send()
	if err := <-done; err != nil {
		t.Fatalf("the competing change: %v", err)
	}
	return r
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
		entries := auditAll(t, s)
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
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{Added: 2}) {
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
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{Changed: 1, Removed: 1}) { // a also reverts to its first description
			t.Fatalf("a vanished tool: %+v", res)
		}
		_, tools := catalogTools(t, admin, slug)
		if got := catalogToolNames(tools); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("tools after the removal: %v", got)
		}
		up.SetTools(fakes.Tools("a", "drop"))
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{Added: 1}) {
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
		if res := catalogSyncOK(t, admin, slug); res != (catalogSyncResult{Removed: 2}) {
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

	// Reference 1.0.1: the stored secret is cleared when the place it is sent to changes, so an edit
	// cannot make the gateway send it to a new address: for shared_secret the upstream URL, for
	// oauth_user (the secret is the OAuth client secret) the token URL, the revoke URL or the client
	// ID. Any other edit keeps it. Both the clearing and its audit record are asserted.
	t.Run("a change of where the secret is sent clears it, is audited, and the new place never sees it", func(t *testing.T) {
		clears := func(slug string) int {
			return len(auditWhere(auditAll(t, s), "server.secret_clear", func(e auditEntry) bool { return ptrIs(e.Target, slug) }))
		}
		put := func(slug string, mut func(b map[string]any)) catalogView {
			t.Helper()
			r := admin.Do(t, "PUT", "/api/servers/"+slug, catalogPutBody(catalogGet(t, admin, slug), mut))
			if r.Status != 200 {
				t.Fatalf("PUT /api/servers/%s: %d %s", slug, r.Status, r.Body)
			}
			var v catalogView
			r.JSON(t, "ServerView", &v)
			return v
		}

		t.Run("shared_secret: the upstream URL", func(t *testing.T) {
			const secret = "old-destination-key"
			oldUp := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a"), RequireHeader: [2]string{"Authorization", "Bearer " + secret}})
			newUp := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a", "b"), RequireHeader: [2]string{"Authorization", "Bearer " + secret}})
			slug := harness.Unique("sdest")
			if v := catalogRegister(t, admin, fakes.ServerInput(oldUp, slug, "shared_secret", fakes.WithSecret(secret))); !v.SecretSet || v.ToolCount != 1 {
				t.Fatalf("registered: %+v", v)
			}
			// Edits that do not move the secret keep it and write no secret_clear.
			for name, mut := range map[string]func(b map[string]any){
				"the name":           func(b map[string]any) { b["name"] = "Renamed" },
				"the header":         func(b map[string]any) { b["auth"].(map[string]any)["header"] = "X-Api-Key" },
				"the template":       func(b map[string]any) { b["auth"].(map[string]any)["template"] = "Key {secret}" },
				"nothing at all":     func(map[string]any) {},
				"the URL, to itself": func(b map[string]any) { b["url"] = oldUp.URL },
			} {
				if v := put(slug, mut); !v.SecretSet || !catalogGet(t, admin, slug).SecretSet {
					t.Fatalf("an edit of %s dropped the secret", name)
				}
			}
			if n := clears(slug); n != 0 {
				t.Fatalf("%d server.secret_clear records after edits that keep the secret", n)
			}
			// Put the header back so the sync below would be accepted by an upstream holding the secret.
			put(slug, func(b map[string]any) {
				b["auth"].(map[string]any)["header"] = "Authorization"
				b["auth"].(map[string]any)["template"] = "Bearer {secret}"
			})
			updatesBefore := len(auditWhere(auditAll(t, s), "server.update", func(e auditEntry) bool { return ptrIs(e.Target, slug) }))

			oldUp.Reset() // what the old upstream saw while the server was set up is not the question
			v := put(slug, func(b map[string]any) { b["url"] = newUp.URL })
			if v.SecretSet || catalogGet(t, admin, slug).SecretSet || v.URL != newUp.URL {
				t.Fatalf("after the URL changed: %+v", v)
			}
			if n := countSQL(t, s, "select count(*) from catalog.servers where slug = $1 and secret is not null", slug); n != 0 {
				t.Fatalf("a secret is still stored after the URL changed (%d rows)", n)
			}
			if n := clears(slug); n != 1 {
				t.Fatalf("server.secret_clear records: %d, want 1", n)
			}
			cleared := auditWhere(auditAll(t, s), "server.secret_clear", func(e auditEntry) bool { return ptrIs(e.Target, slug) })[0]
			if !ptrIs(cleared.ActorID, adminID) || len(cleared.Meta) != 0 {
				t.Errorf("server.secret_clear: %+v, want the admin and no meta", cleared)
			}
			if n := len(auditWhere(auditAll(t, s), "server.update", func(e auditEntry) bool { return ptrIs(e.Target, slug) })); n != updatesBefore+1 {
				t.Errorf("server.update records: %d, want %d", n, updatesBefore+1)
			}
			// A sync does not reach the new upstream: there is no secret to send.
			catalogExpectError(t, "sync after the URL changed", admin.Do(t, "POST", "/api/servers/"+slug+"/sync", nil), 502, "sync_failed|The server secret is not set")
			if got := catalogGet(t, admin, slug); got.syncError() != "The server secret is not set" || got.ToolCount != 1 {
				t.Errorf("after the refused sync: %+v (sync error %q)", got, got.syncError())
			}
			for _, r := range newUp.Requests() {
				t.Errorf("the new upstream received a %q request: %v", r.Method, r.Header)
			}
			for _, r := range oldUp.Requests() {
				t.Errorf("the old upstream received a %q request after the move: %v", r.Method, r.Header)
			}
			// Control: setting the secret again makes the sync work and send it to the new place only.
			if r := admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{"secret": secret}); r.Status != 204 {
				t.Fatalf("set: %d %s", r.Status, r.Body)
			}
			if res := catalogSyncOK(t, admin, slug); res.Added != 1 || res.Removed != 0 {
				t.Errorf("sync with the secret set again: %+v", res)
			}
			if n := newUp.ListRequests(); n != 1 {
				t.Errorf("the new upstream saw %d tools/list requests, want 1", n)
			}
		})

		t.Run("oauth_user: the token URL, the revoke URL and the client ID", func(t *testing.T) {
			up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
			slug := harness.Unique("odest")
			catalogRegister(t, admin, fakes.ServerInput(up, slug, "oauth_user", catalogOAuth(func(o map[string]any) {
				o["revokeUrl"] = "https://git.example.com/revoke"
			}), fakes.WithSecret("client-secret")))
			oauth := func(mut func(o map[string]any)) func(b map[string]any) {
				return func(b map[string]any) {
					o := b["auth"].(map[string]any)["oauth"].(map[string]any)
					mut(o)
				}
			}
			keep := map[string]func(b map[string]any){
				"the authorize URL": oauth(func(o map[string]any) { o["authorizeUrl"] = "https://git.example.com/authorize2" }),
				"the scopes":        oauth(func(o map[string]any) { o["scopes"] = []string{"read", "write"} }),
				"the template":      func(b map[string]any) { b["auth"].(map[string]any)["template"] = "token {secret}" },
				"the upstream URL":  func(b map[string]any) { b["url"] = up.URL + "?x=1" },
			}
			for name, mut := range keep {
				if v := put(slug, mut); !v.SecretSet || !catalogGet(t, admin, slug).SecretSet {
					t.Fatalf("an edit of %s dropped the client secret", name)
				}
			}
			if n := clears(slug); n != 0 {
				t.Fatalf("%d server.secret_clear records after edits that keep the secret", n)
			}
			total := 0
			for name, mut := range map[string]func(b map[string]any){
				"the token URL":  oauth(func(o map[string]any) { o["tokenUrl"] = "https://elsewhere.example.com/token" }),
				"the revoke URL": oauth(func(o map[string]any) { o["revokeUrl"] = "https://elsewhere.example.com/revoke" }),
				"the client ID":  oauth(func(o map[string]any) { o["clientId"] = "another-client" }),
			} {
				if r := admin.Do(t, "PUT", "/api/servers/"+slug+"/secret", map[string]any{"secret": "client-secret"}); r.Status != 204 {
					t.Fatalf("set: %d %s", r.Status, r.Body)
				}
				total = clears(slug)
				if v := put(slug, mut); v.SecretSet || catalogGet(t, admin, slug).SecretSet {
					t.Fatalf("an edit of %s kept the client secret", name)
				}
				if n := clears(slug); n != total+1 {
					t.Errorf("an edit of %s: %d server.secret_clear records, want %d", name, n, total+1)
				}
			}
		})
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
		entries := auditWhere(auditAll(t, s), "server.update", func(e auditEntry) bool { return ptrIs(e.Target, slug) })
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
		// One subtest per route, named without slashes (a slash in a name splits -run and -skip
		// patterns), so that a server under test that lacks one route is judged on the others.
		routes := []struct{ name, method, path string }{
			{"GET servers", "GET", "/api/servers"},
			{"POST servers", "POST", "/api/servers"},
			{"GET server", "GET", "/api/servers/" + slug},
			{"PUT server", "PUT", "/api/servers/" + slug},
			{"PUT secret", "PUT", "/api/servers/" + slug + "/secret"},
			{"POST status", "POST", "/api/servers/" + slug + "/status"},
			{"POST sync", "POST", "/api/servers/" + slug + "/sync"},
			{"GET tools", "GET", "/api/servers/" + slug + "/tools"},
			{"PUT hidden", "PUT", "/api/servers/" + slug + "/tools/list_repos/hidden"},
			{"GET calls", "GET", "/api/calls"},
			{"GET audit", "GET", "/api/audit"},
		}
		for _, rt := range routes {
			t.Run(rt.name, func(t *testing.T) {
				// A body that is not JSON: an admin would get 422, a member must get 403 first.
				catalogExpectError(t, "member "+rt.method+" "+rt.path, member.Do(t, rt.method, rt.path, "{bad"), 403, catalogAdminErr)
			})
		}
		// Negative controls: the admin reaches the body, and a member may read the catalog.
		catalogExpectError(t, "admin POST with a broken body", admin.Do(t, "POST", "/api/servers", "{bad"), 422, "invalid_request|body is not valid JSON")
		if r := member.Do(t, "GET", "/api/catalog", nil); r.Status != 200 {
			t.Fatalf("member GET /api/catalog: %d", r.Status)
		}
		// No session: 401 on every route that needs one.
		anon := s.Anonymous()
		for _, path := range []string{"/api/servers", "/api/catalog", "/api/calls", "/api/audit"} {
			t.Run("anonymous GET "+strings.TrimPrefix(path, "/api/"), func(t *testing.T) {
				catalogExpectError(t, "anonymous GET "+path, anon.Do(t, "GET", path, nil), 401, identityUnauthenticated)
			})
		}
		// A bearer token never opens the web API, even next to a valid session cookie.
		catalogExpectError(t, "Bearer next to a cookie", admin.Do(t, "GET", "/api/servers", nil, "Authorization: Bearer sk_abcdef"), 401, "unauthenticated|tokens do not work on the web API")
		// A mutation from another origin is refused.
		catalogExpectError(t, "POST with a foreign Origin", admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, harness.Unique("csrf"), "none"), "Origin: http://evil.test"), 403, catalogCSRF)
		catalogExpectError(t, "POST without Origin", admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, harness.Unique("csrf"), "none"), "Origin:"), 403, catalogCSRF)
	})

	t.Run("upstream calls", func(t *testing.T) { catalogUpstreamCalls(t, s, admin) })
}

// ---- optimistic concurrency ----

// TestCatalogOptimisticConcurrency enforces optimistic concurrency as CLAUDE.md prescribes: a
// write that was prepared from an older state of the server is refused with 409 server_conflict
// and nothing of it is stored, never merged over the other change. Only the observable outcome
// is asserted (the loser's status and error, the winner's data kept, no lost update).
func TestCatalogOptimisticConcurrency(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})

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

			r := catalogRaceWrite(t, s, slug, func() harness.Response { return w.send(slug, v) })
			catalogExpectError(t, w.name+" racing a change", r, 409, fmt.Sprintf("server_conflict|server %q was changed meanwhile; reload it and try again", slug))
			// Nothing of the losing request was written; the other admin's change stands.
			if got := catalogGet(t, admin, slug); got.Description != "theirs" || got.Name != fakes.DisplayName(slug) {
				t.Fatalf("description %q name %q: the refused request wrote, or the other change was lost", got.Description, got.Name)
			}
			if !bytes.Equal(secretBytes(t, slug), secretBefore) {
				t.Fatal("the secret changed although the request was refused")
			}
			if got := catalogGet(t, admin, slug); got.Status != "active" {
				t.Fatalf("status %q after a refused change", got.Status)
			}

			// Control: from the current state the same request goes through (nothing in between).
			fresh := catalogGet(t, admin, slug)
			if r := w.send(slug, fresh); r.Status != 200 && r.Status != 204 {
				t.Fatalf("the same request without a race: %d %s", r.Status, r.Body)
			}
		})
	}

	t.Run("the version is not part of the API", func(t *testing.T) {
		slug := harness.Unique("ver")
		catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
		// A read-modify-write cycle carries no version: the view has none (the strict ServerView
		// schema would reject one) and a PUT built from it is all the server needs.
		r := admin.Do(t, "GET", "/api/servers/"+slug, nil)
		var raw map[string]any
		r.JSON(t, "ServerView", &raw)
		if _, ok := raw["version"]; ok {
			t.Fatalf("ServerView carries a version: %v", raw)
		}
	})
}

// ---- validation goldens ----

// catalogGoldenCase records a refused request under compat/golden/validation/<name>.json as
// {"status", "code", "path", "message"}. The rule (see also compat/golden/validation/README.md):
//
//   - A 422 invalid_request whose body comes from zod parsing of the request body is recorded
//     with its status, code and the field path the message starts with ("auth.oauth.clientId: ..."
//     gives "auth.oauth.clientId", none gives ""), and "message": null, which the comparison
//     treats as "not compared". The wording and the choice of the first issue are zod v4's and
//     concern inputs the web form already prevents client-side; a new server need not copy them.
//   - Every other refusal keeps its exact message: the domain errors the web form maps to fields
//     or shows verbatim (invalid_slug, invalid_auth, server_exists, server_conflict, ...) and the
//     one invalid_request that is not zod's, "body is not valid JSON".
func catalogGoldenCase(t *testing.T, name string, r harness.Response, wantStatus int) {
	t.Helper()
	if r.Status != wantStatus {
		t.Fatalf("%s: status %d, want %d (body %.300s)", name, r.Status, wantStatus, r.Body)
	}
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	r.JSON(t, "ErrorBody", &e)
	type golden struct {
		Status  int     `json:"status"`
		Code    string  `json:"code"`
		Path    string  `json:"path"`
		Message *string `json:"message"`
	}
	g := golden{Status: r.Status, Code: e.Error.Code, Message: &e.Error.Message}
	if e.Error.Code == "invalid_request" && e.Error.Message != "body is not valid JSON" {
		if m := zodBodyPath.FindStringSubmatch(e.Error.Message); m != nil {
			g.Path = m[1]
		}
		g.Message = nil
	}
	doc, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	harness.Golden(t, "validation/"+name+".json", append(doc, '\n'))
}

// TestCatalogValidation records every refusal the server-administration forms can provoke under
// compat/golden/validation. The comparison rule (zod wording not compared, domain messages exact)
// is documented at catalogGoldenCase.
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
		"body_not_json":             "{bad",
		"body_empty_object":         `{}`,
		"body_missing_name":         `{"slug":"abc"}`,
		"body_array":                `[]`,
		"body_null":                 `null`,
		"body_timeout_not_a_number": `{"slug":"abc","name":"x","description":"","url":"http://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"timeoutMs":"x"}`,
		"body_access_missing":       `{"slug":"abc","name":"x","description":"","url":"http://x.example","auth":{"kind":"none"}}`,
		"body_description_missing":  `{"slug":"abc","name":"x","url":"http://x.example","auth":{"kind":"none"},"access":{"kind":"all"}}`,
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

// catalogUpstreamCalls is the "upstream client" group of TestCatalogServers: the same stack
// (small UPSTREAM_MAX_RESPONSE_BYTES) and admin, call_tool as the way to reach tools/call.
func catalogUpstreamCalls(t *testing.T, s *harness.Stack, admin *harness.Client) {
	const secret = "k-live-7f3a9c" //nolint:gosec // G101: a made-up secret that the test hunts for
	led := newMCPLedger(t, s)
	led.note(secret, "k-second")
	c := led.session(admin.Token(t, "catalog-calls"))
	other := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("z")})
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	slug := harness.Unique("up")
	catalogRegister(t, admin, fakes.ServerInput(up, slug, "shared_secret", fakes.WithHeader("X-API-Key", "key {secret}"), fakes.WithSecret(secret), fakes.WithTimeoutMs(1000)))
	ref := slug + ".echo"
	name := fakes.DisplayName(slug) // product texts name the server by its display name
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
		{"HTTP 401", func() { up.FailWith(401) }, name + " rejected the gateway credentials. Ask the administrator to check the server key.", "transport_error", 0},
		{"HTTP 500", func() { up.FailWith(500) }, name + " is unavailable.", "transport_error", 0},
		{"slower than timeoutMs", func() { up.Delay(5 * time.Second) }, name + " did not respond in 1 s.", "timeout", 4 * time.Second},
		{"a Content-Length above the limit", func() { up.RespondBytes(100_000) }, name + " returned a response larger than 0.06 MiB.", "transport_error", 0},
		// The limit is hit while the answer streams, long before the 1 s timeout: half of it is a generous ceiling.
		{"a streamed answer above the limit", func() { up.RespondChunked(100_000) }, name + " returned a response larger than 0.06 MiB.", "transport_error", 500 * time.Millisecond},
		{"a JSON-RPC invalid params error", func() { up.RejectCalls(-32602, "Invalid params: owner is required") }, name + " rejected the call (JSON-RPC -32602): Invalid params: owner is required", "upstream_error", 0},
		{"a JSON-RPC internal error", func() { up.RejectCalls(-32603, "repository not found") }, name + " rejected the call (JSON-RPC -32603): repository not found", "upstream_error", 0},
		{"a redirect is not followed", func() { up.RedirectTo(other.URL) }, name + " is unavailable.", "transport_error", 0},
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
				t.Fatalf("the call took %s, more than the allowed %s (timeoutMs is 1000)", time.Since(started), f.within)
			}
			got := outcomes()
			if len(got) == 0 || got[len(got)-1] != f.outcome {
				t.Fatalf("call log outcomes %v: the newest must be %q", got, f.outcome)
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
