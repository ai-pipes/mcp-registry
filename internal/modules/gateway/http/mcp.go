package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// The JSON-RPC endpoint of /mcp, written by hand (the spike of milestone 6 found that the server
// side of go-sdk v1.8.0 cannot give the reference's surface: see the task report). It is stateless
// and serves both eras of the protocol on one address, as the reference's SDK does:
//
//   - classic (2025 revisions): `initialize` negotiates a revision, `tools/list`, `tools/call` and
//     `ping` work without it; the MCP-Protocol-Version header, when sent, must name a classic
//     revision; batches are served;
//   - 2026-07-28: a request whose params._meta carries the protocol version claim (what go-sdk's
//     client sends after `server/discover`); `server/discover`, `tools/list` and `tools/call`, each
//     result with resultType "complete" and the server info in _meta; the claim is checked against
//     the MCP-Protocol-Version, Mcp-Method and Mcp-Name headers.
//
// There is never an Mcp-Session-Id, a GET stream or a DELETE; every answer is application/json
// (ruling 11), a notification or a posted response is an empty 202.

const (
	modernRevision = "2026-07-28"
	latestClassic  = "2025-11-25"
	claimKey       = "io.modelcontextprotocol/protocolVersion"
	serverInfoKey  = "io.modelcontextprotocol/serverInfo"
	serverName     = "mcp-registry"

	codeParseError      = -32700
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeServerError     = -32000
	codeHeaderMismatch  = -32020
	codeUnsupportedRevn = -32022
)

// classicRevisions are the revisions `initialize` echoes; headerRevisions are those the
// MCP-Protocol-Version header may name on a classic request.
var (
	classicRevisions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
	headerRevisions  = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"}
)

const (
	msgMethodNotAllowed = "Method not allowed."
	msgNotAcceptable    = "Not Acceptable: Client must accept both application/json and text/event-stream"
	msgUnsupportedMedia = "Unsupported Media Type: Content-Type must be application/json"
	msgParseError       = "Parse error: Invalid JSON"
	msgNotJSONRPC       = "Bad Request: the request body is not a valid JSON-RPC message"
	msgNoModernBatch    = "Bad Request: JSON-RPC batches are not supported by this endpoint"
	msgMethodNotFound   = "Method not found"
	msgInvalidParams    = "Invalid params"
)

// endpoint serves the JSON-RPC of /mcp for the principal the bearer guard stored.
type endpoint struct {
	d          Deps
	o          Options
	principal  func(*http.Request) (identity.Principal, bool)
	log        *slog.Logger
	connectURL string
}

// rpcErr is a JSON-RPC error object.
type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// message is one decoded JSON-RPC message. ID is nil when absent and "null" when null.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  *string         `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func (m message) isRequest() bool {
	return m.JSONRPC == "2.0" && m.Method != nil && validID(m.ID)
}

func (m message) isNotification() bool { return m.JSONRPC == "2.0" && m.Method != nil && m.ID == nil }

func (m message) isResponse() bool {
	return m.JSONRPC == "2.0" && m.Method == nil && validID(m.ID) && (m.Result != nil || m.Error != nil)
}

// wellFormed reports a request, a notification or a response.
func (m message) wellFormed() bool { return m.isRequest() || m.isNotification() || m.isResponse() }

func validID(id json.RawMessage) bool {
	id = bytes.TrimSpace(id)
	return len(id) > 0 && (id[0] == '"' || id[0] == '-' || (id[0] >= '0' && id[0] <= '9'))
}

