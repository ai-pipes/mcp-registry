package httpx_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// compat: TestPlatformHealthz, the failing side.
func TestHealthzClosedPoolIs503(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	assertFail(t, httpx.Healthz(pool), time.Second)
}

func TestHealthzNilPoolIs503(t *testing.T) {
	assertFail(t, httpx.Healthz(nil), time.Second)
}

// A database that accepts the connection and never answers: the probe gives up after 1.5 s.
func TestHealthzStalledDatabaseIs503Within1500ms(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { <-done; _ = c.Close() }()
		}
	}()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@"+ln.Addr().String()+"/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	assertFail(t, httpx.Healthz(pool), 1500*time.Millisecond+400*time.Millisecond)
}

func assertFail(t *testing.T, h http.Handler, limit time.Duration) {
	t.Helper()
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if d := time.Since(start); d > limit {
		t.Errorf("answered after %v, limit %v", d, limit)
	}
	if w.Code != 503 || w.Body.String() != "fail" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %q %q", w.Code, w.Body, w.Header().Get("Content-Type"))
	}
}
