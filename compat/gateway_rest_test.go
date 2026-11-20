//go:build compat

package compat_test

// Call log and audit REST endpoints (GET /api/calls, GET /api/audit): admin only, response
// contracts, filters and bounds, ordering of the call log, the cut of long names, and what the
// audit trail records for connections, servers, tools and tokens. Source: the calls/audit parts of
// admin-rest.int.test.ts and PgCallLog in call.int.test.ts.
//
// Ordering and paging (reference 1.0.1): GET /api/audit is newest first by the NUMERIC id and
// paging with `before` returns every row exactly once; GET /api/calls is ordered by start time and
// then by numeric id, pages with the pair cursor (`before` + `beforeId`) and refuses `beforeId`
// without `before`. Both are asserted on ids that cross digit counts, where a text order differs
// from the numeric one.

import (
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// gwCallEntry is the CallEntry contract.
type gwCallEntry struct {
	ID         string  `json:"id"`
	UserID     string  `json:"userId"`
	TokenID    *string `json:"tokenId"`
	Server     string  `json:"server"`
	Tool       string  `json:"tool"`
	StartedAt  string  `json:"startedAt"`
	DurationMs int     `json:"durationMs"`
	Outcome    string  `json:"outcome"`
}

var gwISO = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func gwCalls(t *testing.T, admin *harness.Client, query string) []gwCallEntry {
	t.Helper()
	r := admin.Do(t, "GET", "/api/calls"+query, nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/calls%s: %d %.300s", query, r.Status, r.Body)
	}
	var out []gwCallEntry
	r.JSON(t, "CallList", &out)
	if out == nil {
		t.Fatalf("GET /api/calls%s: null instead of a list", query)
	}
	for _, e := range out {
		if !gwISO.MatchString(e.StartedAt) {
			t.Fatalf("startedAt %q is not an ISO timestamp with milliseconds", e.StartedAt)
		}
	}
	return out
}

func gwTools(entries []gwCallEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Tool)
	}
	return out
}

func gwUUID(t *testing.T, s *harness.Stack) string {
	t.Helper()
	var id string
	if err := s.DB.QueryRow(t.Context(), "select gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func gwStamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z") }

