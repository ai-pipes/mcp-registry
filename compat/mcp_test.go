//go:build compat

package compat_test

// MCP endpoint scenarios: the protocol of /mcp as raw JSON-RPC over HTTP (the classic `initialize`
// handshake and the stateless 2026-07-28 revision that the Go SDK client speaks), the surface
// golden, the guards in front of the handler, and the four meta-tools (list_servers, search_tools,
// describe_tool and the round trip to call_tool). What call_tool does with the upstream (result
// pass-through, validation, quota, the log) is in gateway_test.go; the call log and audit REST
// endpoints are in gateway_rest_test.go. Source: mcp.int.test.ts.
//
// Two kinds of MCP client reach /mcp and the suite exercises both:
//   - the Go SDK client of Stack.MCP opens with `server/discover` and the headers Mcp-Method,
//     Mcp-Protocol-Version and Mcp-Name (revision 2026-07-28, stateless, one JSON answer);
//   - older clients open with `initialize` and answer in an event stream (revisions 2024-11-05 to
//     2025-11-25).
// Neither issues an Mcp-Session-Id: the server is stateless (one McpServer per request).
//
// Already covered elsewhere (cross-references; the full table is in the task report):
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
//   - "search_tools: ... an empty answer" with the TypeScript fake embedder (a bag of tokens and a
//     low similarity floor): the image has the real model, so the answer to a query no tool
//     resembles is pinned in search_test.go; here an empty answer is asserted for a query that
//     matches nothing by words either.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

const (
	mcpAccept      = "Accept: application/json, text/event-stream"
	mcpJSON        = "Content-Type: application/json"
	mcpStateless   = "2026-07-28"
	mcpClassic     = "2025-11-25" // the newest revision of the initialize handshake
	mcpInstruction = "This server is a gateway to the user's services (git hosting, issue tracker and others). When a task needs one of them, call search_tools with a short description of the action, then describe_tool for the chosen tool, then call_tool. If nothing fits, continue without them. If a tool needs a connection, tell the user the link from the error."
	mcpBudget      = 8000 // characters of tools/list plus instructions
)

// ---- raw JSON-RPC over HTTP ----

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// mcpReply is one HTTP answer of /mcp: the status, the headers, the body and, when it carries a
// JSON-RPC message (a JSON body or the single event of an event stream), that message.
type mcpReply struct {
	Status int
	Header http.Header
	Body   []byte
	SSE    bool
	Msg    struct {
		ID     any            `json:"id"`
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
}

func (r mcpReply) contentType() string { return r.Header.Get("Content-Type") }

// mcpDecode fills Msg: an event stream must be exactly one `event: message` with one `data:` line
// and the blank line that ends the event; a JSON body is the message itself.
func mcpDecode(t *testing.T, r *mcpReply) {
	t.Helper()
	body := r.Body
	switch ct := r.contentType(); {
	case strings.HasPrefix(ct, "text/event-stream"):
		r.SSE = true
		rest, ok := strings.CutPrefix(string(body), "event: message\ndata: ")
		data, tail, ok2 := strings.Cut(rest, "\n")
		if !ok || !ok2 || tail != "\n" {
			t.Fatalf("the event stream is not one `event: message` / `data:` event: %q", body)
		}
		body = []byte(data)
	case strings.HasPrefix(ct, "application/json"):
	default:
		return
	}
	if err := json.Unmarshal(body, &r.Msg); err != nil {
		t.Fatalf("a JSON-RPC message was expected (status %d, %s): %v: %.300s", r.Status, r.contentType(), err, r.Body)
	}
}

// mcpPost sends one JSON-RPC body to /mcp with a bearer token and both Accept types. hdr entries
// are added after (and override) the defaults; an empty value removes one.
func mcpPost(t *testing.T, c *harness.Client, token, body string, hdr ...string) mcpReply {
	t.Helper()
	h := []string{"Authorization: Bearer " + token, mcpJSON, mcpAccept}
	resp := c.Do(t, "POST", "/mcp", body, append(h, hdr...)...)
	r := mcpReply{Status: resp.Status, Header: resp.Header, Body: resp.Body}
	mcpDecode(t, &r)
	return r
}

func mcpNoSession(t *testing.T, what string, r mcpReply) {
	t.Helper()
	if v := r.Header.Values("Mcp-Session-Id"); len(v) != 0 {
		t.Fatalf("%s: the server issued Mcp-Session-Id %v: it is stateless", what, v)
	}
}

// mcpClassicBody builds a request of the classic protocol (no per-request envelope).
func mcpClassicBody(id int, method string, params any) string {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func mcpInitializeBody(version string) string {
	return mcpClassicBody(1, "initialize", map[string]any{"protocolVersion": version, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "raw", "version": "1"}})
}

// mcpEnvelope is the per-request envelope of the stateless revision.
func mcpEnvelope() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "raw", "version": "1"},
		"io.modelcontextprotocol/protocolVersion":    mcpStateless,
	}
}

