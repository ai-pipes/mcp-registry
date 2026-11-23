package http

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

// toolsListJSON is the tools/list result: the four meta-tools, their schemas as the reference's
// zod 4 renders them (compat/golden/mcp-surface.json), in the reference's key order (its own
// tools/list answer, testdata/reference/answers.json). It never depends on the catalog.
const toolsListJSON = `{"tools":[
{"name":"list_servers",
 "description":"List the user's services available through this gateway: slug, name, description, number of tools and connection state (connectUrl when the user has to connect first).",
 "inputSchema":{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{}},
 "annotations":{"readOnlyHint":true},
 "outputSchema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"servers":{"type":"array","items":{"type":"object","properties":{"slug":{"type":"string"},"name":{"type":"string"},"description":{"type":"string"},"tools":{"type":"number"},"connection":{"type":"string","enum":["ready","needs_connection","expired","not_required"]},"connectUrl":{"type":"string"}},"required":["slug","name","description","tools","connection"],"additionalProperties":false}}},"required":["servers"],"additionalProperties":false}},
{"name":"search_tools",
 "description":"Find tools by a short description of the action. Returns only tools above a relevance threshold, best first, with score 0–1; an empty list means nothing fits. servers (slugs from list_servers) limits the search.",
 "inputSchema":{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"query":{"type":"string","minLength":1,"maxLength":500},"limit":{"default":10,"type":"integer","minimum":1,"maximum":25},"servers":{"minItems":1,"maxItems":20,"type":"array","items":{"type":"string","minLength":1,"maxLength":64}}},"required":["query"]},
 "annotations":{"readOnlyHint":true},
 "outputSchema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"results":{"type":"array","items":{"type":"object","properties":{"tool":{"type":"string"},"title":{"type":["string","null"]},"description":{"type":"string"},"server":{"type":"string"},"score":{"type":"number"},"annotations":{"type":"object","propertyNames":{"type":"string"},"additionalProperties":{"type":"boolean"}},"connection":{"type":"string","enum":["ready","needs_connection","expired","not_required"]}},"required":["tool","title","description","server","score","annotations","connection"],"additionalProperties":false}}},"required":["results"],"additionalProperties":false}},
{"name":"describe_tool",
 "description":"Full description and input JSON Schema of a tool from search_tools (\"<server>.<tool>\"). Call before call_tool.",
 "inputSchema":{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"tool":{"type":"string","minLength":1,"maxLength":300}},"required":["tool"]},
 "annotations":{"readOnlyHint":true},
 "outputSchema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"tool":{"type":"string"},"title":{"type":["string","null"]},"description":{"type":"string"},"inputSchema":{"type":"object","propertyNames":{"type":"string"},"additionalProperties":{}},"outputSchema":{"type":"object","propertyNames":{"type":"string"},"additionalProperties":{}},"annotations":{"type":"object","propertyNames":{"type":"string"},"additionalProperties":{"type":"boolean"}},"server":{"type":"string"},"connection":{"type":"string","enum":["ready","needs_connection","expired","not_required"]}},"required":["tool","title","description","inputSchema","annotations","server","connection"],"additionalProperties":false}},
{"name":"call_tool",
 "description":"Call a tool (\"<server>.<tool>\") with arguments matching its inputSchema, on the user's behalf. Returns the service's result as is.",
 "inputSchema":{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"tool":{"type":"string","minLength":1,"maxLength":300},"arguments":{"default":{},"type":"object","propertyNames":{"type":"string"},"additionalProperties":{}}},"required":["tool"]},
 "annotations":{"readOnlyHint":false,"openWorldHint":true}}
]}`

// toolsListResult is toolsListJSON compacted once.
var toolsListResult = func() []byte {
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(toolsListJSON)); err != nil {
		panic("gateway http: the tools/list document is not JSON: " + err.Error())
	}
	return b.Bytes()
}()

const msgInternal = "Internal error."

