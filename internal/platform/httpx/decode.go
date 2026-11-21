package httpx

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// DecodeJSON reads the request body into dst with encoding/json/v2: member names match the
// struct tags case-sensitively (like the TypeScript server: {"NAME":1} does not fill `name`),
// and unknown members are dropped silently. An empty or whitespace-only body counts as `{}`; a
// body of `null`, trailing data after the first value and duplicate member names are errors.
// Failures are DomainErrors: invalid_request "body is not valid JSON", invalid_request
// "<field>: invalid type" for a value of the wrong type (no value echoed), invalid_request "body
// must not be null", and limit_exceeded when BodyLimit cut the body (so WriteDomainError answers
// 413). A handler that reads the body in another way gets the same cut as an error wrapping
// *http.MaxBytesError and passes it to WriteDomainError.
//
// Because names match case-sensitively against the tag, a field WITHOUT a json tag would silently
// never be filled (json/v2 would look for "Role", clients send "role"). DecodeJSON therefore
// refuses a destination type that has an exported field without a `json` tag, in any nested
// struct, slice, array, map value or pointer, by panicking with the type and field: a programming
// error that the first test of the handler hits. Tag `json:"-"` to skip a field on purpose. Types
// that decode themselves (json or text unmarshalers, such as time.Time) are not looked into.
func DecodeJSON(r *http.Request, dst any) error {
	requireJSONTags(reflect.TypeOf(dst))
	var raw []byte
	if r.Body != nil {
		var err error
		raw, err = io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return kernel.NewDomainError("limit_exceeded", errBodyTooLarge)
			}
			return InvalidRequest("body could not be read")
		}
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if string(raw) == "null" {
		return InvalidRequest("body must not be null")
	}
	if err := jsonv2.Unmarshal(raw, dst); err != nil {
		var sem *jsonv2.SemanticError
		if errors.As(err, &sem) && sem.JSONKind != 0 && sem.GoType != nil {
			if field := pointerPath(sem.JSONPointer); field != "" {
				return InvalidRequest(field + ": invalid type")
			}
			return InvalidRequest("invalid type")
		}
		return InvalidRequest("body is not valid JSON")
	}
	return nil
}

// pointerPath renders a JSON Pointer ("/a/b") as a dotted path ("a.b").
func pointerPath(p jsontext.Pointer) string {
	return strings.ReplaceAll(strings.TrimPrefix(string(p), "/"), "/", ".")
}

// taggedTypes caches the types that passed requireJSONTags.
var taggedTypes sync.Map

var (
	jsonUnmarshaler = reflect.TypeFor[interface{ UnmarshalJSON([]byte) error }]()
	textUnmarshaler = reflect.TypeFor[interface{ UnmarshalText([]byte) error }]()
)

// requireJSONTags panics when t (or a type reachable through it) is a struct with an exported
// field that has no json tag.
func requireJSONTags(t reflect.Type) {
	if t == nil {
		return
	}
	if _, ok := taggedTypes.Load(t); ok {
		return
	}
	checkTags(t, t.String(), map[reflect.Type]bool{})
	taggedTypes.Store(t, struct{}{})
}

// embeddedStruct is the struct type of an embedded field (or of an embedded pointer), else nil.
func embeddedStruct(f reflect.StructField) reflect.Type {
	if !f.Anonymous {
		return nil
	}
	t := f.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return t
}

func checkTags(t reflect.Type, where string, seen map[reflect.Type]bool) {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		checkTags(t.Elem(), where, seen)
	case reflect.Map:
		checkTags(t.Elem(), where, seen)
	case reflect.Struct:
		if seen[t] || reflect.PointerTo(t).Implements(jsonUnmarshaler) || reflect.PointerTo(t).Implements(textUnmarshaler) {
			return
		}
		seen[t] = true
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag, ok := f.Tag.Lookup("json")
			if !ok {
				if et := embeddedStruct(f); et != nil {
					// An embedded struct without a tag is inlined: its own fields are checked.
					checkTags(et, where, seen)
					continue
				}
				panic("httpx.DecodeJSON: field " + where + "." + f.Name + " has no json tag (json/v2 would never match it)")
			}
			if name, _, _ := strings.Cut(tag, ","); name == "" {
				// json/v2 takes the Go field name here, which no client sends.
				panic("httpx.DecodeJSON: field " + where + "." + f.Name + " has a json tag without a name (json/v2 would match the Go name): write json:\"name,...\" or json:\"-\"")
			}
			checkTags(f.Type, where+"."+f.Name, seen)
		}
	}
}
