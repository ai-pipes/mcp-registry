package http_test

// The four meta-tools, transcribed from compat TestMCPMetaTools (compat/mcp_tools_test.go) over
// fakes of the catalog, connections, search and the gateway use case, plus the shapes of the
// inventory (5.4) and the shortDescription cases of the reference (v1.0.1:apps/server/src/platform/mcp/text.ts and compat).

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway"
	gwhttp "github.com/ai-pipes/mcp-registry/internal/modules/gateway/http"
	"github.com/ai-pipes/mcp-registry/internal/modules/search"
)

const longDescription = "Create an issue in a YouTrack project. " +
	"The summary is required; the description supports Markdown, mentions and attachments that " +
	"were uploaded before. Custom fields are set by name, for example Priority, Type or Assignee, " +
	"and unknown names are rejected by the tracker."

func b(v bool) *bool { return &v }

// metaEnv is the cast of TestMCPMetaTools: plain (none), shared (shared_secret), personal
// (pat_user, alice ready, carol expired, dave nothing), closed (none, closed to the members).
func metaEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	title := "Create issue"
	e.cat.servers = []catalog.MemberServerView{
		{Slug: "closed", Name: "Closed", Description: "Compat test server closed", ToolCount: 1, AuthKind: catalog.AuthNone},
		{Slug: "personal", Name: "Personal Tracker", Description: "Compat test server personal", ToolCount: 2, AuthKind: catalog.AuthPATUser},
		{Slug: "plain", Name: "Plain", Description: "Compat test server plain", ToolCount: 2, AuthKind: catalog.AuthNone},
		{Slug: "shared", Name: "Shared", Description: "Compat test server shared", ToolCount: 1, AuthKind: catalog.AuthSharedSecret},
	}
	for _, tv := range []catalog.ToolView{
		{Server: "plain", Name: "get_file_contents", Ref: "plain.get_file_contents", Description: "Read a file from a mcpplain repository", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Server: "plain", Name: "a.b.c", Ref: "plain.a.b.c", Description: "A mcpdotted tool whose own name has dots", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"}}}`)},
		{Server: "shared", Name: "ping", Ref: "shared.ping", Description: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{
			Server: "personal", Name: "create_issue", Ref: "personal.create_issue", Title: &title, Description: longDescription,
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"project":{"type":"string"},"summary":{"type":"string"}},"required":["project","summary"]}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
			Annotations:  catalog.ToolAnnotations{ReadOnlyHint: b(false), DestructiveHint: b(false), IdempotentHint: b(false)},
		},
		{Server: "personal", Name: "search_issues", Ref: "personal.search_issues", Description: "Find YouTrack issues with a mcpquery", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: catalog.ToolAnnotations{ReadOnlyHint: b(true)}},
		{Server: "closed", Name: "hidden_tool", Ref: "closed.hidden_tool", Description: "closed", InputSchema: json.RawMessage(`{"type":"object"}`)},
	} {
		e.cat.tools[tv.Ref] = tv
	}
	e.cat.closed["alice"] = map[string]bool{"closed": true}
	e.cat.closed["carol"] = map[string]bool{"closed": true}
	e.con.byUser["alice"] = []connections.ConnectionSummary{{Server: "personal", Name: "Personal Tracker", AuthKind: catalog.AuthPATUser, Status: connections.SummaryReady}}
	e.con.byUser["carol"] = []connections.ConnectionSummary{{Server: "personal", Name: "Personal Tracker", AuthKind: catalog.AuthPATUser, Status: connections.SummaryExpired}}
	e.con.byUser["root"] = []connections.ConnectionSummary{{Server: "personal", Name: "Personal Tracker", AuthKind: catalog.AuthPATUser, Status: connections.SummaryMissing}}
	return e
}

func structured(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	if res["isError"] != nil {
		t.Fatalf("an error: %v", res)
	}
	var fromText map[string]any
	if err := json.Unmarshal([]byte(resultText(res)), &fromText); err != nil || !reflect.DeepEqual(fromText, res["structuredContent"]) {
		t.Fatalf("the text is not the JSON of structuredContent: %q %v", resultText(res), res["structuredContent"])
	}
	return fromText
}

