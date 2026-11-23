package app

import (
	"context"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	catinfra "github.com/ai-pipes/mcp-registry/internal/modules/catalog/infra"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	gwinfra "github.com/ai-pipes/mcp-registry/internal/modules/gateway/infra"
)

type ctxKey struct{}

// markedUpstream is an upstream client with an identity (the real client is an empty struct, whose
// pointers all compare equal).
type markedUpstream struct {
	kernel.UpstreamClient
	id int
}

// What the gateway shares with the other modules is pinned by identity: a second connections
// service would lose the single flight (the Postgres lock hides it), a second upstream client
// would be a second configuration of the process, and every value from the configuration must come
// from it.
func TestTheGatewayIsBuiltFromTheProcessesOwnPieces(t *testing.T) {
	rt := bareRuntime(t)
	rt.Config.PublicURL = "https://gw.example.com/base//"
	rt.Config.UpstreamMaxResponseBytes = 123456
	ctx := context.WithValue(context.Background(), ctxKey{}, "calls")
	rt.CallsContext = ctx

	id := newIdentityWiring(rt)
	client := &markedUpstream{id: 1}
	cat := catalogWiringOver(rt, id, catinfra.NewPg(nil), client)
	con := newConnectionsWiring(rt, id, cat)
	gw := newGatewayWiring(rt, id, cat, con, newSearchWiring(rt, cat))

	if got, ok := gw.deps.Credentials.(*connections.Service); !ok || got != con.service {
		t.Errorf("the gateway's credentials are %T %p, want the connections wiring's one service %p", gw.deps.Credentials, got, con.service)
	}
	if gw.deps.Upstream != kernel.UpstreamClient(client) {
		t.Error("the gateway has its own upstream client, not the catalog's")
	}
	if _, ok := gw.deps.Validator.(*gwinfra.ArgsValidator); !ok {
		t.Errorf("validator %T", gw.deps.Validator)
	}
	if gw.deps.MaxResponseBytes != 123456 {
		t.Errorf("MaxResponseBytes = %d, want UPSTREAM_MAX_RESPONSE_BYTES", gw.deps.MaxResponseBytes)
	}
	if gw.deps.PublicURL != "https://gw.example.com/base" {
		t.Errorf("PublicURL = %q", gw.deps.PublicURL)
	}
	if gw.deps.BaseContext != ctx {
		t.Error("BaseContext is not Runtime.CallsContext")
	}
	if gw.deps.Clock == nil || gw.deps.Secrets == nil || gw.deps.CallLog == nil || gw.deps.Quota == nil || gw.deps.Catalog == nil {
		t.Errorf("deps: %+v", gw.deps)
	}
}
