package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/modules/search"
)

// metaCatalog serves servers and tools per person; closed lists, per user id, the slugs that
// person may not use. reads counts ListServersFor, lookups the GetToolFor refs.
type metaCatalog struct {
	mu      sync.Mutex
	servers []catalog.MemberServerView
	tools   map[string]catalog.ToolView
	closed  map[string]map[string]bool
	listErr error
	toolErr error
	// panicList / panicTool make the reads panic (a bug in a module read).
	panicList, panicTool bool
	reads                int
	lookups              []string
}

func (c *metaCatalog) ListServersFor(_ context.Context, p app.Principal) ([]catalog.MemberServerView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	if c.panicList {
		panic("PANIC-VALUE")
	}
	if c.listErr != nil {
		return nil, c.listErr
	}
	out := []catalog.MemberServerView{}
	for _, s := range c.servers {
		if !c.closed[p.UserID][s.Slug] {
			out = append(out, s)
		}
	}
	return out, nil
}

func (c *metaCatalog) GetToolFor(_ context.Context, p app.Principal, ref string) (catalog.ToolView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lookups = append(c.lookups, ref)
	if c.panicTool {
		panic("PANIC-VALUE")
	}
	if c.toolErr != nil {
		return catalog.ToolView{}, c.toolErr
	}
	if _, _, err := catalog.ParseToolRef(ref); err != nil {
		return catalog.ToolView{}, kernel.NewDomainError(catalog.CodeInvalidToolRef, "invalid tool ref")
	}
	t, ok := c.tools[ref]
	if !ok || c.closed[p.UserID][t.Server] {
		return catalog.ToolView{}, kernel.NewDomainError(catalog.CodeNotFound, "tool not found")
	}
	return t, nil
}

type metaConnections struct {
	byUser map[string][]connections.ConnectionSummary
	err    error
	panics bool
}

func (f *metaConnections) ListConnections(_ context.Context, p app.Principal) ([]connections.ConnectionSummary, error) {
	if f.panics {
		panic("PANIC-VALUE")
	}
	return f.byUser[p.UserID], f.err
}

type metaSearch struct {
	hits  []search.ToolHit
	err   error
	calls []struct {
		query   string
		limit   int
		servers []string
	}
}

func (f *metaSearch) SearchTools(_ context.Context, query string, limit int, servers []string) ([]search.ToolHit, error) {
	f.calls = append(f.calls, struct {
		query   string
		limit   int
		servers []string
	}{query, limit, servers})
	if f.err != nil {
		return nil, f.err
	}
	out := []search.ToolHit{}
	for _, h := range f.hits {
		for _, s := range servers {
			if h.Server == s && len(out) < limit {
				out = append(out, h)
			}
		}
	}
	return out, nil
}

var alice = app.Principal{UserID: "alice", Role: identity.RoleMember, Groups: []string{}, Via: identity.ViaToken}

func metaFixture() (app.MetaDeps, *metaCatalog, *metaSearch) {
	title := "Create issue"
	cat := &metaCatalog{
		servers: []catalog.MemberServerView{
			{Slug: "closed", Name: "Closed", ToolCount: 1, AuthKind: catalog.AuthNone},
			{Slug: "personal", Name: "Personal", ToolCount: 2, AuthKind: catalog.AuthPATUser},
			{Slug: "plain", Name: "Plain", Description: "d", ToolCount: 1, AuthKind: catalog.AuthNone},
			{Slug: "token", Name: "Token", ToolCount: 1, AuthKind: catalog.AuthOAuthUser},
		},
		tools: map[string]catalog.ToolView{
			"personal.create": {Server: "personal", Name: "create", Ref: "personal.create", Title: &title, Description: "full text", InputSchema: json.RawMessage(`{"type":"object"}`)},
			"plain.a.b":       {Server: "plain", Name: "a.b", Ref: "plain.a.b", Description: "dotted"},
			"closed.x":        {Server: "closed", Name: "x", Ref: "closed.x"},
		},
		closed: map[string]map[string]bool{"alice": {"closed": true}},
	}
	con := &metaConnections{byUser: map[string][]connections.ConnectionSummary{
		"alice": {{Server: "personal", Status: connections.SummaryReady}, {Server: "token", Status: connections.SummaryExpired}},
	}}
	srch := &metaSearch{}
	return app.MetaDeps{Catalog: cat, Connections: con, Search: srch, PublicURL: "https://gw.example/"}, cat, srch
}

