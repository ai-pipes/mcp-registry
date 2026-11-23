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
	msgEmptyBatch       = "Bad Request: empty JSON-RPC batch"
	msgInvalidInBatch   = "Bad Request: JSON-RPC batch contains an invalid message"
	msgModernInBatch    = "Bad Request: JSON-RPC batches may not contain requests for protocol revision 2026-07-28 or later"
	msgOneInitialize    = "Invalid Request: Only one initialization request is allowed"
	msgMethodNotFound   = "Method not found"
	msgInvalidParams    = "Invalid params"
)

// maxBatch is the reference's MAX_BATCH_SIZE: a JSON-RPC array of more messages is refused.
const maxBatch = 100

var msgBatchTooLarge = "Invalid Request: Batch must not exceed " + strconv.Itoa(maxBatch) + " messages"

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

// ServeHTTP decides in the reference's order (createMcpHandler's handle, then the classifier,
// then serveModern or the legacy transport's handlePostRequest): the HTTP method, Content-Type,
// the body (its size, then JSON), the classification of the JSON-RPC message(s), then the checks
// of the era.
func (h *endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeTransportError(w, http.StatusMethodNotAllowed, codeServerError, msgMethodNotAllowed)
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
		raw = nil // an unreadable body is answered like an invalid one
	}
	x := &exchange{h: h, w: w, r: r, p: p, headerVersion: strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !json.Valid(raw) {
		// No JSON body: the legacy transport answers, Accept first.
		if x.acceptable() {
			writeTransportError(w, http.StatusBadRequest, codeParseError, msgParseError)
		}
		return
	}
	if raw[0] == '[' {
		x.batch(raw)
		return
	}
	x.single(parseMessage(raw))
}

// exchange is one HTTP request of /mcp.
type exchange struct {
	h             *endpoint
	w             http.ResponseWriter
	r             *http.Request
	p             identity.Principal
	headerVersion string
}

// reject answers with the status and a JSON-RPC error carrying id ("null" when nil).
func (x *exchange) reject(status int, id json.RawMessage, e rpcErr) {
	writeJSON(x.w, status, errorBody(id, e))
}

// acceptable is the legacy transport's Accept check: both types must be named; otherwise the 406
// is written and false returned.
func (x *exchange) acceptable() bool {
	accept := strings.Join(x.r.Header.Values("Accept"), ",")
	if strings.Contains(accept, "application/json") && strings.Contains(accept, "text/event-stream") {
		return true
	}
	writeTransportError(x.w, http.StatusNotAcceptable, codeServerError, msgNotAcceptable)
	return false
}

// header is a request header with surrounding blanks removed; present is false when it is absent
// or empty.
func (x *exchange) header(name string) (value string, present bool) {
	v := strings.TrimSpace(x.r.Header.Get(name))
	return v, v != ""
}

// headerMismatch and unsupportedRevision are error data in the reference's key order.
type headerMismatch struct {
	Header string `json:"header"`
	Body   string `json:"body"`
}

type unsupportedRevision struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

func mismatch(header, what string) rpcErr {
	return rpcErr{
		Code: codeHeaderMismatch, Message: "Bad Request: the request headers and body disagree: " + what,
		Data: map[string]headerMismatch{"mismatch": {header, what}},
	}
}

