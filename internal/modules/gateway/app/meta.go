package app

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
)

// The use cases of the meta-tools list_servers, search_tools and describe_tool (call_tool is
// CallTool). They return plain values; the transport renders them.

// UsableServer is one server a person may use, as list_servers shows it.
type UsableServer struct {
	Slug        string
	Name        string
	Description string
	Tools       int
	Connection  domain.AgentConnection
	// ConnectURL is the connections page when the person has to act (needs_connection, expired);
	// "" otherwise.
	ConnectURL string
}

// usable is the servers a person may use, in the catalog's order, with their connection states.
type usable struct {
	list []UsableServer
	by   map[string]domain.AgentConnection
}

// connectionOf is the state of a server; one that is not in the list (cannot normally happen for a
// re-checked tool) is needs_connection.
func (u usable) connectionOf(slug string) domain.AgentConnection {
	if c, ok := u.by[slug]; ok {
		return c
	}
	return domain.ConnectionNeedsConnection
}

// usableServers reads the person's servers and connections, the two reads in parallel.
func usableServers(ctx context.Context, d MetaDeps, p Principal) (usable, error) {
	var (
		wg              sync.WaitGroup
		servers         []catalog.MemberServerView
		conns           []connectionsSummary
		srvErr, connErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer recoverInto(&srvErr)
		servers, srvErr = d.Catalog.ListServersFor(ctx, p)
	}()
	go func() {
		defer wg.Done()
		defer recoverInto(&connErr)
		conns, connErr = listConnections(ctx, d, p)
	}()
	wg.Wait()
	if srvErr != nil {
		return usable{}, srvErr
	}
	if connErr != nil {
		return usable{}, connErr
	}
	status := map[string]domain.PersonalStatus{}
	for _, c := range conns {
		status[c.server] = c.status
	}
	link := domain.ConnectURL(d.PublicURL)
	u := usable{list: make([]UsableServer, 0, len(servers)), by: map[string]domain.AgentConnection{}}
	for _, s := range servers {
		conn := domain.AgentConnectionOf(domain.AuthKind(s.AuthKind), status[s.Slug])
		e := UsableServer{Slug: s.Slug, Name: s.Name, Description: s.Description, Tools: s.ToolCount, Connection: conn}
		if conn.NeedsLink() {
			e.ConnectURL = link
		}
		u.list = append(u.list, e)
		u.by[s.Slug] = conn
	}
	return u, nil
}

type connectionsSummary struct {
	server string
	status domain.PersonalStatus
}

func listConnections(ctx context.Context, d MetaDeps, p Principal) ([]connectionsSummary, error) {
	list, err := d.Connections.ListConnections(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]connectionsSummary, 0, len(list))
	for _, c := range list {
		out = append(out, connectionsSummary{server: c.Server, status: domain.PersonalStatus(c.Status)})
	}
	return out, nil
}

// ListServers is list_servers: the servers the person may use, in the catalog's order, with the
// count of visible tools and the connection state (and the link when the person has to connect).
// Never nil.
func ListServers(ctx context.Context, d MetaDeps, p Principal) ([]UsableServer, error) {
	u, err := usableServers(ctx, d, p)
	if err != nil {
		return nil, err
	}
	return u.list, nil
}

// SearchToolsQuery is a search_tools input, already validated by the transport.
type SearchToolsQuery struct {
	Principal Principal
	Query     string
	Limit     int
	// Servers limits the search to these slugs; nil searches every server the person may use (the
	// transport refuses an empty list).
	Servers []string
}

// ToolHit is one search_tools result. Description is the tool's full description (the transport
// shortens it).
type ToolHit struct {
	Ref         string
	Server      string
	Title       *string
	Description string
	Score       float64
	Annotations catalog.ToolAnnotations
	Connection  domain.AgentConnection
}

// SearchToolsResult is the hits, best first, or the agent's text of a refusal (Refused non-empty,
// Hits empty).
type SearchToolsResult struct {
	Hits    []ToolHit
	Refused string
}

