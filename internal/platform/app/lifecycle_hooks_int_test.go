//go:build integration

package app

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/ai-pipes/mcp-registry/internal/platform/scheduler"
)

// BeforeServing finishes before the outbox worker's first pass and before the scheduler's jobs
// start (the model must be loaded before anything can ask the embedder).
func TestBeforeServingRunsBeforeTheWorkerAndTheScheduler(t *testing.T) {
	url := pgtest.StartURL(t)
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	early := make(chan string, 4) // what ran while the hook was still going
	hookDone := make(chan struct{})
	jobRan, sweepRan := make(chan struct{}, 1), make(chan struct{}, 1)
	in := startInstance(t, baseConfig(url, newKey(t)), func(o *Options) {
		o.modules = func(Runtime) []Module {
			return []Module{{
				BeforeServing: func(context.Context) {
					note("before serving")
					// Give a worker or a job started too early the chance to show itself.
					select {
					case what := <-early:
						note("EARLY " + what)
					case <-time.After(300 * time.Millisecond):
					}
					note("before serving done")
					close(hookDone)
				},
				Handlers: map[string]outbox.Handler{"stub.x": func(context.Context, map[string]any) error { return nil }},
				Sweeps: []outbox.Sweep{{Every: time.Hour, Run: func(context.Context) error {
					select {
					case <-hookDone:
					default:
						early <- "sweep"
					}
					note("sweep")
					sweepRan <- struct{}{}
					return nil
				}}},
				Jobs: []scheduler.Job{{Name: "stub", Run: func(context.Context) error {
					select {
					case <-hookDone:
					default:
						early <- "job"
					}
					note("job")
					jobRan <- struct{}{}
					return nil
				}}},
			}}
		}
	})
	await(t, jobRan, "the job")
	await(t, sweepRan, "the sweep of the worker's first pass")
	in.stop("SIGTERM")
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 4 || order[0] != "before serving" || order[1] != "before serving done" || !slices.Contains(order, "job") || !slices.Contains(order, "sweep") {
		t.Fatalf("order %v: the hook must finish before the worker and the scheduler start", order)
	}
}

// A start that is stopped before serving (a signal while a module prepares) still releases what
// the modules hold.
func TestAStartStoppedBeforeServingRunsTheClosers(t *testing.T) {
	url := pgtest.StartURL(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	closed := 0
	logs := &logBuf{}
	code := Run(ctx, Options{
		Config: baseConfig(url, newKey(t)), Version: "9.9.9-test", WebRoot: t.TempDir(), Logger: logging.New("info", logs),
		modules: func(Runtime) []Module {
			return []Module{{
				BeforeServing: func(context.Context) { cancel(Signal("SIGTERM")) },
				Closers:       []func(context.Context) error{func(context.Context) error { closed++; return nil }},
			}}
		},
	})
	if code != 0 || closed != 1 {
		t.Fatalf("exit %d, closers run %d\n%s", code, closed, logs.String())
	}
	if len(logs.find(t, "listening")) != 0 || len(logs.find(t, msgNoOutboxWorker)) != 0 {
		t.Fatalf("the start went on: %v", logs.messages(t))
	}
}

// The CLI releases what the modules hold when the command is done, however it ended.
func TestTheCLIRunsTheClosersAfterTheCommand(t *testing.T) {
	e := newCLIEnv(t)
	for name, tc := range map[string]struct {
		args []string
		code int
	}{
		"success":         {[]string{"stub"}, 0},
		"failure":         {[]string{"fails"}, 1},
		"unknown command": {[]string{"nope"}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			var log []string
			cmd := func(n string, err error) CLICommand {
				return CLICommand{Name: n, Run: func(context.Context, CLIIO, []string) error { log = append(log, "command"); return err }}
			}
			var out, errOut bytes.Buffer
			code := RunCLI(context.Background(), CLIOptions{
				Config: e.cfg, Version: "v", Args: tc.args, Stdout: &out, Stderr: &errOut, Env: map[string]string{},
				modules: func(Runtime) []Module {
					return []Module{{
						Commands: []CLICommand{cmd("stub", nil), cmd("fails", CommandError("no"))},
						Closers:  []func(context.Context) error{func(context.Context) error { log = append(log, "closed"); return nil }},
					}}
				},
			})
			want := []string{"command", "closed"}
			if tc.code == 2 {
				want = []string{"closed"}
			}
			if code != tc.code || !slices.Equal(log, want) {
				t.Fatalf("exit %d, log %v, want %v", code, log, want)
			}
		})
	}
}
