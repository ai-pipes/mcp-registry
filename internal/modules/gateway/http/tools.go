package http

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
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

// The wire shapes of the meta-tools' answers (the reference's key order). The use cases are in
// gateway/app; this file only renders their results.

// serverEntry is one server of list_servers.
type serverEntry struct {
	Slug        string                 `json:"slug"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Tools       int                    `json:"tools"`
	Connection  domain.AgentConnection `json:"connection"`
	ConnectURL  string                 `json:"connectUrl,omitempty"`
}

func (h *endpoint) listServers(ctx context.Context, p identity.Principal) ([]byte, error) {
	servers, err := app.ListServers(ctx, h.meta, p)
	if err != nil {
		return nil, err
	}
	out := make([]serverEntry, 0, len(servers))
	for _, s := range servers {
		out = append(out, serverEntry{s.Slug, s.Name, s.Description, s.Tools, s.Connection, s.ConnectURL})
	}
	return structured(struct {
		Servers []serverEntry `json:"servers"`
	}{out})
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
	q := app.SearchToolsQuery{Principal: p, Query: in.query, Limit: in.limit}
	if in.hasServers {
		q.Servers = append([]string{}, in.servers...) // non-nil: the agent named servers
	}
	res, err := app.SearchTools(ctx, h.meta, q)
	if err != nil {
		return nil, err
	}
	if res.Refused != "" {
		return failure(res.Refused), nil
	}
	results := make([]searchHit, 0, len(res.Hits))
	for _, hit := range res.Hits {
		results = append(results, searchHit{
			Tool: hit.Ref, Title: hit.Title, Description: ShortDescription(hit.Description), Server: hit.Server,
			Score: hit.Score, Annotations: hit.Annotations, Connection: hit.Connection,
		})
	}
	return structured(struct {
		Results []searchHit `json:"results"`
	}{results})
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
	res, err := app.DescribeTool(ctx, h.meta, p, ref)
	if err != nil {
		return nil, err
	}
	if res.Tool == nil {
		return failure(res.Refused), nil
	}
	t := res.Tool
	return structured(described{
		Tool: t.Ref, Title: t.Title, Description: t.Description, InputSchema: t.InputSchema, OutputSchema: t.OutputSchema,
		Annotations: t.Annotations, Server: t.Server, Connection: t.Connection,
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
