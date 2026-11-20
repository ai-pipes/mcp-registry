//go:build compat

package compat_test

// MCP endpoint scenarios: the protocol of /mcp as raw JSON-RPC over HTTP (the classic `initialize`
// handshake of older clients) and through the Go SDK client of Stack.MCP (which opens with
// `server/discover`, revision 2026-07-28, and works through the SDK, not by hand), the surface
// golden and the guards in front of the handler. The four meta-tools are in mcp_tools_test.go;
// call_tool in gateway_test.go; the call log and audit endpoints in gateway_rest_test.go.
// Source: mcp.int.test.ts.
//
// The MCP spec lets a server answer a POST with application/json or with text/event-stream, and
// leaves the JSON-RPC code of a transport refusal to the SDK; the scenarios therefore pin what is
// the product's or the protocol's (202 for notifications, no Mcp-Session-Id, 405 with Allow: POST
// for GET and DELETE, the negotiated revisions, -32700 / -32601 / -32602, the Origin refusal, the
// 413) and accept any spec-correct transport for the rest. The table of pinned values is in the
// task report.
//
// Already covered elsewhere (cross-references):
//   - without a token or with a bad token -> 401 `WWW-Authenticate: Bearer`, a foreign Host with no
//     token -> 403: platform_test.go (TestPlatformSecurityHeaders, TestPlatformBearerNeverOpensTheWebAPI),
//     identity_test.go (rate limits of bad tokens);
//   - a disabled person's token and a revoked token -> 401: identity_test.go (TestIdentityNoOIDC,
//     "disabling a person ends their sessions, tokens and links", "revoking a token");
//   - hidden, removed, disabled and closed tools -> one not_found text for search, describe and
//     call: search_test.go (TestCatalogSearch) and, with the call log, gateway_test.go;
//   - upstream failure texts (401, 500, timeout, size, redirect): catalog_test.go
//     (TestCatalogServers, "upstream calls"); the credential rules and upstream 401 handling:
//     connections_*_test.go.
//
// Source cases that are not ported:
//   - "/mcp takes bodies above the 1 MiB REST limit ... (M1)" with the SDK client: ported through
//     raw HTTP (5 MiB argument, then 10 MiB + 1 -> 413) in TestMCPGuards.
//   - "search_tools: ... an empty answer" with the TypeScript fake embedder: the image has the real
//     model; the empty answer is asserted in mcp_tools_test.go on a server whose only tool is
//     unrelated, and the unrelated-query golden is in search_test.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/ai-pipes/mcp-registry/compat/harness"
)

const (
	mcpInstruction = "This server is a gateway to the user's services (git hosting, issue tracker and others). When a task needs one of them, call search_tools with a short description of the action, then describe_tool for the chosen tool, then call_tool. If nothing fits, continue without them. If a tool needs a connection, tell the user the link from the error."
	mcpBudget      = 8000 // characters of tools/list plus instructions
)

// mcpSupported are the revisions of the initialize handshake the image speaks; a request for an
// unknown one is answered with one of them.
var mcpSupported = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

