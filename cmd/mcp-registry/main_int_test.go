//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
)

func envFor(url string) map[string]string {
	return map[string]string{
		"DATABASE_URL": url,
		"PUBLIC_URL":   "http://localhost:3000",
		"SECRETS_KEY":  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	}
}

// compat: TestCLIVersion: "version" is not a command; the name is printed raw. The usage follows.
func TestUnknownCommandWithADatabaseExitsTwo(t *testing.T) {
	var errOut, out bytes.Buffer
	code := run(context.Background(), []string{`ver\sion`}, envFor(pgtest.StartURL(t)), strings.NewReader(""), &out, &errOut)
	if code != 2 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, out.String())
	}
	if !strings.HasPrefix(errOut.String(), "error: unknown command \"ver\\sion\"\n") || !strings.Contains(errOut.String(), "Usage:") {
		t.Fatalf("stderr=%q", errOut.String())
	}
}
