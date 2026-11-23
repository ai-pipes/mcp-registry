package http

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

// Guards are what the routes take from the identity module: the middleware that demands a
// session, the way to read the principal it stored and the way to read the session without
// demanding it (the callback answers a missing session with a redirect, not a 401).
type Guards struct {
	Session          httpx.Middleware
	Principal        func(*http.Request) (identity.Principal, bool)
	SessionPrincipal func(*http.Request) (identity.Principal, bool, error)
}

// Options configure the routes.
type Options struct {
	// Logger receives the unexpected error of the callback; nil is slog's default.
	Logger *slog.Logger
}

// Routes is everything the connections module serves under /api/connections. The prefix puts the
// session guard in front of every path at or under it, so an unknown path there is answered by the
// guard, not by a 404; only GET /api/connections/callback is outside it (an unguarded exact
// route), because the provider sends the browser back there and the answer must be a redirect to
// the UI page, never JSON. The guards are identity's.
func Routes(svc *identity.Service, d app.Deps, o Options) ([]httpx.Route, []httpx.Prefix) {
	return RoutesWith(Guards{Session: svc.RequireSession(), Principal: identity.PrincipalOf, SessionPrincipal: svc.SessionPrincipal}, d, o)
}

// RoutesWith is Routes over the given guards.
func RoutesWith(g Guards, d app.Deps, o Options) ([]httpx.Route, []httpx.Prefix) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	h := &handlers{g: g, d: d, log: o.Logger}
	fn := func(f func(http.ResponseWriter, *http.Request)) http.Handler { return http.HandlerFunc(f) }
	routes := []httpx.Route{
		{Method: "GET", Path: "/api/connections/callback", Handler: fn(h.callback), Unguarded: true},
		{Method: "GET", Path: "/api/connections", Handler: fn(h.list)},
		{Method: "POST", Path: "/api/connections/{server}/start", Handler: fn(h.start)},
		{Method: "PUT", Path: "/api/connections/{server}/pat", Handler: fn(h.savePAT)},
		{Method: "DELETE", Path: "/api/connections/{server}", Handler: fn(h.disconnect)},
	}
	prefixes := []httpx.Prefix{{Path: "/api/connections", Middleware: []httpx.Middleware{g.Session}}}
	return routes, prefixes
}

type handlers struct {
	g   Guards
	d   app.Deps
	log *slog.Logger
}

func (h *handlers) principal(w http.ResponseWriter, r *http.Request) (app.Principal, bool) {
	p, ok := h.g.Principal(r)
	if !ok {
		httpx.WriteError(w, "unauthenticated", "sign in required")
	}
	return p, ok
}

