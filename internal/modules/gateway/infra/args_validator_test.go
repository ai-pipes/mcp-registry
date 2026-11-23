package infra

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
)

type warning struct {
	msg  string
	args []any
}

type warnRecorder struct {
	mu   sync.Mutex
	list []warning
}

func (w *warnRecorder) Warn(msg string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list = append(w.list, warning{msg, args})
}

func (w *warnRecorder) all() []warning {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]warning(nil), w.list...)
}

func newValidator(t *testing.T) (*ArgsValidator, *warnRecorder) {
	t.Helper()
	w := &warnRecorder{}
	return NewArgsValidator(w), w
}

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mainSchema is S of ajv-args-validator.test.ts.
const mainSchema = `{"type":"object","properties":{"title":{"type":"string"},"labels":{"type":"array","items":{"type":"integer"}},
"meta":{"type":"object","properties":{"due date":{"type":"string","format":"date"}}}},"required":["title"],"additionalProperties":false}`

func check(v *ArgsValidator, schema, args string) app.ArgsCheck {
	return v.Validate("gitea.create_issue", json.RawMessage(schema), json.RawMessage(args))
}

func wantRefused(t *testing.T, got app.ArgsCheck, path string) {
	t.Helper()
	if got.OK || got.Path != path || got.Message == "" {
		t.Fatalf("got %+v, want a refusal at %q with a message", got, path)
	}
}

func TestRefTest1ValidArgumentsPass(t *testing.T) {
	v, _ := newValidator(t)
	if got := check(v, mainSchema, `{"title":"Bug","labels":[1,2]}`); !got.OK {
		t.Fatalf("got %+v", got)
	}
}

func TestRefTest2TypeErrorsNameTheProperty(t *testing.T) {
	v, _ := newValidator(t)
	wantRefused(t, check(v, mainSchema, `{"title":7}`), "/title")
	wantRefused(t, check(v, mainSchema, `{"title":"x","labels":[1,"two"]}`), "/labels/1")
}

func TestRefTest3RequiredAndAdditionalAreReportedAtTheObject(t *testing.T) {
	v, _ := newValidator(t)
	got := check(v, mainSchema, `{}`)
	wantRefused(t, got, "/")
	if !strings.Contains(got.Message, "title") {
		t.Fatalf("the message should name the missing property: %q", got.Message)
	}
	wantRefused(t, check(v, mainSchema, `{"title":"x","extra":1}`), "/")
}

func TestRefTest4FormatErrorAtAPropertyWithASpace(t *testing.T) {
	v, _ := newValidator(t)
	got := check(v, mainSchema, `{"title":"x","meta":{"due date":"soon"}}`)
	wantRefused(t, got, "/meta/due date")
	if strings.Contains(got.Message, "soon") {
		t.Fatalf("the message must not echo the argument: %q", got.Message)
	}
}

