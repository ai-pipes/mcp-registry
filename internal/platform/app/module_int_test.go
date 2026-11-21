//go:build integration

package app

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/ai-pipes/mcp-registry/internal/platform/scheduler"
	"github.com/jackc/pgx/v5"
)

// One stub module exercises the whole mechanism: routes, prefixes, a job, an outbox handler, the
// first-start hook and a command.
func TestAStubModulePlugsIntoTheServer(t *testing.T) {
	url := pgtest.StartURL(t)
	var rt Runtime
	jobRan, delivered := make(chan struct{}, 1), make(chan map[string]any, 1)
	in := startInstance(t, baseConfig(url, newKey(t)), func(o *Options) {
		o.modules = func(r Runtime) []Module {
			rt = r
			return []Module{{
				Routes: []httpx.Route{
					{Method: "GET", Path: "/api/stub", Handler: say("stub")},
					{Method: "DELETE", Path: "/api/stub/{id}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("X-Param", httpx.PathParam(r, "id"))
					})},
				},
				Prefixes: []httpx.Prefix{{Path: "/api/guarded", Middleware: []httpx.Middleware{block(401)}}},
				Jobs: []scheduler.Job{{Name: "stub", Run: func(context.Context) error {
					select {
					case jobRan <- struct{}{}:
					default:
					}
					return nil
				}}},
				Handlers: map[string]outbox.Handler{"stub.happened": func(_ context.Context, p map[string]any) error {
					delivered <- p
					return nil
				}},
				FirstStart: func(context.Context) { rt.Logger.Info("stub first start") },
			}}
		}
	})

	if code, body, _ := get(t, in.base+"/api/stub"); code != 200 || body != "stub" {
		t.Errorf("route: %d %q", code, body)
	}
	if code, body, _ := get(t, in.base+"/api/guarded/x"); code != 401 || body != "" {
		t.Errorf("prefix: %d %q", code, body)
	}
	await(t, jobRan, "the job")

	// The worker is running: an event of the stub's type is delivered to its handler.
	err := db.InTx(context.Background(), rt.Pool, func(tx pgx.Tx) error {
		return outbox.Append(context.Background(), tx, []kernel.Event{{Type: "stub.happened", Payload: map[string]any{"n": float64(1)}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if p := await(t, delivered, "the outbox handler"); p["n"] != float64(1) {
		t.Errorf("payload %v", p)
	}
	if len(in.logs.find(t, msgNoOutboxWorker)) != 0 {
		t.Error("the worker was reported as not started although a module has a handler")
	}

	// The runtime the module got: a box that opens what it seals, the system clock, the audit log.
	sealed, err := rt.Box.Seal("x")
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := rt.Box.Open(sealed); err != nil || plain != "x" {
		t.Errorf("box: %q %v", plain, err)
	}
	if rt.Clock == nil || rt.Clock.Now().IsZero() || rt.Version != "9.9.9-test" || rt.Logger == nil {
		t.Errorf("runtime %+v", rt)
	}
	if err := rt.Audit.Record(context.Background(), kernel.AuditEntry{Action: "stub.audit"}); err != nil {
		t.Errorf("audit: %v", err)
	}
	in.stop("SIGTERM")
}

// FirstStart runs after the key check and before the listener accepts connections.
func TestFirstStartRunsAfterTheKeyCheckAndBeforeListening(t *testing.T) {
	url := pgtest.StartURL(t)
	in := startInstance(t, baseConfig(url, newKey(t)), func(o *Options) {
		o.modules = func(rt Runtime) []Module {
			return []Module{{FirstStart: func(context.Context) { rt.Logger.Info("stub first start") }}}
		}
	})
	got := in.logs.messages(t)
	want := []string{"starting", "migrations applied", msgKeyCreated, "stub first start", msgNoOutboxWorker, "listening"}
	if !slices.Equal(got, want) {
		t.Fatalf("records %q, want %q", got, want)
	}
	in.stop("SIGTERM")
}

// FirstStart has no error: the module's wiring logs its own records. The composition only
// recovers a panic (error level, frames, never the value) and the start goes on.
func TestAPanickingFirstStartIsLoggedWithoutItsValueAndDoesNotStopTheStart(t *testing.T) {
	url := pgtest.StartURL(t)
	const secret = "tok-SECRET-value"
	var second bool
	in := startInstance(t, baseConfig(url, newKey(t)), func(o *Options) {
		o.modules = func(Runtime) []Module {
			return []Module{
				{FirstStart: func(context.Context) { panic("boom " + secret) }},
				{FirstStart: func(context.Context) { second = true }},
			}
		}
	})
	failed := in.logs.find(t, msgFirstStartPanicked)
	if len(failed) != 1 || levelOf(failed[0]) != levelError || failed[0]["stack"] == nil {
		t.Fatalf("panic records: %v", failed)
	}
	if strings.Contains(in.logs.String(), secret) {
		t.Fatal("the panic value reached the log")
	}
	if !second {
		t.Error("the next module's FirstStart did not run")
	}
	if code, body, _ := get(t, in.base+"/healthz"); code != 200 || body != "ok" {
		t.Errorf("the server is not serving: %d %q", code, body)
	}
	in.stop("SIGTERM")
}

// Two modules with a handler for one event type: the start is refused before anything runs.
func TestTwoModulesHandlingOneEventTypeRefuseTheStart(t *testing.T) {
	url := pgtest.StartURL(t)
	h := func(context.Context, map[string]any) error { return nil }
	cfg := baseConfig(url, newKey(t))
	cfg.Port = freePort(t)
	firstStarted := false
	in := launch(t, cfg, func(o *Options) {
		o.modules = func(Runtime) []Module {
			return []Module{
				{Handlers: map[string]outbox.Handler{"catalog.tools_changed": h}, FirstStart: func(context.Context) { firstStarted = true }},
				{Handlers: map[string]outbox.Handler{"catalog.tools_changed": h}},
			}
		}
	})
	if code := in.exit(); code != 1 {
		t.Fatalf("exit %d; log:\n%s", code, in.logs.String())
	}
	var fatal []map[string]any
	for _, r := range in.logs.records(t) {
		if levelOf(r) == levelFatal {
			fatal = append(fatal, r)
		}
	}
	if len(fatal) != 1 || !strings.Contains(fatal[0]["msg"].(string), "catalog.tools_changed") {
		t.Fatalf("fatal records: %v", fatal)
	}
	if firstStarted || len(in.logs.find(t, "listening")) != 0 {
		t.Fatal("the start went on")
	}
	eventuallyNoConnections(t, url)
}
