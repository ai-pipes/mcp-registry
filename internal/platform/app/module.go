package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/platform/audit"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/outbox"
	"github.com/ai-pipes/mcp-registry/internal/platform/scheduler"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CLIIO is what a command reads and writes: stdin (a pipe, or what the operator types), the
// two output streams, and the process environment (for `--secret-env`).
type CLIIO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Env            map[string]string
	// Terminal is set when Stdin is a terminal: a secret is then typed without echo. Nil for a
	// pipe, a file or a test.
	Terminal Terminal
}

// KeyGuard says what the CLI does before a command when the stored key check is not opened by the
// configured keys (a "mismatch").
type KeyGuard int

const (
	// GuardWarn (the default) prints one `warning:` line to stderr and runs the command.
	GuardWarn KeyGuard = iota
	// GuardRefuse refuses the command with exit 1: it seals a new secret with SECRETS_KEY
	// (create-server).
	GuardRefuse
	// GuardNone skips the check: the command handles the key itself (rotate-secrets-key,
	// reset-key-check).
	GuardNone
)

// CLICommand is one operator command of `mcp-registry <name> ...`. Run gets the arguments after
// the command name. What it returns is mapped in one place (see exitFor): nil is exit 0;
// UsageError is `error: <msg>` and the usage text, exit 2; CommandError is `error: <msg>`, exit 1;
// a kernel.DomainError is `error: <code>: <message>`, exit 1; any other error is
// `error: unexpected failure` (its text is never printed), exit 1. Usage is this command's entry
// in the usage text.
type CLICommand struct {
	Name  string
	Usage string
	Guard KeyGuard
	Run   func(ctx context.Context, io CLIIO, args []string) error
}

// Module is what a module's wiring hands the composition: everything the server and the CLI need
// from it. The composition merges the modules of modules() in order. The CLI uses Commands only;
// it never starts the HTTP server, the outbox worker, the scheduler or FirstStart.
type Module struct {
	Routes   []httpx.Route
	Prefixes []httpx.Prefix
	Jobs     []scheduler.Job
	// Handlers maps an outbox event type to its subscriber. Two modules registering one type is a
	// start-up error; the worker starts only when the merged table is not empty.
	Handlers map[string]outbox.Handler
	// Emits lists the outbox event types this module writes. Once any module registers a handler
	// (the table is not empty and the worker starts), every listed type must have one: the worker
	// marks an event of an unhandled type as processed, so a partial table loses events for good.
	// An incomplete table is a start-up error naming the missing types. With an empty table
	// nothing is checked (the worker is off and events wait). The module's wiring passes its
	// module's own list (the catalog: catalog.EventTypes()), so the outbox package knows no module.
	Emits []string
	// Sweeps are periodic repair jobs run by the outbox worker's loop (see outbox.Sweep): after a
	// pass's deliveries, so never concurrently with one, first on the first pass and then at most
	// once per Every; an error or panic is logged and the loop goes on. The sweeps of all modules
	// are composed (outbox.ComposeSweeps), each keeping its own period. They run only while the
	// worker runs, i.e. when some module has a handler. The search module's
	// indexMissingEmbeddings plugs in here.
	Sweeps []outbox.Sweep
	// FirstStart runs after the secrets key check and before the listener accepts connections. It
	// has no error: the module's wiring logs its own records (with the operator-facing texts). The
	// composition only recovers a panic (an error record with the call frames, never the value) and
	// the start goes on.
	FirstStart func(ctx context.Context)
	Commands   []CLICommand
}

// Runtime is what the per-module wiring inside internal/platform/app reads plain values from
// (one file per module, e.g. identity.go, which builds the module's infra, app and http pieces and
// returns its Module). The modules themselves never see it: their layers may not import the
// composition. Command bodies live in the same wiring files. The server and the CLI build the
// module list from the same Runtime, and the CLI builds every module for every command, so a
// constructor must stay cheap: no I/O, no goroutines.
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
// server and the CLI build it from the same Runtime. Identity is first; the platform's own routes
// (audit, version) follow, since they use identity's session guards; then the catalog. The other
// modules join in later milestones.
func modules(rt Runtime) []Module {
	id := newIdentityWiring(rt)
	return []Module{id.module(), platformModule(rt, id), newCatalogWiring(rt, id).module()}
}

// platformModule is the platform's own REST routes, guarded with identity's session checks:
// `GET /api/version` for any signed-in person and `GET /api/audit` for admins. Like identity's
// token and user routes, the guards are prefix middleware: every path and method under the prefix
// is answered by the guard first.
func platformModule(rt Runtime, id identityWiring) Module {
	session := id.service.RequireSession()
	return Module{
		Routes: []httpx.Route{
			{Method: http.MethodGet, Path: "/api/version", Handler: versionHandler(rt.Version)},
			{Method: http.MethodGet, Path: "/api/audit", Handler: auditHandler(rt.Pool)},
		},
		Prefixes: []httpx.Prefix{
			{Path: "/api/version", Middleware: []httpx.Middleware{session}},
			{Path: "/api/audit", Middleware: []httpx.Middleware{session, identity.RequireAdmin()}},
		},
	}
}

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
	sweep      *outbox.Sweep
	firstStart []func(ctx context.Context)
	commands   map[string]CLICommand
}

// assemble merges the modules in order. A second registration of an outbox event type or of a
// command name is an error naming it (they are names, never data).
func assemble(mods []Module) (assembled, error) {
	a := assembled{handlers: map[string]outbox.Handler{}, commands: map[string]CLICommand{}}
	var emitted []string
	var sweeps []outbox.Sweep
	for _, m := range mods {
		emitted = append(emitted, m.Emits...)
		sweeps = append(sweeps, m.Sweeps...)
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
	if err := checkHandlerTable(a.handlers, emitted); err != nil {
		return assembled{}, err
	}
	a.sweep = outbox.ComposeSweeps(sweeps...)
	return a, nil
}

// checkHandlerTable refuses a non-empty table that lacks a handler for an emitted event type. The
// text names types (names, never data).
func checkHandlerTable(handlers map[string]outbox.Handler, emitted []string) error {
	if len(handlers) == 0 {
		return nil
	}
	var missing []string
	for _, typ := range emitted {
		if _, ok := handlers[typ]; !ok && !slices.Contains(missing, typ) {
			missing = append(missing, typ)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return fmt.Errorf("outbox handler table is incomplete: no handler for %s (the worker would mark those events processed and lose them)", strings.Join(missing, ", "))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
