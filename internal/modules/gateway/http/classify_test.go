package http_test

// The classification of a POST to /mcp as the reference does it (@modelcontextprotocol/server
// 2.4.0: createMcpHandler's handle, classifyInboundRequest, serveModern,
// validateStandardRequestHeaders and the legacy transport's handlePostRequest), and the cap of a
// JSON-RPC batch at 100 messages.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	gwhttp "github.com/ai-pipes/mcp-registry/internal/modules/gateway/http"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

func batchOf(n int, element string) string {
	return "[" + strings.TrimSuffix(strings.Repeat(element+",", n), ",") + "]"
}

const listElement = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

func TestABatchIsCappedAt100Messages(t *testing.T) {
	e := newEnv(t)
	r := e.post(t, aliceToken, batchOf(100, listElement))
	var answers []json.RawMessage
	if err := json.Unmarshal(r.body, &answers); r.status != 200 || err != nil || len(answers) != 100 {
		t.Fatalf("100 messages: %d %d answers %.200s", r.status, len(answers), r.body)
	}
	for _, n := range []int{101, 5000} {
		r = e.post(t, aliceToken, batchOf(n, listElement))
		if r.status != 400 || string(r.body) != `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Invalid Request: Batch must not exceed 100 messages"},"id":null}` {
			t.Errorf("%d messages: %d %.300s", n, r.status, r.body)
		}
	}
}

