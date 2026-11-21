// Package http holds the REST routes of the identity module: sign-in (OIDC and one-time links),
// sessions, API tokens and people. The session guards live in the identity root package, which
// this layer may not import; the composition passes them in through Options.
package http

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// Options configure the routes.
type Options struct {
	// CookieSecure sets the Secure attribute of the cookies (COOKIE_SECURE).
	CookieSecure bool
	// OIDCConfigured says whether OIDC_* is set; without it people sign in with a one-time link.
	OIDCConfigured bool
	// Logger receives the unexpected error of the login callback; nil is slog's default.
	Logger *slog.Logger
	// RequireSession and RequireAdmin are identity.RequireSession(deps) and identity.RequireAdmin().
	RequireSession httpx.Middleware
	RequireAdmin   httpx.Middleware
	// PrincipalOf is identity.PrincipalOf: the principal the session guard stored.
	PrincipalOf func(*http.Request) (domain.Principal, bool)
}

type handlers struct {
	d app.Deps
	o Options
}

// Routes is everything identity serves under /api: auth, tokens, users. The prefixes guard every
// path under /api/tokens (session) and /api/users (session, then admin), so an unknown path there
// is answered by the guard, not by a 404. /api/auth has no prefix guard.
func Routes(d app.Deps, o Options) ([]httpx.Route, []httpx.Prefix) {
	if o.RequireSession == nil || o.RequireAdmin == nil || o.PrincipalOf == nil {
		panic("identity/http: Options.RequireSession, RequireAdmin and PrincipalOf are required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	h := &handlers{d: d, o: o}
	fn := func(f func(http.ResponseWriter, *http.Request)) http.Handler { return http.HandlerFunc(f) }
	routes := []httpx.Route{
		{Method: "GET", Path: "/api/auth/methods", Handler: fn(h.methods)},
		{Method: "POST", Path: "/api/auth/link", Handler: fn(h.link)},
		{Method: "GET", Path: "/api/auth/login", Handler: fn(h.login)},
		{Method: "GET", Path: "/api/auth/callback", Handler: fn(h.callback)},
		{Method: "POST", Path: "/api/auth/logout", Handler: fn(h.logout)},
		{Method: "GET", Path: "/api/auth/me", Handler: httpx.Chain(fn(h.me), o.RequireSession)},
		{Method: "GET", Path: "/api/tokens", Handler: fn(h.listTokens)},
		{Method: "POST", Path: "/api/tokens", Handler: fn(h.createToken)},
		{Method: "DELETE", Path: "/api/tokens/{id}", Handler: fn(h.revokeToken)},
		{Method: "GET", Path: "/api/users", Handler: fn(h.listUsers)},
		{Method: "POST", Path: "/api/users/{id}/disabled", Handler: fn(h.setDisabled)},
	}
	prefixes := []httpx.Prefix{
		{Path: "/api/tokens", Middleware: []httpx.Middleware{o.RequireSession}},
		{Path: "/api/users", Middleware: []httpx.Middleware{o.RequireSession, o.RequireAdmin}},
	}
	return routes, prefixes
}

func (h *handlers) principal(w http.ResponseWriter, r *http.Request) (domain.Principal, bool) {
	p, ok := h.o.PrincipalOf(r)
	if !ok {
		httpx.WriteError(w, "unauthenticated", "sign in required")
	}
	return p, ok
}

// writeJSON writes v like JSON.stringify: no HTML escaping, no trailing newline.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		httpx.WriteError(w, "internal", "Internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimRight(buf.Bytes(), "\n"))
}

func noContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

func redirect(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusFound)
}

// iso is JavaScript's Date.toISOString: milliseconds and a Z.
func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func isoPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := iso(*t)
	return &s
}

// uuidShape is zod's z.uuid() (RFC 9562 versions 1-8 and the nil and max UUIDs).
var uuidShape = regexp.MustCompile(`^(?:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`)

// parseID answers an id that is not a UUID with the route's 404: such an id cannot exist, and this
// keeps it away from Postgres.
func parseID(w http.ResponseWriter, r *http.Request, what string) (string, bool) {
	id := httpx.PathParam(r, "id")
	if !uuidShape.MatchString(id) {
		httpx.WriteError(w, domain.CodeNotFound, what+" not found")
		return "", false
	}
	return id, true
}

func itoa(n int) string { return strconv.Itoa(n) }
