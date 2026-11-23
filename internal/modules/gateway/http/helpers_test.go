package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway"
	gwhttp "github.com/ai-pipes/mcp-registry/internal/modules/gateway/http"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/modules/search"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// ---- fakes of the module ports ----

type fakeCatalog struct {
	mu      sync.Mutex
	servers []catalog.MemberServerView
	tools   map[string]catalog.ToolView
	// closed lists, per user id, the slugs that person may not use.
	closed  map[string]map[string]bool
	listErr error
	toolErr error
	reads   int
}

func (c *fakeCatalog) visible(p identity.Principal, slug string) bool {
	return p.Role == identity.RoleAdmin || !c.closed[p.UserID][slug]
}

func (c *fakeCatalog) ListServersFor(_ context.Context, p identity.Principal) ([]catalog.MemberServerView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	if c.listErr != nil {
		return nil, c.listErr
	}
	var out []catalog.MemberServerView
	for _, s := range c.servers {
		if c.visible(p, s.Slug) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (c *fakeCatalog) GetToolFor(_ context.Context, p identity.Principal, ref string) (catalog.ToolView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.toolErr != nil {
		return catalog.ToolView{}, c.toolErr
	}
	if _, _, err := catalog.ParseToolRef(ref); err != nil {
		return catalog.ToolView{}, kernel.NewDomainError(catalog.CodeInvalidToolRef, "invalid tool ref")
	}
	t, ok := c.tools[ref]
	if !ok || !c.visible(p, t.Server) {
		return catalog.ToolView{}, kernel.NewDomainError(catalog.CodeNotFound, "tool not found")
	}
	return t, nil
}

type fakeConnections struct {
	mu     sync.Mutex
	byUser map[string][]connections.ConnectionSummary
	err    error
}

func (f *fakeConnections) ListConnections(_ context.Context, p identity.Principal) ([]connections.ConnectionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byUser[p.UserID], f.err
}

type searchCall struct {
	query   string
	limit   int
	servers []string
}

type fakeSearch struct {
	mu    sync.Mutex
	hits  []search.ToolHit
	err   error
	calls []searchCall
}

func (f *fakeSearch) SearchTools(_ context.Context, query string, limit int, servers []string) ([]search.ToolHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, searchCall{query, limit, servers})
	if f.err != nil {
		return nil, f.err
	}
	var out []search.ToolHit
	for _, h := range f.hits {
		for _, s := range servers {
			if h.Server == s && len(out) < limit {
				out = append(out, h)
			}
		}
	}
	return out, nil
}

type fakeGateway struct {
	mu       sync.Mutex
	calls    []gateway.CallToolCommand
	result   gateway.ToolResult
	err      error
	listed   []gateway.ListCallsQuery
	rows     []gateway.CallRecord
	listErr  error
	echoArgs bool
}

func (f *fakeGateway) CallTool(_ context.Context, c gateway.CallToolCommand) (gateway.ToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if f.err != nil {
		return gateway.ToolResult{}, f.err
	}
	if f.echoArgs {
		var text bytes.Buffer
		enc := json.NewEncoder(&text)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(string(c.Arguments))
		return gateway.ToolResult{Content: json.RawMessage(`[{"type":"text","text":` + strings.TrimSpace(text.String()) + `}]`)}, nil
	}
	return f.result, nil
}

func (f *fakeGateway) ListCalls(_ context.Context, q gateway.ListCallsQuery) ([]gateway.CallRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, q)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if q.Principal.Role != identity.RoleAdmin {
		return nil, kernel.NewDomainError("forbidden", "administrators only")
	}
	return f.rows, nil
}

// ---- guards: tokens and sessions of the fake identity ----

type principalKey struct{}

func principalOf(r *http.Request) (identity.Principal, bool) {
	p, ok := r.Context().Value(principalKey{}).(identity.Principal)
	return p, ok
}

type people struct {
	tokens   map[string]identity.Principal // bearer token -> principal
	sessions map[string]identity.Principal // session cookie -> principal
}

func (pp *people) bearer() httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			tok, ok := strings.CutPrefix(h, "Bearer ")
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				httpx.WriteError(w, "unauthenticated", "bearer token required")
				return
			}
			p, ok := pp.tokens[tok]
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				httpx.WriteError(w, "invalid_token", "invalid token")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
		})
	}
}

func (pp *people) session() httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				httpx.WriteError(w, "unauthenticated", "tokens do not work on the web API")
				return
			}
			c, err := r.Cookie("session")
			p, ok := identity.Principal{}, false
			if err == nil {
				p, ok = pp.sessions[c.Value]
			}
			if !ok {
				httpx.WriteError(w, "unauthenticated", "sign in required")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
		})
	}
}

