package http_test

// The protocol of /mcp and the guards in front of it, transcribed from the compat scenarios
// TestMCPProtocolClassic, TestMCPSDKClient, TestMCPSurface and TestMCPGuards
// (compat/mcp_protocol_test.go), plus the wire bodies of the reference that the inventory pins.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	gwhttp "github.com/ai-pipes/mcp-registry/internal/modules/gateway/http"
)

const instruction = "This server is a gateway to the user's services (git hosting, issue tracker and others). When a task needs one of them, call search_tools with a short description of the action, then describe_tool for the chosen tool, then call_tool. If nothing fits, continue without them. If a tool needs a connection, tell the user the link from the error."

var classicRevisions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

var fourTools = []string{"list_servers", "search_tools", "describe_tool", "call_tool"}

func noSession(t *testing.T, what string, r reply) {
	t.Helper()
	if v := r.header.Values("Mcp-Session-Id"); len(v) != 0 {
		t.Fatalf("%s: Mcp-Session-Id %v", what, v)
	}
}

func TestInstructionsAreTheReferenceText(t *testing.T) {
	if gwhttp.Instructions != instruction {
		t.Fatalf("instructions %q", gwhttp.Instructions)
	}
}

func TestClassicInitializeNegotiatesTheRevision(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ asked, want string }{
		{"2024-11-05", "2024-11-05"},
		{"2025-03-26", "2025-03-26"},
		{"2025-06-18", "2025-06-18"},
		{"2025-11-25", "2025-11-25"},
		{"1999-01-01", "2025-11-25"},
		{"2026-07-28", "2025-11-25"},
		{"", "2025-11-25"},
		{"not-a-date", "2025-11-25"},
	} {
		r := e.post(t, aliceToken, initializeBody(c.asked))
		if r.status != 200 || !r.hasMsg || r.msg.Error != nil || r.header.Get("Content-Type") != "application/json" {
			t.Fatalf("initialize %q: %d %s %.200s", c.asked, r.status, r.header.Get("Content-Type"), r.body)
		}
		noSession(t, "initialize", r)
		res := r.msg.Result
		if res["protocolVersion"] != c.want || !slices.Contains(classicRevisions, c.want) {
			t.Errorf("initialize %q: protocolVersion %v, want %q", c.asked, res["protocolVersion"], c.want)
		}
		if !reflect.DeepEqual(res["capabilities"], map[string]any{"tools": map[string]any{"listChanged": false}}) {
			t.Errorf("capabilities %v", res["capabilities"])
		}
		if !reflect.DeepEqual(res["serverInfo"], map[string]any{"name": "mcp-registry", "version": "1.0.1"}) {
			t.Errorf("serverInfo %v", res["serverInfo"])
		}
		if res["instructions"] != instruction {
			t.Errorf("instructions %v", res["instructions"])
		}
	}
	// No params at all: the latest classic revision.
	// Without params it is no initialization request: the in-band error is the zod issues.
	if r := e.post(t, aliceToken, rpcBody(1, "initialize", nil)); r.status != 200 || r.msg.Error == nil || r.msg.Error.Code != -32603 ||
		r.msg.Error.Message != "[\n  {\n    \"expected\": \"object\",\n    \"code\": \"invalid_type\",\n    \"path\": [\n      \"params\"\n    ],\n    \"message\": \"Invalid input: expected object, received undefined\"\n  }\n]" {
		t.Errorf("initialize without params: %.300s", r.body)
	}
}

func TestNotificationsAreAnEmpty202(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`[{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","method":"notifications/x"}]`,
		`{"jsonrpc":"2.0","id":9,"result":{}}`,
	} {
		r := e.post(t, aliceToken, n)
		if r.status != 202 || len(r.body) != 0 || r.header.Get("Content-Type") != "" {
			t.Errorf("%s: %d %q %q", n, r.status, r.header.Get("Content-Type"), r.body)
		}
		noSession(t, n, r)
	}
}

