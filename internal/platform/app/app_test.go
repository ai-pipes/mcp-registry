package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testMCPLimit is MCP_MAX_BODY_BYTES in these tests: above the 1 MiB of the other routes.
const testMCPLimit = 3 << 20

func testHandler(t *testing.T, mount func(*http.ServeMux, *pgxpool.Pool)) http.Handler {
	t.Helper()
	cfg := config.Config{PublicURL: "http://localhost:3000", TrustProxyHops: 1, MCPMaxBodyBytes: testMCPLimit}
	return handler(cfg, discardLogger(), nil, t.TempDir(), mount)
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Everything except /mcp and /mcp/... is capped at 1 MiB; those have MCP_MAX_BODY_BYTES (a later
// milestone brings the routes, the limit is in the chain already).
func TestBodyLimits(t *testing.T) {
	drain := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			httpx.WriteDomainError(w, r, err)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	h := testHandler(t, func(mux *http.ServeMux, _ *pgxpool.Pool) {
		for _, p := range []string{"/mcp", "/mcp/sub", "/mcpx", "/other"} {
			mux.Handle("POST "+p, drain)
		}
	})
	for _, tc := range []struct {
		path        string
		size, limit int
	}{
		{"/mcp", restBodyLimit + 1, testMCPLimit},
		{"/mcp/sub", restBodyLimit + 1, testMCPLimit},
		{"/mcpx", restBodyLimit, restBodyLimit},
		{"/other", restBodyLimit, restBodyLimit},
	} {
		for _, declared := range []bool{true, false} {
			for _, size := range []int{tc.size, tc.limit, tc.limit + 1} {
				var body io.Reader = strings.NewReader(strings.Repeat("a", size))
				if !declared {
					body = io.NopCloser(body)
				}
				r := httptest.NewRequest("POST", tc.path, body)
				if !declared {
					r.ContentLength = -1
				}
				r.Header.Set("Origin", "http://localhost:3000")
				want := 200
				if size > tc.limit {
					want = 413
				}
				if w := do(h, r); w.Code != want {
					t.Errorf("POST %s, %d bytes, declared=%v: %d, want %d", tc.path, size, declared, w.Code, want)
				}
			}
		}
	}
}

// compat: TestPlatformStaticFilesAndSPA (`/healthz/x`), the TypeScript route is app.get only.
func TestHealthzIsGetOnly(t *testing.T) {
	h := testHandler(t, nil)
	get := do(h, httptest.NewRequest("GET", "/healthz", nil))
	if get.Code != 503 || get.Body.String() != "fail" { // no pool: the probe says fail
		t.Errorf("GET /healthz: %d %q", get.Code, get.Body)
	}
	head := do(h, httptest.NewRequest("HEAD", "/healthz", nil))
	if head.Code != 503 { // net/http drops the body of a HEAD answer itself
		t.Errorf("HEAD /healthz: %d", head.Code)
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/healthz"}, {"GET", "/healthz/x"}, {"GET", "/healthz/"}, {"DELETE", "/healthz"},
	} {
		w := do(h, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != 404 || w.Body.String() != "Not found" {
			t.Errorf("%s %s: %d %q, want the plain 404", tc.method, tc.path, w.Code, w.Body)
		}
	}
}

// Go's ServeMux would redirect these; the TypeScript server answers its not-found.
func TestNonCleanPathsAreNotRedirected(t *testing.T) {
	h := testHandler(t, nil)
	for _, p := range []string{"//healthz", "/x/../healthz", "/healthz/.", "//"} {
		r := httptest.NewRequest("GET", "/x", nil)
		r.URL.Path = p
		w := do(h, r)
		if w.Code != 404 || w.Body.String() != "Not found" || w.Header().Get("Location") != "" {
			t.Errorf("%s: %d %q Location=%q", p, w.Code, w.Body, w.Header().Get("Location"))
		}
	}
}

func TestEveryResponseCarriesTheChainsHeaders(t *testing.T) {
	h := testHandler(t, nil)
	w := do(h, httptest.NewRequest("GET", "/nope", nil))
	if w.Header().Get("X-Request-Id") == "" || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("headers: %v", w.Header())
	}
	if w.Header().Get("Server") != "" || w.Header().Get("X-Powered-By") != "" {
		t.Fatalf("Server or X-Powered-By present: %v", w.Header())
	}
	// CSRF is in the chain: a mutating /api request without an Origin is refused.
	if c := do(h, httptest.NewRequest("POST", "/api/anything", nil)); c.Code != 403 {
		t.Fatalf("POST /api/anything without Origin: %d", c.Code)
	}
}

func TestSignalName(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(Signal("SIGINT"))
	if got := signalName(ctx); got != "SIGINT" {
		t.Fatalf("signalName = %q", got)
	}
	plain, cancelPlain := context.WithCancel(context.Background())
	cancelPlain()
	if got := signalName(plain); got != "SIGTERM" {
		t.Fatalf("signalName for a plain cancel = %q", got)
	}
}
