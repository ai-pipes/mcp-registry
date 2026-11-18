//go:build compat

package fakes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxRequestBody = 64 << 20

// UpstreamOptions configure an upstream.
type UpstreamOptions struct {
	Tools         []Tool    // from Tools(...) or FixtureTools("gitea"|"youtrack")
	RequireHeader [2]string // name and exact value; any request without it gets 401
	PageSize      int       // tools per tools/list page; 0 = one page
}

// Request is an HTTP request the upstream received.
type Request struct {
	Method string      // the JSON-RPC method (initialize, tools/list, tools/call...); empty for GET and DELETE
	Header http.Header // the HTTP request headers
}

// Call is a tools/call request the upstream received, whatever it answered.
type Call struct {
	Tool      string
	Arguments json.RawMessage // as sent; empty if the call had none
	Header    http.Header     // the HTTP request headers, including the credential
}

// Upstream is an MCP server on Streamable HTTP (the official Go SDK) wrapped in an http.Handler
// with failure modes. tools/list serves the tools sorted by name (the SDK's order); tools/call
// answers with the arguments echoed as text and as structuredContent {"echo": ...}.
type Upstream struct {
	URL string // http://host.docker.internal:<port>/mcp

	mu          sync.Mutex
	tools       []Tool
	pageSize    int
	require     [2]string
	failStatus  int
	redirectTo  string
	delay       time.Duration
	respondSize int
	rejectCode  int
	rejectMsg   string
	rejecting   bool
	calls       []Call
	requests    []Request
	listReqs    int
	sdk         http.Handler
}

// StartUpstream starts an upstream that stops with the test.
func StartUpstream(t *testing.T, o UpstreamOptions) *Upstream {
	t.Helper()
	u := &Upstream{tools: append([]Tool(nil), o.Tools...), pageSize: o.PageSize, require: o.RequireHeader}
	u.sdk = mcp.NewStreamableHTTPHandler(u.newServer, &mcp.StreamableHTTPOptions{
		Stateless:                  true, // every request sees the current tools
		DisableLocalhostProtection: true, // the Host is host.docker.internal
	})
	port := serve(t, u)
	u.URL = baseURL(port) + "/mcp"
	return u
}

// SetTools replaces the tools served from now on.
func (u *Upstream) SetTools(tools []Tool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.tools = append([]Tool(nil), tools...)
}

// FailWith answers every request with status 401 (with WWW-Authenticate) or 500.
func (u *Upstream) FailWith(status int) { u.set(func() { u.failStatus = status }) }

// RedirectTo answers every request 307 to url.
func (u *Upstream) RedirectTo(url string) { u.set(func() { u.redirectTo = url }) }

// Delay holds tools/list and tools/call answers for d; a caller that gives up ends the wait.
func (u *Upstream) Delay(d time.Duration) { u.set(func() { u.delay = d }) }

// RespondBytes answers tools/call with a plain JSON-RPC result whose whole HTTP body is exactly n
// bytes long (at least the size of an empty result).
func (u *Upstream) RespondBytes(n int) { u.set(func() { u.respondSize = n }) }

// RejectCalls answers tools/call with a JSON-RPC error.
func (u *Upstream) RejectCalls(code int, msg string) {
	u.set(func() { u.rejecting, u.rejectCode, u.rejectMsg = true, code, msg })
}

// Calls returns the tools/call requests received since the start or the last Reset.
func (u *Upstream) Calls() []Call {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]Call, len(u.calls))
	copy(out, u.calls)
	return out
}

// ListRequests returns how many tools/list requests (pages) were received since the start or the
// last Reset, including refused ones.
func (u *Upstream) ListRequests() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.listReqs
}

// Requests returns every request received since the start or the last Reset, including refused
// ones, with its JSON-RPC method and headers.
func (u *Upstream) Requests() []Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]Request, len(u.requests))
	copy(out, u.requests)
	return out
}

