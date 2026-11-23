package infra

import (
	"encoding/json"
	"strings"
	"testing"
)

// wideLimit is a schema number of 390 significant digits and the largest allowed exponent.
var wideLimit = "1." + strings.Repeat("3", 390) + "e-400"

// A schema's own wide number literals weigh their width: the library computes with them exactly
// for every argument value.
func TestASchemasWideNumbersWeighTheirWidth(t *testing.T) {
	weight := func(schema string) int {
		t.Helper()
		sch, err := compile(json.RawMessage(schema))
		if err != nil {
			t.Fatal(err)
		}
		_, w, err := expandedSize(sch)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	for _, kw := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
		narrow, wide := weight(`{"`+kw+`":1}`), weight(`{"`+kw+`":`+wideLimit+`}`)
		if wide < narrow+40 {
			t.Errorf("%s: narrow %d, wide %d", kw, narrow, wide)
		}
	}
	narrow, wide := weight(`{"enum":[1]}`), weight(`{"enum":[`+wideLimit+`]}`)
	if narrow < enumValueWeight || wide < narrow+40 {
		t.Errorf("enum: narrow %d, wide %d", narrow, wide)
	}
	if narrow, wide := weight(`{"const":1}`), weight(`{"const":`+wideLimit+`}`); wide < narrow+40 {
		t.Errorf("const: narrow %d, wide %d", narrow, wide)
	}
}

// An exponent beyond int64 cannot turn the count negative; such a number is refused like any
// other beyond the ±400 bound.
func TestAnExponentBeyondInt64DoesNotOverflowTheCount(t *testing.T) {
	v, _ := newValidator(t)
	for _, lit := range []string{"1e-9223372036854775808", "1e99999999999999999999", "1e9223372036854775807"} {
		if n := countValues([]byte("[" + lit + "]")); n < 2 {
			t.Errorf("%s: count %d", lit, n)
		}
		got := check(v, `{"properties":{"a":{"type":"number"}}}`, `{"a":`+lit+`}`)
		want := check(v, `{"properties":{"a":{"type":"number"}}}`, `{"a":1e401}`)
		if got != want || got.OK {
			t.Errorf("%s: %+v, want %+v", lit, got, want)
		}
	}
}