func listed(t *testing.T, e *env, token string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	servers, _ := structured(t, e.toolsCall(t, token, "list_servers", map[string]any{}))["servers"].([]any)
	for _, s := range servers {
		m := s.(map[string]any)
		out[m["slug"].(string)] = m
	}
	return out
}

func TestListServersShowsUsableServersWithToolCountsAndConnectionState(t *testing.T) {
	e := metaEnv(t)
	want := func(slug, name string, tools int, connection string, link bool) map[string]any {
		m := map[string]any{"slug": slug, "name": name, "description": "Compat test server " + slug, "tools": float64(tools), "connection": connection}
		if link {
			m["connectUrl"] = e.publicURL + "/connections"
		}
		return m
	}
	alice := listed(t, e, aliceToken)
	for slug, w := range map[string]map[string]any{
		"plain":    want("plain", "Plain", 2, "not_required", false),
		"shared":   want("shared", "Shared", 1, "not_required", false),
		"personal": want("personal", "Personal Tracker", 2, "ready", false),
	} {
		if !reflect.DeepEqual(alice[slug], w) {
			t.Errorf("%s:\n got %v\nwant %v", slug, alice[slug], w)
		}
	}
	if _, ok := alice["closed"]; ok || len(alice) != 3 {
		t.Errorf("a member sees %v", alice)
	}
	if got := listed(t, e, carolToken)["personal"]; !reflect.DeepEqual(got, want("personal", "Personal Tracker", 2, "expired", true)) {
		t.Errorf("an expired connection: %v", got)
	}
	root := listed(t, e, adminToken)
	if _, ok := root["closed"]; !ok {
		t.Error("the admin misses the closed server")
	}
	if !reflect.DeepEqual(root["personal"], want("personal", "Personal Tracker", 2, "needs_connection", true)) {
		t.Errorf("a missing connection: %v", root["personal"])
	}
	// The catalog's order is kept, the list is [] when empty, and the raw text is compact JSON.
	res := e.toolsCall(t, aliceToken, "list_servers", map[string]any{})
	if text := resultText(res); !strings.HasPrefix(text, `{"servers":[{"slug":"personal","name":"Personal Tracker",`) {
		t.Errorf("text %s", text)
	}
	e.cat.servers = nil
	if text := resultText(e.toolsCall(t, aliceToken, "list_servers", map[string]any{})); text != `{"servers":[]}` {
		t.Errorf("no servers: %s", text)
	}
}

func TestSearchToolsSearchesUsableServersAndRechecksEveryHit(t *testing.T) {
	e := metaEnv(t)
	e.search.hits = []search.ToolHit{
		{Ref: "personal.create_issue", Server: "personal", Score: 0.9},
		{Ref: "plain.a.b.c", Server: "plain", Score: 0.875},
		{Ref: "plain.gone", Server: "plain", Score: 0.8}, // the index lags the catalog
		{Ref: "plain.get_file_contents", Server: "plain", Score: 0.5},
	}
	res := structured(t, e.toolsCall(t, aliceToken, "search_tools", map[string]any{"query": "create an issue", "limit": 3}))
	results, _ := res["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results %v: the limit applies before the re-check", results)
	}
	want := map[string]any{
		"tool": "personal.create_issue", "title": "Create issue",
		"description": "Create an issue in a YouTrack project. The summary is required; the description supports Markdown, mentions and attachments that were uploaded before. Custom fields are set by name, for example…",
		"server":      "personal", "score": 0.9,
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false},
		"connection":  "ready",
	}
	if !reflect.DeepEqual(results[0], want) {
		t.Errorf("hit:\n got %v\nwant %v", results[0], want)
	}
	if second := results[1].(map[string]any); second["tool"] != "plain.a.b.c" || second["title"] != nil || second["connection"] != "not_required" || !reflect.DeepEqual(second["annotations"], map[string]any{}) {
		t.Errorf("second hit %v", second)
	}
	// Without servers every usable server is searched, in the catalog's order; the query is passed as is.
	if c := e.search.calls[0]; c.query != "create an issue" || c.limit != 3 || !reflect.DeepEqual(c.servers, []string{"personal", "plain", "shared"}) {
		t.Errorf("search call %+v", c)
	}
	// The default limit is 10; the servers filter is de-duplicated in its own order.
	structured(t, e.toolsCall(t, aliceToken, "search_tools", map[string]any{"query": "q", "servers": []string{"shared", "plain", "shared"}}))
	if c := e.search.calls[1]; c.limit != 10 || !reflect.DeepEqual(c.servers, []string{"shared", "plain"}) {
		t.Errorf("search call %+v", c)
	}
	// An unknown or closed server is refused with the first unknown slug, raw.
	for _, c := range []struct {
		servers []string
		text    string
	}{
		{[]string{"plain", "jira", "x"}, `Unknown server "jira". Call list_servers.`},
		{[]string{"closed"}, `Unknown server "closed". Call list_servers.`},
		{[]string{`a"b`}, `Unknown server "a"b". Call list_servers.`},
	} {
		res := e.toolsCall(t, aliceToken, "search_tools", map[string]any{"query": "issue", "servers": c.servers})
		if res["isError"] != true || resultText(res) != c.text {
			t.Errorf("%v: %v", c.servers, res)
		}
	}
	if len(e.search.calls) != 2 {
		t.Fatalf("a refused filter reached the index: %v", e.search.calls)
	}
	// Nothing found is an empty list.
	e.search.hits = nil
	if text := resultText(e.toolsCall(t, aliceToken, "search_tools", map[string]any{"query": "zzzqqq"})); text != `{"results":[]}` {
		t.Errorf("no hits: %s", text)
	}
}

