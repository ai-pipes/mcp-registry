// Package app composes the server: start order, the secrets key check, the HTTP handler and the
// graceful shutdown. It holds no business logic; the modules plug into it in later milestones.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
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
	// callsGrace is how long the gateway's calls in flight get to finish after a shutdown began
	// (the listeners are closed at once); then the remaining ones are cancelled and write their
	// `transport_error` row. Half of shutdownLimit, so the row write still fits inside it. The
	// TypeScript server lets calls shorter than its 10 s limit finish during a deploy. Not a
	// configuration variable.
	callsGrace = 5 * time.Second
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
	listener        net.Listener           // used instead of listening on 0.0.0.0:Config.Port
	modules         func(Runtime) []Module // replaces modules (a stub module); nil means modules
	shutdownTimeout time.Duration          // replaces shutdownLimit when > 0
	callsGrace      time.Duration          // replaces callsGrace when > 0
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
// (1 MiB, and MCP_MAX_BODY_BYTES for /mcp), client address, CSRF; then /healthz, the modules'
// routes (with their prefix middleware), the static files, the SPA shell and the not-found
// answer, in that order. A path under /api or /mcp never reaches the shell.
// pool may be nil (then /healthz answers 503).
func handler(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool, webRoot string, routes []httpx.Route, prefixes []httpx.Prefix) http.Handler {
	health := httpx.Healthz(pool)
	rest := httpx.Static(webRoot, httpx.SPAFallback(webRoot, httpx.NotFound()))
	// The router is not net/http's mux on purpose: that would answer 405 for another method on a
	// known path and redirect non-clean paths (//x, /a/../b), where the TypeScript server answers
	// its ordinary not-found. Whatever the router does not serve goes to the static files.
	modulesRouter := httpx.NewRouter(routes, prefixes, rest)
	// Only GET (and HEAD) /healthz is the probe.
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			health.ServeHTTP(w, r)
			return
		}
		modulesRouter.ServeHTTP(w, r)
	})
	return httpx.Chain(mux,
		httpx.RequestID(logger),
		httpx.SecurityHeaders(),
		bodyLimits(int64(cfg.MCPMaxBodyBytes)),
		httpx.ClientIP(cfg.TrustProxy, cfg.TrustProxyHops),
		httpx.CSRF(cfg.PublicURL),
	)
}

func isMCP(path string) bool { return path == "/mcp" || strings.HasPrefix(path, "/mcp/") }

// bodyLimits caps request bodies: mcpMax (MCP_MAX_BODY_BYTES, httpx.LargeBodyLimit: a longer read
// deadline, and a body without a declared length is cut while the handler reads it, so the guards
// of /mcp answer before it is read) for /mcp and /mcp/..., the 1 MiB default with a read deadline
// for everything else.
func bodyLimits(mcpMax int64) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		rest, mcp := httpx.BodyLimit(restBodyLimit)(next), httpx.LargeBodyLimit(mcpMax)(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isMCP(r.URL.Path) {
				mcp.ServeHTTP(w, r)
				return
			}
			rest.ServeHTTP(w, r)
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