// claim is the protocol version a request's params._meta claims: present reports the key, ok that
// it is a string.
func (m message) claim() (version string, present, ok bool) {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(m.Params, &p) != nil || p.Meta == nil {
		return "", false, false
	}
	raw, present := p.Meta[claimKey]
	if !present {
		return "", false, false
	}
	return version, true, json.Unmarshal(raw, &version) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (h *endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeTransportError(w, http.StatusMethodNotAllowed, codeServerError, msgMethodNotAllowed)
		return
	}
	if accept := strings.Join(r.Header.Values("Accept"), ","); !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		writeTransportError(w, http.StatusNotAcceptable, codeServerError, msgNotAcceptable)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeTransportError(w, http.StatusUnsupportedMediaType, codeServerError, msgUnsupportedMedia)
		return
	}
	p, ok := h.principal(r)
	if !ok {
		httpx.WriteDomainError(w, r, errors.New("mcp request without an authenticated principal"))
		return
	}
	body := r.Body
	if h.o.MaxBodyBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, h.o.MaxBodyBytes)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.WriteError(w, "limit_exceeded", "request body is too large")
			return
		}
		writeTransportError(w, http.StatusBadRequest, codeParseError, msgParseError)
		return
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !json.Valid(raw) {
		writeTransportError(w, http.StatusBadRequest, codeParseError, msgParseError)
		return
	}
	x := &exchange{h: h, w: w, r: r, p: p, headerVersion: strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))}
	if raw[0] == '[' {
		x.batch(raw)
		return
	}
	var m message
	if json.Unmarshal(raw, &m) != nil {
		writeTransportError(w, http.StatusBadRequest, codeInvalidRequest, msgNotJSONRPC)
		return
	}
	x.single(m)
}

// exchange is one HTTP request of /mcp.
type exchange struct {
	h             *endpoint
	w             http.ResponseWriter
	r             *http.Request
	p             identity.Principal
	headerVersion string
}

// reject answers a request the classification refuses: the status, a JSON-RPC error with the
// request's id (null when there is none).
func (x *exchange) reject(status int, id json.RawMessage, e rpcErr) {
	writeJSON(x.w, status, errorBody(id, e))
}

func (x *exchange) single(m message) {
	switch {
	case m.isResponse():
		x.w.WriteHeader(http.StatusAccepted)
		return
	case !m.wellFormed():
		x.reject(http.StatusBadRequest, nil, rpcErr{Code: codeInvalidRequest, Message: msgNotJSONRPC})
		return
	}
	method := *m.Method
	claimed, hasClaim, claimOK := m.claim()
	headerModern := x.headerVersion == modernRevision
	if hasClaim {
		if e, status := x.checkClaim(m, method, claimed, claimOK); e != nil {
			x.reject(status, m.ID, *e)
			return
		}
		if m.isNotification() {
			x.w.WriteHeader(http.StatusAccepted)
			return
		}
		x.answer(x.modern(m, method), m.ID)
		return
	}
	if headerModern {
		if method == "initialize" {
			x.reject(http.StatusBadRequest, m.ID, mismatch("an initialize request (legacy handshake) was sent with a modern MCP-Protocol-Version header"))
			return
		}
		if m.isNotification() {
			x.w.WriteHeader(http.StatusAccepted)
			return
		}
		x.reject(http.StatusBadRequest, m.ID, rpcErr{Code: codeInvalidParams, Message: "Invalid params: the MCP-Protocol-Version header names protocol revision " + modernRevision +
			", but the request is missing the required per-request envelope key(s): " + claimKey})
		return
	}
	if m.isNotification() {
		x.w.WriteHeader(http.StatusAccepted)
		return
	}
	if method != "initialize" && !x.classicHeaderOK() {
		return
	}
	x.answer(x.classic(m, method), m.ID)
}

