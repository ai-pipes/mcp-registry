package httpx

import (
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// RuneLen is the length of s in code points, which is what zod 4 counts for a string's min and
// max (Fields uses it). A rule the TypeScript domain writes with .length counts UTF-16 units instead.
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

// Check is a cross-field rule (zod's refine): when ok is false the problem is path: reason.
func (f *Fields) Check(ok bool, path, reason string) {
	if !ok {
		f.fail(path, reason)
	}
}

// OptionalQueryString is z.string().min(minLen).max(maxLen).optional() for query parameter name:
// nil when absent, else the value (the first occurrence) checked like String.
func (f *Fields) OptionalQueryString(path string, q url.Values, name string, minLen, maxLen int) *string {
	vals, ok := q[name]
	if !ok || len(vals) == 0 {
		return nil
	}
	v := f.String(path, vals[0], minLen, maxLen)
	return &v
}

// QueryEnum is z.enum(allowed).optional() for query parameter name: present reports whether it was
// given; a given value must be one of allowed exactly (case included).
func (f *Fields) QueryEnum(path string, q url.Values, name string, allowed []string) (value string, present bool) {
	vals, ok := q[name]
	if !ok || len(vals) == 0 {
		return "", false
	}
	if !slices.Contains(allowed, vals[0]) {
		f.fail(path, "must be one of "+strings.Join(allowed, ", "))
		return "", true
	}
	return vals[0], true
}

// uuidPattern is zod 4's z.uuid(): RFC 9562 versions 1 to 8 with the variant 8, 9, a or b, the nil
// and the max UUID; hexadecimal digits in either case.
var uuidPattern = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|[fF]{8}-[fF]{4}-[fF]{4}-[fF]{4}-[fF]{12})$`)

// QueryUUID is z.uuid().optional() for query parameter name.
func (f *Fields) QueryUUID(path string, q url.Values, name string) (value string, present bool) {
	vals, ok := q[name]
	if !ok || len(vals) == 0 {
		return "", false
	}
	if !uuidPattern.MatchString(vals[0]) {
		f.fail(path, "must be a UUID")
		return "", true
	}
	return vals[0], true
}

// isoDateTime is the shape of zod 4's z.iso.datetime({ offset: true }): seconds required, any
// number of fraction digits, Z (upper case) or a +HH:MM / -HH:MM offset.
var isoDateTime = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(?:(Z)|([+-])(\d{2}):(\d{2}))$`)

// QueryOffsetDateTime is z.iso.datetime({ offset: true }).optional() for query parameter name: a
// real calendar date, hours 00-23, minutes and seconds 00-59, offset hours 00-23 and minutes
// 00-59. The instant keeps at most nanoseconds of the fraction (the caller cuts it further).
func (f *Fields) QueryOffsetDateTime(path string, q url.Values, name string) (value time.Time, present bool) {
	vals, ok := q[name]
	if !ok || len(vals) == 0 {
		return time.Time{}, false
	}
	t, ok := parseOffsetDateTime(vals[0])
	if !ok {
		f.fail(path, "must be an ISO date-time with an offset")
		return time.Time{}, true
	}
	return t, true
}

func parseOffsetDateTime(s string) (time.Time, bool) {
	m := isoDateTime.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	year, month, day, hour, minute, sec := n(1), n(2), n(3), n(4), n(5), n(6)
	if month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || sec > 59 {
		return time.Time{}, false
	}
	if day > time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day() {
		return time.Time{}, false
	}
	nanos := 0
	if frac := m[7]; frac != "" {
		frac = (frac + "000000000")[:9]
		nanos, _ = strconv.Atoi(frac)
	}
	offset := 0
	if m[8] == "" {
		oh, om := n(10), n(11)
		if oh > 23 || om > 59 {
			return time.Time{}, false
		}
		offset = (oh*60 + om) * 60
		if m[9] == "-" {
			offset = -offset
		}
	}
	return time.Date(year, time.Month(month), day, hour, minute, sec, nanos, time.FixedZone("", offset)).UTC(), true
}
