package infra

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A marker stands for an agent's own value or key: it must never come back in a refusal text.
const marker = "MARKER-31337"

func TestNoErrorKindEchoesAnInstanceValueOrKey(t *testing.T) {
	m := fmt.Sprintf("%q", marker)
	cases := []struct{ name, schema, args string }{
		{"minimum", `{"properties":{"a":{"minimum":99999}}}`, `{"a":31337}`},
		{"maximum", `{"properties":{"a":{"maximum":1}}}`, `{"a":31337}`},
		{"exclusiveMinimum", `{"properties":{"a":{"exclusiveMinimum":99999}}}`, `{"a":31337}`},
		{"exclusiveMaximum", `{"properties":{"a":{"exclusiveMaximum":1}}}`, `{"a":31337}`},
		{"multipleOf", `{"properties":{"a":{"multipleOf":2}}}`, `{"a":31337}`},
		{"minimum huge", `{"properties":{"a":{"minimum":1e30}}}`, `{"a":31337}`},
		{"maximum huge", `{"properties":{"a":{"maximum":1}}}`, `{"a":31337000000000000000001}`},
		{"enum", `{"properties":{"a":{"enum":["x","y"]}}}`, `{"a":` + m + `}`},
		{"enum number", `{"properties":{"a":{"enum":[1,2]}}}`, `{"a":31337}`},
		{"const", `{"properties":{"a":{"const":"x"}}}`, `{"a":` + m + `}`},
		{"const object", `{"properties":{"a":{"const":{"k":1}}}}`, `{"a":{"` + marker + `":1}}`},
		{"minLength", `{"properties":{"a":{"minLength":99}}}`, `{"a":` + m + `}`},
		{"maxLength", `{"properties":{"a":{"maxLength":1}}}`, `{"a":` + m + `}`},
		{"minItems", `{"properties":{"a":{"minItems":3}}}`, `{"a":[` + m + `]}`},
		{"maxItems", `{"properties":{"a":{"maxItems":0}}}`, `{"a":[` + m + `]}`},
		{"uniqueItems", `{"properties":{"a":{"uniqueItems":true}}}`, `{"a":[` + m + `,` + m + `]}`},
		{"minProperties", `{"properties":{"a":{"minProperties":3}}}`, `{"a":{"` + marker + `":1}}`},
		{"maxProperties", `{"properties":{"a":{"maxProperties":0}}}`, `{"a":{"` + marker + `":1}}`},
		{"contains", `{"properties":{"a":{"contains":{"type":"number"}}}}`, `{"a":[` + m + `]}`},
		{"minContains", `{"properties":{"a":{"contains":{"type":"number"},"minContains":2}}}`, `{"a":[1,` + m + `]}`},
		{"maxContains", `{"properties":{"a":{"contains":{"type":"number"},"maxContains":1}}}`, `{"a":[1,2,` + m + `]}`},
		{"required", `{"required":["need"]}`, `{"` + marker + `":1}`},
		{"dependentRequired", `{"dependentRequired":{"a":["b"]}}`, `{"a":` + m + `}`},
		{"dependencies draft7", `{"$schema":"http://json-schema.org/draft-07/schema#","dependencies":{"a":["b"]}}`, `{"a":` + m + `}`},
		{"type", `{"properties":{"a":{"type":"number"}}}`, `{"a":` + m + `}`},
		{"type list", `{"properties":{"a":{"type":["number","null"]}}}`, `{"a":` + m + `}`},
		{"propertyNames", `{"propertyNames":{"maxLength":2}}`, `{"` + marker + `":1}`},
		{"propertyNames const", `{"propertyNames":{"const":"x"}}`, `{"` + marker + `":1}`},
		{"additionalProperties false", `{"additionalProperties":false}`, `{"` + marker + `":1}`},
		{"additionalProperties schema", `{"additionalProperties":{"type":"number"}}`, `{"k":` + m + `}`},
		{"unevaluatedProperties", `{"unevaluatedProperties":false}`, `{"` + marker + `":1}`},
		{"unevaluatedItems", `{"properties":{"a":{"unevaluatedItems":false}}}`, `{"a":[` + m + `]}`},
		{"additionalItems draft7", `{"$schema":"http://json-schema.org/draft-07/schema#","properties":{"a":{"items":[{}],"additionalItems":false}}}`, `{"a":[1,` + m + `]}`},
		{"property false schema", `{"properties":{"a":false}}`, `{"a":` + m + `}`},
		{"items false", `{"properties":{"a":{"items":false}}}`, `{"a":[` + m + `]}`},
		{"format", `{"properties":{"a":{"format":"email"}}}`, `{"a":` + m + `}`},
		{"format ipv4", `{"properties":{"a":{"format":"ipv4"}}}`, `{"a":` + m + `}`},
		{"anyOf", `{"properties":{"a":{"anyOf":[{"type":"number"},{"type":"boolean"}]}}}`, `{"a":` + m + `}`},
		{"oneOf", `{"properties":{"a":{"oneOf":[{"type":"number"},{"minimum":0}]}}}`, `{"a":31337}`},
		{"allOf", `{"properties":{"a":{"allOf":[{"type":"string"},{"minLength":99}]}}}`, `{"a":` + m + `}`},
		{"not", `{"properties":{"a":{"not":{"type":"string"}}}}`, `{"a":` + m + `}`},
		{"if then", `{"if":{"properties":{"a":{"const":` + m + `}}},"then":{"required":["z"]}}`, `{"a":` + m + `}`},
		{"false root", `{"not":{}}`, `{"` + marker + `":1}`},
	}
	v, _ := newValidator(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := check(v, c.schema, c.args)
			if got.OK {
				t.Fatalf("expected a refusal for %s", c.args)
			}
			for _, bad := range []string{marker, "31337", "31,337", "3.1337", "31337000"} {
				if strings.Contains(got.Message, bad) && !strings.Contains(c.schema, bad) {
					t.Fatalf("message %q repeats %q from the arguments", got.Message, bad)
				}
			}
		})
	}
}

