package http

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/modules/search"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// Catalog is what the meta-tools read of the catalog (*catalog.Service).
type Catalog interface {
	ListServersFor(ctx context.Context, p identity.Principal) ([]catalog.MemberServerView, error)
	GetToolFor(ctx context.Context, p identity.Principal, ref string) (catalog.ToolView, error)
}

// Connections is the person's connection list (*connections.Service).
type Connections interface {
	ListConnections(ctx context.Context, p identity.Principal) ([]connections.ConnectionSummary, error)
}

// Search is the tool search (*search.Service).
type Search interface {
	SearchTools(ctx context.Context, query string, limit int, servers []string) ([]search.ToolHit, error)
}

// Gateway is the module's own use cases (*gateway.Service).
type Gateway interface {
	CallTool(ctx context.Context, c gateway.CallToolCommand) (gateway.ToolResult, error)
	ListCalls(ctx context.Context, q gateway.ListCallsQuery) ([]gateway.CallRecord, error)
}

// Deps are the services the routes use; the composition passes the modules' shared services.
type Deps struct {
	Catalog     Catalog
	Connections Connections
	Search      Search
	Gateway     Gateway
}

// Options configure the routes.
type Options struct {
	// PublicURL is PUBLIC_URL: its origin is the only Origin /mcp accepts, its host name is
	// allowed as Host, and the connections link of the meta-tools is built from it.
	PublicURL string
	// AllowedHosts is ALLOWED_HOSTS: further Host names /mcp is served under.
	AllowedHosts []string
	// Version is the server version in serverInfo.
	Version string
	// MaxBodyBytes is MCP_MAX_BODY_BYTES; the composition's body limit already enforces it, the
	// handler reads at most that much again. Zero or less: no second limit.
	MaxBodyBytes int64
	// Logger receives the unexpected failures of the meta-tools; nil is slog's default.
	Logger *slog.Logger
}

// Guards are what the routes take from the identity module.
type Guards struct {
	// Session demands a session (the web API), Admin an administrator after it.
	Session, Admin httpx.Middleware
	// Bearer demands an sk_ token (/mcp only).
	Bearer httpx.Middleware
	// Principal reads the principal a guard stored.
	Principal func(*http.Request) (identity.Principal, bool)
}

// mcpMethods are the HTTP methods routed to /mcp: every one goes through the guards, then POST is
// served and the others are 405 (HEAD is served by the GET route).
var mcpMethods = []string{
	http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	http.MethodOptions, http.MethodTrace,
}

// Routes is everything the gateway serves: /mcp (exactly that path, every method above, behind
// Host, Origin and the bearer token) and GET /api/calls for administrators (the prefix guards every
// path at or under /api/calls, so an unknown one there gets the guard's answer first). The guards
// are identity's.
func Routes(svc *identity.Service, d Deps, o Options) ([]httpx.Route, []httpx.Prefix) {
	return RoutesWith(Guards{
		Session: svc.RequireSession(), Admin: identity.RequireAdmin(), Bearer: svc.RequireBearerToken(), Principal: identity.PrincipalOf,
	}, d, o)
}

// RoutesWith is Routes over the given guards. It panics when PUBLIC_URL has no origin
// (configuration validates it earlier).
func RoutesWith(g Guards, d Deps, o Options) ([]httpx.Route, []httpx.Prefix) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	mcp := MCPHandler(g, d, o)
	routes := make([]httpx.Route, 0, len(mcpMethods)+1)
	for _, m := range mcpMethods {
		routes = append(routes, httpx.Route{Method: m, Path: "/mcp", Handler: mcp})
	}
	routes = append(routes, httpx.Route{Method: http.MethodGet, Path: "/api/calls", Handler: &callsHandler{g: g, gw: d.Gateway}})
	prefixes := []httpx.Prefix{{Path: "/api/calls", Middleware: []httpx.Middleware{g.Session, g.Admin}}}
	return routes, prefixes
}

// MCPHandler is /mcp with its guards, in the reference's order: the Host allow-list, the Origin,
// the bearer token, then the JSON-RPC endpoint. The body limit is the composition's.
func MCPHandler(g Guards, d Deps, o Options) http.Handler {
	origin, ok := httpx.OriginOf(o.PublicURL)
	if !ok {
		panic("gateway http: the public URL is not an absolute URL")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	h := &endpoint{d: d, o: o, principal: g.Principal, log: o.Logger, connectURL: connectURL(o.PublicURL)}
	return httpx.Chain(h, hostCheck(AllowedHosts(o.PublicURL, o.AllowedHosts)), originCheck(origin), g.Bearer)
}
