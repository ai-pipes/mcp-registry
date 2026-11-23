package http_test

// GET /api/calls (calls.ts, inventory 6.1): the guards, the query as zod reads it (paths of the
// first issue, compat TestGatewayCallsAPI), the filters handed to the use case, the response.

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway"
)

func (e *env) calls(t *testing.T, cookie, query string, hdr ...string) reply {
	t.Helper()
	h := []string{}
	if cookie != "" {
		h = append(h, "Cookie: session="+cookie)
	}
	return e.do(t, "GET", "/api/calls"+query, "", append(h, hdr...)...)
}

func TestCallsAreForAdminsOnlyBeforeTheQueryIsRead(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name, cookie, query, path, method string
		hdr                               []string
		status                            int
		code                              string
	}{
		{"anonymous", "", "", "/api/calls", "GET", nil, 401, "unauthenticated|sign in required"},
		{"a bearer token", "", "", "/api/calls", "GET", []string{"Authorization: Bearer " + adminToken}, 401, "unauthenticated|tokens do not work on the web API"},
		{"a member", "member-session", "?limit=0", "/api/calls", "GET", nil, 403, "forbidden|admin only"},
		{"a member below the prefix", "member-session", "", "/api/calls/x", "GET", nil, 403, "forbidden|admin only"},
		{"an admin below the prefix", "admin-session", "", "/api/calls/x", "GET", nil, 404, ""},
		{"an admin with another method", "admin-session", "", "/api/calls", "DELETE", nil, 404, ""},
	} {
		h := c.hdr
		if c.cookie != "" {
			h = append(h, "Cookie: session="+c.cookie)
		}
		r := e.do(t, c.method, c.path+c.query, "", h...)
		if r.status != c.status || (c.code != "" && errorCode(t, r) != c.code) {
			t.Errorf("%s: %d %s", c.name, r.status, r.body)
		}
	}
	if len(e.gw.listed) != 0 {
		t.Fatalf("the log was read: %v", e.gw.listed)
	}
}

func TestCallsQueryProblemsAre422AtTheirPath(t *testing.T) {
	e := newEnv(t)
	issue := regexp.MustCompile(`^invalid_request\|([\w.]+): `)
	for _, c := range []struct{ query, path string }{
		{"?limit=0", "limit"},
		{"?limit=501", "limit"},
		{"?limit=abc", "limit"},
		{"?limit=1.5", "limit"},
		{"?limit=", "limit"},
		{"?limit=Infinity", "limit"},
		{"?user=x", "user"},
		{"?user=", "user"},
		{"?outcome=OK", "outcome"},
		{"?outcome=", "outcome"},
		{"?before=yesterday", "before"},
		{"?before=2026-11-23T10:00Z", "before"},
		{"?server=", "server"},
		{"?server=" + strings.Repeat("s", 201), "server"},
		{"?beforeId=5", "beforeId"},
		{"?beforeId=abc&before=2026-11-23T10:00:00Z", "beforeId"},
		{"?beforeId=" + strings.Repeat("1", 19) + "&before=2026-11-23T10:00:00Z", "beforeId"},
		// The first problem in the schema's key order wins, the cross-field rule is last.
		{"?limit=0&outcome=OK&user=x", "user"},
		{"?beforeId=5&limit=0", "limit"},
		{"?beforeId=x&server=", "server"},
	} {
		r := e.calls(t, "admin-session", c.query)
		m := issue.FindStringSubmatch(errorCode(t, r))
		if r.status != 422 || m == nil || m[1] != c.path {
			t.Errorf("%s: %d %s, want 422 at %s", c.query, r.status, r.body, c.path)
		}
	}
	if len(e.gw.listed) != 0 {
		t.Fatalf("a refused query reached the use case: %v", e.gw.listed)
	}
}

func TestCallsPassTheFiltersAndAnswerTheContract(t *testing.T) {
	e := newEnv(t)
	tok := "11"
	e.gw.rows = []gateway.CallRecord{
		{ID: "100000000000000006", CallLogEntry: gateway.CallLogEntry{UserID: "u-1", TokenID: tok, Server: "gitea", Tool: "<a>&b", StartedAt: time.Date(2026, 11, 23, 10, 0, 0, 123456789, time.UTC), DurationMs: 42, Outcome: gateway.OutcomeOK}},
		{ID: "5", CallLogEntry: gateway.CallLogEntry{UserID: "u-2", Server: "yt", Tool: "x", StartedAt: time.Date(2026, 11, 23, 9, 0, 0, 0, time.FixedZone("", 3600)), Outcome: gateway.OutcomeDeniedQuota}},
	}
	r := e.calls(t, "admin-session", "")
	want := `[{"id":"100000000000000006","userId":"u-1","tokenId":"11","server":"gitea","tool":"<a>&b","startedAt":"2026-11-23T10:00:00.123Z","durationMs":42,"outcome":"ok"},` +
		`{"id":"5","userId":"u-2","tokenId":null,"server":"yt","tool":"x","startedAt":"2026-11-23T08:00:00.000Z","durationMs":0,"outcome":"denied:quota"}]`
	if r.status != 200 || string(r.body) != want || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s %s", r.status, r.header.Get("Content-Type"), r.body)
	}
	q := e.gw.listed[0]
	if q.Principal.UserID != "root" || q.UserID != nil || q.Server != nil || q.Outcome != nil || q.Before != nil || q.BeforeID != nil || q.Limit != 100 {
		t.Fatalf("no filters: %+v", q)
	}
	e.gw.rows = nil
	r = e.calls(t, "admin-session", "?user=123e4567-e89b-12d3-a456-426614174000&server=a+b%2Bc&outcome=denied%3Aquota&before=2026-11-23T12:00:00.1239%2B02:00&beforeId=17&limit=500&user=other&unknown=1")
	if r.status != 200 || string(r.body) != "[]" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	q = e.gw.listed[1]
	if q.UserID == nil || *q.UserID != "123e4567-e89b-12d3-a456-426614174000" || q.Server == nil || *q.Server != "a b+c" ||
		q.Outcome == nil || *q.Outcome != gateway.OutcomeDeniedQuota || q.BeforeID == nil || *q.BeforeID != "17" || q.Limit != 500 ||
		q.Before == nil || !q.Before.Equal(time.Date(2026, 11, 23, 10, 0, 0, 123900000, time.UTC)) {
		t.Fatalf("filters: %+v", q)
	}
	// JavaScript's Number() for the limit, the Hono reading of the query (a ; is data).
	e.calls(t, "admin-session", "?limit=1e2")
	e.calls(t, "admin-session", "?limit=%207%20")
	e.calls(t, "admin-session", "?server=a;b")
	if e.gw.listed[2].Limit != 100 || e.gw.listed[3].Limit != 7 || *e.gw.listed[4].Server != "a;b" {
		t.Fatalf("coercion: %+v %+v %+v", e.gw.listed[2], e.gw.listed[3], e.gw.listed[4])
	}
	// A failure of the store is the generic 500.
	e.gw.listErr = errors.New("db down")
	if r := e.calls(t, "admin-session", ""); r.status != 500 || errorCode(t, r) != "internal|Internal error" {
		t.Fatalf("%d %s", r.status, r.body)
	}
}
