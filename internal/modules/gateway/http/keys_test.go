package http_test

// Keys are read as JSON.parse reads them (the reference): case-sensitive, the last duplicate wins,
// and every check (the Mcp-Name and Mcp-Method headers) sees the same parsed message the handler
// runs.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway"
)

const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

func TestTheToolThatRunsIsTheOneTheExactKeyNames(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name   string
		params string
	}{
		{"a case variant beside the key", `{` + modernMeta + `,"name":"list_servers","Name":"call_tool","arguments":{"tool":"plain.x"}}`},
		{"an upper-case variant", `{` + modernMeta + `,"NAME":"call_tool","name":"list_servers","arguments":{"tool":"plain.x"}}`},
		{"a duplicate: the last wins", `{` + modernMeta + `,"name":"call_tool","arguments":{"tool":"plain.x"},"name":"list_servers"}`},
	} {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + c.params + `}`
		hdr := []string{"MCP-Protocol-Version: 2026-07-28", "Mcp-Method: tools/call"}
		r := e.post(t, aliceToken, body, append(hdr, "Mcp-Name: list_servers")...)
		if r.status != 200 || r.msg.Result["structuredContent"] == nil || !strings.Contains(string(r.body), `"servers":[`) {
			t.Errorf("%s: %d %.300s", c.name, r.status, r.body)
		}
		if r := e.post(t, aliceToken, body, append(hdr, "Mcp-Name: call_tool")...); r.status != 400 || r.msg.Error == nil || r.msg.Error.Code != -32020 {
			t.Errorf("%s, header naming the other tool: %d %.300s", c.name, r.status, r.body)
		}
	}
	if len(e.gw.calls) != 0 {
		t.Fatalf("call_tool ran: %+v", e.gw.calls)
	}
	// A name given only as a case variant is no name.
	r := e.post(t, aliceToken, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"Name":"list_servers"}}`)
	if r.status != 200 || r.msg.Error == nil || r.msg.Error.Code != -32602 {
		t.Errorf("Name only: %d %.300s", r.status, r.body)
	}
}

func TestADuplicateMethodIsTheLastOne(t *testing.T) {
	e := newEnv(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` + modernMeta + `},"method":"tools/list"}`
	r := e.post(t, aliceToken, body, "MCP-Protocol-Version: 2026-07-28", "Mcp-Method: tools/list")
	if r.status != 200 || len(toolNames(r.msg.Result)) != 4 {
		t.Fatalf("%d %.300s", r.status, r.body)
	}
	if r := e.post(t, aliceToken, body, "MCP-Protocol-Version: 2026-07-28", "Mcp-Method: tools/call"); r.status != 400 || r.msg.Error == nil || r.msg.Error.Code != -32020 {
		t.Fatalf("header naming the first method: %d %.300s", r.status, r.body)
	}
	// A case variant of an envelope key makes no request.
	if r := e.post(t, aliceToken, `{"jsonrpc":"2.0","id":1,"Method":"tools/list"}`); r.status != 400 || r.msg.Error == nil || r.msg.Error.Code != -32600 {
		t.Fatalf("Method: %d %.300s", r.status, r.body)
	}
}

func TestArgumentsAreReadByTheirExactKeyAndTheLastDuplicateWins(t *testing.T) {
	e := newEnv(t)
	// Arguments is not arguments: call_tool gets none, so its tool is missing.
	res := e.toolsCallRaw(t, `{"name":"call_tool","Arguments":{"tool":"plain.x"}}`)
	if res["isError"] != true || resultText(res) != "Input validation error: Invalid arguments for tool call_tool: tool: Invalid input: expected string, received undefined" {
		t.Errorf("Arguments: %v", res)
	}
	// A meta-tool argument given only as a case variant is missing.
	for _, c := range []struct{ params, text string }{
		{`{"name":"search_tools","arguments":{"Query":"x"}}`, "Input validation error: Invalid arguments for tool search_tools: query: Invalid input: expected string, received undefined"},
		{`{"name":"describe_tool","arguments":{"Tool":"plain.x"}}`, "Input validation error: Invalid arguments for tool describe_tool: tool: Invalid input: expected string, received undefined"},
		{`{"name":"call_tool","arguments":{"Tool":"plain.x","tool":7}}`, "Input validation error: Invalid arguments for tool call_tool: tool: Invalid input: expected string, received number"},
		{`{"name":"search_tools","arguments":{"query":"x","Limit":99,"limit":26}}`, "Input validation error: Invalid arguments for tool search_tools: limit: Too big: expected number to be <=25"},
	} {
		if res := e.toolsCallRaw(t, c.params); res["isError"] != true || resultText(res) != c.text {
			t.Errorf("%s: %v", c.params, res)
		}
	}
	if len(e.gw.calls)+len(e.search.calls) != 0 {
		t.Fatalf("a refused input ran: %v %v", e.gw.calls, e.search.calls)
	}
	// Duplicate arguments (of tools/call and of call_tool): the last wins, its bytes as sent.
	e.gw.result = gateway.ToolResult{Content: json.RawMessage(`[]`)}
	e.toolsCallRaw(t, `{"name":"call_tool","arguments":{"tool":"a.b"},"arguments":{"tool":"plain.x","arguments":{"n":1},"arguments":{"n":2}}}`)
	if len(e.gw.calls) != 1 || e.gw.calls[0].Ref != "plain.x" || string(e.gw.calls[0].Arguments) != `{"n":2}` {
		t.Fatalf("duplicates: %+v", e.gw.calls)
	}
}

func TestInitializeReadsTheExactKey(t *testing.T) {
	e := newEnv(t)
	const rest = `"capabilities":{},"clientInfo":{"name":"x","version":"1"}`
	r := e.post(t, aliceToken, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"ProtocolVersion":"2025-06-18","protocolVersion":"2025-03-26",`+rest+`}}`)
	if r.msg.Result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("ProtocolVersion: %.300s", r.body)
	}
	r = e.post(t, aliceToken, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","protocolVersion":"2025-06-18",`+rest+`}}`)
	if r.msg.Result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("duplicate: %.300s", r.body)
	}
	// Only the case variant: protocolVersion is missing, so this is no initialization request.
	r = e.post(t, aliceToken, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"ProtocolVersion":"2025-06-18",`+rest+`}}`)
	if r.msg.Error == nil || r.msg.Error.Code != -32603 || !strings.Contains(r.msg.Error.Message, `"protocolVersion"`) {
		t.Fatalf("ProtocolVersion only: %.300s", r.body)
	}
}

// toolsCallRaw is a classic tools/call with params as raw JSON.
func (e *env) toolsCallRaw(t *testing.T, params string) map[string]any {
	t.Helper()
	r := e.post(t, aliceToken, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":`+params+`}`)
	if r.status != 200 || r.msg.Result == nil {
		t.Fatalf("tools/call %s: %d %.300s", params, r.status, r.body)
	}
	return r.msg.Result
}