func TestNumericLimitsAreWrittenFromTheSchemaOnlyAndExactly(t *testing.T) {
	v, _ := newValidator(t)
	for _, c := range []struct{ schema, args, want string }{
		{`{"properties":{"a":{"minimum":10}}}`, `{"a":3}`, "must be >= 10"},
		{`{"properties":{"a":{"maximum":10}}}`, `{"a":31337}`, "must be <= 10"},
		{`{"properties":{"a":{"exclusiveMinimum":10}}}`, `{"a":10}`, "must be > 10"},
		{`{"properties":{"a":{"exclusiveMaximum":10}}}`, `{"a":10}`, "must be < 10"},
		{`{"properties":{"a":{"multipleOf":0.5}}}`, `{"a":31337.25}`, "must be a multiple of 0.5"},
		{`{"properties":{"a":{"multipleOf":5}}}`, `{"a":31337}`, "must be a multiple of 5"},
		{`{"properties":{"a":{"maximum":9007199254740993}}}`, `{"a":9007199254740994}`, "must be <= 9007199254740993"},
		{`{"properties":{"a":{"minimum":0.1}}}`, `{"a":0}`, "must be >= 0.1"},
		{`{"properties":{"a":{"minimum":-1234567.125}}}`, `{"a":-9999999}`, "must be >= -1234567.125"},
		{`{"properties":{"a":{"minimum":1e30}}}`, `{"a":0}`, "must be >= 1000000000000000000000000000000"},
	} {
		got := check(v, c.schema, c.args)
		if got.OK || got.Message != c.want {
			t.Errorf("%s with %s: %+v, want message %q", c.schema, c.args, got, c.want)
		}
	}
}

func TestUnevaluatedPropertiesReadsLikeAnAdditionalProperty(t *testing.T) {
	v, _ := newValidator(t)
	got := check(v, `{"unevaluatedProperties":false}`, `{"k":1}`)
	want := check(v, `{"additionalProperties":false}`, `{"k":1}`)
	if got.OK || got.Message != want.Message || got.Message == "false schema" {
		t.Fatalf("got %+v, want the additionalProperties wording %q", got, want.Message)
	}
}

func TestOnlyTheFormatsOfTheReferenceAreChecked(t *testing.T) {
	v, w := newValidator(t)
	for _, f := range []string{"iri", "iri-reference", "period", "semver", "regex"} {
		s := fmt.Sprintf(`{"properties":{"a":{"format":%q}}}`, f)
		for _, bad := range []string{`"not valid at all ((("`, `"(?=lookahead)\\1"`, `"1.2"`, `"x y"`} {
			if got := check(v, s, `{"a":`+bad+`}`); !got.OK {
				t.Errorf("format %s refused %s: %+v (the reference does not check it)", f, bad, got)
			}
		}
	}
	// The set of the reference is still enforced, in every dialect.
	for _, f := range []string{"date", "date-time", "time", "email", "hostname", "ipv4", "ipv6", "uri", "uri-reference", "uri-template", "uuid", "duration", "json-pointer", "relative-json-pointer"} {
		for _, dialect := range []string{"", `"$schema":"https://json-schema.org/draft/2019-09/schema",`, `"$schema":"http://json-schema.org/draft-07/schema#",`} {
			s := fmt.Sprintf(`{%s"properties":{"a":{"format":%q}}}`, dialect, f)
			if got := check(v, s, `{"a":"%%%"}`); got.OK {
				t.Errorf("format %s (%s) accepted garbage", f, dialect)
			}
		}
	}
	if len(w.all()) != 0 {
		t.Fatalf("warnings %+v", w.all())
	}
}