func TestDescribeToolGivesTheFullDescriptionAndSchemas(t *testing.T) {
	e := metaEnv(t)
	got := structured(t, e.toolsCall(t, aliceToken, "describe_tool", map[string]any{"tool": "personal.create_issue"}))
	want := map[string]any{
		"tool": "personal.create_issue", "title": "Create issue", "description": longDescription,
		"inputSchema":  map[string]any{"type": "object", "properties": map[string]any{"project": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string"}}, "required": []any{"project", "summary"}},
		"outputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
		"annotations":  map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false},
		"server":       "personal", "connection": "ready",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("describe:\n got %v\nwant %v", got, want)
	}
	res := e.toolsCall(t, aliceToken, "describe_tool", map[string]any{"tool": "plain.get_file_contents"})
	got = structured(t, res)
	want = map[string]any{
		"tool": "plain.get_file_contents", "title": nil, "description": "Read a file from a mcpplain repository",
		"inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{}, "server": "plain", "connection": "not_required",
	}
	if !reflect.DeepEqual(got, want) || !strings.Contains(resultText(res), `"title":null`) {
		t.Errorf("describe without extras: %v / %s", got, resultText(res))
	}
	// A closed, unknown or malformed tool is the not_found text, and the servers are not read.
	reads := e.cat.reads
	for _, ref := range []string{"closed.hidden_tool", "plain.a", "plain.a.b.c.d", "nodot", "Bad.x"} {
		res := e.toolsCall(t, aliceToken, "describe_tool", map[string]any{"tool": ref})
		if res["isError"] != true || resultText(res) != `Tool "`+ref+`" is not available. Use search_tools to find another.` {
			t.Errorf("%s: %v", ref, res)
		}
	}
	if e.cat.reads != reads {
		t.Error("describe_tool read the servers for a tool it does not offer")
	}
	if res := e.toolsCall(t, adminToken, "describe_tool", map[string]any{"tool": "closed.hidden_tool"}); res["isError"] != nil {
		t.Errorf("the admin: %v", res)
	}
}

func TestCallToolHandsTheAgentsBytesToTheGatewayAndItsResultBack(t *testing.T) {
	e := metaEnv(t)
	isErr := false
	e.gw.result = gateway.ToolResult{
		Content:           json.RawMessage(`[{"type":"text","text":"<b>&amp;\u2028</b>"},{"type":"image","data":"AAAA","mimeType":"image/png","annotations":{"audience":["user"]}}]`),
		StructuredContent: json.RawMessage(`{"a":"<x>","n":1.0}`),
		IsError:           &isErr,
	}
	raw := `{"tool":"personal.create_issue","arguments":{"summary":"<Disk> & full","n":1.0,"big":12345678901234567890}}`
	r := e.post(t, aliceToken, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"call_tool","arguments":`+raw+`}}`)
	if r.status != 200 || r.msg.Result == nil {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if len(e.gw.calls) != 1 {
		t.Fatalf("calls %v", e.gw.calls)
	}
	c := e.gw.calls[0]
	if c.Ref != "personal.create_issue" || string(c.Arguments) != `{"summary":"<Disk> & full","n":1.0,"big":12345678901234567890}` ||
		c.Principal.UserID != "alice" || c.Principal.TokenID != "tok-alice" {
		t.Fatalf("the gateway got %+v %s", c, c.Arguments)
	}
	// The result goes out byte for byte (no HTML escaping), the U+2028 escape as the upstream wrote it.
	if !strings.Contains(string(r.body), `"result":{"content":[{"type":"text","text":"<b>&amp;\u2028</b>"},{"type":"image","data":"AAAA","mimeType":"image/png","annotations":{"audience":["user"]}}],"structuredContent":{"a":"<x>","n":1.0},"isError":false}`) {
		t.Fatalf("body %s", r.body)
	}
	// Omitted arguments are none (the gateway reads an empty value as {}).
	e.toolsCall(t, aliceToken, "call_tool", map[string]any{"tool": "plain.a.b.c"})
	if got := e.gw.calls[1]; got.Ref != "plain.a.b.c" || len(got.Arguments) != 0 {
		t.Fatalf("omitted arguments: %+v %q", got, got.Arguments)
	}
}

func TestAnUnexpectedFailureIsInternalErrorAndLogsNoArguments(t *testing.T) {
	e := metaEnv(t)
	e.gw.err = errors.New("pq: secret-marker in the failure text")
	res := e.toolsCall(t, aliceToken, "call_tool", map[string]any{"tool": "plain.a.b.c", "arguments": map[string]any{"x": "arg-marker"}})
	if res["isError"] != true || resultText(res) != "Internal error." {
		t.Fatalf("%v", res)
	}
	e.cat.listErr = errors.New("db down secret-marker")
	for _, c := range []struct {
		tool string
		args map[string]any
	}{{"list_servers", map[string]any{}}, {"search_tools", map[string]any{"query": "arg-marker"}}} {
		if res := e.toolsCall(t, aliceToken, c.tool, c.args); res["isError"] != true || resultText(res) != "Internal error." {
			t.Errorf("%s: %v", c.tool, res)
		}
	}
	e.cat.listErr, e.cat.toolErr = nil, errors.New("db down secret-marker")
	if res := e.toolsCall(t, aliceToken, "describe_tool", map[string]any{"tool": "plain.a.b.c"}); resultText(res) != "Internal error." {
		t.Errorf("describe: %v", res)
	}
	e.cat.toolErr = nil
	e.search.err = errors.New("fts secret-marker")
	if res := e.toolsCall(t, aliceToken, "search_tools", map[string]any{"query": "x"}); resultText(res) != "Internal error." {
		t.Errorf("search: %v", res)
	}
	logs := e.logText()
	if strings.Count(logs, `"msg":"mcp tool failed"`) != 5 || strings.Contains(logs, "marker") || !strings.Contains(logs, `"tool":"call_tool"`) {
		t.Fatalf("logs %s", logs)
	}
}

func TestStructuredTextIsJavaScriptsJSON(t *testing.T) {
	e := metaEnv(t)
	e.cat.servers = append(e.cat.servers,
		catalog.MemberServerView{Slug: "odd", Name: "A <b> & \u2028 \u2029 \"q\"", AuthKind: catalog.AuthNone},
		catalog.MemberServerView{Slug: "slash", Name: `back\u2028slash`, AuthKind: catalog.AuthNone})
	res := e.toolsCall(t, aliceToken, "list_servers", map[string]any{})
	text := resultText(res)
	if !strings.Contains(text, "\"name\":\"A <b> & \u2028 \u2029 \\\"q\\\"\"") || !strings.Contains(text, `"name":"back\\u2028slash"`) {
		t.Fatalf("text %q", text)
	}
	if got := listed(t, e, aliceToken); got["odd"]["name"] != "A <b> & \u2028 \u2029 \"q\"" || got["slash"]["name"] != `back\u2028slash` {
		t.Fatalf("names %v %v", got["odd"], got["slash"])
	}
	e.cat.servers = e.cat.servers[:len(e.cat.servers)-1]
	r := e.post(t, aliceToken, rpcBody(3, "tools/call", map[string]any{"name": "list_servers", "arguments": map[string]any{}}))
	if strings.Contains(string(r.body), `\u003c`) || strings.Contains(string(r.body), `\u0026`) || strings.Contains(string(r.body), `\u2028`) || !strings.Contains(string(r.body), "\u2028") {
		t.Fatalf("the wire escapes like Go, not like JavaScript: %s", r.body)
	}
}

func TestShortDescription(t *testing.T) {
	words := func(n int) string {
		var sb strings.Builder
		for i := 0; sb.Len() < n; i++ {
			fmt.Fprintf(&sb, "w%03d ", i)
		}
		return strings.TrimSpace(sb.String())
	}
	for _, c := range []struct{ in, want string }{
		{"short", "short"},
		{strings.Repeat("word ", 50) + "tail", strings.Repeat("word ", 39) + "word…"},
		{"a" + strings.Repeat("a", 189) + ", " + strings.Repeat("b", 30), strings.Repeat("a", 190) + "…"},
		{strings.Repeat("a", 198) + "😀😀😀", strings.Repeat("a", 198) + "😀…"},
		{strings.Repeat("b", 196) + " 😀 😀😀 tail", strings.Repeat("b", 196) + " 😀…"},
		{strings.Repeat("c", 199) + "😀", strings.Repeat("c", 199) + "😀"},
		{strings.Repeat("x", 500), strings.Repeat("x", 199) + "…"},
		{"mcpshort1 " + strings.Repeat("e", 190), "mcpshort1 " + strings.Repeat("e", 190)},
		{"mcpshort3" + strings.Repeat("z", 400), "mcpshort3" + strings.Repeat("z", 190) + "…"},
		{"mcpshort4   a \n\n b\t c  ", "mcpshort4 a b c"},
		{"mcpshort5" + strings.Repeat("😀", 250), "mcpshort5" + strings.Repeat("😀", 190) + "…"},
		{"  \u00a0lead\ufeff\u2028trail\u0085x ", "lead trail\u0085x"},
		{longDescription, "Create an issue in a YouTrack project. The summary is required; the description supports Markdown, mentions and attachments that were uploaded before. Custom fields are set by name, for example…"},
		{strings.Repeat("—", 300), strings.Repeat("—", 199) + "…"},
	} {
		if got := gwhttp.ShortDescription(c.in); got != c.want {
			t.Errorf("ShortDescription(%.40q) = %q, want %q", c.in, got, c.want)
		}
	}
	first := gwhttp.ShortDescription("First sentence,\n\n   " + strings.Repeat("second ", 40))
	if !strings.HasPrefix(first, "First sentence, second") || strings.Contains(first, "  ") || strings.Contains(first, "\n") || !strings.HasSuffix(first, "second…") {
		t.Errorf("collapse: %q", first)
	}
	got := gwhttp.ShortDescription("mcpshort6 " + strings.Repeat("abcdefgh, ", 30))
	if !strings.HasSuffix(got, "abcdefgh…") {
		t.Errorf("punctuation: %q", got)
	}
	over := "mcpshort2 " + words(400)
	got = gwhttp.ShortDescription(over)
	body := strings.TrimSuffix(got, "…")
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > 200 || !strings.HasPrefix(over, body) || over[len(body)] != ' ' {
		t.Errorf("words: %q", got)
	}
}

func TestMetaToolsThroughTheSDKClient(t *testing.T) {
	e := metaEnv(t)
	sess := sdkSession(t, e, aliceToken, nil)
	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "describe_tool", Arguments: map[string]any{"tool": "plain.get_file_contents"}})
	if err != nil || res.IsError {
		t.Fatalf("%+v %v", res, err)
	}
	sc, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(sc), `"title":null`) {
		t.Fatalf("structured %s", sc)
	}
	e.gw.result = gateway.ToolResult{Content: json.RawMessage(`[{"type":"text","text":"{\"x\":1}"}]`)}
	res, err = sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "call_tool", Arguments: map[string]any{"tool": "plain.a.b.c", "arguments": map[string]any{"x": 1}}})
	if err != nil || res.IsError || res.Content[0].(*mcp.TextContent).Text != `{"x":1}` {
		t.Fatalf("%+v %v", res, err)
	}
	if got := e.gw.calls[0]; string(got.Arguments) != `{"x":1}` {
		t.Fatalf("arguments %s", got.Arguments)
	}
}