// detached is the context of a write: the client going away must not leave a change half
// recorded (the attempt consumed and the connection not stored, a record not written).
func detached(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

// hasNUL reports a path parameter that no server can have, and Postgres would refuse (SQLSTATE
// 22021).
func hasNUL(s string) bool { return strings.ContainsRune(s, 0) }

func notFound(w http.ResponseWriter, slug string) {
	httpx.WriteError(w, "not_found", `server "`+slug+`" not found`)
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

type summaryDTO struct {
	Server   string `json:"server"`
	Name     string `json:"name"`
	AuthKind string `json:"authKind"`
	Status   string `json:"status"`
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	rows, err := app.ListConnections(r.Context(), h.d, app.ListQuery{Principal: p})
	if err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	out := make([]summaryDTO, 0, len(rows))
	for _, s := range rows {
		out = append(out, summaryDTO{Server: s.Server, Name: s.Name, AuthKind: string(s.AuthKind), Status: string(s.Status)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) start(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	slug := httpx.PathParam(r, "server")
	if hasNUL(slug) {
		notFound(w, slug)
		return
	}
	u, err := app.StartConnect(detached(r), h.d, app.StartConnectCommand{Principal: p, Server: slug})
	if err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		URL string `json:"url"`
	}{u})
}

type patRequest struct {
	Token *string `json:"token"`
}

func (h *handlers) savePAT(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	// The body is read before the server is looked at, as in the reference.
	var body patRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	var f httpx.Fields
	token := f.RequiredString("token", body.Token, 1, 4096)
	if err := f.Err(); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	slug := httpx.PathParam(r, "server")
	if hasNUL(slug) {
		notFound(w, slug)
		return
	}
	if err := app.SavePAT(detached(r), h.d, app.SavePATCommand{Principal: p, Server: slug, Token: token}); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) disconnect(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	slug := httpx.PathParam(r, "server")
	// No server has such a name, so nothing is stored under it: the answer for a missing connection.
	if !hasNUL(slug) {
		if err := app.Disconnect(detached(r), h.d, app.DisconnectCommand{Principal: p, Server: slug}); err != nil {
			httpx.WriteDomainError(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// The ?error= codes of the redirect (ConnectCallbackError of the contract). Nothing from the
// request is reflected.
const (
	errUnauthenticated = "unauthenticated"
	errDenied          = "denied"
	errInternal        = "internal"
)

var callbackCodes = map[string]bool{
	"invalid_state": true, "not_found": true, "invalid_auth_kind": true, "exchange_failed": true,
}

// callback is where the provider sends the browser back. Every outcome is a 302 to the UI page,
// so the session is read here instead of demanded by a guard. The query string (the authorization
// code and the state) is never logged: the access log records the path only, and the one error
// record has no message.
func (h *handlers) callback(w http.ResponseWriter, r *http.Request) {
	p, ok, err := h.g.SessionPrincipal(r)
	if err != nil {
		h.internal(w, r, err)
		return
	}
	if !ok {
		toPage(w, "error="+errUnauthenticated)
		return
	}
	// The query is read as the reference reads it (httpx.QueryParam), not with net/url: a code with a raw
	// ; or an invalid escape must connect as it does there.
	rawState, _ := httpx.QueryParam(r.URL.RawQuery, "state")
	rawCode, _ := httpx.QueryParam(r.URL.RawQuery, "code")
	var f httpx.Fields
	state := f.String("state", rawState, 1, 256)
	code := f.String("code", rawCode, 1, 4096)
	// No code: the person declined at the provider, or the answer is broken.
	if f.Err() != nil {
		toPage(w, "error="+errDenied)
		return
	}
	// No attempt can have such a state, and Postgres refuses a text with a NUL or with bytes that
	// are not UTF-8 (the reference's URL parser turns raw bytes into text: an unknown state there).
	if hasNUL(state) || !utf8.ValidString(state) {
		toPage(w, "error=invalid_state")
		return
	}
	res, err := app.CompleteConnect(detached(r), h.d, app.CompleteConnectCommand{Principal: p, State: state, Code: code})
	if err != nil {
		if de, isDomain := kernel.AsDomainError(err); isDomain {
			// An unknown code is exchange_failed, as in the reference.
			if callbackCodes[de.Code] {
				toPage(w, "error="+de.Code)
			} else {
				toPage(w, "error=exchange_failed")
			}
			return
		}
		h.internal(w, r, err)
		return
	}
	toPage(w, "connected="+encodeURIComponent(res.Server))
}

// internal answers a failure nobody planned for with a page, not a JSON 500, and logs it without
// its message (it may quote a value).
func (h *handlers) internal(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Error("connections: connect callback failed", "reqId", httpx.RequestIDOf(r), logging.ErrAttr(err))
	toPage(w, "error="+errInternal)
}

// toPage answers 302 to the connections page of the UI.
func toPage(w http.ResponseWriter, query string) {
	w.Header().Set("Location", "/connections?"+query)
	w.WriteHeader(http.StatusFound)
}

// encodeURIComponent is JavaScript's: everything but letters, digits and -_.!~*'() is
// percent-encoded as UTF-8 bytes.
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}
