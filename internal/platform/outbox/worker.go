package outbox

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	batchSize       = 20
	defaultInterval = time.Second
	// defaultOutcomeTimeout bounds each write that records a delivery's outcome; it starts after
	// the handler returned and survives the worker's own cancellation, otherwise a delivered
	// event would be delivered again.
	defaultOutcomeTimeout = 10 * time.Second
)

// Handler handles one event. It must be idempotent: delivery is at least once.
type Handler func(ctx context.Context, payload map[string]any) error

// Sweep is a periodic job run from the worker loop at most once per Every (first on the first
// pass), after the pass's deliveries. Used to repair what exhausted events left behind.
type Sweep struct {
	Every time.Duration
	Run   func(ctx context.Context) error
}

// WorkerOptions configures StartWorker. Interval <= 0 means one second.
type WorkerOptions struct {
	Pool *pgxpool.Pool
	// Handlers by event type. An event of a type that is not in the table is logged and marked
	// processed, exactly as the TypeScript worker does (there every handler is always registered, so
	// it only happens for a stale type). A caller that has no handlers yet must not start a worker
	// against a database that holds pending events: they would be consumed (see app.startOutbox).
	Handlers map[string]Handler
	Interval time.Duration
	Logger   *slog.Logger
	Sweep    *Sweep
}

// clock holds the time seams of the worker; tests replace them. A zero outcomeTimeout means
// defaultOutcomeTimeout.
type clock struct {
	now            func() time.Time
	newTimer       func(d time.Duration) (<-chan time.Time, func())
	outcomeTimeout time.Duration
}

func realClock() clock {
	return clock{
		now: time.Now,
		newTimer: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		},
	}
}

// handlerPanic is the error a panicking handler turns into. The panic value is dropped (it can
// quote values); frames name the call stack as "function file:line".
type handlerPanic struct{ frames []string }

func (handlerPanic) Error() string { return "handler panicked" }

// StartWorker polls the outbox every Interval (first pass at once) and delivers events to
// handlers one at a time, in id order. A failing handler leaves the row for retry with backoff
// (the last attempt logs an error); an event without a handler is marked processed. stop cancels
// the context handed to handlers, waits for the delivery in flight and returns; nothing is
// delivered after it returns nil. If ctx ends first stop returns ctx.Err(). Safe to call twice.
func StartWorker(o WorkerOptions) (stop func(ctx context.Context) error) {
	return startWorker(o, realClock())
}

func startWorker(o WorkerOptions, clk clock) func(ctx context.Context) error {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Interval <= 0 {
		o.Interval = defaultInterval
	}
	if clk.outcomeTimeout <= 0 {
		clk.outcomeTimeout = defaultOutcomeTimeout
	}
	w := &worker{o: o, clk: clk}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.loop(ctx)
	}()
	var once sync.Once
	return func(stopCtx context.Context) error {
		once.Do(cancel)
		select {
		case <-done:
			return nil
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}
}

type worker struct {
	o         WorkerOptions
	clk       clock
	lastSweep time.Time
	swept     bool
}

func (w *worker) loop(ctx context.Context) {
	for {
		w.tick(ctx)
		w.maybeSweep(ctx)
		ch, stopTimer := w.clk.newTimer(w.o.Interval)
		select {
		case <-ch:
		case <-ctx.Done():
			stopTimer()
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (w *worker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	rows, err := claim(ctx, w.o.Pool, batchSize)
	if err != nil {
		if ctx.Err() == nil {
			w.o.Logger.Error("outbox: tick failed", logging.ErrAttr(err))
		}
		return
	}
	// Sequential: events of one aggregate are applied in order, and each handler is short.
	for i, r := range rows {
		if ctx.Err() != nil {
			w.release(ctx, rows[i:]...)
			return
		}
		w.deliver(ctx, r)
	}
}

// release gives back rows this worker claimed but did not deliver; see release in outbox.go.
func (w *worker) release(ctx context.Context, rows ...row) {
	octx, cancel := w.outcomeCtx(ctx)
	defer cancel()
	if err := release(octx, w.o.Pool, rows); err != nil {
		w.o.Logger.Error("outbox: tick failed", logging.ErrAttr(err))
	}
}

// outcomeCtx is the context of one outcome write. Call it after the handler returned, so the
// deadline does not run while the handler works.
func (w *worker) outcomeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), w.clk.outcomeTimeout)
}

func (w *worker) markProcessed(ctx context.Context, id int64) {
	octx, cancel := w.outcomeCtx(ctx)
	defer cancel()
	w.bookkeep(markProcessed(octx, w.o.Pool, id))
}

func (w *worker) markFailed(ctx context.Context, id int64, err error) {
	octx, cancel := w.outcomeCtx(ctx)
	defer cancel()
	w.bookkeep(markFailed(octx, w.o.Pool, id, err))
}