func TestGatewayCallsAPI(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	member := s.Member(t, harness.Unique("calls-member"))

	// Rows written straight into the table the endpoint reads, with distinct, known timestamps.
	server, elsewhere := harness.Unique("callsapi"), harness.Unique("callsapi-o")
	u1, u2, tok := gwUUID(t, s), gwUUID(t, s), gwUUID(t, s)
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Hour) }
	seed := []struct {
		server, tool, user, outcome string
		token                       *string
		dur                         int
	}{
		{server, "a", u1, "ok", nil, 10},
		{server, "b", u1, "timeout", &tok, 20},
		{server, "c", u2, "denied:quota", nil, 0},
		{server, "d", u2, "ok", &tok, 30},
		{elsewhere, "e", u1, "upstream_error", nil, 40},
	}
	for i, r := range seed {
		execSQL(t, s, `insert into gateway.calls (user_id, token_id, server, tool, started_at, duration_ms, outcome)
			values ($1::uuid, $2::uuid, $3, $4, $5, $6, $7)`, r.user, r.token, r.server, r.tool, at(i), r.dur, r.outcome)
	}

	t.Run("newest first, every field of a row, the filters combine", func(t *testing.T) {
		got := gwCalls(t, admin, "?server="+server)
		if want := []string{"d", "c", "b", "a"}; !slices.Equal(gwTools(got), want) {
			t.Fatalf("tools %v, want %v (newest first)", gwTools(got), want)
		}
		for i, e := range got {
			r := seed[len(got)-1-i]
			want := gwCallEntry{ID: e.ID, UserID: r.user, TokenID: r.token, Server: server, Tool: r.tool, StartedAt: gwStamp(at(len(got) - 1 - i)), DurationMs: r.dur, Outcome: r.outcome}
			if !reflect.DeepEqual(e, want) {
				t.Errorf("row %d:\n got %+v\nwant %+v", i, e, want)
			}
			if !regexp.MustCompile(`^\d+$`).MatchString(e.ID) {
				t.Errorf("id %q is not a number in a string", e.ID)
			}
		}
		for _, c := range []struct {
			query string
			want  []string
		}{
			{"?server=" + server + "&user=" + u1, []string{"b", "a"}},
			{"?server=" + server + "&user=" + u2, []string{"d", "c"}},
			{"?server=" + server + "&outcome=ok", []string{"d", "a"}},
			{"?server=" + server + "&outcome=" + url.QueryEscape("denied:quota"), []string{"c"}},
			{"?server=" + server + "&user=" + u2 + "&outcome=ok", []string{"d"}},
			{"?server=" + elsewhere, []string{"e"}},
			{"?server=" + server + "&user=" + gwUUID(t, s), []string{}},
			{"?server=" + server + "&outcome=timeout&user=" + u2, []string{}},
			{"?server=no-such-" + server, []string{}},
		} {
			if got := gwTools(gwCalls(t, admin, c.query)); !slices.Equal(got, c.want) {
				t.Errorf("%s: %v, want %v", c.query, got, c.want)
			}
		}
		// An empty answer is an empty JSON list.
		if r := admin.Do(t, "GET", "/api/calls?server=no-such-"+server, nil); strings.TrimSpace(string(r.Body)) != "[]" {
			t.Errorf("empty answer %q", r.Body)
		}
	})

	t.Run("before is a timestamp cursor (strictly older rows), limit pages through it", func(t *testing.T) {
		c := gwStamp(at(2)) // the row "c"
		for _, before := range []string{c, at(2).Format("2006-01-02T15:04:05Z"), at(2).In(time.FixedZone("", 2*3600)).Format(time.RFC3339)} {
			if got := gwTools(gwCalls(t, admin, "?server="+server+"&before="+url.QueryEscape(before))); !slices.Equal(got, []string{"b", "a"}) {
				t.Errorf("before=%s: %v, want [b a]", before, got)
			}
		}
		if got := gwTools(gwCalls(t, admin, "?server="+server+"&limit=2")); !slices.Equal(got, []string{"d", "c"}) {
			t.Errorf("limit=2: %v", got)
		}
		page := gwCalls(t, admin, "?server="+server+"&limit=2")
		next := gwCalls(t, admin, "?server="+server+"&limit=2&before="+url.QueryEscape(page[len(page)-1].StartedAt))
		if !slices.Equal(gwTools(next), []string{"b", "a"}) {
			t.Errorf("the next page: %v, want [b a]", gwTools(next))
		}
		if got := gwTools(gwCalls(t, admin, "?server="+server+"&limit=1")); !slices.Equal(got, []string{"d"}) {
			t.Errorf("limit=1: %v", got)
		}
		if got := gwCalls(t, admin, "?server="+server+"&before="+url.QueryEscape(gwStamp(at(0)))); len(got) != 0 {
			t.Errorf("before the oldest row: %v", gwTools(got))
		}
	})

	t.Run("rows that share a start time are ordered by numeric id and the pair cursor pages through them once each", func(t *testing.T) {
		tied := harness.Unique("callsapi-tie")
		old, mid, newest := base.Add(-2*time.Hour), base.Add(-time.Hour), base
		// Ids that cross digit counts (17 and 18 digits), so a text order differs from the numeric one;
		// the sequence of the table never reaches them.
		group := []string{"90000000000000001", "90000000000000002", "99999999999999999", "100000000000000000", "100000000000000001", "100000000000000002"}
		insert := func(id string, tool string, at time.Time) {
			execSQL(t, s, `insert into gateway.calls (id, user_id, server, tool, started_at, duration_ms, outcome)
				values ($1::bigint, $2::uuid, $3, $4, $5, 1, 'ok')`, id, u1, tied, tool, at)
		}
		insert("90000000000000000", "old", old)
		for _, id := range group {
			insert(id, "g"+id, mid)
		}
		insert("100000000000000003", "newest", newest)
		want := []string{"100000000000000003", "100000000000000002", "100000000000000001", "100000000000000000", "99999999999999999", "90000000000000002", "90000000000000001", "90000000000000000"}
		ids := func(rows []gwCallEntry) []string {
			out := make([]string, 0, len(rows))
			for _, e := range rows {
				out = append(out, e.ID)
			}
			return out
		}
		if got := ids(gwCalls(t, admin, "?server="+tied)); !slices.Equal(got, want) {
			t.Fatalf("order %v, want start time then numeric id, newest first: %v", got, want)
		}
		for _, limit := range []int{1, 2, 3, 5} {
			var read []string
			cursor := ""
			for page := 0; ; page++ {
				if page > len(want)+1 {
					t.Fatalf("limit=%d: the pages do not end (read %v)", limit, read)
				}
				rows := gwCalls(t, admin, fmt.Sprintf("?server=%s&limit=%d%s", tied, limit, cursor))
				if len(rows) == 0 {
					break
				}
				if len(rows) > limit {
					t.Fatalf("limit=%d: a page of %d rows", limit, len(rows))
				}
				read = append(read, ids(rows)...)
				last := rows[len(rows)-1]
				cursor = "&before=" + url.QueryEscape(last.StartedAt) + "&beforeId=" + last.ID
			}
			if !slices.Equal(read, want) {
				t.Errorf("limit=%d: the pair cursor read %v, want every row once: %v", limit, read, want)
			}
		}
		// The pair is strict: rows of the same start time with a smaller id, and older rows.
		pair := "?server=" + tied + "&before=" + url.QueryEscape(gwStamp(mid)) + "&beforeId="
		for _, c := range []struct {
			beforeID string
			want     []string
		}{
			{"100000000000000001", []string{"100000000000000000", "99999999999999999", "90000000000000002", "90000000000000001", "90000000000000000"}},
			{"90000000000000001", []string{"90000000000000000"}},
			{"90000000000000000", []string{"90000000000000000"}},
			{"1", []string{"90000000000000000"}},
			{"999999999999999999", []string{"100000000000000002", "100000000000000001", "100000000000000000", "99999999999999999", "90000000000000002", "90000000000000001", "90000000000000000"}},
		} {
			if got := ids(gwCalls(t, admin, pair+c.beforeID)); !slices.Equal(got, c.want) {
				t.Errorf("beforeId=%s: %v, want %v", c.beforeID, got, c.want)
			}
		}
		// Negative control: before alone is the strict timestamp cursor, so it skips the rows that
		// share the boundary start time (that is what the pair is for).
		if got := ids(gwCalls(t, admin, "?server="+tied+"&before="+url.QueryEscape(gwStamp(mid)))); !slices.Equal(got, []string{"90000000000000000"}) {
			t.Errorf("before alone: %v, want only the older row", got)
		}
		// The pair cursor composes with the other filters.
		if got := ids(gwCalls(t, admin, pair+"100000000000000001&outcome=ok&user="+u1+"&limit=2")); !slices.Equal(got, []string{"100000000000000000", "99999999999999999"}) {
			t.Errorf("the pair with filters and a limit: %v", got)
		}
	})

	t.Run("the default limit is 100 and the largest is 500", func(t *testing.T) {
		many := harness.Unique("callsapi-many")
		execSQL(t, s, `insert into gateway.calls (user_id, server, tool, started_at, duration_ms, outcome)
			select $1::uuid, $2, 't' || i, $3::timestamptz + i * interval '1 second', i, 'ok' from generate_series(1, 520) as i`, u1, many, base)
		if got := gwCalls(t, admin, "?server="+many); len(got) != 100 {
			t.Errorf("default: %d rows, want 100", len(got))
		}
		if got := gwCalls(t, admin, "?server="+many+"&limit=500"); len(got) != 500 {
			t.Errorf("limit=500: %d rows, want 500", len(got))
		} else if !slices.IsSortedFunc(got, func(a, b gwCallEntry) int { return strings.Compare(b.StartedAt, a.StartedAt) }) || got[0].Tool != "t520" {
			t.Errorf("not newest first: starts with %s", got[0].Tool)
		}
		if got := gwCalls(t, admin, "?server="+many+"&limit=1"); len(got) != 1 || got[0].Tool != "t520" {
			t.Errorf("limit=1: %v", got)
		}
	})

	t.Run("invalid parameters are 422 invalid_request at the parameter", func(t *testing.T) {
		ts := url.QueryEscape(gwStamp(at(2)))
		for _, c := range []struct{ query, path string }{
			{"?limit=0", "limit"},
			{"?limit=501", "limit"},
			{"?limit=-1", "limit"},
			{"?limit=abc", "limit"},
			{"?limit=1.5", "limit"},
			{"?user=not-a-uuid", "user"},
			{"?outcome=bogus", "outcome"},
			{"?outcome=OK", "outcome"},
			{"?before=yesterday", "before"},
			{"?before=2026-13-45T00:00:00Z", "before"},
			{"?before=1700000000", "before"},
			{"?server=", "server"},
			{"?server=" + strings.Repeat("s", 201), "server"},
			// beforeId pairs with before: alone it is refused, and it is a number of 1 to 18 digits.
			{"?beforeId=5", "beforeId"},
			{"?beforeId=abc&before=" + ts, "beforeId"},
			{"?beforeId=&before=" + ts, "beforeId"},
			{"?beforeId=-1&before=" + ts, "beforeId"},
			{"?beforeId=1.5&before=" + ts, "beforeId"},
			{"?beforeId=" + strings.Repeat("9", 19) + "&before=" + ts, "beforeId"},
		} {
			expectZodBodyError(t, "GET /api/calls"+c.query, admin.Do(t, "GET", "/api/calls"+c.query, nil), 422, c.path)
		}
		// Controls: the same parameters at their limits are fine.
		for _, q := range []string{"?limit=1", "?limit=500", "?server=" + strings.Repeat("s", 200), "?outcome=denied:expired&limit=3", "?before=" + ts, "?before=" + ts + "&beforeId=1", "?before=" + ts + "&beforeId=" + strings.Repeat("9", 18)} {
			gwCalls(t, admin, q)
		}
	})

	t.Run("only an admin reads the call log, before anything is parsed", func(t *testing.T) {
		const adminOnly = "forbidden|admin only"
		for _, path := range []string{"/api/calls", "/api/calls?server=" + server, "/api/calls?limit=0"} {
			if r := member.Do(t, "GET", path, nil); r.Status != 403 || errorCode(t, r) != adminOnly {
				t.Errorf("member GET %s: %d %.200s", path, r.Status, r.Body)
			}
		}
		r := s.Anonymous().Do(t, "GET", "/api/calls", nil)
		if r.Status != 401 {
			t.Errorf("anonymous: %d %.200s", r.Status, r.Body)
		}
		// Control: the admin reads it.
		if got := gwCalls(t, admin, "?server="+server); len(got) != 4 {
			t.Errorf("admin: %d rows", len(got))
		}
	})

	t.Run("a call over MCP is one row with the person, the token and the outcome; no arguments", func(t *testing.T) {
		led := newMCPLedger(t, s)
		e := gwNewServer(t, led, admin, nil, "none")
		p := gwNewPerson(t, s, led)
		marker := "compat-rest-" + harness.Unique("m")
		started := time.Now().UTC().Add(-time.Minute)
		if res := p.call(led, e.ref("echo"), map[string]any{"text": marker}); res.IsError {
			t.Fatal(catalogText(res))
		}
		p.call(led, e.ref("nope"), map[string]any{"text": marker})
		r := admin.Do(t, "GET", "/api/calls?user="+p.id, nil)
		if strings.Contains(string(r.Body), marker) {
			t.Fatalf("the response contains the arguments: %s", r.Body)
		}
		got := gwCalls(t, admin, "?user="+p.id)
		if len(got) != 2 || got[0].Tool != "nope" || got[0].Outcome != "denied:not_found" || got[1].Tool != "echo" || got[1].Outcome != "ok" {
			t.Fatalf("rows %+v", got)
		}
		for _, row := range got {
			stamp, err := time.Parse(time.RFC3339Nano, row.StartedAt)
			if err != nil || stamp.Before(started) || stamp.After(time.Now().Add(time.Minute)) || row.UserID != p.id || row.TokenID == nil || *row.TokenID != p.tokenID || row.Server != e.slug || row.DurationMs < 0 {
				t.Errorf("row %+v (started after %s)", row, started)
			}
		}
		if got[1].StartedAt > got[0].StartedAt {
			t.Errorf("the older call is listed first: %v", got)
		}
	})

	t.Run("a tool name, and a reference that is no <server>.<tool>, are cut to 200 characters in the log", func(t *testing.T) {
		// A server name is a slug (a short one), so only a tool name or a malformed reference can be
		// long; the log cuts both, the latter being kept whole as the tool of an empty server.
		led := newMCPLedger(t, s)
		p := gwNewPerson(t, s, led)
		malformed, longTool := strings.Repeat("s", 250)+".x", strings.Repeat("t", 290)
		p.call(led, malformed, map[string]any{})
		p.call(led, "srv."+longTool, map[string]any{})
		got := gwCalls(t, admin, "?user="+p.id)
		if len(got) != 2 {
			t.Fatalf("rows %+v", got)
		}
		if got[1].Server != "" || got[1].Tool != malformed[:200] || got[0].Server != "srv" || got[0].Tool != longTool[:200] {
			t.Fatalf("in the log: server/tool of %d/%d characters and %d/%d", len(got[1].Server), len(got[1].Tool), len(got[0].Server), len(got[0].Tool))
		}
		// Negative control: a tool name of exactly 200 characters is kept whole.
		p.call(led, "srv."+strings.Repeat("v", 200), map[string]any{})
		if rows := gwCalls(t, admin, "?user="+p.id+"&limit=1"); rows[0].Server != "srv" || rows[0].Tool != strings.Repeat("v", 200) {
			t.Fatalf("a name of exactly 200 characters was changed: %d/%d", len(rows[0].Server), len(rows[0].Tool))
		}
	})
}

