package infra

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// maxExpandedNodes bounds the size of a compiled schema with every reference expanded in place. A
// validation can visit each of those nodes for each value of the arguments, so a few bytes of
// schema that refer to the same definition twice per level (2^levels nodes) would cost
// exponential time. Real tool schemas are far below it (the recorded Gitea and YouTrack lists have
// no reference at all).
const maxExpandedNodes = 10_000

// errTooExpanded is the warning text of a schema over maxExpandedNodes.
var errTooExpanded = fixedError("the schema is too large to be checked once its references are expanded")

// errPatternPropertiesBehindRef is the warning text of patternProperties the walk could not strip:
// the never-compiling expression engine would make it claim every property.
var errPatternPropertiesBehindRef = fixedError("the schema has patternProperties that cannot be removed")

// neverCompiled is the expression engine the compiler gets: upstream expressions are never
// compiled (the walk removes pattern and patternProperties at the schema positions; one reached
// through a $ref to any other location lands here) and the result matches everything.
func neverCompiled(string) (jsonschema.Regexp, error) { return matchAll{}, nil }

type matchAll struct{}

func (matchAll) MatchString(string) bool { return true }
func (matchAll) String() string          { return "" }

// errUnstrippedKeyword is the warning text of uniqueItems or multipleOf the walk could not strip
// (reached through a $ref to a location that is not a schema position): the gateway never runs
// them (uniqueItems is quadratic through the library's hash collisions, multipleOf divides exact
// rationals and keeps one per failure).
var errUnstrippedKeyword = fixedError("the schema has uniqueItems or multipleOf that cannot be removed")

// errDynamicRef is the warning text of a schema that uses $dynamicRef or $recursiveRef.
var errDynamicRef = fixedError("the schema uses dynamic references ($dynamicRef or $recursiveRef)")

// maxValidationWork bounds one validation: the schema's reference-expanded weight times the
// number of JSON values in the arguments. Validation can visit every expanded node for every value,
// so past this the arguments go on to the upstream unchecked, like arguments over maxArgsBytes.
// The weight is ownWeight summed over the expanded nodes (a long enum or required list weighs its
// length). The library also keeps an error (about 130 bytes) for every failing (node, value) pair,
// so the budget bounds memory too: at 1 000 000 three failing type checks over 255 KB of numbers
// held 132 MiB; at 250 000 the known hostile shapes stay within tens of MiB (see
// TestKnownAdversarialShapesStayWithinTheirAllocationCeiling).
const maxValidationWork = 250_000

// checkBounds refuses a compiled schema that uses a dynamic reference ($dynamicRef or
// $recursiveRef reachable from the root: its targets are decided while validating, and the
// library compiles every $dynamicAnchor of a resource, also one in an unreferenced $defs entry,
// so the graph below cannot see them), whose references form any cycle, whose reference-expanded
// size is over maxExpandedNodes, or that still has patternProperties. A keyword in a schema
// position is what counts: a property merely named "$dynamicRef" compiles to no reference. The
// cost is linear in the compiled graph: each node is expanded once.
//
// The validator's own cycle detection does not catch a cycle that consumes data on the way (an
// "items" or a "properties" between the $ref and its target): the library only stops a loop
// that returns to the same schema at the same instance location.
func checkBounds(root *jsonschema.Schema) error {
	_, _, err := expandedSize(root)
	return err
}

// expandedSize is the reference-expanded node count of a compiled schema (saturated at
// maxExpandedNodes+1) and its reference-expanded weight (the work one argument value can cost,
// saturated at maxValidationWork+1), or the reason it is not enforced. The node count is the
// structural bound (never enforced over it); the weight only feeds the work budget, so a schema
// with one long enum is still enforced for a few argument values.
func expandedSize(root *jsonschema.Schema) (nodes, work int, err error) {
	g := &schemaGraph{state: map[*jsonschema.Schema]int{}, size: map[*jsonschema.Schema]int{}, work: map[*jsonschema.Schema]int{}}
	n, w, err := g.expand(root)
	switch {
	case err != nil:
		return 0, 0, err
	case g.patternProperties:
		return 0, 0, errPatternPropertiesBehindRef
	case n > maxExpandedNodes:
		return 0, 0, errTooExpanded
	}
	return n, w, nil
}

const (
	unvisited = iota
	onPath
	finished
)

type schemaGraph struct {
	state             map[*jsonschema.Schema]int
	size              map[*jsonschema.Schema]int
	work              map[*jsonschema.Schema]int
	patternProperties bool
}

