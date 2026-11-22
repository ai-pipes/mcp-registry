//go:build integration

package outbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const wait = 30 * time.Second

func fresh(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return pgtest.StartMigrated(t)
}

func appendEvents(t *testing.T, pool *pgxpool.Pool, events ...kernel.Event) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Append(ctx, tx, events); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func ev(typ string, n int) kernel.Event {
	return kernel.Event{Type: typ, Payload: map[string]any{"n": float64(n)}}
}

// fakeTimers hands each interval timer to the test, which fires it by hand: a request means the
// worker finished a pass.
type fakeTimers struct {
	reqs chan chan time.Time
	mu   sync.Mutex
	now  time.Time
}

func newFakeTimers() *fakeTimers {
	return &fakeTimers{reqs: make(chan chan time.Time, 64), now: time.Unix(1_000_000, 0)}
}

func (f *fakeTimers) clock() clock {
	return clock{
		now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
		newTimer: func(time.Duration) (<-chan time.Time, func()) {
			c := make(chan time.Time, 1)
			f.reqs <- c
			return c, func() {}
		},
	}
}

func (f *fakeTimers) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

// passDone waits until the worker has finished a pass and returns the handle to start the next.
func (f *fakeTimers) passDone(t *testing.T) chan time.Time {
	t.Helper()
	select {
	case c := <-f.reqs:
		return c
	case <-time.After(wait):
		t.Fatal("the worker did not finish a pass")
		return nil
	}
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(wait):
		t.Fatalf("timed out: %s", what)
		var z T
		return z
	}
}

