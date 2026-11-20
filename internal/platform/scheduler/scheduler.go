// Package scheduler runs periodic jobs in the background.
package scheduler

import (
	"context"
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
		if err := run(ctx, j); err != nil && ctx.Err() == nil {
			logger.Error("scheduler: job failed", "job", j.Name, logging.ErrAttr(err))
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

type jobPanic struct{}

func (jobPanic) Error() string { return "job panicked" }

func run(ctx context.Context, j Job) (err error) {
	defer func() {
		if recover() != nil {
			err = jobPanic{}
		}
	}()
	return j.Run(ctx)
}
