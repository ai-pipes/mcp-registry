package app

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// With no registered command the usage is the text of today's server.
const usageWithoutCommands = `Usage: mcp-registry
       mcp-registry --version
  Without a command the server starts. Operator commands arrive in a later release.
`

func TestUsageWithoutCommandsIsTheCurrentText(t *testing.T) {
	if got := usageFor(nil); got != usageWithoutCommands {
		t.Fatalf("usage:\n%s", got)
	}
}

func TestUsageListsTheRegisteredCommandsInAFixedOrder(t *testing.T) {
	cmds := map[string]CLICommand{
		"create-token": {Name: "create-token", Usage: "  create-token <sub> --name <name>\n      Create a token.\n"},
		"create-user":  {Name: "create-user", Usage: "  create-user --sub <sub> --name <name>\n      Create a person.\n"},
		"zzz-extra":    {Name: "zzz-extra", Usage: "  zzz-extra\n"},
	}
	got := usageFor(cmds)
	i, j, k := strings.Index(got, "create-user"), strings.Index(got, "create-token"), strings.Index(got, "zzz-extra")
	if i <= 0 || i >= j || j >= k {
		t.Fatalf("order create-user, create-token, then the others:\n%s", got)
	}
	if strings.Contains(got, "arrive in a later release") {
		t.Errorf("the placeholder stays with commands present:\n%s", got)
	}
	if !strings.HasPrefix(got, "Usage: mcp-registry") || !strings.Contains(got, "--version") {
		t.Errorf("header:\n%s", got)
	}
}

func TestEveryErrorIsMappedInOnePlace(t *testing.T) {
	const usage = "USAGE\n"
	for _, tc := range []struct {
		name   string
		err    error
		code   int
		stderr string
	}{
		{"nil", nil, 0, ""},
		{"usage error", UsageError("--sub is required"), 2, "error: --sub is required\nUSAGE\n"},
		{"wrapped usage error", fmt.Errorf("parse: %w", UsageError("bad")), 2, "error: bad\nUSAGE\n"},
		{"command error", CommandError("cannot read x.json"), 1, "error: cannot read x.json\n"},
		{"domain error", kernel.NewDomainError("not_found", "no such person"), 1, "error: not_found: no such person\n"},
		{"wrapped domain error", fmt.Errorf("ctx: %w", kernel.NewDomainError("disabled", "person is disabled")), 1, "error: disabled: person is disabled\n"},
		// The TypeScript CLI lets an unexpected error escape as a stack trace; ours prints no text.
		{"unexpected error", errors.New("dial tcp 10.0.0.1: secret-token-xyz"), 1, "error: unexpected failure\n"},
	} {
		var out bytes.Buffer
		if code := exitFor(tc.err, &out, usage, discardLogger()); code != tc.code || out.String() != tc.stderr {
			t.Errorf("%s: %d %q, want %d %q", tc.name, code, out.String(), tc.code, tc.stderr)
		}
	}
}

// The server's default info becomes warn in the CLI (cli.ts); every other level is unchanged.
func TestCLILogLevelRule(t *testing.T) {
	for in, want := range map[string]string{
		"info": "warn", "warn": "warn", "debug": "debug", "trace": "trace", "error": "error",
		"fatal": "fatal", "silent": "silent",
	} {
		if got := cliLogLevel(in); got != want {
			t.Errorf("cliLogLevel(%q) = %q, want %q", in, got, want)
		}
	}
}
