package outbox

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	batchSize       = 20
	defaultInterval = time.Second
	// bookkeepingTimeout bounds the writes that record a delivery's outcome; they must survive
	// the worker's own cancellation, otherwise a delivered event would be delivered again.
	bookkeepingTimeout = 10 * time.Second
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
	Pool     *pgxpool.Pool
	Handlers map[string]Handler
	Interval time.Duration
	Logger   *slog.Logger
	Sweep    *Sweep
}

// clock is the time seam of the worker; tests replace it.
type clock struct {
	now      func() time.Time
	newTimer func(d time.Duration) (<-chan time.Time, func())
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

// handlerPanic is the error a panicking handler turns into; the panic value is dropped (it can
// quote values).
type handlerPanic struct{}

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
			w.releaseRest(rows[i:])
			return
		}
		w.deliver(ctx, r)
	}
}

func (w *worker) releaseRest(rows []row) {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.id
	}
	bctx, cancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
	defer cancel()
	if err := release(bctx, w.o.Pool, ids); err != nil {
		w.o.Logger.Error("outbox: tick failed", logging.ErrAttr(err))
	}
}

func (w *worker) deliver(ctx context.Context, r row) {
	id := strconv.FormatInt(r.id, 10)
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()

	h, ok := w.o.Handlers[r.typ]
	if !ok {
		w.o.Logger.Warn("outbox: no handler, skipping", "outboxId", id, "type", r.typ)
		w.bookkeep(markProcessed(bctx, w.o.Pool, r.id))
		return
	}
	if err := w.call(ctx, h, r); err != nil {
		// Neither the log nor the row's last_error gets the message: it can quote values.
		attrs := []any{"outboxId", id, "type", r.typ, "attempts", r.attempts, logging.ErrAttr(err)}
		if r.attempts >= MaxAttempts {
			w.o.Logger.Error("outbox: handler failed, giving up", attrs...)
		} else {
			w.o.Logger.Warn("outbox: handler failed, will retry", attrs...)
		}
		w.bookkeep(markFailed(bctx, w.o.Pool, r.id, err))
		return
	}
	w.bookkeep(markProcessed(bctx, w.o.Pool, r.id))
}

func (w *worker) bookkeep(err error) {
	if err != nil {
		w.o.Logger.Error("outbox: tick failed", logging.ErrAttr(err))
	}
}

func (w *worker) call(ctx context.Context, h Handler, r row) (err error) {
	defer func() {
		if recover() != nil {
			err = handlerPanic{}
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
		w.o.Logger.Warn("outbox: sweep failed", logging.ErrAttr(err))
	}
}

func (w *worker) runSweep(ctx context.Context, s *Sweep) (err error) {
	defer func() {
		if recover() != nil {
			err = handlerPanic{}
		}
	}()
	return s.Run(ctx)
}
