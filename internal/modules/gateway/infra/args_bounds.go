package infra

import (
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

// checkBounds refuses a compiled schema whose references form any cycle (including $ref "#",
// $dynamicRef and $recursiveRef, whose dynamic targets are taken to be every schema with the
// matching anchor), whose reference-expanded size is over maxExpandedNodes, or that still has
// patternProperties. The cost is linear in the compiled graph: each node is expanded once.
//
// The validator's own cycle detection does not catch a cycle that consumes data on the way (an
// "items" or a "properties" between the $ref and its target): the library only stops a loop
// that returns to the same schema at the same instance location.
func checkBounds(root *jsonschema.Schema) error {
	g := &schemaGraph{state: map[*jsonschema.Schema]int{}, size: map[*jsonschema.Schema]int{}, dynamic: map[string][]*jsonschema.Schema{}}
	g.collect(root, map[*jsonschema.Schema]bool{})
	if g.patternProperties {
		return errPatternPropertiesBehindRef
	}
	n, err := g.expand(root)
	if err != nil {
		return err
	}
	if n > maxExpandedNodes {
		return errTooExpanded
	}
	return nil
}

const (
	unvisited = iota
	onPath
	finished
)

type schemaGraph struct {
	state map[*jsonschema.Schema]int
	size  map[*jsonschema.Schema]int
	// dynamic holds the possible targets of a dynamic reference: "$" + anchor for $dynamicAnchor,
	// "recursive" for $recursiveAnchor: true.
	dynamic           map[string][]*jsonschema.Schema
	patternProperties bool
}

// collect records the anchors of every schema reachable from s (structurally or by a static
// reference), so that a dynamic reference can be given every target it might resolve to.
func (g *schemaGraph) collect(s *jsonschema.Schema, seen map[*jsonschema.Schema]bool) {
	if s == nil || seen[s] {
		return
	}
	seen[s] = true
	if s.DynamicAnchor != "" {
		g.dynamic["$"+s.DynamicAnchor] = append(g.dynamic["$"+s.DynamicAnchor], s)
	}
	if s.RecursiveAnchor {
		g.dynamic["recursive"] = append(g.dynamic["recursive"], s)
	}
	if len(s.PatternProperties) > 0 {
		g.patternProperties = true
	}
	for _, c := range staticChildren(s) {
		g.collect(c, seen)
	}
}

// expand is the reference-expanded node count of s (memoised; it stops counting past the bound)
// or errRefCycle when a path from s comes back to a schema still being expanded.
func (g *schemaGraph) expand(s *jsonschema.Schema) (int, error) {
	switch g.state[s] {
	case onPath:
		return 0, errRefCycle
	case finished:
		return g.size[s], nil
	}
	g.state[s] = onPath
	total := 1
	for _, c := range g.children(s) {
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

// children are the static children of s plus every possible target of its dynamic references.
func (g *schemaGraph) children(s *jsonschema.Schema) []*jsonschema.Schema {
	out := staticChildren(s)
	if s.DynamicRef != nil && s.DynamicRef.Anchor != "" {
		out = append(out, g.dynamic["$"+s.DynamicRef.Anchor]...)
	}
	if s.RecursiveRef != nil {
		out = append(out, g.dynamic["recursive"]...)
	}
	return out
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