// checkClaim validates the envelope claim against the headers; a non-nil error is the refusal.
func (x *exchange) checkClaim(m message, method, claimed string, ok bool) (*rpcErr, int) {
	switch {
	case !ok:
		return &rpcErr{Code: codeInvalidParams, Message: "Invalid _meta envelope for protocol revision " + modernRevision + ": " + claimKey + ": expected a protocol version string"}, http.StatusBadRequest
	case x.headerVersion != "" && x.headerVersion != claimed:
		e := mismatch("the body envelope names protocol version " + claimed + " but the MCP-Protocol-Version header names " + x.headerVersion)
		return &e, http.StatusBadRequest
	case x.headerPresent("Mcp-Method") && strings.TrimSpace(x.r.Header.Get("Mcp-Method")) != method:
		e := mismatch("the body names method " + method + " but the Mcp-Method header names " + strings.TrimSpace(x.r.Header.Get("Mcp-Method")))
		return &e, http.StatusBadRequest
	case claimed != modernRevision:
		return &rpcErr{
			Code:    codeUnsupportedRevn,
			Message: "Unsupported protocol version: " + claimed + " (supported versions: " + modernRevision + ")",
			Data:    map[string]any{"supported": []string{modernRevision}, "requested": claimed},
		}, http.StatusBadRequest
	}
	if method == "tools/call" && x.headerPresent("Mcp-Name") {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if name := strings.TrimSpace(x.r.Header.Get("Mcp-Name")); name != p.Name {
			e := mismatch("the body names " + p.Name + " but the Mcp-Name header names " + name)
			return &e, http.StatusBadRequest
		}
	}
	return nil, 0
}

func (x *exchange) headerPresent(name string) bool {
	_, ok := x.r.Header[http.CanonicalHeaderKey(name)]
	return ok
}

func mismatch(what string) rpcErr {
	return rpcErr{Code: codeHeaderMismatch, Message: "Bad Request: the request headers and body disagree: " + what}
}

// classicHeaderOK refuses a classic request whose MCP-Protocol-Version names no classic revision.
func (x *exchange) classicHeaderOK() bool {
	if x.headerVersion == "" || slices.Contains(headerRevisions, x.headerVersion) {
		return true
	}
	x.reject(http.StatusBadRequest, nil, rpcErr{Code: codeServerError, Message: "Bad Request: Unsupported protocol version: " + x.headerVersion +
		" (supported versions: " + strings.Join(headerRevisions, ", ") + ")"})
	return false
}

// batch serves a JSON-RPC array of the classic era: each request answered in order, nothing for
// notifications and posted responses.
func (x *exchange) batch(raw []byte) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		x.reject(http.StatusBadRequest, nil, rpcErr{Code: codeInvalidRequest, Message: msgNotJSONRPC})
		return
	}
	msgs := make([]message, len(items))
	hasInitialize := false
	for i, it := range items {
		if json.Unmarshal(it, &msgs[i]) != nil || !msgs[i].wellFormed() {
			x.reject(http.StatusBadRequest, nil, rpcErr{Code: codeInvalidRequest, Message: msgNotJSONRPC})
			return
		}
		if _, hasClaim, _ := msgs[i].claim(); hasClaim || x.headerVersion == modernRevision {
			x.reject(http.StatusBadRequest, nil, rpcErr{Code: codeInvalidRequest, Message: msgNoModernBatch})
			return
		}
		hasInitialize = hasInitialize || (msgs[i].Method != nil && *msgs[i].Method == "initialize")
	}
	if !hasInitialize && !x.classicHeaderOK() {
		return
	}
	var answers [][]byte
	for _, m := range msgs {
		if m.isRequest() {
			answers = append(answers, answerBody(x.classic(m, *m.Method), m.ID))
		}
	}
	if len(answers) == 0 {
		x.w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(x.w, http.StatusOK, append(append([]byte("["), bytes.Join(answers, []byte(","))...), ']'))
}

// outcome is the result object (raw JSON) or the error of one request.
type outcome struct {
	result []byte
	err    *rpcErr
}

func (x *exchange) answer(o outcome, id json.RawMessage) {
	writeJSON(x.w, http.StatusOK, answerBody(o, id))
}

func answerBody(o outcome, id json.RawMessage) []byte {
	if o.err != nil {
		return errorBody(id, *o.err)
	}
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(bytes.TrimSpace(id))
	b.WriteString(`,"result":`)
	b.Write(o.result)
	b.WriteByte('}')
	return b.Bytes()
}

