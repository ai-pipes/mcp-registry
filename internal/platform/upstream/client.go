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

	// maxPages is the number of tools/list pages followed, as in the TypeScript client; a server
	// whose cursor never ends is a transport failure. maxTools is a limit the TypeScript client
	// does not have.
	maxPages = 100
	maxTools = 10000

	// maxRetries bounds how often the SDK resumes an interrupted SSE answer (the TypeScript
	// client's default).
	maxRetries = 2
)

// supportedVersions are the protocol revisions the server may answer initialize with (the
// TypeScript client's list).
var supportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"}

// Options configure a Client.
type Options struct {
	// Version is the gateway version sent as the client's version in initialize.
	Version string
}

// Client implements kernel.UpstreamClient. It holds no state between operations and is safe for
// concurrent use.
type Client struct {
	version string
}

var _ kernel.UpstreamClient = (*Client)(nil)

// New returns a Client.
func New(o Options) *Client {
	v := o.Version
	if v == "" {
		v = "unknown"
	}
	return &Client{version: v}
}

// ListTools returns every page of tools/list. The schemas and annotations are the raw JSON
// upstream sent (annotations reduced to the keys the TypeScript client keeps).
func (c *Client) ListTools(ctx context.Context, t kernel.UpstreamTarget) ([]kernel.UpstreamTool, error) {
	var tools []kernel.UpstreamTool
	err := c.run(ctx, t, func(s *session) error {
		cursor := ""
		for page := 0; ; page++ {
			if page >= maxPages {
				return errMalformed // the pagination does not end
			}
			params := map[string]string{}
			if cursor != "" {
				params["cursor"] = cursor
			}
			res, err := s.call("tools/list", params)
			if err != nil {
				return err
			}
			batch, next, err := decodePage(res)
			if err != nil {
				return err
			}
			if tools = append(tools, batch...); len(tools) > maxTools {
				return errMalformed
			}
			if next == "" {
				return nil
			}
			cursor = next
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
	deadline, _ := opCtx.Deadline()
	op := &operation{ctx: opCtx, cancel: cancel, deadline: deadline, headers: maps(t.Headers), limit: t.MaxResponseBytes}
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
	if err != nil {
		op.failed.Store(true)
	}
	_ = conn.Close() // the session-ending DELETE (success only) is a courtesy: its failure is nothing
	if err != nil {
		return c.classify(parent, err, op)
	}
	return nil
}

func maps(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
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
		"clientInfo":      map[string]string{"name": clientName, "version": c.version},
	})
	if err != nil {
		return err
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(res, &init) != nil || !slices.Contains(supportedVersions, init.ProtocolVersion) {
		return errMalformed
	}
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
// transport failure. A JSON-RPC error that came with a non-2xx status is a transport failure too.
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
	var rpc *rpcError
	if errors.As(err, &rpc) && !op.badStatus.Load() {
		return kernel.RejectedUpstreamError(rpc.code, rpc.message)
	}
	return kernel.NewUpstreamError(kernel.UpstreamTransport)
}

// session is the JSON-RPC conversation over the SDK's connection.
type session struct {
	conn mcp.Connection
	op   *operation
	next int64
}

// call sends a request and waits for its answer: the raw result, or an *rpcError. Requests and
// notifications upstream sends meanwhile are not ours to serve: a ping is answered, anything
// else is "method not found" (the client declares no capabilities).
func (s *session) call(method string, params any) (json.RawMessage, error) {
	s.next++
	id, err := jsonrpc.MakeID(float64(s.next))
	if err != nil {
		return nil, err
	}
	raw, err := marshal(params)
	if err != nil {
		return nil, err
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
				s.reply(m)
			}
		}
	}
}

func (s *session) reply(req *jsonrpc.Request) {
	resp := &jsonrpc.Response{ID: req.ID, Result: json.RawMessage("{}")}
	if req.Method != "ping" {
		resp = &jsonrpc.Response{ID: req.ID, Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found"}}
	}
	_ = s.conn.Write(s.op.ctx, resp) // a refused answer sets the operation's flags like any request
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