func TestToolsWorkWithoutAHandshakeAndThereIsNoSession(t *testing.T) {
	e := newEnv(t)
	list := e.post(t, aliceToken, rpcBody(2, "tools/list", nil))
	if list.status != 200 || !list.hasMsg || list.msg.Error != nil {
		t.Fatalf("tools/list: %d %s", list.status, list.body)
	}
	noSession(t, "tools/list", list)
	if names := toolNames(list.msg.Result); !slices.Equal(names, fourTools) {
		t.Fatalf("tools %v", names)
	}
	if len(list.msg.Result) != 1 {
		t.Fatalf("a classic tools/list result carries more than the tools: %s", list.body)
	}
	if r := e.post(t, aliceToken, rpcBody(5, "ping", nil)); r.status != 200 || r.msg.Error != nil || r.msg.Result == nil || len(r.msg.Result) != 0 {
		t.Fatalf("ping: %d %s", r.status, r.body)
	}
	call := e.toolsCall(t, aliceToken, "list_servers", map[string]any{})
	if call["isError"] != nil {
		t.Fatalf("list_servers: %v", call)
	}
	if blocks, _ := call["content"].([]any); len(blocks) != 1 {
		t.Fatalf("%d content blocks", len(blocks))
	}
	var fromText any
	if err := json.Unmarshal([]byte(resultText(call)), &fromText); err != nil || !reflect.DeepEqual(fromText, call["structuredContent"]) {
		t.Fatalf("the text %q is not the structuredContent %v", resultText(call), call["structuredContent"])
	}
	// An undeclared argument is ignored; arguments may be omitted.
	if res := e.toolsCall(t, aliceToken, "list_servers", map[string]any{"unknown": 1}); res["isError"] != nil || res["structuredContent"] == nil {
		t.Fatalf("undeclared argument: %v", res)
	}
	if res := e.toolsCall(t, aliceToken, "list_servers", nil); res["isError"] != nil {
		t.Fatalf("no arguments: %v", res)
	}
	// A batch is answered message by message, notifications get no answer.
	r := e.post(t, aliceToken, `[{"jsonrpc":"2.0","id":"a","method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":7,"method":"tools/list"}]`)
	var batch []map[string]any
	if err := json.Unmarshal(r.body, &batch); err != nil || r.status != 200 || len(batch) != 2 || batch[0]["id"] != "a" || batch[1]["id"] != float64(7) {
		t.Fatalf("batch: %d %.300s", r.status, r.body)
	}
}

func TestJSONRPCErrorsCarryTheDocumentedCodes(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name, body string
		code       int
		status     int
		message    string
	}{
		{"unknown method", rpcBody(2, "no/such", nil), -32601, 200, "Method not found"},
		{"resources/list", rpcBody(2, "resources/list", nil), -32601, 200, "Method not found"},
		{"server/discover without the envelope", rpcBody(2, "server/discover", nil), -32601, 200, "Method not found"},
		{"unknown tool", rpcBody(2, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}}), -32602, 200, "Tool nope not found"},
		{"invalid JSON", "{", -32700, 400, "Parse error: Invalid JSON"},
		{"empty body", "", -32700, 400, "Parse error: Invalid JSON"},
		{"not a JSON-RPC message", `{"hello":1}`, -32600, 400, "Bad Request: the request body is not a valid JSON-RPC message"},
		{"an empty batch", `[]`, -32600, 400, "Bad Request: empty JSON-RPC batch"},
		{"tools/call without a name", rpcBody(2, "tools/call", map[string]any{"arguments": map[string]any{}}), -32602, 200, ""},
		{"tools/call with arguments that are not an object", rpcBody(2, "tools/call", map[string]any{"name": "list_servers", "arguments": []int{1}}), -32602, 200, ""},
	} {
		r := e.post(t, aliceToken, c.body)
		if r.status != c.status || !r.hasMsg || r.msg.Error == nil || r.msg.Error.Code != c.code || r.msg.Error.Message == "" ||
			(c.message != "" && r.msg.Error.Message != c.message) {
			t.Errorf("%s: %d %.300s, want %d and error %d %q", c.name, r.status, r.body, c.status, c.code, c.message)
		}
		noSession(t, c.name, r)
	}
	// Accept must name both types; Content-Type must be JSON.
	for _, c := range []struct {
		hdr    string
		status int
		msg    string
	}{
		{"Accept: application/json", 406, "Not Acceptable: Client must accept both application/json and text/event-stream"},
		{"Accept: text/event-stream", 406, "Not Acceptable: Client must accept both application/json and text/event-stream"},
		{"Accept:", 406, "Not Acceptable: Client must accept both application/json and text/event-stream"},
		{"Content-Type: text/plain", 415, "Unsupported Media Type: Content-Type must be application/json"},
		{"Content-Type:", 415, "Unsupported Media Type: Content-Type must be application/json"},
	} {
		r := e.post(t, aliceToken, rpcBody(2, "tools/list", nil), c.hdr)
		if r.status != c.status || r.msg.Error == nil || r.msg.Error.Code != -32000 || r.msg.Error.Message != c.msg || strings.Contains(string(r.body), "list_servers") {
			t.Errorf("%s: %d %.200s", c.hdr, r.status, r.body)
		}
	}
	if r := e.post(t, aliceToken, rpcBody(2, "tools/list", nil), "Content-Type: application/json; charset=utf-8", "Accept: text/event-stream, application/json;q=0.9"); r.status != 200 {
		t.Errorf("a charset and parameters: %d %.200s", r.status, r.body)
	}
	if r := e.post(t, aliceToken, rpcBody(2, "tools/list", nil), "Accept: */*"); r.status != 406 {
		t.Errorf("Accept */*: %d", r.status)
	}
	// The MCP-Protocol-Version header.
	for _, v := range []string{"2025-06-18", "2024-10-07", "2025-11-25"} {
		if r := e.post(t, aliceToken, rpcBody(2, "tools/list", nil), "MCP-Protocol-Version: "+v); r.status != 200 || r.msg.Error != nil {
			t.Errorf("MCP-Protocol-Version %s: %d %.200s", v, r.status, r.body)
		}
	}
	r := e.post(t, aliceToken, rpcBody(2, "tools/list", nil), "MCP-Protocol-Version: 1999-01-01")
	if r.status != 400 || r.msg.Error == nil || r.msg.Error.Message != "Bad Request: Unsupported protocol version: 1999-01-01 (supported versions: 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05, 2024-10-07)" {
		t.Errorf("an unsupported MCP-Protocol-Version: %d %.300s", r.status, r.body)
	}
	// The modern revision named by the header without the per-request envelope.
	if r := e.post(t, aliceToken, rpcBody(2, "tools/list", nil), "MCP-Protocol-Version: 2026-07-28"); r.status != 400 || r.msg.Error == nil || r.msg.Error.Code != -32602 {
		t.Errorf("the modern header without the envelope: %d %.300s", r.status, r.body)
	}
	// An initialize with the modern header is a contradiction.
	if r := e.post(t, aliceToken, initializeBody("2025-11-25"), "MCP-Protocol-Version: 2026-07-28"); r.status != 400 || r.msg.Error == nil || r.msg.Error.Code != -32020 {
		t.Errorf("initialize with the modern header: %d %.300s", r.status, r.body)
	}
}

