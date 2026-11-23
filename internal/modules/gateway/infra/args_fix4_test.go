package infra

import (
	"fmt"
	"strings"
	"testing"
)

// wantNotEnforced checks that the schema is not enforced (every call passes) with one warning
// that gives the reason.
func wantNotEnforced(t *testing.T, schema string, reason error, args ...string) {
	t.Helper()
	v, w := newValidator(t)
	for i := 0; i < 2; i++ {
		for _, a := range args {
			if got := check(v, schema, a); !got.OK {
				t.Fatalf("%.40s: got %+v, want not enforced", a, got)
			}
		}
	}
	all := w.all()
	if len(all) != 1 {
		t.Fatalf("warnings %d, want one", len(all))
	}
	if got := all[0].args[len(all[0].args)-1]; got != reason.Error() {
		t.Fatalf("warning %q, want %q", got, reason)
	}
}

// The review's reproduction: the recursion consumes data, so the library never meets a cycle while
// validating, and each level multiplies the work. Any cycle in the references is not enforced.
func TestASchemaWhoseReferencesFormACycleIsNotEnforced(t *testing.T) {
	const reviewer = `{"anyOf":[{"type":"array","items":{"$ref":"#"},"maxItems":0},{"type":"array","items":{"$ref":"#"},"minItems":5}]}`
	deep := strings.Repeat("[", 22) + "1" + strings.Repeat("]", 22)
	wantNotEnforced(t, reviewer, errRefCycle, deep)

	for name, s := range map[string]string{
		"terminating recursion": `{"$defs":{"node":{"type":"object","properties":{"child":{"$ref":"#/$defs/node"},"n":{"type":"number"}}}},"$ref":"#/$defs/node"}`,
		"through $defs":         `{"properties":{"x":{"$ref":"#/$defs/a"}},"$defs":{"a":{"items":{"$ref":"#/$defs/b"}},"b":{"properties":{"y":{"$ref":"#/$defs/a"}}}}}`,
		"through $anchor":       `{"properties":{"x":{"$ref":"#node"}},"$defs":{"n":{"$anchor":"node","properties":{"c":{"$ref":"#node"}},"required":["c"]}}}`,
		"through $id":           `{"properties":{"x":{"$ref":"https://gateway.invalid/inner.json"}},"$defs":{"n":{"$id":"https://gateway.invalid/inner.json","properties":{"c":{"$ref":"inner.json"}},"required":["c"]}}}`,
		"$dynamicRef":           `{"$dynamicAnchor":"meta","properties":{"x":{"items":{"$dynamicRef":"#meta"}}},"required":["x"]}`,
		"$recursiveRef":         `{"$schema":"https://json-schema.org/draft/2019-09/schema","$recursiveAnchor":true,"properties":{"x":{"items":{"$recursiveRef":"#"}}},"required":["x"]}`,
		"draft-07 definitions":  `{"$schema":"http://json-schema.org/draft-07/schema#","definitions":{"a":{"properties":{"c":{"$ref":"#/definitions/a"}},"required":["c"]}},"properties":{"x":{"$ref":"#/definitions/a"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantNotEnforced(t, s, errRefCycle, `{"x":{"c":1}}`, `{"child":{"n":"x"}}`, `{}`)
		})
	}
}

// fanIn is a chain of $defs in which each level refers to the next twice: no cycle, but the
// reference-expanded schema has 2^levels nodes.
func fanIn(levels int) string {
	var sb strings.Builder
	sb.WriteString(`{"properties":{"a":{"$ref":"#/$defs/d0"}},"$defs":{`)
	for i := 0; i < levels; i++ {
		fmt.Fprintf(&sb, `"d%d":{"allOf":[{"$ref":"#/$defs/d%d"},{"$ref":"#/$defs/d%d"}]},`, i, i+1, i+1)
	}
	fmt.Fprintf(&sb, `"d%d":{"type":"string"}}}`, levels)
	return sb.String()
}

func TestASchemaTooLargeOnceItsReferencesAreExpandedIsNotEnforced(t *testing.T) {
	wantNotEnforced(t, fanIn(40), errTooExpanded, `{"a":1}`, `{"a":[[[[[[1]]]]]]}`)
}

// Shared definitions without recursion are ordinary and stay enforced, also a modest fan-in.
func TestSharedDefinitionsWithoutACycleAreEnforced(t *testing.T) {
	v, w := newValidator(t)
	const s = `{"type":"object","$defs":{"id":{"type":"integer","minimum":1},"user":{"type":"object","properties":{"id":{"$ref":"#/$defs/id"}},"required":["id"]}},
"properties":{"owner":{"$ref":"#/$defs/user"},"assignees":{"type":"array","items":{"$ref":"#/$defs/user"}},"parent":{"$ref":"#/$defs/id"}}}`
	wantRefused(t, check(v, s, `{"owner":{"id":1},"assignees":[{"id":2},{"id":0}]}`), "/assignees/1/id")
	wantRefused(t, check(v, s, `{"parent":"x"}`), "/parent")
	if got := check(v, s, `{"owner":{"id":1},"assignees":[{"id":2}],"parent":3}`); !got.OK {
		t.Fatalf("got %+v", got)
	}
	wantRefused(t, check(v, fanIn(6), `{"a":1}`), "/a")
	if n := len(w.all()); n != 0 {
		t.Fatalf("warnings %v", w.all())
	}
}

// A pattern reached through a $ref to a location that is not a schema keyword is never compiled:
// the engine the compiler gets matches everything.
func TestAPatternBehindARefIsNotCompiled(t *testing.T) {
	v, w := newValidator(t)
	const s = `{"properties":{"a":{"$ref":"#/x/y"}},"x":{"y":{"pattern":"^b+$"}}}`
	if got := check(v, s, `{"a":"zzz"}`); !got.OK {
		t.Fatalf("got %+v: the pattern was applied", got)
	}
	// A JavaScript-only pattern there does not make the rest of the schema unenforced.
	const js = `{"properties":{"a":{"$ref":"#/x/y"},"n":{"type":"number"}},"x":{"y":{"pattern":"(?<=a)b"}}}`
	wantRefused(t, check(v, js, `{"a":"zzz","n":"x"}`), "/n")
	if n := len(w.all()); n != 0 {
		t.Fatalf("warnings %v", w.all())
	}
	// patternProperties there would claim every property (a match-all expression): not enforced.
	wantNotEnforced(t, `{"properties":{"a":{"$ref":"#/x/y"}},"x":{"y":{"patternProperties":{"^q":{"type":"number"}}}}}`, errPatternPropertiesBehindRef, `{"a":{"k":"v"}}`)
}