// Reset clears every failure mode and the recorded requests; the tools stay.
func (u *Upstream) Reset() {
	u.set(func() {
		u.failStatus, u.redirectTo, u.delay, u.respondSize = 0, "", 0, 0
		u.rejecting, u.rejectCode, u.rejectMsg = false, 0, ""
		u.calls, u.requests, u.listReqs = nil, nil, 0
	})
}

func (u *Upstream) set(f func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f()
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

func (u *Upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var rpc rpcRequest
	_ = json.Unmarshal(raw, &rpc) // a GET or DELETE has no JSON body: Method stays empty

	u.mu.Lock()
	u.requests = append(u.requests, Request{Method: rpc.Method, Header: r.Header.Clone()})
	switch rpc.Method {
	case "tools/list":
		u.listReqs++
	case "tools/call":
		u.calls = append(u.calls, Call{Tool: rpc.Params.Name, Arguments: rpc.Params.Arguments, Header: r.Header.Clone()})
	}
	require, failStatus, redirectTo, delay, size := u.require, u.failStatus, u.redirectTo, u.delay, u.respondSize
	rejecting, rejectCode, rejectMsg := u.rejecting, u.rejectCode, u.rejectMsg
	u.mu.Unlock()

	if require[0] != "" && r.Header.Get(require[0]) != require[1] {
		unauthorized(w)
		return
	}
	if redirectTo != "" {
		w.Header().Set("Location", redirectTo)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	switch failStatus {
	case 0:
	case http.StatusUnauthorized:
		unauthorized(w)
		return
	default:
		http.Error(w, "nope", failStatus)
		return
	}
	if delay > 0 && (rpc.Method == "tools/list" || rpc.Method == "tools/call") {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
	if rpc.Method == "tools/call" {
		switch {
		case rejecting:
			writeRejection(w, rpc.ID, rejectCode, rejectMsg)
			return
		case size > 0:
			writeSized(w, rpc.ID, size)
			return
		}
	}
	u.sdk.ServeHTTP(w, r)
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// writeRejection answers a request with HTTP 200 and a JSON-RPC error; the SDK would map a handler
// error to an HTTP error status instead.
func writeRejection(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": msg}})
	out := fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,%s`, id, body[1:])
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	_, _ = w.Write(out)
}

// writeSized writes a JSON-RPC text result so that the entire body is exactly size bytes.
func writeSized(w http.ResponseWriter, id json.RawMessage, size int) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	build := func(text string) []byte {
		return fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":%q}]}}`, id, text)
	}
	body := build("")
	if pad := size - len(body); pad > 0 {
		body = build(string(bytes.Repeat([]byte("x"), pad)))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// newServer builds the SDK server for one request from the current tools.
func (u *Upstream) newServer(*http.Request) *mcp.Server {
	u.mu.Lock()
	tools := append([]Tool(nil), u.tools...)
	pageSize := u.pageSize
	u.mu.Unlock()
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-upstream", Version: "1.0.0"}, &mcp.ServerOptions{PageSize: pageSize})
	for _, t := range tools {
		s.AddTool(toSDK(t), u.handleCall)
	}
	return s
}

func (u *Upstream) handleCall(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.Params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var echo any
	if err := json.Unmarshal(args, &echo); err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "arguments are not JSON"}
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(args)}},
		StructuredContent: map[string]any{"echo": echo},
	}, nil
}

func toSDK(t Tool) *mcp.Tool {
	in := map[string]any{"type": "object"}
	for k, v := range t.InputSchema {
		in[k] = v
	}
	out := &mcp.Tool{Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: in}
	if t.OutputSchema != nil {
		o := map[string]any{"type": "object"}
		for k, v := range t.OutputSchema {
			o[k] = v
		}
		out.OutputSchema = o
	}
	if t.Annotations != nil {
		// Annotations are a typed struct in the SDK; known keys carry over through JSON.
		var a mcp.ToolAnnotations
		if raw, err := json.Marshal(t.Annotations); err == nil && json.Unmarshal(raw, &a) == nil {
			out.Annotations = &a
		}
	}
	return out
}