func stopWorker(t *testing.T, stop func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if err := stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAppendNothingIsNoQuery(t *testing.T) {
	// A nil transaction would panic on any query.
	if err := Append(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestEventIsInvisibleUntilCommitThenDelivered(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	timers := newFakeTimers()
	got := make(chan map[string]any, 4)
	stop := startWorker(WorkerOptions{Pool: pool, Interval: time.Hour, Handlers: map[string]Handler{
		"t": func(_ context.Context, p map[string]any) error { got <- p; return nil },
	}}, timers.clock())
	defer stopWorker(t, stop)
	next := timers.passDone(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Append(ctx, tx, []kernel.Event{{Type: "t", Payload: map[string]any{"k": "v", "nested": map[string]any{"a": []any{1.0}}}}}); err != nil {
		t.Fatal(err)
	}
	next <- time.Time{}
	next = timers.passDone(t)
	select {
	case p := <-got:
		t.Fatalf("delivered before commit: %v", p)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	next <- time.Time{}
	p := recv(t, got, "delivery")
	if p["k"] != "v" || p["nested"].(map[string]any)["a"].([]any)[0] != 1.0 {
		t.Fatalf("payload = %v", p)
	}
	timers.passDone(t)
	var processed bool
	var lastErr *string
	if err := pool.QueryRow(ctx, `select processed_at is not null, last_error from platform.outbox`).Scan(&processed, &lastErr); err != nil {
		t.Fatal(err)
	}
	if !processed || lastErr != nil {
		t.Fatalf("processed=%v last_error=%v", processed, lastErr)
	}
}

func TestRolledBackEventsAreGone(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Append(ctx, tx, []kernel.Event{ev("t", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from platform.outbox`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestDeliveryOrder(t *testing.T) {
	pool := fresh(t)
	var evs []kernel.Event
	for i := 0; i < 25; i++ { // more than one batch
		evs = append(evs, ev("t", i))
	}
	appendEvents(t, pool, evs...)
	timers := newFakeTimers()
	got := make(chan int, 64)
	stop := startWorker(WorkerOptions{Pool: pool, Interval: time.Hour, Handlers: map[string]Handler{
		"t": func(_ context.Context, p map[string]any) error { got <- int(p["n"].(float64)); return nil },
	}}, timers.clock())
	defer stopWorker(t, stop)
	next := timers.passDone(t)
	next <- time.Time{}
	timers.passDone(t)
	for i := 0; i < 25; i++ {
		if n := recv(t, got, "event"); n != i {
			t.Fatalf("event %d delivered at position %d", n, i)
		}
	}
}

func TestFailureBacksOffAndStoresOnlyALabel(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	appendEvents(t, pool, ev("t", 1))
	// The claim makes it the third attempt: the wait is 2^3 = 8 s (4 or 16 would be a wrong exponent).
	if _, err := pool.Exec(ctx, `update platform.outbox set attempts = 2`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	timers := newFakeTimers()
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("info", &buf),
		Handlers: map[string]Handler{"t": func(context.Context, map[string]any) error {
			return errors.New("secret-value-123")
		}},
	}, timers.clock())
	defer stopWorker(t, stop)
	timers.passDone(t)

	var attempts int
	var lastErr string
	var delay float64
	var processed bool
	if err := pool.QueryRow(ctx, `select attempts, last_error, extract(epoch from next_attempt_at - now()),
		processed_at is not null from platform.outbox`).Scan(&attempts, &lastErr, &delay, &processed); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || processed || lastErr != "errors.errorString" {
		t.Fatalf("attempts=%d processed=%v last_error=%q", attempts, processed, lastErr)
	}
	if delay < 7 || delay > 8.5 {
		t.Fatalf("next attempt in %vs", delay)
	}
	out := buf.String()
	if strings.Contains(out, "secret-value-123") || strings.Contains(lastErr, "secret") {
		t.Fatalf("message leaked: %s", out)
	}
	for _, want := range []string{`"msg":"outbox: handler failed, will retry"`, `"outboxId":"`, `"type":"t"`, `"attempts":3`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %s: %s", want, out)
		}
	}
}

func TestTenFailuresStopClaiming(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	appendEvents(t, pool, ev("t", 1))
	if _, err := pool.Exec(ctx, `update platform.outbox set attempts = 9`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	timers := newFakeTimers()
	calls := atomic.Int32{}
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("info", &buf),
		Handlers: map[string]Handler{"t": func(context.Context, map[string]any) error {
			calls.Add(1)
			return errors.New("nope")
		}},
	}, timers.clock())
	defer stopWorker(t, stop)
	next := timers.passDone(t)
	if calls.Load() != 1 || !strings.Contains(buf.String(), `"msg":"outbox: handler failed, giving up"`) {
		t.Fatalf("calls=%d log=%s", calls.Load(), buf.String())
	}
	if _, err := pool.Exec(ctx, `update platform.outbox set next_attempt_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	next <- time.Time{}
	timers.passDone(t)
	rows, err := claim(ctx, pool, 20)
	if err != nil || len(rows) != 0 || calls.Load() != 1 {
		t.Fatalf("rows=%d err=%v calls=%d", len(rows), err, calls.Load())
	}
}

func TestClaimsAreDisjointWhileLeased(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	var evs []kernel.Event
	for i := 0; i < 30; i++ {
		evs = append(evs, ev("t", i))
	}
	appendEvents(t, pool, evs...)
	a, err := claim(ctx, pool, 20)
	if err != nil {
		t.Fatal(err)
	}
	b, err := claim(ctx, pool, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 20 || len(b) != 10 {
		t.Fatalf("claimed %d and %d", len(a), len(b))
	}
	seen := map[int64]bool{}
	for _, r := range append(a, b...) {
		if seen[r.id] || r.attempts != 1 {
			t.Fatalf("row %d claimed twice or attempts=%d", r.id, r.attempts)
		}
		seen[r.id] = true
	}
}

func TestTwoWorkersDeliverEachEventOnce(t *testing.T) {
	pool := fresh(t)
	const n = 60
	var evs []kernel.Event
	for i := 0; i < n; i++ {
		evs = append(evs, ev("t", i))
	}
	appendEvents(t, pool, evs...)
	var mu sync.Mutex
	counts := map[int]int{}
	h := map[string]Handler{"t": func(_ context.Context, p map[string]any) error {
		mu.Lock()
		counts[int(p["n"].(float64))]++
		mu.Unlock()
		return nil
	}}
	var stops []func(context.Context) error
	var timersList []*fakeTimers
	for i := 0; i < 2; i++ {
		tm := newFakeTimers()
		timersList = append(timersList, tm)
		stops = append(stops, startWorker(WorkerOptions{Pool: pool, Interval: time.Hour, Handlers: h}, tm.clock()))
	}
	nexts := make([]chan time.Time, len(timersList))
	for i, tm := range timersList {
		nexts[i] = tm.passDone(t)
	}
	// Three batches of 20 are enough for 60 events whichever worker takes which.
	for round := 0; round < 3; round++ {
		for i := range timersList {
			nexts[i] <- time.Time{}
		}
		for i, tm := range timersList {
			nexts[i] = tm.passDone(t)
		}
	}
	for _, s := range stops {
		stopWorker(t, s)
	}
	var left int
	if err := pool.QueryRow(context.Background(), `select count(*) from platform.outbox where processed_at is null`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	total := 0
	for i, c := range counts {
		if c != 1 {
			t.Fatalf("event %d delivered %d times", i, c)
		}
		total++
	}
	if left != 0 || total != n {
		t.Fatalf("delivered %d, left %d, want %d", total, left, n)
	}
}

func TestStopDuringHandlerWaitsAndDeliversNothingAfter(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	appendEvents(t, pool, ev("t", 1), ev("t", 2), ev("t", 3))
	timers := newFakeTimers()
	started := make(chan context.Context, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	stop := startWorker(WorkerOptions{Pool: pool, Interval: time.Hour, Handlers: map[string]Handler{
		"t": func(hctx context.Context, _ map[string]any) error {
			calls.Add(1)
			started <- hctx
			<-release
			return nil
		},
	}}, timers.clock())
	hctx := recv(t, started, "handler start")

	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := stop(gone); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop with a dead context = %v", err)
	}
	select {
	case <-hctx.Done():
	case <-time.After(wait):
		t.Fatal("handler context not cancelled by stop")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- stop(ctx) }()
	select {
	case err := <-stopped:
		t.Fatalf("stop returned while the handler ran: %v", err)
	default:
	}
	close(release)
	if err := recv(t, stopped, "stop"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler called %d times", calls.Load())
	}
	rows, err := pool.Query(ctx, `select id, processed_at is not null, attempts from platform.outbox order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seen []string
	for rows.Next() {
		var id int64
		var processed bool
		var attempts int
		if err := rows.Scan(&id, &processed, &attempts); err != nil {
			t.Fatal(err)
		}
		if processed {
			seen = append(seen, "processed")
		} else if attempts != 0 {
			t.Fatalf("undelivered row %d keeps attempts=%d", id, attempts)
		} else {
			seen = append(seen, "pending")
		}
	}
	if strings.Join(seen, ",") != "processed,pending,pending" {
		t.Fatalf("rows = %v", seen)
	}
	if err := stop(ctx); err != nil {
		t.Fatalf("second stop = %v", err)
	}
}

func TestEventWithoutHandlerIsMarkedProcessed(t *testing.T) {
	pool := fresh(t)
	appendEvents(t, pool, ev("unknown", 1))
	var buf bytes.Buffer
	timers := newFakeTimers()
	stop := startWorker(WorkerOptions{Pool: pool, Interval: time.Hour, Logger: logging.New("info", &buf)}, timers.clock())
	defer stopWorker(t, stop)
	timers.passDone(t)
	var processed bool
	if err := pool.QueryRow(context.Background(), `select processed_at is not null from platform.outbox`).Scan(&processed); err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	if !strings.Contains(buf.String(), `"msg":"outbox: no handler, skipping"`) {
		t.Fatalf("log: %s", buf.String())
	}
}

func TestPanickingHandlerIsAFailedHandler(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	appendEvents(t, pool, ev("p", 1), ev("ok", 2))
	var buf bytes.Buffer
	timers := newFakeTimers()
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("info", &buf),
		Handlers: map[string]Handler{
			"p":  func(context.Context, map[string]any) error { panic("leaky-panic-value") },
			"ok": func(context.Context, map[string]any) error { return nil },
		},
	}, timers.clock())
	defer stopWorker(t, stop)
	timers.passDone(t)
	var failed, okDone bool
	if err := pool.QueryRow(ctx, `select
		(select attempts = 1 and processed_at is null and last_error is not null from platform.outbox where type = 'p'),
		(select processed_at is not null from platform.outbox where type = 'ok')`).Scan(&failed, &okDone); err != nil {
		t.Fatal(err)
	}
	if !failed || !okDone || strings.Contains(buf.String(), "leaky-panic-value") {
		t.Fatalf("failed=%v okDone=%v log=%s", failed, okDone, buf.String())
	}
}

func TestSweepRunsOnFirstPassThenAtMostOncePerEvery(t *testing.T) {
	pool := fresh(t)
	timers := newFakeTimers()
	var runs atomic.Int32
	var buf bytes.Buffer
	var fail atomic.Bool
	fail.Store(true)
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("info", &buf),
		Sweep: &Sweep{Every: 10 * time.Minute, Run: func(context.Context) error {
			runs.Add(1)
			if fail.Load() {
				return errors.New("sweep secret")
			}
			return nil
		}},
	}, timers.clock())
	defer stopWorker(t, stop)

	next := timers.passDone(t)
	if runs.Load() != 1 {
		t.Fatalf("runs after first pass = %d", runs.Load())
	}
	if !strings.Contains(buf.String(), `"msg":"outbox: sweep failed"`) || strings.Contains(buf.String(), "sweep secret") {
		t.Fatalf("log: %s", buf.String())
	}
	next <- time.Time{}
	next = timers.passDone(t)
	timers.advance(9 * time.Minute)
	next <- time.Time{}
	next = timers.passDone(t)
	if runs.Load() != 1 {
		t.Fatalf("sweep ran again before Every: %d", runs.Load())
	}
	timers.advance(time.Minute)
	fail.Store(false)
	next <- time.Time{}
	timers.passDone(t)
	if runs.Load() != 2 {
		t.Fatalf("sweep after Every: %d", runs.Load())
	}
}

func TestBackoffIsCappedAt300Seconds(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	appendEvents(t, pool, ev("t", 1))
	if _, err := pool.Exec(ctx, `update platform.outbox set attempts = 8`); err != nil { // claim: 9, 2^9 > 300
		t.Fatal(err)
	}
	timers := newFakeTimers()
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("silent", io.Discard),
		Handlers: map[string]Handler{"t": func(context.Context, map[string]any) error { return errors.New("x") }},
	}, timers.clock())
	defer stopWorker(t, stop)
	timers.passDone(t)
	var delay float64
	if err := pool.QueryRow(ctx, `select extract(epoch from next_attempt_at - now()) from platform.outbox`).Scan(&delay); err != nil {
		t.Fatal(err)
	}
	if delay < 295 || delay > 301 {
		t.Fatalf("next attempt in %vs, want about 300", delay)
	}
}

