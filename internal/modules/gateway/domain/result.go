package domain

import (
	"bytes"
	"encoding/json"
	"errors"
)

// ToolResult is what the agent receives from call_tool: the upstream's content, its structured
// content (only an object) and its isError (only when it sent one). Nothing else of the upstream
// result (a top-level _meta, resultType, any other field) is passed on.
type ToolResult struct {
	// Content is the JSON array of content blocks, byte for byte as the upstream sent it.
	Content json.RawMessage
	// StructuredContent is the JSON object, or nil when absent or not an object.
	StructuredContent json.RawMessage
	// IsError is nil when the upstream sent no isError.
	IsError *bool
}

// ErrMalformedResult covers: the upstream result is not a CallToolResult (not an object, content not an
// array, isError not a boolean). The caller treats it as a transport failure.
var ErrMalformedResult = errors.New("upstream result is malformed")

// ProjectUpstreamResult keeps content, an object structuredContent and a defined isError of a
// raw upstream result. An absent content is the empty list (the MCP schema's default); with
// duplicate keys the last wins.
func ProjectUpstreamResult(raw []byte) (ToolResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ToolResult{}, ErrMalformedResult
	}
	res := ToolResult{Content: json.RawMessage("[]")}
	if c, ok := fields["content"]; ok {
		if !isKind(c, '[') {
			return ToolResult{}, ErrMalformedResult
		}
		res.Content = c
	}
	if s, ok := fields["structuredContent"]; ok && isKind(s, '{') {
		res.StructuredContent = s
	}
	if e, ok := fields["isError"]; ok {
		var b bool
		if err := json.Unmarshal(e, &b); err != nil || bytes.Equal(bytes.TrimSpace(e), []byte("null")) {
			return ToolResult{}, ErrMalformedResult
		}
		res.IsError = &b
	}
	return res, nil
}

func isKind(v json.RawMessage, first byte) bool {
	v = bytes.TrimSpace(v)
	return len(v) > 0 && v[0] == first
}

// JSON is the result as the JSON object the agent receives: keys content, structuredContent
// (when present), isError (when present). The raw parts are copied as they are, without the
// HTML escaping json.Marshal would add (it also would for a json.RawMessage inside a
// Marshaler, so this is not a MarshalJSON).
func (r ToolResult) JSON() []byte {
	var b bytes.Buffer
	b.WriteString(`{"content":`)
	b.Write(r.Content)
	if r.StructuredContent != nil {
		b.WriteString(`,"structuredContent":`)
		b.Write(r.StructuredContent)
	}
	if r.IsError != nil {
		if *r.IsError {
			b.WriteString(`,"isError":true`)
		} else {
			b.WriteString(`,"isError":false`)
		}
	}
	b.WriteByte('}')
	return b.Bytes()
}

// FailureResult is the answer for a failed call: exactly one text block and isError: true.
func FailureResult(text string) ToolResult {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{"text", text}) // a struct of strings always encodes
	block := bytes.TrimRight(b.Bytes(), "\n")
	t := true
	return ToolResult{Content: append(append([]byte("["), block...), ']'), IsError: &t}
}
