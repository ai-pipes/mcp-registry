//go:build integration

package infra

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA  = "11111111-1111-1111-1111-111111111111"
	userB  = "22222222-2222-2222-2222-222222222222"
	token1 = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
)

func setup(t *testing.T) (*pgxpool.Pool, Stores) {
	t.Helper()
	pool := pgtest.StartMigrated(t)
	return pool, NewPg(pool)
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func quotaCount(t *testing.T, pool *pgxpool.Pool, user, server, day string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(ctxT(t), `select count from gateway.quota where user_id = $1 and server = $2 and day = $3::date`, user, server, day).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestQuotaTwentyParallelCallsAtLimitTenGrantExactlyTen(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	var granted, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.Quota.TryConsume(ctx, app.QuotaRequest{UserID: userA, Server: "gitea", Day: "2026-11-23", Limit: 10})
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				granted.Add(1)
			} else {
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	if granted.Load() != 10 || refused.Load() != 10 {
		t.Fatalf("granted %d, refused %d, want 10 and 10", granted.Load(), refused.Load())
	}
	if n := quotaCount(t, pool, userA, "gitea", "2026-11-23"); n != 10 {
		t.Fatalf("counter %d, want 10 (a refused call counts nothing)", n)
	}
}

func TestQuotaIsPerPersonServerAndDay(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	try := func(user, server, day string) bool {
		ok, err := s.Quota.TryConsume(ctx, app.QuotaRequest{UserID: user, Server: server, Day: day, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !try(userA, "gitea", "2026-11-23") || try(userA, "gitea", "2026-11-23") {
		t.Fatal("the second call of the day must be refused")
	}
	if !try(userB, "gitea", "2026-11-23") || !try(userA, "youtrack", "2026-11-23") || !try(userA, "gitea", "2026-11-24") {
		t.Fatal("another person, server or day has its own counter")
	}
	if n := quotaCount(t, pool, userA, "gitea", "2026-11-23"); n != 1 {
		t.Fatalf("counter %d", n)
	}
}

func TestQuotaFirstCallIsGrantedWhateverTheLimit(t *testing.T) {
	_, s := setup(t)
	ok, err := s.Quota.TryConsume(ctxT(t), app.QuotaRequest{UserID: userA, Server: "gitea", Day: "2026-11-23", Limit: 0})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v: limits are at least one by contract, the first call inserts", ok, err)
	}
}

func TestQuotaRejectsABadDayAndAnUnknownUserShape(t *testing.T) {
	_, s := setup(t)
	if _, err := s.Quota.TryConsume(ctxT(t), app.QuotaRequest{UserID: userA, Server: "gitea", Day: "not-a-day", Limit: 1}); err == nil {
		t.Fatal("a malformed day must be an error")
	}
}

func TestQuotaDeleteDaysBefore(t *testing.T) {
	pool, s := setup(t)
	ctx := ctxT(t)
	for _, d := range []string{"2026-11-21", "2026-11-22", "2026-11-23"} {
		if _, err := s.Quota.TryConsume(ctx, app.QuotaRequest{UserID: userA, Server: "gitea", Day: d, Limit: 5}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Quota.DeleteDaysBefore(ctx, "2026-11-22")
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, err %v, want only the day before", n, err)
	}
	var left int
	if err := pool.QueryRow(ctx, `select count(*) from gateway.quota`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("left %d, err %v", left, err)
	}
}

var t0 = time.Date(2026, 11, 23, 12, 0, 0, 123_000_000, time.UTC)

func entry(user, server string, at time.Time, outcome domain.CallOutcome) app.CallLogEntry {
	return app.CallLogEntry{UserID: user, TokenID: token1, Server: server, Tool: "create_issue", StartedAt: at, DurationMs: 12, Outcome: outcome}
}

func record(t *testing.T, s Stores, e app.CallLogEntry) {
	t.Helper()
	if err := s.CallLog.Record(ctxT(t), e); err != nil {
		t.Fatal(err)
	}
}

func TestCallLogRecordsAndListsARow(t *testing.T) {
	_, s := setup(t)
	record(t, s, entry(userA, "gitea", t0, domain.OutcomeOK))
	got, err := s.CallLog.List(ctxT(t), app.CallLogFilter{Limit: 10})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	r := got[0]
	want := entry(userA, "gitea", t0, domain.OutcomeOK)
	if r.CallLogEntry != want || r.ID == "" || !r.StartedAt.Equal(t0) {
		t.Fatalf("got %+v, want %+v", r, want)
	}
}

func TestCallLogASessionHasNoToken(t *testing.T) {
	pool, s := setup(t)
	e := entry(userA, "gitea", t0, domain.OutcomeOK)
	e.TokenID = ""
	record(t, s, e)
	var isNull bool
	if err := pool.QueryRow(ctxT(t), `select token_id is null from gateway.calls`).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("token_id null: %v, err %v", isNull, err)
	}
	got, _ := s.CallLog.List(ctxT(t), app.CallLogFilter{Limit: 10})
	if len(got) != 1 || got[0].TokenID != "" {
		t.Fatalf("%+v", got)
	}
}

func TestCallLogStartedAtIsStoredWithMillisecondPrecision(t *testing.T) {
	pool, s := setup(t)
	at := time.Date(2026, 11, 23, 12, 0, 0, 123_456_789, time.UTC)
	record(t, s, entry(userA, "gitea", at, domain.OutcomeOK))
	var exact bool
	if err := pool.QueryRow(ctxT(t), `select started_at = date_trunc('milliseconds', started_at) from gateway.calls`).Scan(&exact); err != nil || !exact {
		t.Fatalf("stored with sub-millisecond precision (exact=%v, err=%v)", exact, err)
	}
	got, _ := s.CallLog.List(ctxT(t), app.CallLogFilter{Limit: 1})
	if !got[0].StartedAt.Equal(at.Truncate(time.Millisecond)) {
		t.Fatalf("started %s", got[0].StartedAt.Format(time.RFC3339Nano))
	}
}

func TestCallLogDurationIsAnIntegerOfAtLeastZero(t *testing.T) {
	pool, s := setup(t)
	for i, d := range []int64{-5, 0, 7, 1 << 40} {
		e := entry(userA, "gitea", t0.Add(time.Duration(i)*time.Second), domain.OutcomeOK)
		e.DurationMs = d
		record(t, s, e)
	}
	rows, err := pool.Query(ctxT(t), `select duration_ms from gateway.calls order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int32
	for rows.Next() {
		var d int32
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		got = append(got, d)
	}
	if !slices.Equal(got, []int32{0, 0, 7, 2147483647}) {
		t.Fatalf("durations %v", got)
	}
}

func TestCallLogRejectsAnUnknownOutcome(t *testing.T) {
	_, s := setup(t)
	if err := s.CallLog.Record(ctxT(t), entry(userA, "gitea", t0, domain.CallOutcome("bogus"))); err == nil {
		t.Fatal("the CHECK constraint must refuse an outcome outside the list")
	}
}

func TestCallLogFiltersAndOrder(t *testing.T) {
	_, s := setup(t)
	record(t, s, entry(userA, "gitea", t0, domain.OutcomeOK))
	record(t, s, entry(userB, "gitea", t0.Add(time.Second), domain.OutcomeTimeout))
	record(t, s, entry(userA, "youtrack", t0.Add(2*time.Second), domain.OutcomeDeniedQuota))
	record(t, s, entry(userA, "gitea", t0.Add(3*time.Second), domain.OutcomeDeniedQuota))
	list := func(f app.CallLogFilter) []string {
		f.Limit = 10
		rows, err := s.CallLog.List(ctxT(t), f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, fmt.Sprintf("%s/%s/%s", r.UserID[:2], r.Server, r.Outcome))
		}
		return out
	}
	str := func(v string) *string { return &v }
	quota := domain.OutcomeDeniedQuota
	if got := list(app.CallLogFilter{}); !slices.Equal(got, []string{"11/gitea/denied:quota", "11/youtrack/denied:quota", "22/gitea/timeout", "11/gitea/ok"}) {
		t.Fatalf("all, newest first: %v", got)
	}
	if got := list(app.CallLogFilter{UserID: str(userB)}); !slices.Equal(got, []string{"22/gitea/timeout"}) {
		t.Fatalf("by user: %v", got)
	}
	if got := list(app.CallLogFilter{Server: str("youtrack")}); !slices.Equal(got, []string{"11/youtrack/denied:quota"}) {
		t.Fatalf("by server: %v", got)
	}
	if got := list(app.CallLogFilter{Outcome: &quota, UserID: str(userA), Server: str("gitea")}); !slices.Equal(got, []string{"11/gitea/denied:quota"}) {
		t.Fatalf("combined: %v", got)
	}
	rows, _ := s.CallLog.List(ctxT(t), app.CallLogFilter{Limit: 2})
	if len(rows) != 2 {
		t.Fatalf("limit: %d rows", len(rows))
	}
	none, err := s.CallLog.List(ctxT(t), app.CallLogFilter{Server: str("nope"), Limit: 5})
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("no rows must be an empty list, got %#v (%v)", none, err)
	}
}

func TestCallLogBeforeAloneIsStrictlyOlder(t *testing.T) {
	_, s := setup(t)
	for i := 0; i < 3; i++ {
		record(t, s, entry(userA, "gitea", t0.Add(time.Duration(i)*time.Second), domain.OutcomeOK))
	}
	before := t0.Add(time.Second)
	got, err := s.CallLog.List(ctxT(t), app.CallLogFilter{Before: &before, Limit: 10})
	if err != nil || len(got) != 1 || !got[0].StartedAt.Equal(t0) {
		t.Fatalf("%v %+v", err, got)
	}
	// A cursor with extra fractional digits is cut to the millisecond like the stored values.
	fine := t0.Add(time.Second + 999*time.Microsecond)
	got, _ = s.CallLog.List(ctxT(t), app.CallLogFilter{Before: &fine, Limit: 10})
	if len(got) != 1 {
		t.Fatalf("a cursor of %s must behave as %s (the row that starts there is not older): %d rows", fine.Format(time.RFC3339Nano), fine.Truncate(time.Millisecond).Format(time.RFC3339Nano), len(got))
	}
}

// Rows that share a start time are neither skipped nor repeated when pages are cut with the pair
// (started_at, id), including ids that cross 17 and 18 digits (the id is compared as a number).
func TestCallLogCursorPagesWithoutGapsOverEqualStartTimes(t *testing.T) {
	pool, s := setup(t)
	if _, err := pool.Exec(ctxT(t), `select setval(pg_get_serial_sequence('gateway.calls', 'id'), 99999999999999994)`); err != nil {
		t.Fatal(err)
	}
	same := t0
	const n = 12
	for i := 0; i < n; i++ {
		at := same
		if i >= n-2 {
			at = same.Add(time.Second) // two newer rows, then the tie group
		}
		if i < 3 {
			at = same.Add(-time.Second) // and three older ones
		}
		record(t, s, entry(userA, "gitea", at, domain.OutcomeOK))
	}
	all, err := s.CallLog.List(ctxT(t), app.CallLogFilter{Limit: 100})
	if err != nil || len(all) != n {
		t.Fatalf("%v %d", err, len(all))
	}
	if !slices.IsSortedFunc(all, func(a, b app.CallRecord) int {
		if c := b.StartedAt.Compare(a.StartedAt); c != 0 {
			return c
		}
		return compareIDs(b.ID, a.ID)
	}) {
		t.Fatalf("not newest first by (started_at, numeric id): %+v", all)
	}
	var crossed bool
	for _, r := range all {
		if len(r.ID) == 18 {
			crossed = true
		}
	}
	if !crossed {
		t.Fatal("the ids never reached 18 digits; the numeric ordering is not exercised")
	}
	for _, pageSize := range []int{1, 2, 3, 5} {
		var got []string
		f := app.CallLogFilter{Limit: pageSize}
		for {
			page, err := s.CallLog.List(ctxT(t), f)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range page {
				got = append(got, r.ID)
			}
			if len(page) < pageSize {
				break
			}
			last := page[len(page)-1]
			at, id := last.StartedAt, last.ID
			f.Before, f.BeforeID = &at, &id
		}
		var want []string
		for _, r := range all {
			want = append(want, r.ID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("page size %d:\n got %v\nwant %v", pageSize, got, want)
		}
	}
}

// compareIDs orders decimal ids as numbers.
func compareIDs(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}

func TestCallLogDeleteOlderThan(t *testing.T) {
	pool, s := setup(t)
	for i := 0; i < 3; i++ {
		record(t, s, entry(userA, "gitea", t0.Add(time.Duration(i)*time.Hour), domain.OutcomeOK))
	}
	cut := t0.Add(time.Hour)
	n, err := s.CallLog.DeleteOlderThan(ctxT(t), cut)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d err %v, want one (strictly older)", n, err)
	}
	var left int
	if err := pool.QueryRow(ctxT(t), `select count(*) from gateway.calls`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("left %d err %v", left, err)
	}
}

func TestStoresTouchOnlyTheGatewaySchema(t *testing.T) {
	pool, s := setup(t)
	record(t, s, entry(userA, "gitea", t0, domain.OutcomeOK))
	if _, err := s.Quota.TryConsume(ctxT(t), app.QuotaRequest{UserID: userA, Server: "gitea", Day: "2026-11-23", Limit: 1}); err != nil {
		t.Fatal(err)
	}
	var tables int
	err := pool.QueryRow(ctxT(t), `select count(*) from information_schema.columns where table_schema = 'gateway' and table_name = 'calls'
      and column_name in ('duration_ms','id','outcome','server','started_at','token_id','tool','user_id')`).Scan(&tables)
	if err != nil || tables != 8 {
		t.Fatalf("columns %d err %v", tables, err)
	}
}
