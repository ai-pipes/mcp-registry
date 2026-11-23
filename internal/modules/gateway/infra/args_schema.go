package infra

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// dialect is the JSON Schema dialect a tool schema is compiled as.
type dialect struct {
	draft *jsonschema.Draft
	// meta is the canonical $schema written into the compiled copy, so that the library never
	// meets a spelling it does not know (no trailing #, the http twin, a non-string value).
	meta string
	// refSiblings is true for the drafts in which a library ignores the keywords next to $ref
	// (draft-07 and older): the reference's validator evaluates them in every dialect.
	refSiblings bool
}

var (
	dialect202012 = dialect{jsonschema.Draft2020, "https://json-schema.org/draft/2020-12/schema", false}
	dialect201909 = dialect{jsonschema.Draft2019, "https://json-schema.org/draft/2019-09/schema", false}
	// draft-06 is compiled as draft-07, as the reference does (one engine for both).
	dialectDraft7 = dialect{jsonschema.Draft7, "http://json-schema.org/draft-07/schema#", true}
)

// maxDialectEcho is how much of an unsupported $schema value an error text quotes.
const maxDialectEcho = 200

// dialectOf is the reference's declaredDialect: absent or not a string is 2020-12, one trailing #
// is ignored, http and https are the same, anything else is unsupported.
func dialectOf(schema map[string]any) (dialect, error) {
	declared, isString := schema["$schema"].(string)
	if !isString {
		return dialect202012, nil
	}
	switch strings.TrimSuffix(declared, "#") {
	case "https://json-schema.org/draft/2020-12/schema", "http://json-schema.org/draft/2020-12/schema":
		return dialect202012, nil
	case "https://json-schema.org/draft/2019-09/schema", "http://json-schema.org/draft/2019-09/schema":
		return dialect201909, nil
	case "https://json-schema.org/draft-07/schema", "http://json-schema.org/draft-07/schema",
		"https://json-schema.org/draft-06/schema", "http://json-schema.org/draft-06/schema":
		return dialectDraft7, nil
	}
	shown := declared
	if r := []rune(shown); len(r) > maxDialectEcho {
		shown = string(r[:maxDialectEcho])
	}
	return dialect{}, fmt.Errorf("JSON Schema declares an unsupported dialect (%q); supported: 2020-12, 2019-09, draft-07, draft-06", shown)
}

// Keys whose value is a schema, a list of schemas, or a map of schemas (the reference's
// ONE_SCHEMA, SCHEMA_LIST and SCHEMA_MAP). Only these positions are walked.
var (
	oneSchemaKeys = []string{
		"additionalProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems",
		"items", "contains", "propertyNames", "not", "if", "then", "else", "contentSchema",
	}
	schemaListKeys = []string{"allOf", "anyOf", "oneOf", "prefixItems", "items"}
	schemaMapKeys  = []string{"properties", "$defs", "definitions", "dependentSchemas", "dependencies"}
)

// prepare decodes a tool's input schema (numbers exact) and returns the copy to compile: patterns
// are removed (ReDoS), the top-level $id is removed (two tools may share one), $schema is the
// canonical form of the chosen dialect, and for draft-07 a $ref's siblings are kept evaluated. The
// input bytes are never touched.
func prepare(raw json.RawMessage) (any, *dialect, error) {
	if len(raw) > maxSchemaBytes {
		return nil, nil, fixedError("the schema is too large to be checked")
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, nil, errors.New("the schema is not valid JSON")
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, nil, errors.New("the schema is not an object")
	}
	if hasExcessNumber(doc) {
		return nil, nil, fixedError("the schema has a number too large or too precise to be checked")
	}
	if depthOf(doc, 0) > maxSchemaDepth {
		return nil, nil, fixedError("the schema is nested too deeply to be checked")
	}
	d, err := dialectOf(root)
	if err != nil {
		return nil, nil, err
	}
	walk(root, d)
	delete(root, "$id")
	root["$schema"] = d.meta
	return root, &d, nil
}

