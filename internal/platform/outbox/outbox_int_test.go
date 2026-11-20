//go:build integration

package outbox

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/apps/server/migrations"
	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/ai-pipes/mcp-registry/internal/platform/migrate"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

const wait = 30 * time.Second

func fresh(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.Start(t)
	if _, err := migrate.Run(context.Background(), pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return pool
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
	if attempts != 1 || processed || lastErr != "errors.errorString" {
		t.Fatalf("attempts=%d processed=%v last_error=%q", attempts, processed, lastErr)
	}
	if delay < 0.5 || delay > 2.5 { // 2^1 seconds
		t.Fatalf("next attempt in %vs", delay)
	}
	out := buf.String()
	if strings.Contains(out, "secret-value-123") || strings.Contains(lastErr, "secret") {
		t.Fatalf("message leaked: %s", out)
	}
	for _, want := range []string{`"msg":"outbox: handler failed, will retry"`, `"outboxId":"`, `"type":"t"`, `"attempts":1`} {
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
