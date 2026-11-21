package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestVersionFlagPrintsVersionAndExitsZero(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--version"}, &out, io.Discard)
	if code != 0 || strings.TrimSpace(out.String()) != version {
		t.Fatalf("code=%d out=%q want %q", code, out.String(), version)
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	var errOut bytes.Buffer
	code := run([]string{"nope"}, io.Discard, &errOut)
	if code != 2 {
		t.Fatalf("code=%d want 2", code)
	}
	if !strings.Contains(errOut.String(), "unknown command: nope") {
		t.Fatalf("stderr=%q", errOut.String())
	}
}

func TestNoArgumentsPrintsUsageAndExitsTwo(t *testing.T) {
	var errOut bytes.Buffer
	code := run(nil, io.Discard, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "usage:") {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
}