// fieldNamed is compat's mcpFieldNamed.
func fieldNamed(text, field string) bool {
	text = regexp.MustCompile(`for tool \S+:`).ReplaceAllString(text, "")
	q := regexp.QuoteMeta(field)
	return regexp.MustCompile(`"` + q + `"|/` + q + `\b|(^|[\s(\[])` + q + `:`).MatchString(text)
}

func TestARefusedToolCallIsAnIsErrorResultNamingTheField(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		tool  string
		args  any
		field string
		text  string // the reference's text (zod 4 wording), when the inventory pins it
	}{
		{"search_tools", map[string]any{"query": "x", "limit": 26}, "limit", "limit: Too big: expected number to be <=25"},
		{"search_tools", map[string]any{"query": "x", "limit": 0}, "limit", "limit: Too small: expected number to be >=1"},
		{"search_tools", map[string]any{"query": "x", "limit": 1.5}, "limit", "limit: Invalid input: expected int, received number"},
		{"search_tools", map[string]any{"query": "x", "limit": "3"}, "limit", "limit: Invalid input: expected number, received string"},
		{"search_tools", map[string]any{"query": ""}, "query", "query: Too small: expected string to have >=1 characters"},
		{"search_tools", map[string]any{"query": strings.Repeat("q", 501)}, "query", "query: Too big: expected string to have <=500 characters"},
		{"search_tools", map[string]any{"query": 5}, "query", "query: Invalid input: expected string, received number"},
		{"search_tools", map[string]any{"limit": 3}, "query", "query: Invalid input: expected string, received undefined"},
		{"search_tools", map[string]any{"query": "x", "servers": []string{}}, "servers", "servers: Too small: expected array to have >=1 items"},
		{"search_tools", map[string]any{"query": "x", "servers": slices.Repeat([]string{"a"}, 21)}, "servers", "servers: Too big: expected array to have <=20 items"},
		{"search_tools", map[string]any{"query": "x", "servers": []string{""}}, "servers.0", "servers.0: Too small: expected string to have >=1 characters"},
		{"search_tools", map[string]any{"query": "x", "servers": []string{strings.Repeat("s", 65)}}, "servers.0", "servers.0: Too big: expected string to have <=64 characters"},
		{"search_tools", map[string]any{"query": "x", "servers": "a"}, "servers", "servers: Invalid input: expected array, received string"},
		{"search_tools", map[string]any{"query": "", "limit": 0}, "query", "query: Too small: expected string to have >=1 characters, limit: Too small: expected number to be >=1"},
		{"search_tools", map[string]any{"query": nil}, "query", "query: Invalid input: expected string, received null"},
		{"describe_tool", map[string]any{"tool": 5}, "tool", "tool: Invalid input: expected string, received number"},
		{"describe_tool", map[string]any{"tool": ""}, "tool", "tool: Too small: expected string to have >=1 characters"},
		{"describe_tool", map[string]any{"tool": strings.Repeat("t", 301)}, "tool", "tool: Too big: expected string to have <=300 characters"},
		// zod 4.6.7 counts code points: an astral character is one.
		{"search_tools", map[string]any{"query": strings.Repeat("😀", 501)}, "query", "query: Too big: expected string to have <=500 characters"},
		{"describe_tool", map[string]any{"tool": strings.Repeat("😀", 301)}, "tool", "tool: Too big: expected string to have <=300 characters"},
		{"search_tools", map[string]any{"query": "x", "servers": []string{strings.Repeat("😀", 65)}}, "servers.0", "servers.0: Too big: expected string to have <=64 characters"},
		{"describe_tool", map[string]any{}, "tool", "tool: Invalid input: expected string, received undefined"},
		{"call_tool", map[string]any{"tool": "a.b", "arguments": "no"}, "arguments", "arguments: Invalid input: expected record, received string"},
		{"call_tool", map[string]any{"tool": "a.b", "arguments": []int{}}, "arguments", "arguments: Invalid input: expected record, received array"},
		{"call_tool", map[string]any{"tool": "a.b", "arguments": nil}, "arguments", "arguments: Invalid input: expected record, received null"},
		{"call_tool", map[string]any{"arguments": map[string]any{}}, "tool", "tool: Invalid input: expected string, received undefined"},
		{"call_tool", map[string]any{"tool": true}, "tool", "tool: Invalid input: expected string, received boolean"},
	} {
		res := e.toolsCall(t, aliceToken, c.tool, c.args)
		text := resultText(res)
		want := "Input validation error: Invalid arguments for tool " + c.tool + ": " + c.text
		if res["isError"] != true || text != want || !fieldNamed(text, c.field) || res["structuredContent"] != nil {
			t.Errorf("%s %.60v: %v\n got %q\nwant %q", c.tool, c.args, res, text, want)
		}
	}
	if n := len(e.gw.calls) + len(e.search.calls); n != 0 {
		t.Fatalf("a refused input reached the use cases %d times", n)
	}
	// The boundaries are accepted, code points are counted (zod 4.6.7).
	for _, args := range []map[string]any{{"query": "x", "limit": 25}, {"query": "x", "limit": 1}, {"query": strings.Repeat("q", 500)}, {"query": strings.Repeat("😀", 500)}, {"query": "x", "limit": 2.0}} {
		if res := e.toolsCall(t, aliceToken, "search_tools", args); res["isError"] != nil {
			t.Errorf("search_tools %.60v: %v", args, res)
		}
	}
	if res := e.toolsCall(t, aliceToken, "describe_tool", map[string]any{"tool": strings.Repeat("😀", 300)}); strings.HasPrefix(resultText(res), "Input validation") {
		t.Errorf("300 code points are refused: %v", res)
	}
	if res := e.toolsCall(t, aliceToken, "search_tools", map[string]any{"query": "x", "servers": []string{strings.Repeat("😀", 64)}}); strings.HasPrefix(resultText(res), "Input validation") {
		t.Errorf("a server of 64 code points is refused: %v", res)
	}
}

