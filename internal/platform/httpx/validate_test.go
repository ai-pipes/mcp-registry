package httpx_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

func ptr[T any](v T) *T { return &v }

func code(t *testing.T, err error) (string, string) {
	t.Helper()
	de, ok := kernel.AsDomainError(err)
	if !ok {
		t.Fatalf("error %v is not a DomainError", err)
	}
	return de.Code, de.Message
}

func TestUTF16LenCountsCodeUnits(t *testing.T) {
	for in, want := range map[string]int{"": 0, "abc": 3, "é": 1, "😀": 2, "a😀b": 4, "\u2028": 1} {
		if got := httpx.UTF16Len(in); got != want {
			t.Errorf("UTF16Len(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestFieldsStringCountsUTF16(t *testing.T) {
	var f httpx.Fields
	// 50 emoji are 100 UTF-16 units (allowed) and 50 code points; 51 are 102 units (too long).
	if got := f.String("name", strings.Repeat("😀", 50), 1, 100); got == "" {
		t.Error("100 units were refused")
	}
	if f.Err() != nil {
		t.Fatalf("unexpected %v", f.Err())
	}
	f.String("name", strings.Repeat("😀", 51), 1, 100)
	c, m := code(t, f.Err())
	if c != "invalid_request" || m != "name: must be at most 100 characters" {
		t.Errorf("%s %q", c, m)
	}
}

func TestFieldsKeepTheFirstIssueInCallOrder(t *testing.T) {
	var f httpx.Fields
	f.String("sub", "", 1, 10)
	f.String("name", strings.Repeat("a", 11), 1, 10)
	f.Bool("admin", nil)
	if _, m := code(t, f.Err()); m != "sub: is required" {
		t.Errorf("message %q", m)
	}
	if f.Err() == nil {
		t.Error("Err is not repeatable")
	}
	if (&httpx.Fields{}).Err() != nil {
		t.Error("no issue, no error")
	}
}

func TestFieldsStringPhrases(t *testing.T) {
	for _, tc := range []struct {
		value    string
		min, max int
		want     string
	}{
		{"", 1, 5, "p: is required"},
		{"ab", 3, 5, "p: must be at least 3 characters"},
		{"abcdef", 1, 5, "p: must be at most 5 characters"},
		{"abc", 1, 5, ""},
	} {
		var f httpx.Fields
		f.String("p", tc.value, tc.min, tc.max)
		if tc.want == "" {
			if f.Err() != nil {
				t.Errorf("%q: %v", tc.value, f.Err())
			}
			continue
		}
		if _, m := code(t, f.Err()); m != tc.want {
			t.Errorf("%q: %q, want %q", tc.value, m, tc.want)
		}
	}
}

func TestFieldsTrimmedStringTrimsLikeJavaScriptBeforeChecking(t *testing.T) {
	var f httpx.Fields
	if got := f.TrimmedString("name", "  \u00a0Ann\ufeff ", 1, 10); got != "Ann" || f.Err() != nil {
		t.Errorf("got %q, %v", got, f.Err())
	}
	f.TrimmedString("name", " \t ", 1, 10)
	if _, m := code(t, f.Err()); m != "name: is required" {
		t.Errorf("message %q", m)
	}
}

func TestFieldsRequiredAndOptionalReaders(t *testing.T) {
	var f httpx.Fields
	if got := f.RequiredString("sub", ptr("x"), 1, 5); got != "x" {
		t.Errorf("got %q", got)
	}
	f.RequiredString("sub", nil, 1, 5)
	if _, m := code(t, f.Err()); m != "sub: is required" {
		t.Errorf("message %q", m)
	}

	var g httpx.Fields
	if got := g.OptionalString("note", nil, 5); got != nil {
		t.Errorf("absent: %v", got)
	}
	if got := g.OptionalString("note", ptr("ok"), 5); got == nil || *got != "ok" {
		t.Errorf("present: %v", got)
	}
	if g.Err() != nil {
		t.Fatalf("%v", g.Err())
	}
	g.OptionalString("note", ptr("too long"), 5)
	if _, m := code(t, g.Err()); m != "note: must be at most 5 characters" {
		t.Errorf("message %q", m)
	}

	var h httpx.Fields
	if !h.Bool("admin", ptr(true)) || h.Err() != nil {
		t.Error("true was not read")
	}
	h.Bool("admin", nil)
	if _, m := code(t, h.Err()); m != "admin: is required" {
		t.Errorf("message %q", m)
	}
}

func TestFieldsQueryIntCoercesLikeJavaScriptNumber(t *testing.T) {
	q := func(raw string) url.Values { return url.Values{"limit": {raw}} }
	for _, tc := range []struct {
		name string
		q    url.Values
		want int
		msg  string
	}{
		{"absent is the default", url.Values{}, 100, ""},
		{"a number", q("25"), 25, ""},
		{"padded", q(" 25 "), 25, ""},
		{"exponent", q("1e2"), 100, ""},
		{"hex", q("0x10"), 16, ""},
		{"the maximum", q("500"), 500, ""},
		{"the minimum", q("1"), 1, ""},
		{"empty counts as 0, below the minimum", q(""), 0, "limit: must be at least 1"},
		{"zero", q("0"), 0, "limit: must be at least 1"},
		{"above the maximum", q("501"), 0, "limit: must be at most 500"},
		{"a fraction", q("1.5"), 0, "limit: must be an integer"},
		{"not a number", q("abc"), 0, "limit: must be a number"},
		{"infinity", q("1e999"), 0, "limit: must be a number"},
		{"Infinity", q("Infinity"), 0, "limit: must be a number"},
		{"+Infinity", q("+Infinity"), 0, "limit: must be a number"},
		{"-Infinity", q("-Infinity"), 0, "limit: must be a number"},
		{"lowercase infinity is NaN", q("infinity"), 0, "limit: must be a number"},
		{"first value wins", url.Values{"limit": {"7", "9"}}, 7, ""},
	} {
		var f httpx.Fields
		got := f.QueryInt("limit", tc.q, "limit", 1, 500, 100)
		if tc.msg == "" {
			if f.Err() != nil || got != tc.want {
				t.Errorf("%s: %d, %v; want %d", tc.name, got, f.Err(), tc.want)
			}
			continue
		}
		if _, m := code(t, f.Err()); m != tc.msg {
			t.Errorf("%s: %q, want %q", tc.name, m, tc.msg)
		}
	}
}

func TestFieldsQueryDigitsIsDigitsOnlyWithAMaximumLength(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		present bool
		want    string
		msg     string
	}{
		{"", false, "", ""},
		{"123", true, "123", ""},
		{"123456789012345678", true, "123456789012345678", ""},
		{"1234567890123456789", true, "", "before: must be at most 18 digits"},
		{"", true, "", "before: must be digits only"},
		{"12a", true, "", "before: must be digits only"},
		{"-1", true, "", "before: must be digits only"},
		{"١٢٣", true, "", "before: must be digits only"}, // Arabic-Indic digits are not \d in JavaScript
	} {
		q := url.Values{}
		if tc.present {
			q.Set("before", tc.raw)
		}
		var f httpx.Fields
		got, present := f.QueryDigits("before", q, "before", 18)
		if tc.msg == "" {
			if f.Err() != nil || got != tc.want || present != tc.present {
				t.Errorf("%q: %q %v %v", tc.raw, got, present, f.Err())
			}
			continue
		}
		if _, m := code(t, f.Err()); m != tc.msg {
			t.Errorf("%q: %q, want %q", tc.raw, m, tc.msg)
		}
	}
}