// SearchTools is search_tools: only servers the person may use are searched (a named one the person
// may not use is refused, the first in argument order); the named servers are de-duplicated in
// their order; the limit applies in the index, and then every hit is shown only while the catalog
// still offers the tool to this person (the index may lag), so fewer than the limit is normal.
func SearchTools(ctx context.Context, d MetaDeps, q SearchToolsQuery) (SearchToolsResult, error) {
	u, err := usableServers(ctx, d, q.Principal)
	if err != nil {
		return SearchToolsResult{}, err
	}
	servers := make([]string, 0, len(u.list))
	if q.Servers != nil {
		for _, slug := range q.Servers {
			if _, ok := u.by[slug]; !ok {
				return SearchToolsResult{Hits: []ToolHit{}, Refused: domain.TextUnknownServer(slug)}, nil
			}
		}
		seen := map[string]bool{}
		for _, slug := range q.Servers {
			if !seen[slug] {
				seen[slug] = true
				servers = append(servers, slug)
			}
		}
	} else {
		for _, s := range u.list {
			servers = append(servers, s.Slug)
		}
	}
	hits, err := d.Search.SearchTools(ctx, q.Query, q.Limit, servers)
	if err != nil {
		return SearchToolsResult{}, err
	}
	tools := make([]*catalog.ToolView, len(hits))
	errs := make([]error, len(hits))
	var wg sync.WaitGroup
	for i, hit := range hits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverInto(&errs[i])
			tools[i], errs[i] = toolFor(ctx, d, q.Principal, hit.Ref)
		}()
	}
	wg.Wait()
	out := make([]ToolHit, 0, len(hits))
	for i, hit := range hits {
		if errs[i] != nil {
			return SearchToolsResult{}, errs[i]
		}
		t := tools[i]
		if t == nil {
			continue
		}
		out = append(out, ToolHit{
			Ref: t.Ref, Server: t.Server, Title: t.Title, Description: t.Description,
			Score: hit.Score, Annotations: t.Annotations, Connection: u.connectionOf(t.Server),
		})
	}
	return SearchToolsResult{Hits: out}, nil
}

// toolFor is the tool the catalog offers the person now, nil when it does not (unknown, hidden,
// removed, a malformed ref, a server disabled or closed to the person).
func toolFor(ctx context.Context, d MetaDeps, p Principal, ref string) (*catalog.ToolView, error) {
	t, err := d.Catalog.GetToolFor(ctx, p, ref)
	if err != nil {
		if de, ok := kernel.AsDomainError(err); ok && (de.Code == catalog.CodeNotFound || de.Code == catalog.CodeInvalidToolRef) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// DescribedTool is describe_tool's answer: the full description and the schemas as stored.
type DescribedTool struct {
	Ref         string
	Server      string
	Title       *string
	Description string
	InputSchema json.RawMessage
	// OutputSchema is nil when the tool has none.
	OutputSchema json.RawMessage
	Annotations  catalog.ToolAnnotations
	Connection   domain.AgentConnection
}

// DescribeToolResult is the tool, or the agent's text when it is not offered (Tool nil).
type DescribeToolResult struct {
	Tool    *DescribedTool
	Refused string
}

// DescribeTool is describe_tool: a tool the person may not use now is the not_found text, decided
// before the person's servers are read.
func DescribeTool(ctx context.Context, d MetaDeps, p Principal, ref string) (DescribeToolResult, error) {
	t, err := toolFor(ctx, d, p, ref)
	if err != nil {
		return DescribeToolResult{}, err
	}
	if t == nil {
		return DescribeToolResult{Refused: domain.TextNotFound(ref)}, nil
	}
	u, err := usableServers(ctx, d, p)
	if err != nil {
		return DescribeToolResult{}, err
	}
	input := t.InputSchema
	if len(input) == 0 {
		input = json.RawMessage(`{"type":"object"}`)
	}
	return DescribeToolResult{Tool: &DescribedTool{
		Ref: t.Ref, Server: t.Server, Title: t.Title, Description: t.Description,
		InputSchema: input, OutputSchema: t.OutputSchema, Annotations: t.Annotations, Connection: u.connectionOf(t.Server),
	}}, nil
}

// errReadPanicked is the error of a read that panicked in one of the use cases' goroutines (where
// net/http's recovery does not reach: the process would end). The value is never kept.
type errReadPanicked struct{}

func (errReadPanicked) Error() string { return "gateway: a read panicked" }

// recoverInto, deferred in a goroutine, turns a panic into errReadPanicked in *err: the use case
// then fails with its ordinary internal error.
func recoverInto(err *error) {
	if recover() != nil {
		*err = errReadPanicked{}
	}
}