func TestOnlyPostIsServed(t *testing.T) {
	e := newEnv(t)
	for _, m := range []string{"GET", "DELETE", "HEAD"} {
		for _, extra := range [][]string{nil, {"Mcp-Session-Id: abc"}, {"Accept: text/event-stream"}} {
			r := e.do(t, m, "/mcp", "", append([]string{"Authorization: Bearer " + aliceToken}, extra...)...)
			if r.status != 405 || r.header.Get("Allow") != "POST" || len(r.header.Values("Mcp-Session-Id")) != 0 {
				t.Errorf("%s %v: %d Allow %q %.200s", m, extra, r.status, r.header.Get("Allow"), r.body)
			}
			if m != "HEAD" && string(r.body) != `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Method not allowed."},"id":null}` {
				t.Errorf("%s: body %s", m, r.body)
			}
		}
		if r := e.do(t, m, "/mcp", ""); r.status != 401 {
			t.Errorf("%s without a token: %d, want 401 before 405", m, r.status)
		}
	}
	for _, m := range []string{"PUT", "PATCH", "OPTIONS"} {
		r := e.do(t, m, "/mcp", "", "Authorization: Bearer "+aliceToken)
		if r.status != 405 || strings.Contains(string(r.body), "list_servers") {
			t.Errorf("%s: %d %.200s", m, r.status, r.body)
		}
	}
	// Only the exact path: /mcp/ and /mcp/x are the plain not-found, unguarded.
	for _, p := range []string{"/mcp/", "/mcp/sub", "/mcp/x/y"} {
		if r := e.do(t, "GET", p, ""); r.status != 404 || string(r.body) != "Not found" {
			t.Errorf("GET %s: %d %q", p, r.status, r.body)
		}
	}
}