// toolsCall serves tools/call: an unknown tool name is a JSON-RPC error, everything else a result.
func (x *exchange) toolsCall(m message) outcome {
	// The name and the arguments are read from the one parsed message (exact keys, the last
	// duplicate wins), the same values the Mcp-Name check saw.
	if issues := callParamsIssues(m.params, m.params != nil); len(issues) > 0 {
		return outcome{err: &rpcErr{Code: codeInvalidParams, Message: "Invalid tools/call request: " + issues.text()}}
	}
	name, _ := m.nameOf()
	var args map[string]json.RawMessage
	if raw, present := m.params["arguments"]; present {
		_ = json.Unmarshal(raw, &args) // an object (checked above)
	}
	ctx := x.r.Context()
	var run func() ([]byte, error)
	switch name {
	case "list_servers":
		run = func() ([]byte, error) { return x.h.listServers(ctx, x.p) }
	case "search_tools":
		in, is := parseSearch(args)
		if len(is) > 0 {
			return outcome{result: failure(is.text(name))}
		}
		run = func() ([]byte, error) { return x.h.searchTools(ctx, x.p, in) }
	case "describe_tool":
		ref, is := parseTool(args)
		if len(is) > 0 {
			return outcome{result: failure(is.text(name))}
		}
		run = func() ([]byte, error) { return x.h.describeTool(ctx, x.p, ref) }
	case "call_tool":
		ref, arguments, is := parseCall(args)
		if len(is) > 0 {
			return outcome{result: failure(is.text(name))}
		}
		run = func() ([]byte, error) { return x.h.callTool(ctx, x.p, ref, arguments) }
	default:
		return outcome{err: &rpcErr{Code: codeInvalidParams, Message: "Tool " + name + " not found"}}
	}
	res, err := run()
	if err != nil {
		// Only the tool's name and the error's label: never the arguments or the error's text.
		x.h.log.Error("mcp tool failed", "tool", name, logging.ErrAttr(err))
		return outcome{result: failure(msgInternal)}
	}
	return outcome{result: res}
}

// failure is an isError result with one text block.
func failure(text string) []byte { return rawLineSeparators(domain.FailureResult(text).JSON()) }

// structured is the result of a meta-tool: one text block holding the compact JSON of value and
// structuredContent, the same value.
func structured(value any) ([]byte, error) {
	js, err := marshalJS(value)
	if err != nil {
		return nil, err
	}
	text, err := marshalJS(string(js))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(`{"content":[{"type":"text","text":`)
	b.Write(text)
	b.WriteString(`}],"structuredContent":`)
	b.Write(js)
	b.WriteByte('}')
	return b.Bytes(), nil
}

