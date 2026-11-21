package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/app"
)

func TestVersionFlagPrintsVersionAndExitsZero(t *testing.T) {
	var out bytes.Buffer
	code := run(context.Background(), []string{"--version"}, nil, strings.NewReader(""), &out, io.Discard)
	if code != 0 || out.String() != version+"\n" {
		t.Fatalf("code=%d out=%q want %q", code, out.String(), version)
	}
}

// validEnv passes the configuration check; the database address is unreachable.
var validEnv = map[string]string{
	"DATABASE_URL": "postgres://u@127.0.0.1:1/db",
	"PUBLIC_URL":   "http://localhost:3000",
	"SECRETS_KEY":  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
}

// Like cli.ts, a command is looked up after the migrations: with the database unreachable the
// run is an operation failure (exit 1, no usage), whatever the command is called. The unknown
// command with a database (exit 2) is in main_int_test.go.
func TestACommandNeedsTheDatabaseFirst(t *testing.T) {
	var errOut, out bytes.Buffer
	code := run(context.Background(), []string{"anything"}, validEnv, strings.NewReader(""), &out, &errOut)
	if code != 1 || out.Len() != 0 || !strings.HasPrefix(errOut.String(), "error: cannot connect to the database") || strings.Contains(errOut.String(), "Usage:") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

// The configuration is read first: a bare environment is a configuration error, not usage.
func TestUnknownCommandWithBareEnvironmentReportsTheConfiguration(t *testing.T) {
	var errOut, out bytes.Buffer
	code := run(context.Background(), []string{"version"}, nil, strings.NewReader(""), &out, &errOut)
	if code != 1 || out.Len() != 0 || !strings.HasPrefix(errOut.String(), "Invalid configuration:\n") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if strings.Contains(errOut.String(), "unknown command") {
		t.Fatal("the command was looked at before the configuration")
	}
}

func TestVersionNeedsNoConfiguration(t *testing.T) {
	var out bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, nil, strings.NewReader(""), &out, io.Discard); code != 0 || out.String() != version+"\n" {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}

// compat: TestConfigRefused: the report on stderr, exit 1, nothing on stdout.
func TestBadConfigurationExitsOneWithReportOnStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), nil, map[string]string{"PORT": "99999"}, strings.NewReader(""), &out, &errOut)
	if code != 1 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, out.String())
	}
	got := errOut.String()
	if !strings.HasPrefix(got, "Invalid configuration:\n") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("stderr=%q", got)
	}
	for _, name := range []string{"PORT", "DATABASE_URL", "SECRETS_KEY", "PUBLIC_URL"} {
		if !strings.Contains(got, "  "+name+": ") {
			t.Errorf("the report does not name %s: %q", name, got)
		}
	}
	if strings.Contains(got, "99999") {
		t.Errorf("the report quotes a value: %q", got)
	}
}

func TestEnvironIsAMap(t *testing.T) {
	env := environ([]string{"A=1", "B=x=y", "C=", "noequals"})
	if env["A"] != "1" || env["B"] != "x=y" || env["C"] != "" || len(env) != 3 {
		t.Fatalf("%v", env)
	}
}

// A second signal neither changes the cause nor ends the pump.
func TestOnlyTheFirstSignalCounts(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	sigs := make(chan os.Signal, 2)
	done := make(chan struct{})
	go func() { watchSignals(sigs, cancel); close(done) }()
	sigs <- syscall.SIGINT
	sigs <- syscall.SIGTERM
	close(sigs)
	<-done
	if cause := context.Cause(ctx); cause != app.Signal("SIGINT") {
		t.Fatalf("cause %v", cause)
	}
}