// expand is the reference-expanded node count and weight of s (memoised, both saturating) or
// errRefCycle when a path from s comes back to a schema still being expanded, or errDynamicRef at
// the first dynamic reference. It notes patternProperties on the way.
func (g *schemaGraph) expand(s *jsonschema.Schema) (int, int, error) {
	switch g.state[s] {
	case onPath:
		return 0, 0, errRefCycle
	case finished:
		return g.size[s], g.work[s], nil
	}
	if s.DynamicRef != nil || s.RecursiveRef != nil {
		return 0, 0, errDynamicRef
	}
	g.state[s] = onPath
	if len(s.PatternProperties) > 0 {
		g.patternProperties = true
	}
	if s.UniqueItems || s.MultipleOf != nil {
		return 0, 0, errUnstrippedKeyword
	}
	total, work := 1, ownWeight(s)
	for _, c := range staticChildren(s) {
		n, w, err := g.expand(c)
		if err != nil {
			return 0, 0, err
		}
		total = min(total+n, maxExpandedNodes+1)
		work = min(work+w, maxValidationWork+1)
	}
	g.state[s] = finished
	g.size[s], g.work[s] = total, work
	return total, work, nil
}

// ownWeight is the work one argument value can cost in s itself, beyond its subschemas (which
// expand counts as children). Every field of the compiled Schema (jsonschema/v6 v6.0.3):
//
//	field                                 per-value work grows with it?
//	Bool, ID, Anchor, DynamicAnchor,      no: scalars or annotations
//	RecursiveAnchor, DraftVersion,
//	Location
//	Ref, RecursiveRef, DynamicRef         children (dynamic ones are refused)
//	Types                                 no: at most seven types
//	Enum.Values                           yes: each entry compared, an entry by its JSON values
//	Const                                 yes: compared by its JSON values
//	Not, AllOf, AnyOf, OneOf, If, Then,   children
//	Else
//	Format                                no: one check of the value itself
//	MaxProperties, MinProperties          no
//	Required                              yes: each name looked up, the missing ones listed
//	PropertyNames                         child
//	Properties                            children (one per name)
//	PatternProperties                     refused (see checkBounds)
//	AdditionalProperties                  child when a schema
//	Dependencies                          child when a schema; yes for a name list
//	DependentRequired                     yes: the names of every list
//	DependentSchemas                      children
//	UnevaluatedProperties                 child
//	MinItems, MaxItems, MinContains,      no
//	MaxContains
//	UniqueItems                           stripped (prepare); refused when left behind a $ref: the
//	                                      library's hash collides for 1 and -1 (absolute
//	                                      numerator bytes), so it is quadratic in the argument
//	Contains, Items, AdditionalItems,     children
//	PrefixItems, Items2020,
//	UnevaluatedItems
//	MinLength, MaxLength, Pattern         no (Pattern matches everything here)
//	ContentEncoding, ContentMediaType     no: one decode of the value itself
//	ContentSchema                         child
//	Maximum, Minimum, ExclusiveMaximum,   no
//	ExclusiveMinimum                      (paid by the width of each argument number)
//	MultipleOf                            stripped (prepare), refused behind a $ref: exact
//	                                      rational division, one Rat kept per failure
//	Extensions                            no: none registered
//	Title, Description, Default,          no: annotations, never evaluated
//	Comment, ReadOnly, WriteOnly,
//	Examples, Deprecated
func ownWeight(s *jsonschema.Schema) int {
	w := 1 + len(s.Required)
	if s.Enum != nil {
		for _, v := range s.Enum.Values {
			w += enumValueWeight * valueCount(v)
		}
	}
	if s.Const != nil {
		w += enumValueWeight * valueCount(*s.Const)
	}
	for _, names := range s.DependentRequired {
		w += len(names)
	}
	for _, d := range s.Dependencies {
		if names, ok := d.([]string); ok {
			w += len(names)
		}
	}
	return min(w, maxValidationWork+1)
}

// enumValueWeight is the weight of one JSON value of an enum entry or a const. The library
// compares numbers exactly (big.Rat through their text), about 4.7 us per entry and argument value,
// some 20 times an ordinary expanded node (0.7 ms / 3 000 nodes): measured, 95 argument values
// against a 10 000-number enum took 0.45 s at weight 1.
const enumValueWeight = 16