func TestRefTest5Draft07(t *testing.T) {
	v, _ := newValidator(t)
	const s = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"n":{"type":"number"}}}`
	if got := check(v, s, `{"n":1}`); !got.OK {
		t.Fatalf("got %+v", got)
	}
	wantRefused(t, check(v, s, `{"n":"x"}`), "/n")
}

func TestRefTest6CacheCompilesOncePerSchema(t *testing.T) {
	v, _ := newValidator(t)
	for i := 0; i < 3; i++ {
		// A different spelling of the same JSON must hit the same entry.
		s := mainSchema
		if i == 1 {
			s = strings.ReplaceAll(mainSchema, "\n", " ")
		}
		if got := check(v, s, `{"title":"x"}`); !got.OK {
			t.Fatal(got)
		}
	}
	if v.compiledCount != 1 {
		t.Fatalf("compiled %d times, want 1", v.compiledCount)
	}
	check(v, `{"type":"object","properties":{"a":{"type":"string"}}}`, `{}`)
	if v.compiledCount != 2 {
		t.Fatalf("compiled %d times, want 2", v.compiledCount)
	}
}

func TestRefTest7UnknownDialectIsNotEnforcedAndWarnsOnce(t *testing.T) {
	v, w := newValidator(t)
	const s = `{"$schema":"https://example.com/my-dialect","type":"object"}`
	for _, args := range []string{`{"anything":"secret arg"}`, `{"anything":1}`} {
		if got := check(v, s, args); !got.OK {
			t.Fatalf("%s: %+v", args, got)
		}
	}
	got := w.all()
	if len(got) != 1 || got[0].msg != "gateway: tool input schema not compiled, not validating" {
		t.Fatalf("warnings %+v", got)
	}
	fields := fmt.Sprint(got[0].args...)
	if !strings.Contains(fields, "gitea") || !strings.Contains(fields, "create_issue") || strings.Contains(fields, "secret arg") {
		t.Fatalf("fields %v", got[0].args)
	}
	hasErr := false
	for i := 0; i+1 < len(got[0].args); i += 2 {
		if got[0].args[i] == "error" {
			_, hasErr = got[0].args[i+1].(string)
		}
	}
	if !hasErr {
		t.Fatalf("no string error field: %v", got[0].args)
	}
}

func TestRefTest8RegexKeywordsAreNotRun(t *testing.T) {
	v, _ := newValidator(t)
	const s = `{"type":"object","properties":{"name":{"type":"string","pattern":"^(a+)+$"}},"patternProperties":{"^(x+)+y$":{"type":"integer"}},"additionalProperties":false}`
	start := time.Now()
	args := fmt.Sprintf(`{"name":%q,%q:"z"}`, strings.Repeat("a", 40)+"!", strings.Repeat("x", 40))
	if got := check(v, s, args); !got.OK {
		t.Fatalf("got %+v", got)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("took %s", d)
	}
	wantRefused(t, check(v, s, `{"name":5}`), "/name")
}

func TestRefTest9OnlySchemaPositionsAreStripped(t *testing.T) {
	v, _ := newValidator(t)
	s := `{"type":"object","properties":{"pattern":{"type":"string"},"patternProperties":{"type":"integer"},
"nested":{"type":"array","items":{"anyOf":[{"type":"string","pattern":"^(a+)+$"}]}}},"required":["pattern"],
"$defs":{"d":{"type":"string","pattern":"^(a+)+$"}}}`
	before := string(append(json.RawMessage(nil), s...))
	wantRefused(t, check(v, s, `{}`), "/")
	wantRefused(t, check(v, s, `{"pattern":1}`), "/pattern")
	wantRefused(t, check(v, s, `{"pattern":"x","patternProperties":"y"}`), "/patternProperties")
	if got := check(v, s, fmt.Sprintf(`{"pattern":"x","nested":[%q]}`, strings.Repeat("a", 40)+"!")); !got.OK {
		t.Fatalf("got %+v", got)
	}
	if s != before {
		t.Fatal("the input schema was modified")
	}
}

func TestRefTest10TopLevelIdIsRemovedSoSchemasDoNotCollide(t *testing.T) {
	v, _ := newValidator(t)
	const v1 = `{"$id":"urn:tool","type":"object","properties":{"n":{"type":"string"}}}`
	const v2 = `{"$id":"urn:tool","type":"object","properties":{"n":{"type":"number"}}}`
	wantRefused(t, v.Validate("a.one", json.RawMessage(v1), json.RawMessage(`{"n":1}`)), "/n")
	if got := v.Validate("a.one", json.RawMessage(v2), json.RawMessage(`{"n":1}`)); !got.OK {
		t.Fatalf("got %+v", got)
	}
	wantRefused(t, v.Validate("other.tool", json.RawMessage(v2), json.RawMessage(`{"n":"x"}`)), "/n")
}

func TestRefTest11PropertyNamesContainingMust(t *testing.T) {
	v, _ := newValidator(t)
	const s = `{"type":"object","properties":{"x must y":{"type":"string"}}}`
	// The reference cuts the path at the first " must" (a known parse limit, so path /x). The Go
	// validator reports the real pointer.
	wantRefused(t, check(v, s, `{"x must y":1}`), "/x must y")
}

func TestUncompilableSchemasAreNotEnforced(t *testing.T) {
	for name, s := range map[string]string{
		"type nonsense":       `{"type":"object","properties":{"x":{"type":"nonsense"}}}`,
		"missing ref":         `{"type":"object","properties":{"x":{"$ref":"#/definitions/missing"}}}`,
		"remote ref":          `{"type":"object","properties":{"x":{"$ref":"https://example.com/x.json"}}}`,
		"file ref":            `{"type":"object","properties":{"x":{"$ref":"file:///etc/passwd"}}}`,
		"required not array":  `{"type":"object","required":"a"}`,
		"boolean root":        `true`,
		"not json":            `{"type":`,
		"draft-04":            `{"$schema":"http://json-schema.org/draft-04/schema#","type":"object"}`,
		"minLength not a num": `{"type":"object","properties":{"a":{"minLength":"x"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			v, w := newValidator(t)
			if got := check(v, s, `{"x":[1,"a"]}`); !got.OK {
				t.Fatalf("got %+v", got)
			}
			if got := check(v, s, `{"x":2}`); !got.OK {
				t.Fatalf("got %+v", got)
			}
			if len(w.all()) != 1 {
				t.Fatalf("warnings %+v", w.all())
			}
		})
	}
}

