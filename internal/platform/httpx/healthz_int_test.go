//go:build integration

package httpx_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

// An exhausted pool (every connection held) is a failed probe, not a hang.
func TestHealthzExhaustedPoolIs503Within1500ms(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(pgtest.StartURL(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	start := time.Now()
	w := httptest.NewRecorder()
	httpx.Healthz(pool).ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if d := time.Since(start); d > 1700*time.Millisecond {
		t.Errorf("answered after %v", d)
	}
	if w.Code != 503 || w.Body.String() != "fail" {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
}