func TestMCPProtocolClassic(t *testing.T) {
	s := harness.Shared(t)
	led := newMCPLedger(t, s)
	anon := s.Anonymous()
	token := s.Member(t, harness.Unique("mcp-classic")).Token(t, "classic")
	led.note(token)

	t.Run("initialize negotiates the protocol revision", func(t *testing.T) {
		for _, c := range []struct{ asked, want string }{
			{"2025-03-26", "2025-03-26"},
			{"2025-06-18", "2025-06-18"},
			{"2025-11-25", "2025-11-25"},
			{"1999-01-01", ""}, // an unknown revision: one the server supports
			{"2026-07-28", ""},
			{"", ""},
			{"not-a-date", ""},
		} {
			r := mcpPost(t, led, anon, token, mcpInitializeBody(c.asked))
			if r.Status != 200 || !r.HasMsg || r.Msg.Error != nil || (!mcpIsJSON(r.contentType()) && !mcpIsStream(r.contentType())) {
				t.Fatalf("initialize %q: status %d %s error %+v: %.200s", c.asked, r.Status, r.contentType(), r.Msg.Error, r.Body)
			}
			mcpNoSession(t, "initialize "+c.asked, r)
			res := r.Msg.Result
			got, _ := res["protocolVersion"].(string)
			if (c.want != "" && got != c.want) || (c.want == "" && !slices.Contains(mcpSupported, got)) {
				t.Errorf("initialize %q: protocolVersion %q, want %q (an unknown revision gets one of %v)", c.asked, got, c.want, mcpSupported)
			}
			if !reflect.DeepEqual(res["capabilities"], map[string]any{"tools": map[string]any{"listChanged": false}}) {
				t.Errorf("initialize %q: capabilities %v, want only tools with listChanged false", c.asked, res["capabilities"])
			}
			info, _ := res["serverInfo"].(map[string]any)
			if info["name"] != "mcp-registry" || info["version"] == "" || info["version"] == nil {
				t.Errorf("initialize %q: serverInfo %v", c.asked, info)
			}
			if res["instructions"] != mcpInstruction {
				t.Errorf("initialize %q: instructions %v", c.asked, res["instructions"])
			}
		}
	})

	t.Run("notifications are accepted with an empty 202", func(t *testing.T) {
		for _, n := range []string{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`} {
			r := mcpPost(t, led, anon, token, n)
			if r.Status != 202 || len(r.Body) != 0 {
				t.Fatalf("%s: status %d body %q, want 202 and no body", n, r.Status, r.Body)
			}
			mcpNoSession(t, n, r)
		}
	})

	t.Run("tools/list and tools/call work without a handshake, there is no session", func(t *testing.T) {
		list := mcpPost(t, led, anon, token, mcpClassicBody(2, "tools/list", nil))
		if list.Status != 200 || !list.HasMsg || list.Msg.Error != nil {
			t.Fatalf("tools/list: %d %s", list.Status, list.Body)
		}
		mcpNoSession(t, "tools/list", list)
		names := mcpToolNames(list.Msg.Result)
		if !slices.Equal(names, []string{"list_servers", "search_tools", "describe_tool", "call_tool"}) {
			t.Fatalf("tools/list names %v, want the four meta-tools in this order", names)
		}
		if r := mcpPost(t, led, anon, token, mcpClassicBody(5, "ping", nil)); r.Status != 200 || !r.HasMsg || r.Msg.Error != nil || r.Msg.Result == nil || len(r.Msg.Result) != 0 {
			t.Fatalf("ping: %d %.200s, want an empty result", r.Status, r.Body)
		}
		call := mcpPost(t, led, anon, token, mcpClassicBody(3, "tools/call", map[string]any{"name": "list_servers", "arguments": map[string]any{}}))
		if call.Status != 200 || !call.HasMsg || call.Msg.Result["isError"] != nil {
			t.Fatalf("tools/call list_servers: %d %v", call.Status, call.Msg.Result)
		}
		mcpNoSession(t, "tools/call", call)
		if blocks, _ := call.Msg.Result["content"].([]any); len(blocks) != 1 {
			t.Fatalf("list_servers returned %d content blocks, want exactly one", len(blocks))
		}
		// The text is the structured content as compact JSON.
		var fromText any
		if err := json.Unmarshal([]byte(mcpResultText(call.Msg.Result)), &fromText); err != nil || !reflect.DeepEqual(fromText, call.Msg.Result["structuredContent"]) {
			t.Fatalf("the text %q is not the structuredContent %v (%v)", mcpResultText(call.Msg.Result), call.Msg.Result["structuredContent"], err)
		}
	})

	t.Run("an argument the tool does not declare is ignored (control of the validation test below)", func(t *testing.T) {
		res := mcpToolsCall(t, led, anon, token, "list_servers", map[string]any{"unknown": 1})
		if res["isError"] != nil || res["structuredContent"] == nil {
			t.Fatalf("an undeclared argument of list_servers: %v", res)
		}
	})

	t.Run("JSON-RPC errors carry the documented codes", func(t *testing.T) {
		for _, c := range []struct {
			name string
			body string
			code int
		}{
			{"unknown method", mcpClassicBody(2, "no/such", nil), -32601},
			{"unknown tool", mcpClassicBody(2, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}}), -32602},
			{"invalid JSON", "{", -32700},
		} {
			r := mcpPost(t, led, anon, token, c.body)
			if r.Status >= 500 || r.Msg.Error == nil || r.Msg.Error.Code != c.code || r.Msg.Error.Message == "" {
				t.Errorf("%s: status %d error %+v, want a JSON-RPC error %d with a message (body %.200s)", c.name, r.Status, r.Msg.Error, c.code, r.Body)
			}
			mcpNoSession(t, c.name, r)
		}
		// A missing or one-sided Accept: refused with a 4xx or with a JSON-RPC error, never with a
		// tool list; only a one-sided `Accept: application/json` may also be answered normally, and
		// then the answer must be a valid answer to the request that was sent (the four tools).
		for _, c := range []struct {
			hdr      string
			validOK  bool
			whatItIs string
		}{{"Accept: application/json", true, "a one-sided Accept"}, {"Accept:", false, "no Accept"}} {
			r := mcpPost(t, led, anon, token, mcpClassicBody(2, "tools/list", nil), c.hdr)
			refused := r.Status/100 == 4 || (r.HasMsg && r.Msg.Error != nil && r.Msg.Error.Message != "")
			valid := c.validOK && r.Status == 200 && r.HasMsg && r.Msg.Error == nil && len(mcpToolNames(r.Msg.Result)) == 4
			if !refused && !valid {
				t.Errorf("%s: status %d %.200s, want a 4xx or a JSON-RPC error (or a valid tool list for a one-sided Accept)", c.whatItIs, r.Status, r.Body)
			}
			if !refused && !c.validOK && strings.Contains(string(r.Body), "list_servers") {
				t.Errorf("%s: a tool list was returned", c.whatItIs)
			}
		}
		// A request of the stateless revision without its headers: no server error, no tool list.
		if r := mcpPost(t, led, anon, token, mcpClassicBody(2, "server/discover", nil)); r.Status >= 500 || strings.Contains(string(r.Body), "list_servers") {
			t.Errorf("server/discover without the stateless headers: %d %.200s", r.Status, r.Body)
		}
		// The MCP-Protocol-Version header: a supported revision is fine, an unsupported one is 400.
		if r := mcpPost(t, led, anon, token, mcpClassicBody(2, "tools/list", nil), "MCP-Protocol-Version: 2025-06-18"); r.Status != 200 || r.Msg.Error != nil {
			t.Fatalf("a supported MCP-Protocol-Version: %d %.200s", r.Status, r.Body)
		}
		if r := mcpPost(t, led, anon, token, mcpClassicBody(2, "tools/list", nil), "MCP-Protocol-Version: 1999-01-01"); r.Status != 400 {
			t.Fatalf("an unsupported MCP-Protocol-Version: %d %.200s, want 400", r.Status, r.Body)
		}
	})

	t.Run("a refused tool call is an isError result that names the field, not a protocol error", func(t *testing.T) {
		for _, c := range []struct {
			tool  string
			args  map[string]any
			field string
		}{
			{"search_tools", map[string]any{"query": "x", "limit": 26}, "limit"},
			{"search_tools", map[string]any{"query": "x", "limit": 0}, "limit"},
			{"search_tools", map[string]any{"query": ""}, "query"},
			{"search_tools", map[string]any{"query": strings.Repeat("q", 501)}, "query"},
			{"search_tools", map[string]any{"query": "x", "servers": []string{}}, "servers"},
			{"search_tools", map[string]any{"query": "x", "servers": slices.Repeat([]string{"a"}, 21)}, "servers"},
			{"search_tools", map[string]any{"limit": 3}, "query"},
			{"describe_tool", map[string]any{"tool": 5}, "tool"},
			{"describe_tool", map[string]any{"tool": ""}, "tool"},
			{"describe_tool", map[string]any{"tool": strings.Repeat("t", 301)}, "tool"},
			{"call_tool", map[string]any{"tool": "a.b", "arguments": "no"}, "arguments"},
			{"call_tool", map[string]any{"arguments": map[string]any{}}, "tool"},
		} {
			mcpExpectInputError(t, fmt.Sprintf("%s %.60v", c.tool, c.args), mcpToolsCall(t, led, anon, token, c.tool, c.args), c.field)
		}
		// Controls: the boundary values are accepted.
		for _, args := range []map[string]any{{"query": "x", "limit": 25}, {"query": "x", "limit": 1}, {"query": strings.Repeat("q", 500)}} {
			if res := mcpToolsCall(t, led, anon, token, "search_tools", args); res["isError"] != nil {
				t.Errorf("search_tools %.60v: %v", args, res)
			}
		}
		// And the helper does tell an input error from the not_found text and from a success.
		if mcpFieldNamed("Tool \"a.b\" is not available. Use search_tools to find another.", "tool") {
			t.Error("the not_found text is taken for an input error naming `tool`")
		}
		if !mcpFieldNamed("Input validation error: Invalid arguments for tool search_tools: limit: Too big", "limit") ||
			mcpFieldNamed("Input validation error: Invalid arguments for tool describe_tool: query: Too small", "tool") {
			t.Error("the field-path helper accepts or refuses the wrong texts")
		}
	})

	t.Run("only POST is served: GET and DELETE are 405 with Allow: POST, 401 without a token", func(t *testing.T) {
		for _, m := range []string{"GET", "DELETE"} {
			for _, extra := range [][]string{nil, {"Mcp-Session-Id: abc"}, {"Accept: text/event-stream"}} {
				r := anon.Do(t, m, "/mcp", nil, append([]string{"Authorization: Bearer " + token}, extra...)...)
				if r.Status != 405 || r.Header.Get("Allow") != "POST" {
					t.Errorf("%s /mcp %v: %d Allow %q %.200s", m, extra, r.Status, r.Header.Get("Allow"), r.Body)
				}
				if v := r.Header.Values("Mcp-Session-Id"); len(v) != 0 {
					t.Errorf("%s /mcp: Mcp-Session-Id %v", m, v)
				}
			}
			if r := anon.Do(t, m, "/mcp", nil); r.Status != 401 {
				t.Errorf("%s /mcp without a token: %d, want 401 (before 405)", m, r.Status)
			}
		}
		// Other methods are not served either: a 4xx, or a JSON-RPC error body, and in no case a tool
		// result.
		for _, m := range []string{"PUT", "PATCH", "OPTIONS"} {
			r := anon.Do(t, m, "/mcp", nil, "Authorization: Bearer "+token)
			msgs, _ := mcpMessages(t, r.Header.Get("Content-Type"), r.Body)
			rpcError := len(msgs) > 0 && msgs[0].Error != nil
			if (r.Status/100 != 4 && !rpcError) || strings.Contains(string(r.Body), "list_servers") {
				t.Errorf("%s /mcp: %d %.200s, want a 4xx or a JSON-RPC error and no tool result", m, r.Status, r.Body)
			}
		}
	})
}

// ---- the Go SDK client ----

func TestMCPSDKClient(t *testing.T) {
	s := harness.Shared(t)
	led := newMCPLedger(t, s)
	token := s.Member(t, harness.Unique("mcp-sdk")).Token(t, "sdk")
	sess := led.session(token)
	list, err := sess.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	if !slices.Equal(names, []string{"list_servers", "search_tools", "describe_tool", "call_tool"}) {
		t.Fatalf("the SDK client lists %v", names)
	}
	res := led.call(sess, "list_servers", map[string]any{})
	if res.IsError || res.StructuredContent == nil {
		t.Fatalf("list_servers through the SDK client: %+v", res)
	}
}

// ---- the surface golden ----

func mcpUTF16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func TestMCPSurface(t *testing.T) {
	s := harness.Shared(t)
	led := newMCPLedger(t, s)
	anon := s.Anonymous()
	person := s.Member(t, harness.Unique("mcp-surface"))
	token := person.Token(t, "surface")
	led.note(token)

	var version struct{ Version string }
	person.Do(t, "GET", "/api/version", nil).JSON(t, "VersionResponse", &version)

	init := mcpPost(t, led, anon, token, mcpInitializeBody(mcpClassic))
	list := mcpPost(t, led, anon, token, mcpClassicBody(2, "tools/list", nil))
	if init.Msg.Result == nil || list.Msg.Result == nil {
		t.Fatalf("no result: %d %.200s / %d %.200s", init.Status, init.Body, list.Status, list.Body)
	}
	info, _ := init.Msg.Result["serverInfo"].(map[string]any)
	tools, _ := list.Msg.Result["tools"].([]any)

	t.Run("the Go SDK client sees the same surface as the wire", func(t *testing.T) {
		sess := led.session(token)
		got := sess.InitializeResult()
		if got.ServerInfo == nil || got.ServerInfo.Name != "mcp-registry" || got.ServerInfo.Version != version.Version || got.Instructions != mcpInstruction ||
			got.Capabilities == nil || got.Capabilities.Tools == nil || got.Capabilities.Tools.ListChanged {
			t.Fatalf("initialize result %+v", got)
		}
		listed, err := sess.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		// The SDK's Tool type writes idempotentHint:false, which the server never sends; leave it out.
		raw, _ := json.Marshal(listed.Tools)
		var sdk []any
		if err := json.Unmarshal(raw, &sdk); err != nil {
			t.Fatal(err)
		}
		for _, tl := range sdk {
			if a, ok := tl.(map[string]any)["annotations"].(map[string]any); ok && a["idempotentHint"] == false {
				delete(a, "idempotentHint")
			}
		}
		if !reflect.DeepEqual(sdk, tools) {
			t.Errorf("the SDK client's tools differ from the wire:\nsdk  %v\nwire %v", sdk, tools)
		}
		if info["version"] != version.Version {
			t.Errorf("serverInfo.version %v, GET /api/version %q", info["version"], version.Version)
		}
	})

	t.Run("the golden holds the four tools, the instructions, the server info and the capabilities", func(t *testing.T) {
		// Written from parsed JSON with sorted keys (encoding/json sorts map keys), so it is compared
		// as a document, not as the key order of one server.
		golden := map[string]any{
			"serverInfo":   map[string]any{"name": info["name"], "version": "<version>"},
			"instructions": init.Msg.Result["instructions"],
			"capabilities": init.Msg.Result["capabilities"],
			"tools":        tools,
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(golden); err != nil {
			t.Fatal(err)
		}
		harness.Golden(t, "mcp-surface.json", buf.Bytes())
	})

	t.Run("tools/list plus the instructions stay under 8000 characters", func(t *testing.T) {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(map[string]any{"tools": tools}); err != nil {
			t.Fatal(err)
		}
		size := mcpUTF16Len(strings.TrimSpace(buf.String())) + mcpUTF16Len(mcpInstruction)
		if size >= mcpBudget {
			t.Fatalf("tools/list + instructions: %d characters, the budget is under %d", size, mcpBudget)
		}
		t.Logf("tools/list + instructions: %d of %d characters", size, mcpBudget)
	})
}

// ---- the guards in front of the handler ----

func TestMCPGuards(t *testing.T) {
	s := harness.Shared(t)
	led := newMCPLedger(t, s)
	admin := s.Admin(t)
	person := s.Member(t, harness.Unique("mcp-guard"))
	token := person.Token(t, "guard")
	led.note(token)
	anon := s.Anonymous()
	listBody := mcpClassicBody(2, "tools/list", nil)

	t.Run("without a token POST /mcp is 401 with a Bearer challenge", func(t *testing.T) {
		r := anon.Do(t, "POST", "/mcp", listBody, mcpJSON, mcpAccept)
		if r.Status != 401 {
			t.Fatalf("status %d, want 401: %s", r.Status, r.Body)
		}
		if got := r.Header.Get("Www-Authenticate"); got != "Bearer" {
			t.Fatalf("WWW-Authenticate = %q, want %q", got, "Bearer")
		}
		// Control: the same request with a token is served.
		if ok := mcpPost(t, led, anon, token, listBody); ok.Status != 200 {
			t.Fatalf("with a token: %d", ok.Status)
		}
	})

	t.Run("a foreign Origin is refused before the token is looked at; no Origin and the own one are served", func(t *testing.T) {
		const want = "forbidden|origin not allowed"
		for _, c := range []struct {
			name string
			hdr  []string
		}{
			{"with a valid token", []string{"Authorization: Bearer " + token}},
			{"without a token", nil},
			{"with a bad token", []string{"Authorization: Bearer sk_" + strings.Repeat("o", 43)}},
		} {
			r := anon.Do(t, "POST", "/mcp", listBody, append([]string{mcpJSON, mcpAccept, "Origin: http://evil.example"}, c.hdr...)...)
			if r.Status != 403 || errorCode(t, r) != want {
				t.Errorf("%s: %d %.200s, want 403 %s", c.name, r.Status, r.Body, want)
			}
		}
		for _, origin := range []string{"Origin:", "Origin: " + s.BaseURL} {
			if r := mcpPost(t, led, anon, token, listBody, origin); r.Status != 200 {
				t.Errorf("%q: %d %.200s, want 200", origin, r.Status, r.Body)
			}
		}
		// The origin is compared exactly: a lookalike is foreign.
		if r := anon.Do(t, "POST", "/mcp", listBody, mcpJSON, mcpAccept, "Authorization: Bearer "+token, "Origin: "+s.BaseURL+".evil.example"); r.Status != 403 {
			t.Errorf("a lookalike Origin: %d", r.Status)
		}
	})

	t.Run("a foreign Host is refused before the Origin and the token; loopback names are served", func(t *testing.T) {
		for _, c := range []struct {
			name string
			hdr  []string
		}{
			{"with a valid token", []string{"Authorization: Bearer " + token}},
			{"with a foreign Origin too", []string{"Authorization: Bearer " + token, "Origin: http://evil.example"}},
			{"without a token", nil},
		} {
			r := anon.Do(t, "POST", "/mcp", listBody, append([]string{mcpJSON, mcpAccept, "Host: evil.example"}, c.hdr...)...)
			// The refusal's text is the transport library's, not the product's: status and "nothing served".
			assertHostRefused(t, c.name, r)
		}
		port := s.BaseURL[strings.LastIndex(s.BaseURL, ":")+1:]
		for _, host := range []string{"localhost", "localhost:" + port, "127.0.0.1:" + port} {
			if r := mcpPost(t, led, anon, token, listBody, "Host: "+host); r.Status != 200 {
				t.Errorf("Host %s: %d %.200s, want 200", host, r.Status, r.Body)
			}
		}
	})

	t.Run("the body limit of /mcp is 10 MiB, not the 1 MiB of the REST API", func(t *testing.T) {
		const limit = 10 * 1024 * 1024
		pad := func(n int) string {
			prefix := mcpClassicBody(2, "tools/list", nil)
			return prefix + strings.Repeat(" ", n-len(prefix))
		}
		// One byte over is refused first: the server answers 413 without reading the rest and closes
		// the connection, so right after a heavy exchange a 10 MiB upload can lose the answer to a
		// reset. The request goes through its own client (its own connections), so a reset
		// connection is never reused by another request, and it is the first large one of the test.
		r := s.Anonymous().DoExpectingEarlyRefusal(t, "POST", "/mcp", []byte(pad(limit+1)), mcpJSON, mcpAccept, "Authorization: Bearer "+token)
		if r.Status != 413 || errorCode(t, r) != "limit_exceeded|request body is too large" {
			t.Fatalf("10 MiB + 1 byte: %d %.200s, want 413 limit_exceeded", r.Status, r.Body)
		}
		env := gwNewServer(t, led, admin, nil, "none")
		ref := env.slug + ".echo"
		// 5 MiB of arguments: above the REST limit and above the SDK's own 4 MiB default.
		big := strings.Repeat("x", 5*1024*1024)
		res := mcpToolsCall(t, led, anon, token, "call_tool", map[string]any{"tool": ref, "arguments": map[string]any{"text": big}})
		if res["isError"] != nil || !strings.Contains(mcpResultText(res), "xxxxxxxx") {
			t.Fatalf("a 5 MiB call: %.200v", res)
		}
		if n := len(env.up.Calls()); n != 1 {
			t.Fatalf("the upstream saw %d calls, want 1", n)
		}
		// Exactly the limit is accepted.
		if r := s.Anonymous().Do(t, "POST", "/mcp", pad(limit), mcpJSON, mcpAccept, "Authorization: Bearer "+token); r.Status != 200 {
			t.Errorf("a body of exactly 10 MiB: %d %.200s, want 200", r.Status, r.Body)
		}
	})

	t.Run("a session cookie is not a credential of /mcp", func(t *testing.T) {
		r := person.Do(t, "POST", "/mcp", listBody, mcpJSON, mcpAccept)
		if r.Status != 401 || r.Header.Get("Www-Authenticate") != "Bearer" {
			t.Fatalf("a session cookie on /mcp: %d %s", r.Status, r.Body)
		}
	})
}