// ---- the guards ----

func TestWithoutATokenMCPIs401WithABearerChallenge(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, "POST", "/mcp", rpcBody(2, "tools/list", nil), mcpHeaders...)
	if r.status != 401 || r.header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("%d %q %s", r.status, r.header.Get("WWW-Authenticate"), r.body)
	}
	if ok := e.post(t, aliceToken, rpcBody(2, "tools/list", nil)); ok.status != 200 {
		t.Fatalf("with a token: %d", ok.status)
	}
	// A session cookie is not a credential.
	if r := e.do(t, "POST", "/mcp", rpcBody(2, "tools/list", nil), append(mcpHeaders, "Cookie: session=member-session")...); r.status != 401 {
		t.Fatalf("a cookie: %d", r.status)
	}
}

func TestAForeignOriginIsRefusedBeforeTheToken(t *testing.T) {
	e := newEnv(t)
	body := rpcBody(2, "tools/list", nil)
	for _, c := range []struct {
		name string
		hdr  []string
	}{
		{"with a valid token", []string{"Authorization: Bearer " + aliceToken}},
		{"without a token", nil},
		{"with a bad token", []string{"Authorization: Bearer sk_" + strings.Repeat("o", 43)}},
	} {
		for _, origin := range []string{"http://evil.example", e.publicURL + ".evil.example", "http://localhost:1", "https://localhost:" + e.port, e.publicURL + "/"} {
			r := e.do(t, "POST", "/mcp", body, append(append(slices.Clone(mcpHeaders), "Origin: "+origin), c.hdr...)...)
			if r.status != 403 || errorCode(t, r) != "forbidden|origin not allowed" {
				t.Errorf("%s, Origin %s: %d %.200s", c.name, origin, r.status, r.body)
			}
			if strings.Contains(string(r.body), "evil") {
				t.Errorf("the refusal quotes the Origin: %s", r.body)
			}
		}
	}
	for _, origin := range []string{"", e.publicURL, "HTTP://LOCALHOST:" + e.port} {
		hdr := []string{}
		if origin != "" {
			hdr = append(hdr, "Origin: "+origin)
		}
		if r := e.post(t, aliceToken, body, hdr...); (origin == "HTTP://LOCALHOST:"+e.port) != (r.status == 403) {
			t.Errorf("Origin %q: %d %.200s", origin, r.status, r.body)
		}
	}
}

