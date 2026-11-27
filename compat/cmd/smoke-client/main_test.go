//go:build compat

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	testToken = "sk_test-token-0123456789" //nolint:gosec // G101: a made-up token that the test hunts for
	testKey   = "test-upstream-key-0123456789"
	testRef   = "smoke.echo"
)

// gateway is a fake of the gateway's /mcp: the four meta-tools behind a Bearer token. leak puts the
// upstream key into the call_tool answer; firstHit is the tool search_tools ranks first ("" for no
// results).
type gateway struct {
	leak     bool
	firstHit string
	tools    []string
}

func (g gateway) start(t *testing.T) string {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-gateway", Version: "1.0.0"}, nil)
	obj := map[string]any{"type": "object"}
	text := func(s string) []mcp.Content { return []mcp.Content{&mcp.TextContent{Text: s}} }
	handlers := map[string]mcp.ToolHandler{
		"list_servers": func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: text("[]")}, nil
		},
		"search_tools": func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			results := []any{}
			if g.firstHit != "" {
				results = append(results, map[string]any{"tool": g.firstHit, "score": 0.91})
			}
			return &mcp.CallToolResult{Content: text("results"), StructuredContent: map[string]any{"results": results}}, nil
		},
		"describe_tool": func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: text("echo")}, nil
		},
		"call_tool": func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in struct {
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &in)
			out := string(in.Arguments)
			if g.leak {
				out += " " + testKey
			}
			return &mcp.CallToolResult{Content: text(out)}, nil
		},
	}
	names := g.tools
	if names == nil {
		names = []string{"list_servers", "search_tools", "describe_tool", "call_tool"}
	}
	for _, n := range names {
		server.AddTool(&mcp.Tool{Name: n, InputSchema: obj}, handlers[n])
	}
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sdk.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runClient(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, func(k string) string { return env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
}

var goodEnv = map[string]string{"SMOKE_TOKEN": testToken, "SMOKE_UPSTREAM_SECRET": testKey}

func TestTheWholePathPasses(t *testing.T) {
	url := gateway{firstHit: testRef}.start(t)
	code, out, errOut := runClient(t, goodEnv, url, testRef)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, line := range []string{
		"tools/list → call_tool, describe_tool, list_servers, search_tools\n",
		"search_tools → smoke.echo (0.910)\n",
		"describe_tool(smoke.echo) → ok\n",
		"call_tool(smoke.echo) → ",
		"client never saw the upstream secret\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("stdout has no %q:\n%s", line, out)
		}
	}
	if strings.Contains(out+errOut, testToken) || strings.Contains(out+errOut, testKey) {
		t.Errorf("the output carries the token or the key:\n%s%s", out, errOut)
	}
}

func TestNotYetIndexedIsExitOne(t *testing.T) {
	url := gateway{}.start(t)
	code, out, errOut := runClient(t, goodEnv, url, testRef)
	if code != 1 || !strings.Contains(errOut, "search_tools did not find smoke.echo first") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestAnAnswerCarryingTheUpstreamKeyFails(t *testing.T) {
	url := gateway{firstHit: testRef, leak: true}.start(t)
	code, out, errOut := runClient(t, goodEnv, url, testRef)
	if code != 1 || !strings.Contains(errOut, "the client saw the upstream secret") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if strings.Contains(out+errOut, testKey) {
		t.Errorf("the failure printed the key:\n%s%s", out, errOut)
	}
}

func TestMoreOrFewerThanTheFourMetaToolsFails(t *testing.T) {
	url := gateway{firstHit: testRef, tools: []string{"list_servers", "search_tools", "describe_tool", "call_tool", "echo"}}.start(t)
	if code, out, errOut := runClient(t, goodEnv, url, testRef); code != 1 || !strings.Contains(errOut, "exactly the 4 meta-tools") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestAWrongTokenIsExitOneWithoutTheToken(t *testing.T) {
	url := gateway{firstHit: testRef}.start(t)
	env := map[string]string{"SMOKE_TOKEN": "sk_wrong-token-value", "SMOKE_UPSTREAM_SECRET": testKey} //nolint:gosec // G101: a made-up token that the test hunts for
	code, out, errOut := runClient(t, env, url, testRef)
	if code != 1 || strings.Contains(out+errOut, "sk_wrong-token-value") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestUsage(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		args []string
	}{
		{goodEnv, nil},
		{goodEnv, []string{"http://x"}},
		{map[string]string{"SMOKE_UPSTREAM_SECRET": testKey}, []string{"http://x", testRef}},
		{map[string]string{"SMOKE_TOKEN": testToken}, []string{"http://x", testRef}},
	} {
		if code, _, errOut := runClient(t, tc.env, tc.args...); code != 2 || !strings.HasPrefix(errOut, "Usage: ") {
			t.Errorf("%v %v: code=%d stderr=%q", tc.env, tc.args, code, errOut)
		}
	}
}