// single classifies one message (classifyRequestBody / classifyNotificationBody) and serves it in
// its era.
func (x *exchange) single(m message) {
	headerModern := x.headerVersion != "" && isModern(x.headerVersion)
	switch m.kind {
	case kindInvalid:
		x.reject(http.StatusBadRequest, m.echo, rpcErr{Code: codeInvalidRequest, Message: msgNotJSONRPC})
	case kindResponse:
		x.legacy(m)
	case kindRequest:
		switch {
		case m.method == "initialize" && !m.validModernClaim():
			if headerModern {
				x.reject(http.StatusBadRequest, m.echo, mismatch(x.headerVersion, "an initialize request (legacy handshake) was sent with a modern MCP-Protocol-Version header"))
				return
			}
			x.legacy(m)
		case m.hasClaim():
			if issues := envelopeIssues(m.meta); len(issues) > 0 {
				x.reject(http.StatusBadRequest, m.echo, rpcErr{
					Code:    codeInvalidParams,
					Message: "Invalid _meta envelope for protocol revision " + modernRevision + ": " + issues[0].Key + ": " + issues[0].Problem,
					Data:    map[string]any{"envelope": issues[0]},
				})
				return
			}
			claimed, _ := m.claimVersion()
			if x.headerVersion != "" && x.headerVersion != claimed {
				x.reject(http.StatusBadRequest, m.echo, mismatch(x.headerVersion, "the body envelope names protocol version "+claimed+" but the MCP-Protocol-Version header names "+x.headerVersion))
				return
			}
			if mm, ok := x.header("Mcp-Method"); ok && mm != m.method {
				x.reject(http.StatusBadRequest, m.echo, mismatch(mm, "the body names method "+m.method+" but the Mcp-Method header names "+mm))
				return
			}
			x.modern(m, claimed)
		case headerModern:
			missing := []string{"_meta"}
			if m.meta != nil {
				missing = nil
				for _, is := range envelopeIssues(m.meta) {
					if is.Problem == "missing" {
						missing = append(missing, is.Key)
					}
				}
				if len(missing) == 0 {
					missing = []string{claimKey}
				}
			}
			x.reject(http.StatusBadRequest, m.echo, rpcErr{
				Code: codeInvalidParams,
				Message: "Invalid params: the MCP-Protocol-Version header names protocol revision " + x.headerVersion +
					", but the request is missing the required per-request envelope key(s): " + strings.Join(missing, ", "),
				Data: map[string]any{"envelope": map[string]any{"missing": missing}},
			})
		default:
			x.legacy(m)
		}
	case kindNotification:
		switch {
		case m.hasClaim():
			claimed, ok := m.claimVersion()
			if !ok {
				problem := "expected a protocol version string"
				for _, is := range envelopeIssues(m.meta) {
					if is.Key == claimKey {
						problem = is.Problem
						break
					}
				}
				issue := envelopeIssue{claimKey, problem}
				x.reject(http.StatusBadRequest, nullID, rpcErr{
					Code: codeInvalidParams, Message: "Invalid _meta envelope for protocol revision " + modernRevision + ": " + claimKey + ": " + problem,
					Data: map[string]any{"envelope": issue},
				})
				return
			}
			if x.headerVersion != "" && x.headerVersion != claimed {
				x.reject(http.StatusBadRequest, nullID, mismatch(x.headerVersion, "the notification envelope names protocol version "+claimed+" but the MCP-Protocol-Version header names "+x.headerVersion))
				return
			}
			if mm, ok := x.header("Mcp-Method"); ok && isModern(claimed) && mm != m.method {
				x.reject(http.StatusBadRequest, nullID, mismatch(mm, "the notification body names method "+m.method+" but the Mcp-Method header names "+mm))
				return
			}
			x.modern(m, claimed)
		case headerModern:
			if mm, ok := x.header("Mcp-Method"); ok && mm != m.method {
				x.reject(http.StatusBadRequest, nullID, mismatch(mm, "the notification body names method "+m.method+" but the Mcp-Method header names "+mm))
				return
			}
			x.modern(m, x.headerVersion)
		default:
			x.legacy(m)
		}
	}
}

// modern is serveModern: the claimed revision must be one this server speaks, a request must carry
// the standard headers (validateStandardRequestHeaders); a notification is accepted, a request
// answered with the revision's fields.
func (x *exchange) modern(m message, revision string) {
	if revision != modernRevision {
		requested := revision
		if requested == "" {
			requested = "unknown"
		}
		x.reject(http.StatusBadRequest, m.echo, rpcErr{
			Code: codeUnsupportedRevn, Message: "Unsupported protocol version: " + requested,
			Data: unsupportedRevision{[]string{modernRevision}, requested},
		})
		return
	}
	if m.kind == kindNotification {
		x.w.WriteHeader(http.StatusAccepted)
		return
	}
	if !x.standardHeaders(m, revision) {
		return
	}
	x.answer(x.modernResult(m), m.id)
}

// standardHeaders refuses a modern request without MCP-Protocol-Version or Mcp-Method, or whose
// Mcp-Name (for tools/call) is missing, undecodable or not params.name.
func (x *exchange) standardHeaders(m message, revision string) bool {
	if _, ok := x.header("MCP-Protocol-Version"); !ok {
		x.reject(http.StatusBadRequest, m.echo, mismatch("(missing)", "the body envelope names protocol version "+revision+" but the required MCP-Protocol-Version header is absent"))
		return false
	}
	if _, ok := x.header("Mcp-Method"); !ok {
		x.reject(http.StatusBadRequest, m.echo, mismatch("(missing)", "the body names method "+m.method+" but the required Mcp-Method header is absent"))
		return false
	}
	if m.method != "tools/call" {
		return true
	}
	name, hasName := m.nameOf()
	header, present := x.header("Mcp-Name")
	if !present {
		if hasName {
			x.reject(http.StatusBadRequest, m.echo, mismatch("(missing)", `the body carries params.name="`+name+`" but the required Mcp-Name header is absent`))
			return false
		}
		return true
	}
	decoded, ok := decodeMcpParam(header)
	if !ok {
		x.reject(http.StatusBadRequest, m.echo, mismatch(header, "the Mcp-Name header carries an invalid Base64 sentinel value"))
		return false
	}
	if hasName && decoded != name {
		x.reject(http.StatusBadRequest, m.echo, mismatch(header, `the body carries params.name="`+name+`" but the Mcp-Name header names "`+decoded+`"`))
		return false
	}
	return true
}