func TestReferenceCyclesAreNotEnforcedAndNeverNameTheBase(t *testing.T) {
	for name, s := range map[string]string{
		"self ref": `{"$ref":"#"}`,
		"a b a":    `{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}},"properties":{"x":{"$ref":"#/$defs/a"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			v, w := newValidator(t)
			for i := 0; i < 3; i++ {
				got := check(v, s, `{"x":{"x":1}}`)
				if !got.OK || strings.Contains(got.Message, "gateway.invalid") {
					t.Fatalf("got %+v", got)
				}
			}
			if n := len(w.all()); n != 1 {
				t.Fatalf("warnings %d, want one", n)
			}
			for _, a := range w.all()[0].args {
				if str, ok := a.(string); ok && strings.Contains(str, "gateway.invalid") {
					t.Fatalf("the warning names the internal base: %v", w.all()[0].args)
				}
			}
		})
	}
}

// A recursive schema that terminates is still enforced.
func TestRecursiveSchemasThatTerminateAreEnforced(t *testing.T) {
	v, _ := newValidator(t)
	const s = `{"$defs":{"node":{"type":"object","properties":{"child":{"$ref":"#/$defs/node"},"n":{"type":"number"}}}},"$ref":"#/$defs/node"}`
	wantRefused(t, check(v, s, `{"child":{"child":{"n":"x"}}}`), "/child/child/n")
	if got := check(v, s, `{"child":{"child":{"n":1}}}`); !got.OK {
		t.Fatal(got)
	}
}

func nested(depth int) string {
	return strings.Repeat(`{"type":"object","properties":{"a":`, depth) + `{"type":"string"}` + strings.Repeat(`}}`, depth)
}

func TestSchemasBeyondTheBoundsAreNotEnforcedAndWarnOnce(t *testing.T) {
	deep := nested(maxSchemaDepth) // 2 JSON levels per step: well over the bound
	big := `{"description":"` + strings.Repeat("x", maxSchemaBytes) + `"}`
	for name, s := range map[string]string{"deep": deep, "big": big} {
		t.Run(name, func(t *testing.T) {
			v, w := newValidator(t)
			for i := 0; i < 3; i++ {
				if got := check(v, s, `{"a":{"a":1}}`); !got.OK {
					t.Fatalf("got %+v", got)
				}
			}
			if n := len(w.all()); n != 1 {
				t.Fatalf("warnings %d, want one", n)
			}
		})
	}
	// Just inside the bounds is enforced.
	v, _ := newValidator(t)
	wantRefused(t, check(v, nested(maxSchemaDepth/2-2), strings.Repeat(`{"a":`, maxSchemaDepth/2-2)+`1`+strings.Repeat(`}`, maxSchemaDepth/2-2)), pathOf(maxSchemaDepth/2-2))
}

func pathOf(n int) string { return strings.Repeat("/a", n) }

func TestARealisticBigSchemaCompilesFast(t *testing.T) {
	v, _ := newValidator(t)
	var sb strings.Builder
	sb.WriteString(`{"type":"object","properties":{`)
	for i := 0; i < 2000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"p%d":{"type":"string","description":"text"}`, i)
	}
	sb.WriteString(`}}`)
	if sb.Len() >= maxSchemaBytes {
		t.Fatalf("test schema is %d bytes, over the bound", sb.Len())
	}
	wantRefused(t, check(v, sb.String(), `{"p7":1}`), "/p7")
}

func TestASlowCompileDoesNotDelayOtherSchemas(t *testing.T) {
	v, _ := newValidator(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	v.compileFn = func(raw json.RawMessage) (*jsonschema.Schema, error) {
		if strings.Contains(string(raw), "SLOW") {
			once.Do(func() { close(started) })
			<-release
		}
		return compile(raw)
	}
	const fast = `{"properties":{"b":{"type":"string"}}}`
	wantRefused(t, check(v, fast, `{"b":1}`), "/b") // cached

	done := make(chan struct{})
	go func() {
		defer close(done)
		check(v, `{"description":"SLOW","properties":{"a":{"type":"string"}}}`, `{"a":1}`)
	}()
	<-started
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		wantRefused(t, check(v, fast, `{"b":1}`), "/b")                                     // cached
		wantRefused(t, check(v, `{"properties":{"c":{"type":"string"}}}`, `{"c":1}`), "/c") // new, other schema
	}()
	<-finished // would deadlock (test timeout) if compilation held the lock
	close(release)
	<-done
}

func TestConcurrentFirstUsesOfOneSchemaCompileOnce(t *testing.T) {
	v, _ := newValidator(t)
	var compiles atomic.Int32
	gate := make(chan struct{})
	v.compileFn = func(raw json.RawMessage) (*jsonschema.Schema, error) {
		compiles.Add(1)
		<-gate
		return compile(raw)
	}
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wantRefused(t, check(v, mainSchema, `{"title":7}`), "/title")
		}()
	}
	// Everyone is either compiling or waiting for the one compile.
	for int(v.waiters.Load()) < n-1 {
		runtimeGosched()
	}
	close(gate)
	wg.Wait()
	if compiles.Load() != 1 || v.compiledCount != 1 {
		t.Fatalf("compiled %d times (count %d), want once", compiles.Load(), v.compiledCount)
	}
}

func runtimeGosched() { runtime.Gosched() }
