package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/ai-pipes/mcp-registry/apps/server/migrations"
	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/ai-pipes/mcp-registry/internal/platform/migrate"
)

// usageOrder is the fixed order of the commands in the usage text (the order of USAGE in
// apps/server/src/platform/cli-commands.ts); registered commands not listed follow by name.
var usageOrder = []string{
	"create-user", "create-token", "login-link", "create-server", "sync", "reindex",
	"rotate-secrets-key", "reset-key-check",
}

// pendingUsage is the usage entry of each reference command the Go server does not implement yet
// (apps/server/src/platform/cli-commands.ts, with this image's name); a milestone that registers
// the command sets CLICommand.Usage and the entry here goes.
var pendingUsage = map[string]string{
	"reindex": `  reindex
      Rebuild the search index for every server.
`,
	"rotate-secrets-key": `  rotate-secrets-key
      Re-seal every stored secret (server secrets and people's tokens) from SECRETS_KEY_OLD
      to SECRETS_KEY. Safe to repeat.
`,
	"reset-key-check": `  reset-key-check --yes
      Remove the stored key check, so the next start records the configured SECRETS_KEY. Use it
      only after restoring the right key, or when the key is lost for good (stored server secrets
      and personal connections then stay unreadable and must be entered again).
`,
}

// usageFor is the usage text of the registered commands: a header, then each command's Usage entry
// in usageOrder. With no command at all it is the text of the server before the operator commands
// arrived.
func usageFor(cmds map[string]CLICommand) string {
	if len(cmds) == 0 {
		return "Usage: mcp-registry\n       mcp-registry --version\n" +
			"  Without a command the server starts. Operator commands arrive in a later release.\n"
	}
	var b strings.Builder
	b.WriteString("Usage: mcp-registry <command>\n       mcp-registry --version\n       mcp-registry (the server starts)\n\n")
	// Every command of the reference is named, also those whose milestone has not come: the
	// compatibility suite expects the whole list. A registered command's own Usage wins.
	for _, n := range usageOrder {
		if c, ok := cmds[n]; ok && c.Usage != "" {
			b.WriteString(c.Usage)
		} else {
			b.WriteString(pendingUsage[n])
		}
	}
	var rest []string
	for n := range cmds {
		if !slices.Contains(usageOrder, n) {
			rest = append(rest, n)
		}
	}
	slices.Sort(rest)
	for _, n := range rest {
		b.WriteString(cmds[n].Usage)
	}
	return b.String()
}

// UsageErr is a bad invocation: `error: <msg>`, the usage text, exit 2.
type UsageErr struct{ Msg string }

func (e *UsageErr) Error() string { return e.Msg }

// UsageError is a UsageErr with msg.
func UsageError(msg string) error { return &UsageErr{Msg: msg} }

// CommandErr is an operation that failed for a reason the operator can fix: `error: <msg>`,
// exit 1, no usage.
type CommandErr struct{ Msg string }

func (e *CommandErr) Error() string { return e.Msg }

// CommandError is a CommandErr with msg.
func CommandError(msg string) error { return &CommandErr{Msg: msg} }

// exitFor maps what a command returned to what the operator sees and the exit code, mirroring
// runCli in cli-commands.ts: a usage error is exit 2 with the usage, a command error and a domain
// error are exit 1. Any other error is `error: unexpected failure` and exit 1: the TypeScript CLI
// lets it escape as a stack trace, this one never prints the text of an unexpected error (it may
// quote values).
func exitFor(err error, stderr io.Writer, usage string, logger *slog.Logger) int {
	var usageErr *UsageErr
	var commandErr *CommandErr
	switch {
	case err == nil:
		return 0
	case errors.As(err, &usageErr):
		_, _ = io.WriteString(stderr, "error: "+usageErr.Msg+"\n"+usage)
		return 2
	case errors.As(err, &commandErr):
		_, _ = io.WriteString(stderr, "error: "+commandErr.Msg+"\n")
		return 1
	}
	if de, ok := kernel.AsDomainError(err); ok {
		_, _ = io.WriteString(stderr, "error: "+de.Code+": "+de.Message+"\n")
		return 1
	}
	// Type and neutral label only: the message may quote values.
	logger.Error("command failed", logging.ErrAttr(err))
	_, _ = io.WriteString(stderr, "error: unexpected failure\n")
	return 1
}

