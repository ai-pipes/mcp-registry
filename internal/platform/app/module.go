package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/audit"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
	"github.com/ai-pipes/mcp-registry/internal/platform/scheduler"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CLICommand is one operator command of `mcp-registry <name> ...`. Run gets the arguments after
// the command name and returns the exit code: 0 done, 1 the operation failed, 2 bad invocation
// (print the reason and usage to stderr).
type CLICommand struct {
	Name string
	Run  func(ctx context.Context, args []string, stdout, stderr io.Writer) int
}

// Module is what a module hands the composition: everything the server and the CLI need from it.
// The composition merges the modules of modules() in order. The CLI uses Commands only; it never
// starts the HTTP server, the outbox worker, the scheduler or FirstStart.
type Module struct {
	Routes   []httpx.Route
	Prefixes []httpx.Prefix
	Jobs     []scheduler.Job
	// Handlers maps an outbox event type to its subscriber. Two modules registering one type is a
	// start-up error; the worker starts only when the merged table is not empty.
	Handlers map[string]outbox.Handler
	// FirstStart runs after the secrets key check and before the listener accepts connections. Its
	// error is logged at error level (without the error's text) and does not stop the start.
	FirstStart func(ctx context.Context) error
	Commands   []CLICommand
}

// Runtime is what the composition gives the constructor of a module.
type Runtime struct {
	Config  config.Config
	Pool    *pgxpool.Pool
	Logger  *slog.Logger
	Clock   kernel.Clock
	Audit   kernel.AuditLog // the Postgres audit log (platform.audit_log)
	Box     kernel.SecretBox
	Version string
}

// newRuntime builds the Runtime for cfg over an open pool. The box seals with SECRETS_KEY and
// also opens with SECRETS_KEY_OLD when it is set.
func newRuntime(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool, version string) (Runtime, error) {
	var previous []byte
	if cfg.SecretsKeyOld.IsSet() {
		previous = cfg.SecretsKeyOld.Reveal()
	}
	box, err := crypto.NewAESGCMBox(cfg.SecretsKey.Reveal(), previous)
	if err != nil {
		return Runtime{}, fmt.Errorf("secret box: %w", err)
	}
	return Runtime{
		Config: cfg, Pool: pool, Logger: logger, Clock: kernel.SystemClock{},
		Audit: audit.NewPgAuditLog(pool), Box: box, Version: version,
	}, nil
}

// modules is the list of modules, in the order their routes, prefixes and jobs are merged. The
// server and the CLI build it from the same Runtime. Identity joins it in milestone 2, the others
// in later ones.
func modules(Runtime) []Module { return nil }

// modulesOf is the module list for rt: the default, or the test seam.
func modulesOf(seam func(Runtime) []Module, rt Runtime) []Module {
	if seam != nil {
		return seam(rt)
	}
	return modules(rt)
}

// assembled is the merge of the modules.
type assembled struct {
	routes     []httpx.Route
	prefixes   []httpx.Prefix
	jobs       []scheduler.Job
	handlers   map[string]outbox.Handler
	firstStart []func(ctx context.Context) error
	commands   map[string]CLICommand
}

// assemble merges the modules in order. A second registration of an outbox event type or of a
// command name is an error naming it (they are names, never data).
func assemble(mods []Module) (assembled, error) {
	a := assembled{handlers: map[string]outbox.Handler{}, commands: map[string]CLICommand{}}
	for _, m := range mods {
		a.routes = append(a.routes, m.Routes...)
		a.prefixes = append(a.prefixes, m.Prefixes...)
		a.jobs = append(a.jobs, m.Jobs...)
		for _, typ := range sortedKeys(m.Handlers) {
			if _, dup := a.handlers[typ]; dup {
				return assembled{}, fmt.Errorf("outbox event type %q has handlers in two modules", typ)
			}
			a.handlers[typ] = m.Handlers[typ]
		}
		if m.FirstStart != nil {
			a.firstStart = append(a.firstStart, m.FirstStart)
		}
		for _, c := range m.Commands {
			if _, dup := a.commands[c.Name]; dup {
				return assembled{}, fmt.Errorf("command %q is registered by two modules", c.Name)
			}
			a.commands[c.Name] = c
		}
	}
	return a, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
