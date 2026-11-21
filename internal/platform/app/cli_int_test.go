//go:build integration

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/ai-pipes/mcp-registry/internal/platform/scheduler"
)

// cliRun is one RunCLI over a stub module.
type cliRun struct {
	code           int
	stdout, stderr string
}

type stubCalls struct {
	args                      [][]string
	order                     []string
	firstStart, jobs, handled int
	rt                        Runtime
	runtimeErr                error
}

// useRuntime exercises what the module got while the pool is still open: the box, the audit log.
func (c *stubCalls) useRuntime() error {
	sealed, err := c.rt.Box.Seal("v")
	if err != nil {
		return err
	}
	if plain, err := c.rt.Box.Open(sealed); err != nil || plain != "v" {
		return errors.New("the box does not open what it sealed")
	}
	return c.rt.Audit.Record(context.Background(), kernel.AuditEntry{Action: "cli.stub"})
}

// stubModule has the commands the tests call, and a FirstStart, a job and a handler that the CLI
// must never run.
func (c *stubCalls) module(rt Runtime) []Module {
	c.rt = rt
	run := func(name string, code int, out string) CLICommand {
		return CLICommand{Name: name, Run: func(_ context.Context, args []string, stdout, stderr io.Writer) int {
			c.args = append(c.args, args)
			c.order = append(c.order, name)
			c.runtimeErr = c.useRuntime()
			_, _ = io.WriteString(stdout, out)
			if code == 2 {
				_, _ = io.WriteString(stderr, "error: bad arguments\n")
			}
			return code
		}}
	}
	return []Module{{
		Commands: []CLICommand{
			run("stub", 0, "stub output\n"),
			run("fails", 1, ""),
			run("bad-args", 2, ""),
			run("rotate-secrets-key", 0, "rotated\n"),
			run("reset-key-check", 0, "reset\n"),
		},
		FirstStart: func(context.Context) error { c.firstStart++; return nil },
		Jobs:       []scheduler.Job{{Name: "j", Run: func(context.Context) error { c.jobs++; return nil }}},
		Handlers:   map[string]outbox.Handler{"stub.x": func(context.Context, map[string]any) error { c.handled++; return nil }},
	}}
}

func runCLI(t *testing.T, url string, key []byte, calls *stubCalls, args ...string) cliRun {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunCLI(context.Background(), CLIOptions{
		Config: baseConfig(url, key), Version: "9.9.9-test", Args: args, Stdout: &out, Stderr: &errOut,
		modules: calls.module,
	})
	return cliRun{code, out.String(), errOut.String()}
}

func TestCLIDispatchesToAModuleCommandWithTheArgumentsAfterItsName(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	var calls stubCalls
	r := runCLI(t, url, key, &calls, "stub", "alpha", "--flag", "x")
	if r.code != 0 || r.stdout != "stub output\n" {
		t.Fatalf("%+v", r)
	}
	if len(calls.args) != 1 || strings.Join(calls.args[0], "|") != "alpha|--flag|x" {
		t.Fatalf("the command got %v", calls.args)
	}
	// The exit code of the command is the CLI's.
	if r := runCLI(t, url, key, &calls, "fails"); r.code != 1 {
		t.Errorf("fails: %d", r.code)
	}
	if r := runCLI(t, url, key, &calls, "bad-args"); r.code != 2 || !strings.Contains(r.stderr, "bad arguments") {
		t.Errorf("bad-args: %+v", r)
	}
	if calls.runtimeErr != nil {
		t.Errorf("runtime: %v", calls.runtimeErr)
	}
	// It never starts the server's parts, and closes the pool.
	if calls.firstStart != 0 || calls.jobs != 0 || calls.handled != 0 {
		t.Errorf("the CLI ran FirstStart=%d jobs=%d handlers=%d", calls.firstStart, calls.jobs, calls.handled)
	}
	eventuallyNoConnections(t, url)
}