func TestWarningNamesTheFirstToolAndHasNoArgumentsAndIsBounded(t *testing.T) {
	v, w := newValidator(t)
	s := `{"type":"object","properties":{"x":{"$ref":"#/definitions/` + strings.Repeat("m", 900) + `"}}}`
	v.Validate("srv.first", json.RawMessage(s), json.RawMessage(`{"arg":"MARKER"}`))
	v.Validate("srv.second", json.RawMessage(s), json.RawMessage(`{"arg":"MARKER"}`))
	got := w.all()
	if len(got) != 1 {
		t.Fatalf("warnings %+v", got)
	}
	fields := fmt.Sprint(got[0].args...)
	if !strings.Contains(fields, "first") || strings.Contains(fields, "second") || strings.Contains(fields, "MARKER") {
		t.Fatalf("fields %v", got[0].args)
	}
	for i := 0; i+1 < len(got[0].args); i += 2 {
		if got[0].args[i] == "error" {
			if n := len([]rune(got[0].args[i+1].(string))); n > 500 {
				t.Fatalf("error text has %d characters", n)
			}
		}
	}
}

func TestNoDotRefIsLoggedAsToolOnly(t *testing.T) {
	v, w := newValidator(t)
	v.Validate("nodot", json.RawMessage(`{"type":"nonsense"}`), json.RawMessage(`{}`))
	got := w.all()
	if len(got) != 1 {
		t.Fatal(got)
	}
	want := []any{"server", "", "tool", "nodot"}
	for i, a := range want {
		if got[0].args[i] != a {
			t.Fatalf("args %v", got[0].args)
		}
	}
}

