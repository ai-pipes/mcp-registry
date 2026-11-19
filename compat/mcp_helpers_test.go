//go:build compat

package compat_test

// Helpers of the MCP and gateway scenarios (mcp_protocol_test.go, mcp_tools_test.go,
// gateway_test.go, gateway_rest_test.go): raw JSON-RPC over HTTP with a lenient reader, the
// ledger that scans every MCP answer for what must never leave, servers and people on the fakes.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

const (
	mcpAccept  = "Accept: application/json, text/event-stream"
	mcpJSON    = "Content-Type: application/json"
	mcpClassic = "2025-11-25" // the newest revision of the initialize handshake
)

// ---- raw JSON-RPC over HTTP ----

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type rpcMessage struct {
	ID     any            `json:"id"`
	Result map[string]any `json:"result"`
	Error  *rpcError      `json:"error"`
}

// mcpReply is one HTTP answer of /mcp: the status, the headers, the body and, when it carries a
// JSON-RPC message for the request, that message. The MCP spec lets a server answer a POST with
// application/json or with text/event-stream; both are read, the event stream leniently (the
// fields id: and retry:, comments and unknown fields are ignored, several events are allowed and
// the message whose id is the request's is the answer).
type mcpReply struct {
	Status int
	Header http.Header
	Body   []byte
	SSE    bool
	Msg    rpcMessage
	HasMsg bool
}

func (r mcpReply) contentType() string { return r.Header.Get("Content-Type") }

// mcpIsStream and mcpIsJSON compare content types by prefix: parameters (charset) are allowed.
func mcpIsStream(ct string) bool { return strings.HasPrefix(ct, "text/event-stream") }
func mcpIsJSON(ct string) bool   { return strings.HasPrefix(ct, "application/json") }

// mcpMessages decodes every JSON-RPC message in a body of either type.
func mcpMessages(t *testing.T, ct string, body []byte) (msgs []rpcMessage, sse bool) {
	t.Helper()
	switch {
	case mcpIsStream(ct):
		text := strings.ReplaceAll(string(body), "\r\n", "\n")
		for _, event := range strings.Split(text, "\n\n") {
			var data []string
			name := ""
			for _, line := range strings.Split(event, "\n") {
				if rest, ok := strings.CutPrefix(line, "data:"); ok {
					data = append(data, strings.TrimPrefix(rest, " "))
				} else if rest, ok := strings.CutPrefix(line, "event:"); ok {
					name = strings.TrimSpace(rest)
				}
			}
			// A priming event (an id and empty data) and events of another type are not answers.
			payload := strings.Join(data, "\n")
			if strings.TrimSpace(payload) == "" || (name != "" && name != "message") {
				continue
			}
			var m rpcMessage
			if err := json.Unmarshal([]byte(payload), &m); err != nil {
				t.Fatalf("a message event of the stream is not JSON: %v: %.300s", err, body)
			}
			msgs = append(msgs, m)
		}
		return msgs, true
	case mcpIsJSON(ct):
		var m rpcMessage
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("a JSON-RPC message was expected (%s): %v: %.300s", ct, err, body)
		}
		return []rpcMessage{m}, false
	}
	return nil, false
}