// ---- audit ----

func TestGatewayAuditAPI(t *testing.T) {
	// A fresh stack: the whole trail of this test fits in one page of 500.
	s := harness.Start(t, harness.Options{})
	admin := s.Admin(t)
	adminID := whoami(t, admin).UserID
	member := s.Member(t, harness.Unique("audit-member"))
	memberID := whoami(t, member).UserID

	const pat = "audit-member-pat-4c1d"
	e := gwNewServer(t, nil, admin, fakes.Tools("alpha", "beta"), "pat_user", fakes.WithHeader("X-Token", "pat {secret}"))
	catalogHide(t, admin, e.slug, "beta", true)
	if msg := savePAT(t, member, e.slug, pat); msg != "" {
		t.Fatal(msg)
	}
	if r := member.Do(t, "DELETE", "/api/connections/"+e.slug, nil); r.Status != 204 {
		t.Fatalf("disconnect: %d %s", r.Status, r.Body)
	}
	tokenID := func() string {
		r := admin.Do(t, "POST", "/api/tokens", map[string]string{"name": "audit-token"})
		var c struct{ ID string }
		r.JSON(t, "CreateTokenResponse", &c)
		return c.ID
	}()

	api := auditViaAPI(t, admin)
	t.Run("the trail holds what was done, by whom, to what", func(t *testing.T) {
		find := func(action string, keep func(auditEntry) bool) []auditEntry { return auditWhere(api, action, keep) }
		one := func(what string, got []auditEntry) auditEntry {
			t.Helper()
			if len(got) != 1 {
				t.Fatalf("%s: %d records %+v, want 1", what, len(got), got)
			}
			return got[0]
		}
		target := func(want string) func(auditEntry) bool {
			return func(e auditEntry) bool { return ptrIs(e.Target, want) }
		}

		created := one("server.create", find("server.create", target(e.slug)))
		if !ptrIs(created.ActorID, adminID) || !reflect.DeepEqual(created.Meta, map[string]any{"auth": "pat_user"}) {
			t.Errorf("server.create: %+v, want the admin and meta {auth: pat_user}", created)
		}
		hidden := one("tool.hide", find("tool.hide", target(e.slug+".beta")))
		if !ptrIs(hidden.ActorID, adminID) || len(hidden.Meta) != 0 {
			t.Errorf("tool.hide: %+v, want the admin and no meta", hidden)
		}
		saved := one("the member's connection.pat_saved", find("connection.pat_saved", func(a auditEntry) bool { return ptrIs(a.Target, e.slug) && ptrIs(a.ActorID, memberID) }))
		if len(saved.Meta) != 0 {
			t.Errorf("connection.pat_saved: %+v, want the member and no meta", saved)
		}
		gone := one("connection.disconnect", find("connection.disconnect", target(e.slug)))
		if !ptrIs(gone.ActorID, memberID) || len(gone.Meta) != 0 {
			t.Errorf("connection.disconnect: %+v, want the member and no meta", gone)
		}
		tok := one("token.create", find("token.create", target(tokenID)))
		if !ptrIs(tok.ActorID, adminID) || len(tok.Meta) != 2 || tok.Meta["name"] != "audit-token" || !strings.HasPrefix(fmt.Sprint(tok.Meta["prefix"]), "sk_") {
			t.Errorf("token.create: %+v, want the admin and meta {name, prefix}", tok)
		}
		// The admin saved a token of their own for the sync: that is a pat_saved record of the admin.
		if got := find("connection.pat_saved", func(a auditEntry) bool { return ptrIs(a.Target, e.slug) && ptrIs(a.ActorID, adminID) }); len(got) != 1 {
			t.Errorf("the admin's own pat_saved records: %d, want 1", len(got))
		}
		// A secret value is not in any meta (the whole list is scanned below).
	})

	t.Run("the endpoint returns the whole trail when it fits in one page, and no secret", func(t *testing.T) {
		sql := auditAll(t, s)
		if len(api) != len(sql) {
			t.Fatalf("the API returned %d records, platform.audit_log holds %d", len(api), len(sql))
		}
		byID := map[string]auditEntry{}
		for _, a := range sql {
			byID[a.ID] = a
		}
		for _, a := range api {
			if want, ok := byID[a.ID]; !ok || !reflect.DeepEqual(a, want) {
				t.Errorf("API record %+v differs from the table's %+v", a, want)
			}
			if !gwISO.MatchString(a.At) {
				t.Errorf("at %q is not an ISO timestamp with milliseconds", a.At)
			}
		}
		raw := fmt.Sprint(mustJSON(t, api))
		for _, secret := range []string{pat, "admin-pat-" + e.slug} {
			if strings.Contains(raw, secret) {
				t.Errorf("the audit list contains the personal access token %s", harness.Redact(secret))
			}
		}
	})

	t.Run("limit is 1 to 500", func(t *testing.T) {
		for _, c := range []struct {
			query string
			n     int // expected length, -1 = any
		}{{"?limit=1", 1}, {"?limit=2", 2}, {"?limit=500", len(api)}, {"", len(api)}} {
			r := admin.Do(t, "GET", "/api/audit"+c.query, nil)
			var got []auditEntry
			r.JSON(t, "AuditList", &got)
			if r.Status != 200 || len(got) != c.n {
				t.Errorf("GET /api/audit%s: %d, %d records, want %d", c.query, r.Status, len(got), c.n)
			}
			for _, a := range got { // whatever the order, a record is a record of the trail
				if !slices.ContainsFunc(api, func(x auditEntry) bool { return x.ID == a.ID }) {
					t.Errorf("a record that is not in the trail: %+v", a)
				}
			}
		}
		for _, q := range []string{"?limit=0", "?limit=501", "?limit=-1", "?limit=abc", "?before=abc", "?before=-1", "?before=" + strings.Repeat("9", 19)} {
			path := "limit"
			if strings.HasPrefix(q, "?before") {
				path = "before"
			}
			expectZodBodyError(t, "GET /api/audit"+q, admin.Do(t, "GET", "/api/audit"+q, nil), 422, path)
		}
		// Controls: the largest accepted cursor is a number of 18 digits.
		if r := admin.Do(t, "GET", "/api/audit?before="+strings.Repeat("9", 18), nil); r.Status != 200 {
			t.Errorf("before with 18 digits: %d %.200s", r.Status, r.Body)
		}
	})

	t.Run("order and paging", func(t *testing.T) {
		// Enough records that the ids cross from one to two digits whatever the stack did before.
		for n := len(auditAll(t, s)); n < 15; n++ {
			if r := admin.Do(t, "POST", "/api/tokens", map[string]string{"name": fmt.Sprintf("audit-page-%d", n)}); r.Status != 201 && r.Status != 200 {
				t.Fatalf("POST /api/tokens: %d %s", r.Status, r.Body)
			}
		}
		const limit = 4
		var read []string
		before := ""
		for page := 0; ; page++ {
			if page > 100 {
				t.Fatal("the audit pages do not end")
			}
			q := fmt.Sprintf("?limit=%d", limit)
			if before != "" {
				q += "&before=" + before
			}
			r := admin.Do(t, "GET", "/api/audit"+q, nil)
			var got []auditEntry
			r.JSON(t, "AuditList", &got)
			if r.Status != 200 {
				t.Fatalf("GET /api/audit%s: %d", q, r.Status)
			}
			if len(got) == 0 {
				break
			}
			if len(got) > limit {
				t.Fatalf("GET /api/audit%s returned %d records, limit %d", q, len(got), limit)
			}
			for _, a := range got {
				read = append(read, a.ID)
			}
			before = got[len(got)-1].ID
		}
		var want []string
		for _, a := range auditAll(t, s) {
			want = append(want, a.ID)
		}
		if len(want) < 12 {
			t.Fatalf("only %d audit records, the scenario needs at least 12", len(want))
		}
		if problem := auditPagingProblem(read, want); problem != "" {
			t.Errorf("paging through GET /api/audit?limit=%d: %s (read %v)", limit, problem, read)
		}
		// A page of the default size is newest first as well.
		var first []auditEntry
		admin.Do(t, "GET", "/api/audit", nil).JSON(t, "AuditList", &first)
		if len(first) == 0 || first[0].ID != want[0] {
			t.Errorf("the first record of the default page is not the newest (%s)", want[0])
		}
	})

	t.Run("only an admin reads the audit trail", func(t *testing.T) {
		for _, q := range []string{"", "?limit=1", "?limit=0"} {
			if r := member.Do(t, "GET", "/api/audit"+q, nil); r.Status != 403 || errorCode(t, r) != "forbidden|admin only" {
				t.Errorf("member GET /api/audit%s: %d %.200s", q, r.Status, r.Body)
			}
		}
		if r := s.Anonymous().Do(t, "GET", "/api/audit", nil); r.Status != 401 {
			t.Errorf("anonymous: %d %.200s", r.Status, r.Body)
		}
		if r := admin.Do(t, "GET", "/api/audit", nil); r.Status != 200 {
			t.Errorf("admin: %d", r.Status)
		}
	})
}
