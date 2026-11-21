package httpx

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const healthzTimeout = 1500 * time.Millisecond

// Healthz is liveness: the database only (`select 1`, at most 1.5 s). Upstream servers and the
// embedding model are deliberately not checked: without them the gateway degrades but stays
// healthy. Answers "ok" 200 or "fail" 503, as text/plain.
func Healthz(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		ctx, cancel := context.WithTimeout(r.Context(), healthzTimeout)
		defer cancel()
		if pool == nil || ping(ctx, pool) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("fail"))
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
}

func ping(ctx context.Context, pool *pgxpool.Pool) error {
	var one int
	return pool.QueryRow(ctx, "select 1").Scan(&one)
}

// NotFound answers an unmatched request: JSON 404 not_found under /api/, text "Not found" elsewhere.
func NotFound() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			WriteError(w, "not_found", "not found")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not found"))
	})
}
