//go:build compat

package compat_test

// Gateway scenarios: what call_tool does between the agent and the upstream. The result goes
// through unchanged, the arguments are checked against the tool's inputSchema, the daily limit is
// counted per person and server, every call leaves one row in the call log (no arguments, no
// results), and an unexpected failure never leaks. Source: v1.0.1:apps/server/test/gateway/call.int.test.ts. The protocol, the
// meta-tools and the surface are in mcp_test.go; the call log and audit REST endpoints are in
// gateway_rest_test.go.
//
// Already covered elsewhere (cross-references):
//   - "pat_user: upstream 401 expires the connection" (no retry), "pat_user without a connection:
//     needs_connection, upstream not called" and the OAuth 401 -> one refresh and one retry with the
//     quota spent once: connections_connect_test.go (TestConnectionsPAT) and
//     connections_refresh_test.go (TestConnectionsUpstreamRejectsTheToken);
//   - "a hanging upstream: timeout text" (1 s), "upstream JSON-RPC errors", "a huge answer: size
//     text" and the 401/500/redirect texts: catalog_test.go ("upstream calls"); this file adds the
//     rounding of the timeout (1.5 s), the cut of long JSON-RPC messages and the empty one;
//   - the daily limit counted per person: connections_refresh_test.go ("the daily limit counts
//     calls per person"); this file has the full scenario (reset at UTC midnight, per server,
//     parallel calls).
//
// Source cases that are not ported (they need the application's internals, not its outside):
//   - "PgCallLog lists newest first with filters and paging" is ported through GET /api/calls
//     (gateway_rest_test.go) with rows written by SQL, which is what the endpoint reads;
//   - "PgCallLog.deleteOlderThan" and "PgQuota.deleteDaysBefore": ported through the scheduler's
//     run at start (TestGatewayFaults, "history retention"), the only trigger from outside;
//   - the callTool unit-level result "toStrictEqual" on the SDK object: the wire form is compared
//     instead (the classic protocol result, exactly as it went over HTTP).
//
// Not provoked black-box: an unexpected failure of list_servers, search_tools or describe_tool
// (`Internal error.` is the shared `guarded` wrapper; call_tool is provoked through the quota
// table) and "mcp: upstream result is not a CallToolResult" (the upstream client validates it
// first). Upstream `_meta` is not passed to the agent by the image (see the report).

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// gwPNG is a 1x1 transparent PNG.
const gwPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// ---- helpers ----

// gwExpectRefused checks an isError answer with exactly that text.
func gwExpectRefused(t *testing.T, what string, res *mcp.CallToolResult, want string) {
	t.Helper()
	if !res.IsError || catalogText(res) != want {
		t.Fatalf("%s: isError %v text %q, want an error with %q", what, res.IsError, catalogText(res), want)
	}
}

func gwQuotaCount(t *testing.T, s *harness.Stack, userID, server string) int {
	t.Helper()
	return countSQL(t, s, "select coalesce(sum(count), 0)::int from gateway.quota where user_id = $1::uuid and server = $2", userID, server)
}

