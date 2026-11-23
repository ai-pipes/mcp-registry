package infra

import (
	"fmt"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const tooBigText = "the arguments contain a number that is too large or too precise to be checked"

func TestAPanicWhileApplyingASchemaIsOneWarningAndNotEnforced(t *testing.T) {
	v, w := newValidator(t)
	v.validateFn = func(*jsonschema.Schema, any) error { panic("boom: SECRET-ARGUMENT") }
	const s = `{"properties":{"a":{"type":"string"}}}`
	for i := 0; i < 3; i++ {
		if got := check(v, s, `{"a":1}`); !got.OK {
			t.Fatalf("call %d: %+v", i, got)
		}
	}
	got := w.all()
	if len(got) != 1 {
		t.Fatalf("warnings %d, want one", len(got))
	}
	if strings.Contains(fmt.Sprint(got[0].args...), "SECRET-ARGUMENT") {
		t.Fatalf("the warning carries the panic value: %v", got[0].args)
	}
	// Later calls are not enforced even when the library works again.
	v.validateFn = (*jsonschema.Schema).Validate
	if got := check(v, s, `{"a":1}`); !got.OK {
		t.Fatalf("a disabled schema must stay not enforced: %+v", got)
	}
}

func digits(n int) string { return "1" + strings.Repeat("0", n-1) }

// The bounds are 400 characters and a decimal exponent of 400 in magnitude, both inclusive.
func TestNumberBoundsAreExactOnBothSidesAndBothSigns(t *testing.T) {
	cases := []struct {
		literal string
		beyond  bool
	}{
		{"1e400", false},
		{"1e-400", false},
		{"1E+400", false},
		{"-1e400", false},
		{"1e401", true},
		{"1e-401", true},
		{"1E+401", true},
		{"-1e401", true},
		{"1e99999999999999999999", true},
		{digits(400), false},
		{digits(401), true},
		{"0." + strings.Repeat("0", 397) + "1", false}, // 400 characters
		{"0." + strings.Repeat("0", 398) + "1", true},  // 401 characters
	}
	for _, c := range cases {
		t.Run("schema "+c.literal[:min(14, len(c.literal))]+fmt.Sprint(len(c.literal)), func(t *testing.T) {
			v, w := newValidator(t)
			s := fmt.Sprintf(`{"properties":{"a":{"minimum":%s}}}`, c.literal)
			within(t, "check", func() { check(v, s, `{"a":0}`) })
			if warned := len(w.all()) == 1; warned != c.beyond {
				t.Fatalf("warned %v, want %v (warnings %v)", warned, c.beyond, w.all())
			}
		})
		t.Run("argument "+c.literal[:min(14, len(c.literal))]+fmt.Sprint(len(c.literal)), func(t *testing.T) {
			v, _ := newValidator(t)
			got := check(v, `{"properties":{"a":{"type":"number"}}}`, fmt.Sprintf(`{"a":%s}`, c.literal))
			if refused := !got.OK && got.Message == tooBigText; refused != c.beyond {
				t.Fatalf("%+v, beyond=%v", got, c.beyond)
			}
			if !c.beyond && !got.OK {
				t.Fatalf("a number inside the bounds is a number: %+v", got)
			}
		})
	}
}

func TestEscapesInStringsDoNotHideOrInventNumbers(t *testing.T) {
	v, _ := newValidator(t)
	const s = `{"type":"object"}`
	if got := check(v, s, `{"a":"\\","b":1e999}`); got.OK || got.Message != tooBigText {
		t.Fatalf("a backslash before the closing quote must not swallow the number after it: %+v", got)
	}
	for _, args := range []string{`{"a":"\"1e999"}`, `{"a":"\\\"1e999"}`, `{"a":"1e999"}`} {
		if got := check(v, s, args); !got.OK {
			t.Fatalf("%s: a number inside a string is text: %+v", args, got)
		}
	}
}

func TestMalformedNumbersTakeTheInvalidJSONPath(t *testing.T) {
	v, _ := newValidator(t)
	for _, args := range []string{
		`{"a":1e}`, `{"a":1e+}`, `{"a":--e+-}`, `{"a":-}`, `{"a":01}`, `{"a":1.}`, `{"a":1.e5}`,
		`{"a":` + digits(200000) + `e}`,
	} {
		within(t, args[:min(12, len(args))], func() {
			got := check(v, `{"type":"object"}`, args)
			if got.OK || got.Path != "/" || got.Message != "the arguments are not valid JSON" {
				t.Fatalf("%.30s: %+v", args, got)
			}
		})
	}
	for _, lit := range []string{"0", "-0", "1.5", "-12.5e+3", "0.0e-0", "10E2"} {
		if !wellFormedNumber(lit) {
			t.Errorf("%s is a JSON number", lit)
		}
	}
}

// Arguments over maxArgsBytes are not validated; the worst shapes just below it still are (no
// wall-clock assertion: the bound is what keeps one check small, see maxArgsBytes).
func TestOversizedArgumentsAreNotValidatedAndTheWorstShapesBelowAre(t *testing.T) {
	v, w := newValidator(t)
	const arraySchema = `{"properties":{"a":{"type":"array","items":{"type":"integer","minimum":0}}}}`
	const objectSchema = `{"additionalProperties":{"type":"string"}}`

	// A single big string keeps working, validated or not.
	bigText := `{"text":"` + strings.Repeat("x", 5<<20) + `"}`
	if got := check(v, `{"properties":{"text":{"type":"string"}},"required":["text"]}`, bigText); !got.OK {
		t.Fatalf("%+v", got)
	}

	// Over the bound: not validated, no warning.
	over := `{"a":[` + strings.Repeat("-1,", maxArgsBytes/3) + `-1]}`
	if len(over) <= maxArgsBytes {
		t.Fatal("the test body is not over the bound")
	}
	if got := check(v, arraySchema, over); !got.OK {
		t.Fatalf("an oversized body must go on to the upstream: %+v", got)
	}
	if len(w.all()) != 0 {
		t.Fatalf("warnings %v: a warning per oversized call is a log amplifier", w.all())
	}

	// Just under the bound the work budget decides: 131 000 values are over it (not validated,
	// silently), a few hundred are validated, failing late.
	under := `{"a":[` + strings.Repeat("1,", (maxArgsBytes-30)/2) + `-1]}`
	if len(under) > maxArgsBytes {
		t.Fatal("the test body is over the bound")
	}
	if got := check(v, arraySchema, under); !got.OK {
		t.Fatalf("a body over the work budget must go on to the upstream: %+v", got)
	}
	wantRefused(t, check(v, arraySchema, `{"a":[`+strings.Repeat("1,", 500)+`-1]}`), "/a/500")
	var sb strings.Builder
	sb.WriteString(`{`)
	for i := 0; sb.Len() < maxArgsBytes-40; i++ {
		fmt.Fprintf(&sb, `"k%d":"v",`, i)
	}
	sb.WriteString(`"z":"v"}`)
	if got := check(v, objectSchema, sb.String()); !got.OK {
		t.Fatalf("%+v", got)
	}
}
