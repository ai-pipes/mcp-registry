package upstream

import (
	"encoding/json"
	"errors"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

var errMalformed = errors.New("malformed answer")

// decodePage reads one tools/list result: the tools and the next cursor ("" when the list ends).
// It checks what the TypeScript client's zod schemas check and keeps the raw JSON of the schemas.
func decodePage(result json.RawMessage) ([]kernel.UpstreamTool, string, error) {
	var page struct {
		Tools      []json.RawMessage `json:"tools"`
		NextCursor json.RawMessage   `json:"nextCursor"`
	}
	if err := json.Unmarshal(result, &page); err != nil || page.Tools == nil {
		return nil, "", errMalformed
	}
	cursor := ""
	if len(page.NextCursor) > 0 && string(page.NextCursor) != "null" {
		c, ok := jsonString(page.NextCursor)
		if !ok {
			return nil, "", errMalformed
		}
		cursor = c
	}
	tools := make([]kernel.UpstreamTool, 0, len(page.Tools))
	for _, raw := range page.Tools {
		tool, err := decodeTool(raw)
		if err != nil {
			return nil, "", err
		}
		tools = append(tools, tool)
	}
	return tools, cursor, nil
}

func decodeTool(raw json.RawMessage) (kernel.UpstreamTool, error) {
	var f map[string]json.RawMessage
	if err := json.Unmarshal(raw, &f); err != nil || f == nil {
		return kernel.UpstreamTool{}, errMalformed
	}
	var t kernel.UpstreamTool
	name, ok := jsonString(f["name"])
	if !ok {
		return t, errMalformed
	}
	t.Name = name
	var err error
	if t.Title, err = optionalString(f, "title"); err != nil {
		return t, err
	}
	if t.Description, err = optionalString(f, "description"); err != nil {
		return t, err
	}
	in, ok := f["inputSchema"]
	if !ok || !validInputSchema(in) {
		return t, errMalformed
	}
	t.InputSchema = in
	if out, ok := f["outputSchema"]; ok {
		if !validOutputSchema(out) {
			return t, errMalformed
		}
		t.OutputSchema = out
	}
	if a, ok := f["annotations"]; ok {
		if t.Annotations, err = knownAnnotations(a); err != nil {
			return t, err
		}
	}
	return t, nil
}

func optionalString(f map[string]json.RawMessage, key string) (*string, error) {
	raw, ok := f[key]
	if !ok {
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

// validInputSchema: an object of "type": "object" whose properties, when present, form an object
// and whose required, when present, is a list of strings (the TypeScript tool schema).
func validInputSchema(raw json.RawMessage) bool {
	var s map[string]json.RawMessage
	if json.Unmarshal(raw, &s) != nil || s == nil {
		return false
	}
	if typ, ok := jsonString(s["type"]); !ok || typ != "object" {
		return false
	}
	if p, ok := s["properties"]; ok {
		var props map[string]json.RawMessage
		if json.Unmarshal(p, &props) != nil || props == nil {
			return false
		}
	}
	if r, ok := s["required"]; ok {
		var req []string
		if json.Unmarshal(r, &req) != nil || req == nil {
			return false
		}
	}
	return true
}

// validOutputSchema: an object whose $schema, when present, is a string.
func validOutputSchema(raw json.RawMessage) bool {
	var s map[string]json.RawMessage
	if json.Unmarshal(raw, &s) != nil || s == nil {
		return false
	}
	if d, ok := s["$schema"]; ok {
		if _, ok := jsonString(d); !ok {
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
