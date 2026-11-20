//go:build integration

package httpx_test

import (
	"net/http/httptest"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
)

// compat: TestPlatformHealthz, the live side.
func TestHealthzLiveDatabase(t *testing.T) {
	pool := pgtest.Start(t)
	w := httptest.NewRecorder()
	httpx.Healthz(pool).ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 || w.Body.String() != "ok" || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("%d %q %q", w.Code, w.Body, w.Header().Get("Content-Type"))
	}
}