func TestAForeignHostIsRefusedBeforeTheOriginAndTheToken(t *testing.T) {
	e := newEnv(t, "Mcp.Example.com", " ", "intranet")
	body := rpcBody(2, "tools/list", nil)
	for _, c := range []struct {
		name string
		hdr  []string
	}{
		{"with a valid token", []string{"Authorization: Bearer " + aliceToken}},
		{"with a foreign Origin too", []string{"Authorization: Bearer " + aliceToken, "Origin: http://evil.example"}},
		{"without a token", nil},
	} {
		for _, host := range []string{"evil.example", "evil.example:" + e.port, "[::1]:" + e.port, "localhost.:" + e.port, "mcp.example.com.evil"} {
			r := e.do(t, "POST", "/mcp", body, append(append(slices.Clone(mcpHeaders), "Host: "+host), c.hdr...)...)
			var msg map[string]json.RawMessage
			_ = json.Unmarshal(r.body, &msg)
			if _, hasResult := msg["result"]; r.status != 403 || hasResult || strings.Contains(string(r.body), "list_servers") {
				t.Errorf("%s, Host %s: %d %.200s", c.name, host, r.status, r.body)
			}
			if string(r.body) != `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Invalid Host header"},"id":null}` || r.header.Get("Content-Type") != "application/json" {
				t.Errorf("Host %s: the refusal %s %s", host, r.header.Get("Content-Type"), r.body)
			}
		}
	}
	for _, host := range []string{"localhost", "localhost:" + e.port, "127.0.0.1:" + e.port, "LOCALHOST:" + e.port, "mcp.example.com", "MCP.EXAMPLE.COM:443", "intranet:" + e.port} {
		if r := e.post(t, aliceToken, body, "Host: "+host); r.status != 200 {
			t.Errorf("Host %s: %d %.200s", host, r.status, r.body)
		}
	}
}

