// Package http holds the REST routes of the identity module: sign-in (OIDC and one-time links),
// sessions, API tokens and people. The session guards and the principal are the identity root
// package's.
package http

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
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
}

type handlers struct {
	d app.Deps
	o Options
}

// Routes is everything identity serves under /api: auth, tokens, users. The prefixes guard every
// path under /api/tokens (session) and /api/users (session, then admin), so an unknown path there
// is answered by the guard, not by a 404. /api/auth has no prefix guard. The guards are the identity
// root package's (this layer may import its own root).
func Routes(d app.Deps, o Options) ([]httpx.Route, []httpx.Prefix) {
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
		{Method: "GET", Path: "/api/auth/me", Handler: httpx.Chain(fn(h.me), identity.RequireSession(d))},
		{Method: "GET", Path: "/api/tokens", Handler: fn(h.listTokens)},
		{Method: "POST", Path: "/api/tokens", Handler: fn(h.createToken)},
		{Method: "DELETE", Path: "/api/tokens/{id}", Handler: fn(h.revokeToken)},
		{Method: "GET", Path: "/api/users", Handler: fn(h.listUsers)},
		{Method: "POST", Path: "/api/users/{id}/disabled", Handler: fn(h.setDisabled)},
	}
	session := identity.RequireSession(d)
	prefixes := []httpx.Prefix{
		{Path: "/api/tokens", Middleware: []httpx.Middleware{session}},
		{Path: "/api/users", Middleware: []httpx.Middleware{session, identity.RequireAdmin()}},
	}
	return routes, prefixes
}

func (h *handlers) principal(w http.ResponseWriter, r *http.Request) (domain.Principal, bool) {
	p, ok := identity.PrincipalOf(r)
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

// redirect answers 302. Like the TypeScript framework, a location with a character above U+00FF
// (which a header cannot carry) is percent-encoded as JavaScript's encodeURI does; one within
// Latin-1 is sent as it is (an accepted difference: the bytes are UTF-8 here).
func redirect(w http.ResponseWriter, location string) {
	for _, r := range location {
		if r > 0xFF {
			location = encodeURI(location)
			break
		}
	}
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

// encodeURI is JavaScript's encodeURI: everything but letters, digits and ;,/?:@&=+$-_.!~*'()# is
// percent-encoded as UTF-8 bytes.
func encodeURI(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(";,/?:@&=+$-_.!~*'()#", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}
