//go:build compat

package compat_test

// The four meta-tools of /mcp through the Go SDK client: list_servers, search_tools, describe_tool
// and the round trip to call_tool, ToolRef, the short description and the leak scan. The protocol,
// the guards and the surface golden are in mcp_protocol_test.go; call_tool in gateway_test.go.
// Source: mcp.int.test.ts. Every session is opened through the ledger (mcp_helpers_test.go), which
// scans the raw bytes of every answer for upstream addresses, auth settings, tokens and secrets.

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// mcpLong is a description well over 200 characters.
const mcpLong = "Create an issue in a YouTrack project. " +
	"The summary is required; the description supports Markdown, mentions and attachments that " +
	"were uploaded before. Custom fields are set by name, for example Priority, Type or Assignee, " +
	"and unknown names are rejected by the tracker."

func TestMCPMetaTools(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	led := newMCPLedger(t, s)

	const apiKey = "mcp-gw-secret-5b1f" //nolint:gosec // G101: a made-up secret that the test hunts for
	led.note(apiKey)

	// The annotations as the upstream sent them. The Go SDK of the fake always writes
	// idempotentHint (false unless set); the gateway passes the booleans on as they are.
	createAnnotations := map[string]bool{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false}
	createSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"project": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string"}},
		"required":   []string{"project", "summary"},
	}
	// A server that needs nothing: its tools exercise describe, ToolRef and the answer shape.
	plain := gwNewServer(t, led, admin, []fakes.Tool{
		{Name: "get_file_contents", Description: "Read a file from a mcpplain repository", InputSchema: map[string]any{"type": "object"}},
		{Name: "a.b.c", Description: "A mcpdotted tool whose own name has dots", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "number"}}}},
	}, "none")
	// A shared-secret server: the secret and the header must never appear in an answer.
	shared := gwNewServer(t, led, admin, fakes.Tools("ping"), "shared_secret", fakes.WithHeader("X-API-Key", "key {secret}"), fakes.WithSecret(apiKey))
	// A personal-token server for the group "mcp-users" with the described tool.
	personal := gwNewServer(t, led, admin, []fakes.Tool{
		{
			Name: "create_issue", Title: "Create issue", Description: mcpLong, InputSchema: createSchema,
			OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
			Annotations:  map[string]any{"readOnlyHint": false, "destructiveHint": false},
		},
		{
			Name: "search_issues", Description: "Find YouTrack issues with a mcpquery", InputSchema: map[string]any{"type": "object"},
			Annotations: map[string]any{"readOnlyHint": true},
		},
	}, "pat_user", fakes.WithHeader("Authorization", "Bearer {secret}"), fakes.WithGroups("mcp-users"))
	closed := gwNewServer(t, led, admin, fakes.Tools("hidden_tool"), "none", fakes.WithGroups("ops"))
	// A server whose only tool has nothing to do with the query of the empty-answer check.
	unrelated := gwNewServer(t, led, admin, []fakes.Tool{{Name: "read_file", Description: "Read the contents of a file from disk", InputSchema: map[string]any{"type": "object"}}}, "none")
	for _, e := range []*gwServer{plain, shared, personal, unrelated} {
		catalogWaitDocs(t, s, e.slug, len(e.tools))
	}

	alice := gwNewPerson(t, s, led)
	carol := gwNewPerson(t, s, led)
	adminMCP := led.session(admin.Token(t, "meta-admin"))
	connectURL := s.BaseURL + "/connections"
	if err := savePAT(t, alice.c, personal.slug, "alice-pat"); err != "" {
		t.Fatal(err)
	}
	if err := savePAT(t, carol.c, personal.slug, "carol-pat"); err != "" {
		t.Fatal(err)
	}
	led.note("alice-pat", "carol-pat")
	dave := gwNewPerson(t, s, led) // no PAT

	listed := func(c *mcp.ClientSession) map[string]map[string]any {
		var out struct {
			Servers []map[string]any `json:"servers"`
		}
		catalogStructured(t, led.call(c, "list_servers", map[string]any{}), &out)
		bySlug := map[string]map[string]any{}
		for _, sv := range out.Servers {
			bySlug[sv["slug"].(string)] = sv
		}
		return bySlug
	}

	t.Run("list_servers: the servers the person may use, with tool counts and connection state", func(t *testing.T) {
		got := listed(alice.mcp)
		want := func(e *gwServer, tools int, connection string, extra map[string]any) map[string]any {
			m := map[string]any{"slug": e.slug, "name": e.name, "description": "Compat test server " + e.slug, "tools": float64(tools), "connection": connection}
			for k, v := range extra {
				m[k] = v
			}
			return m
		}
		for _, c := range []struct {
			e    *gwServer
			want map[string]any
		}{
			{plain, want(plain, 2, "not_required", nil)},
			{shared, want(shared, 1, "not_required", nil)},
			{personal, want(personal, 2, "ready", nil)},
		} {
			if !reflect.DeepEqual(got[c.e.slug], c.want) {
				t.Errorf("list_servers entry of %s:\n got %v\nwant %v", c.e.slug, got[c.e.slug], c.want)
			}
		}
		if _, ok := got[closed.slug]; ok {
			t.Errorf("a member's list_servers shows the server closed to them: %v", got[closed.slug])
		}
		// Control: the admin sees the closed server.
		if _, ok := listed(adminMCP)[closed.slug]; !ok {
			t.Error("the admin's list_servers misses the closed server")
		}
		// Without a PAT the person is told where to connect; with one they are not.
		if d := listed(dave.mcp)[personal.slug]; !reflect.DeepEqual(d, want(personal, 2, "needs_connection", map[string]any{"connectUrl": connectURL})) {
			t.Errorf("a person without a token: %v", d)
		}
	})

	t.Run("list_servers: an upstream that rejects the token turns the state to expired, with the link", func(t *testing.T) {
		personal.up.FailWith(401)
		defer personal.up.Reset()
		res := led.call(carol.mcp, "call_tool", map[string]any{"tool": personal.slug + ".search_issues", "arguments": map[string]any{}})
		if !res.IsError {
			t.Fatalf("a rejected token: %s", catalogText(res))
		}
		personal.up.Reset()
		if got := listed(carol.mcp)[personal.slug]; got["connection"] != "expired" || got["connectUrl"] != connectURL {
			t.Fatalf("after the rejection: %v", got)
		}
		// Control: alice, whose token was never rejected, is still ready and has no link.
		if got := listed(alice.mcp)[personal.slug]; got["connection"] != "ready" || got["connectUrl"] != nil {
			t.Fatalf("alice: %v", got)
		}
	})

	t.Run("search, describe and call: the round trip with the token of this very person", func(t *testing.T) {
		ref := personal.slug + ".create_issue"
		hits := catalogSearch(t, alice.mcp, "create an issue", 10, []string{personal.slug})
		var hit *catalogHit
		for i := range hits {
			if hits[i].Tool == ref {
				hit = &hits[i]
			}
		}
		if hit == nil {
			t.Fatalf("search_tools does not find %s: %v", ref, catalogHitRefs(hits))
		}
		if hit.Title == nil || *hit.Title != "Create issue" || hit.Server != personal.slug || hit.Connection != "ready" ||
			!reflect.DeepEqual(hit.Annotations, createAnnotations) || hit.Score <= 0 || hit.Score > 1 {
			t.Errorf("hit: %+v", hit)
		}
		// The short description: whitespace collapsed, cut at a word to 200 characters, ending with an ellipsis.
		const wantShort = "Create an issue in a YouTrack project. The summary is required; the description supports Markdown, mentions and attachments that were uploaded before. Custom fields are set by name, for example…"
		if hit.Description != wantShort {
			t.Errorf("short description %q, want %q", hit.Description, wantShort)
		}
		if n := len([]rune(hit.Description)); n > 200 || !strings.HasSuffix(hit.Description, "…") || !strings.HasPrefix(mcpLong, strings.TrimSuffix(hit.Description, "…")) {
			t.Errorf("short description of %d characters %q", n, hit.Description)
		}

		var desc catalogDescribed
		catalogStructured(t, led.call(alice.mcp, "describe_tool", map[string]any{"tool": ref}), &desc)
		wantDesc := catalogDescribed{
			Tool: ref, Title: ptr("Create issue"), Description: mcpLong,
			InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"project": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string"}}, "required": []any{"project", "summary"}},
			OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
			Annotations:  createAnnotations,
			Server:       personal.slug, Connection: "ready",
		}
		if !reflect.DeepEqual(desc, wantDesc) {
			t.Errorf("describe_tool:\n got %+v\nwant %+v", desc, wantDesc)
		}

		args := map[string]any{"project": "OPS", "summary": "Disk is full"}
		personal.up.Reset()
		res := led.call(alice.mcp, "call_tool", map[string]any{"tool": ref, "arguments": args})
		if res.IsError || catalogText(res) != `{"project":"OPS","summary":"Disk is full"}` {
			t.Fatalf("call_tool: isError %v %q", res.IsError, catalogText(res))
		}
		if calls := personal.up.Calls(); len(calls) != 1 || calls[0].Tool != "create_issue" || calls[0].Header.Get("Authorization") != "Bearer alice-pat" {
			t.Fatalf("the upstream saw %+v, want one call with Alice's token", calls)
		}
		// Another person's call reaches the upstream with that person's credentials.
		personal.up.Reset()
		gwExpectRefused(t, "a person without a token", led.call(dave.mcp, "call_tool", map[string]any{"tool": ref, "arguments": args}), connTextNotConnected(s, personal.slug))
		if n := len(personal.up.Calls()); n != 0 {
			t.Fatalf("a person without a token reached the upstream %d times", n)
		}
		// A person whose token the upstream rejects is told to reconnect, by the server's name; after
		// reconnecting the call reaches the upstream with the new token. The state is set up here, not
		// inherited from another subtest.
		erin := gwNewPerson(t, s, led)
		if msg := savePAT(t, erin.c, personal.slug, "erin-pat"); msg != "" {
			t.Fatal(msg)
		}
		led.note("erin-pat", "erin-pat-2")
		personal.up.FailWith(401)
		gwExpectRefused(t, "a rejected token", erin.call(led, ref, args), connTextExpired(s, personal.slug))
		personal.up.Reset()
		gwExpectRefused(t, "the token stays expired", erin.call(led, ref, args), connTextExpired(s, personal.slug))
		if n := len(personal.up.Calls()); n != 0 {
			t.Fatalf("an expired connection reached the upstream %d times", n)
		}
		if msg := savePAT(t, erin.c, personal.slug, "erin-pat-2"); msg != "" {
			t.Fatal(msg)
		}
		if res := erin.call(led, ref, args); res.IsError {
			t.Fatalf("after reconnecting: %q", catalogText(res))
		}
		if calls := personal.up.Calls(); len(calls) != 1 || calls[0].Header.Get("Authorization") != "Bearer erin-pat-2" {
			t.Fatalf("the call reached the upstream with %v, want the new token", calls)
		}
	})

	t.Run("describe_tool: no outputSchema key and empty annotations when the upstream gave none, a null title", func(t *testing.T) {
		res := led.call(alice.mcp, "describe_tool", map[string]any{"tool": plain.slug + ".get_file_contents"})
		if res.IsError {
			t.Fatal(catalogText(res))
		}
		want := map[string]any{
			"tool": plain.slug + ".get_file_contents", "title": nil, "description": "Read a file from a mcpplain repository",
			"inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{}, "server": plain.slug, "connection": "not_required",
		}
		var got map[string]any
		catalogStructured(t, res, &got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("describe_tool:\n got %v\nwant %v", got, want)
		}
		if text := catalogText(res); !strings.Contains(text, `"title":null`) {
			t.Errorf("the text content is not the JSON of the structured content: %s", text)
		}
	})

	t.Run("a tool whose own name has dots is addressed after the first dot", func(t *testing.T) {
		ref := plain.slug + ".a.b.c"
		hits := catalogSearch(t, alice.mcp, "mcpdotted", 10, []string{plain.slug})
		if !slices.Contains(catalogHitRefs(hits), ref) {
			t.Fatalf("search does not find %s: %v", ref, catalogHitRefs(hits))
		}
		var desc catalogDescribed
		catalogStructured(t, led.call(alice.mcp, "describe_tool", map[string]any{"tool": ref}), &desc)
		if desc.Tool != ref || desc.Server != plain.slug {
			t.Fatalf("describe %s: %+v", ref, desc)
		}
		plain.up.Reset()
		res := led.call(alice.mcp, "call_tool", map[string]any{"tool": ref, "arguments": map[string]any{"x": 1}})
		if res.IsError || catalogText(res) != `{"x":1}` {
			t.Fatalf("call %s: %v %q", ref, res.IsError, catalogText(res))
		}
		if calls := plain.up.Calls(); len(calls) != 1 || calls[0].Tool != "a.b.c" {
			t.Fatalf("the upstream saw %+v, want one call of the tool named a.b.c", calls)
		}
		// Negative controls: the prefixes of the name are not tools.
		for _, bad := range []string{plain.slug + ".a", plain.slug + ".a.b", plain.slug + ".a.b.c.d"} {
			if r := led.call(alice.mcp, "call_tool", map[string]any{"tool": bad, "arguments": map[string]any{}}); !r.IsError || catalogText(r) != catalogNotAvailable(bad) {
				t.Errorf("%s: %q", bad, catalogText(r))
			}
		}
		if rows := gwRows(t, s, plain.slug); rows[len(rows)-4].Tool != "a.b.c" || rows[len(rows)-4].Outcome != "ok" {
			t.Errorf("the call log keeps the tool as %+v", rows[len(rows)-4])
		}
	})

	t.Run("search_tools: limit, the servers filter, unknown servers and an empty answer", func(t *testing.T) {
		if got := catalogSearch(t, alice.mcp, "mcpplain mcpdotted", 1, []string{plain.slug}); len(got) != 1 {
			t.Fatalf("limit 1: %v", catalogHitRefs(got))
		}
		both := catalogSearch(t, alice.mcp, "mcpplain mcpdotted", 25, []string{plain.slug})
		if len(both) != 2 {
			t.Fatalf("both tools of the server expected: %v", catalogHitRefs(both))
		}
		for i := 1; i < len(both); i++ {
			if both[i].Score > both[i-1].Score {
				t.Errorf("hits are not best first: %v", both)
			}
		}
		for _, h := range both {
			if h.Server != plain.slug {
				t.Errorf("the filter let %s through", h.Server)
			}
		}
		if got := catalogSearch(t, alice.mcp, "mcpplain", 25, []string{shared.slug}); slices.Contains(catalogHitRefs(got), plain.slug+".get_file_contents") {
			t.Errorf("a search in %s finds a tool of %s", shared.slug, plain.slug)
		}
		res := led.call(alice.mcp, "search_tools", map[string]any{"query": "issue", "servers": []string{"jira"}})
		if !res.IsError || catalogText(res) != `Unknown server "jira". Call list_servers.` {
			t.Errorf("an unknown server: %q", catalogText(res))
		}
		res = led.call(alice.mcp, "search_tools", map[string]any{"query": "issue", "servers": []string{closed.slug}})
		if !res.IsError || catalogText(res) != fmt.Sprintf("Unknown server %q. Call list_servers.", closed.slug) {
			t.Errorf("a closed server: %q", catalogText(res))
		}
		var empty struct {
			Results []catalogHit `json:"results"`
		}
		catalogStructured(t, led.call(alice.mcp, "search_tools", map[string]any{"query": "zzzqqq", "servers": []string{unrelated.slug}}), &empty)
		if empty.Results == nil || len(empty.Results) != 0 {
			t.Errorf("a query nothing matches: %v, want an empty list", empty.Results)
		}
	})

	t.Run("search_tools: the short description is cut at a word to 200 characters", func(t *testing.T) {
		words := func(n int) string { // "w000 w001 ..." of at least n characters, no trailing space
			var b strings.Builder
			for i := 0; b.Len() < n; i++ {
				fmt.Fprintf(&b, "w%03d ", i)
			}
			return strings.TrimSpace(b.String())
		}
		cases := []struct {
			tool, desc string
			check      func(t *testing.T, got string)
		}{
			{"exact", "mcpshort1 " + strings.Repeat("e", 190), func(t *testing.T, got string) {
				if got != "mcpshort1 "+strings.Repeat("e", 190) {
					t.Errorf("a description of exactly 200 characters is cut: %q", got)
				}
			}},
			{"over", "mcpshort2 " + words(400), func(t *testing.T, got string) {
				if !strings.HasSuffix(got, "…") || len([]rune(got)) > 200 || strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") {
					t.Errorf("a long description: %q", got)
				}
				// Cut at a word: the text before the ellipsis is whole words of the original.
				body := strings.TrimSuffix(got, "…")
				original := "mcpshort2 " + words(400)
				if !strings.HasPrefix(original, body) || (original[len(body)] != ' ') {
					t.Errorf("not cut at a word boundary: %q", got)
				}
			}},
			{"oneword", "mcpshort3" + strings.Repeat("z", 400), func(t *testing.T, got string) {
				if want := "mcpshort3" + strings.Repeat("z", 190) + "…"; got != want {
					t.Errorf("a single word longer than the limit: %d characters %q", len([]rune(got)), got)
				}
			}},
			{"spaces", "mcpshort4   a \n\n b\t c  ", func(t *testing.T, got string) {
				if got != "mcpshort4 a b c" {
					t.Errorf("whitespace: %q", got)
				}
			}},
			{"emoji", "mcpshort5" + strings.Repeat("😀", 250), func(t *testing.T, got string) {
				want := "mcpshort5" + strings.Repeat("😀", 190) + "…"
				if got != want || strings.ContainsRune(got, '\uFFFD') {
					t.Errorf("code points are counted, a surrogate pair is never split: %d runes %q", len([]rune(got)), got)
				}
			}},
			{"punctuation", "mcpshort6 " + strings.Repeat("abcdefgh, ", 30), func(t *testing.T, got string) {
				if !strings.HasSuffix(got, "abcdefgh…") {
					t.Errorf("trailing punctuation is dropped before the ellipsis: %q", got)
				}
			}},
		}
		tools := make([]fakes.Tool, 0, len(cases))
		for _, c := range cases {
			tools = append(tools, fakes.Tool{Name: "short_" + c.tool, Description: c.desc, InputSchema: map[string]any{"type": "object"}})
		}
		desc := gwNewServer(t, led, admin, tools, "none")
		catalogWaitDocs(t, s, desc.slug, len(tools))
		for i, c := range cases {
			hits := catalogSearch(t, alice.mcp, fmt.Sprintf("mcpshort%d", i+1), 25, []string{desc.slug})
			var got *catalogHit
			for j := range hits {
				if hits[j].Tool == desc.slug+".short_"+c.tool {
					got = &hits[j]
				}
			}
			if got == nil {
				t.Errorf("%s: not found: %v", c.tool, catalogHitRefs(hits))
				continue
			}
			c.check(t, got.Description)
			// describe_tool returns the text as the upstream gave it (not collapsed, not cut).
			var d catalogDescribed
			catalogStructured(t, led.call(alice.mcp, "describe_tool", map[string]any{"tool": got.Tool}), &d)
			if d.Description != c.desc {
				t.Errorf("%s: describe_tool changed the description: %q", c.tool, d.Description)
			}
		}
	})

	t.Run("a closed server is as unknown as one that does not exist", func(t *testing.T) {
		// The full matrix (hidden, removed, disabled, closed) is in search_test.go and gateway_test.go.
		ref := closed.slug + ".hidden_tool"
		for _, name := range []string{"describe_tool", "call_tool"} {
			if res := led.call(alice.mcp, name, map[string]any{"tool": ref}); !res.IsError || catalogText(res) != catalogNotAvailable(ref) {
				t.Errorf("%s: %q", name, catalogText(res))
			}
		}
		if n := len(closed.up.Calls()); n != 0 {
			t.Errorf("the upstream of a closed server was called %d times", n)
		}
		// Control: the admin calls it.
		if res := led.call(adminMCP, "call_tool", map[string]any{"tool": ref, "arguments": map[string]any{}}); res.IsError {
			t.Errorf("the admin: %q", catalogText(res))
		}
	})

	t.Run("the shared secret reaches the upstream and nothing else", func(t *testing.T) {
		shared.up.Reset()
		res := led.call(alice.mcp, "call_tool", map[string]any{"tool": shared.slug + ".ping", "arguments": map[string]any{}})
		if res.IsError {
			t.Fatal(catalogText(res))
		}
		if got := connAuths(shared.up, "X-API-Key"); len(got) != 1 || got[0] != "key "+apiKey {
			t.Fatalf("the upstream saw X-API-Key %v, want one call with the key", redactAll(got))
		}
		// The ledger scan at the end of the test finds no secret, upstream address or header in any answer.
	})
}
