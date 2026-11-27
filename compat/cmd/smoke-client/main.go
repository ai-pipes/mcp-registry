//go:build compat

// Command smoke-client is the MCP part of scripts/smoke.sh, as an agent would use the gateway:
// connect with a Bearer token, then search_tools -> describe_tool -> call_tool. It checks that the
// gateway exposes exactly the four meta-tools, that the call reached the upstream tool, and that no
// answer the client got contains the upstream secret. The Go replacement of
// apps/server/test/smoke/mcp-client.ts, with the same output lines.
//
// Usage: SMOKE_TOKEN=sk_... SMOKE_UPSTREAM_SECRET=... smoke-client <base-url> <tool-ref>
// Exit codes: 0 as expected, 1 not as expected (yet: the index may still be building), 2 usage.
// Neither the token nor the secret is ever printed; both stay in local variables.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const usage = "Usage: SMOKE_TOKEN=... SMOKE_UPSTREAM_SECRET=... smoke-client <base-url> <tool-ref>"

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	cancel()
	os.Exit(code)
}

// bearer adds the Authorization header to every request of the client.
type bearer struct {
	header string // "Bearer <token>"; never printed (the type has no String and is not formatted)
	next   http.RoundTripper
}

func (b *bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", b.header)
	return b.next.RoundTrip(r)
}

// failure is a check that did not hold: exit 1 with its message.
type failure string

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	token, upstreamKey := getenv("SMOKE_TOKEN"), getenv("SMOKE_UPSTREAM_SECRET")
	if len(args) != 2 || args[0] == "" || args[1] == "" || token == "" || upstreamKey == "" {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	baseURL, ref := strings.TrimSuffix(args[0], "/"), args[1]
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             baseURL + "/mcp",
		HTTPClient:           &http.Client{Transport: &bearer{header: "Bearer " + token, next: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "connect failed")
		return 1
	}
	defer func() { _ = session.Close() }()
	// Every answer is checked for the upstream key before anything of it is printed.
	clean := func(answer any) bool {
		b, err := json.Marshal(answer)
		return err == nil && !strings.Contains(string(b), upstreamKey)
	}
	if f := walk(ctx, session, ref, clean, stdout); f != "" {
		_, _ = fmt.Fprintln(stderr, string(f))
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "client never saw the upstream secret")
	return 0
}

const leaked failure = "the client saw the upstream secret"

// walk is tools/list, then search_tools -> describe_tool -> call_tool. clean reports whether an
// answer is free of the upstream key; an answer that is not fails the walk before it is printed.
func walk(ctx context.Context, session *mcp.ClientSession, ref string, clean func(any) bool, stdout io.Writer) failure {
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return "tools/list failed"
	}
	if !clean(listed) {
		return leaked
	}
	names := make([]string, 0, len(listed.Tools))
	for _, t := range listed.Tools {
		names = append(names, t.Name)
	}
	slices.Sort(names)
	_, _ = fmt.Fprintf(stdout, "tools/list → %s\n", strings.Join(names, ", "))
	if strings.Join(names, ",") != "call_tool,describe_tool,list_servers,search_tools" {
		return "the gateway must expose exactly the 4 meta-tools"
	}

	search, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search_tools", Arguments: map[string]any{"query": "echo text back"}})
	if err != nil {
		return "search_tools failed"
	}
	if !clean(search) {
		return leaked
	}
	hits := searchHits(search)
	shown := make([]string, 0, len(hits))
	for _, h := range hits {
		shown = append(shown, fmt.Sprintf("%s (%.3f)", h.Tool, h.Score))
	}
	if len(shown) == 0 {
		shown = []string{"no results"}
	}
	_, _ = fmt.Fprintf(stdout, "search_tools → %s\n", strings.Join(shown, ", "))
	if search.IsError || len(hits) == 0 || hits[0].Tool != ref {
		return failure("search_tools did not find " + ref + " first")
	}

	described, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "describe_tool", Arguments: map[string]any{"tool": ref}})
	if err != nil || described.IsError {
		return failure("describe_tool(" + ref + ") failed")
	}
	if !clean(described) {
		return leaked
	}
	_, _ = fmt.Fprintf(stdout, "describe_tool(%s) → ok\n", ref)

	called, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "call_tool",
		Arguments: map[string]any{"tool": ref, "arguments": map[string]any{"text": "hello from smoke"}},
	})
	if err != nil {
		return failure("call_tool(" + ref + ") failed")
	}
	if !clean(called) {
		return leaked
	}
	content, _ := json.Marshal(called.Content)
	if called.IsError || !strings.Contains(string(content), "hello from smoke") {
		return failure("call_tool(" + ref + ") did not echo")
	}
	_, _ = fmt.Fprintf(stdout, "call_tool(%s) → %s\n", ref, content)
	return ""
}

type hit struct {
	Tool  string  `json:"tool"`
	Score float64 `json:"score"`
}

// searchHits reads structuredContent.results of a search_tools answer; anything else is no hit.
func searchHits(r *mcp.CallToolResult) []hit {
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		return nil
	}
	var sc struct {
		Results []hit `json:"results"`
	}
	if json.Unmarshal(raw, &sc) != nil {
		return nil
	}
	return sc.Results
}
