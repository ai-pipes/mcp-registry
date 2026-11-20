package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/config"
)

func testHandler(t *testing.T, mount func(*http.ServeMux)) http.Handler {
	t.Helper()
	cfg := config.Config{PublicURL: "http://localhost:3000", TrustProxyHops: 1}
	return handler(cfg, discardLogger(), nil, t.TempDir(), mount)
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The 1 MiB limit applies to everything except /mcp and /mcp/...; those routes arrive in a later
// milestone with their own limit.
func TestBodyLimitAppliesExceptToMCP(t *testing.T) {
	drain := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	})
	h := testHandler(t, func(mux *http.ServeMux) {
		mux.Handle("POST /mcp", drain)
		mux.Handle("POST /mcp/sub", drain)
		mux.Handle("POST /mcpx", drain)
		mux.Handle("POST /other", drain)
	})
	big := strings.Repeat("a", restBodyLimit+1)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/mcp", 200}, {"/mcp/sub", 200}, {"/mcpx", 413}, {"/other", 413},
	} {
		if w := do(h, httptest.NewRequest("POST", tc.path, strings.NewReader(big))); w.Code != tc.want {
			t.Errorf("POST %s with a known length: %d, want %d", tc.path, w.Code, tc.want)
		}
		// Without a length the body is cut while it is read.
		r := httptest.NewRequest("POST", tc.path, io.NopCloser(strings.NewReader(big)))
		r.ContentLength = -1
		w := do(h, r)
		if tc.want == 200 && w.Code != 200 {
			t.Errorf("POST %s without a length: %d, want 200", tc.path, w.Code)
		}
	}
	small := do(h, httptest.NewRequest("POST", "/other", strings.NewReader("{}")))
	if small.Code != 200 {
		t.Errorf("a small body: %d", small.Code)
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
