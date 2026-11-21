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
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
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
	stdin, env                string
	firstStart, jobs, handled int
	rt                        Runtime
	runtimeErr                error
	oldSealed                 []byte // opened through rt.Box by the stub command when set
	oldOpened                 string
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
	if c.oldSealed != nil {
		if c.oldOpened, err = c.rt.Box.Open(c.oldSealed); err != nil {
			return err
		}
	}
	return c.rt.Audit.Record(context.Background(), kernel.AuditEntry{Action: "cli.stub"})
}

// stubModule has the commands the tests call, and a FirstStart, a job and a handler that the CLI
// must never run.
func (c *stubCalls) module(rt Runtime) []Module {
	c.rt = rt
	cmd := func(name string, guard KeyGuard, run func(cio CLIIO) error) CLICommand {
		return CLICommand{Name: name, Guard: guard, Run: func(_ context.Context, cio CLIIO, args []string) error {
			c.args = append(c.args, args)
			c.order = append(c.order, name)
			return run(cio)
		}}
	}
	return []Module{{
		Commands: []CLICommand{
			cmd("stub", GuardWarn, func(cio CLIIO) error {
				in, _ := io.ReadAll(cio.Stdin)
				c.stdin, c.env = string(in), cio.Env["STUB_VAR"]
				c.runtimeErr = c.useRuntime()
				c.rt.Logger.Info("stub info record")
				c.rt.Logger.Warn("stub warn record")
				_, _ = io.WriteString(cio.Stdout, "stub output\n")
				return nil
			}),
			cmd("fails", GuardWarn, func(CLIIO) error { return CommandError("it failed") }),
			cmd("bad-args", GuardWarn, func(CLIIO) error { return UsageError("bad arguments") }),
			cmd("domain", GuardWarn, func(CLIIO) error { return kernel.NewDomainError("not_found", "no such person") }),
			cmd("boom", GuardWarn, func(CLIIO) error { return errors.New("dial tcp secret-token-xyz") }),
			cmd("rotate-secrets-key", GuardNone, func(cio CLIIO) error { _, _ = io.WriteString(cio.Stdout, "rotated\n"); return nil }),
			cmd("seals", GuardRefuse, func(cio CLIIO) error { _, _ = io.WriteString(cio.Stdout, "sealed\n"); return nil }),
		},
		FirstStart: func(context.Context) { c.firstStart++ },
		Jobs:       []scheduler.Job{{Name: "j", Run: func(context.Context) error { c.jobs++; return nil }}},
		Handlers:   map[string]outbox.Handler{"stub.x": func(context.Context, map[string]any) error { c.handled++; return nil }},
	}}
}

func runCLIWith(t *testing.T, cfg config.Config, calls *stubCalls, stdin string, args ...string) cliRun {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunCLI(context.Background(), CLIOptions{
		Config: cfg, Version: "9.9.9-test", Args: args, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut,
		Env:     map[string]string{"STUB_VAR": "from-env"},
		modules: calls.module,
	})
	return cliRun{code, out.String(), errOut.String()}
}

func runCLI(t *testing.T, url string, key []byte, calls *stubCalls, args ...string) cliRun {
	t.Helper()
	return runCLIWith(t, baseConfig(url, key), calls, "", args...)
}

