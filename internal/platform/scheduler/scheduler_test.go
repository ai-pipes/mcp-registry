package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

const wait = 10 * time.Second

type timerReq struct {
	d    time.Duration
	fire chan time.Time
}

// fakeTimers hands every requested timer to the test, which fires it by hand.
type fakeTimers struct{ reqs chan timerReq }

func newFakeTimers() *fakeTimers { return &fakeTimers{reqs: make(chan timerReq, 16)} }

func (f *fakeTimers) newTimer(d time.Duration) (<-chan time.Time, func()) {
	r := timerReq{d: d, fire: make(chan time.Time, 1)}
	f.reqs <- r
	return r.fire, func() {}
}

func (f *fakeTimers) next(t *testing.T) timerReq {
	t.Helper()
	select {
	case r := <-f.reqs:
		return r
	case <-time.After(wait):
		t.Fatal("no timer requested")
		return timerReq{}
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

func TestFirstRunIsImmediateAndNextWaitsEveryAfterTheRunEnded(t *testing.T) {
	timers := newFakeTimers()
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	stop := start(nil, []Job{{Name: "j", Every: 7 * time.Minute, Run: func(context.Context) error {
		started <- struct{}{}
		<-release
		return nil
	}}}, timers.newTimer)

	recv(t, started, "first run")
	select {
	case <-timers.reqs:
		t.Fatal("timer requested while the run was still going")
	default:
	}
	close(release)
	r := timers.next(t)
	if r.d != 7*time.Minute {
		t.Fatalf("timer = %v", r.d)
	}
	r.fire <- time.Time{}
	recv(t, started, "second run")
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAnErrorDoesNotStopTheSchedule(t *testing.T) {
	timers := newFakeTimers()
	var buf bytes.Buffer
	logger := logging.New("info", &buf)
	runs := make(chan struct{}, 4)
	stop := start(logger, []Job{{Name: "flaky", Every: time.Hour, Run: func(context.Context) error {
		runs <- struct{}{}
		return errors.New("password=hunter2")
	}}}, timers.newTimer)

	recv(t, runs, "first run")
	timers.next(t).fire <- time.Time{}
	recv(t, runs, "run after the failure")
	timers.next(t)
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "scheduler: job failed") || !strings.Contains(out, `"job":"flaky"`) {
		t.Fatalf("log: %s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Fatalf("log has the error message: %s", out)
	}
}

func panicHere() { panic("leaky-panic-value") }

func TestAPanicIsAFailedRunAndTheRecordNamesTheFunctionNotTheValue(t *testing.T) {
	timers := newFakeTimers()
	var buf bytes.Buffer
	runs := make(chan struct{}, 4)
	stop := start(logging.New("info", &buf), []Job{{Name: "p", Every: time.Hour, Run: func(context.Context) error {
		runs <- struct{}{}
		panicHere()
		return nil
	}}}, timers.newTimer)
	recv(t, runs, "first run")
	timers.next(t).fire <- time.Time{}
	recv(t, runs, "run after the panic")
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "scheduler: job panicked") || !strings.Contains(out, "scheduler.panicHere") ||
		!strings.Contains(out, "scheduler_test.go:") {
		t.Fatalf("log: %s", out)
	}
	if strings.Contains(out, "leaky-panic-value") || strings.Contains(out, "/Users/") {
		t.Fatalf("log leaks: %s", out)
	}
}

func TestPanicWhileStoppingIsLogged(t *testing.T) {
	var buf bytes.Buffer
	started := make(chan struct{})
	stop := start(slog.New(slog.NewJSONHandler(&buf, nil)), []Job{{Name: "j", Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		panicHere()
		return nil
	}}}, newFakeTimers().newTimer)
	recv(t, started, "run start")
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "scheduler: job panicked") || !strings.Contains(out, "scheduler.panicHere") {
		t.Fatalf("the panic during stop was not logged: %s", out)
	}
}

func TestStopWaitsForTheRunningJobAndCancelsItsContext(t *testing.T) {
	timers := newFakeTimers()
	started := make(chan struct{})
	jobCtx := make(chan context.Context, 1)
	release := make(chan struct{})
	finished := make(chan struct{})
	stop := start(nil, []Job{{Name: "slow", Every: time.Hour, Run: func(ctx context.Context) error {
		jobCtx <- ctx
		close(started)
		<-release
		close(finished)
		return nil
	}}}, timers.newTimer)
	recv(t, started, "run start")

	// The caller gives up first: ctx.Err(), the job keeps running.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stop(gone); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop with a dead context = %v", err)
	}
	if recv(t, jobCtx, "job context").Err() == nil {
		t.Fatal("job context not cancelled by stop")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("stop returned while the job was running: %v", err)
	default:
	}
	close(release)
	if err := recv(t, stopped, "stop"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("stop returned before the job finished")
	}
	select {
	case <-timers.reqs:
		t.Fatal("a wait was scheduled after stop")
	default:
	}
	if err := stop(context.Background()); err != nil {
		t.Fatalf("second stop = %v", err)
	}
}

func TestStopCancelsTheWaitBetweenRuns(t *testing.T) {
	timers := newFakeTimers()
	runs := make(chan struct{}, 4)
	stop := start(nil, []Job{{Name: "j", Every: time.Hour, Run: func(context.Context) error {
		runs <- struct{}{}
		return nil
	}}}, timers.newTimer)
	recv(t, runs, "first run")
	timers.next(t)
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runs:
		t.Fatal("job ran after stop")
	default:
	}
}

func TestNoJobsStopsAtOnce(t *testing.T) {
	if err := Start(nil, nil)(context.Background()); err != nil {
		t.Fatal(err)
	}
}
