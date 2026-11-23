package infra

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// within fails the test when f takes longer than a generous bound: a schema or an argument must
// never make one check expensive.
func within(t *testing.T, name string, f func()) {
	t.Helper()
	start := time.Now()
	f()
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("%s took %s", name, d)
	}
}

func TestAnUnevaluatedPropertyIsRefusedAtItsParentAndNeverNamesTheKey(t *testing.T) {
	v, _ := newValidator(t)
	for name, c := range map[string]struct{ schema, args, path string }{
		"unevaluatedProperties":         {`{"unevaluatedProperties":false}`, `{"a":1,"` + marker + `":2}`, "/"},
		"nested unevaluated":            {`{"properties":{"nested":{"unevaluatedProperties":false}}}`, `{"nested":{"` + marker + `":2}}`, "/nested"},
		"additionalProperties":          {`{"additionalProperties":false}`, `{"a":1,"` + marker + `":2}`, "/"},
		"nested additionalProperties":   {`{"properties":{"nested":{"additionalProperties":false}}}`, `{"nested":{"` + marker + `":2}}`, "/nested"},
		"unevaluated with properties":   {`{"properties":{"a":{}},"unevaluatedProperties":false}`, `{"a":1,"` + marker + `":2}`, "/"},
		"unevaluated in 2019-09":        {`{"$schema":"https://json-schema.org/draft/2019-09/schema","unevaluatedProperties":false}`, `{"` + marker + `":2}`, "/"},
		"unevaluated behind an allOf":   {`{"allOf":[{"properties":{"a":{}}}],"unevaluatedProperties":false}`, `{"a":1,"` + marker + `":2}`, "/"},
		"unevaluated with a type error": {`{"properties":{"t":{"type":"string"}},"unevaluatedProperties":false}`, `{"` + marker + `":2}`, "/"},
	} {
		t.Run(name, func(t *testing.T) {
			got := check(v, c.schema, c.args)
			wantRefused(t, got, c.path)
			if text := got.Path + " " + got.Message; strings.Contains(text, marker) {
				t.Fatalf("the refusal %q names the agent's key", text)
			}
		})
	}
}

func TestHugeSchemaNumbersAreNotEnforcedAndCostNothing(t *testing.T) {
	for _, kw := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "maxLength", "minItems"} {
		for _, num := range []string{"1e9999999", "1e-9999999", "-1e9999999", "1" + strings.Repeat("0", 300000)} {
			name := kw + " " + num[:min(12, len(num))]
			t.Run(name, func(t *testing.T) {
				v, w := newValidator(t)
				s := fmt.Sprintf(`{"properties":{"a":{%q:%s}}}`, kw, num)
				within(t, "first use", func() {
					if got := check(v, s, `{"a":5}`); !got.OK {
						t.Fatalf("got %+v", got)
					}
				})
				within(t, "second use", func() { check(v, s, `{"a":"x"}`) })
				if n := len(w.all()); n != 1 {
					t.Fatalf("warnings %d, want one", n)
				}
			})
		}
	}
	t.Run("const and enum numbers", func(t *testing.T) {
		v, w := newValidator(t)
		within(t, "const", func() { check(v, `{"properties":{"a":{"const":1e9999999}}}`, `{"a":1}`) })
		within(t, "enum", func() { check(v, `{"properties":{"a":{"enum":[1,1e9999999]}}}`, `{"a":1}`) })
		if len(w.all()) != 2 {
			t.Fatalf("warnings %d", len(w.all()))
		}
	})
}