// valueCount is the number of JSON values in a decoded value (itself and everything inside).
func valueCount(v any) int {
	n := 1
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			n += valueCount(e)
		}
	case map[string]any:
		for _, e := range x {
			n += valueCount(e)
		}
	}
	return n
}

// countValues is the number of JSON values in a JSON text (every scalar, array and object; object
// keys count too, which only overestimates), read from the bytes without decoding. A number
// counts by its width (numberWeight).
// numberWeight is what one number literal counts as: 1 + (its length in bytes + |its decimal
// exponent|) / 16. The library computes with exact rationals, whose cost grows with the digits
// and the exponent (1e-400 is a 401-digit denominator), so a wide number pays for the number
// keywords that remain (minimum, maximum, exclusive*, enum, const). Literals beyond the bounds of
// numberTooBig are refused before this is used.
func numberWeight(literal []byte) int {
	exp := 0
	if i := strings.IndexAny(string(literal), "eE"); i >= 0 {
		if e, err := strconv.Atoi(strings.TrimPrefix(string(literal[i+1:]), "+")); err == nil {
			exp = max(e, -e)
		}
	}
	return 1 + (len(literal)+min(exp, maxNumberExponent))/16
}

func countValues(doc []byte) int {
	n := 0
	for i := 0; i < len(doc); {
		switch c := doc[i]; {
		case c == '"':
			n++
			for i++; i < len(doc) && doc[i] != '"'; i++ {
				if doc[i] == '\\' {
					i++
				}
			}
			i++
		case c == '{' || c == '[':
			n++
			i++
		case c == '-' || (c >= '0' && c <= '9'):
			start := i
			for i < len(doc) && strings.IndexByte(",]} \t\r\n", doc[i]) < 0 {
				i++
			}
			n += numberWeight(doc[start:i])
		case c == 't' || c == 'f' || c == 'n':
			n++
			for i < len(doc) && strings.IndexByte(",]} \t\r\n", doc[i]) < 0 {
				i++
			}
		default:
			i++
		}
	}
	return n
}

// staticChildren are the subschemas of s and the targets of its references as compiled.
func staticChildren(s *jsonschema.Schema) []*jsonschema.Schema {
	var out []*jsonschema.Schema
	add := func(list ...*jsonschema.Schema) {
		for _, c := range list {
			if c != nil {
				out = append(out, c)
			}
		}
	}
	addAny := func(v any) {
		switch c := v.(type) {
		case *jsonschema.Schema:
			add(c)
		case []*jsonschema.Schema:
			add(c...)
		}
	}
	add(s.Ref, s.RecursiveRef, s.Not, s.If, s.Then, s.Else, s.PropertyNames, s.UnevaluatedProperties,
		s.Contains, s.Items2020, s.UnevaluatedItems, s.ContentSchema)
	if s.DynamicRef != nil {
		add(s.DynamicRef.Ref)
	}
	add(s.AllOf...)
	add(s.AnyOf...)
	add(s.OneOf...)
	add(s.PrefixItems...)
	for _, c := range s.Properties {
		add(c)
	}
	for _, c := range s.PatternProperties {
		add(c)
	}
	for _, c := range s.DependentSchemas {
		add(c)
	}
	for _, d := range s.Dependencies {
		addAny(d)
	}
	addAny(s.AdditionalProperties)
	addAny(s.Items)
	addAny(s.AdditionalItems)
	return out
}

// maxRefusalMessage is how many characters of the refusal the validator derives (the JSON pointer,
// ": " and the library's message) the agent gets: the library lists every allowed value of an enum
// and every missing required name, and the pointer carries the agent's keys, both as long as the
// schema or the arguments. The reference has no cap (ajv's messages never list them).
const maxRefusalMessage = 2000

// capRefusal cuts the pointer and the message so that together with the ": " between them they
// stay within maxRefusalMessage characters, each cut at a rune boundary ending in an ellipsis. The
// message keeps at least half when both are long.
func capRefusal(path, message string) (string, string) {
	room := maxRefusalMessage - 2
	pathLen, msgLen := utf8.RuneCountInString(path), utf8.RuneCountInString(message)
	if pathLen+msgLen <= room {
		return path, message
	}
	pathRoom := max(room-msgLen, room/2)
	path = cutRunes(path, pathRoom)
	return path, cutRunes(message, room-utf8.RuneCountInString(path))
}

// cutRunes cuts s to at most limit characters at a rune boundary, the last one an ellipsis.
func cutRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return string(r[:limit-1]) + "…"
}
