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
	code := run(context.Background(), []string{"--version"}, nil, &out, io.Discard)
	if code != 0 || out.String() != version+"\n" {
		t.Fatalf("code=%d out=%q want %q", code, out.String(), version)
	}
}

// compat: TestCLIVersion needs no configuration for --version, and "version" is not a command.
func TestUnknownCommandExitsTwoWithoutReadingConfiguration(t *testing.T) {
	var errOut, out bytes.Buffer
	code := run(context.Background(), []string{"version"}, nil, &out, &errOut)
	if code != 2 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, out.String())
	}
	if !strings.HasPrefix(errOut.String(), `error: unknown command "version"`+"\n") || !strings.Contains(errOut.String(), "Usage:") {
		t.Fatalf("stderr=%q", errOut.String())
	}
	if strings.Contains(errOut.String(), "Invalid configuration") {
		t.Fatal("the configuration was read for an unknown command")
	}
}

// compat: TestConfigRefused: the report on stderr, exit 1, nothing on stdout.
func TestBadConfigurationExitsOneWithReportOnStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), nil, map[string]string{"PORT": "99999"}, &out, &errOut)
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