func walk(node any, d dialect) {
	obj, ok := node.(map[string]any)
	if !ok {
		return
	}
	delete(obj, "pattern")
	if f, isString := obj["format"].(string); isString && f == "regex" {
		// The reference compiles the value with a JavaScript RegExp; Go's syntax would refuse
		// what that accepts (lookahead, backreferences), so the format is not checked.
		delete(obj, "format")
	}
	if _, has := obj["patternProperties"]; has {
		// The properties the pattern would have allowed must not become additional ones.
		delete(obj, "patternProperties")
		delete(obj, "additionalProperties")
		delete(obj, "unevaluatedProperties")
	}
	if d.refSiblings {
		liftRefSiblings(obj)
	}
	for _, k := range oneSchemaKeys {
		if child, ok := obj[k].(map[string]any); ok {
			walk(child, d)
		}
	}
	for _, k := range schemaListKeys {
		if list, ok := obj[k].([]any); ok {
			for _, child := range list {
				walk(child, d)
			}
		}
	}
	for _, k := range schemaMapKeys {
		if m, ok := obj[k].(map[string]any); ok {
			for _, child := range m {
				walk(child, d)
			}
		}
	}
}

// liftRefSiblings turns {"$ref": X, ...rest} into {...rest, "allOf": [..., {"$ref": X}]}: the
// library follows draft-07 and ignores the keywords next to a $ref, the reference does not.
func liftRefSiblings(obj map[string]any) {
	ref, has := obj["$ref"].(string)
	if !has || len(obj) == 1 {
		return
	}
	delete(obj, "$ref")
	lifted := map[string]any{"$ref": ref}
	if list, ok := obj["allOf"].([]any); ok {
		obj["allOf"] = append(list, lifted)
		return
	}
	obj["allOf"] = []any{lifted}
}

// depthOf is the nesting depth of objects and arrays in a decoded JSON document, stopping early once it is past the bound.
func depthOf(node any, depth int) int {
	if depth > maxSchemaDepth {
		return depth
	}
	deepest := depth
	switch n := node.(type) {
	case map[string]any:
		deepest++
		for _, child := range n {
			deepest = max(deepest, depthOf(child, depth+1))
		}
	case []any:
		deepest++
		for _, child := range n {
			deepest = max(deepest, depthOf(child, depth+1))
		}
	}
	return deepest
}

// The bounds of a number literal, in a schema and in the arguments: at most maxNumberChars
// characters and a decimal exponent of at most maxNumberExponent in magnitude. A double reaches
// 1e308, so no real tool or call comes near them; a literal such as 1e9999999 is a few bytes that
// the exact (big.Rat) arithmetic would turn into megabytes and seconds, or a panic.
const (
	maxNumberChars    = 400
	maxNumberExponent = 400
)

// numberTooBig reports whether a JSON number literal is beyond the bounds above.
func numberTooBig(literal string) bool {
	if len(literal) > maxNumberChars {
		return true
	}
	i := strings.IndexAny(literal, "eE")
	if i < 0 {
		return false
	}
	exp, err := strconv.Atoi(strings.TrimPrefix(literal[i+1:], "+"))
	return err != nil || exp > maxNumberExponent || exp < -maxNumberExponent
}

// hasExcessNumber reports whether any number in a decoded document is beyond the bounds. It reads
// the literals only, never turning them into values.
func hasExcessNumber(node any) bool {
	switch n := node.(type) {
	case json.Number:
		return numberTooBig(string(n))
	case map[string]any:
		for _, child := range n {
			if hasExcessNumber(child) {
				return true
			}
		}
	case []any:
		for _, child := range n {
			if hasExcessNumber(child) {
				return true
			}
		}
	}
	return false
}

// rawHasExcessNumber is hasExcessNumber on JSON text: it finds the number literals outside strings
// and judges each, without decoding the document.
func rawHasExcessNumber(doc []byte) bool {
	for i := 0; i < len(doc); {
		c := doc[i]
		switch {
		case c == '"':
			for i++; i < len(doc) && doc[i] != '"'; i++ {
				if doc[i] == '\\' {
					i++
				}
			}
			i++
		case c == '-' || (c >= '0' && c <= '9'):
			start := i
			for i < len(doc) && strings.IndexByte("0123456789+-.eE", doc[i]) >= 0 {
				i++
			}
			if numberTooBig(string(doc[start:i])) {
				return true
			}
		default:
			i++
		}
	}
	return false
}