func TestHugeArgumentNumbersAreRefusedCheaplyWithoutEchoingThem(t *testing.T) {
	v, _ := newValidator(t)
	schemas := []string{
		`{"properties":{"a":{"multipleOf":3}}}`,
		`{"properties":{"a":{"minimum":5}}}`,
		`{"properties":{"a":{"maximum":5}}}`,
		`{"properties":{"a":{"type":"integer"}}}`,
		`{"properties":{"a":{"const":1}}}`,
		`{"properties":{"a":{"enum":[1,2]}}}`,
		`{"properties":{"a":{"exclusiveMinimum":0.5}}}`,
	}
	args := []string{
		`{"a":1e9999999}`, `{"a":-1e9999999}`, `{"a":1e-9999999}`, `{"a":[1e9999999]}`,
		`{"a":1` + strings.Repeat("0", 5<<20) + `}`,            // 5 MiB of digits
		`{"a":0.` + strings.Repeat("1", 5<<20) + `}`,           // 5 MiB of decimals
		`{"a":[` + strings.Repeat("1e999999,", 100000) + `1]}`, // many huge literals
	}
	for _, s := range schemas {
		for _, a := range args {
			within(t, s+" "+a[:min(20, len(a))], func() {
				got := check(v, s, a)
				if len(a) > maxArgsBytes { // not validated at all (see maxArgsBytes): cheap and accepted
					if !got.OK {
						t.Fatalf("an oversized body is not validated: %+v", got)
					}
					return
				}
				if got.OK {
					t.Fatalf("an argument number far beyond any double was accepted for %s", s)
				}
				if strings.Contains(got.Message, "9999999") || strings.Contains(got.Path, "9999999") || len(got.Message) > 200 {
					t.Fatalf("message %q echoes the number", got.Message)
				}
			})
		}
	}
}

func TestNumbersWithinTheBoundsAreStillJudgedExactly(t *testing.T) {
	v, _ := newValidator(t)
	wantRefused(t, check(v, `{"properties":{"a":{"exclusiveMinimum":1e300}}}`, `{"a":1e300}`), "/a")
	if got := check(v, `{"properties":{"a":{"minimum":1e300}}}`, `{"a":1e300}`); !got.OK {
		t.Fatalf("%+v", got)
	}
	wantRefused(t, check(v, `{"properties":{"a":{"maximum":1e300}}}`, `{"a":1e301}`), "/a")
	if got := check(v, `{"properties":{"a":{"type":"integer"}}}`, `{"a":1e300}`); !got.OK {
		t.Fatalf("%+v", got)
	}
	if got := check(v, `{"properties":{"a":{"minimum":-1e300}}}`, `{"a":-123456789012345678901234567890}`); !got.OK {
		t.Fatalf("%+v", got)
	}
}

func TestANumberOfTheSchemaIsRenderedWithinABound(t *testing.T) {
	v, _ := newValidator(t)
	for _, c := range []struct{ schema, args, want string }{
		{`{"properties":{"a":{"minimum":1e100}}}`, `{"a":0}`, "must be >= the schema's minimum"},
		{`{"properties":{"a":{"maximum":-1e100}}}`, `{"a":0}`, "must be <= the schema's maximum"},
		{`{"properties":{"a":{"exclusiveMinimum":1e100}}}`, `{"a":0}`, "must be > the schema's exclusive minimum"},
		{`{"properties":{"a":{"exclusiveMaximum":-1e100}}}`, `{"a":0}`, "must be < the schema's exclusive maximum"},
		{`{"properties":{"a":{"minimum":0.1234567890123456789012345678901234567890123456789012345678901234567890123456789}}}`, `{"a":0}`, "must be >= the schema's minimum"},
		{`{"properties":{"a":{"minimum":1e30}}}`, `{"a":0}`, "must be >= 1000000000000000000000000000000"},
		{`{"properties":{"a":{"minimum":0.0625}}}`, `{"a":0}`, "must be >= 0.0625"},
	} {
		got := check(v, c.schema, c.args)
		if got.OK || got.Message != c.want {
			t.Errorf("%s: %+v, want %q", c.schema, got, c.want)
		}
	}
}

func TestAPanicInACompileIsOneWarningAndNotEnforced(t *testing.T) {
	v, w := newValidator(t)
	v.compileFn = func(json.RawMessage) (*jsonschema.Schema, error) { panic("boom: SECRET-SCHEMA-TEXT") }
	for i := 0; i < 3; i++ {
		if got := check(v, `{"properties":{"a":{"type":"string"}}}`, `{"a":1}`); !got.OK {
			t.Fatalf("got %+v", got)
		}
	}
	got := w.all()
	if len(got) != 1 {
		t.Fatalf("warnings %d, want one", len(got))
	}
	if strings.Contains(fmt.Sprint(got[0].args...), "SECRET-SCHEMA-TEXT") {
		t.Fatalf("the warning carries the panic value: %v", got[0].args)
	}
}
