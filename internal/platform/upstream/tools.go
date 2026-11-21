package upstream

import (
	"encoding/json"
	"errors"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

var errMalformed = errors.New("malformed answer")

// wireTool is a tool as sent. Every field is kept as raw JSON, copied once from the page: absent is
// nil, present is the bytes as sent ("null" included).
type wireTool struct {
	Name         json.RawMessage `json:"name"`
	Title        json.RawMessage `json:"title"`
	Description  json.RawMessage `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Annotations  json.RawMessage `json:"annotations"`
}

// decodePage reads one tools/list result in a single pass: the tools and the next cursor (nil when
// the list ends). It checks what the TypeScript client's zod schemas check and keeps the raw JSON
// of the schemas.
func decodePage(result json.RawMessage) ([]kernel.UpstreamTool, *string, error) {
	var page struct {
		Tools      []wireTool      `json:"tools"`
		NextCursor json.RawMessage `json:"nextCursor"`
	}
	if err := json.Unmarshal(result, &page); err != nil || page.Tools == nil {
		return nil, nil, errMalformed
	}
	var cursor *string
	if len(page.NextCursor) > 0 && string(page.NextCursor) != "null" {
		c, ok := jsonString(page.NextCursor)
		if !ok {
			return nil, nil, errMalformed
		}
		cursor = &c
	}
	tools := make([]kernel.UpstreamTool, 0, len(page.Tools))
	for _, w := range page.Tools {
		tool, err := w.tool()
		if err != nil {
			return nil, nil, err
		}
		tools = append(tools, tool)
	}
	return tools, cursor, nil
}

func (w wireTool) tool() (kernel.UpstreamTool, error) {
	var t kernel.UpstreamTool
	name, ok := jsonString(w.Name)
	if !ok {
		return t, errMalformed
	}
	t.Name = name
	var err error
	if t.Title, err = optionalString(w.Title); err != nil {
		return t, err
	}
	if t.Description, err = optionalString(w.Description); err != nil {
		return t, err
	}
	if !validInputSchema(w.InputSchema) {
		return t, errMalformed
	}
	t.InputSchema = w.InputSchema
	if w.OutputSchema != nil {
		if !validOutputSchema(w.OutputSchema) {
			return t, errMalformed
		}
		t.OutputSchema = w.OutputSchema
	}
	if w.Annotations != nil {
		if t.Annotations, err = knownAnnotations(w.Annotations); err != nil {
			return t, err
		}
	}
	return t, nil
}

func optionalString(raw json.RawMessage) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	s, ok := jsonString(raw)
	if !ok {
		return nil, errMalformed
	}
	return &s, nil
}

// jsonString decodes a JSON string; null, absent and every other type are not one.
func jsonString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func isJSONBool(raw json.RawMessage) bool {
	return string(raw) == "true" || string(raw) == "false"
}

// shape records whether a JSON value is present and whether it is an object, without copying it
// (UnmarshalJSON is handed a slice of the page, which is all a check needs).
type shape struct{ present, object bool }

func (s *shape) UnmarshalJSON(b []byte) error {
	s.present, s.object = true, len(b) > 0 && b[0] == '{'
	return nil
}

// validInputSchema: an object of "type": "object" whose properties, when present, form an object
// and whose required, when present, is a list of strings (the TypeScript tool schema).
func validInputSchema(raw json.RawMessage) bool {
	var s struct {
		Type       json.RawMessage `json:"type"`
		Properties shape           `json:"properties"`
		Required   json.RawMessage `json:"required"`
	}
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &s) != nil {
		return false
	}
	if typ, ok := jsonString(s.Type); !ok || typ != "object" {
		return false
	}
	if s.Properties.present && !s.Properties.object {
		return false
	}
	if s.Required != nil {
		var req []string
		if json.Unmarshal(s.Required, &req) != nil || req == nil {
			return false
		}
	}
	return true
}

// validOutputSchema: an object whose $schema, when present, is a string.
func validOutputSchema(raw json.RawMessage) bool {
	var s struct {
		Schema json.RawMessage `json:"$schema"`
	}
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &s) != nil {
		return false
	}
	if s.Schema != nil {
		if _, ok := jsonString(s.Schema); !ok {
			return false
		}
	}
	return true
}

// knownAnnotations keeps what the TypeScript client keeps of a tool's annotations: its strict zod
// object has title and four boolean hints, any other key is dropped, a value of the wrong type
// fails the whole answer. The values are copied as sent; a hint sent as false stays false, one not
// sent stays absent.
func knownAnnotations(raw json.RawMessage) (json.RawMessage, error) {
	var a map[string]json.RawMessage
	if json.Unmarshal(raw, &a) != nil || a == nil {
		return nil, errMalformed
	}
	out := []byte{'{'}
	for _, key := range []string{"title", "readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
		v, ok := a[key]
		if !ok {
			continue
		}
		if _, isString := jsonString(v); (key == "title" && !isString) || (key != "title" && !isJSONBool(v)) {
			return nil, errMalformed
		}
		if len(out) > 1 {
			out = append(out, ',')
		}
		k, _ := json.Marshal(key)
		out = append(out, k...)
		out = append(out, ':')
		out = append(out, v...)
	}
	return append(out, '}'), nil
}