// runCommand runs a command and turns a panic into an error record with the call frames (never
// the value, which may carry a secret) and the unexpected-failure line, exit 1: an unrecovered
// panic would end the process with Go's exit code 2, the code of a bad invocation.
func runCommand(ctx context.Context, c CLICommand, cio CLIIO, args []string, logger *slog.Logger, usage string) (code int) {
	defer func() {
		if recover() != nil {
			logger.Error("command panicked", "stack", logging.PanicFrames())
			_, _ = io.WriteString(cio.Stderr, "error: unexpected failure\n")
			code = 1
		}
	}()
	return exitFor(c.Run(ctx, cio, args), cio.Stderr, usage, logger)
}

// msgKeyMismatchWarning is the one warning line printed before a command when the stored key
// check is not opened by the configured keys (guardKey in cli-commands.ts).
const msgKeyMismatchWarning = `warning: SECRETS_KEY does not match the data in this database. See "Recovering from a key mismatch" in the README.`

// msgKeyMismatchRefusal is what a GuardRefuse command gets instead of running (create-server).
const msgKeyMismatchRefusal = `error: SECRETS_KEY does not match the data in this database; refusing to seal a new secret with it. See "Recovering from a key mismatch" in the README.`

// CLIOptions configures RunCLI.
type CLIOptions struct {
	Config  config.Config
	Version string
	// Args is the command and its arguments (never empty: no command starts the server).
	Args           []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Env            map[string]string // the process environment, for commands that read a variable

	// Seam for tests in this package.
	modules func(Runtime) []Module // replaces modules (a stub module); nil means modules
}

// RunCLI runs one operator command and returns the exit code: 0 done, 1 the operation failed
// (database, migrations, the command), 2 bad invocation. The order is that of cli.ts: the
// configuration (already loaded by the caller), the pool, the migrations, then the command is
// looked up (an unknown one is exit 2 with the usage), the key guard (read-only; a mismatch is one
// `warning:` line on stderr and the command still runs), and the command. The CLI never starts the
// HTTP server, the outbox worker, the scheduler or FirstStart, and closes the pool before it
// returns. Logs go to stderr at warn and above, command output to stdout.
func RunCLI(ctx context.Context, o CLIOptions) int {
	name, args := "", []string(nil)
	if len(o.Args) > 0 {
		name, args = o.Args[0], o.Args[1:]
	}
	fail := func(msg string) int {
		_, _ = fmt.Fprintln(o.Stderr, "error: "+msg)
		return 1
	}

	cfg := o.Config
	openCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	pool, err := db.Open(openCtx, cfg.DatabaseURL.Reveal(), databasePassword(cfg))
	cancel()
	if err != nil {
		return fail(err.Error()) // db.Open's text never quotes the address or a password
	}
	defer pool.Close()

	if _, err := migrate.Run(ctx, pool, migrations.FS); err != nil {
		var newer *migrate.NewerDatabaseError
		var failed *migrate.MigrationError
		if errors.As(err, &newer) || errors.As(err, &failed) {
			return fail(err.Error()) // written for the operator, carries no data
		}
		return fail("migrations failed")
	}

	logger := logging.New(cliLogLevel(cfg.LogLevel), o.Stderr)
	rt, err := newRuntime(cfg, logger, pool, o.Version)
	if err != nil {
		return fail("cannot build the runtime")
	}
	asm, err := assemble(modulesOf(o.modules, rt))
	if err != nil {
		return fail(err.Error())
	}

	command, ok := asm.commands[name]
	if !ok {
		reason := "no command given"
		if name != "" {
			// The name is printed raw between double quotes, as the TypeScript template string does.
			reason = `unknown command "` + name + `"`
		}
		_, _ = io.WriteString(o.Stderr, "error: "+reason+"\n"+usageFor(asm.commands))
		return 2
	}

	if command.Guard != GuardNone {
		verdict, err := crypto.Inspect(ctx, pool, crypto.Keys{Current: cfg.SecretsKey.Reveal(), Previous: cfg.SecretsKeyOld.Reveal()})
		if err != nil {
			return fail("secrets key check failed")
		}
		if verdict == crypto.KeyCheckMismatch {
			if command.Guard == GuardRefuse {
				_, _ = fmt.Fprintln(o.Stderr, msgKeyMismatchRefusal)
				return 1
			}
			_, _ = fmt.Fprintln(o.Stderr, msgKeyMismatchWarning)
		}
	}
	stdin := o.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	cio := CLIIO{Stdin: stdin, Stdout: o.Stdout, Stderr: o.Stderr, Env: o.Env, Terminal: terminalOf(stdin)}
	return runCommand(ctx, command, cio, args, logger, usageFor(asm.commands))
}

// cliLogLevel keeps the CLI quiet: the server's default info becomes warn (cli.ts).
func cliLogLevel(level string) string {
	if level == "info" {
		return "warn"
	}
	return level
}
