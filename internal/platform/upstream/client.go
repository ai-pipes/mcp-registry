// Package upstream is the client for upstream MCP servers (Streamable HTTP): the implementation of
// kernel.UpstreamClient on the official Go SDK's transport.
//
// It reaches servers the gateway does not control, with people's credentials, so every operation
// is bounded and leaves nothing behind: one MCP session per operation, one deadline, one byte
// budget for all its responses, no redirects, no standalone event stream, no connection or header
// shared with any other operation, and error texts that carry no body, header or URL.
//
// The SDK is used below its typed layer. mcp.Client decodes tools/list and tools/call into Go
// structs that cannot tell an absent annotation from a false one, drop tools it dislikes and fail
// on content it does not know, and it probes a newer protocol revision before initialize. The
// package therefore takes the SDK's transport (HTTP, SSE framing, session ids), speaks the three
// JSON-RPC exchanges it needs over the transport's Connection and reads the results as raw JSON.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	clientName = "mcp-registry"
	// protocolVersion is what the TypeScript client asks for in initialize: the exchange starts
	// there, not with the SDK's default discovery request of a newer revision.
	protocolVersion = "2025-11-25"

	// maxPages is the number of tools/list pages followed: DEFAULT_LIST_MAX_PAGES of the TypeScript
	// SDK's own page walk (@modelcontextprotocol/client 2.4.0, dist/public-CvV4ZHkX.mjs:2647), which
	// is what the TypeScript server really runs (its own MAX_PAGES = 100 never applies: the SDK
	// walks the pages when no cursor is given). A server whose cursor never ends is a transport
	// failure. maxTools is a limit the TypeScript client does not have.
	maxPages = 64
	maxTools = 10000

	// clientVersion is the fixed clientInfo.version the TypeScript client sends (CLIENT_INFO in
	// v1.0.1:apps/server/src/platform/upstream/mcp-upstream-client.ts), unrelated to the gateway's version.
	clientVersion = "1.0.0"

	// maxRetries bounds how often the SDK resumes an interrupted SSE answer (the TypeScript
	// client's default).
	maxRetries = 2

	// maxServerRequests bounds the requests upstream may send us during one operation; each is
	// answered with one more credentialed POST.
	maxServerRequests = 32
)

// supportedVersions are the protocol revisions the server may answer initialize with (the
// TypeScript client's list).
var supportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"}

// Options configure a Client.
type Options struct {
	// Version is accepted for the composition's sake and not sent: clientInfo.version is the fixed
	// string the TypeScript client sends.
	Version string
}

// Client implements kernel.UpstreamClient. It holds no state between operations and is safe for
// concurrent use.
type Client struct{}

var _ kernel.UpstreamClient = (*Client)(nil)

// New returns a Client.
func New(Options) *Client { return &Client{} }

// ListTools returns every page of tools/list, walked as the TypeScript SDK walks them: the first
// page without parameters, then each nextCursor ("" included) until there is none, a page that
// repeats the cursor just used with the items of the page before it (the walk ends with what it
// has), or 64 pages (transport). A server that did not advertise the tools capability has no
// tools: nothing is asked. The schemas and annotations are the raw JSON upstream sent
// (annotations reduced to the keys the TypeScript client keeps).
func (c *Client) ListTools(ctx context.Context, t kernel.UpstreamTarget) ([]kernel.UpstreamTool, error) {
	var tools []kernel.UpstreamTool
	err := c.run(ctx, t, func(s *session) error {
		if !s.toolsCapability {
			tools = []kernel.UpstreamTool{}
			return nil
		}
		var params any
		var previous []kernel.UpstreamTool
		for pages := 0; ; {
			res, err := s.call("tools/list", params)
			if err != nil {
				return err
			}
			batch, next, err := decodePage(res)
			if err != nil {
				return err
			}
			used := ""
			if p, ok := params.(map[string]string); ok {
				used = p["cursor"]
			}
			if pages > 0 && next != nil && *next == used && reflect.DeepEqual(batch, previous) {
				return nil
			}
			if tools = append(tools, batch...); len(tools) > maxTools {
				return errMalformed
			}
			previous = batch
			pages++
			if next == nil {
				return nil
			}
			if pages >= maxPages {
				return errMalformed // the pagination does not end
			}
			params = map[string]string{"cursor": *next}
		}
	})
	if err != nil {
		return nil, err
	}
	return tools, nil
}

