//go:build compat

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testKey = "test-upstream-key-0123456789"

// lockedBuffer is the request log, written from the handler goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// headerTransport adds one header to every request.
type headerTransport struct{ name, value string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.name, h.value)
	return http.DefaultTransport.RoundTrip(r)
}

func TestARequestWithoutTheKeyIsRefusedAndLogged(t *testing.T) {
	log := &lockedBuffer{}
	srv := httptest.NewServer(newHandler("Key "+testKey, log))
	t.Cleanup(srv.Close)
	for _, key := range []string{"", "Key wrong", testKey} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if key != "" {
			req.Header.Set(keyHeader, key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q: status %d, want 401", key, resp.StatusCode)
		}
	}
	if got, want := log.String(), strings.Repeat("request initialize authorized=false\n", 3); got != want {
		t.Errorf("log %q, want %q", got, want)
	}
}

func TestWithTheKeyEchoIsListedAndCalled(t *testing.T) {
	log := &lockedBuffer{}
	srv := httptest.NewServer(newHandler("Key "+testKey, log))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: headerTransport{keyHeader, "Key " + testKey}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 1 || listed.Tools[0].Name != "echo" || listed.Tools[0].Title != "Echo" {
		t.Fatalf("tools/list: %v %+v", err, listed)
	}
	called, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hello from smoke"}})
	if err != nil || called.IsError || len(called.Content) != 1 {
		t.Fatalf("tools/call: %v %+v", err, called)
	}
	if text, ok := called.Content[0].(*mcp.TextContent); !ok || text.Text != `{"text":"hello from smoke"}` {
		t.Fatalf("tools/call content %+v", called.Content[0])
	}
	got := log.String()
	// The session's opening request (initialize, or server/discover for this SDK) is not pinned.
	for _, line := range []string{"request tools/list authorized=true\n", "request tools/call authorized=true\n"} {
		if !strings.Contains(got, line) {
			t.Errorf("log has no %q: %q", line, got)
		}
	}
	if strings.Contains(got, "authorized=false") || strings.Contains(got, testKey) {
		t.Errorf("log %q", got)
	}
}

// A GET or DELETE (no JSON-RPC body) is logged with "-" as its method.
func TestARequestWithoutABodyIsLoggedWithADash(t *testing.T) {
	log := &lockedBuffer{}
	srv := httptest.NewServer(newHandler("Key "+testKey, log))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := log.String(); got != "request - authorized=false\n" {
		t.Errorf("log %q", got)
	}
}