// legacy is the legacy transport for one message: Accept, then (except for initialize) the
// MCP-Protocol-Version header; a notification or a response is accepted, a request answered.
func (x *exchange) legacy(m message) {
	if !x.acceptable() {
		return
	}
	if (m.kind != kindRequest || m.method != "initialize") && !x.classicHeaderOK() {
		return
	}
	if m.kind != kindRequest {
		x.w.WriteHeader(http.StatusAccepted)
		return
	}
	x.answer(x.classic(m), m.id)
}

// classicHeaderOK is the legacy transport's validateProtocolVersion: the header, when sent, must
// name a classic revision; otherwise the 400 is written and false returned.
func (x *exchange) classicHeaderOK() bool {
	if x.headerVersion == "" || slices.Contains(headerRevisions, x.headerVersion) {
		return true
	}
	writeTransportError(x.w, http.StatusBadRequest, codeServerError, "Bad Request: Unsupported protocol version: "+x.headerVersion+
		" (supported versions: "+strings.Join(headerRevisions, ", ")+")")
	return false
}

// batch serves a JSON-RPC array. classifyBatch judges the elements in order (a modern claim, then
// the shape); the cap of maxBatch messages is counted while the elements are read, so a larger
// array is refused after reading at most maxBatch+1 of them, never decoded whole. Then the legacy
// transport: Accept, the cap, one initialize alone, the header (unless it is initialize), and the
// requests answered in order (nothing for notifications and responses).
func (x *exchange) batch(raw []byte) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	_, _ = dec.Token() // the '[' of valid JSON
	msgs := make([]message, 0, 8)
	overCap := false
	for dec.More() {
		if len(msgs) == maxBatch {
			overCap = true
			break
		}
		var el json.RawMessage
		if dec.Decode(&el) != nil {
			x.reject(http.StatusBadRequest, nil, rpcErr{Code: codeInvalidRequest, Message: msgInvalidInBatch})
			return
		}
		m := parseMessage(el)
		if m.hasClaim() {
			x.reject(http.StatusBadRequest, nil, rpcErr{Code: codeInvalidRequest, Message: msgModernInBatch})
			return
		}
		if m.kind == kindInvalid {
			x.reject(http.StatusBadRequest, nil, rpcErr{Code: codeInvalidRequest, Message: msgInvalidInBatch})
			return
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		x.reject(http.StatusBadRequest, nil, rpcErr{Code: codeInvalidRequest, Message: msgEmptyBatch})
		return
	}
	if !x.acceptable() {
		return
	}
	if overCap {
		writeTransportError(x.w, http.StatusBadRequest, codeInvalidRequest, msgBatchTooLarge)
		return
	}
	initialize, requests := false, 0
	for _, m := range msgs {
		initialize = initialize || (m.kind == kindRequest && m.method == "initialize")
		if m.kind == kindRequest {
			requests++
		}
	}
	if initialize && len(msgs) > 1 {
		writeTransportError(x.w, http.StatusBadRequest, codeInvalidRequest, msgOneInitialize)
		return
	}
	if !initialize && !x.classicHeaderOK() {
		return
	}
	if requests == 0 {
		x.w.WriteHeader(http.StatusAccepted)
		return
	}
	// The answers are written once, into the one buffer that becomes the body.
	var b bytes.Buffer
	b.WriteByte('[')
	first := true
	for _, m := range msgs {
		if m.kind != kindRequest {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		writeAnswer(&b, x.classic(m), m.id)
	}
	b.WriteByte(']')
	writeJSON(x.w, http.StatusOK, b.Bytes())
}

// outcome is the result object (raw JSON) or the error of one request.
type outcome struct {
	result []byte
	err    *rpcErr
}

func (x *exchange) answer(o outcome, id json.RawMessage) {
	var b bytes.Buffer
	writeAnswer(&b, o, id)
	writeJSON(x.w, http.StatusOK, b.Bytes())
}

// writeAnswer writes the response to a request: its result or its error, with its id.
func writeAnswer(b *bytes.Buffer, o outcome, id json.RawMessage) {
	if o.err != nil {
		b.Write(errorBody(id, *o.err))
		return
	}
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(id)
	b.WriteString(`,"result":`)
	b.Write(o.result)
	b.WriteByte('}')
}

func errorBody(id json.RawMessage, e rpcErr) []byte {
	errJSON, _ := marshalJS(e) // ints, strings and string slices always encode
	idJSON := []byte("null")
	if len(id) > 0 {
		idJSON = id
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
func (x *exchange) classic(m message) outcome {
	switch m.method {
	case "initialize":
		var p struct {
			ProtocolVersion any `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.params, &p)
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

// modernResult dispatches a request of the 2026-07-28 revision; every result carries resultType and the
// server info in _meta.
func (x *exchange) modernResult(m message) outcome {
	method := m.method
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