// mcpPost sends one JSON-RPC body to /mcp with a bearer token and both Accept types; hdr entries
// are added after (and override) the defaults, an empty value removes one. The answer is recorded
// in the ledger (which may be nil only for requests with no token worth hiding).
func mcpPost(t *testing.T, led *mcpLedger, c *harness.Client, token, body string, hdr ...string) mcpReply {
	t.Helper()
	h := []string{"Authorization: Bearer " + token, mcpJSON, mcpAccept}
	resp := c.Do(t, "POST", "/mcp", body, append(h, hdr...)...)
	if led != nil {
		led.record(resp.Body)
	}
	r := mcpReply{Status: resp.Status, Header: resp.Header, Body: resp.Body}
	var req struct {
		ID any `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	msgs, sse := mcpMessages(t, r.contentType(), r.Body)
	r.SSE = sse
	r.Msg, r.HasMsg = mcpSelect(msgs, req.ID)
	return r
}

// mcpSelect picks the answer to a request with that id from the messages of a reply: the message
// whose id is the request's; for a request without an id (or one the server could not read) a
// single message with a null id and an error.
func mcpSelect(msgs []rpcMessage, reqID any) (rpcMessage, bool) {
	var found rpcMessage
	var ok bool
	for _, m := range msgs {
		if (reqID != nil && fmt.Sprint(m.ID) == fmt.Sprint(reqID)) || (reqID == nil && m.Error != nil) {
			found, ok = m, true
		}
	}
	if !ok && reqID != nil && len(msgs) == 1 && msgs[0].ID == nil && msgs[0].Error != nil {
		return msgs[0], true
	}
	return found, ok
}

func mcpNoSession(t *testing.T, what string, r mcpReply) {
	t.Helper()
	if v := r.Header.Values("Mcp-Session-Id"); len(v) != 0 {
		t.Fatalf("%s: the server issued Mcp-Session-Id %v: it is stateless", what, v)
	}
}

func mcpClassicBody(id int, method string, params any) string {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func mcpInitializeBody(version string) string {
	return mcpClassicBody(1, "initialize", map[string]any{"protocolVersion": version, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "raw", "version": "1"}})
}

// mcpToolsCall calls a tool over the classic protocol and returns the result object as it went
// over the wire.
func mcpToolsCall(t *testing.T, led *mcpLedger, c *harness.Client, token, name string, args map[string]any) map[string]any {
	t.Helper()
	r := mcpPost(t, led, c, token, mcpClassicBody(3, "tools/call", map[string]any{"name": name, "arguments": args}))
	if r.Status != 200 || !r.HasMsg || r.Msg.Error != nil || r.Msg.Result == nil {
		t.Fatalf("tools/call %s: status %d error %+v body %.300s", name, r.Status, r.Msg.Error, r.Body)
	}
	return r.Msg.Result
}

// mcpResultText joins the text blocks of a wire result.
func mcpResultText(res map[string]any) string {
	var sb strings.Builder
	blocks, _ := res["content"].([]any)
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && m["type"] == "text" {
			fmt.Fprint(&sb, m["text"])
		}
	}
	return sb.String()
}

// mcpFieldNamed reports whether an input-validation message identifies the field as a path token.
// The wording is the MCP SDK's and not pinned, but the field must be named as one of
//   - "<field>" (quoted), as in path: ["<field>"] or a JSON key,
//   - /<field> (a JSON pointer),
//   - <field>: at the start of a word (the TypeScript image writes "<field>: Too small ...").
//
// The header "for tool <name>:" is removed first so that the tool's own name is not mistaken for a field.
func mcpFieldNamed(text, field string) bool {
	text = regexp.MustCompile(`for tool \S+:`).ReplaceAllString(text, "")
	q := regexp.QuoteMeta(field)
	return regexp.MustCompile(`"` + q + `"|/` + q + `\b|(^|[\s(\[])` + q + `:`).MatchString(text)
}

// mcpExpectInputError checks that a wire result is an input-validation error for the field: an
// error that is neither a success nor the not_found text, and that names the field as a path token.
func mcpExpectInputError(t *testing.T, what string, res map[string]any, field string) {
	t.Helper()
	text := mcpResultText(res)
	if res["isError"] != true || text == "" || strings.Contains(text, "is not available") || !mcpFieldNamed(text, field) {
		t.Errorf("%s: result %.300v, want an input-validation isError that names the field %q as a path token", what, res, field)
	}
}

// ---- the ledger: what must never appear in an answer of /mcp ----

// mcpLedger collects the RAW bytes of every MCP answer of a test (sessions opened through it, and
// raw requests passed to mcpPost) and, when the test ends, looks in them (and in the app log) for
// what must never leave: the upstream's address (every fake's URL contains host.docker.internal),
// the auth settings of a server, and the secrets, tokens and PATs the test noted.
type mcpLedger struct {
	t       *testing.T
	s       *harness.Stack
	mu      sync.Mutex
	seen    []string
	secrets []string
}

// mcpForbidden are the names of settings and the header and template of the scenarios' servers.
var mcpForbidden = []string{
	"host.docker.internal", "X-API-Key", "{secret}",
	"authorizeUrl", "tokenUrl", "revokeUrl", "clientId", "clientSecret", `"template"`, `\"template\"`,
}

func newMCPLedger(t *testing.T, s *harness.Stack) *mcpLedger {
	t.Helper()
	l := &mcpLedger{t: t, s: s}
	t.Cleanup(l.scan)
	return l
}

// note registers secrets (tokens, PATs, keys) that must appear in no answer and no log line.
func (l *mcpLedger) note(values ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.secrets = append(l.secrets, values...)
}

func (l *mcpLedger) record(body []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, string(body))
}

// session opens an MCP session whose every answer is recorded raw; the token is noted as a secret.
func (l *mcpLedger) session(token string) *mcp.ClientSession {
	l.t.Helper()
	l.note(token)
	return l.s.MCPRecording(l.t, token, l.record)
}

func (l *mcpLedger) scan() {
	l.mu.Lock()
	seen, secrets := slices.Clone(l.seen), slices.Clone(l.secrets)
	l.mu.Unlock()
	forbidden := append(slices.Clone(mcpForbidden), secrets...)
	for _, answer := range seen {
		for _, f := range forbidden {
			if strings.Contains(answer, f) {
				l.t.Errorf("an MCP answer contains %q: %.300s", harness.Redact(f), answer)
			}
		}
	}
	logs := l.s.Logs(l.t)
	for _, sec := range secrets {
		if strings.Contains(logs, sec) {
			l.t.Errorf("the app log contains the secret %s", harness.Redact(sec))
		}
	}
	if len(seen) == 0 {
		l.t.Error("the ledger saw no MCP answer: the scan proves nothing")
	}
}

// call is call_tool or any meta-tool through a session of the ledger.
func (l *mcpLedger) call(sess *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	l.t.Helper()
	return catalogCallMCP(l.t, sess, name, args)
}

// ---- servers and people on the fakes ----

func ptr[T any](v T) *T { return &v }

// savePAT saves a personal access token for the person; it returns "" or the failure.
func savePAT(t *testing.T, c *harness.Client, slug, token string) string {
	t.Helper()
	if r := c.Do(t, "PUT", "/api/connections/"+slug+"/pat", map[string]string{"token": token}); r.Status != 204 {
		return fmt.Sprintf("PUT pat of %s: %d %s", slug, r.Status, r.Body)
	}
	return ""
}

// gwServer is a server on a fake upstream with its tools synced.
type gwServer struct {
	slug  string
	name  string // the display name: what product texts call the server
	up    *fakes.Upstream
	tools []fakes.Tool
}

func (e *gwServer) ref(tool string) string { return e.slug + "." + tool }

// gwNewServer registers a server of authKind on a new fake upstream (tools default to one tool,
// echo). Its timeout is 5 s unless an option says otherwise. A pat_user server is synced with the
// admin's own token, as the product requires.
func gwNewServer(t *testing.T, led *mcpLedger, admin *harness.Client, tools []fakes.Tool, authKind string, opts ...fakes.ServerOpt) *gwServer {
	t.Helper()
	if tools == nil {
		tools = fakes.Tools("echo")
	}
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: tools})
	slug := harness.Unique("gw")
	opts = append([]fakes.ServerOpt{fakes.WithTimeoutMs(5000)}, opts...)
	body := fakes.ServerInput(up, slug, authKind, opts...)
	view := catalogRegister(t, admin, body)
	// The name that product texts use is the one the scenario asked for (the default display name
	// unless an option set another), not whatever the server answers.
	name, _ := body["name"].(string)
	if name == "" || view.Name != name {
		t.Fatalf("server %s: registered with the name %q, the response says %q", slug, name, view.Name)
	}
	if authKind == "pat_user" {
		token := "admin-pat-" + slug
		if led != nil {
			led.note(token)
		}
		if msg := savePAT(t, admin, slug, token); msg != "" {
			t.Fatal(msg)
		}
		catalogSyncOK(t, admin, slug)
	}
	if v := catalogGet(t, admin, slug); v.ToolCount != len(tools) {
		t.Fatalf("server %s: %d tools synced, want %d (last sync error %q)", slug, v.ToolCount, len(tools), v.syncError())
	}
	up.Reset()
	return &gwServer{slug: slug, name: name, up: up, tools: tools}
}

