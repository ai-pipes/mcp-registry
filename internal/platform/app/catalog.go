package app

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	cathttp "github.com/ai-pipes/mcp-registry/internal/modules/catalog/http"
	catinfra "github.com/ai-pipes/mcp-registry/internal/modules/catalog/infra"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/ai-pipes/mcp-registry/internal/platform/scheduler"
	"github.com/ai-pipes/mcp-registry/internal/platform/upstream"
)

// Operator-facing texts of apps/server/src/platform/scheduler.ts.
const (
	msgCatalogSyncDone = "catalog sync: done"
	msgCatalogSyncFail = "catalog sync: run failed"
)

// catalogWiring is the catalog module built from the runtime. Building it makes no request.
type catalogWiring struct {
	rt       Runtime
	id       identityWiring
	deps     catalog.Deps
	service  *catalog.Service
	interval time.Duration
}

// failedSync is one entry of the `failed` field of the `catalog sync: done` record.
type failedSync struct {
	Server string `json:"server"`
	Code   string `json:"code"`
}

func newCatalogWiring(rt Runtime, id identityWiring) catalogWiring {
	// The one client of the process: it holds no state between operations, and the catalog (the
	// scheduler, the REST sync, the CLI) is the only user until the gateway arrives.
	return catalogWiringOver(rt, id, catinfra.NewPg(rt.Pool), upstream.New(upstream.Options{}))
}

// syncCredentialsFor is the port over identity's service; nil when there is none.
func syncCredentialsFor(id identityWiring) catalog.SyncCredentials {
	if id.service == nil {
		return nil
	}
	return personalSyncCredentials{identity: id.service}
}

// catalogWiringOver builds the module over the given repositories and upstream client (tests hand
// in memory repositories and a fake client). It panics when Deps.Credentials would be nil.
func catalogWiringOver(rt Runtime, id identityWiring, repos catinfra.Repos, client kernel.UpstreamClient) catalogWiring {
	creds := syncCredentialsFor(id)
	if creds == nil {
		// A catalog without credentials would panic at the first personal-auth synchronisation.
		panic("catalog wiring: no sync credentials (the identity service is missing)")
	}
	deps := catalog.Deps{
		Servers: repos.Servers, Tools: repos.Tools,
		Box: rt.Box, Clock: rt.Clock, Audit: rt.Audit,
		Upstream:         client,
		Credentials:      creds,
		MaxResponseBytes: int64(rt.Config.UpstreamMaxResponseBytes),
		Logger:           rt.Logger,
	}
	return catalogWiring{
		rt: rt, id: id, deps: deps, service: catalog.New(deps),
		interval: time.Duration(rt.Config.SyncIntervalHours) * time.Hour,
	}
}

// module is what the catalog adds to the server: its routes with identity's guards, and the
// synchronisation job. Its events (catalog.tools_changed, tools_removed, server_changed) go to
// platform.outbox in the aggregate's transaction; the search module's handlers (registered in
// search.go) deliver them, and the outbox worker starts because that table is not empty.
func (w catalogWiring) module() Module {
	routes, prefixes := cathttp.Routes(w.id.service, w.deps)
	return Module{
		Routes:   routes,
		Prefixes: prefixes,
		Jobs:     []scheduler.Job{{Name: "catalog-sync", Every: w.interval, Run: w.syncAll}},
		Emits:    catalog.EventTypes(),
		Commands: w.commands(),
	}
}

// syncAll is the job of scheduler.ts syncAll: every active server, at start and then every
// SYNC_INTERVAL_HOURS (after the run ends). A failure is logged here and does not stop the
// schedule; one caused by the shutdown is left to the scheduler, which does not log it (and a
// run in flight is cancelled through ctx, with its upstream calls).
func (w catalogWiring) syncAll(ctx context.Context) error {
	outcomes, err := w.service.SyncAllServers(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		w.rt.Logger.Error(msgCatalogSyncFail, logging.ErrAttr(err))
		return nil
	}
	failed := []failedSync{}
	for _, o := range outcomes {
		if !o.OK {
			failed = append(failed, failedSync{Server: o.Slug, Code: o.Code})
		}
	}
	w.rt.Logger.Info(msgCatalogSyncDone, "servers", len(outcomes), "failed", failed)
	return nil
}