// A hostile batch (as many messages as the body limit holds) is refused without decoding it or
// building its answers.
func TestAHostileBatchCostsLittle(t *testing.T) {
	pp := &people{tokens: map[string]identity.Principal{aliceToken: principal("alice", identity.RoleMember)}}
	h := gwhttp.MCPHandler(
		gwhttp.Guards{Bearer: pp.bearer(), Principal: principalOf},
		gwhttp.Deps{Catalog: &fakeCatalog{}, Connections: &fakeConnections{}, Search: &fakeSearch{}, Gateway: &fakeGateway{}},
		gwhttp.Options{PublicURL: "http://localhost:3000", Version: "1.0.1", MaxBodyBytes: 2 << 20},
	)
	body := batchOf(1<<20/(len(listElement)+1), listElement)
	serve := func() int {
		req := httptest.NewRequestWithContext(context.Background(), "POST", "/mcp", strings.NewReader(body))
		req.Host = "localhost:3000"
		req.Header.Set("Authorization", "Bearer "+aliceToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := serve(); code != http.StatusBadRequest {
		t.Fatalf("status %d", code)
	}
	allocs := testing.AllocsPerRun(5, func() { serve() })
	t.Logf("%.0f allocations", allocs)
	if allocs > 5000 {
		t.Errorf("%.0f allocations for a refused batch of 1 MiB", allocs)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	serve()
	runtime.ReadMemStats(&after)
	n := after.TotalAlloc - before.TotalAlloc
	t.Logf("%d bytes allocated", n)
	if n > 16<<20 {
		t.Errorf("%d bytes allocated for a refused batch of 1 MiB", n)
	}
}

func TestTheClassificationFollowsTheReference(t *testing.T) {
	e := newEnv(t)
	capabilities := map[string]any{"io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	env := func(version any) map[string]any {
		meta := map[string]any{"io.modelcontextprotocol/protocolVersion": version}
		for k, v := range capabilities {
			meta[k] = v
		}
		return map[string]any{"_meta": meta}
	}
	call := func(name string) map[string]any {
		p := env("2026-07-28")
		p["name"], p["arguments"] = name, map[string]any{}
		return p
	}
	initialize := initializeBody("2025-11-25")
	modern := []string{"MCP-Protocol-Version: 2026-07-28"}
	for _, c := range []struct {
		name   string
		body   string
		hdr    []string
		status int
		want   string // the exact body, or "" for a served answer
	}{
		// Batches (classifyBatch, then the legacy transport).
		{"an empty batch", `[]`, nil, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: empty JSON-RPC batch"},"id":null}`},
		{"an invalid element", `[` + listElement + `,{"jsonrpc":"2.0"}]`, nil, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: JSON-RPC batch contains an invalid message"},"id":null}`},
		{"a modern element", `[` + rpcBody(1, "tools/list", env("2026-07-28")) + `]`, nil, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: JSON-RPC batches may not contain requests for protocol revision 2026-07-28 or later"},"id":null}`},
		{"the elements are judged in order", `[{"x":1},` + rpcBody(1, "tools/list", env(5)) + `]`, nil, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: JSON-RPC batch contains an invalid message"},"id":null}`},
		{"initialize with another message", `[` + initialize + `,` + listElement + `]`, nil, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Invalid Request: Only one initialization request is allowed"},"id":null}`},
		{"a batch with only the modern header: the transport's version check", `[` + listElement + `]`, modern, 400, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Bad Request: Unsupported protocol version: 2026-07-28 (supported versions: 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05, 2024-10-07)"},"id":null}`},
		{"a lone initialize in a batch with the modern header is served", `[` + initialize + `]`, modern, 200, ""},
		{"a batch of notifications with an unsupported header", `[{"jsonrpc":"2.0","method":"notifications/initialized"}]`, []string{"MCP-Protocol-Version: 1999-01-01"}, 400, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Bad Request: Unsupported protocol version: 1999-01-01 (supported versions: 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05, 2024-10-07)"},"id":null}`},
		// The order of the checks: Content-Type, then the body, then the classifier, then Accept.
		{"Content-Type before Accept", rpcBody(2, "tools/list", nil), []string{"Content-Type: text/plain", "Accept: text/html"}, 415, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Unsupported Media Type: Content-Type must be application/json"},"id":null}`},
		{"Accept before the parse error", "{", []string{"Accept: text/html"}, 406, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Not Acceptable: Client must accept both application/json and text/event-stream"},"id":null}`},
		{"the classifier before Accept", `{"hello":1}`, []string{"Accept: text/html"}, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: the request body is not a valid JSON-RPC message"},"id":null}`},
		{"a JSON value that is no object", `5`, nil, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: the request body is not a valid JSON-RPC message"},"id":null}`},
		{"an unknown top-level key: not a request, the id echoed", `{"jsonrpc":"2.0","id":7,"method":"tools/list","extra":1}`, nil, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: the request body is not a valid JSON-RPC message"},"id":7}`},
		{"a fractional id", `{"jsonrpc":"2.0","id":1.5,"method":"tools/list"}`, nil, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: the request body is not a valid JSON-RPC message"},"id":1.5}`},
		{"params that are no object", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}`, nil, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: the request body is not a valid JSON-RPC message"},"id":1}`},
		// Notifications and responses go through the transport's version check too.
		{"a notification with an unsupported header", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, []string{"MCP-Protocol-Version: 1999-01-01"}, 400, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Bad Request: Unsupported protocol version: 1999-01-01 (supported versions: 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05, 2024-10-07)"},"id":null}`},
		{"a posted response with an unsupported header", `{"jsonrpc":"2.0","id":3,"result":{}}`, []string{"MCP-Protocol-Version: 1999-01-01"}, 400, ""},
		{"a notification without Accept", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, []string{"Accept:"}, 406, ""},
		{"a notification with the modern header: a modern notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, modern, 202, ""},
		{"a notification naming a later modern revision", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, []string{"MCP-Protocol-Version: 2027-01-01"}, 400, `{"jsonrpc":"2.0","error":{"code":-32022,"message":"Unsupported protocol version: 2027-01-01","data":{"supported":["2026-07-28"],"requested":"2027-01-01"}},"id":null}`},
		// A later modern revision in the header makes a claim-less request a missing envelope.
		{"a later modern header without the envelope", rpcBody(4, "tools/list", nil), []string{"MCP-Protocol-Version: 2027-01-01"}, 400, `{"jsonrpc":"2.0","error":{"code":-32602,"message":"Invalid params: the MCP-Protocol-Version header names protocol revision 2027-01-01, but the request is missing the required per-request envelope key(s): _meta","data":{"envelope":{"missing":["_meta"]}}},"id":4}`},
		{"the modern header with a partial envelope", rpcBody(4, "tools/list", map[string]any{"_meta": map[string]any{}}), modern, 400, `{"jsonrpc":"2.0","error":{"code":-32602,"message":"Invalid params: the MCP-Protocol-Version header names protocol revision 2026-07-28, but the request is missing the required per-request envelope key(s): io.modelcontextprotocol/protocolVersion, io.modelcontextprotocol/clientCapabilities","data":{"envelope":{"missing":["io.modelcontextprotocol/protocolVersion","io.modelcontextprotocol/clientCapabilities"]}}},"id":4}`},
		// The envelope.
		{"an envelope without client capabilities", rpcBody(5, "tools/list", map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28"}}), modern, 400, `{"jsonrpc":"2.0","error":{"code":-32602,"message":"Invalid _meta envelope for protocol revision 2026-07-28: io.modelcontextprotocol/clientCapabilities: missing","data":{"envelope":{"key":"io.modelcontextprotocol/clientCapabilities","problem":"missing"}}},"id":5}`},
		{"a claim that is no string", rpcBody(5, "tools/list", env(5)), modern, 400, `{"jsonrpc":"2.0","error":{"code":-32602,"message":"Invalid _meta envelope for protocol revision 2026-07-28: io.modelcontextprotocol/protocolVersion: Invalid input: expected string, received number","data":{"envelope":{"key":"io.modelcontextprotocol/protocolVersion","problem":"Invalid input: expected string, received number"}}},"id":5}`},
		{"an unsupported claimed revision", rpcBody(5, "tools/list", env("2099-01-01")), []string{"MCP-Protocol-Version: 2099-01-01", "Mcp-Method: tools/list"}, 400, `{"jsonrpc":"2.0","error":{"code":-32022,"message":"Unsupported protocol version: 2099-01-01","data":{"supported":["2026-07-28"],"requested":"2099-01-01"}},"id":5}`},
		// The standard headers of a modern request are required.
		{"no MCP-Protocol-Version", rpcBody(6, "tools/list", env("2026-07-28")), []string{"Mcp-Method: tools/list"}, 400, `{"jsonrpc":"2.0","error":{"code":-32020,"message":"Bad Request: the request headers and body disagree: the body envelope names protocol version 2026-07-28 but the required MCP-Protocol-Version header is absent","data":{"mismatch":{"header":"(missing)","body":"the body envelope names protocol version 2026-07-28 but the required MCP-Protocol-Version header is absent"}}},"id":6}`},
		{"no Mcp-Method", rpcBody(6, "tools/list", env("2026-07-28")), modern, 400, `{"jsonrpc":"2.0","error":{"code":-32020,"message":"Bad Request: the request headers and body disagree: the body names method tools/list but the required Mcp-Method header is absent","data":{"mismatch":{"header":"(missing)","body":"the body names method tools/list but the required Mcp-Method header is absent"}}},"id":6}`},
		{"no Mcp-Name", rpcBody(6, "tools/call", call("list_servers")), append([]string{"Mcp-Method: tools/call"}, modern...), 400, `{"jsonrpc":"2.0","error":{"code":-32020,"message":"Bad Request: the request headers and body disagree: the body carries params.name=\"list_servers\" but the required Mcp-Name header is absent","data":{"mismatch":{"header":"(missing)","body":"the body carries params.name=\"list_servers\" but the required Mcp-Name header is absent"}}},"id":6}`},
		{"another Mcp-Name", rpcBody(6, "tools/call", call("list_servers")), append([]string{"Mcp-Method: tools/call", "Mcp-Name: call_tool"}, modern...), 400, `{"jsonrpc":"2.0","error":{"code":-32020,"message":"Bad Request: the request headers and body disagree: the body carries params.name=\"list_servers\" but the Mcp-Name header names \"call_tool\"","data":{"mismatch":{"header":"call_tool","body":"the body carries params.name=\"list_servers\" but the Mcp-Name header names \"call_tool\""}}},"id":6}`},
		{"a Base64 Mcp-Name", rpcBody(6, "tools/call", call("list_servers")), append([]string{"Mcp-Method: tools/call", "Mcp-Name: =?base64?bGlzdF9zZXJ2ZXJz?="}, modern...), 200, ""},
		{"a broken Base64 Mcp-Name", rpcBody(6, "tools/call", call("list_servers")), append([]string{"Mcp-Method: tools/call", "Mcp-Name: =?base64?b!?="}, modern...), 400, `{"jsonrpc":"2.0","error":{"code":-32020,"message":"Bad Request: the request headers and body disagree: the Mcp-Name header carries an invalid Base64 sentinel value","data":{"mismatch":{"header":"=?base64?b!?=","body":"the Mcp-Name header carries an invalid Base64 sentinel value"}}},"id":6}`},
		// A modern request needs no Accept (the modern path does not check it).
		{"a modern request with a one-sided Accept", rpcBody(7, "tools/list", env("2026-07-28")), append([]string{"Mcp-Method: tools/list", "Accept: application/json"}, modern...), 200, ""},
		// An initialize whose claim is not a valid modern one is the classic handshake.
		{"initialize with a classic claim", rpcBody(8, "initialize", map[string]any{"protocolVersion": "2025-06-18", "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2025-06-18"}}), nil, 200, ""},
	} {
		r := e.post(t, aliceToken, c.body, c.hdr...)
		if r.status != c.status || (c.want != "" && string(r.body) != c.want) {
			t.Errorf("%s: %d %s\nwant %d %s", c.name, r.status, r.body, c.status, c.want)
		}
	}
}