func errorBody(id json.RawMessage, e rpcErr) []byte {
	errJSON, _ := marshalJS(e) // ints, strings and string slices always encode
	idJSON := []byte("null")
	if validID(id) {
		idJSON = bytes.TrimSpace(id)
	}
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","error":`)
	b.Write(errJSON)
	b.WriteString(`,"id":`)
	b.Write(idJSON)
	b.WriteByte('}')
	return b.Bytes()
}

// classic dispatches a request of the 2025 revisions.
func (x *exchange) classic(m message, method string) outcome {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion any `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		version := latestClassic
		if v, ok := p.ProtocolVersion.(string); ok && slices.Contains(classicRevisions, v) {
			version = v
		}
		return outcome{result: x.h.initializeResult(version)}
	case "ping":
		return outcome{result: []byte("{}")}
	case "tools/list":
		return outcome{result: toolsListResult}
	case "tools/call":
		return x.toolsCall(m)
	}
	return outcome{err: &rpcErr{Code: codeMethodNotFound, Message: msgMethodNotFound}}
}

// modern dispatches a request of the 2026-07-28 revision; every result carries resultType and the
// server info in _meta.
func (x *exchange) modern(m message, method string) outcome {
	var o outcome
	switch method {
	case "server/discover":
		o = outcome{result: x.h.discoverResult()}
	case "tools/list":
		o = outcome{result: toolsListResult}
	case "tools/call":
		o = x.toolsCall(m)
	default:
		return outcome{err: &rpcErr{Code: codeMethodNotFound, Message: msgMethodNotFound}}
	}
	if o.err == nil {
		o.result = x.h.withModernFields(o.result, method == "server/discover")
	}
	return o
}

func (h *endpoint) serverInfo() []byte {
	b, _ := marshalJS(struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}{serverName, h.o.Version})
	return b
}

func (h *endpoint) initializeResult(version string) []byte {
	b, _ := marshalJS(struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
		Instructions    string          `json:"instructions"`
	}{version, capabilities, h.serverInfo(), Instructions})
	return b
}

func (h *endpoint) discoverResult() []byte {
	b, _ := marshalJS(struct {
		SupportedVersions []string        `json:"supportedVersions"`
		Capabilities      json.RawMessage `json:"capabilities"`
		Instructions      string          `json:"instructions"`
	}{[]string{modernRevision}, capabilities, Instructions})
	return b
}

// withModernFields appends resultType (and, for server/discover, the cache hints) and the server
// info to a result object.
func (h *endpoint) withModernFields(result []byte, discover bool) []byte {
	var b bytes.Buffer
	body := bytes.TrimSuffix(bytes.TrimSpace(result), []byte("}"))
	b.Write(body)
	if !bytes.Equal(bytes.TrimSpace(body), []byte("{")) {
		b.WriteByte(',')
	}
	b.WriteString(`"resultType":"complete",`)
	if discover {
		b.WriteString(`"ttlMs":0,"cacheScope":"private",`)
	}
	b.WriteString(`"_meta":{"` + serverInfoKey + `":`)
	b.Write(h.serverInfo())
	b.WriteString(`}}`)
	return b.Bytes()
}

// capabilities are exactly the tools capability without change notifications.
var capabilities = json.RawMessage(`{"tools":{"listChanged":false}}`)

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeTransportError is a refusal of the transport (no request id known): a JSON-RPC error with
// id null, in the key order of the reference's transport.
func writeTransportError(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, errorBodyTransport(code, message))
}

func errorBodyTransport(code int, message string) []byte {
	msg, _ := marshalJS(message)
	return []byte(`{"jsonrpc":"2.0","error":{"code":` + strconv.Itoa(code) + `,"message":` + string(msg) + `},"id":null}`)
}