func TestDialects(t *testing.T) {
	v, w := newValidator(t)
	cases := []struct{ name, schema, bad string }{
		{"default 2020-12", `{"type":"object","properties":{"a":{"type":"string"}}}`, `{"a":1}`},
		{"2020-12 https", `{"$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"a":{"type":"string"}}}`, `{"a":1}`},
		{"2020-12 http hash", `{"$schema":"http://json-schema.org/draft/2020-12/schema#","properties":{"a":{"type":"string"}}}`, `{"a":1}`},
		{"2019-09", `{"$schema":"https://json-schema.org/draft/2019-09/schema","properties":{"a":{"type":"string"}}}`, `{"a":1}`},
		{"draft-07 without hash", `{"$schema":"http://json-schema.org/draft-07/schema","properties":{"a":{"type":"string"}}}`, `{"a":1}`},
		{"draft-06", `{"$schema":"http://json-schema.org/draft-06/schema#","properties":{"a":{"type":"string"}}}`, `{"a":1}`},
		{"non-string $schema is 2020-12", `{"$schema":5,"properties":{"a":{"type":"string"}}}`, `{"a":1}`},
		{"draft-07 tuple items", `{"$schema":"http://json-schema.org/draft-07/schema#","properties":{"a":{"items":[{"type":"string"},{"type":"number"}]}}}`, `{"a":["x","y"]}`},
		{"2020-12 prefixItems", `{"properties":{"a":{"prefixItems":[{"type":"string"},{"type":"number"}]}}}`, `{"a":["x","y"]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "draft-07 tuple items" {
				wantRefused(t, check(v, c.schema, c.bad), "/a/1")
				return
			}
			if c.name == "2020-12 prefixItems" {
				wantRefused(t, check(v, c.schema, c.bad), "/a/1")
				return
			}
			wantRefused(t, check(v, c.schema, c.bad), "/a")
		})
	}
	if len(w.all()) != 0 {
		t.Fatalf("unexpected warnings %+v", w.all())
	}
}

func TestDraft07RefSiblingsAreEvaluatedAsInTheReference(t *testing.T) {
	v, _ := newValidator(t)
	const s = `{"$schema":"http://json-schema.org/draft-07/schema#","definitions":{"s":{"type":"string"}},"properties":{"a":{"$ref":"#/definitions/s","minLength":5}}}`
	wantRefused(t, check(v, s, `{"a":"abc"}`), "/a")
	wantRefused(t, check(v, s, `{"a":1}`), "/a")
	if got := check(v, s, `{"a":"abcdef"}`); !got.OK {
		t.Fatal(got)
	}
}

func TestRefsAndAnchorsWork(t *testing.T) {
	v, _ := newValidator(t)
	const s = `{"$defs":{"s":{"type":"string"},"n":{"$anchor":"num","type":"number"}},"properties":{"a":{"$ref":"#/$defs/s"},"b":{"$ref":"#num"}}}`
	wantRefused(t, check(v, s, `{"a":1}`), "/a")
	wantRefused(t, check(v, s, `{"b":"x"}`), "/b")
	if got := check(v, s, `{"a":"x","b":1}`); !got.OK {
		t.Fatal(got)
	}
}

func TestPointersEscapeAndKeepOtherCharacters(t *testing.T) {
	v, _ := newValidator(t)
	for key, want := range map[string]string{"a/b": "/a~1b", "a~b": "/a~0b", `a"b`: `/a"b`, "ключ": "/ключ"} {
		s := fmt.Sprintf(`{"properties":{%q:{"type":"string"}}}`, key)
		wantRefused(t, check(v, s, fmt.Sprintf(`{%q:1}`, key)), want)
	}
}

func TestKeywordTable(t *testing.T) {
	v, _ := newValidator(t)
	cases := []struct{ name, schema, args, path string }{
		{"type", `{"properties":{"a":{"type":"string"}}}`, `{"a":1}`, "/a"},
		{"type list", `{"properties":{"a":{"type":["string","null"]}}}`, `{"a":1}`, "/a"},
		{"additional schema", `{"additionalProperties":{"type":"string"}}`, `{"z":1}`, "/z"},
		{"enum", `{"properties":{"a":{"enum":["x"]}}}`, `{"a":"y"}`, "/a"},
		{"const", `{"properties":{"a":{"const":1}}}`, `{"a":2}`, "/a"},
		{"minimum", `{"properties":{"a":{"minimum":1}}}`, `{"a":0}`, "/a"},
		{"exclusiveMinimum", `{"properties":{"a":{"exclusiveMinimum":1}}}`, `{"a":1}`, "/a"},
		{"maximum", `{"properties":{"a":{"maximum":1}}}`, `{"a":2}`, "/a"},
		{"exclusiveMaximum", `{"properties":{"a":{"exclusiveMaximum":1}}}`, `{"a":1}`, "/a"},
		{"multipleOf", `{"properties":{"a":{"multipleOf":5}}}`, `{"a":7}`, "/a"},
		{"minLength", `{"properties":{"a":{"minLength":2}}}`, `{"a":"x"}`, "/a"},
		{"maxLength", `{"properties":{"a":{"maxLength":2}}}`, `{"a":"xyz"}`, "/a"},
		{"minItems", `{"properties":{"a":{"minItems":2}}}`, `{"a":[1]}`, "/a"},
		{"maxItems", `{"properties":{"a":{"maxItems":1}}}`, `{"a":[1,2]}`, "/a"},
		{"uniqueItems", `{"properties":{"a":{"uniqueItems":true}}}`, `{"a":[1,1]}`, "/a"},
		{"items", `{"properties":{"a":{"items":{"type":"string"}}}}`, `{"a":["x",1]}`, "/a/1"},
		{"contains", `{"properties":{"a":{"contains":{"type":"string"}}}}`, `{"a":[1]}`, "/a/0"}, // the item's own error, as the reference
		{"minProperties", `{"properties":{"a":{"minProperties":1}}}`, `{"a":{}}`, "/a"},
		{"maxProperties", `{"properties":{"a":{"maxProperties":1}}}`, `{"a":{"x":1,"y":2}}`, "/a"},
		{"anyOf", `{"properties":{"a":{"anyOf":[{"type":"string"},{"type":"boolean"}]}}}`, `{"a":1}`, "/a"},
		{"oneOf none", `{"properties":{"a":{"oneOf":[{"type":"string"},{"type":"boolean"}]}}}`, `{"a":1}`, "/a"},
		{"oneOf two", `{"properties":{"a":{"oneOf":[{"type":"number"},{"minimum":0}]}}}`, `{"a":1}`, "/a"},
		{"allOf", `{"properties":{"a":{"allOf":[{"type":"string"},{"minLength":3}]}}}`, `{"a":"x"}`, "/a"},
		{"not", `{"properties":{"a":{"not":{"type":"string"}}}}`, `{"a":"x"}`, "/a"},
		{"if then", `{"if":{"properties":{"a":{"const":1}}},"then":{"required":["z"]}}`, `{"a":1}`, "/"},
		{"dependentRequired", `{"dependentRequired":{"a":["b"]}}`, `{"a":1}`, "/"},
		{"draft-07 dependencies", `{"$schema":"http://json-schema.org/draft-07/schema#","dependencies":{"a":["b"]}}`, `{"a":1}`, "/"},
		{"unevaluatedProperties", `{"properties":{"a":{}},"unevaluatedProperties":false}`, `{"a":1,"b":2}`, "/b"}, // the library blames the property; the reference the object
		{"propertyNames", `{"propertyNames":{"maxLength":2}}`, `{"abc":1}`, "/"},
		{"date", `{"properties":{"a":{"format":"date"}}}`, `{"a":"2026-02-30"}`, "/a"},
		{"date-time", `{"properties":{"a":{"format":"date-time"}}}`, `{"a":"2026-01-01"}`, "/a"},
		{"email", `{"properties":{"a":{"format":"email"}}}`, `{"a":"nope"}`, "/a"},
		{"uri", `{"properties":{"a":{"format":"uri"}}}`, `{"a":"::nope"}`, "/a"},
		{"uuid", `{"properties":{"a":{"format":"uuid"}}}`, `{"a":"nope"}`, "/a"},
		{"ipv4", `{"properties":{"a":{"format":"ipv4"}}}`, `{"a":"999.1.1.1"}`, "/a"},
		{"nested required", `{"properties":{"n":{"required":["q"]}}}`, `{"n":{}}`, "/n"},
		{"integer rejects 1.5", `{"properties":{"a":{"type":"integer"}}}`, `{"a":1.5}`, "/a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantRefused(t, check(v, c.schema, c.args), c.path) })
	}
}

func TestSilentCases(t *testing.T) {
	v, w := newValidator(t)
	cases := []struct{ name, schema, args string }{
		{"unknown format", `{"properties":{"a":{"format":"nope"}}}`, `{"a":"x"}`},
		{"format on a non-string", `{"properties":{"a":{"format":"date"}}}`, `{"a":1}`},
		{"default and examples", `{"properties":{"a":{"type":"string","default":1,"examples":[2]}}}`, `{"a":"x"}`},
		{"unknown keywords", `{"properties":{"a":{"type":"string","discriminator":{"propertyName":"x"},"x-custom":1}}}`, `{"a":"x"}`},
		{"integer accepts 1.0", `{"properties":{"a":{"type":"integer"}}}`, `{"a":1.0}`},
		{"integer accepts 1e21", `{"properties":{"a":{"type":"integer"}}}`, `{"a":1e21}`},
		{"empty schema", `{}`, `{"a":[1,{"b":null}]}`},
		{"maxLength counts code points", `{"properties":{"a":{"maxLength":1}}}`, `{"a":"😀"}`},
		{"nested $id", `{"properties":{"a":{"$id":"urn:x","type":"string"}}}`, `{"a":"x"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := check(v, c.schema, c.args); !got.OK {
				t.Fatalf("got %+v", got)
			}
		})
	}
	if len(w.all()) != 0 {
		t.Fatalf("warnings %+v", w.all())
	}
}