func admin() httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, _ := principalOf(r); p.Role != identity.RoleAdmin {
				httpx.WriteError(w, "forbidden", "admin only")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---- the environment: the routes behind the composition's chain ----

const (
	aliceToken = "sk_alice"
	adminToken = "sk_admin"
	carolToken = "sk_carol"
	testLimit  = 1 << 20 // the /mcp body limit of the tests (production: MCP_MAX_BODY_BYTES)
)

type env struct {
	url       string
	port      string
	publicURL string
	cat       *fakeCatalog
	con       *fakeConnections
	search    *fakeSearch
	gw        *fakeGateway
	logs      *bytes.Buffer
	logMu     *sync.Mutex
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (e *env) logText() string {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	return e.logs.String()
}

func principal(id string, role identity.Role) identity.Principal {
	return identity.Principal{UserID: id, Name: id, Role: role, Groups: []string{}, Via: identity.ViaToken, TokenID: "tok-" + id}
}

// newEnv serves the module's routes on a loopback port, behind the body limit the composition
// puts in front of /mcp. PUBLIC_URL is http://localhost:<port>; allowedHosts is ALLOWED_HOSTS.
func newEnv(t *testing.T, allowedHosts ...string) *env {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	e := &env{
		url: "http://" + ln.Addr().String(), port: port, publicURL: "http://localhost:" + port,
		cat:    &fakeCatalog{tools: map[string]catalog.ToolView{}, closed: map[string]map[string]bool{}},
		con:    &fakeConnections{byUser: map[string][]connections.ConnectionSummary{}},
		search: &fakeSearch{}, gw: &fakeGateway{}, logs: &bytes.Buffer{}, logMu: &sync.Mutex{},
	}
	pp := &people{
		tokens: map[string]identity.Principal{
			aliceToken: principal("alice", identity.RoleMember),
			carolToken: principal("carol", identity.RoleMember),
			adminToken: principal("root", identity.RoleAdmin),
		},
		sessions: map[string]identity.Principal{
			"admin-session":  principal("root", identity.RoleAdmin),
			"member-session": principal("alice", identity.RoleMember),
		},
	}
	logger := slog.New(slog.NewJSONHandler(lockedWriter{e.logMu, e.logs}, nil))
	routes, prefixes := gwhttp.RoutesWith(
		gwhttp.Guards{Session: pp.session(), Admin: admin(), Bearer: pp.bearer(), Principal: principalOf},
		gwhttp.Deps{Catalog: e.cat, Connections: e.con, Search: e.search, Gateway: e.gw},
		gwhttp.Options{PublicURL: e.publicURL + "/", AllowedHosts: allowedHosts, Version: "1.0.1", MaxBodyBytes: testLimit, Logger: logger},
	)
	router := httpx.NewRouter(routes, prefixes, httpx.NotFound())
	srv := &http.Server{Handler: httpx.LargeBodyLimit(testLimit)(router), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return e
}

// ---- raw requests ----

type reply struct {
	status int
	header http.Header
	body   []byte
	msg    rpcMessage
	hasMsg bool
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMessage struct {
	ID     any            `json:"id"`
	Result map[string]any `json:"result"`
	Error  *rpcError      `json:"error"`
}

// do sends a request; hdr entries "Name: value" are set after the defaults, an empty value removes
// the header.
func (e *env) do(t *testing.T, method, path, body string, hdr ...string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hdr {
		k, v, _ := strings.Cut(h, ":")
		v = strings.TrimSpace(v)
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: b}
}

var mcpHeaders = []string{"Content-Type: application/json", "Accept: application/json, text/event-stream"}

// post is compat's mcpPost: a JSON-RPC body with a bearer token and both Accept types.
func (e *env) post(t *testing.T, token, body string, hdr ...string) reply {
	t.Helper()
	h := append([]string{"Authorization: Bearer " + token}, mcpHeaders...)
	r := e.do(t, "POST", "/mcp", body, append(h, hdr...)...)
	var req struct {
		ID any `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	for _, m := range messages(t, r.header.Get("Content-Type"), r.body) {
		if (req.ID != nil && fmt.Sprint(m.ID) == fmt.Sprint(req.ID)) || (req.ID == nil && m.Error != nil) || (m.ID == nil && m.Error != nil) {
			r.msg, r.hasMsg = m, true
		}
	}
	return r
}

// messages reads a JSON or an event-stream body leniently, as compat's reader does.
func messages(t *testing.T, ct string, body []byte) []rpcMessage {
	t.Helper()
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		var out []rpcMessage
		for _, event := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n\n") {
			var data []string
			for _, line := range strings.Split(event, "\n") {
				if rest, ok := strings.CutPrefix(line, "data:"); ok {
					data = append(data, strings.TrimPrefix(rest, " "))
				}
			}
			if p := strings.Join(data, "\n"); strings.TrimSpace(p) != "" {
				var m rpcMessage
				if err := json.Unmarshal([]byte(p), &m); err != nil {
					t.Fatalf("event %q: %v", p, err)
				}
				out = append(out, m)
			}
		}
		return out
	case strings.HasPrefix(ct, "application/json"):
		var m rpcMessage
		if err := json.Unmarshal(body, &m); err == nil {
			return []rpcMessage{m}
		}
		var batch []rpcMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil // a REST error body (the Origin and token guards)
		}
		return batch
	}
	return nil
}

func rpcBody(id int, method string, params any) string {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func initializeBody(version string) string {
	return rpcBody(1, "initialize", map[string]any{"protocolVersion": version, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "raw", "version": "1"}})
}

// toolsCall calls a tool over the classic protocol and returns the wire result.
func (e *env) toolsCall(t *testing.T, token, name string, args any) map[string]any {
	t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	r := e.post(t, token, rpcBody(3, "tools/call", params))
	if r.status != 200 || !r.hasMsg || r.msg.Error != nil || r.msg.Result == nil {
		t.Fatalf("tools/call %s: %d %+v %.300s", name, r.status, r.msg.Error, r.body)
	}
	return r.msg.Result
}

func resultText(res map[string]any) string {
	var sb strings.Builder
	blocks, _ := res["content"].([]any)
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && m["type"] == "text" {
			fmt.Fprint(&sb, m["text"])
		}
	}
	return sb.String()
}

func errorCode(t *testing.T, r reply) string {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("not an error body: %v %.200s", err, r.body)
	}
	return e.Error.Code + "|" + e.Error.Message
}

func toolNames(res map[string]any) []string {
	var names []string
	tools, _ := res["tools"].([]any)
	for _, tl := range tools {
		if m, ok := tl.(map[string]any); ok {
			names = append(names, fmt.Sprint(m["name"]))
		}
	}
	return names
}
