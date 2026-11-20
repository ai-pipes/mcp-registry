// Package app composes the server: start order, the secrets key check, the HTTP handler and the
// graceful shutdown. It holds no business logic; the modules plug into it in later milestones.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultWebRoot = "/app/web"
	// outboxInterval is the outbox poll period (OUTBOX_INTERVAL_MS in main.ts).
	outboxInterval = time.Second
	// shutdownLimit is the overall limit of a shutdown (SHUTDOWN_TIMEOUT_MS in main.ts).
	shutdownLimit = 10 * time.Second
	// restBodyLimit caps request bodies of everything except /mcp.
	restBodyLimit = 1 << 20
	// The TypeScript server sets neither; a Go server without them is open to slow clients. There
	// is no read or write timeout: /mcp carries large bodies and long responses.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 60 * time.Second
	// Node's default header limit; Go's default is 1 MiB.
	maxHeaderBytes = 16 << 10
	// connectTimeout is the Node client's default connect timeout.
	connectTimeout = 30 * time.Second
)

// Options configures Run.
type Options struct {
	Config  config.Config
	Version string
	// WebRoot is the built UI; empty means /app/web.
	WebRoot string
	// Logger receives every record; nil means a JSON logger on stdout at Config.LogLevel.
	Logger *slog.Logger

	// Seams for tests in this package.
	listener        net.Listener                        // used instead of listening on 0.0.0.0:Config.Port
	mount           func(*http.ServeMux, *pgxpool.Pool) // extra routes, registered before the catch-all
	shutdownTimeout time.Duration                       // replaces shutdownLimit when > 0
}

// Signal is the cancellation cause of the context given to Run when the process got a signal; its
// text is the signal's name (SIGTERM or SIGINT) and goes into the `shutting down` record.
type Signal string

func (s Signal) Error() string { return "received " + string(s) }

// signalName is the name of the signal that cancelled ctx; SIGTERM when it was cancelled for
// another reason.
func signalName(ctx context.Context) string {
	var s Signal
	if errors.As(context.Cause(ctx), &s) {
		return string(s)
	}
	return string(Signal("SIGTERM"))
}

// handler assembles the HTTP handler: request id and access log, security headers, body limit
// (everything except /mcp, which has its own limit), client address, CSRF; then the routes.
// pool may be nil (then /healthz answers 503).
func handler(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool, webRoot string, mount func(*http.ServeMux, *pgxpool.Pool)) http.Handler {
	health := httpx.Healthz(pool)
	rest := httpx.Static(webRoot, httpx.SPAFallback(webRoot, httpx.NotFound()))
	var extra *http.ServeMux
	if mount != nil {
		extra = http.NewServeMux()
		mount(extra, pool)
	}
	// Dispatch by hand instead of registering patterns: ServeMux would answer 405 for another
	// method on a known path and redirect non-clean paths (//x, /a/../b), where the TypeScript
	// server answers its ordinary not-found. Only GET (and HEAD) /healthz is the probe.
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			health.ServeHTTP(w, r)
		case extra != nil && path.Clean(p) == p && routed(extra, r):
			extra.ServeHTTP(w, r)
		default:
			rest.ServeHTTP(w, r)
		}
	})
	return httpx.Chain(mux,
		httpx.RequestID(logger),
		httpx.SecurityHeaders(),
		exceptMCP(httpx.BodyLimit(restBodyLimit)),
		httpx.ClientIP(cfg.TrustProxy, cfg.TrustProxyHops),
		httpx.CSRF(cfg.PublicURL),
	)
}

// routed reports whether mux has a route for r (a test seam; nothing else is registered in it).
func routed(mux *http.ServeMux, r *http.Request) bool {
	_, pattern := mux.Handler(r)
	return pattern != ""
}

func isMCP(path string) bool { return path == "/mcp" || strings.HasPrefix(path, "/mcp/") }

// exceptMCP applies m to every request except those for /mcp and /mcp/...
func exceptMCP(m httpx.Middleware) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		limited := m(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isMCP(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			limited.ServeHTTP(w, r)
		})
	}
}

func webRootOf(o Options) string {
	if o.WebRoot == "" {
		return defaultWebRoot
	}
	return o.WebRoot
}

func loggerOf(o Options) *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return newStdoutLogger(o.Config.LogLevel)
}
