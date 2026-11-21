package httpx

import (
	"math"
	"net/url"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// UTF16Len is the length of s in UTF-16 code units, which is what JavaScript's String length
// (and so zod's min and max) counts: an emoji outside the Basic Multilingual Plane is 2.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// RuneLen is the length of s in code points, which is what zod 4 counts for a string's min and
// max (Fields uses it); a hand-written JavaScript .length is UTF16Len.
func RuneLen(s string) int { return utf8.RuneCountInString(s) }

// Fields collects input problems in call order. The TypeScript server reports the first zod
// issue as `invalid_request` with the message "<dotted.path>: <reason>"; so does Err. The path
// is the one the TypeScript server reports (the compatibility suite pins it); the reason is ours,
// a short English phrase. Calls after the first problem still run (and keep the first one).
// The zero value is ready to use.
type Fields struct{ first string }

func (f *Fields) fail(path, reason string) {
	if f.first == "" {
		f.first = path + ": " + reason
	}
}

// Err is the first problem as an invalid_request DomainError, or nil.
func (f *Fields) Err() error {
	if f.first == "" {
		return nil
	}
	return InvalidRequest(f.first)
}

func plural(n int, unit string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// String checks value as given (no trimming), counted in code points like zod 4, against minLen and maxLen and returns it. An
// empty value is "is required" when minLen is at least 1.
func (f *Fields) String(path, value string, minLen, maxLen int) string {
	n := RuneLen(value)
	switch {
	case n == 0 && minLen >= 1:
		f.fail(path, "is required")
	case n < minLen:
		f.fail(path, "must be at least "+plural(minLen, "character"))
	case n > maxLen:
		f.fail(path, "must be at most "+plural(maxLen, "character"))
	}
	return value
}

// TrimmedString is z.string().trim().min().max(): JavaScript trim first, then String.
func (f *Fields) TrimmedString(path, value string, minLen, maxLen int) string {
	return f.String(path, kernel.JSTrim(value), minLen, maxLen)
}

// RequiredString is String for a member decoded into a pointer: absent (or null) is "is required".
func (f *Fields) RequiredString(path string, value *string, minLen, maxLen int) string {
	if value == nil {
		f.fail(path, "is required")
		return ""
	}
	return f.String(path, *value, minLen, maxLen)
}

// OptionalString is z.string().max().optional(): nil when absent, else the value checked against
// maxLen.
func (f *Fields) OptionalString(path string, value *string, maxLen int) *string {
	if value == nil {
		return nil
	}
	if RuneLen(*value) > maxLen {
		f.fail(path, "must be at most "+plural(maxLen, "character"))
	}
	return value
}

// Bool is z.boolean() for a member decoded into a pointer: absent (or null) is "is required".
// (A value of the wrong JSON type never gets here: DecodeJSON reports "<path>: invalid type".)
func (f *Fields) Bool(path string, value *bool) bool {
	if value == nil {
		f.fail(path, "is required")
		return false
	}
	return *value
}

// QueryInt is z.coerce.number().int().min(lo).max(hi).default(def) for query parameter name:
// absent is def (the first value when repeated, as Hono's c.req.query does); otherwise JavaScript's
// Number() of the text (so "" is 0, " 7 " is 7, "1e2" is 100, "0x10" is 16), which must be finite
// and an integer within [lo, hi].
func (f *Fields) QueryInt(path string, q url.Values, name string, lo, hi, def int) int {
	vals, ok := q[name]
	if !ok || len(vals) == 0 {
		return def
	}
	n, ok := kernel.JSNumber(vals[0])
	switch {
	case !ok || math.IsInf(n, 0):
		f.fail(path, "must be a number")
	case n != math.Trunc(n):
		f.fail(path, "must be an integer")
	case n < float64(lo):
		f.fail(path, "must be at least "+strconv.Itoa(lo))
	case n > float64(hi):
		f.fail(path, "must be at most "+strconv.Itoa(hi))
	default:
		return int(n)
	}
	return def
}

// QueryDigits is z.string().regex(/^\d{1,max}$/).optional() for query parameter name: present
// reports whether the parameter was given; a given one must be ASCII digits only, at most
// maxLen of them (at least one).
func (f *Fields) QueryDigits(path string, q url.Values, name string, maxLen int) (value string, present bool) {
	vals, ok := q[name]
	if !ok || len(vals) == 0 {
		return "", false
	}
	v := vals[0]
	if v == "" {
		f.fail(path, "must be digits only")
		return "", true
	}
	for i := range len(v) {
		if v[i] < '0' || v[i] > '9' {
			f.fail(path, "must be digits only")
			return "", true
		}
	}
	if len(v) > maxLen {
		f.fail(path, "must be at most "+plural(maxLen, "digit"))
		return "", true
	}
	return v, true
}
