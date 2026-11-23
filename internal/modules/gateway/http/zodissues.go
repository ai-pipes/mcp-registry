package http

import (
	"bytes"
	"encoding/json"
)

// The protocol-level parameter checks the reference's SDK makes with zod after the message shape:
// initialize (InitializeRequestSchema: the in-band error -32603 whose message is the zod issues)
// and tools/call (CallToolRequestSchema: -32602 "Invalid tools/call request: <issues>"). The
// message is JSON.stringify(issues, null, 2) of zod's invalid_type issues.

// zodIssue is one zod 4 invalid_type issue, in its key order.
type zodIssue struct {
	Expected string `json:"expected"`
	Code     string `json:"code"`
	Path     []any  `json:"path"`
	Message  string `json:"message"`
}

type zodIssues []zodIssue

// want records an issue when the value at path (raw, nil when absent) is not of the expected kind;
// a missing optional value is fine. It reports whether the value is of the kind.
func (z *zodIssues) want(path []any, raw json.RawMessage, expected string, optional bool) bool {
	got := kindOf(raw)
	if got == "undefined" && optional {
		return false
	}
	ok := got == expected || (expected == "record" && got == "object")
	if !ok {
		*z = append(*z, zodIssue{Expected: expected, Code: "invalid_type", Path: path, Message: invalidType(expected, got)})
	}
	return ok
}

// text is JSON.stringify(issues, null, 2).
func (z zodIssues) text() string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode([]zodIssue(z)) // strings and paths always encode
	return string(rawLineSeparators(bytes.TrimRight(b.Bytes(), "\n")))
}

func path(keys ...any) []any { return keys }

// objectAt decodes an object value (the caller checked its kind).
func objectAt(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return m
}

// initializeIssues is InitializeRequestParamsSchema for the params of an initialize request
// (nil params: absent). Empty means it is an initialization request (isInitializeRequest).
func initializeIssues(params map[string]json.RawMessage, present bool) zodIssues {
	var z zodIssues
	if !present {
		z.want(path("params"), nil, "object", false)
		return z
	}
	z.want(path("params", "protocolVersion"), params["protocolVersion"], "string", false)
	if z.want(path("params", "capabilities"), params["capabilities"], "object", false) {
		caps := objectAt(params["capabilities"])
		for _, k := range []string{"experimental", "sampling", "elicitation", "roots", "tasks", "extensions"} {
			expected := "object"
			if k == "experimental" || k == "extensions" {
				expected = "record"
			}
			if z.want(path("params", "capabilities", k), caps[k], expected, true) && k == "roots" {
				z.want(path("params", "capabilities", "roots", "listChanged"), objectAt(caps[k])["listChanged"], "boolean", true)
			}
		}
	}
	if z.want(path("params", "clientInfo"), params["clientInfo"], "object", false) {
		info := objectAt(params["clientInfo"])
		z.want(path("params", "clientInfo", "name"), info["name"], "string", false)
		z.want(path("params", "clientInfo", "title"), info["title"], "string", true)
		if z.want(path("params", "clientInfo", "icons"), info["icons"], "array", true) {
			var icons []json.RawMessage
			_ = json.Unmarshal(info["icons"], &icons)
			for i, icon := range icons {
				if z.want(path("params", "clientInfo", "icons", i), icon, "object", false) {
					z.want(path("params", "clientInfo", "icons", i, "src"), objectAt(icon)["src"], "string", false)
				}
			}
		}
		z.want(path("params", "clientInfo", "version"), info["version"], "string", false)
		z.want(path("params", "clientInfo", "websiteUrl"), info["websiteUrl"], "string", true)
		z.want(path("params", "clientInfo", "description"), info["description"], "string", true)
	}
	return z
}

// callParamsIssues is CallToolRequestSchema for the params of tools/call: params an object, name
// a string, arguments (when present) a record.
func callParamsIssues(params map[string]json.RawMessage, present bool) zodIssues {
	var z zodIssues
	if !present {
		z.want(path("params"), nil, "object", false)
		return z
	}
	z.want(path("params", "name"), params["name"], "string", false)
	z.want(path("params", "arguments"), params["arguments"], "record", true)
	return z
}