func TestCLIDispatchesToAModuleCommandWithItsIO(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	var calls stubCalls
	r := runCLIWith(t, baseConfig(url, key), &calls, "typed secret", "stub", "alpha", "--flag", "x")
	if r.code != 0 || r.stdout != "stub output\n" {
		t.Fatalf("%+v", r)
	}
	if len(calls.args) != 1 || strings.Join(calls.args[0], "|") != "alpha|--flag|x" {
		t.Fatalf("the command got %v", calls.args)
	}
	if calls.stdin != "typed secret" || calls.env != "from-env" {
		t.Errorf("stdin %q env %q", calls.stdin, calls.env)
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

// The one place that maps what a command returns.
func TestCLIMapsTheErrorsACommandReturns(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	var calls stubCalls
	for _, tc := range []struct {
		cmd    string
		code   int
		stderr string
	}{
		{"fails", 1, "error: it failed\n"},
		{"bad-args", 2, "error: bad arguments\nUsage: mcp-registry"},
		{"domain", 1, "error: not_found: no such person\n"},
		{"boom", 1, "error: unexpected failure\n"},
	} {
		r := runCLI(t, url, key, &calls, tc.cmd)
		if r.code != tc.code || r.stdout != "" || !strings.HasPrefix(r.stderr, tc.stderr) || (tc.code == 1 && r.stderr != tc.stderr) {
			t.Errorf("%s: %+v, want %d %q", tc.cmd, r, tc.code, tc.stderr)
		}
	}
}

// cli.ts writes its logger to stderr and never to stdout; info is cut to warn by default.
func TestCLILoggerWritesToStderrOnlyAndInfoBecomesWarn(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	var calls stubCalls

	r := runCLI(t, url, key, &calls, "stub")
	if r.stdout != "stub output\n" {
		t.Fatalf("stdout must be exactly the command's output, got %q", r.stdout)
	}
	if !strings.Contains(r.stderr, `"msg":"stub warn record"`) || strings.Contains(r.stderr, "stub info record") {
		t.Errorf("LOG_LEVEL=info: stderr %q (want the warn record, not the info one)", r.stderr)
	}

	cfg := baseConfig(url, key)
	for level, wantInfo := range map[string]bool{"debug": true, "warn": false} {
		cfg.LogLevel = level
		r := runCLIWith(t, cfg, &calls, "", "stub")
		if r.stdout != "stub output\n" {
			t.Errorf("LOG_LEVEL=%s: stdout %q", level, r.stdout)
		}
		if got := strings.Contains(r.stderr, "stub info record"); got != wantInfo {
			t.Errorf("LOG_LEVEL=%s: info record on stderr = %v, want %v", level, got, wantInfo)
		}
	}
	cfg.LogLevel = "error"
	if r := runCLIWith(t, cfg, &calls, "", "stub"); strings.Contains(r.stderr, "stub warn record") || r.stdout != "stub output\n" {
		t.Errorf("LOG_LEVEL=error: %+v", r)
	}
}

func TestCLIUnknownAndMissingCommandAreExitTwoWithUsage(t *testing.T) {
	url := pgtest.StartURL(t)
	var calls stubCalls
	r := runCLI(t, url, newKey(t), &calls, `ver\sion`)
	if r.code != 2 || r.stdout != "" || !strings.HasPrefix(r.stderr, "error: unknown command \"ver\\sion\"\n") {
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

const (
	warningLine = `warning: SECRETS_KEY does not match the data in this database. See "Recovering from a key mismatch" in the README.` + "\n"
	refusalLine = `error: SECRETS_KEY does not match the data in this database; refusing to seal a new secret with it. See "Recovering from a key mismatch" in the README.` + "\n"
)

// The configuration came first; then migrations (a fresh database is migrated by a CLI run), then
// the key guard, then the command.
func TestCLIMigratesBeforeTheCommandAndGuardsTheKey(t *testing.T) {
	url := pgtest.StartURL(t)
	right, wrong := newKey(t), newKey(t)
	var calls stubCalls

	// A fresh database: migrated, the check is NOT created by the CLI (the server does that).
	if r := runCLI(t, url, right, &calls, "seals"); r.code != 0 || r.stderr != "" {
		t.Fatalf("fresh database: %+v", r)
	}
	startInstance(t, baseConfig(url, right), nil).stop("SIGTERM")

	// GuardWarn: one warning line, the command still runs.
	r := runCLI(t, url, wrong, &calls, "stub", "a")
	if r.code != 0 || !strings.HasPrefix(r.stderr, warningLine) || r.stdout != "stub output\n" || strings.Count(r.stderr, "warning:") != 1 {
		t.Fatalf("mismatch: %+v", r)
	}
	// The warning comes before the command's own argument errors.
	if r := runCLI(t, url, wrong, &calls, "bad-args"); r.code != 2 || !strings.HasPrefix(r.stderr, warningLine+"error: bad arguments") {
		t.Fatalf("bad-args under a mismatch: %+v", r)
	}
	// GuardNone runs without a word.
	if r := runCLI(t, url, wrong, &calls, "rotate-secrets-key"); r.code != 0 || r.stderr != "" || r.stdout != "rotated\n" {
		t.Errorf("GuardNone under a mismatch: %+v", r)
	}
	// GuardRefuse: the command that seals a new secret does not run on a mismatch.
	before := len(calls.order)
	if r := runCLI(t, url, wrong, &calls, "seals"); r.code != 1 || r.stdout != "" || r.stderr != refusalLine || len(calls.order) != before {
		t.Errorf("GuardRefuse under a mismatch: %+v (ran %v)", r, calls.order[before:])
	}
	// With the right key every guard is silent and the command runs.
	for _, name := range []string{"stub", "seals", "rotate-secrets-key"} {
		if r := runCLI(t, url, right, &calls, name); r.code != 0 || strings.Contains(r.stderr, "warning:") || strings.Contains(r.stderr, "refusing") {
			t.Errorf("%s with the right key: %+v", name, r)
		}
	}
	// An unknown command is refused before the guard: no warning.
	if r := runCLI(t, url, wrong, &calls, "nope"); r.code != 2 || strings.Contains(r.stderr, "warning:") {
		t.Errorf("unknown under a mismatch: %+v", r)
	}
	eventuallyNoConnections(t, url)
}

// The guard passes SECRETS_KEY_OLD as the previous key: a check that opens only with it is not a
// mismatch (the server only warns about rotation).
func TestCLIKeyGuardAcceptsTheOldKey(t *testing.T) {
	url := pgtest.StartURL(t)
	oldKey, next := newKey(t), newKey(t)
	var calls stubCalls
	startInstance(t, baseConfig(url, oldKey), nil).stop("SIGTERM") // the check is sealed with oldKey

	cfg := baseConfig(url, next)
	cfg.SecretsKeyOld = config.NewSecretBytes(oldKey)
	for _, name := range []string{"stub", "seals"} {
		if r := runCLIWith(t, cfg, &calls, "", name); r.code != 0 || strings.Contains(r.stderr, "warning:") || strings.Contains(r.stderr, "refusing") {
			t.Errorf("%s with SECRETS_KEY_OLD: %+v", name, r)
		}
	}
	// An old key that does not open the check either is a mismatch.
	cfg.SecretsKeyOld = config.NewSecretBytes(newKey(t))
	if r := runCLIWith(t, cfg, &calls, "", "stub"); !strings.HasPrefix(r.stderr, warningLine) {
		t.Errorf("an unrelated old key: %+v", r)
	}
}

// Runtime.Box seals with SECRETS_KEY and still opens what the old key sealed (key rotation).
func TestRuntimeBoxOpensAValueSealedWithTheOldKey(t *testing.T) {
	url := pgtest.StartURL(t)
	oldKey, next := newKey(t), newKey(t)
	oldBox, err := crypto.NewAESGCMBox(oldKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := oldBox.Seal("old secret")
	if err != nil {
		t.Fatal(err)
	}
	calls := stubCalls{oldSealed: sealed}
	cfg := baseConfig(url, next)
	cfg.SecretsKeyOld = config.NewSecretBytes(oldKey)
	if r := runCLIWith(t, cfg, &calls, "", "rotate-secrets-key"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if r := runCLIWith(t, cfg, &calls, "", "stub"); r.code != 0 || calls.runtimeErr != nil || calls.oldOpened != "old secret" {
		t.Fatalf("%+v: runtime %v, opened %q", r, calls.runtimeErr, calls.oldOpened)
	}
	// Without the old key the same value is not readable.
	calls = stubCalls{oldSealed: sealed}
	if r := runCLI(t, url, next, &calls, "stub"); calls.runtimeErr == nil {
		t.Errorf("a new-key-only box opened an old-key value: %+v", r)
	}
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