func TestClaimSkipsRowsLockedByAnotherTransaction(t *testing.T) {
	pool := fresh(t)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var evs []kernel.Event
	for i := 0; i < 30; i++ {
		evs = append(evs, ev("t", i))
	}
	appendEvents(t, pool, evs...)
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	locked, err := holder.Query(ctx, `select id from platform.outbox order by id limit 10 for update`)
	if err != nil {
		t.Fatal(err)
	}
	lockedIDs, err := pgx.CollectRows(locked, pgx.RowTo[int64])
	if err != nil || len(lockedIDs) != 10 {
		t.Fatalf("locked %d rows, err=%v", len(lockedIDs), err)
	}
	// With the holder still open, a plain wait would block until the context ends.
	rows, err := claim(ctx, pool, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 20 {
		t.Fatalf("claimed %d rows", len(rows))
	}
	for _, r := range rows {
		if slices.Contains(lockedIDs, r.id) {
			t.Fatalf("claimed locked row %d", r.id)
		}
	}
}

func TestReleaseIsCompareAndSet(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	appendEvents(t, pool, ev("t", 1), ev("t", 2))
	rows, err := claim(ctx, pool, 20)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	// Another replica claims the first row after its lease ran out.
	if _, err := pool.Exec(ctx, `update platform.outbox set attempts = attempts + 1,
		next_attempt_at = now() + interval '60 seconds' where id = $1`, rows[0].id); err != nil {
		t.Fatal(err)
	}
	if err := release(ctx, pool, rows); err != nil {
		t.Fatal(err)
	}
	attempts := map[int64]int{}
	r, err := pool.Query(ctx, `select id, attempts from platform.outbox`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		var id int64
		var a int
		if err := r.Scan(&id, &a); err != nil {
			t.Fatal(err)
		}
		attempts[id] = a
	}
	if attempts[rows[0].id] != 2 || attempts[rows[1].id] != 0 {
		t.Fatalf("attempts after release = %v", attempts)
	}
}

func TestHandlerFailingBecauseOfStopIsNotAFailedAttempt(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	appendEvents(t, pool, ev("t", 1))
	var buf bytes.Buffer
	timers := newFakeTimers()
	started := make(chan struct{})
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("info", &buf),
		Handlers: map[string]Handler{"t": func(hctx context.Context, _ map[string]any) error {
			close(started)
			<-hctx.Done()
			return hctx.Err()
		}},
	}, timers.clock())
	recv(t, started, "handler start")
	stopWorker(t, stop)
	var attempts int
	var lastErr *string
	var processed bool
	if err := pool.QueryRow(ctx, `select attempts, last_error, processed_at is not null from platform.outbox`).Scan(&attempts, &lastErr, &processed); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || lastErr != nil || processed {
		t.Fatalf("attempts=%d last_error=%v processed=%v", attempts, lastErr, processed)
	}
	if strings.Contains(buf.String(), "handler failed") {
		t.Fatalf("log: %s", buf.String())
	}
}

