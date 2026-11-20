// Command mcp-registry is the Go implementation of the MCP registry gateway.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ai-pipes/mcp-registry/internal/platform/app"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// usage follows the TypeScript CLI's text; there are no operator commands yet.
const usage = `Usage: mcp-registry
       mcp-registry --version

  Without a command the server starts. Operator commands arrive in a later release.
`

// commands is the table of operator commands (name -> function returning the exit code).
var commands = map[string]func(args []string, stdout, stderr io.Writer) int{}

func readTextFile(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the file (SECRETS_KEY_FILE, PGPASSWORD_FILE); that is the feature
	return string(b), err
}

// run is the whole program except the process: no os.Exit, no signals, no global state. ctx stops
// the server (cancelled with an app.Signal cause on SIGTERM or SIGINT); env is the environment as
// a map. Exit codes: 0 stopped cleanly, 1 operation failed (configuration, start, shutdown),
// 2 bad invocation.
func run(ctx context.Context, args []string, env map[string]string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	// Like the TypeScript CLI, the configuration is read before the command is looked at.
	cfg, err := config.Load(env, readTextFile)
	if err != nil {
		// The text starts with "Invalid configuration:" and names variables, never values.
		_, _ = fmt.Fprintln(stderr, err.Error())
		return 1
	}
	if len(args) == 0 {
		return app.Run(ctx, app.Options{
			Config:  cfg,
			Version: version,
			Logger:  logging.New(cfg.LogLevel, stdout),
		})
	}
	// Migrations before the command (as in cli.ts) arrive with the first real command, in milestone 2.
	command, ok := commands[args[0]]
	if !ok {
		// The name is printed raw between double quotes, as the TypeScript template string does.
		_, _ = io.WriteString(stderr, "error: unknown command \""+args[0]+"\"\n"+usage)
		return 2
	}
	return command(args[1:], stdout, stderr)
}

func environ(pairs []string) map[string]string {
	env := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if k, v, ok := strings.Cut(p, "="); ok {
			env[k] = v
		}
	}
	return env
}

// signalName is the name the `shutting down` record carries.
func signalName(s os.Signal) string {
	if s == syscall.SIGTERM {
		return "SIGTERM"
	}
	return "SIGINT"
}

// watchSignals cancels the context with the first signal and swallows the rest: a second signal
// during the shutdown does not start another one and does not kill the process (the shutdown has
// its own limit).
func watchSignals(sigs <-chan os.Signal, cancel context.CancelCauseFunc) {
	for s := range sigs {
		cancel(app.Signal(signalName(s))) // only the first cause is kept
	}
}

func main() {
	ctx, cancel := context.WithCancelCause(context.Background())
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go watchSignals(sigs, cancel)
	os.Exit(run(ctx, os.Args[1:], environ(os.Environ()), os.Stdout, os.Stderr))
}