// CallTool sends tools/call and returns the result object exactly as upstream sent it.
func (c *Client) CallTool(ctx context.Context, t kernel.UpstreamTarget, name string, arguments json.RawMessage) (kernel.UpstreamCallResult, error) {
	args := bytes.TrimSpace(arguments)
	if len(args) == 0 || string(args) == "null" {
		args = []byte("{}")
	}
	if !json.Valid(args) {
		return nil, errors.New("upstream: arguments are not valid JSON") // the caller's bug, nothing was sent
	}
	var result json.RawMessage
	err := c.run(ctx, t, func(s *session) error {
		res, err := s.call("tools/call", struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}{name, args})
		if err != nil {
			return err
		}
		if trimmed := bytes.TrimSpace(res); len(trimmed) == 0 || trimmed[0] != '{' {
			return errMalformed
		}
		result = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// run is one operation: connect, initialize, fn, close, all inside one deadline. It returns the
// failure classified (see classify).
func (c *Client) run(parent context.Context, t kernel.UpstreamTarget, fn func(*session) error) error {
	if err := parent.Err(); err != nil {
		return cancelled(err)
	}
	if u, err := url.Parse(t.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return kernel.NewUpstreamError(kernel.UpstreamTransport)
	}
	opCtx, cancel := context.WithTimeout(parent, t.Timeout)
	defer cancel()
	op := &operation{ctx: opCtx, cancel: cancel, headers: maps(t.Headers), limit: t.MaxResponseBytes}
	client, closeIdle := newHTTPClient(op)
	defer closeIdle()

	transport := &mcp.StreamableClientTransport{
		Endpoint:             t.URL,
		HTTPClient:           client,
		MaxRetries:           maxRetries,
		DisableStandaloneSSE: true,
		MaxEventSize:         -1, // the byte budget bounds it
	}
	conn, err := transport.Connect(opCtx)
	if err != nil {
		return c.classify(parent, err, op)
	}
	s := &session{conn: conn, op: op}
	err = c.handshake(s)
	if err == nil {
		err = fn(s)
	}
	_ = conn.Close() // the SDK's session-ending DELETE is refused by the transport: nothing is sent
	if err != nil {
		return c.classify(parent, err, op)
	}
	return nil
}

func maps(h kernel.UpstreamHeaders) kernel.UpstreamHeaders {
	out := make(kernel.UpstreamHeaders, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// handshake is initialize and the initialized notification, the exchange of the TypeScript
// client's legacy mode.
func (c *Client) handshake(s *session) error {
	res, err := s.call("initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": clientName, "version": clientVersion},
	})
	if err != nil {
		return err
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(res, &init) != nil || !slices.Contains(supportedVersions, init.ProtocolVersion) {
		return errMalformed
	}
	// as the TypeScript client: a missing or null tools capability means "no tools"
	s.toolsCapability = len(init.Capabilities.Tools) > 0 && string(init.Capabilities.Tools) != "null"
	pv := init.ProtocolVersion
	s.op.protocol.Store(&pv)
	return s.conn.Write(s.op.ctx, &jsonrpc.Request{Method: "notifications/initialized"})
}

// rpcError is a JSON-RPC error upstream answered a request with.
type rpcError struct {
	code    int
	message string
}

func (e *rpcError) Error() string { return "json-rpc error" }

func cancelled(err error) error { return fmt.Errorf("upstream operation cancelled: %w", err) }

// classify maps a failure to the port's error. Order, as in the TypeScript client: the byte limit,
// a 401 seen on any request, the deadline, then a JSON-RPC error answer, and everything else is a
// transport failure (a JSON-RPC error that came with a non-2xx status included).
// The caller's own cancellation or deadline is not an upstream failure: it is returned as the
// context's error, wrapped, so the caller can tell it from an upstream timeout.
func (c *Client) classify(parent context.Context, err error, op *operation) error {
	switch {
	case op.tooLarge.Load():
		return kernel.NewUpstreamError(kernel.UpstreamTooLarge)
	case op.unauthorized.Load():
		return kernel.NewUpstreamError(kernel.UpstreamUnauthorized)
	case parent.Err() != nil:
		return cancelled(parent.Err())
	case errors.Is(op.ctx.Err(), context.DeadlineExceeded):
		return kernel.NewUpstreamError(kernel.UpstreamTimeout)
	}
	// A JSON-RPC error that came with a non-2xx status never reaches here as an rpcError: the SDK
	// reports it from Write as its own error, which is a transport failure below.
	var rpc *rpcError
	if errors.As(err, &rpc) {
		return kernel.RejectedUpstreamError(rpc.code, rpc.message)
	}
	return kernel.NewUpstreamError(kernel.UpstreamTransport)
}

// session is the JSON-RPC conversation over the SDK's connection.
type session struct {
	conn            mcp.Connection
	op              *operation
	next            int64
	toolsCapability bool
	serverRequests  int
}

// call sends a request and waits for its answer: the raw result, or an *rpcError. Requests and
// notifications upstream sends meanwhile are not ours to serve: a ping is answered, anything
// else is "Method not found" (the client declares no capabilities), at most maxServerRequests times.
func (s *session) call(method string, params any) (json.RawMessage, error) {
	s.next++
	id, err := jsonrpc.MakeID(float64(s.next))
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if params != nil {
		if raw, err = marshal(params); err != nil {
			return nil, err
		}
	}
	if err := s.conn.Write(s.op.ctx, &jsonrpc.Request{ID: id, Method: method, Params: raw}); err != nil {
		return nil, err
	}
	for {
		msg, err := s.conn.Read(s.op.ctx)
		if err != nil {
			return nil, err
		}
		switch m := msg.(type) {
		case *jsonrpc.Response:
			if m.ID != id {
				continue
			}
			if m.Error != nil {
				var we *jsonrpc.Error
				if !errors.As(m.Error, &we) {
					return nil, errMalformed
				}
				return nil, &rpcError{code: int(we.Code), message: we.Message}
			}
			if len(m.Result) == 0 {
				return nil, errMalformed
			}
			return m.Result, nil
		case *jsonrpc.Request:
			if m.IsCall() {
				if s.serverRequests++; s.serverRequests > maxServerRequests {
					return nil, errMalformed // upstream asks more than a client without capabilities answers
				}
				go s.reply(m) // off the read loop: a slow or hung reply must not hold the operation
			}
		}
	}
}

func (s *session) reply(req *jsonrpc.Request) {
	resp := &jsonrpc.Response{ID: req.ID, Result: json.RawMessage("{}")}
	if req.Method != "ping" {
		// what the TypeScript SDK answers a request it has no handler for
		resp = &jsonrpc.Response{ID: req.ID, Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "Method not found"}}
	}
	// Bounded by the operation's context; best effort (the transport turns the answer into a 202).
	_ = s.conn.Write(context.WithValue(s.op.ctx, replyKey{}, true), resp)
}

// marshal is json.Marshal without HTML escaping, so a raw value goes out as it came in.
func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
