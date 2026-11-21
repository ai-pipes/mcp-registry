package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/ai-pipes/mcp-registry/apps/server/migrations"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/ai-pipes/mcp-registry/internal/platform/migrate"
)

// Usage is the text printed with a bad invocation (apps/server/src/platform/cli-commands.ts USAGE;
// it lists the commands the Go server has).
const Usage = `Usage: mcp-registry
       mcp-registry --version
  Without a command the server starts. Operator commands arrive in a later release.
`

// keyAwareCommands handle the secrets key themselves (rotate-secrets-key, reset-key-check): they
// run on a key mismatch without the guard. Neither exists in the Go server yet.
var keyAwareCommands = []string{"rotate-secrets-key", "reset-key-check"}

// msgKeyMismatchWarning is the one warning line printed before a command when the stored key
// check is not opened by the configured keys (guardKey in cli-commands.ts).
const msgKeyMismatchWarning = `warning: SECRETS_KEY does not match the data in this database. See "Recovering from a key mismatch" in the README.`

// CLIOptions configures RunCLI.
type CLIOptions struct {
	Config  config.Config
	Version string
	// Args is the command and its arguments (never empty: no command starts the server).
	Args           []string
	Stdout, Stderr io.Writer

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
		_, _ = io.WriteString(o.Stderr, "error: "+reason+"\n"+Usage)
		return 2
	}

	if !slices.Contains(keyAwareCommands, name) {
		verdict, err := crypto.Inspect(ctx, pool, crypto.Keys{Current: cfg.SecretsKey.Reveal(), Previous: cfg.SecretsKeyOld.Reveal()})
		if err != nil {
			return fail("secrets key check failed")
		}
		if verdict == crypto.KeyCheckMismatch {
			_, _ = fmt.Fprintln(o.Stderr, msgKeyMismatchWarning)
		}
	}
	return command.Run(ctx, args, o.Stdout, o.Stderr)
}

// cliLogLevel keeps the CLI quiet: the server's default info becomes warn (cli.ts).
func cliLogLevel(level string) string {
	if level == "info" {
		return "warn"
	}
	return level
}