func TestTheBodyLimitOfMCPIsItsOwn(t *testing.T) {
	e := newEnv(t)
	e.gw.echoArgs = true
	pad := func(n int) string {
		prefix := rpcBody(2, "tools/list", nil)
		return prefix + strings.Repeat(" ", n-len(prefix))
	}
	if r := e.do(t, "POST", "/mcp", pad(testLimit+1), append(mcpHeaders, "Authorization: Bearer "+aliceToken)...); r.status != 413 || errorCode(t, r) != "limit_exceeded|request body is too large" {
		t.Fatalf("limit + 1: %d %.200s", r.status, r.body)
	}
	if r := e.post(t, aliceToken, pad(testLimit)); r.status != 200 {
		t.Fatalf("exactly the limit: %d %.200s", r.status, r.body)
	}
	// A body without a declared length is not read before the guards: an anonymous one over the
	// limit gets the bearer refusal, an authenticated one the same 413.
	chunked := func(body string, hdr ...string) reply {
		req, err := http.NewRequestWithContext(t.Context(), "POST", e.url+"/mcp", io.NopCloser(strings.NewReader(body)))
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range append(append([]string{}, mcpHeaders...), hdr...) {
			k, v, _ := strings.Cut(h, ":")
			req.Header.Set(k, strings.TrimSpace(v))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return reply{status: resp.StatusCode, header: resp.Header, body: b}
	}
	if r := chunked(pad(testLimit + 1)); r.status != 401 || r.header.Get("Www-Authenticate") != "Bearer" {
		t.Fatalf("anonymous, chunked, limit + 1: %d %.200s", r.status, r.body)
	}
	if r := chunked(pad(testLimit+1), "Authorization: Bearer "+aliceToken); r.status != 413 || errorCode(t, r) != "limit_exceeded|request body is too large" {
		t.Fatalf("authenticated, chunked, limit + 1: %d %.200s", r.status, r.body)
	}
	if r := chunked(pad(testLimit), "Authorization: Bearer "+aliceToken); r.status != 200 {
		t.Fatalf("authenticated, chunked, exactly the limit: %d %.200s", r.status, r.body)
	}
	// Arguments above the REST limit go through, as the agent sent them.
	big := strings.Repeat("x", 600*1024)
	res := e.toolsCall(t, aliceToken, "call_tool", map[string]any{"tool": "gw.echo", "arguments": map[string]any{"text": big}})
	if res["isError"] != nil || resultText(res) != `{"text":"`+big+`"}` {
		t.Fatalf("a large call: %.200v", res)
	}
}

// ---- the surface: the golden, the budget and the SDK client ----

func surface(t *testing.T, e *env) (init, list map[string]any) {
	t.Helper()
	i := e.post(t, aliceToken, initializeBody("2025-11-25"))
	l := e.post(t, aliceToken, rpcBody(2, "tools/list", nil))
	if i.msg.Result == nil || l.msg.Result == nil {
		t.Fatalf("no result: %s / %s", i.body, l.body)
	}
	return i.msg.Result, l.msg.Result
}

func TestTheSurfaceEqualsTheGolden(t *testing.T) {
	e := newEnv(t)
	init, list := surface(t, e)
	info, _ := init["serverInfo"].(map[string]any)
	got := map[string]any{
		"serverInfo":   map[string]any{"name": info["name"], "version": "<version>"},
		"instructions": init["instructions"],
		"capabilities": init["capabilities"],
		"tools":        list["tools"],
	}
	raw, err := os.ReadFile("../../../../compat/golden/mcp-surface.json")
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	var gotDoc any
	_ = json.Unmarshal(gotJSON, &gotDoc)
	if !reflect.DeepEqual(gotDoc, want) {
		t.Fatalf("the surface differs from compat/golden/mcp-surface.json:\n got %s", gotJSON)
	}
	// tools/list plus the instructions stay under 8000 UTF-16 units.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"tools": list["tools"]})
	size := len(utf16.Encode([]rune(strings.TrimSpace(buf.String())))) + len(utf16.Encode([]rune(instruction)))
	if size >= 8000 {
		t.Fatalf("tools/list + instructions: %d", size)
	}
	// The dash of "score 0–1" is U+2013 on the wire.
	if r := e.post(t, aliceToken, rpcBody(2, "tools/list", nil)); !strings.Contains(string(r.body), "score 0–1") {
		t.Fatal("the typographic dash is escaped or missing")
	}
}

type bearerTransport struct {
	token  string
	record func(req *http.Request, body []byte)
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && b.record != nil {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		b.record(r, body.Bytes())
		resp.Body = ioNopCloser{bytes.NewReader(body.Bytes())}
	}
	return resp, err
}

type ioNopCloser struct{ *bytes.Reader }

func (ioNopCloser) Close() error { return nil }

// sdkSession opens a session as compat/harness does (Stack.MCPOpen): the go-sdk client over the
// streamable transport, no standalone stream.
func sdkSession(t *testing.T, e *env, token string, record func(*http.Request, []byte)) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "compat-harness", Version: "0"}, nil)
	sess, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             e.url + "/mcp",
		HTTPClient:           &http.Client{Transport: &bearerTransport{token: token, record: record}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestTheSDKClientSeesTheSameSurfaceAsTheWire(t *testing.T) {
	e := newEnv(t)
	type exchange struct {
		header http.Header
		body   string
	}
	var seen []exchange
	sess := sdkSession(t, e, aliceToken, func(r *http.Request, b []byte) { seen = append(seen, exchange{r.Header.Clone(), string(b)}) })
	got := sess.InitializeResult()
	if got.ServerInfo == nil || got.ServerInfo.Name != "mcp-registry" || got.ServerInfo.Version != "1.0.1" || got.Instructions != instruction ||
		got.Capabilities == nil || got.Capabilities.Tools == nil || got.Capabilities.Tools.ListChanged {
		t.Fatalf("initialize result %+v", got)
	}
	listed, err := sess.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(listed.Tools)
	var sdk []any
	_ = json.Unmarshal(raw, &sdk)
	for _, tl := range sdk {
		if a, ok := tl.(map[string]any)["annotations"].(map[string]any); ok && a["idempotentHint"] == false {
			delete(a, "idempotentHint")
		}
	}
	_, list := surface(t, e)
	if !reflect.DeepEqual(sdk, list["tools"]) {
		t.Errorf("the SDK client's tools differ from the wire:\nsdk  %v\nwire %v", sdk, list["tools"])
	}
	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_servers", Arguments: map[string]any{}})
	if err != nil || res.IsError || res.StructuredContent == nil {
		t.Fatalf("list_servers through the SDK client: %+v %v", res, err)
	}
	// The client speaks the 2026-07-28 revision: server/discover, then the requests with the
	// envelope; the answers are JSON with resultType and the server info in _meta.
	if len(seen) < 3 || seen[0].header.Get("Mcp-Method") != "server/discover" {
		t.Fatalf("the client's requests: %v", seen)
	}
	var discover struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(seen[0].body), &discover); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"supportedVersions": []any{"2026-07-28"},
		"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
		"instructions":      instruction,
		"resultType":        "complete",
		"ttlMs":             float64(0),
		"cacheScope":        "private",
		"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "mcp-registry", "version": "1.0.1"}},
	}
	if !reflect.DeepEqual(discover.Result, want) {
		t.Errorf("server/discover %s", seen[0].body)
	}
	for _, x := range seen[1:] {
		var m struct {
			Result map[string]any `json:"result"`
		}
		_ = json.Unmarshal([]byte(x.body), &m)
		if m.Result["resultType"] != "complete" || m.Result["_meta"] == nil {
			t.Errorf("%s: %s", x.header.Get("Mcp-Method"), x.body)
		}
	}
	// An input error through the SDK client is an isError result with the same text.
	bad, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "search_tools", Arguments: map[string]any{"query": "x", "limit": 26}})
	if err != nil || !bad.IsError {
		t.Fatalf("input error through the SDK: %+v %v", bad, err)
	}
	if text := bad.Content[0].(*mcp.TextContent).Text; text != "Input validation error: Invalid arguments for tool search_tools: limit: Too big: expected number to be <=25" {
		t.Fatalf("%q", text)
	}
	// An unknown tool is a protocol error.
	if _, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "nope", Arguments: map[string]any{}}); err == nil || !strings.Contains(err.Error(), "Tool nope not found") {
		t.Fatalf("unknown tool through the SDK: %v", err)
	}
}

