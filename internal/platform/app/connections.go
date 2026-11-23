package app

import (
	"fmt"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	conhttp "github.com/ai-pipes/mcp-registry/internal/modules/connections/http"
	coninfra "github.com/ai-pipes/mcp-registry/internal/modules/connections/infra"
)

// oauthTimeout is the limit of one request to an OAuth provider (the reference's 10 s).
const oauthTimeout = 10 * time.Second

// connectionsWiring is the connections module built from the runtime. Building it makes no
// request.
type connectionsWiring struct {
	rt      Runtime
	id      identityWiring
	deps    connections.Deps
	service *connections.Service // the ONE service: it owns the single flight
}

// newConnectionsWiring builds the module over the Postgres stores, the hand-written OAuth client
// and the catalog's public API, and binds the service to the catalog's sync credentials.
func newConnectionsWiring(rt Runtime, id identityWiring, cat catalogWiring) connectionsWiring {
	stores := coninfra.NewPg(rt.Pool)
	return connectionsWiringOver(rt, id, cat, stores.Connections, stores.Pending)
}

// connectionsWiringOver is newConnectionsWiring over the given stores (tests hand in memory ones).
// A missing piece is a programming error of the composition, so it panics at start rather than on
// the first request.
func connectionsWiringOver(rt Runtime, id identityWiring, cat catalogWiring, store connections.ConnectionStore, pending connections.PendingConnects) connectionsWiring {
	if id.service == nil || cat.service == nil || cat.creds == nil {
		panic("connections wiring: the identity and catalog wirings are not built")
	}
	deps := connections.Deps{
		Connections: store,
		Pending:     pending,
		Servers:     coninfra.NewServerDirectory(cat.service),
		OAuth:       coninfra.NewOAuthClient(rt.Clock, oauthTimeout),
		Pkce:        coninfra.NewPkce(),
		Box:         rt.Box,
		Clock:       rt.Clock,
		Audit:       rt.Audit,
		// `new URL('/api/connections/callback', PUBLIC_URL).toString()`: the provider compares the
		// redirect URI exactly.
		RedirectURI: resolveURL(rt.Config.PublicURL, "/api/connections/callback"),
	}
	svc, err := connections.New(deps)
	if err != nil {
		panic(fmt.Sprintf("connections wiring: %v", err))
	}
	// Late binding: the catalog was built first, with an adapter that had no service yet.
	cat.creds.bind(svc)
	// The routes get the service's completed dependencies (the one single flight), not the raw ones.
	return connectionsWiring{rt: rt, id: id, deps: svc.Deps(), service: svc}
}

// module is what connections adds to the server: its routes (the session guard is a prefix
// middleware; the OAuth callback is the one unguarded route, answering with redirects). It emits
// no events and has no job or sweep: expired pending attempts are removed when a new one is
// inserted.
func (w connectionsWiring) module() Module {
	routes, prefixes := conhttp.Routes(w.id.service, w.deps, conhttp.Options{Logger: w.rt.Logger})
	return Module{Routes: routes, Prefixes: prefixes}
}
