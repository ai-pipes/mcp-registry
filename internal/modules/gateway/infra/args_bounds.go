package infra

import (
	"strings"

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

// errDynamicRef is the warning text of a schema that uses $dynamicRef or $recursiveRef.
var errDynamicRef = fixedError("the schema uses dynamic references ($dynamicRef or $recursiveRef)")

// maxValidationWork bounds one validation: the schema's reference-expanded node count times the
// number of JSON values in the arguments. Validation can visit every expanded node for every value,
// so past this the arguments go on to the upstream unchecked, like arguments over maxArgsBytes.
// Measured: a 10-level allOf fan-in (about 3 000 expanded nodes) on "items" costs about 0.7 ms per
// element, so 1 000 000 is about a quarter of a second in the worst shape.
const maxValidationWork = 1_000_000

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
	_, err := expandedSize(root)
	return err
}

// expandedSize is the reference-expanded node count of a compiled schema (saturated at
// maxExpandedNodes+1), or the reason it is not enforced.
func expandedSize(root *jsonschema.Schema) (int, error) {
	g := &schemaGraph{state: map[*jsonschema.Schema]int{}, size: map[*jsonschema.Schema]int{}}
	n, err := g.expand(root)
	switch {
	case err != nil:
		return 0, err
	case g.patternProperties:
		return 0, errPatternPropertiesBehindRef
	case n > maxExpandedNodes:
		return 0, errTooExpanded
	}
	return n, nil
}

const (
	unvisited = iota
	onPath
	finished
)

type schemaGraph struct {
	state             map[*jsonschema.Schema]int
	size              map[*jsonschema.Schema]int
	patternProperties bool
}

// expand is the reference-expanded node count of s (memoised; it stops counting past the bound)
// or errRefCycle when a path from s comes back to a schema still being expanded, or errDynamicRef
// at the first dynamic reference. It notes patternProperties on the way.
func (g *schemaGraph) expand(s *jsonschema.Schema) (int, error) {
	switch g.state[s] {
	case onPath:
		return 0, errRefCycle
	case finished:
		return g.size[s], nil
	}
	if s.DynamicRef != nil || s.RecursiveRef != nil {
		return 0, errDynamicRef
	}
	g.state[s] = onPath
	if len(s.PatternProperties) > 0 {
		g.patternProperties = true
	}
	total := 1
	for _, c := range staticChildren(s) {
		n, err := g.expand(c)
		if err != nil {
			return 0, err
		}
		total = min(total+n, maxExpandedNodes+1)
	}
	g.state[s] = finished
	g.size[s] = total
	return total, nil
}

// countValues is the number of JSON values in a JSON text (every scalar, array and object; object
// keys count too, which only overestimates), read from the bytes without decoding.
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
		case c == '-' || (c >= '0' && c <= '9') || c == 't' || c == 'f' || c == 'n':
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