func TestListServersIsTheUsableServersWithTheirConnectionState(t *testing.T) {
	d, _, _ := metaFixture()
	got, err := app.ListServers(t.Context(), d, alice)
	if err != nil {
		t.Fatal(err)
	}
	want := []app.UsableServer{
		{Slug: "personal", Name: "Personal", Tools: 2, Connection: domain.ConnectionReady},
		{Slug: "plain", Name: "Plain", Description: "d", Tools: 1, Connection: domain.ConnectionNotRequired},
		{Slug: "token", Name: "Token", Tools: 1, Connection: domain.ConnectionExpired, ConnectURL: "https://gw.example/connections"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// No connection entry: needs_connection, with the link.
	d.Connections = &metaConnections{}
	got, _ = app.ListServers(t.Context(), d, alice)
	if got[0].Connection != domain.ConnectionNeedsConnection || got[0].ConnectURL == "" {
		t.Fatalf("%+v", got[0])
	}
	// Either read failing is the error.
	for _, broken := range []func(*app.MetaDeps){
		func(d *app.MetaDeps) { d.Catalog.(*metaCatalog).listErr = errors.New("db") },
		func(d *app.MetaDeps) { d.Connections = &metaConnections{err: errors.New("db")} },
	} {
		d, _, _ := metaFixture()
		broken(&d)
		if _, err := app.ListServers(t.Context(), d, alice); err == nil {
			t.Error("a failed read is not an error")
		}
	}
	d.Catalog.(*metaCatalog).servers = nil
	if got, _ := app.ListServers(t.Context(), d, alice); got == nil || len(got) != 0 {
		t.Errorf("no servers: %#v", got)
	}
}

func TestSearchToolsSearchesUsableServersAndRechecksEveryHit(t *testing.T) {
	d, cat, srch := metaFixture()
	srch.hits = []search.ToolHit{
		{Ref: "personal.create", Server: "personal", Score: 0.9},
		{Ref: "plain.gone", Server: "plain", Score: 0.8}, // the index lags the catalog
		{Ref: "plain.a.b", Server: "plain", Score: 0.7},
	}
	res, err := app.SearchTools(t.Context(), d, app.SearchToolsQuery{Principal: alice, Query: " q ", Limit: 2})
	if err != nil || res.Refused != "" {
		t.Fatal(res, err)
	}
	// The limit applies in the index, before the re-check: two hits, one dropped.
	title := "Create issue"
	want := []app.ToolHit{{Ref: "personal.create", Server: "personal", Title: &title, Description: "full text", Score: 0.9, Connection: domain.ConnectionReady}}
	if !reflect.DeepEqual(res.Hits, want) {
		t.Fatalf("hits %+v", res.Hits)
	}
	if c := srch.calls[0]; c.query != " q " || c.limit != 2 || !reflect.DeepEqual(c.servers, []string{"personal", "plain", "token"}) {
		t.Fatalf("search %+v", c)
	}
	// Named servers: de-duplicated in their order.
	if _, err := app.SearchTools(t.Context(), d, app.SearchToolsQuery{Principal: alice, Query: "q", Limit: 10, Servers: []string{"plain", "personal", "plain"}}); err != nil {
		t.Fatal(err)
	}
	if c := srch.calls[1]; !reflect.DeepEqual(c.servers, []string{"plain", "personal"}) {
		t.Fatalf("servers %v", c.servers)
	}
	// An unknown or closed server is refused (the first in argument order) before the index.
	for text, servers := range map[string][]string{
		`Unknown server "jira". Call list_servers.`:   {"plain", "jira", "nope"},
		`Unknown server "closed". Call list_servers.`: {"closed"},
	} {
		res, err := app.SearchTools(t.Context(), d, app.SearchToolsQuery{Principal: alice, Query: "q", Limit: 10, Servers: servers})
		if err != nil || res.Refused != text || len(res.Hits) != 0 {
			t.Errorf("%v: %+v %v", servers, res, err)
		}
	}
	if len(srch.calls) != 2 {
		t.Fatalf("a refused search reached the index: %d", len(srch.calls))
	}
	// A hit whose server is not in the person's list (the catalog still offers the tool) falls back
	// to needs_connection.
	cat.tools["ghost.t"] = catalog.ToolView{Server: "ghost", Name: "t", Ref: "ghost.t"}
	ghost := app.MetaDeps{Catalog: cat, Connections: d.Connections, Search: &fixedSearch{[]search.ToolHit{{Ref: "ghost.t", Server: "ghost", Score: 0.5}}}, PublicURL: d.PublicURL}
	res, _ = app.SearchTools(t.Context(), ghost, app.SearchToolsQuery{Principal: alice, Query: "q", Limit: 10})
	if len(res.Hits) != 1 || res.Hits[0].Connection != domain.ConnectionNeedsConnection {
		t.Fatalf("fallback: %+v", res.Hits)
	}
	// Nothing found is an empty list; failures are errors.
	d, cat, srch = metaFixture()
	if res, err := app.SearchTools(t.Context(), d, app.SearchToolsQuery{Principal: alice, Query: "q", Limit: 10}); err != nil || res.Hits == nil || len(res.Hits) != 0 {
		t.Fatalf("empty: %#v %v", res, err)
	}
	srch.err = errors.New("fts")
	if _, err := app.SearchTools(t.Context(), d, app.SearchToolsQuery{Principal: alice, Query: "q", Limit: 10}); err == nil {
		t.Fatal("a failed search is not an error")
	}
	srch.err, srch.hits, cat.toolErr = nil, []search.ToolHit{{Ref: "plain.a.b", Server: "plain"}}, errors.New("db")
	if _, err := app.SearchTools(t.Context(), d, app.SearchToolsQuery{Principal: alice, Query: "q", Limit: 10}); err == nil {
		t.Fatal("a failed re-check is not an error")
	}
	cat.toolErr, cat.listErr = nil, errors.New("db")
	if _, err := app.SearchTools(t.Context(), d, app.SearchToolsQuery{Principal: alice, Query: "q", Limit: 10}); err == nil {
		t.Fatal("a failed server read is not an error")
	}
}

// fixedSearch returns its hits whatever is asked.
type fixedSearch struct{ hits []search.ToolHit }

func (f *fixedSearch) SearchTools(context.Context, string, int, []string) ([]search.ToolHit, error) {
	return f.hits, nil
}

func TestDescribeToolIsTheToolOrTheNotFoundTextBeforeTheServersAreRead(t *testing.T) {
	d, cat, _ := metaFixture()
	res, err := app.DescribeTool(t.Context(), d, alice, "personal.create")
	if err != nil || res.Tool == nil || res.Refused != "" {
		t.Fatal(res, err)
	}
	if got := res.Tool; got.Ref != "personal.create" || got.Description != "full text" || got.Connection != domain.ConnectionReady || got.OutputSchema != nil || string(got.InputSchema) != `{"type":"object"}` {
		t.Fatalf("%+v", got)
	}
	// A tool stored without an input schema is described as an object schema.
	res, _ = app.DescribeTool(t.Context(), d, alice, "plain.a.b")
	if string(res.Tool.InputSchema) != `{"type":"object"}` || res.Tool.Connection != domain.ConnectionNotRequired {
		t.Fatalf("%+v", res.Tool)
	}
	reads := cat.reads
	for _, ref := range []string{"closed.x", "plain.a", "nodot", "Bad.x"} {
		res, err := app.DescribeTool(t.Context(), d, alice, ref)
		if err != nil || res.Tool != nil || res.Refused != `Tool "`+ref+`" is not available. Use search_tools to find another.` {
			t.Errorf("%s: %+v %v", ref, res, err)
		}
	}
	if cat.reads != reads {
		t.Fatal("the servers were read for a tool that is not offered")
	}
	cat.toolErr = errors.New("db")
	if _, err := app.DescribeTool(t.Context(), d, alice, "plain.a.b"); err == nil {
		t.Fatal("a failed lookup is not an error")
	}
	cat.toolErr, cat.listErr = nil, errors.New("db")
	if _, err := app.DescribeTool(t.Context(), d, alice, "plain.a.b"); err == nil {
		t.Fatal("a failed server read is not an error")
	}
}

// A panic in a read that runs on a goroutine of its own is the use case's ordinary error, never
// the end of the process (net/http recovers only the handler's goroutine).
func TestAPanicInAParallelReadIsAnError(t *testing.T) {
	for name, broken := range map[string]func(*app.MetaDeps){
		"servers":     func(d *app.MetaDeps) { d.Catalog.(*metaCatalog).panicList = true },
		"connections": func(d *app.MetaDeps) { d.Connections = &metaConnections{panics: true} },
	} {
		t.Run(name, func(t *testing.T) {
			d, _, _ := metaFixture()
			broken(&d)
			if _, err := app.ListServers(t.Context(), d, alice); err == nil || strings.Contains(err.Error(), "PANIC-VALUE") {
				t.Fatalf("err %v", err)
			}
		})
	}
	d, cat, srch := metaFixture()
	srch.hits = []search.ToolHit{{Ref: "personal.create", Server: "personal", Score: 0.9}}
	cat.panicTool = true
	if _, err := app.SearchTools(t.Context(), d, app.SearchToolsQuery{Principal: alice, Query: "q", Limit: 2}); err == nil || strings.Contains(err.Error(), "PANIC-VALUE") {
		t.Fatalf("err %v", err)
	}
}
