//go:build compat

// Command smoke-upstream is the upstream MCP server of scripts/smoke.sh, run in its own container
// (Dockerfile target smoke-go, never part of the production image). It serves one `echo` tool on
// 0.0.0.0:8080/mcp (Streamable HTTP, the official Go SDK) and answers 401 to any request without
// `X-Smoke-Key: Key <SMOKE_UPSTREAM_SECRET>`. It writes one line per request to stdout,
// `request <JSON-RPC method or -> authorized=<true|false>`, never the key itself; smoke.sh reads
// those lines. The Go replacement of apps/server/test/smoke/upstream.ts, with the same lines.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	keyHeader      = "X-Smoke-Key"
	maxRequestBody = 1 << 20
)

func main() {
	key := os.Getenv("SMOKE_UPSTREAM_SECRET")
	if key == "" {
		_, _ = fmt.Fprintln(os.Stderr, "SMOKE_UPSTREAM_SECRET is not set")
		os.Exit(2)
	}
	srv := &http.Server{Addr: ":8080", Handler: newHandler("Key "+key, os.Stdout), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	_, _ = fmt.Fprintln(os.Stdout, "smoke upstream listening on :8080")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		_, _ = fmt.Fprintln(os.Stderr, "smoke upstream:", err)
		os.Exit(1)
	}
}

// newHandler is the upstream: the key check and the request log in front of the SDK's handler.
// want is the whole expected header value; it is compared in constant time and never written.
func newHandler(want string, log io.Writer) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "smoke-upstream", Version: "1.0.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Title:       "Echo",
		Description: "Echo the given text back unchanged. Use it to check that calls go through.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
	}, echo)
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		DisableLocalhostProtection: true, // the gateway reaches it as upstream:8080
	})
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(raw, &rpc) // a GET or DELETE has no JSON-RPC body
		method := rpc.Method
		if method == "" {
			method = "-"
		}
		authorized := subtle.ConstantTimeCompare([]byte(r.Header.Get(keyHeader)), []byte(want)) == 1
		mu.Lock()
		_, _ = fmt.Fprintf(log, "request %s authorized=%t\n", method, authorized)
		mu.Unlock()
		if !authorized {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sdk.ServeHTTP(w, r)
	})
}

// echo answers with the arguments as they were sent, as text.
func echo(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.Params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(args)}}}, nil
}
