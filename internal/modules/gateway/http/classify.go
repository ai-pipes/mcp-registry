package http

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The shapes of JSON-RPC messages and the envelope of the 2026-07-28 revision, as the reference's
// SDK (@modelcontextprotocol/server 2.4.0) judges them: JSONRPCRequestSchema and friends are
// strict objects, an id is a string or an integer, params (when present) an object whose _meta
// (when present) is an object.

type msgKind int

const (
	kindInvalid msgKind = iota
	kindRequest
	kindNotification
	kindResponse
)

const (
	clientCapabilitiesKey = "io.modelcontextprotocol/clientCapabilities"
	clientInfoKey         = "io.modelcontextprotocol/clientInfo"
)

// message is one JSON-RPC message of a POST.
type message struct {
	kind   msgKind
	id     json.RawMessage // the raw id; nil when absent
	method string
	params json.RawMessage // nil when absent
	meta   map[string]json.RawMessage
	// echo is the id the reference echoes in a refusal (echoableRequestId): the id of an object
	// whose method is a string and whose id is a string or a number; "null" otherwise.
	echo json.RawMessage
}

var nullID = json.RawMessage("null")

// parseMessage judges one JSON value (valid JSON) as a JSON-RPC message.
func parseMessage(raw json.RawMessage) message {
	m := message{echo: nullID}
	var obj map[string]json.RawMessage
	if kindOf(raw) != "object" || json.Unmarshal(raw, &obj) != nil {
		return m
	}
	id, hasID := obj["id"]
	methodRaw, hasMethod := obj["method"]
	params, hasParams := obj["params"]
	if hasID {
		m.id = bytes.TrimSpace(id)
	}
	if hasMethod && kindOf(methodRaw) == "string" {
		_ = json.Unmarshal(methodRaw, &m.method)
		if k := kindOf(id); hasID && (k == "string" || k == "number") {
			m.echo = m.id
		}
	}
	if !jsonrpcVersion(obj["jsonrpc"]) {
		return m
	}
	switch {
	case hasMethod:
		if kindOf(methodRaw) != "string" || (hasID && !requestID(m.id)) || !onlyKeys(obj, "jsonrpc", "id", "method", "params") {
			return m
		}
		if hasParams {
			if kindOf(params) != "object" {
				return m
			}
			m.params = bytes.TrimSpace(params)
			var p map[string]json.RawMessage
			_ = json.Unmarshal(params, &p)
			if metaRaw, ok := p["_meta"]; ok {
				if kindOf(metaRaw) != "object" {
					return m
				}
				_ = json.Unmarshal(metaRaw, &m.meta)
			}
		}
		m.kind = kindNotification
		if hasID {
			m.kind = kindRequest
		}
	case hasID && obj["result"] != nil:
		if requestID(m.id) && kindOf(obj["result"]) == "object" && onlyKeys(obj, "jsonrpc", "id", "result") {
			m.kind = kindResponse
		}
	case obj["error"] != nil:
		if (!hasID || requestID(m.id)) && errorObject(obj["error"]) && onlyKeys(obj, "jsonrpc", "id", "error") {
			m.kind = kindResponse
		}
	}
	return m
}

func jsonrpcVersion(raw json.RawMessage) bool {
	var v string
	return kindOf(raw) == "string" && json.Unmarshal(raw, &v) == nil && v == "2.0"
}

// requestID is z.union([z.string(), z.number().int()]).
func requestID(raw json.RawMessage) bool {
	switch kindOf(raw) {
	case "string":
		return true
	case "number":
		f, err := strconv.ParseFloat(string(raw), 64)
		return err == nil && f == math.Trunc(f)
	}
	return false
}

func onlyKeys(obj map[string]json.RawMessage, allowed ...string) bool {
	for k := range obj {
		ok := false
		for _, a := range allowed {
			ok = ok || k == a
		}
		if !ok {
			return false
		}
	}
	return true
}

func errorObject(raw json.RawMessage) bool {
	var e map[string]json.RawMessage
	if kindOf(raw) != "object" || json.Unmarshal(raw, &e) != nil || kindOf(e["message"]) != "string" || !requestID(e["code"]) || kindOf(e["code"]) != "number" {
		return false
	}
	return onlyKeys(e, "code", "message", "data")
}

// hasClaim reports the envelope claim (the protocol version key in params._meta, any value).
func (m message) hasClaim() bool {
	_, ok := m.meta[claimKey]
	return ok
}

// claimVersion is the claimed revision when the claim is a string.
func (m message) claimVersion() (string, bool) {
	raw, ok := m.meta[claimKey]
	if !ok || kindOf(raw) != "string" {
		return "", false
	}
	var v string
	return v, json.Unmarshal(raw, &v) == nil
}

// envelopeIssue is one problem of a 2026-07-28 envelope: which key, what problem.
type envelopeIssue struct {
	Key     string `json:"key"`
	Problem string `json:"problem"`
}

// envelopeIssues is validateEnvelopeMeta: the missing required keys first, then the type problems
// of the keys that are present (the zod schema's key order).
func envelopeIssues(meta map[string]json.RawMessage) []envelopeIssue {
	var out []envelopeIssue
	missing := map[string]bool{}
	for _, k := range []string{claimKey, clientCapabilitiesKey} {
		if _, ok := meta[k]; !ok {
			out = append(out, envelopeIssue{k, "missing"})
			missing[k] = true
		}
	}
	expect := func(key, want string) {
		raw, ok := meta[key]
		if !ok || missing[key] {
			return
		}
		if got := kindOf(raw); got != want {
			out = append(out, envelopeIssue{key, "Invalid input: expected " + want + ", received " + got})
		}
	}
	expect(claimKey, "string")
	if raw, ok := meta[clientInfoKey]; ok {
		var info map[string]json.RawMessage
		if kindOf(raw) != "object" || json.Unmarshal(raw, &info) != nil {
			out = append(out, envelopeIssue{clientInfoKey, "Invalid input: expected object, received " + kindOf(raw)})
		} else {
			for _, f := range []string{"name", "version"} {
				if k := kindOf(info[f]); k != "string" {
					out = append(out, envelopeIssue{clientInfoKey + "." + f, "Invalid input: expected string, received " + k})
				}
			}
		}
	}
	expect(clientCapabilitiesKey, "object")
	return out
}

// validModernClaim is carriesValidModernEnvelopeClaim: a claim naming a modern revision in a valid
// envelope.
func (m message) validModernClaim() bool {
	v, ok := m.claimVersion()
	return ok && isModern(v) && len(envelopeIssues(m.meta)) == 0
}

// isModern is isModernProtocolVersion: the 2026-07-28 revision or a later one (string order).
func isModern(version string) bool { return version >= modernRevision }

// nameOf is the params.name of a tools/call (the source of the Mcp-Name header), ok false when it
// is not a string.
func (m message) nameOf() (string, bool) {
	var p map[string]json.RawMessage
	if json.Unmarshal(m.params, &p) != nil || kindOf(p["name"]) != "string" {
		return "", false
	}
	var name string
	return name, json.Unmarshal(p["name"], &name) == nil
}

var base64Canonical = regexp.MustCompile(`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)

// decodeMcpParam is decodeMcpParamValue: a value in the =?base64?...?= sentinel is decoded (ok
// false when it is not canonical Base64 of UTF-8), any other value is itself.
func decodeMcpParam(value string) (string, bool) {
	const prefix, suffix = "=?base64?", "?="
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, suffix) || len(value) < len(prefix)+len(suffix) {
		return value, true
	}
	b64 := value[len(prefix) : len(value)-len(suffix)]
	if !base64Canonical.MatchString(b64) {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}
