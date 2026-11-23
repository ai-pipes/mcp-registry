package infra

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// maxCachedSchemas bounds the compiled-schema cache (MAX_CACHED of the reference): when it is full
// the oldest inserted entry goes first; a hit does not refresh.
const maxCachedSchemas = 2000

// maxWarnedError is how much of a compile error the warning carries.
const maxWarnedError = 500

// schemaBase is the address the tool schema is compiled under. Nothing is ever loaded from it:
// the compiler has no URL loader, so a $ref to another document is a compile error (not enforced).
const schemaBase = "https://gateway.invalid/tool-input-schema.json"

// Warner receives the one warning per schema that cannot be compiled. *slog.Logger fits. The
// record carries the server, the tool and the compile error, never the arguments.
type Warner interface {
	Warn(msg string, args ...any)
}

// ArgsValidator is app.ArgsValidator on santhosh-tekuri/jsonschema (ruling 5). It is advisory (the
// upstream validates too): a schema it cannot use is not enforced, with one warning per schema.
// Compiled schemas, and the failures to compile, are cached by the SHA-256 of the schema.
type ArgsValidator struct {
	warn Warner

	mu    sync.Mutex
	cache map[[sha256.Size]byte]*jsonschema.Schema // nil value: cannot be compiled, not enforced
	order [][sha256.Size]byte
	// compiledCount counts cache misses, also the failed compiles (a test hook).
	compiledCount int
}

var _ app.ArgsValidator = (*ArgsValidator)(nil)

// NewArgsValidator builds a validator; warn may be nil (no warnings).
func NewArgsValidator(warn Warner) *ArgsValidator {
	return &ArgsValidator{warn: warn, cache: map[[sha256.Size]byte]*jsonschema.Schema{}}
}

// Validate implements app.ArgsValidator: only the first offence is reported, as its JSON pointer
// ("/" for the root) and the library's message. A nil or empty schema accepts everything; nil or
// empty args are {}.
func (v *ArgsValidator) Validate(ref string, schema, args json.RawMessage) app.ArgsCheck {
	compiled := v.compiled(ref, schema)
	if compiled == nil {
		return app.ArgsCheck{OK: true}
	}
	doc := "{}"
	if len(strings.TrimSpace(string(args))) > 0 {
		doc = string(args)
	}
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(doc))
	if err != nil {
		return app.ArgsCheck{Path: "/", Message: "the arguments are not valid JSON"}
	}
	err = compiled.Validate(value)
	if err == nil {
		return app.ArgsCheck{OK: true}
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return app.ArgsCheck{Path: "/", Message: "the arguments do not match the schema"}
	}
	leaf := firstLeaf(verr)
	return app.ArgsCheck{Path: pointer(leaf.InstanceLocation), Message: messageOf(leaf)}
}

// compiled returns the compiled schema, or nil when the schema is absent or cannot be compiled.
func (v *ArgsValidator) compiled(ref string, schema json.RawMessage) *jsonschema.Schema {
	if len(strings.TrimSpace(string(schema))) == 0 {
		return nil
	}
	key := cacheKey(schema)
	v.mu.Lock()
	defer v.mu.Unlock()
	if sch, hit := v.cache[key]; hit {
		return sch
	}
	v.compiledCount++
	sch, err := compile(schema)
	if err != nil {
		v.warnOnce(ref, err)
		sch = nil
	}
	if len(v.order) >= maxCachedSchemas {
		delete(v.cache, v.order[0])
		v.order = v.order[1:]
	}
	v.cache[key] = sch
	v.order = append(v.order, key)
	return sch
}

// cacheKey hashes the schema as given, whitespace aside.
func cacheKey(schema json.RawMessage) [sha256.Size]byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, schema); err != nil {
		return sha256.Sum256(schema)
	}
	return sha256.Sum256(buf.Bytes())
}

func (v *ArgsValidator) warnOnce(ref string, err error) {
	if v.warn == nil {
		return
	}
	server, tool, found := strings.Cut(ref, ".")
	if !found {
		server, tool = "", ref
	}
	text := err.Error()
	if r := []rune(text); len(r) > maxWarnedError {
		text = string(r[:maxWarnedError])
	}
	v.warn.Warn("gateway: tool input schema not compiled, not validating", "server", server, "tool", tool, "error", text)
}

func compile(raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, d, err := prepare(raw)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(d.draft)
	c.AssertFormat()
	if err := c.AddResource(schemaBase, doc); err != nil {
		return nil, err
	}
	return c.Compile(schemaBase)
}

// firstLeaf is the offence reported to the agent. The library evaluates the properties of an
// object in map order, so the order is fixed here: the node's own keyword errors (required,
// additionalProperties ...) come first, then the errors below it by instance location. For a
// failed anyOf or oneOf the first branch's error is reported, not the summary.
func firstLeaf(e *jsonschema.ValidationError) *jsonschema.ValidationError {
	for len(e.Causes) > 0 {
		causes := e.Causes
		switch e.ErrorKind.(type) {
		case *kind.AnyOf, *kind.OneOf:
			e = causes[0]
			continue
		case *kind.Group, *kind.Schema:
			causes = slices.Clone(causes)
			own := len(e.InstanceLocation)
			slices.SortStableFunc(causes, func(a, b *jsonschema.ValidationError) int {
				aOwn, bOwn := len(a.InstanceLocation) <= own, len(b.InstanceLocation) <= own
				switch {
				case aOwn && bOwn:
					return 0
				case aOwn:
					return -1
				case bOwn:
					return 1
				}
				return slices.Compare(a.InstanceLocation, b.InstanceLocation)
			})
		}
		e = causes[0]
	}
	return e
}

// messageOf is the library's wording for one offence, except that a format error and an
// additional-properties error do not quote the offending value or keys (the argument stays out of every text the gateway writes).
func messageOf(e *jsonschema.ValidationError) string {
	switch k := e.ErrorKind.(type) {
	case *kind.Format:
		return "value is not a valid " + k.Want
	case *kind.AdditionalProperties:
		return "additional properties are not allowed" // the library names them: they are the agent's keys
	}
	at := *e
	at.InstanceLocation = nil
	return strings.TrimPrefix(at.Error(), "at '': ")
}

// pointer is a JSON pointer, "/" for the root (the reference's convention for the empty pointer).
func pointer(tokens []string) string {
	if len(tokens) == 0 {
		return "/"
	}
	var sb strings.Builder
	for _, t := range tokens {
		sb.WriteByte('/')
		sb.WriteString(strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1"))
	}
	return sb.String()
}
