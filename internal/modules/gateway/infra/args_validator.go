package infra

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

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
// Compiled schemas, and the failures to compile, are cached by the SHA-256 of the schema. A
// schema is compiled outside the lock, once per key (callers of the same schema wait for that one
// compilation, other schemas are not held up), and only within maxSchemaBytes and maxSchemaDepth.
type ArgsValidator struct {
	warn Warner

	mu    sync.Mutex
	cache map[[sha256.Size]byte]*cacheSlot
	order [][sha256.Size]byte
	// compiledCount counts cache misses, also the failed compiles (a test hook).
	compiledCount int
	// compileFn builds a schema (compile; a seam for tests that need a slow or blocked compile).
	compileFn func(json.RawMessage) (*jsonschema.Schema, error)
	// waiters is how many callers wait for another caller's compilation (a test hook).
	waiters atomic.Int32
}

// cacheSlot is one cache slot: the compiled schema once done is closed (nil: not enforced). disabled
// is set when the schema compiled but cannot be applied (a reference cycle found while validating).
type cacheSlot struct {
	done     chan struct{}
	sch      *jsonschema.Schema
	disabled atomic.Bool
}

// The bounds of what is compiled. Real tool schemas are far inside them (the recorded Gitea and
// YouTrack lists: at most 2.2 KB and 9 levels). Compile time grows roughly with the cube of the
// nesting depth (measured: 200 JSON levels 9 ms, 600 levels 225 ms, 1000 levels 530 ms) but only
// linearly with width (430 KB wide: 70 ms), so the depth is bounded tightly and the size generously.
const (
	maxSchemaDepth = 100
	maxSchemaBytes = 256 << 10
)

var _ app.ArgsValidator = (*ArgsValidator)(nil)

// NewArgsValidator builds a validator; warn may be nil (no warnings).
func NewArgsValidator(warn Warner) *ArgsValidator {
	return &ArgsValidator{warn: warn, cache: map[[sha256.Size]byte]*cacheSlot{}, compileFn: compile}
}

// Validate implements app.ArgsValidator: only the first offence is reported, as its JSON pointer
// ("/" for the root) and the library's message. A nil or empty schema accepts everything; nil or
// empty args are {}.
func (v *ArgsValidator) Validate(ref string, schema, args json.RawMessage) app.ArgsCheck {
	e := v.entryFor(ref, schema)
	compiled := e.sch
	if compiled == nil || e.disabled.Load() {
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
	if cycle := findRefCycle(verr); cycle {
		// A schema that refers to itself without consuming any data cannot be applied: not enforced,
		// never a refusal (its text would name the internal base address).
		if e.disabled.CompareAndSwap(false, true) {
			v.warnOnce(ref, errRefCycle)
		}
		return app.ArgsCheck{OK: true}
	}
	leaf := firstLeaf(verr)
	return app.ArgsCheck{Path: pointer(leaf.InstanceLocation), Message: messageOf(leaf)}
}

// errRefCycle is the warning text of a schema whose references loop.
var errRefCycle = fixedError("the schema's references form a cycle")

// entryFor returns the cache slot of the schema, compiling it (once, outside the lock) on a miss.
func (v *ArgsValidator) entryFor(ref string, schema json.RawMessage) *cacheSlot {
	if len(strings.TrimSpace(string(schema))) == 0 {
		return &cacheSlot{} // no schema: nothing to enforce
	}
	key := cacheKey(schema)
	v.mu.Lock()
	if e, hit := v.cache[key]; hit {
		v.mu.Unlock()
		select {
		case <-e.done:
		default:
			v.waiters.Add(1)
			<-e.done
			v.waiters.Add(-1)
		}
		return e
	}
	e := &cacheSlot{done: make(chan struct{})}
	v.compiledCount++
	if len(v.order) >= maxCachedSchemas {
		delete(v.cache, v.order[0])
		v.order = v.order[1:]
	}
	v.cache[key] = e
	v.order = append(v.order, key)
	v.mu.Unlock()

	defer close(e.done)
	sch, err := v.compileFn(schema)
	if err != nil {
		v.warnOnce(ref, err)
		return e
	}
	e.sch = sch
	return e
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
	// Only the formats the reference checks are checked: the library also asserts these four.
	for _, name := range []string{"iri", "iri-reference", "period", "semver"} {
		c.RegisterFormat(&jsonschema.Format{Name: name, Validate: func(any) error { return nil }})
	}
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

// findRefCycle is true when any error in the tree is a reference cycle.
func findRefCycle(e *jsonschema.ValidationError) bool {
	if _, ok := e.ErrorKind.(*kind.RefCycle); ok {
		return true
	}
	for _, c := range e.Causes {
		if findRefCycle(c) {
			return true
		}
	}
	return false
}

// messageOf is the library's wording for one offence, except that a format error and an
// additional-properties error do not quote the offending value or keys (the argument stays out of every text the gateway writes).
func messageOf(e *jsonschema.ValidationError) string {
	switch k := e.ErrorKind.(type) {
	case *kind.Format:
		return "value is not a valid " + k.Want
	case *kind.AdditionalProperties:
		return "additional properties are not allowed" // the library lists the keys, which are the agent's
	case *kind.Minimum:
		return "must be >= " + exactNumber(k.Want)
	case *kind.Maximum:
		return "must be <= " + exactNumber(k.Want)
	case *kind.ExclusiveMinimum:
		return "must be > " + exactNumber(k.Want)
	case *kind.ExclusiveMaximum:
		return "must be < " + exactNumber(k.Want)
	case *kind.MultipleOf:
		return "must be a multiple of " + exactNumber(k.Want)
	case *kind.PropertyNames:
		return "a property name is not allowed"
	case *kind.Pattern:
		return "value does not match the required pattern"
	case *kind.ContentEncoding, *kind.ContentMediaType:
		return "value does not have the required content encoding"
	case *kind.FalseSchema:
		switch {
		case strings.HasSuffix(e.SchemaURL, "/unevaluatedProperties"):
			return "additional properties are not allowed"
		case strings.HasSuffix(e.SchemaURL, "/unevaluatedItems"), strings.HasSuffix(e.SchemaURL, "/additionalItems"):
			return "additional items are not allowed"
		}
		return "value is not allowed"
	}
	at := *e
	at.InstanceLocation = nil
	return strings.TrimPrefix(at.Error(), "at '': ")
}

// exactNumber writes a number the schema asked for as a plain decimal, without locale grouping
// and without rounding: an integer as its digits, a finite fraction as its exact decimal.
func exactNumber(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	for digits := 1; digits <= 1100; digits++ {
		text := r.FloatString(digits)
		if back, ok := new(big.Rat).SetString(text); ok && back.Cmp(r) == 0 {
			return text
		}
	}
	return r.FloatString(30) // not a finite decimal (cannot come from JSON text)
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