func TestNullableIsIgnored(t *testing.T) {
	v, _ := newValidator(t)
	wantRefused(t, check(v, `{"properties":{"a":{"type":"string","nullable":true}}}`, `{"a":null}`), "/a")
}

func TestRequiredIsReportedBeforeAPropertyTypeError(t *testing.T) {
	v, _ := newValidator(t)
	const s = `{"properties":{"a":{"type":"string"},"b":{}},"required":["b"]}`
	for i := 0; i < 50; i++ {
		got := check(v, s, `{"a":1}`)
		wantRefused(t, got, "/")
		if !strings.Contains(got.Message, "b") {
			t.Fatalf("message %q", got.Message)
		}
	}
}

func TestTheFirstErrorIsDeterministicAmongProperties(t *testing.T) {
	v, _ := newValidator(t)
	const s = `{"properties":{"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"},"d":{"type":"string"}}}`
	for i := 0; i < 100; i++ {
		wantRefused(t, check(v, s, `{"d":1,"c":1,"b":1,"a":1}`), "/a")
	}
}

func TestAnyOfReportsTheFirstBranch(t *testing.T) {
	v, _ := newValidator(t)
	got := check(v, `{"properties":{"a":{"anyOf":[{"type":"string"},{"type":"boolean"}]}}}`, `{"a":1}`)
	wantRefused(t, got, "/a")
	if !strings.Contains(got.Message, "string") {
		t.Fatalf("message %q", got.Message)
	}
}

