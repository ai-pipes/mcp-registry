package httpx_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

func TestQueryValuesKeepsTheFirstOccurrenceTheHonoWay(t *testing.T) {
	q := httpx.QueryValues("limit=5&limit=7&server=a;b&user=x+y&empty=&bare&%ZZ=1&outcome=%FF#frag=1")
	want := map[string]string{"limit": "5", "server": "a;b", "user": "x y", "empty": "", "bare": "", "%ZZ": "1", "outcome": "%FF"}
	if len(q) != len(want) {
		t.Fatalf("QueryValues = %v, want the keys of %v", q, want)
	}
	for k, v := range want {
		if got := q[k]; len(got) != 1 || got[0] != v {
			t.Errorf("%s = %q, want [%q]", k, got, v)
		}
	}
	if _, ok := q["frag"]; ok {
		t.Error("the fragment is read as a parameter")
	}
	if got := httpx.QueryValues(""); len(got) != 0 {
		t.Errorf("an empty query gives %v", got)
	}
}

func TestFieldsOptionalQueryString(t *testing.T) {
	var f httpx.Fields
	if v := f.OptionalQueryString("server", url.Values{}, "server", 1, 200); v != nil || f.Err() != nil {
		t.Fatalf("absent: %v %v", v, f.Err())
	}
	if v := f.OptionalQueryString("server", url.Values{"server": {"gitea"}}, "server", 1, 200); v == nil || *v != "gitea" || f.Err() != nil {
		t.Fatalf("present: %v %v", v, f.Err())
	}
	for _, c := range []struct{ value, msg string }{
		{"", "server: is required"},
		{strings.Repeat("é", 201), "server: must be at most 200 characters"},
	} {
		var f httpx.Fields
		f.OptionalQueryString("server", url.Values{"server": {c.value}}, "server", 1, 200)
		if _, msg := code(t, f.Err()); msg != c.msg {
			t.Errorf("%.20q: %q, want %q", c.value, msg, c.msg)
		}
	}
	var ok httpx.Fields
	if v := ok.OptionalQueryString("server", url.Values{"server": {strings.Repeat("é", 200)}}, "server", 1, 200); v == nil || ok.Err() != nil {
		t.Errorf("200 code points are refused: %v", ok.Err())
	}
}

func TestFieldsQueryEnumIsExactCase(t *testing.T) {
	allowed := []string{"ok", "upstream_error", "denied:quota"}
	var f httpx.Fields
	if v, present := f.QueryEnum("outcome", url.Values{}, "outcome", allowed); present || v != "" || f.Err() != nil {
		t.Fatalf("absent: %q %v %v", v, present, f.Err())
	}
	if v, present := f.QueryEnum("outcome", url.Values{"outcome": {"denied:quota"}}, "outcome", allowed); !present || v != "denied:quota" || f.Err() != nil {
		t.Fatalf("listed: %q %v %v", v, present, f.Err())
	}
	for _, bad := range []string{"OK", "", " ok", "nope"} {
		var f httpx.Fields
		f.QueryEnum("outcome", url.Values{"outcome": {bad}}, "outcome", allowed)
		if c, msg := code(t, f.Err()); c != "invalid_request" || !strings.HasPrefix(msg, "outcome: ") {
			t.Errorf("%q: %s %q", bad, c, msg)
		}
	}
}

func TestFieldsQueryUUIDIsZodsUUID(t *testing.T) {
	for _, good := range []string{
		"123e4567-e89b-12d3-a456-426614174000", "123E4567-E89B-42D3-B456-426614174000",
		"00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff", "FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF",
		"123e4567-e89b-82d3-8456-426614174000",
	} {
		var f httpx.Fields
		if v, present := f.QueryUUID("user", url.Values{"user": {good}}, "user"); !present || v != good || f.Err() != nil {
			t.Errorf("%s refused: %v", good, f.Err())
		}
	}
	for _, bad := range []string{
		"x", "", "123e4567-e89b-02d3-a456-426614174000", "123e4567-e89b-92d3-a456-426614174000",
		"123e4567-e89b-12d3-c456-426614174000", "123e4567e89b12d3a456426614174000", " 123e4567-e89b-12d3-a456-426614174000",
		"123e4567-e89b-12d3-a456-42661417400g",
	} {
		var f httpx.Fields
		f.QueryUUID("user", url.Values{"user": {bad}}, "user")
		if _, msg := code(t, f.Err()); !strings.HasPrefix(msg, "user: ") {
			t.Errorf("%q: %q", bad, msg)
		}
	}
	var f httpx.Fields
	if _, present := f.QueryUUID("user", url.Values{}, "user"); present || f.Err() != nil {
		t.Error("an absent user is a problem")
	}
}