func TestTheModernEnvelopeIsCheckedAgainstTheHeaders(t *testing.T) {
	e := newEnv(t)
	envelope := map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "x", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}}
	withName := func(name string) map[string]any {
		p := map[string]any{"name": name, "arguments": map[string]any{}}
		for k, v := range envelope {
			p[k] = v
		}
		return p
	}
	modern := []string{"MCP-Protocol-Version: 2026-07-28"}
	for _, c := range []struct {
		name   string
		body   string
		hdr    []string
		status int
		code   int
	}{
		{"discover", rpcBody(1, "server/discover", envelope), append(slices.Clone(modern), "Mcp-Method: server/discover"), 200, 0},
		{"list", rpcBody(1, "tools/list", envelope), append(slices.Clone(modern), "Mcp-Method: tools/list"), 200, 0},
		{"call", rpcBody(1, "tools/call", withName("list_servers")), append(slices.Clone(modern), "Mcp-Method: tools/call", "Mcp-Name: list_servers"), 200, 0},
		{"no headers: the standard headers are required", rpcBody(1, "tools/list", envelope), nil, 400, -32020},
		{"method mismatch", rpcBody(1, "tools/list", envelope), append(slices.Clone(modern), "Mcp-Method: tools/call"), 400, -32020},
		{"name mismatch", rpcBody(1, "tools/call", withName("list_servers")), append(slices.Clone(modern), "Mcp-Method: tools/call", "Mcp-Name: call_tool"), 400, -32020},
		{"version mismatch", rpcBody(1, "tools/list", envelope), []string{"MCP-Protocol-Version: 2025-11-25", "Mcp-Method: tools/list"}, 400, -32020},
		{"an unsupported modern revision", rpcBody(1, "tools/list", map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2099-01-01", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}), nil, 400, -32022},
		{"an envelope claim that is not a string", rpcBody(1, "tools/list", map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": 5}}), nil, 400, -32602},
		{"ping is not a method of the revision", rpcBody(1, "ping", envelope), append(slices.Clone(modern), "Mcp-Method: ping"), 404, -32601},
		{"initialize is not a method of the revision", rpcBody(1, "initialize", envelope), append(slices.Clone(modern), "Mcp-Method: initialize"), 404, -32601},
		{"a batch with an envelope", "[" + rpcBody(1, "tools/list", envelope) + "]", nil, 400, -32600},
	} {
		r := e.post(t, aliceToken, c.body, c.hdr...)
		if r.status != c.status || (c.code == 0) != (r.msg.Error == nil) || (c.code != 0 && r.msg.Error.Code != c.code) {
			t.Errorf("%s: %d %.300s", c.name, r.status, r.body)
		}
		if c.code == 0 && (r.msg.Result["resultType"] != "complete" || r.msg.Result["_meta"] == nil) {
			t.Errorf("%s: %s", c.name, r.body)
		}
	}
	// A modern notification is accepted with an empty 202.
	if r := e.post(t, aliceToken, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"requestId":1}}`, modern...); r.status != 202 {
		t.Errorf("modern notification: %d %s", r.status, r.body)
	}
}

func TestTheDefaultContextOfTheSDKClientIsNotNeeded(t *testing.T) {
	// The SDK client and the raw requests share one handler: two sessions in parallel work.
	e := newEnv(t)
	done := make(chan error, 2)
	for _, tok := range []string{aliceToken, carolToken} {
		go func() {
			client := mcp.NewClient(&mcp.Implementation{Name: "p", Version: "0"}, nil)
			sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
				Endpoint: e.url + "/mcp", HTTPClient: &http.Client{Transport: &bearerTransport{token: tok}}, DisableStandaloneSSE: true,
			}, nil)
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = sess.Close() }()
			_, err = sess.ListTools(context.Background(), nil)
			done <- err
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(fmt.Errorf("parallel session: %w", err))
		}
	}
}