// gwPerson is a member with a token and an MCP session that records every answer raw.
type gwPerson struct {
	c       *harness.Client
	id      string
	token   string
	tokenID string
	mcp     *mcp.ClientSession
}

func gwNewPerson(t *testing.T, s *harness.Stack, led *mcpLedger) gwPerson {
	t.Helper()
	c := s.Member(t, harness.Unique("gw-person"))
	r := c.Do(t, "POST", "/api/tokens", map[string]string{"name": "gw"})
	var created struct{ ID, Token string }
	if r.Status != 201 {
		t.Fatalf("POST /api/tokens: %d %s", r.Status, r.Body)
	}
	r.JSON(t, "CreateTokenResponse", &created)
	return gwPerson{c: c, id: whoami(t, c).UserID, token: created.Token, tokenID: created.ID, mcp: led.session(created.Token)}
}

// call is call_tool.
func (p gwPerson) call(led *mcpLedger, ref string, args map[string]any) *mcp.CallToolResult {
	return led.call(p.mcp, "call_tool", map[string]any{"tool": ref, "arguments": args})
}

// gwRow is a row of gateway.calls.
type gwRow struct {
	Server, Tool, Outcome, UserID string
	TokenID                       *string
	DurationMs                    int
}

// gwRowsWhere reads the call log rows matching the condition, oldest first.
func gwRowsWhere(t *testing.T, s *harness.Stack, where string, args ...any) []gwRow {
	t.Helper()
	rows, err := s.DB.Query(t.Context(), `select server, tool, outcome, user_id::text, token_id::text, duration_ms
		from gateway.calls where `+where+` order by id`, args...)
	if err != nil {
		t.Fatalf("select gateway.calls: %v", err)
	}
	defer rows.Close()
	var out []gwRow
	for rows.Next() {
		var r gwRow
		if err := rows.Scan(&r.Server, &r.Tool, &r.Outcome, &r.UserID, &r.TokenID, &r.DurationMs); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func gwRows(t *testing.T, s *harness.Stack, server string) []gwRow {
	t.Helper()
	return gwRowsWhere(t, s, "server = $1", server)
}

func gwOutcomes(rows []gwRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Outcome)
	}
	return out
}

// mcpToolNames lists the names in a tools/list result.
func mcpToolNames(res map[string]any) []string {
	var names []string
	tools, _ := res["tools"].([]any)
	for _, tl := range tools {
		if m, ok := tl.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	return names
}