// gwNextMidnight is the next UTC midnight by the database's clock, as the quota text writes it.
func gwNextMidnight(t *testing.T, s *harness.Stack) string {
	t.Helper()
	var out string
	err := s.DB.QueryRow(t.Context(), `select to_char(date_trunc('day', now() at time zone 'utc') + interval '1 day', 'YYYY-MM-DD"T"HH24:MI:SS".000Z"')`).Scan(&out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// gwBlockWrites makes every insert or update of the table fail, until the returned restore runs.
func gwBlockWrites(t *testing.T, s *harness.Stack, table string) (restore func()) {
	t.Helper()
	name := "compat_gw_" + strings.ReplaceAll(table, ".", "_")
	execSQL(t, s, fmt.Sprintf(`create function public.%s() returns trigger language plpgsql as
		$$ begin raise exception 'blocked by the compat suite'; end $$`, name))
	execSQL(t, s, fmt.Sprintf(`create trigger %s before insert or update on %s for each row execute function public.%s()`, name, table, name))
	done := false
	return func() {
		if done {
			return
		}
		done = true
		execSQL(t, s, fmt.Sprintf(`drop trigger %s on %s`, name, table))
		execSQL(t, s, fmt.Sprintf(`drop function public.%s()`, name))
	}
}

// gwTablesContaining returns the tables (schema.table) with a row whose text contains the marker.
func gwTablesContaining(t *testing.T, s *harness.Stack, marker string) []string {
	t.Helper()
	rows, err := s.DB.Query(t.Context(), `select table_schema, table_name from information_schema.tables
		where table_schema in ('identity', 'catalog', 'connections', 'search', 'gateway', 'platform') and table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables [][2]string
	for rows.Next() {
		var sc, tb string
		if err := rows.Scan(&sc, &tb); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, [2]string{sc, tb})
	}
	rows.Close()
	if len(tables) < 10 {
		t.Fatalf("only %d tables found: the scan would prove little", len(tables))
	}
	var found []string
	for _, tb := range tables {
		q := fmt.Sprintf(`select count(*) from %q.%q x where x::text like $1`, tb[0], tb[1])
		if n := countSQL(t, s, q, "%"+marker+"%"); n > 0 {
			found = append(found, tb[0]+"."+tb[1])
		}
	}
	return found
}

// ---- TestGatewayCall ----

func TestGatewayCall(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	led := newMCPLedger(t, s)
	alice := gwNewPerson(t, s, led)
	anon := s.Anonymous()

	t.Run("the upstream's result goes through unchanged: text, structured, image, several blocks, isError", func(t *testing.T) {
		png, err := base64.StdEncoding.DecodeString(gwPNG)
		if err != nil {
			t.Fatal(err)
		}
		e := gwNewServer(t, led, admin, fakes.Tools("echo", "image", "multi", "failing"), "shared_secret", fakes.WithHeader("X-API-Key", "key {secret}"), fakes.WithSecret("gw-key-1"))
		led.note("gw-key-1")
		e.up.SetResult("image", &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: png, MIMEType: "image/png"}}})
		e.up.SetResult("multi", &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "two parts"}, &mcp.ImageContent{Data: png, MIMEType: "image/png"}},
			StructuredContent: map[string]any{"n": 1, "list": []any{1, 2}},
		})
		e.up.SetResult("failing", &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "repository not found"}}, IsError: true})

		textBlock := func(text string) map[string]any { return map[string]any{"type": "text", "text": text} }
		imageBlock := map[string]any{"type": "image", "data": gwPNG, "mimeType": "image/png"}
		for _, c := range []struct {
			tool string
			args map[string]any
			want map[string]any // the result object exactly as it goes over the wire (classic protocol)
		}{
			{"echo", map[string]any{"title": "Bug"}, map[string]any{
				"content": []any{textBlock(`{"title":"Bug"}`)}, "structuredContent": map[string]any{"echo": map[string]any{"title": "Bug"}},
			}},
			{"image", map[string]any{}, map[string]any{"content": []any{imageBlock}}},
			{"multi", map[string]any{}, map[string]any{
				"content": []any{textBlock("two parts"), imageBlock}, "structuredContent": map[string]any{"n": float64(1), "list": []any{float64(1), float64(2)}},
			}},
			{"failing", map[string]any{}, map[string]any{"content": []any{textBlock("repository not found")}, "isError": true}},
		} {
			got := mcpToolsCall(t, led, anon, alice.token, "call_tool", map[string]any{"tool": e.ref(c.tool), "arguments": c.args})
			// An upstream's own _meta is not part of the comparison, on either side: whether the
			// gateway passes it on or drops it is an open decision (finding "upstream _meta" in the
			// task 10 report), and so is the resultType a client library may add.
			delete(got, "_meta")
			delete(got, "resultType")
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s:\n got %v\nwant %v", c.tool, got, c.want)
			}
		}
		// Through the SDK client too (the stateless revision adds its own _meta and resultType).
		res := alice.call(led, e.ref("image"), map[string]any{})
		img, ok := res.Content[0].(*mcp.ImageContent)
		if res.IsError || len(res.Content) != 1 || !ok || string(img.Data) != string(png) || img.MIMEType != "image/png" {
			t.Errorf("the image through the SDK client: %+v", res)
		}
		if calls := connAuths(e.up, "X-API-Key"); len(calls) != 5 || calls[0] != "key gw-key-1" {
			t.Errorf("the upstream saw %v", redactAll(calls))
		}
		// The call log tells the upstream's own error from success.
		rows := gwRows(t, s, e.slug)
		if got := gwOutcomes(rows); !slices.Equal(got, []string{"ok", "ok", "ok", "upstream_error", "ok"}) {
			t.Errorf("outcomes %v", got)
		}
	})

	t.Run("the arguments are checked against the inputSchema before the upstream, the limit and the credentials", func(t *testing.T) {
		strict := fakes.Tool{Name: "strict", Description: "strict", InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":  map[string]any{"type": "string"},
				"count":  map[string]any{"type": "integer", "minimum": 0},
				"nested": map[string]any{"type": "object", "properties": map[string]any{"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "additionalProperties": false},
			},
			"required": []string{"title"}, "additionalProperties": false,
		}}
		pattern := fakes.Tool{Name: "pattern", Description: "pattern", InputSchema: map[string]any{
			"type":              "object",
			"properties":        map[string]any{"code": map[string]any{"type": "string", "pattern": "^[A-Z]+$"}},
			"patternProperties": map[string]any{"^x": map[string]any{"type": "number"}},
		}}
		brokenType := fakes.Tool{Name: "broken_type", Description: "b", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "nonsense"}}}}
		brokenRef := fakes.Tool{Name: "broken_ref", Description: "b", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"$ref": "#/definitions/missing"}}}}
		e := gwNewServer(t, led, admin, []fakes.Tool{strict, pattern, brokenType, brokenRef}, "none", fakes.WithDailyLimit(10))

		// The text is the product's: `Invalid arguments at <path>: <message>. Call describe_tool("<ref>")
		// for the schema.` The message is the JSON Schema validator's own wording and is not pinned
		// (it only has to be there). Which node a validator blames for a missing required property or a
		// forbidden extra one is a convention of the library: the image reports the object itself
		// (`/`), another validator may report the property's own path, so both are accepted there.
		refuse := func(ref string, args map[string]any, paths ...string) {
			t.Helper()
			res := alice.call(led, ref, args)
			text := catalogText(res)
			rest, ok := strings.CutPrefix(text, "Invalid arguments at ")
			path, tail, found := strings.Cut(rest, ": ")
			wantTail := fmt.Sprintf(". Call describe_tool(%q) for the schema.", ref)
			message, hasTail := strings.CutSuffix(tail, wantTail)
			if !res.IsError || !ok || !found || !hasTail || message == "" || !slices.Contains(paths, path) {
				t.Errorf("%v: isError %v text %q, want `Invalid arguments at <%v>: <message>%s`", args, res.IsError, text, paths, wantTail)
			}
		}
		ref := e.ref("strict")
		refuse(ref, map[string]any{"title": 5}, "/title")
		refuse(ref, map[string]any{}, "/", "/title") // a missing required property
		refuse(ref, map[string]any{"title": "x", "extra": 1}, "/", "/extra")
		refuse(ref, map[string]any{"title": "x", "nested": map[string]any{"tags": []any{"a", 3}}}, "/nested/tags/1")
		refuse(ref, map[string]any{"title": "x", "nested": map[string]any{"zzz": 1}}, "/nested", "/nested/zzz")
		refuse(ref, map[string]any{"title": "x", "count": 1.5}, "/count")
		refuse(ref, map[string]any{"title": "x", "count": -1}, "/count")
		if n := len(e.up.Calls()); n != 0 {
			t.Fatalf("the upstream was called %d times for invalid arguments", n)
		}
		if n := gwQuotaCount(t, s, alice.id, e.slug); n != 0 {
			t.Fatalf("invalid arguments spent %d of the daily limit", n)
		}
		if got := gwOutcomes(gwRows(t, s, e.slug)); len(got) != 7 || slices.Contains(got, "ok") || slices.ContainsFunc(got, func(o string) bool { return o != "denied:invalid_arguments" }) {
			t.Fatalf("outcomes %v, want seven denied:invalid_arguments", got)
		}
		// An omitted arguments object is {}: it fails the required field of this schema.
		res := catalogCallMCP(t, alice.mcp, "call_tool", map[string]any{"tool": ref})
		if text := catalogText(res); !res.IsError || !strings.HasPrefix(text, "Invalid arguments at ") || !strings.HasSuffix(text, fmt.Sprintf(". Call describe_tool(%q) for the schema.", ref)) {
			t.Fatalf("no arguments: %q", text)
		}
		// Control: valid arguments reach the upstream and spend the limit once.
		good := map[string]any{"title": "x", "count": 2, "nested": map[string]any{"tags": []any{"a"}}}
		if res := alice.call(led, ref, good); res.IsError {
			t.Fatalf("valid arguments: %s", catalogText(res))
		}
		if calls := e.up.Calls(); len(calls) != 1 || calls[0].Tool != "strict" {
			t.Fatalf("the upstream saw %+v", calls)
		}
		if n := gwQuotaCount(t, s, alice.id, e.slug); n != 1 {
			t.Fatalf("the daily limit spent: %d, want 1", n)
		}

		// A pattern is not enforced (ReDoS protection): the upstream decides. A type still is.
		e.up.Reset()
		res = alice.call(led, e.ref("pattern"), map[string]any{"code": "abc", "xa": 1})
		if res.IsError {
			t.Fatalf("a value violating a pattern: %s", catalogText(res))
		}
		if calls := e.up.Calls(); len(calls) != 1 || string(calls[0].Arguments) != `{"code":"abc","xa":1}` {
			t.Fatalf("the call did not reach the upstream as it was: %+v", calls)
		}
		refuse(e.ref("pattern"), map[string]any{"code": 5}, "/code")

		// A schema the validator cannot compile is not enforced at all.
		for _, tool := range []string{"broken_type", "broken_ref"} {
			e.up.Reset()
			if res := alice.call(led, e.ref(tool), map[string]any{"x": []any{1, "a"}}); res.IsError {
				t.Errorf("%s: %s", tool, catalogText(res))
			}
			if n := len(e.up.Calls()); n != 1 {
				t.Errorf("%s: the upstream saw %d calls, want 1", tool, n)
			}
		}
	})

	t.Run("patterns are not enforced, but properties named like keywords and shared $id values are", func(t *testing.T) {
		long := strings.Repeat("a", 40) + "!"
		evil := fakes.Tool{Name: "evil", Description: "e", InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"name": map[string]any{"type": "string", "pattern": "^(a+)+$"}},
			"patternProperties":    map[string]any{"^(x+)+y$": map[string]any{"type": "integer"}},
			"additionalProperties": false,
		}}
		keywords := fakes.Tool{Name: "keywords", Description: "k", InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":           map[string]any{"type": "string"},
				"patternProperties": map[string]any{"type": "integer"},
				"nested":            map[string]any{"type": "array", "items": map[string]any{"anyOf": []any{map[string]any{"type": "string", "pattern": "^(a+)+$"}}}},
			},
			"required": []string{"pattern"},
		}}
		idA := fakes.Tool{Name: "id_a", Description: "a", InputSchema: map[string]any{"$id": "urn:compat:tool", "type": "object", "properties": map[string]any{"n": map[string]any{"type": "string"}}}}
		idB := fakes.Tool{Name: "id_b", Description: "b", InputSchema: map[string]any{"$id": "urn:compat:tool", "type": "object", "properties": map[string]any{"n": map[string]any{"type": "number"}}}}
		e := gwNewServer(t, led, admin, []fakes.Tool{evil, keywords, idA, idB}, "none")
		refuse := func(tool string, args map[string]any, paths ...string) {
			t.Helper()
			text := catalogText(alice.call(led, e.ref(tool), args))
			path, _, _ := strings.Cut(strings.TrimPrefix(text, "Invalid arguments at "), ": ")
			if !strings.HasPrefix(text, "Invalid arguments at ") || !slices.Contains(paths, path) {
				t.Errorf("%s %v: %q, want a refusal at one of %v", tool, args, text, paths)
			}
		}
		reaches := func(tool string, args map[string]any) {
			t.Helper()
			e.up.Reset()
			if res := alice.call(led, e.ref(tool), args); res.IsError {
				t.Errorf("%s %v: %s", tool, args, catalogText(res))
			} else if n := len(e.up.Calls()); n != 1 {
				t.Errorf("%s %v: the upstream saw %d calls, want 1", tool, args, n)
			}
		}
		// The properties that a patternProperties of the schema allows are not refused by the
		// additionalProperties: false next to it (the keyword is not run); the catastrophic regexes
		// of the schema return at once. Everything else is still checked.
		started := time.Now()
		reaches("evil", map[string]any{"name": long, strings.Repeat("x", 40): "z"})
		if d := time.Since(started); d > 5*time.Second {
			t.Errorf("a catastrophic pattern took %s", d)
		}
		refuse("evil", map[string]any{"name": 5}, "/name")
		// A property that is NAMED pattern or patternProperties is a property like any other.
		refuse("keywords", map[string]any{}, "/", "/pattern")
		refuse("keywords", map[string]any{"pattern": 1}, "/pattern")
		refuse("keywords", map[string]any{"pattern": "x", "patternProperties": "y"}, "/patternProperties")
		reaches("keywords", map[string]any{"pattern": "x", "nested": []any{long}})
		// Two tools that carry the same $id each validate against their own schema.
		refuse("id_a", map[string]any{"n": 1}, "/n")
		reaches("id_b", map[string]any{"n": 1})
		refuse("id_b", map[string]any{"n": "x"}, "/n")
		reaches("id_a", map[string]any{"n": "x"})
	})

	t.Run("every way a tool becomes unavailable gives one answer, no upstream call and denied:not_found", func(t *testing.T) {
		e := gwNewServer(t, led, admin, fakes.Tools("echo", "other"), "none")
		ref := e.ref("echo")
		view := catalogGet(t, admin, e.slug)
		denied := func(what, ref string) {
			t.Helper()
			before := len(gwRowsWhere(t, s, "user_id = $1::uuid and outcome = 'denied:not_found'", alice.id))
			gwExpectRefused(t, what, alice.call(led, ref, map[string]any{}), catalogNotAvailable(ref))
			after := gwRowsWhere(t, s, "user_id = $1::uuid and outcome = 'denied:not_found'", alice.id)
			if len(after) != before+1 {
				t.Fatalf("%s: %d denied:not_found rows, want %d", what, len(after), before+1)
			}
			if n := len(e.up.Calls()); n != 0 {
				t.Fatalf("%s: the upstream was called %d times", what, n)
			}
		}
		allowed := func(what string) {
			t.Helper()
			if res := alice.call(led, ref, map[string]any{}); res.IsError {
				t.Fatalf("%s: %s", what, catalogText(res))
			}
			e.up.Reset()
		}
		allowed("at the start")

		catalogHide(t, admin, e.slug, "echo", true)
		denied("hidden by the admin", ref)
		catalogHide(t, admin, e.slug, "echo", false)
		allowed("unhidden")

		e.up.SetTools(fakes.Tools("other"))
		catalogSyncOK(t, admin, e.slug)
		denied("gone from the upstream", ref)
		e.up.SetTools(fakes.Tools("echo", "other"))
		catalogSyncOK(t, admin, e.slug)
		e.up.Reset()
		allowed("back on the upstream")

		connSetStatus(t, admin, e.slug, "disabled")
		denied("server disabled", ref)
		connSetStatus(t, admin, e.slug, "active")
		allowed("server active again")

		closed := admin.Do(t, "PUT", "/api/servers/"+e.slug, catalogPutBody(view, func(b map[string]any) {
			b["access"] = map[string]any{"kind": "groups", "groups": []string{"ops"}}
		}))
		if closed.Status != 200 {
			t.Fatalf("PUT access: %d %s", closed.Status, closed.Body)
		}
		denied("the person's group lost the access", ref)
		if res := catalogCallMCP(t, s.MCP(t, admin.Token(t, "gw-admin")), "call_tool", map[string]any{"tool": ref, "arguments": map[string]any{}}); res.IsError {
			t.Fatalf("the admin is not shut out by a group list: %s", catalogText(res))
		}
		e.up.Reset()
		reopen := admin.Do(t, "PUT", "/api/servers/"+e.slug, catalogPutBody(view, nil))
		if reopen.Status != 200 {
			t.Fatalf("PUT access back: %d %s", reopen.Status, reopen.Body)
		}
		allowed("access open again")

		denied("an unknown tool", e.ref("nope"))
		denied("an unknown server", "no-such-"+e.slug+".echo")
		// A name that is not a <server>.<tool> at all is logged with the whole text as the tool.
		for _, bad := range []string{"nodot", e.slug + ".", "." + e.slug} {
			denied("the reference "+bad, bad)
		}
		rows := gwRowsWhere(t, s, "user_id = $1::uuid and outcome = 'denied:not_found' and server = ''", alice.id)
		if got := []string{rows[len(rows)-3].Tool, rows[len(rows)-2].Tool, rows[len(rows)-1].Tool}; !slices.Equal(got, []string{"nodot", e.slug + ".", "." + e.slug}) {
			t.Errorf("the log keeps a malformed reference as the tool of an empty server: %v", got)
		}
	})

	t.Run("the daily limit: per person and server, refused without reaching the upstream, reset at UTC midnight", func(t *testing.T) {
		q := gwNewServer(t, led, admin, nil, "none", fakes.WithDailyLimit(2))
		other := gwNewServer(t, led, admin, nil, "none", fakes.WithDailyLimit(1))
		free := gwNewServer(t, led, admin, nil, "none")
		bob := gwNewPerson(t, s, led)
		ref := q.ref("echo")
		args := map[string]any{"text": "hi"}
		ok := func(p gwPerson, ref string) {
			t.Helper()
			if res := p.call(led, ref, args); res.IsError {
				t.Fatalf("%s: %s", ref, catalogText(res))
			}
		}
		refused := func(p gwPerson, e *gwServer) {
			t.Helper()
			wantMidnight := []string{gwNextMidnight(t, s)}
			res := p.call(led, e.ref("echo"), args)
			wantMidnight = append(wantMidnight, gwNextMidnight(t, s))
			text := catalogText(res)
			var matched bool
			for _, m := range wantMidnight {
				matched = matched || text == fmt.Sprintf("Daily limit for %s reached. Resets at %s.", e.name, m)
			}
			if !res.IsError || !matched {
				t.Fatalf("isError %v text %q, want the daily limit text resetting at %v", res.IsError, text, wantMidnight)
			}
		}

		ok(alice, ref)
		ok(alice, ref)
		if n := len(q.up.Calls()); n != 2 {
			t.Fatalf("%d calls reached the upstream, want 2", n)
		}
		refused(alice, q)
		refused(alice, q)
		if n := len(q.up.Calls()); n != 2 {
			t.Fatalf("a refused call reached the upstream: %d calls", n)
		}
		if n := gwQuotaCount(t, s, alice.id, q.slug); n != 2 {
			t.Fatalf("the counter is %d, want 2: a refused call does not count", n)
		}
		if got := gwOutcomes(gwRows(t, s, q.slug)); !slices.Equal(got, []string{"ok", "ok", "denied:quota", "denied:quota"}) {
			t.Fatalf("outcomes %v", got)
		}

		// Per person: another person has the whole limit.
		ok(bob, ref)
		ok(bob, ref)
		refused(bob, q)
		// Per server: Alice's use of one server leaves another's limit alone, and each is its own.
		ok(alice, other.ref("echo"))
		refused(alice, other)
		ok(bob, other.ref("echo"))
		// No limit, no counter.
		for range 5 {
			ok(alice, free.ref("echo"))
		}
		if n := countSQL(t, s, "select count(*) from gateway.quota where server = $1", free.slug); n != 0 {
			t.Fatalf("a server without a limit has %d counter rows", n)
		}

		// A new UTC day: the counter of the day before is no longer the one in force.
		execSQL(t, s, "update gateway.quota set day = day - 1 where user_id = $1::uuid and server = $2", alice.id, q.slug)
		ok(alice, ref)
		ok(alice, ref)
		refused(alice, q)
		if n := countSQL(t, s, "select count(*) from gateway.quota where user_id = $1::uuid and server = $2 and day = (now() at time zone 'utc')::date and count = 2", alice.id, q.slug); n != 1 {
			t.Fatalf("today's counter row of Alice: %d matching rows, want 1 with count 2", n)
		}
		if n := countSQL(t, s, "select count(*) from gateway.quota where user_id = $1::uuid and server = $2 and day = (now() at time zone 'utc')::date - 1 and count = 2", alice.id, q.slug); n != 1 {
			t.Fatalf("yesterday's counter row is kept: %d rows", n)
		}
	})

	t.Run("parallel calls cannot exceed the daily limit", func(t *testing.T) {
		e := gwNewServer(t, led, admin, nil, "none", fakes.WithDailyLimit(3))
		p := gwNewPerson(t, s, led)
		results := gwCallMany(t, p, e.ref("echo"), 10, func(int) map[string]any { return map[string]any{"text": "hi"} })
		var okN, refusedN int
		for _, r := range results {
			if !r.IsError {
				okN++
			} else if strings.HasPrefix(catalogText(r), "Daily limit for "+e.name+" reached.") {
				refusedN++
			}
		}
		if okN != 3 || refusedN != 7 || len(e.up.Calls()) != 3 {
			t.Fatalf("%d ok, %d refused by the limit, %d reached the upstream; want 3, 7, 3", okN, refusedN, len(e.up.Calls()))
		}
		if n := gwQuotaCount(t, s, p.id, e.slug); n != 3 {
			t.Fatalf("counter %d, want 3", n)
		}
		got := gwOutcomes(gwRows(t, s, e.slug))
		slices.Sort(got)
		if want := append(slices.Repeat([]string{"denied:quota"}, 7), "ok", "ok", "ok"); !slices.Equal(got, want) {
			t.Fatalf("outcomes %v", got)
		}
	})

	t.Run("a JSON-RPC error of the upstream: its code and message, cut to 500 characters, trimmed", func(t *testing.T) {
		e := gwNewServer(t, led, admin, nil, "none")
		head := e.name + " rejected the call (JSON-RPC %d)"
		long := strings.Repeat("m", 600)
		for _, c := range []struct {
			name string
			code int
			msg  string
			want string
		}{
			{"invalid params", -32602, "Invalid params: owner is required", fmt.Sprintf(head, -32602) + ": Invalid params: owner is required"},
			{"internal error", -32603, "repository not found", fmt.Sprintf(head, -32603) + ": repository not found"},
			{"exactly 500 characters", -32000, long[:500], fmt.Sprintf(head, -32000) + ": " + long[:500]},
			{"501 characters", -32000, long[:501], fmt.Sprintf(head, -32000) + ": " + long[:499] + "…"},
			{"600 characters", -32000, long, fmt.Sprintf(head, -32000) + ": " + long[:499] + "…"},
			{"no message", -32001, "", fmt.Sprintf(head, -32001) + "."},
			{"only blanks", -32001, "  \n ", fmt.Sprintf(head, -32001) + "."},
			{"padded", -32002, "  padded  ", fmt.Sprintf(head, -32002) + ": padded"},
		} {
			e.up.Reset()
			e.up.RejectCalls(c.code, c.msg)
			gwExpectRefused(t, c.name, alice.call(led, e.ref("echo"), map[string]any{}), c.want)
			if n := len(e.up.Calls()); n != 1 {
				t.Errorf("%s: %d calls reached the upstream, want 1 (no retry)", c.name, n)
			}
		}
		rows := gwRows(t, s, e.slug)
		if len(rows) != 8 || slices.ContainsFunc(gwOutcomes(rows), func(o string) bool { return o != "upstream_error" }) {
			t.Errorf("outcomes %v, want eight upstream_error", gwOutcomes(rows))
		}
		// Control: without the rejection the same server answers.
		e.up.Reset()
		if res := alice.call(led, e.ref("echo"), map[string]any{}); res.IsError {
			t.Fatalf("after the rejections: %s", catalogText(res))
		}
	})

	t.Run("a timeout is written in seconds, 1.5 s for 1500 ms, and logged as timeout", func(t *testing.T) {
		e := gwNewServer(t, led, admin, nil, "none", fakes.WithTimeoutMs(1500))
		e.up.Delay(5 * time.Second)
		gwExpectRefused(t, "slow upstream", alice.call(led, e.ref("echo"), map[string]any{}), e.name+" did not respond in 1.5 s.")
		rows := gwRows(t, s, e.slug)
		if len(rows) != 1 || rows[0].Outcome != "timeout" || rows[0].DurationMs < 1400 || rows[0].DurationMs > 4500 {
			t.Fatalf("call log %+v, want one timeout of about 1.5 s", rows)
		}
		e.up.Reset()
		if res := alice.call(led, e.ref("echo"), map[string]any{}); res.IsError {
			t.Fatalf("the upstream is fast again: %s", catalogText(res))
		}
	})

	t.Run("20 parallel calls with one token all succeed and leave 20 rows", func(t *testing.T) {
		e := gwNewServer(t, led, admin, nil, "none")
		p := gwNewPerson(t, s, led)
		results := gwCallMany(t, p, e.ref("echo"), 20, func(i int) map[string]any { return map[string]any{"text": fmt.Sprint("n", i)} })
		for i, r := range results {
			if want := fmt.Sprintf(`{"text":"n%d"}`, i); r.IsError || catalogText(r) != want {
				t.Errorf("call %d: isError %v %q, want the echo %s (the answers must not be mixed up)", i, r.IsError, catalogText(r), want)
			}
		}
		if n := len(e.up.Calls()); n != 20 {
			t.Errorf("%d calls reached the upstream, want 20", n)
		}
		rows := gwRows(t, s, e.slug)
		if len(rows) != 20 || slices.ContainsFunc(rows, func(r gwRow) bool {
			return r.Outcome != "ok" || r.UserID != p.id || r.TokenID == nil || *r.TokenID != p.tokenID || r.Tool != "echo"
		}) {
			t.Fatalf("%d call log rows: %+v", len(rows), rows)
		}
	})

	t.Run("the call log has these columns and no arguments or results, here or anywhere", func(t *testing.T) {
		argMarker, resMarker := "compat-arg-"+harness.Unique("a"), "compat-res-"+harness.Unique("r")
		e := gwNewServer(t, led, admin, fakes.Tools("echo", "answer"), "none")
		e.up.SetResult("answer", &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: resMarker}}, StructuredContent: map[string]any{"k": resMarker}})
		if res := alice.call(led, e.ref("echo"), map[string]any{"text": argMarker}); res.IsError || !strings.Contains(catalogText(res), argMarker) {
			t.Fatalf("echo: %s", catalogText(res))
		}
		if res := alice.call(led, e.ref("answer"), map[string]any{"text": argMarker}); res.IsError || !strings.Contains(catalogText(res), resMarker) {
			t.Fatalf("answer: %s", catalogText(res))
		}
		// And a refused one: the arguments of a denied call are not kept either.
		alice.call(led, e.ref("nope"), map[string]any{"text": argMarker})

		var cols []string
		rows, err := s.DB.Query(t.Context(), `select column_name from information_schema.columns where table_schema = 'gateway' and table_name = 'calls'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, c)
		}
		rows.Close()
		slices.Sort(cols)
		if want := []string{"duration_ms", "id", "outcome", "server", "started_at", "token_id", "tool", "user_id"}; !slices.Equal(cols, want) {
			t.Fatalf("gateway.calls has the columns %v, want %v", cols, want)
		}
		got := gwRows(t, s, e.slug)
		if len(got) != 3 || got[0].Tool != "echo" || got[0].Outcome != "ok" || got[0].UserID != alice.id || got[0].TokenID == nil || *got[0].TokenID != alice.tokenID ||
			got[0].Server != e.slug || got[1].Tool != "answer" || got[2].Tool != "nope" || got[2].Outcome != "denied:not_found" {
			t.Fatalf("rows %+v", got)
		}
		for _, marker := range []string{argMarker, resMarker} {
			if where := gwTablesContaining(t, s, marker); len(where) != 0 {
				t.Errorf("the marker %s of an argument or result is stored in %v", marker, where)
			}
			if strings.Contains(s.Logs(t), marker) {
				t.Errorf("the marker %s appears in the app log", marker)
			}
			for _, path := range []string{"/api/calls?server=" + e.slug, "/api/audit?limit=500"} {
				if r := admin.Do(t, "GET", path, nil); strings.Contains(string(r.Body), marker) {
					t.Errorf("GET %s returns the marker %s", path, marker)
				}
			}
		}
	})
}

// gwCallMany sends n calls at once on the person's MCP session; args(i) are the arguments of the
// i-th call. The results are in call order.
func gwCallMany(t *testing.T, p gwPerson, ref string, n int, args func(int) map[string]any) []*mcp.CallToolResult {
	t.Helper()
	out := make([]*mcp.CallToolResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], errs[i] = p.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "call_tool", Arguments: map[string]any{"tool": ref, "arguments": args(i)}})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: protocol error %v", i, err)
		}
	}
	return out
}

// ---- TestGatewayFaults: a stack of its own (small MCP body limit, triggers, a restart) ----

func TestGatewayFaults(t *testing.T) {
	const bodyLimit = 65536
	s := harness.Start(t, harness.Options{Env: map[string]string{"MCP_MAX_BODY_BYTES": fmt.Sprint(bodyLimit)}})
	admin := s.Admin(t)
	led := newMCPLedger(t, s)
	p := gwNewPerson(t, s, led)

	t.Run("MCP_MAX_BODY_BYTES sets the body limit of /mcp and of nothing else", func(t *testing.T) {
		listBody := mcpClassicBody(2, "tools/list", nil)
		pad := func(n int) string { return listBody + strings.Repeat(" ", n-len(listBody)) }
		// One byte over is 413 (the body limit is checked first). Where the request is also wrong in
		// another way (a bad token, a foreign Host or Origin) the documented order is Host, Origin,
		// token and nothing is documented about the body limit against them: refused is all that is
		// pinned. Each request goes through its own client, so a connection the server reset after
		// the refusal is never reused.
		over := func(hdr ...string) harness.Response {
			return s.Anonymous().DoExpectingEarlyRefusal(t, "POST", "/mcp", []byte(pad(bodyLimit+1)), append([]string{mcpJSON, mcpAccept}, hdr...)...)
		}
		if r := over("Authorization: Bearer " + p.token); r.Status != 413 || errorCode(t, r) != "limit_exceeded|request body is too large" {
			t.Fatalf("one byte over: %d %.200s", r.Status, r.Body)
		}
		for _, hdr := range [][]string{nil, {"Authorization: Bearer sk_" + strings.Repeat("b", 43)}, {"Host: evil.example"}, {"Origin: http://evil.example"}} {
			if r := over(hdr...); r.Status != 413 && r.Status != 403 && r.Status != 401 {
				t.Fatalf("one byte over with %v: %d %.200s, want a refusal (413, 403 or 401)", hdr, r.Status, r.Body)
			}
		}
		// Exactly the limit is accepted (a request that must succeed: plain Do, no tolerance).
		if r := s.Anonymous().Do(t, "POST", "/mcp", pad(bodyLimit), mcpJSON, mcpAccept, "Authorization: Bearer "+p.token); r.Status != 200 {
			t.Fatalf("a body of exactly the limit: %d %.200s", r.Status, r.Body)
		}
		// The REST limit stays 1 MiB: a body far above the MCP limit is fine there.
		name := `{"name":"big-body"}` + strings.Repeat(" ", 200*1024)
		if r := admin.Do(t, "POST", "/api/tokens", name, mcpJSON); r.Status != 201 {
			t.Fatalf("a 200 KiB REST body: %d %.200s", r.Status, r.Body)
		}
	})

	t.Run("an unexpected failure is Internal error. and the log keeps no arguments", func(t *testing.T) {
		e := gwNewServer(t, led, admin, nil, "none", fakes.WithDailyLimit(5))
		marker := "compat-fault-" + harness.Unique("a")
		errorsBefore := logCountFrom(t, s, "error")
		restore := gwBlockWrites(t, s, "gateway.quota")
		defer restore()
		res := p.call(led, e.ref("echo"), map[string]any{"text": marker})
		gwExpectRefused(t, "the quota table refuses writes", res, "Internal error.")
		if n := len(e.up.Calls()); n != 0 {
			t.Fatalf("the upstream was called %d times", n)
		}
		rows := gwRows(t, s, e.slug)
		if len(rows) != 1 || rows[0].Outcome != "transport_error" || rows[0].Tool != "echo" {
			t.Fatalf("call log %+v, want one transport_error row", rows)
		}
		// Log messages and levels are not part of the stable surface: all that is pinned is that the
		// failure is logged at error level or above and that no
		// line of the log carries the arguments.
		if n := logCountFrom(t, s, "error"); n <= errorsBefore {
			t.Fatalf("no error-level record for the failure (%d before, %d now)", errorsBefore, n)
		}
		if strings.Contains(s.Logs(t), marker) || strings.Contains(mustJSON(t, res), "blocked by the compat suite") {
			t.Fatal("the arguments or the failure's own message leaked")
		}
		restore()
		// Control: the same call works once the database accepts writes.
		if res := p.call(led, e.ref("echo"), map[string]any{"text": marker}); res.IsError {
			t.Fatalf("after the failure: %s", catalogText(res))
		}
		if strings.Contains(s.Logs(t), marker) {
			t.Fatal("the arguments are in the app log")
		}
	})

	t.Run("a failed call-log write never changes the agent's answer", func(t *testing.T) {
		e := gwNewServer(t, led, admin, nil, "none")
		marker := "compat-logfail-" + harness.Unique("a")
		errorsBefore := logCountFrom(t, s, "error")
		restore := gwBlockWrites(t, s, "gateway.calls")
		defer restore()
		res := p.call(led, e.ref("echo"), map[string]any{"text": marker})
		if res.IsError || catalogText(res) != fmt.Sprintf(`{"text":%q}`, marker) {
			t.Fatalf("the answer changed: isError %v %q", res.IsError, catalogText(res))
		}
		if n := len(e.up.Calls()); n != 1 {
			t.Fatalf("the upstream saw %d calls, want 1", n)
		}
		if n := len(gwRows(t, s, e.slug)); n != 0 {
			t.Fatalf("%d rows were written although the table refuses writes", n)
		}
		if n := logCountFrom(t, s, "error"); n <= errorsBefore {
			t.Fatalf("the lost log entry left no error-level record (%d before, %d now)", errorsBefore, n)
		}
		if strings.Contains(s.Logs(t), marker) {
			t.Fatal("the arguments are in the app log")
		}
		restore()
		if res := p.call(led, e.ref("echo"), map[string]any{"text": "again"}); res.IsError {
			t.Fatal(catalogText(res))
		}
		if n := len(gwRows(t, s, e.slug)); n != 1 {
			t.Fatalf("%d rows after the table accepts writes again, want 1", n)
		}
	})

	t.Run("history retention: the call log is kept 90 days, the quota counters from yesterday, pruned at start", func(t *testing.T) {
		const server = "gw-retention"
		for _, c := range []struct{ age string }{{"91 days"}, {"89 days"}} {
			execSQL(t, s, `insert into gateway.calls (user_id, server, tool, started_at, duration_ms, outcome)
				values ($1::uuid, $2, $3, now() - $4::interval, 5, 'ok')`, p.id, server, "age-"+strings.Fields(c.age)[0], c.age)
		}
		for _, d := range []int{3, 2, 1, 0} {
			execSQL(t, s, `insert into gateway.quota (user_id, server, day, count) values ($1::uuid, $2, (now() at time zone 'utc')::date - $3::int, 1)`, p.id, server, d)
		}
		s.SwapImage(t, platformImage(), "")
		harness.Eventually(t, "the history older than the retention to be pruned at start", func() bool {
			return countSQL(t, s, "select count(*) from gateway.calls where server = $1", server) == 1
		})
		if got := gwRows(t, s, server); len(got) != 1 || got[0].Tool != "age-89" {
			t.Fatalf("the call log rows left: %+v, want only the one of 89 days ago", got)
		}
		harness.Eventually(t, "the quota counters before yesterday to be pruned", func() bool {
			return countSQL(t, s, "select count(*) from gateway.quota where server = $1", server) == 2
		})
		if n := countSQL(t, s, "select count(*) from gateway.quota where server = $1 and day >= (now() at time zone 'utc')::date - 1", server); n != 2 {
			t.Fatalf("the counters left must be yesterday's and today's, %d of them are", n)
		}
	})
}
