package infra

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// signPatterns writes arrays of n elements, each 1 or -1, every sign pattern in turn, until the
// document has `values` JSON values (countValues) or the arguments cap is near.
func signPatterns(n, values int) string {
	var sb strings.Builder
	sb.WriteString(`{"a":[`)
	count := 3
	for p := 0; count+n+1 <= values && sb.Len()+4*n < maxArgsBytes-16; p++ {
		if p > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('[')
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			if p>>(i%31)&1 == 1 {
				sb.WriteString("-1")
			} else {
				sb.WriteString("1")
			}
		}
		sb.WriteByte(']')
		count += n + 1
	}
	sb.WriteString(`]}`)
	return sb.String()
}

// uniqueItems and multipleOf are never applied: they are stripped at schema positions, and one
// reached through a $ref to another location makes the schema not enforced.
func TestUniqueItemsAndMultipleOfAreNotChecked(t *testing.T) {
	v, w := newValidator(t)
	if got := check(v, `{"type":"object","properties":{"a":{"type":"array","uniqueItems":true}}}`, `{"a":[1,1]}`); !got.OK {
		t.Fatalf("uniqueItems applied: %+v", got)
	}
	if got := check(v, `{"type":"object","properties":{"a":{"multipleOf":3}}}`, `{"a":4}`); !got.OK {
		t.Fatalf("multipleOf applied: %+v", got)
	}
	// The rest of the node still applies; properties merely named so are ordinary.
	wantRefused(t, check(v, `{"type":"object","properties":{"a":{"type":"array","uniqueItems":true,"maxItems":1}}}`, `{"a":[1,2]}`), "/a")
	wantRefused(t, check(v, `{"type":"object","properties":{"uniqueItems":{"type":"string"},"multipleOf":{"type":"string"}}}`, `{"uniqueItems":1}`), "/uniqueItems")
	if n := len(w.all()); n != 0 {
		t.Fatalf("warnings %v", w.all())
	}
	wantNotEnforced(t, `{"properties":{"a":{"$ref":"#/x/y"}},"x":{"y":{"uniqueItems":true}}}`, errUnstrippedKeyword, `{"a":[1,1]}`)
	wantNotEnforced(t, `{"properties":{"a":{"$ref":"#/x/y"}},"x":{"y":{"multipleOf":3}}}`, errUnstrippedKeyword, `{"a":4}`)
}

// A number literal weighs its width: 1e-400 is not one cheap value.
func TestWideNumbersWeighMore(t *testing.T) {
	if got := countValues([]byte(`[1e-400]`)); got != 1+1+(6+400)/16 {
		t.Fatalf("[1e-400]: %d", got)
	}
	if got := countValues([]byte(`[1,-2.5,30]`)); got != 4 {
		t.Fatalf("plain numbers: %d", got)
	}
	if got := countValues([]byte(`["1e-400"]`)); got != 2 {
		t.Fatalf("a string is one value: %d", got)
	}
}

// The library message and the path (which carries the agent's keys) together stay within the cap.
func TestTheRefusalPathAndMessageAreCappedTogether(t *testing.T) {
	v, _ := newValidator(t)
	key := strings.Repeat("k", 200<<10)
	got := check(v, `{"additionalProperties":{"type":"object","additionalProperties":{"type":"string"}}}`, `{"`+key+`":{"x":1}}`)
	if got.OK {
		t.Fatal("not refused")
	}
	if n := utf8.RuneCountInString(got.Path) + 2 + utf8.RuneCountInString(got.Message); n > maxRefusalMessage {
		t.Fatalf("path %d + message %d characters", utf8.RuneCountInString(got.Path), utf8.RuneCountInString(got.Message))
	}
	if !utf8.ValidString(got.Path) || !strings.HasPrefix(got.Path, "/kkk") {
		t.Fatalf("path %.40q", got.Path)
	}
	short := check(v, `{"properties":{"a":{"type":"string"}}}`, `{"a":1}`)
	wantRefused(t, short, "/a")
}

// adversarial is one known hostile shape: a schema and arguments built to sit just under the
// budget (or at the arguments cap when the budget does not bite), and a ceiling on what one
// validation may allocate in total.
type adversarial struct {
	name, schema, args string
	ceiling            uint64
}

func budgetArgs(weight int, element string, elementValues int) string {
	n := (maxValidationWork/weight - 3) / elementValues
	if n < 1 {
		n = 1
	}
	if limit := (maxArgsBytes - 16) / (len(element) + 1); n > limit {
		n = limit
	}
	return `{"a":[` + strings.TrimSuffix(strings.Repeat(element+",", n), ",") + `]}`
}