func (w *worker) deliver(ctx context.Context, r row) {
	id := strconv.FormatInt(r.id, 10)
	h, ok := w.o.Handlers[r.typ]
	if !ok {
		w.o.Logger.Warn("outbox: no handler, skipping", "outboxId", id, "type", r.typ)
		w.markProcessed(ctx, r.id)
		return
	}
	err := w.call(ctx, h, r)
	if err == nil {
		w.markProcessed(ctx, r.id)
		return
	}
	// A panic is logged whether or not the worker is being stopped: it is a defect either way.
	// Neither the log nor the row's last_error gets the message: it can quote values.
	var hp handlerPanic
	if errors.As(err, &hp) {
		w.o.Logger.Error("outbox: handler panicked", "outboxId", id, "type", r.typ, "stack", hp.frames)
	}
	if ctx.Err() != nil {
		// The worker was stopped while the handler ran: not a failed attempt.
		w.release(ctx, r)
		return
	}
	attrs := []any{"outboxId", id, "type", r.typ, "attempts", r.attempts, logging.ErrAttr(err)}
	if r.attempts >= MaxAttempts {
		w.o.Logger.Error("outbox: handler failed, giving up", attrs...)
	} else {
		w.o.Logger.Warn("outbox: handler failed, will retry", attrs...)
	}
	w.markFailed(ctx, r.id, err)
}

func (w *worker) bookkeep(err error) {
	if err != nil {
		w.o.Logger.Error("outbox: tick failed", logging.ErrAttr(err))
	}
}

func (w *worker) call(ctx context.Context, h Handler, r row) (err error) {
	defer func() {
		if recover() != nil {
			err = handlerPanic{frames: logging.PanicFrames()}
		}
	}()
	return h(ctx, r.payload)
}

func (w *worker) maybeSweep(ctx context.Context) {
	s := w.o.Sweep
	if s == nil || s.Run == nil || ctx.Err() != nil {
		return
	}
	now := w.clk.now()
	if w.swept && now.Sub(w.lastSweep) < s.Every {
		return
	}
	w.swept, w.lastSweep = true, now
	if err := w.runSweep(ctx, s); err != nil && !errors.Is(err, context.Canceled) {
		var hp handlerPanic
		if errors.As(err, &hp) {
			w.o.Logger.Error("outbox: sweep panicked", "stack", hp.frames)
		}
		w.o.Logger.Warn("outbox: sweep failed", logging.ErrAttr(err))
	}
}

func (w *worker) runSweep(ctx context.Context, s *Sweep) (err error) {
	defer func() {
		if recover() != nil {
			err = handlerPanic{frames: logging.PanicFrames()}
		}
	}()
	return s.Run(ctx)
}

// ComposeSweeps merges the sweeps of several modules into the one the worker takes. Each member
// keeps its own Every (the first run is on the first pass); the composite wakes at the shortest
// period and runs the members that are due, one after another, so two sweeps never overlap each
// other or a delivery. A member that returns an error or panics does not stop the others; their
// errors are joined (a panic stays recognisable for the worker's log). Members without Run are
// ignored; no members give nil.
func ComposeSweeps(sweeps ...Sweep) *Sweep { return composeSweeps(time.Now, sweeps) }

func composeSweeps(now func() time.Time, sweeps []Sweep) *Sweep {
	members := make([]Sweep, 0, len(sweeps))
	for _, s := range sweeps {
		if s.Run != nil {
			members = append(members, s)
		}
	}
	if len(members) == 0 {
		return nil
	}
	every := members[0].Every
	for _, s := range members[1:] {
		every = min(every, s.Every)
	}
	last := make([]time.Time, len(members))
	ran := make([]bool, len(members))
	return &Sweep{Every: every, Run: func(ctx context.Context) error {
		var errs []error
		t := now() // once: a slow member does not count against the periods of the ones after it
		for i, s := range members {
			if ran[i] && t.Sub(last[i]) < s.Every {
				continue
			}
			ran[i], last[i] = true, t
			if err := runMember(ctx, s); err != nil {
				errs = append(errs, err)
			}
		}
		switch len(errs) {
		case 0:
			return nil
		case 1:
			return errs[0] // its own label, not a wrapper's
		}
		return newSweepFailures(errs)
	}}
}

// sweepFailures is the error of a composed run in which several members failed. Its message and
// its code list the members' labels (type names, never messages), since the label is what the
// log and the operator see. The members stay reachable through errors.As.
type sweepFailures struct {
	errs   []error
	labels []string
}

func newSweepFailures(errs []error) sweepFailures {
	f := sweepFailures{errs: errs}
	for _, e := range errs {
		f.labels = append(f.labels, kernel.ErrorLabel(e))
	}
	return f
}

func (f sweepFailures) Error() string { return "sweeps failed: " + strings.Join(f.labels, "; ") }

func (f sweepFailures) Unwrap() []error { return f.errs }

// Code is the labels joined and cut to the 32 characters a label code may have (kernel.ErrorLabel
// appends it to the type name).
func (f sweepFailures) Code() string {
	short := make([]string, len(f.labels))
	for i, l := range f.labels {
		typ, _, _ := strings.Cut(l, ": ")
		short[i] = typ[strings.LastIndex(typ, ".")+1:] // without the package, to fit the limit
	}
	code := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, strings.Join(short, "_"))
	return code[:min(len(code), 32)]
}

func runMember(ctx context.Context, s Sweep) (err error) {
	defer func() {
		if recover() != nil {
			err = handlerPanic{frames: logging.PanicFrames()}
		}
	}()
	return s.Run(ctx)
}
