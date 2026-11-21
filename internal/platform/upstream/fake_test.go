package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A request the fake received. RPC is the JSON-RPC method of a POST ("(response)" for a
// JSON-RPC response sent back by the client, "" for GET and DELETE).
type seen struct {
	HTTP   string
	RPC    string
	Header http.Header
	Body   []byte
}

// fake is a hand-written MCP server over Streamable HTTP with failure modes. It is raw on purpose:
// the SDK's server cannot be told to misbehave.
type fake struct {
	srv *httptest.Server

	mu      sync.Mutex
	reqs    []seen
	cfg     cfg
	written atomic.Int64 // bytes streamed for hugeBody
}

// cfg is what the fake does; changed through set.
type cfg struct {
	tools    []json.RawMessage // raw tool objects, served as given
	pageSize int               // tools per page; 0 = one page
	endless  bool              // every page carries a nextCursor
	sse      bool              // answer POSTs as text/event-stream
	session  string            // issue this Mcp-Session-Id on initialize
	protocol string            // protocolVersion answered by initialize ("" = the one asked)

	// call result, served as given (nil: an echo of the arguments)
	callResult json.RawMessage
	// A failure applied to every request whose RPC method is failOn ("*": all).
	failOn     string
	failStatus int    // HTTP status; 0: none
	failBody   string // body sent with failStatus
	redirectTo string // 307 to this URL
	rpcErr     *rpcErr
	rpcErrOn   string        // the RPC method it answers with the error ("" = all)
	rpcErrHTTP int           // status the JSON-RPC error is sent with (0: 200)
	delay      time.Duration // before answering tools/list and tools/call
	hangDelete bool          // never answer the session-ending DELETE
	drop       bool          // close the connection on tools/call
	serverReq  bool          // SSE only: a server-to-client request before the answer
	hugeBody   int64         // stream this many bytes as the answer to tools/call
	declared   int64         // Content-Length announced for hugeBody (0: chunked)
}

type rpcErr struct {
	Code    int
	Message string
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) url() string { return f.srv.URL + "/mcp" }

func (f *fake) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.reqs...)
}

// methods lists "HTTP RPC" per request, e.g. "POST initialize", "DELETE".
func (f *fake) methods() []string {
	var out []string
	for _, r := range f.requests() {
		if r.RPC == "" {
			out = append(out, r.HTTP)
		} else {
			out = append(out, r.HTTP+" "+r.RPC)
		}
	}
	return out
}

func (f *fake) count(rpc string) int {
	n := 0
	for _, r := range f.requests() {
		if r.RPC == rpc {
			n++
		}
	}
	return n
}

func (f *fake) set(fn func(*cfg)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.cfg)
}

type rpcIn struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Error  json.RawMessage `json:"error"`
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var in rpcIn
	_ = json.Unmarshal(body, &in)
	rpc := in.Method
	if r.Method == http.MethodPost && rpc == "" {
		rpc = "(response)"
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, seen{HTTP: r.Method, RPC: rpc, Header: r.Header.Clone(), Body: body})
	c := f.cfg // a copy; the lock is released below
	f.mu.Unlock()

	if c.redirectTo != "" && (c.failOn == "" || c.failOn == "*" || c.failOn == rpc) {
		w.Header().Set("Location", c.redirectTo)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	if c.failStatus != 0 && (c.failOn == "*" || c.failOn == rpc || (c.failOn == "" && r.Method == http.MethodPost)) {
		if c.failStatus == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		}
		w.WriteHeader(c.failStatus)
		_, _ = io.WriteString(w, c.failBody)
		return
	}
	switch r.Method {
	case http.MethodGet:
		http.Error(w, "no stream", http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		if c.hangDelete {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch in.Method {
	case "":
		w.WriteHeader(http.StatusAccepted) // a JSON-RPC response from the client
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(in.Params, &p)
		pv := c.protocol
		if pv == "" {
			pv = p.ProtocolVersion
		}
		if c.session != "" {
			w.Header().Set("Mcp-Session-Id", c.session)
		}
		f.answer(w, c, in.Method, in.ID, fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}`, pv))
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		f.hold(r, c.delay)
		f.answer(w, c, in.Method, in.ID, f.page(c, in.Params))
	case "tools/call":
		f.hold(r, c.delay)
		if c.drop {
			hj, _ := w.(http.Hijacker)
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		if c.hugeBody > 0 {
			f.stream(w, c, in.ID)
			return
		}
		res := c.callResult
		if res == nil {
			var p struct {
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(in.Params, &p)
			res = json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, string(p.Arguments)))
		}
		f.answer(w, c, in.Method, in.ID, string(res))
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

func (f *fake) hold(r *http.Request, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-r.Context().Done():
	}
}

// page serves the page asked for by params.cursor: the cursor is the index of the first tool.
func (f *fake) page(c cfg, params json.RawMessage) string {
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(params, &p)
	start := 0
	if p.Cursor != "" {
		start, _ = strconv.Atoi(p.Cursor)
	}
	end := len(c.tools)
	if c.pageSize > 0 {
		end = min(start+c.pageSize, end)
	}
	if start > end {
		start = end
	}
	parts := make([][]byte, 0, end-start)
	for _, t := range c.tools[start:end] {
		parts = append(parts, t)
	}
	out := `{"tools":[` + string(bytes.Join(parts, []byte(","))) + `]`
	switch {
	case c.endless:
		out += fmt.Sprintf(`,"nextCursor":"%d"`, end)
	case end < len(c.tools):
		out += fmt.Sprintf(`,"nextCursor":"%d"`, end)
	}
	return out + `}`
}

// answer sends the JSON-RPC answer to the request: a result, or the configured error.
func (f *fake) answer(w http.ResponseWriter, c cfg, rpc string, id json.RawMessage, result string) {
	var msg string
	status := http.StatusOK
	if c.rpcErr != nil && (c.rpcErrOn == "" || c.rpcErrOn == rpc) {
		msg = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%q}}`, id, c.rpcErr.Code, c.rpcErr.Message)
		if c.rpcErrHTTP != 0 {
			status = c.rpcErrHTTP
		}
	} else {
		msg = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
	}
	if c.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		if c.serverReq {
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"srv-1\",\"method\":\"sampling/createMessage\",\"params\":{}}\n\n")
		}
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(msg)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg)
}

// stream sends hugeBody bytes of one JSON answer in chunks until the client stops reading.
func (f *fake) stream(w http.ResponseWriter, c cfg, id json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	if c.declared > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(c.declared, 10))
	}
	head := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"`, id)
	_, _ = io.WriteString(w, head)
	chunk := bytes.Repeat([]byte("x"), 32<<10)
	fl, _ := w.(http.Flusher)
	for f.written.Load() < c.hugeBody {
		n, err := w.Write(chunk)
		f.written.Add(int64(n))
		if err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
}
