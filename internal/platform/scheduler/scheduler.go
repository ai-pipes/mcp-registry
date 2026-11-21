// Package scheduler runs periodic jobs in the background.
//
// A job either logs its own failure and returns nil, or returns the error and relies on the
// "scheduler: job failed" record, never both. The composition root closes the pool only after
// the stop function has returned.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

// Job runs at start and then Every after each run ends. A failing run is logged and the
// schedule goes on. Every <= 0 means the job runs once.
type Job struct {
	Name  string
	Every time.Duration
	Run   func(ctx context.Context) error
}

type newTimerFunc func(d time.Duration) (<-chan time.Time, func())

func realTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// Start runs every job in its own goroutine. stop cancels the context handed to jobs and the
// waits between runs, waits for the runs in flight and returns; nothing runs after it returns
// nil. If ctx ends first stop returns ctx.Err(). Safe to call twice.
func Start(logger *slog.Logger, jobs []Job) (stop func(ctx context.Context) error) {
	return start(logger, jobs, realTimer)
}

func start(logger *slog.Logger, jobs []Job, newTimer newTimerFunc) func(ctx context.Context) error {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runJob(ctx, logger, j, newTimer)
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
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

func runJob(ctx context.Context, logger *slog.Logger, j Job, newTimer newTimerFunc) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := run(ctx, j); err != nil {
			// A panic is logged also while stopping: it is a defect either way. An error is not,
			// a job interrupted by the stop fails for that reason.
			var jp jobPanic
			isPanic := errors.As(err, &jp)
			if isPanic {
				logger.Error("scheduler: job panicked", "job", j.Name, "stack", jp.frames)
			}
			if isPanic || ctx.Err() == nil {
				logger.Error("scheduler: job failed", "job", j.Name, logging.ErrAttr(err))
			}
		}
		if j.Every <= 0 || ctx.Err() != nil {
			return
		}
		ch, stopTimer := newTimer(j.Every)
		select {
		case <-ch:
		case <-ctx.Done():
			stopTimer()
			return
		}
	}
}

// jobPanic is the error a panicking job turns into. The panic value is dropped (it can quote
// values); frames name the call stack as "function file:line".
type jobPanic struct{ frames []string }

func (jobPanic) Error() string { return "job panicked" }

func run(ctx context.Context, j Job) (err error) {
	defer func() {
		if recover() != nil {
			err = jobPanic{frames: logging.PanicFrames()}
		}
	}()
	return j.Run(ctx)
}