// serverEntry is one server of list_servers.
type serverEntry struct {
	Slug        string                 `json:"slug"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Tools       int                    `json:"tools"`
	Connection  domain.AgentConnection `json:"connection"`
	ConnectURL  string                 `json:"connectUrl,omitempty"`
}

// usable is the servers the person may use, in the catalog's order, with their connection state.
type usable struct {
	list []serverEntry
	by   map[string]domain.AgentConnection
}

// connectionOf is the state of a hit's server (needs_connection when it is not in the list).
func (u usable) connectionOf(slug string) domain.AgentConnection {
	if c, ok := u.by[slug]; ok {
		return c
	}
	return domain.ConnectionNeedsConnection
}

// usableServers reads the person's servers and connections (in parallel, as the reference does).
func (h *endpoint) usableServers(ctx context.Context, p identity.Principal) (usable, error) {
	var (
		wg       sync.WaitGroup
		servers  []catalog.MemberServerView
		conns    []connectionStatus
		srvErr   error
		connsErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); servers, srvErr = h.d.Catalog.ListServersFor(ctx, p) }()
	go func() { defer wg.Done(); conns, connsErr = h.connections(ctx, p) }()
	wg.Wait()
	if srvErr != nil {
		return usable{}, srvErr
	}
	if connsErr != nil {
		return usable{}, connsErr
	}
	status := map[string]domain.PersonalStatus{}
	for _, c := range conns {
		status[c.server] = c.status
	}
	u := usable{list: make([]serverEntry, 0, len(servers)), by: map[string]domain.AgentConnection{}}
	for _, s := range servers {
		conn := domain.AgentConnectionOf(domain.AuthKind(s.AuthKind), status[s.Slug])
		e := serverEntry{Slug: s.Slug, Name: s.Name, Description: s.Description, Tools: s.ToolCount, Connection: conn}
		if conn.NeedsLink() {
			e.ConnectURL = h.connectURL
		}
		u.list = append(u.list, e)
		u.by[s.Slug] = conn
	}
	return u, nil
}

type connectionStatus struct {
	server string
	status domain.PersonalStatus
}

func (h *endpoint) connections(ctx context.Context, p identity.Principal) ([]connectionStatus, error) {
	list, err := h.d.Connections.ListConnections(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]connectionStatus, 0, len(list))
	for _, c := range list {
		out = append(out, connectionStatus{server: c.Server, status: domain.PersonalStatus(c.Status)})
	}
	return out, nil
}

func (h *endpoint) listServers(ctx context.Context, p identity.Principal) ([]byte, error) {
	u, err := h.usableServers(ctx, p)
	if err != nil {
		return nil, err
	}
	return structured(struct {
		Servers []serverEntry `json:"servers"`
	}{u.list})
}

// searchHit is one result of search_tools.
type searchHit struct {
	Tool        string                  `json:"tool"`
	Title       *string                 `json:"title"`
	Description string                  `json:"description"`
	Server      string                  `json:"server"`
	Score       float64                 `json:"score"`
	Annotations catalog.ToolAnnotations `json:"annotations"`
	Connection  domain.AgentConnection  `json:"connection"`
}

func (h *endpoint) searchTools(ctx context.Context, p identity.Principal, in searchInput) ([]byte, error) {
	u, err := h.usableServers(ctx, p)
	if err != nil {
		return nil, err
	}
	servers := make([]string, 0, len(u.list))
	if in.hasServers {
		for _, slug := range in.servers {
			if _, ok := u.by[slug]; !ok {
				return failure(`Unknown server "` + slug + `". Call list_servers.`), nil
			}
		}
		seen := map[string]bool{}
		for _, slug := range in.servers {
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
	hits, err := h.d.Search.SearchTools(ctx, in.query, in.limit, servers)
	if err != nil {
		return nil, err
	}
	// The index may lag the catalog: every hit is shown only while the catalog still offers the
	// tool to this person.
	tools := make([]*catalog.ToolView, len(hits))
	errs := make([]error, len(hits))
	var wg sync.WaitGroup
	for i, hit := range hits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tools[i], errs[i] = h.toolFor(ctx, p, hit.Ref)
		}()
	}
	wg.Wait()
	results := make([]searchHit, 0, len(hits))
	for i, hit := range hits {
		if errs[i] != nil {
			return nil, errs[i]
		}
		t := tools[i]
		if t == nil {
			continue
		}
		results = append(results, searchHit{
			Tool: t.Ref, Title: t.Title, Description: ShortDescription(t.Description), Server: t.Server,
			Score: hit.Score, Annotations: t.Annotations, Connection: u.connectionOf(t.Server),
		})
	}
	return structured(struct {
		Results []searchHit `json:"results"`
	}{results})
}

// toolFor is the tool the catalog offers the person now, nil when it does not (unknown, hidden,
// removed, a malformed ref, a server disabled or closed to the person).
func (h *endpoint) toolFor(ctx context.Context, p identity.Principal, ref string) (*catalog.ToolView, error) {
	t, err := h.d.Catalog.GetToolFor(ctx, p, ref)
	if err != nil {
		if de, ok := kernel.AsDomainError(err); ok && (de.Code == catalog.CodeNotFound || de.Code == catalog.CodeInvalidToolRef) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// described is the result of describe_tool.
type described struct {
	Tool         string                  `json:"tool"`
	Title        *string                 `json:"title"`
	Description  string                  `json:"description"`
	InputSchema  json.RawMessage         `json:"inputSchema"`
	OutputSchema json.RawMessage         `json:"outputSchema,omitempty"`
	Annotations  catalog.ToolAnnotations `json:"annotations"`
	Server       string                  `json:"server"`
	Connection   domain.AgentConnection  `json:"connection"`
}

func (h *endpoint) describeTool(ctx context.Context, p identity.Principal, ref string) ([]byte, error) {
	t, err := h.toolFor(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return failure(domain.TextNotFound(ref)), nil
	}
	u, err := h.usableServers(ctx, p)
	if err != nil {
		return nil, err
	}
	input := t.InputSchema
	if len(input) == 0 {
		input = json.RawMessage(`{"type":"object"}`)
	}
	return structured(described{
		Tool: t.Ref, Title: t.Title, Description: t.Description, InputSchema: input, OutputSchema: t.OutputSchema,
		Annotations: t.Annotations, Server: t.Server, Connection: u.connectionOf(t.Server),
	})
}

// callTool hands the agent's arguments to the gateway as they were sent and its answer back as
// bytes: a ToolResult never goes through json.Marshal (which would HTML-escape upstream bytes).
func (h *endpoint) callTool(ctx context.Context, p identity.Principal, ref string, arguments json.RawMessage) ([]byte, error) {
	res, err := h.d.Gateway.CallTool(ctx, gateway.CallToolCommand{Principal: p, Ref: ref, Arguments: arguments})
	if err != nil {
		return nil, err
	}
	return res.JSON(), nil
}