func TestFieldsQueryOffsetDateTimeIsZodsISODateTimeWithOffset(t *testing.T) {
	for _, c := range []struct {
		value string
		want  time.Time
	}{
		{"2026-11-23T10:20:30Z", time.Date(2026, 11, 23, 10, 20, 30, 0, time.UTC)},
		{"2026-11-23T10:20:30.123Z", time.Date(2026, 11, 23, 10, 20, 30, 123e6, time.UTC)},
		{"2026-11-23T10:20:30.1234567891234Z", time.Date(2026, 11, 23, 10, 20, 30, 123456789, time.UTC)},
		{"2026-11-23T12:20:30+02:00", time.Date(2026, 11, 23, 10, 20, 30, 0, time.UTC)},
		{"2026-11-23T10:20:30-00:00", time.Date(2026, 11, 23, 10, 20, 30, 0, time.UTC)},
		{"2026-11-23T00:20:30-23:59", time.Date(2026, 11, 24, 0, 19, 30, 0, time.UTC)},
		{"2024-02-29T00:00:00Z", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)},
		{"0000-01-01T00:00:00Z", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		var f httpx.Fields
		got, present := f.QueryOffsetDateTime("before", url.Values{"before": {c.value}}, "before")
		if !present || f.Err() != nil || !got.Equal(c.want) {
			t.Errorf("%s: %v %v %v, want %v", c.value, got, present, f.Err(), c.want)
		}
	}
	for _, bad := range []string{
		"yesterday", "", "2026-11-23", "2026-11-23T10:20Z", "2026-11-23T10:20:30", "2026-11-23T10:20:30z",
		"2026-11-23 10:20:30Z", " 2026-11-23T10:20:30Z", "2026-11-23T10:20:30+0200", "2026-11-23T10:20:30+02",
		"2026-02-30T00:00:00Z", "2026-13-45T00:00:00Z", "2025-02-29T00:00:00Z", "2026-11-23T24:00:00Z",
		"2026-11-23T10:60:00Z", "2026-11-23T10:20:60Z", "2026-11-23T10:20:30+24:00", "2026-11-23T10:20:30+02:60",
		"2026-11-23T10:20:30.Z", "+2026-11-23T10:20:30Z", "2026-1-23T10:20:30Z",
	} {
		var f httpx.Fields
		f.QueryOffsetDateTime("before", url.Values{"before": {bad}}, "before")
		if _, msg := code(t, f.Err()); !strings.HasPrefix(msg, "before: ") {
			t.Errorf("%q: %q", bad, msg)
		}
	}
	var f httpx.Fields
	if _, present := f.QueryOffsetDateTime("before", url.Values{}, "before"); present || f.Err() != nil {
		t.Error("an absent before is a problem")
	}
}

func TestFieldsCheckReportsOnlyWhenFalseAndKeepsTheFirst(t *testing.T) {
	var f httpx.Fields
	f.Check(true, "beforeId", "needs before")
	if f.Err() != nil {
		t.Fatal(f.Err())
	}
	f.Check(false, "beforeId", "needs before")
	f.Check(false, "other", "later")
	if _, msg := code(t, f.Err()); msg != "beforeId: needs before" {
		t.Fatalf("%q", msg)
	}
}

func TestOriginOfIsTheBrowsersOrigin(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"http://localhost:3000", "http://localhost:3000"},
		{"https://MCP.Example.com/base/", "https://mcp.example.com"},
		{"https://example.com:443/x", "https://example.com"},
		{"http://example.com:80", "http://example.com"},
		{"http://[::1]:3000", "http://[::1]:3000"},
	} {
		if got, ok := httpx.OriginOf(c.in); !ok || got != c.want {
			t.Errorf("OriginOf(%q) = %q %v, want %q", c.in, got, ok, c.want)
		}
	}
	if _, ok := httpx.OriginOf("not a url"); ok {
		t.Error("a relative text has an origin")
	}
}