// mcpStatelessPost sends a request of the stateless revision: the envelope in params._meta and the
// headers Mcp-Method, Mcp-Protocol-Version (and Mcp-Name for a call).
func mcpStatelessPost(t *testing.T, c *harness.Client, token, method string, params map[string]any, hdr ...string) mcpReply {
	t.Helper()
	p := map[string]any{"_meta": mcpEnvelope()}
	for k, v := range params {
		p[k] = v
	}
	h := []string{"Mcp-Method: " + method, "Mcp-Protocol-Version: " + mcpStateless}
	if name, ok := params["name"].(string); ok {
		h = append(h, "Mcp-Name: "+name)
	}
	return mcpPost(t, c, token, mcpClassicBody(7, method, p), append(h, hdr...)...)
}

// mcpToolsCall calls a tool over the classic protocol and returns the result object exactly as it
// went over the wire.
func mcpToolsCall(t *testing.T, c *harness.Client, token, name string, args map[string]any) map[string]any {
	t.Helper()
	r := mcpPost(t, c, token, mcpClassicBody(3, "tools/call", map[string]any{"name": name, "arguments": args}))
	if r.Status != 200 || r.Msg.Error != nil || r.Msg.Result == nil {
		t.Fatalf("tools/call %s: status %d error %+v body %.300s", name, r.Status, r.Msg.Error, r.Body)
	}
	return r.Msg.Result
}

// ---- the ledger: what must never appear in an answer of /mcp ----

// mcpLedger collects every MCP answer a test sees and, when the test ends, looks in them (and in
// the app log) for what must never leave: the upstream's address, the auth settings of a server
// and the secrets the test noted. The upstream URLs all contain host.docker.internal; the header
// and template used by the scenarios are X-API-Key and "key {secret}".
type mcpLedger struct {
	t       *testing.T
	s       *harness.Stack
	mu      sync.Mutex
	seen    []string
	secrets []string
}

func newMCPLedger(t *testing.T, s *harness.Stack) *mcpLedger {
	t.Helper()
	l := &mcpLedger{t: t, s: s}
	t.Cleanup(l.scan)
	return l
}

// note registers a secret that must appear in no answer and no log line.
func (l *mcpLedger) note(values ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.secrets = append(l.secrets, values...)
}

func (l *mcpLedger) record(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		l.t.Errorf("ledger: %v", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, string(b))
}

func (l *mcpLedger) scan() {
	l.mu.Lock()
	seen, secrets := slices.Clone(l.seen), slices.Clone(l.secrets)
	l.mu.Unlock()
	forbidden := append([]string{"host.docker.internal", "X-API-Key", "{secret}"}, secrets...)
	for _, answer := range seen {
		for _, f := range forbidden {
			if strings.Contains(answer, f) {
				l.t.Errorf("an MCP answer contains %q: %.300s", harness.Redact(f), answer)
			}
		}
	}
	logs := l.s.Logs(l.t)
	for _, sec := range secrets {
		if strings.Contains(logs, sec) {
			l.t.Errorf("the app log contains the secret %s", harness.Redact(sec))
		}
	}
	if len(seen) == 0 {
		l.t.Error("the ledger saw no MCP answer: the scan proves nothing")
	}
}

// call is a tools/call through an SDK session; the answer goes into the ledger.
func (l *mcpLedger) call(sess *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	l.t.Helper()
	res := catalogCallMCP(l.t, sess, name, args)
	l.record(res)
	return res
}

// raw records a raw reply.
func (l *mcpLedger) raw(r mcpReply) mcpReply {
	l.record(string(r.Body))
	return r
}

// ---- the protocol ----