func TestCLIUnknownAndMissingCommandAreExitTwoWithUsage(t *testing.T) {
	url := pgtest.StartURL(t)
	var calls stubCalls
	r := runCLI(t, url, newKey(t), &calls, `ver\sion`)
	if r.code != 2 || r.stdout != "" || !strings.HasPrefix(r.stderr, "error: unknown command \"ver\\sion\"\n") || !strings.HasSuffix(r.stderr, Usage) {
		t.Errorf("unknown: %+v", r)
	}
	if r := runCLI(t, url, newKey(t), &calls); r.code != 2 || !strings.HasPrefix(r.stderr, "error: no command given\n") {
		t.Errorf("none: %+v", r)
	}
	if len(calls.order) != 0 {
		t.Errorf("a command ran: %v", calls.order)
	}
	eventuallyNoConnections(t, url)
}

// The configuration came first; then migrations (a fresh database is migrated by a CLI run), then
// the key guard, then the command.
func TestCLIMigratesBeforeTheCommandAndGuardsTheKey(t *testing.T) {
	url := pgtest.StartURL(t)
	right, wrong := newKey(t), newKey(t)
	var calls stubCalls

	// A fresh database: migrated, the check is NOT created by the CLI (the server does that).
	if r := runCLI(t, url, right, &calls, "stub"); r.code != 0 || r.stderr != "" {
		t.Fatalf("fresh database: %+v", r)
	}
	// The server records the key once...
	startInstance(t, baseConfig(url, right), nil).stop("SIGTERM")

	// ...and another key is one warning line, the command still runs.
	r := runCLI(t, url, wrong, &calls, "stub", "a")
	const warning = `warning: SECRETS_KEY does not match the data in this database. See "Recovering from a key mismatch" in the README.` + "\n"
	if r.code != 0 || r.stderr != warning || r.stdout != "stub output\n" {
		t.Fatalf("mismatch: %+v", r)
	}
	// The warning comes before the command's own argument errors.
	if r := runCLI(t, url, wrong, &calls, "bad-args"); r.code != 2 || !strings.HasPrefix(r.stderr, warning+"error: bad arguments") {
		t.Fatalf("bad-args under a mismatch: %+v", r)
	}
	// The key-aware commands run without the guard (no warning).
	for _, name := range keyAwareCommands {
		if r := runCLI(t, url, wrong, &calls, name); r.code != 0 || r.stderr != "" {
			t.Errorf("%s under a mismatch: %+v", name, r)
		}
	}
	// The right key, and the old key as the previous one, are silent.
	if r := runCLI(t, url, right, &calls, "stub"); r.stderr != "" {
		t.Errorf("right key: %+v", r)
	}
	// An unknown command is refused before the guard: no warning.
	if r := runCLI(t, url, wrong, &calls, "nope"); r.code != 2 || strings.Contains(r.stderr, "warning:") {
		t.Errorf("unknown under a mismatch: %+v", r)
	}
	eventuallyNoConnections(t, url)
}

func TestCLIRefusesADatabaseFromANewerVersion(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	var calls stubCalls
	runCLI(t, url, key, &calls, "stub")
	const future = "9999_from_the_future.sql"
	exec(t, url, "insert into platform.schema_migrations (name) values ($1)", future)
	r := runCLI(t, url, key, &calls, "stub")
	if r.code != 1 || r.stdout != "" || !strings.HasPrefix(r.stderr, "error: ") ||
		!strings.Contains(r.stderr, "migrated by a newer version of mcp-registry (unknown migration: "+future+")") {
		t.Fatalf("%+v", r)
	}
	if len(calls.order) != 1 { // only the first run's command
		t.Errorf("the command ran against a newer database: %v", calls.order)
	}
	eventuallyNoConnections(t, url)
}

func TestCLIOnAnUnreachableDatabaseIsExitOneWithoutUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	cfg := baseConfig("postgres://u@127.0.0.1:1/db", newKey(t))
	code := RunCLI(context.Background(), CLIOptions{Config: cfg, Args: []string{"stub"}, Stdout: &out, Stderr: &errOut})
	if code != 1 || !strings.HasPrefix(errOut.String(), "error: cannot connect to the database") || strings.Contains(errOut.String(), "Usage:") {
		t.Fatalf("%d %q", code, errOut.String())
	}
	if strings.Contains(errOut.String(), "127.0.0.1") {
		t.Errorf("the address is quoted: %q", errOut.String())
	}
}
