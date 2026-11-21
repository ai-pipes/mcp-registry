package http

import (
	"context"
	"net/http"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// Guards are what the routes take from the identity module: the middleware that demands a
// session, the one that demands an admin (run after Session, before any body or id is read) and
// the way to read the principal the first one stored.
type Guards struct {
	Session   httpx.Middleware
	Admin     httpx.Middleware
	Principal func(*http.Request) (identity.Principal, bool)
}

// Routes is everything the catalog serves under /api: GET /api/catalog for any signed-in person
// and the administration under /api/servers. The prefixes guard every path at or under them
// (session, then admin for /api/servers), so an unknown path there is answered by the guard, not
// by a 404. The guards are identity's.
func Routes(svc *identity.Service, d app.Deps) ([]httpx.Route, []httpx.Prefix) {
	return RoutesWith(Guards{Session: svc.RequireSession(), Admin: identity.RequireAdmin(), Principal: identity.PrincipalOf}, d)
}

// RoutesWith is Routes over the given guards.
func RoutesWith(g Guards, d app.Deps) ([]httpx.Route, []httpx.Prefix) {
	h := &handlers{g: g, d: d}
	fn := func(f func(http.ResponseWriter, *http.Request)) http.Handler { return http.HandlerFunc(f) }
	routes := []httpx.Route{
		{Method: "GET", Path: "/api/catalog", Handler: fn(h.catalog)},
		{Method: "GET", Path: "/api/servers", Handler: fn(h.listServers)},
		{Method: "POST", Path: "/api/servers", Handler: fn(h.createServer)},
		{Method: "GET", Path: "/api/servers/{slug}", Handler: fn(h.getServer)},
		{Method: "PUT", Path: "/api/servers/{slug}", Handler: fn(h.updateServer)},
		{Method: "PUT", Path: "/api/servers/{slug}/secret", Handler: fn(h.setSecret)},
		{Method: "POST", Path: "/api/servers/{slug}/status", Handler: fn(h.setStatus)},
		{Method: "POST", Path: "/api/servers/{slug}/sync", Handler: fn(h.syncServer)},
		{Method: "GET", Path: "/api/servers/{slug}/tools", Handler: fn(h.listTools)},
		{Method: "PUT", Path: "/api/servers/{slug}/tools/{name}/hidden", Handler: fn(h.setHidden)},
	}
	prefixes := []httpx.Prefix{
		{Path: "/api/catalog", Middleware: []httpx.Middleware{g.Session}},
		{Path: "/api/servers", Middleware: []httpx.Middleware{g.Session, g.Admin}},
	}
	return routes, prefixes
}

type handlers struct {
	g Guards
	d app.Deps
}

func (h *handlers) principal(w http.ResponseWriter, r *http.Request) (app.Principal, bool) {
	p, ok := h.g.Principal(r)
	if !ok {
		httpx.WriteError(w, "unauthenticated", "sign in required")
	}
	return p, ok
}

// detached is the context of a write: the client going away must not leave a change half
// recorded (the aggregate saved, its audit record or its sync state not).
func detached(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

// hasNUL reports a path parameter that no server or tool can have, and Postgres would refuse
// (SQLSTATE 22021): such a request gets the answer for an unknown one.
func hasNUL(parts ...string) bool {
	for _, p := range parts {
		if strings.ContainsRune(p, 0) {
			return true
		}
	}
	return false
}

func notFound(w http.ResponseWriter, message string) { httpx.WriteError(w, "not_found", message) }

func fail(w http.ResponseWriter, r *http.Request, err error) { httpx.WriteDomainError(w, r, err) }

func noContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

func (h *handlers) catalog(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	entries, err := app.ListCatalogFor(r.Context(), h.d, p)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toCatalogDTO(entries))
}

func (h *handlers) listServers(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	views, err := app.ListAllServers(r.Context(), h.d, p)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]serverDTO, 0, len(views))
	for _, v := range views {
		out = append(out, toServerDTO(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) createServer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	in, secret, err := serverRequest(r, true)
	if err != nil {
		fail(w, r, err)
		return
	}
	// A failed first synchronisation does not undo the registration: the view carries its error.
	view, err := app.RegisterServer(detached(r), h.d, app.CreateServerCommand{Principal: p, Input: in, Secret: secret})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toServerDTO(view))
}

func (h *handlers) getServer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	slug := httpx.PathParam(r, "slug")
	if hasNUL(slug) {
		notFound(w, "server not found")
		return
	}
	view, err := app.GetServer(r.Context(), h.d, p, slug)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toServerDTO(view))
}

func (h *handlers) updateServer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	in, _, err := serverRequest(r, false)
	if err != nil {
		fail(w, r, err)
		return
	}
	slug := httpx.PathParam(r, "slug")
	if hasNUL(slug) {
		notFound(w, `server "`+slug+`" not found`)
		return
	}
	view, err := app.UpdateServer(detached(r), h.d, app.UpdateServerCommand{Principal: p, Slug: slug, Fields: in.Fields})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toServerDTO(view))
}

func (h *handlers) setSecret(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	secret, err := secretRequest(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	slug := httpx.PathParam(r, "slug")
	if hasNUL(slug) {
		notFound(w, `server "`+slug+`" not found`)
		return
	}
	if err := app.SetSecret(detached(r), h.d, app.SetSecretCommand{Principal: p, Slug: slug, Secret: secret}); err != nil {
		fail(w, r, err)
		return
	}
	noContent(w)
}

func (h *handlers) setStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	status, err := statusRequest(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	slug := httpx.PathParam(r, "slug")
	if hasNUL(slug) {
		notFound(w, `server "`+slug+`" not found`)
		return
	}
	if err := app.SetStatus(detached(r), h.d, app.SetStatusCommand{Principal: p, Slug: slug, Status: status}); err != nil {
		fail(w, r, err)
		return
	}
	noContent(w)
}

func (h *handlers) syncServer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	slug := httpx.PathParam(r, "slug")
	if hasNUL(slug) {
		notFound(w, `active server "`+slug+`" not found`)
		return
	}
	counts, err := app.SyncServer(detached(r), h.d, app.SyncCommand{Principal: &p, Slug: slug})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, syncDTO{Added: counts.Added, Changed: counts.Changed, Removed: counts.Removed})
}

func (h *handlers) listTools(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	slug := httpx.PathParam(r, "slug")
	if hasNUL(slug) {
		notFound(w, `server "`+slug+`" not found`)
		return
	}
	tools, err := app.ListServerTools(r.Context(), h.d, p, slug)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdminToolDTOs(tools))
}

func (h *handlers) setHidden(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	hidden, err := hiddenRequest(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	slug, name := httpx.PathParam(r, "slug"), httpx.PathParam(r, "name")
	ref := slug + "." + name
	if hasNUL(slug, name) {
		notFound(w, `tool "`+ref+`" not found`)
		return
	}
	if err := app.SetToolHidden(detached(r), h.d, app.SetToolHiddenCommand{Principal: p, Ref: ref, Hidden: hidden}); err != nil {
		fail(w, r, err)
		return
	}
	noContent(w)
}