func TestMCPProtocolClassic(t *testing.T) {
	s := harness.Shared(t)
	anon := s.Anonymous()
	token := s.Member(t, harness.Unique("mcp-classic")).Token(t, "classic")

	t.Run("initialize negotiates the protocol revision and answers in an event stream", func(t *testing.T) {
		for _, c := range []struct{ asked, want string }{
			{"2025-03-26", "2025-03-26"},
			{"2025-06-18", "2025-06-18"},
			{"2025-11-25", "2025-11-25"},
			{"2024-11-05", "2024-11-05"},
			{"1999-01-01", mcpClassic}, // an unknown revision: the newest the server speaks
			{"2026-07-28", mcpClassic}, // the stateless revision is not negotiated through initialize
			{"", mcpClassic},           // none given
			{"not-a-date", mcpClassic},
		} {
			r := mcpPost(t, anon, token, mcpInitializeBody(c.asked))
			if r.Status != 200 || !r.SSE || r.contentType() != "text/event-stream" {
				t.Fatalf("initialize %q: status %d content type %q, want 200 text/event-stream", c.asked, r.Status, r.contentType())
			}
			if !strings.HasPrefix(r.Header.Get("Cache-Control"), "no-cache") {
				t.Errorf("initialize %q: Cache-Control %q", c.asked, r.Header.Get("Cache-Control"))
			}
			mcpNoSession(t, "initialize "+c.asked, r)
			res := r.Msg.Result
			if res["protocolVersion"] != c.want {
				t.Errorf("initialize %q: protocolVersion %v, want %q", c.asked, res["protocolVersion"], c.want)
			}
			keys := make([]string, 0, len(res))
			for k := range res {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, []string{"capabilities", "instructions", "protocolVersion", "serverInfo"}) {
				t.Errorf("initialize %q: result keys %v", c.asked, keys)
			}
			if !reflect.DeepEqual(res["capabilities"], map[string]any{"tools": map[string]any{"listChanged": false}}) {
				t.Errorf("initialize %q: capabilities %v, want only tools with listChanged false", c.asked, res["capabilities"])
			}
			info, _ := res["serverInfo"].(map[string]any)
			if info["name"] != "mcp-registry" || info["version"] == "" || len(info) != 2 {
				t.Errorf("initialize %q: serverInfo %v", c.asked, info)
			}
			if res["instructions"] != mcpInstruction {
				t.Errorf("initialize %q: instructions %v", c.asked, res["instructions"])
			}
			if r.Msg.ID != float64(1) {
				t.Errorf("initialize %q: id %v", c.asked, r.Msg.ID)
			}
		}
	})

	t.Run("notifications are accepted with an empty 202", func(t *testing.T) {
		for _, n := range []string{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`} {
			r := mcpPost(t, anon, token, n)
			if r.Status != 202 || len(r.Body) != 0 {
				t.Fatalf("%s: status %d body %q, want 202 and no body", n, r.Status, r.Body)
			}
			mcpNoSession(t, n, r)
		}
	})

	t.Run("tools/list and tools/call work without a handshake, there is no session", func(t *testing.T) {
		list := mcpPost(t, anon, token, mcpClassicBody(2, "tools/list", nil))
		if list.Status != 200 || !list.SSE {
			t.Fatalf("tools/list: %d %s", list.Status, list.contentType())
		}
		mcpNoSession(t, "tools/list", list)
		var names []string
		tools, _ := list.Msg.Result["tools"].([]any)
		for _, tl := range tools {
			names = append(names, tl.(map[string]any)["name"].(string))
		}
		if !slices.Equal(names, []string{"list_servers", "search_tools", "describe_tool", "call_tool"}) {
			t.Fatalf("tools/list names %v, want the four meta-tools in this order", names)
		}
		call := mcpPost(t, anon, token, mcpClassicBody(3, "tools/call", map[string]any{"name": "list_servers", "arguments": map[string]any{}}))
		if call.Status != 200 || !call.SSE || call.Msg.Result["isError"] != nil {
			t.Fatalf("tools/call list_servers: %d %s %v", call.Status, call.contentType(), call.Msg.Result)
		}
		mcpNoSession(t, "tools/call", call)
		// The text is the structured content as compact JSON.
		content, _ := call.Msg.Result["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("content: %v", content)
		}
		var fromText any
		if err := json.Unmarshal([]byte(content[0].(map[string]any)["text"].(string)), &fromText); err != nil || !reflect.DeepEqual(fromText, call.Msg.Result["structuredContent"]) {
			t.Fatalf("the text %v is not the structuredContent %v (%v)", content[0], call.Msg.Result["structuredContent"], err)
		}
		// An argument the tool does not declare is ignored (negative control for the next test).
		extra := mcpPost(t, anon, token, mcpClassicBody(4, "tools/call", map[string]any{"name": "list_servers", "arguments": map[string]any{"unknown": 1}}))
		if extra.Status != 200 || extra.Msg.Result["isError"] != nil {
			t.Fatalf("an undeclared argument of list_servers: %d %v", extra.Status, extra.Msg.Result)
		}
	})

	t.Run("errors: the JSON-RPC code and the HTTP status", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			body   string
			hdr    []string
			status int
			code   int
			sse    bool
		}{
			{"unknown method", mcpClassicBody(2, "no/such", nil), nil, 200, -32601, true},
			{"unknown tool", mcpClassicBody(2, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}}), nil, 200, -32602, true},
			{"resources/list", mcpClassicBody(2, "resources/list", nil), nil, 200, -32601, true},
			{"server/discover without the stateless headers", mcpClassicBody(2, "server/discover", nil), nil, 200, -32601, true},
			{"invalid JSON", "{", nil, 400, -32700, false},
			{"only application/json accepted", mcpClassicBody(2, "tools/list", nil), []string{"Accept: application/json"}, 406, -32000, false},
			{"no Accept at all", mcpClassicBody(2, "tools/list", nil), []string{"Accept:"}, 406, -32000, false},
			{"an unsupported MCP-Protocol-Version", mcpClassicBody(2, "tools/list", nil), []string{"MCP-Protocol-Version: 1999-01-01"}, 400, -32000, false},
		} {
			r := mcpPost(t, anon, token, c.body, c.hdr...)
			if r.Status != c.status || r.Msg.Error == nil || r.Msg.Error.Code != c.code || r.SSE != c.sse {
				t.Errorf("%s: status %d sse %v error %+v, want %d / %d / sse %v (body %.200s)", c.name, r.Status, r.SSE, r.Msg.Error, c.status, c.code, c.sse, r.Body)
			}
			mcpNoSession(t, c.name, r)
		}
		// Controls: a supported MCP-Protocol-Version header and ping are fine.
		if r := mcpPost(t, anon, token, mcpClassicBody(2, "tools/list", nil), "MCP-Protocol-Version: 2025-06-18"); r.Status != 200 || r.Msg.Error != nil {
			t.Fatalf("a supported MCP-Protocol-Version: %d %.200s", r.Status, r.Body)
		}
		if r := mcpPost(t, anon, token, mcpClassicBody(2, "ping", nil)); r.Status != 200 || len(r.Msg.Result) != 0 || r.Msg.Error != nil {
			t.Fatalf("ping: %d %.200s", r.Status, r.Body)
		}
	})

	t.Run("a tool that is refused for its arguments is an isError result, not a protocol error", func(t *testing.T) {
		// The wording of the validation message is the SDK's and is not pinned: the status, isError
		// and the field named in the text are.
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
			res := mcpToolsCall(t, anon, token, c.tool, c.args)
			text := fmt.Sprint(res["content"])
			if res["isError"] != true || !strings.Contains(text, c.field) {
				t.Errorf("%s %.60v: result %.300v, want isError naming %q", c.tool, c.args, res, c.field)
			}
		}
		// Controls: the boundary values are accepted.
		for _, args := range []map[string]any{{"query": "x", "limit": 25}, {"query": "x", "limit": 1}, {"query": strings.Repeat("q", 500)}} {
			if res := mcpToolsCall(t, anon, token, "search_tools", args); res["isError"] != nil {
				t.Errorf("search_tools %.60v: %v", args, res)
			}
		}
	})

	t.Run("only POST is served: GET, DELETE and the rest are 405 with Allow: POST", func(t *testing.T) {
		for _, m := range []string{"GET", "DELETE", "PUT", "PATCH", "OPTIONS"} {
			for _, extra := range [][]string{nil, {"Mcp-Session-Id: abc"}, {"Accept: text/event-stream"}} {
				r := anon.Do(t, m, "/mcp", nil, append([]string{"Authorization: Bearer " + token}, extra...)...)
				mcpDecode(t, &mcpReply{Status: r.Status, Header: r.Header, Body: r.Body})
				var msg struct {
					ID    any       `json:"id"`
					Error *rpcError `json:"error"`
				}
				if err := json.Unmarshal(r.Body, &msg); err != nil {
					t.Fatalf("%s /mcp %v: %v: %.200s", m, extra, err, r.Body)
				}
				if r.Status != 405 || r.Header.Get("Allow") != "POST" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") ||
					msg.Error == nil || msg.Error.Code != -32000 || msg.ID != nil {
					t.Errorf("%s /mcp %v: %d Allow %q %s id %v error %+v", m, extra, r.Status, r.Header.Get("Allow"), r.Header.Get("Content-Type"), msg.ID, msg.Error)
				}
				if v := r.Header.Values("Mcp-Session-Id"); len(v) != 0 {
					t.Errorf("%s /mcp: Mcp-Session-Id %v", m, v)
				}
			}
		}
		// The token is still checked first: a 405 is not an answer to a stranger.
		if r := anon.Do(t, "DELETE", "/mcp", nil); r.Status != 401 {
			t.Fatalf("DELETE /mcp without a token: %d, want 401", r.Status)
		}
	})
}

func TestMCPProtocolStateless(t *testing.T) {
	s := harness.Shared(t)
	anon := s.Anonymous()
	token := s.Member(t, harness.Unique("mcp-stateless")).Token(t, "stateless")

	serverInfo := func(t *testing.T, what string, r mcpReply) {
		t.Helper()
		meta, _ := r.Msg.Result["_meta"].(map[string]any)
		info, _ := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
		if info["name"] != "mcp-registry" || info["version"] == "" || len(info) != 2 {
			t.Errorf("%s: _meta serverInfo %v", what, meta)
		}
	}

	t.Run("server/discover answers one JSON message with the capabilities and no session", func(t *testing.T) {
		r := mcpStatelessPost(t, anon, token, "server/discover", nil)
		if r.Status != 200 || r.SSE || !strings.HasPrefix(r.contentType(), "application/json") || r.Msg.Error != nil {
			t.Fatalf("server/discover: %d %s %.300s", r.Status, r.contentType(), r.Body)
		}
		mcpNoSession(t, "server/discover", r)
		res := r.Msg.Result
		if !reflect.DeepEqual(res["supportedVersions"], []any{mcpStateless}) ||
			!reflect.DeepEqual(res["capabilities"], map[string]any{"tools": map[string]any{"listChanged": false}}) ||
			res["instructions"] != mcpInstruction || res["resultType"] != "complete" || res["ttlMs"] != float64(0) || res["cacheScope"] != "private" {
			t.Fatalf("server/discover result: %v", res)
		}
		serverInfo(t, "server/discover", r)
	})

	t.Run("tools/list and tools/call carry resultType and the server info in _meta", func(t *testing.T) {
		list := mcpStatelessPost(t, anon, token, "tools/list", nil)
		tools, _ := list.Msg.Result["tools"].([]any)
		if list.Status != 200 || list.SSE || len(tools) != 4 || list.Msg.Result["resultType"] != "complete" || list.Msg.Result["ttlMs"] != float64(0) || list.Msg.Result["cacheScope"] != "private" {
			t.Fatalf("tools/list: %d sse %v %d tools %.300s", list.Status, list.SSE, len(tools), list.Body)
		}
		serverInfo(t, "tools/list", list)
		call := mcpStatelessPost(t, anon, token, "tools/call", map[string]any{"name": "list_servers", "arguments": map[string]any{}})
		if call.Status != 200 || call.SSE || call.Msg.Result["resultType"] != "complete" || call.Msg.Result["structuredContent"] == nil {
			t.Fatalf("tools/call: %d %.300s", call.Status, call.Body)
		}
		serverInfo(t, "tools/call", call)
		mcpNoSession(t, "tools/call", call)
		// An unknown tool is a JSON-RPC error with HTTP 200, as in the classic protocol.
		unknown := mcpStatelessPost(t, anon, token, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})
		if unknown.Status != 200 || unknown.Msg.Error == nil || unknown.Msg.Error.Code != -32602 {
			t.Fatalf("unknown tool: %d %.300s", unknown.Status, unknown.Body)
		}
	})

	t.Run("a request whose headers and envelope disagree is 400", func(t *testing.T) {
		body := func(method string, params map[string]any) string {
			p := map[string]any{"_meta": mcpEnvelope()}
			for k, v := range params {
				p[k] = v
			}
			return mcpClassicBody(7, method, p)
		}
		call := map[string]any{"name": "list_servers", "arguments": map[string]any{}}
		for _, c := range []struct {
			name string
			body string
			hdr  []string
			code int
		}{
			{"no Mcp-Protocol-Version header", body("server/discover", nil), []string{"Mcp-Method: server/discover"}, -32020},
			{"no Mcp-Name header", body("tools/call", call), []string{"Mcp-Method: tools/call", "Mcp-Protocol-Version: " + mcpStateless}, -32020},
			{"Mcp-Method names another method", body("tools/call", call), []string{"Mcp-Method: tools/list", "Mcp-Protocol-Version: " + mcpStateless, "Mcp-Name: list_servers"}, -32020},
			{"initialize with the stateless header", mcpInitializeBody("2025-06-18"), []string{"Mcp-Method: initialize", "Mcp-Protocol-Version: " + mcpStateless}, -32020},
			{"the header without the _meta envelope", mcpClassicBody(7, "server/discover", nil), []string{"Mcp-Method: server/discover", "Mcp-Protocol-Version: " + mcpStateless}, -32602},
		} {
			r := mcpPost(t, anon, token, c.body, c.hdr...)
			if r.Status != 400 || r.Msg.Error == nil || r.Msg.Error.Code != c.code {
				t.Errorf("%s: status %d error %+v, want 400 / %d (body %.300s)", c.name, r.Status, r.Msg.Error, c.code, r.Body)
			}
		}
	})
}

// ---- the surface golden ----

// mcpTools returns the tools of a tools/list result as decoded JSON.
func mcpTools(t *testing.T, res map[string]any) []any {
	t.Helper()
	tools, ok := res["tools"].([]any)
	if !ok {
		t.Fatalf("no tools in %v", res)
	}
	return tools
}

func mcpUTF16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func TestMCPSurface(t *testing.T) {
	s := harness.Shared(t)
	anon := s.Anonymous()
	person := s.Member(t, harness.Unique("mcp-surface"))
	token := person.Token(t, "surface")

	var version struct{ Version string }
	person.Do(t, "GET", "/api/version", nil).JSON(t, "VersionResponse", &version)

	classicInit := mcpPost(t, anon, token, mcpInitializeBody(mcpClassic))
	classicList := mcpPost(t, anon, token, mcpClassicBody(2, "tools/list", nil))
	discover := mcpStatelessPost(t, anon, token, "server/discover", nil)
	statelessList := mcpStatelessPost(t, anon, token, "tools/list", nil)

	infoOf := func(res map[string]any, stateless bool) map[string]any {
		if stateless {
			res, _ = res["_meta"].(map[string]any)
			info, _ := res["io.modelcontextprotocol/serverInfo"].(map[string]any)
			return info
		}
		info, _ := res["serverInfo"].(map[string]any)
		return info
	}
	classicInfo, statelessInfo := infoOf(classicInit.Msg.Result, false), infoOf(discover.Msg.Result, true)
	classicTools, statelessTools := mcpTools(t, classicList.Msg.Result), mcpTools(t, statelessList.Msg.Result)

	t.Run("both kinds of client see the same surface", func(t *testing.T) {
		if !reflect.DeepEqual(classicInfo, statelessInfo) {
			t.Errorf("serverInfo: classic %v, stateless %v", classicInfo, statelessInfo)
		}
		if !reflect.DeepEqual(classicInit.Msg.Result["capabilities"], discover.Msg.Result["capabilities"]) {
			t.Errorf("capabilities: classic %v, stateless %v", classicInit.Msg.Result["capabilities"], discover.Msg.Result["capabilities"])
		}
		if classicInit.Msg.Result["instructions"] != discover.Msg.Result["instructions"] {
			t.Error("instructions differ between the two kinds of client")
		}
		if !reflect.DeepEqual(classicTools, statelessTools) {
			t.Errorf("tools differ:\nclassic   %v\nstateless %v", classicTools, statelessTools)
		}
		if classicInfo["version"] != version.Version {
			t.Errorf("serverInfo.version %v, GET /api/version %q", classicInfo["version"], version.Version)
		}
	})

	t.Run("the Go SDK client's view agrees with the wire", func(t *testing.T) {
		sess := s.MCP(t, token)
		init := sess.InitializeResult()
		if init.ServerInfo == nil || init.ServerInfo.Name != "mcp-registry" || init.ServerInfo.Version != version.Version || init.Instructions != mcpInstruction ||
			init.Capabilities == nil || init.Capabilities.Tools == nil || init.Capabilities.Tools.ListChanged {
			t.Fatalf("initialize result %+v", init)
		}
		list, err := sess.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		// The SDK's Tool type writes idempotentHint:false, which the server never sends; leave it out.
		raw, _ := json.Marshal(list.Tools)
		var got []any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		for _, tl := range got {
			if a, ok := tl.(map[string]any)["annotations"].(map[string]any); ok && a["idempotentHint"] == false {
				delete(a, "idempotentHint")
			}
		}
		if !reflect.DeepEqual(got, statelessTools) {
			t.Errorf("the SDK client's tools differ from the wire:\nsdk  %v\nwire %v", got, statelessTools)
		}
	})

	t.Run("the golden holds the four tools, the instructions, the server info and the capabilities", func(t *testing.T) {
		// Envelope fields of each revision that are not part of the surface itself.
		envelope := func(r mcpReply, drop ...string) map[string]any {
			out := map[string]any{}
			for k, v := range r.Msg.Result {
				if !slices.Contains(drop, k) {
					out[k] = v
				}
			}
			return out
		}
		info := map[string]any{"name": classicInfo["name"], "version": "<version>"}
		golden := map[string]any{
			"serverInfo":   info,
			"instructions": classicInit.Msg.Result["instructions"],
			"capabilities": classicInit.Msg.Result["capabilities"],
			"tools":        classicTools,
			"revisions": map[string]any{
				"classic": map[string]any{
					"initializeResultKeys": keysOf(classicInit.Msg.Result),
					"toolsListResultKeys":  keysOf(classicList.Msg.Result),
					"initializeEncoding":   classicInit.contentType(),
				},
				"stateless": map[string]any{
					"discover":            envelope(discover, "instructions", "capabilities"),
					"toolsListResultKeys": keysOf(statelessList.Msg.Result),
					"toolsListEnvelope":   envelope(statelessList, "tools", "_meta"),
					"encoding":            statelessList.contentType(),
				},
			},
		}
		// The server info of the stateless revision is the same, in _meta.
		if m, _ := discover.Msg.Result["_meta"].(map[string]any); m != nil {
			golden["revisions"].(map[string]any)["stateless"].(map[string]any)["discover"].(map[string]any)["_meta"] = map[string]any{"io.modelcontextprotocol/serverInfo": info}
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
		if err := enc.Encode(map[string]any{"tools": classicTools}); err != nil {
			t.Fatal(err)
		}
		size := mcpUTF16Len(strings.TrimSpace(buf.String())) + mcpUTF16Len(mcpInstruction)
		if size >= mcpBudget {
			t.Fatalf("tools/list + instructions: %d characters, the budget is under %d", size, mcpBudget)
		}
		t.Logf("tools/list + instructions: %d of %d characters", size, mcpBudget)
	})
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ---- the guards in front of the handler ----

func TestMCPGuards(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	person := s.Member(t, harness.Unique("mcp-guard"))
	token := person.Token(t, "guard")
	anon := s.Anonymous()
	listBody := mcpClassicBody(2, "tools/list", nil)

	t.Run("without a token POST /mcp is 401 with a Bearer challenge", func(t *testing.T) {
		r := anon.Do(t, "POST", "/mcp", listBody, mcpJSON, mcpAccept)
		if r.Status != 401 || r.Header.Get("Www-Authenticate") != "Bearer" {
			t.Fatalf("%d %v %s", r.Status, r.Header, r.Body)
		}
		// Control: the same request with a token is served.
		if ok := mcpPost(t, anon, token, listBody); ok.Status != 200 {
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
			if r := mcpPost(t, anon, token, listBody, origin); r.Status != 200 {
				t.Errorf("%q: %d %.200s, want 200", origin, r.Status, r.Body)
			}
		}
		// The origin is compared exactly: a lookalike is foreign.
		if r := anon.Do(t, "POST", "/mcp", listBody, mcpJSON, mcpAccept, "Authorization: Bearer "+token, "Origin: "+s.BaseURL+".evil.example"); r.Status != 403 {
			t.Errorf("a lookalike Origin: %d", r.Status)
		}
	})

	t.Run("a foreign Host is refused first, in the transport's own shape; loopback names are served", func(t *testing.T) {
		for _, c := range []struct {
			name string
			hdr  []string
		}{
			{"with a valid token", []string{"Authorization: Bearer " + token}},
			{"with a foreign Origin too", []string{"Authorization: Bearer " + token, "Origin: http://evil.example"}},
		} {
			r := anon.Do(t, "POST", "/mcp", listBody, append([]string{mcpJSON, mcpAccept, "Host: evil.example"}, c.hdr...)...)
			var msg struct{ Error rpcError }
			if err := json.Unmarshal(r.Body, &msg); err != nil || r.Status != 403 || msg.Error.Code != -32000 || msg.Error.Message != "Invalid Host: evil.example" {
				t.Errorf("%s: %d %.200s, want 403 with the JSON-RPC error `Invalid Host: evil.example`", c.name, r.Status, r.Body)
			}
		}
		port := s.BaseURL[strings.LastIndex(s.BaseURL, ":")+1:]
		for _, host := range []string{"localhost", "localhost:" + port, "127.0.0.1:" + port} {
			if r := mcpPost(t, anon, token, listBody, "Host: "+host); r.Status != 200 {
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
		// reset; this is the first large request of the test on purpose. That the order of the guards
		// is body limit, Host, Origin, token is pinned with a small limit in TestGatewayFaults.
		r := anon.Do(t, "POST", "/mcp", pad(limit+1), mcpJSON, mcpAccept, "Authorization: Bearer "+token, "Connection: close")
		if r.Status != 413 || errorCode(t, r) != "limit_exceeded|request body is too large" {
			t.Fatalf("10 MiB + 1 byte: %d %.200s, want 413 limit_exceeded", r.Status, r.Body)
		}
		env := gwNewServer(t, s, admin, nil, "none")
		ref := env.slug + ".echo"
		// 5 MiB of arguments: above the REST limit and above the SDK's own 4 MiB default.
		big := strings.Repeat("x", 5*1024*1024)
		res := mcpToolsCall(t, anon, token, "call_tool", map[string]any{"tool": ref, "arguments": map[string]any{"text": big}})
		if res["isError"] != nil || !strings.Contains(fmt.Sprint(res["content"]), "xxxxxxxx") {
			t.Fatalf("a 5 MiB call: %.200v", res)
		}
		if n := len(env.up.Calls()); n != 1 {
			t.Fatalf("the upstream saw %d calls, want 1", n)
		}
		// Exactly the limit is accepted.
		if r := anon.Do(t, "POST", "/mcp", pad(limit), mcpJSON, mcpAccept, "Authorization: Bearer "+token); r.Status != 200 {
			t.Errorf("a body of exactly 10 MiB: %d %.200s, want 200", r.Status, r.Body)
		}
	})

	t.Run("a token never opens /api (cross-reference), and /mcp refuses a session cookie without a token", func(t *testing.T) {
		// The browser's session is not a credential of /mcp: only sk_ tokens are.
		r := person.Do(t, "POST", "/mcp", listBody, mcpJSON, mcpAccept)
		if r.Status != 401 || r.Header.Get("Www-Authenticate") != "Bearer" {
			t.Fatalf("a session cookie on /mcp: %d %s", r.Status, r.Body)
		}
	})
}

// ---- the four meta-tools ----

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
	plain := gwNewServer(t, s, admin, []fakes.Tool{
		{Name: "get_file_contents", Description: "Read a file from a mcpplain repository", InputSchema: map[string]any{"type": "object"}},
		{Name: "a.b.c", Description: "A mcpdotted tool whose own name has dots", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "number"}}}},
	}, "none")
	// A shared-secret server: the secret and the header must never appear in an answer.
	shared := gwNewServer(t, s, admin, fakes.Tools("ping"), "shared_secret", fakes.WithHeader("X-API-Key", "key {secret}"), fakes.WithSecret(apiKey))
	// A personal-token server for the group "mcp-users" with the described tool.
	personal := gwNewServer(t, s, admin, []fakes.Tool{
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
	closed := gwNewServer(t, s, admin, fakes.Tools("hidden_tool"), "none", fakes.WithGroups("ops"))
	for _, e := range []*gwServer{plain, shared, personal} {
		catalogWaitDocs(t, s, e.slug, len(e.tools))
	}

	alice := gwNewPerson(t, s)
	carol := gwNewPerson(t, s)
	adminMCP := s.MCP(t, admin.Token(t, "meta-admin"))
	connectURL := s.BaseURL + "/connections"
	if err := savePAT(t, alice.c, personal.slug, "alice-pat"); err != "" {
		t.Fatal(err)
	}
	if err := savePAT(t, carol.c, personal.slug, "carol-pat"); err != "" {
		t.Fatal(err)
	}
	led.note("alice-pat", "carol-pat")
	dave := gwNewPerson(t, s) // no PAT

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
			m := map[string]any{"slug": e.slug, "name": e.slug, "description": "Compat test server " + e.slug, "tools": float64(tools), "connection": connection}
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
		led.record(hits)
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
		if res := led.call(dave.mcp, "call_tool", map[string]any{"tool": ref, "arguments": args}); !res.IsError {
			t.Fatalf("a person without a token must not reach the upstream: %q", catalogText(res))
		}
		if n := len(personal.up.Calls()); n != 0 {
			t.Fatalf("a person without a token reached the upstream %d times", n)
		}
		if res := led.call(carol.mcp, "call_tool", map[string]any{"tool": ref, "arguments": args}); res.IsError {
			// carol's token was marked expired by the earlier subtest: reconnect first (control).
			if got := catalogText(res); !strings.Contains(got, "expired") {
				t.Fatalf("carol: %q", got)
			}
			if err := savePAT(t, carol.c, personal.slug, "carol-pat-2"); err != "" {
				t.Fatal(err)
			}
			led.note("carol-pat-2")
			personal.up.Reset()
			if res := led.call(carol.mcp, "call_tool", map[string]any{"tool": ref, "arguments": args}); res.IsError {
				t.Fatalf("carol after reconnecting: %q", catalogText(res))
			}
			if calls := personal.up.Calls(); len(calls) != 1 || calls[0].Header.Get("Authorization") != "Bearer carol-pat-2" {
				t.Fatalf("carol's call reached the upstream with %v, want her own token", calls)
			}
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
		led.record(hits)
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
		catalogStructured(t, led.call(alice.mcp, "search_tools", map[string]any{"query": "zzzqqq", "servers": []string{plain.slug}}), &empty)
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
		desc := gwNewServer(t, s, admin, tools, "none")
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

func ptr[T any](v T) *T { return &v }

// savePAT saves a personal access token for the person; it returns "" or the failure.
func savePAT(t *testing.T, c *harness.Client, slug, token string) string {
	t.Helper()
	if r := c.Do(t, "PUT", "/api/connections/"+slug+"/pat", map[string]string{"token": token}); r.Status != 204 {
		return fmt.Sprintf("PUT pat of %s: %d %s", slug, r.Status, r.Body)
	}
	return ""
}
