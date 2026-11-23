package app

import (
	"fmt"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway"
	gwhttp "github.com/ai-pipes/mcp-registry/internal/modules/gateway/http"
	gwinfra "github.com/ai-pipes/mcp-registry/internal/modules/gateway/infra"
	"github.com/ai-pipes/mcp-registry/internal/platform/scheduler"
)

// gatewayWiring is the gateway module built from the runtime: call_tool, `/mcp`, `GET /api/calls`
// and the history retention. Building it makes no request.
type gatewayWiring struct {
	rt      Runtime
	id      identityWiring
	cat     catalogWiring
	con     connectionsWiring
	srch    searchWiring
	service *gateway.Service
}

// newGatewayWiring builds the module over the Postgres stores, the other modules' public services
// and the process's one upstream client (shared with the catalog's synchronisation) and ONE
// argument validator (its compiled-schema cache lives in it). A missing piece is a programming
// error of the composition, so it panics at start.
func newGatewayWiring(rt Runtime, id identityWiring, cat catalogWiring, con connectionsWiring, srch searchWiring) gatewayWiring {
	if id.service == nil || cat.service == nil || cat.upstream == nil || con.service == nil || srch.service == nil {
		panic("gateway wiring: the identity, catalog, connections and search wirings are not built")
	}
	stores := gwinfra.NewPg(rt.Pool)
	svc, err := gateway.New(gateway.Deps{
		CallLog:     stores.CallLog,
		Quota:       stores.Quota,
		Catalog:     gwinfra.NewToolCatalog(cat.service),
		Credentials: gwinfra.NewPersonalCredentials(con.service),
		Validator:   gwinfra.NewArgsValidator(rt.Logger),
		Upstream:    cat.upstream,
		Clock:       rt.Clock,
		Logger:      rt.Logger,
		Secrets:     gwinfra.ConstantTimeSecrets{},
		// The links in the agent's texts: `PUBLIC_URL` without trailing slashes (a path is kept).
		PublicURL:        strings.TrimRight(rt.Config.PublicURL, "/"),
		MaxResponseBytes: int64(rt.Config.UpstreamMaxResponseBytes),
		BaseContext:      rt.CallsContext,
	})
	if err != nil {
		panic(fmt.Sprintf("gateway wiring: %v", err))
	}
	return gatewayWiring{rt: rt, id: id, cat: cat, con: con, srch: srch, service: svc}
}

// module is what the gateway adds to the server: `/mcp` and `GET /api/calls` with identity's
// guards, and the retention job (the call log older than CALL_LOG_RETENTION_DAYS and the quota
// counters older than yesterday, at start and then every day). It emits no events and has no
// closer: in-flight calls end through Runtime.CallsContext when the shutdown begins.
func (w gatewayWiring) module() Module {
	routes, prefixes := gwhttp.Routes(w.id.service, gwhttp.Deps{
		Catalog: w.cat.service, Connections: w.con.service, Search: w.srch.service, Gateway: w.service,
	}, gwhttp.Options{
		PublicURL:    w.rt.Config.PublicURL,
		AllowedHosts: w.rt.Config.AllowedHosts,
		Version:      w.rt.Version,
		MaxBodyBytes: int64(w.rt.Config.MCPMaxBodyBytes),
		Logger:       w.rt.Logger,
	})
	return Module{
		Routes:   routes,
		Prefixes: prefixes,
		Jobs: []scheduler.Job{{
			Name:  "gateway-retention",
			Every: gateway.RetentionEvery,
			Run:   w.service.RetentionJob(w.rt.Config.CallLogRetentionDays, w.rt.Logger),
		}},
	}
}
