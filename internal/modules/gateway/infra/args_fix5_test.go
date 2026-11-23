package infra

import (
	"fmt"
	"strings"
	"testing"
)

// A $dynamicAnchor in an unreferenced $defs entry is a target the static graph never shows: any
// reachable $dynamicRef or $recursiveRef makes the schema not enforced.
func TestASchemaWithADynamicReferenceIsNotEnforced(t *testing.T) {
	const base = "https://gateway.invalid/"
	hidden := `{"properties":{"a":{"$ref":"` + base + `s"}},"$defs":{` +
		`"x":{"$dynamicAnchor":"m","anyOf":[{"type":"array","items":{"$ref":"` + base + `s"},"maxItems":0},{"type":"array","items":{"$ref":"` + base + `s"},"minItems":5}]},` +
		`"s":{"$id":"` + base + `s","$dynamicRef":"` + base + `t#m"},` +
		`"t":{"$id":"` + base + `t","$dynamicAnchor":"m"}}}`
	deep := `{"a":` + strings.Repeat("[", 20) + "1" + strings.Repeat("]", 20) + `}`
	wantNotEnforced(t, hidden, errDynamicRef, deep)
	wantNotEnforced(t, `{"$dynamicAnchor":"meta","properties":{"x":{"items":{"$dynamicRef":"#meta"}}},"required":["x"]}`, errDynamicRef, `{}`)
	wantNotEnforced(t, `{"$schema":"https://json-schema.org/draft/2019-09/schema","$recursiveAnchor":true,"properties":{"x":{"items":{"$recursiveRef":"#"}}},"required":["x"]}`, errDynamicRef, `{}`)
}

// A property merely NAMED $dynamicRef or $recursiveRef is an ordinary property.
func TestAPropertyNamedLikeADynamicReferenceIsOrdinary(t *testing.T) {
	v, w := newValidator(t)
	const s = `{"type":"object","properties":{"$dynamicRef":{"type":"string"},"$recursiveRef":{"type":"integer"}},"required":["$dynamicRef"]}`
	wantRefused(t, check(v, s, `{"$recursiveRef":1}`), "/")
	wantRefused(t, check(v, s, `{"$dynamicRef":"x","$recursiveRef":"no"}`), "/$recursiveRef")
	if got := check(v, s, `{"$dynamicRef":"x","$recursiveRef":2}`); !got.OK {
		t.Fatalf("got %+v", got)
	}
	if n := len(w.all()); n != 0 {
		t.Fatalf("warnings %v", w.all())
	}
}

// fanInItems is an acyclic allOf fan-in of `levels` levels (about 3 * 2^levels expanded nodes)
// applied to every element of the array "a"; the leaf requires an integer.
func fanInItems(levels int) string {
	var sb strings.Builder
	sb.WriteString(`{"properties":{"a":{"type":"array","items":{"$ref":"#/$defs/d0"}}},"$defs":{`)
	for i := 0; i < levels; i++ {
		fmt.Fprintf(&sb, `"d%d":{"allOf":[{"$ref":"#/$defs/d%d"},{"$ref":"#/$defs/d%d"}]},`, i, i+1, i+1)
	}
	fmt.Fprintf(&sb, `"d%d":{"type":"integer"}}}`, levels)
	return sb.String()
}

// Validation work is expanded schema nodes times argument values: past the budget the arguments
// go on to the upstream unchecked, silently, as arguments over maxArgsBytes do.
func TestValidationWorkOverTheBudgetIsNotDone(t *testing.T) {
	v, w := newValidator(t)
	s := fanInItems(10)
	many := `{"a":[` + strings.Repeat("1,", 3999) + `"x"]}`
	if got := check(v, s, many); !got.OK {
		t.Fatalf("4000 elements: %+v, want not validated", got)
	}
	wantRefused(t, check(v, s, `{"a":[1,2,3,4,5,6,7,8,9,"x"]}`), "/a/9")
	if n := len(w.all()); n != 0 {
		t.Fatalf("warnings %v: no log line per call", w.all())
	}
}

func TestArgumentValuesAreCountedFromTheBytes(t *testing.T) {
	for doc, want := range map[string]int{
		`{}`:                             1,
		`{"a":1}`:                        3, // the object, the key, the value
		`[1,"x",true,null,{"k":[]}]`:     8,
		`{"s":"a\"b{[1,2]}","n":-1.5e3}`: 5,
	} {
		if got := countValues([]byte(doc)); got != want {
			t.Errorf("%s: %d, want %d", doc, got, want)
		}
	}
}