func TestSlowSuccessfulHandlerIsStillMarkedProcessed(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	appendEvents(t, pool, ev("t", 1))
	timers := newFakeTimers()
	clk := timers.clock()
	clk.outcomeTimeout = 150 * time.Millisecond
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("silent", io.Discard),
		Handlers: map[string]Handler{"t": func(context.Context, map[string]any) error {
			<-time.After(3 * clk.outcomeTimeout) // the handler alone outlasts the outcome timeout
			return nil
		}},
	}, clk)
	defer stopWorker(t, stop)
	next := timers.passDone(t)
	if _, err := pool.Exec(ctx, `update platform.outbox set next_attempt_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	next <- time.Time{}
	timers.passDone(t)
	var processed bool
	var attempts int
	if err := pool.QueryRow(ctx, `select processed_at is not null, attempts from platform.outbox`).Scan(&processed, &attempts); err != nil {
		t.Fatal(err)
	}
	if !processed || attempts != 1 {
		t.Fatalf("processed=%v attempts=%d (redelivered or not recorded)", processed, attempts)
	}
}

func panicHere() { panic("leaky-panic-value") }

func TestPanicRecordNamesTheFunctionNotTheValue(t *testing.T) {
	pool := fresh(t)
	appendEvents(t, pool, ev("p", 1))
	var buf bytes.Buffer
	timers := newFakeTimers()
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("info", &buf),
		Handlers: map[string]Handler{"p": func(context.Context, map[string]any) error { panicHere(); return nil }},
	}, timers.clock())
	defer stopWorker(t, stop)
	timers.passDone(t)
	out := buf.String()
	if !strings.Contains(out, `"msg":"outbox: handler panicked"`) || !strings.Contains(out, "outbox.panicHere") ||
		!strings.Contains(out, "outbox_int_test.go:") {
		t.Fatalf("log: %s", out)
	}
	if strings.Contains(out, "leaky-panic-value") || strings.Contains(out, "/Users/") {
		t.Fatalf("log leaks: %s", out)
	}
}

func TestSweepRunsAfterTheDeliveriesOfThePass(t *testing.T) {
	pool := fresh(t)
	appendEvents(t, pool, ev("t", 1), ev("t", 2))
	timers := newFakeTimers()
	var delivered atomic.Int32
	var seenAtSweep atomic.Int32
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("silent", io.Discard),
		Handlers: map[string]Handler{"t": func(context.Context, map[string]any) error { delivered.Add(1); return nil }},
		Sweep: &Sweep{Every: time.Hour, Run: func(context.Context) error {
			seenAtSweep.Store(delivered.Load())
			return nil
		}},
	}, timers.clock())
	defer stopWorker(t, stop)
	timers.passDone(t)
	if seenAtSweep.Load() != 2 {
		t.Fatalf("sweep saw %d deliveries, want 2", seenAtSweep.Load())
	}
}

// A handler that panics because the worker is being stopped is still a defect: its frames are
// logged, while the event is given back without an attempt, as for any stop-time failure.
func TestPanicWhileStoppingIsLogged(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	appendEvents(t, pool, ev("t", 1))
	var buf bytes.Buffer
	timers := newFakeTimers()
	started := make(chan struct{})
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("info", &buf),
		Handlers: map[string]Handler{"t": func(hctx context.Context, _ map[string]any) error {
			close(started)
			<-hctx.Done()
			panicHere()
			return nil
		}},
	}, timers.clock())
	recv(t, started, "handler start")
	stopWorker(t, stop)
	out := buf.String()
	if !strings.Contains(out, `"msg":"outbox: handler panicked"`) || !strings.Contains(out, "outbox.panicHere") {
		t.Fatalf("the panic during stop was not logged: %s", out)
	}
	if strings.Contains(out, "leaky-panic-value") {
		t.Fatalf("log leaks: %s", out)
	}
	var attempts int
	var processed bool
	if err := pool.QueryRow(ctx, `select attempts, processed_at is not null from platform.outbox`).Scan(&attempts, &processed); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || processed {
		t.Fatalf("attempts=%d processed=%v: a stop-time panic must not count as an attempt", attempts, processed)
	}
}

// A sweep that panics or fails on every pass never stops deliveries: events appended later are
// still delivered, in id order, and the sweep runs on the pass after its deliveries.
func TestAFailingOrPanickingSweepDoesNotStopDeliveries(t *testing.T) {
	for name, run := range map[string]func(context.Context) error{
		"error": func(context.Context) error { return errors.New("sweep secret") },
		"panic": func(context.Context) error { panic("sweep panic secret") },
	} {
		t.Run(name, func(t *testing.T) {
			pool := fresh(t)
			var buf bytes.Buffer
			timers := newFakeTimers()
			var mu sync.Mutex
			var got []float64
			stop := startWorker(WorkerOptions{
				Pool: pool, Interval: time.Hour, Logger: logging.New("info", &buf),
				Handlers: map[string]Handler{"t": func(_ context.Context, p map[string]any) error {
					mu.Lock()
					defer mu.Unlock()
					got = append(got, p["n"].(float64))
					return nil
				}},
				Sweep: &Sweep{Every: time.Nanosecond, Run: run},
			}, timers.clock())
			defer stopWorker(t, stop)

			next := timers.passDone(t) // the first pass: nothing to deliver, the sweep ran and failed
			appendEvents(t, pool, ev("t", 1), ev("t", 2))
			timers.advance(time.Second)
			next <- time.Time{}
			next = timers.passDone(t)
			appendEvents(t, pool, ev("t", 3))
			next <- time.Time{}
			timers.passDone(t)

			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(got, []float64{1, 2, 3}) {
				t.Fatalf("delivered %v", got)
			}
			out := buf.String()
			if !strings.Contains(out, `"msg":"outbox: sweep failed"`) || strings.Contains(out, "secret") {
				t.Fatalf("log: %s", out)
			}
		})
	}
}

// Rows written while no worker ran (attempts 0) are delivered once each, in id order, by the
// first worker that has handlers, whatever mix of types they are, and are marked processed.
func TestBacklogWrittenWithoutAWorkerIsDeliveredInIDOrderOnce(t *testing.T) {
	pool := fresh(t)
	ctx := context.Background()
	appendEvents(t, pool, ev("x.b", 1))
	appendEvents(t, pool, ev("x.c", 2), ev("x.a", 3))
	appendEvents(t, pool, ev("x.a", 4))
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from platform.outbox where attempts = 0 and processed_at is null`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("backlog rows: %d %v", n, err)
	}

	timers := newFakeTimers()
	var mu sync.Mutex
	var got []string
	record := func(_ context.Context, p map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, strconv.Itoa(int(p["n"].(float64))))
		return nil
	}
	stop := startWorker(WorkerOptions{
		Pool: pool, Interval: time.Hour, Logger: logging.New("silent", io.Discard),
		Handlers: map[string]Handler{"x.a": record, "x.b": record, "x.c": record},
	}, timers.clock())
	next := timers.passDone(t)
	next <- time.Time{}
	timers.passDone(t) // a second pass delivers nothing again
	stopWorker(t, stop)

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, []string{"1", "2", "3", "4"}) {
		t.Fatalf("delivered %v, want each once in id order", got)
	}
	if err := pool.QueryRow(ctx, `select count(*) from platform.outbox where attempts = 1 and processed_at is not null and last_error is null`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("processed rows with one attempt: %d %v", n, err)
	}
}