func TestLargeNumbersAreJudgedExactly(t *testing.T) {
	v, _ := newValidator(t)
	// 2^53+1 is not representable as a double: a validator that sees doubles reads it as 2^53.
	const s = `{"properties":{"a":{"maximum":9007199254740992}}}`
	wantRefused(t, check(v, s, `{"a":9007199254740993}`), "/a")
	if got := check(v, s, `{"a":9007199254740992}`); !got.OK {
		t.Fatal(got)
	}
	const dec = `{"properties":{"a":{"multipleOf":0.1}}}`
	if got := check(v, dec, `{"a":0.3}`); !got.OK {
		t.Fatalf("0.3 is a multiple of 0.1: %+v", got)
	}
}

func TestArgumentsAreNotEchoedByFormatErrors(t *testing.T) {
	v, _ := newValidator(t)
	got := check(v, `{"properties":{"a":{"format":"email"}}}`, `{"a":"SECRET-MARKER"}`)
	wantRefused(t, got, "/a")
	if strings.Contains(got.Message, "SECRET-MARKER") {
		t.Fatalf("message %q echoes the argument", got.Message)
	}
	got = check(v, `{"additionalProperties":false}`, `{"SECRET-MARKER":1}`)
	wantRefused(t, got, "/")
	if strings.Contains(got.Message, "SECRET-MARKER") {
		t.Fatalf("message %q echoes an argument key", got.Message)
	}
}

func TestEmptyAndMalformedArguments(t *testing.T) {
	v, _ := newValidator(t)
	const s = `{"type":"object","required":["a"]}`
	wantRefused(t, v.Validate("a.b", json.RawMessage(s), nil), "/")
	wantRefused(t, v.Validate("a.b", json.RawMessage(s), json.RawMessage(`{"a":`)), "/")
	if got := v.Validate("a.b", json.RawMessage(`{}`), nil); !got.OK {
		t.Fatal(got)
	}
	if got := v.Validate("a.b", nil, json.RawMessage(`{"a":1}`)); !got.OK {
		t.Fatalf("an absent schema accepts: %+v", got)
	}
}

func TestCacheIsBoundedAndFIFO(t *testing.T) {
	v, _ := newValidator(t)
	for i := 0; i < maxCachedSchemas+10; i++ {
		check(v, fmt.Sprintf(`{"properties":{"p%d":{"type":"string"}}}`, i), `{}`)
	}
	if len(v.cache) != maxCachedSchemas || len(v.order) != maxCachedSchemas {
		t.Fatalf("cache %d, order %d", len(v.cache), len(v.order))
	}
	n := v.compiledCount
	check(v, `{"properties":{"p0":{"type":"string"}}}`, `{}`) // evicted: compiled again
	if v.compiledCount != n+1 {
		t.Fatal("the oldest entry should have been evicted")
	}
	check(v, fmt.Sprintf(`{"properties":{"p%d":{"type":"string"}}}`, maxCachedSchemas+9), `{}`) // newest: a hit
	if v.compiledCount != n+1 {
		t.Fatal("the newest entry should still be cached")
	}
}

func TestConcurrentUse(t *testing.T) {
	v, _ := newValidator(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wantRefused(t, check(v, mainSchema, `{"title":7}`), "/title")
		}()
	}
	wg.Wait()
	if v.compiledCount != 1 {
		t.Fatalf("compiled %d times", v.compiledCount)
	}
}

func TestADeeplyNestedArgumentDoesNotCrash(t *testing.T) {
	v, _ := newValidator(t)
	deep := strings.Repeat(`{"a":`, 500) + `1` + strings.Repeat(`}`, 500)
	if got := check(v, `{"type":"object"}`, deep); !got.OK {
		t.Fatal(got)
	}
}