func adversarialShapes(t *testing.T) []adversarial {
	t.Helper()
	weightOf := func(schema string) int {
		sch, err := compile([]byte(schema))
		if err != nil {
			return 1
		}
		_, w, err := expandedSize(sch)
		if err != nil {
			return 1
		}
		return w
	}
	const base = "https://gateway.invalid/"
	recursive := `{"type":"object","properties":{"a":{"anyOf":[{"type":"array","items":{"$ref":"#/properties/a"},"maxItems":0},{"type":"array","items":{"$ref":"#/properties/a"},"minItems":5}]}}}`
	dynamic := `{"properties":{"a":{"$ref":"` + base + `s"}},"$defs":{` +
		`"x":{"$dynamicAnchor":"m","anyOf":[{"type":"array","items":{"$ref":"` + base + `s"},"maxItems":0},{"type":"array","items":{"$ref":"` + base + `s"},"minItems":5}]},` +
		`"s":{"$id":"` + base + `s","$dynamicRef":"` + base + `t#m"},"t":{"$id":"` + base + `t","$dynamicAnchor":"m"}}}`
	deep := `{"a":` + strings.Repeat("[", 22) + "1" + strings.Repeat("]", 22) + `}`
	fan := fanInItems(10)
	enum := `{"type":"object","properties":{"a":{"type":"array","items":{"enum":[` + numberList(10_000) + `]}}}}`
	req := `{"type":"object","properties":{"a":{"type":"array","items":{"type":"object","required":[` + nameList(20_000) + `]}}}}`
	unique := `{"type":"object","properties":{"a":{"type":"array","uniqueItems":true}}}`
	var branches []string
	for i := 0; i < 25; i++ {
		branches = append(branches, fmt.Sprintf(`{"multipleOf":%de-400}`, 3+2*i))
	}
	multiple := `{"type":"object","properties":{"a":{"type":"array","items":{"allOf":[` + strings.Join(branches, ",") + `]}}}}`
	typeBranch := `{"items":{"type":["string","boolean","object","array","null"]}}`
	fanOut := `{"type":"object","properties":{"a":{"type":"array","allOf":[` + typeBranch + "," + typeBranch + "," + typeBranch + `]}}}`
	enum1k := `{"type":"object","properties":{"a":{"type":"array","items":{"enum":[` + numberList(1_000) + `]}}}}`
	// Ceilings: about four times the total allocation measured on an Apple M-series machine
	// (Follow-up 3 of the fix-wave report), at least 4 MiB, at most 256 MiB.
	return []adversarial{
		{"recursive anyOf", recursive, deep, 4 << 20},
		{"hidden dynamic anchor", dynamic, deep, 4 << 20},
		{"allOf fan-in", fan, budgetArgs(weightOf(fan), `"x"`, 1), 256 << 20},
		{"10k enum", enum, budgetArgs(weightOf(enum), "-1", 1), 4 << 20},
		{"1k enum", enum1k, budgetArgs(weightOf(enum1k), "-1", 1), 12 << 20},
		{"20k required", req, budgetArgs(weightOf(req), "{}", 1), 80 << 20},
		{"uniqueItems with colliding signs", unique, signPatterns(13, maxValidationWork/weightOf(unique)), 34 << 20},
		{"1e-400 multipleOf", multiple, budgetArgs(weightOf(multiple), "1e-400", 1+(6+400)/16), 8 << 20},
		{"failing type fan-out", fanOut, budgetArgs(weightOf(fanOut), "1", 1), 232 << 20},
	}
}

// Every known adversarial shape stays within a fixed allocation ceiling through the real
// validator. Total allocation tracks the library's work for these shapes and, unlike time, does
// not depend on the machine.
func TestKnownAdversarialShapesStayWithinTheirAllocationCeiling(t *testing.T) {
	for _, s := range adversarialShapes(t) {
		t.Run(s.name, func(t *testing.T) {
			v, _ := newValidator(t)
			v.entryFor("x.y", []byte(s.schema)) // compiled outside the measurement
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			check(v, s.schema, s.args)
			runtime.ReadMemStats(&after)
			if grew := after.TotalAlloc - before.TotalAlloc; grew > s.ceiling {
				t.Fatalf("allocated %d MiB, ceiling %d MiB", grew>>20, s.ceiling>>20)
			}
		})
	}
}
