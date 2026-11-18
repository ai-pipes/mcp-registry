//go:build compat

package harness

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// CLIResult is the outcome of one operator CLI invocation inside the app container.
type CLIResult struct {
	Stdout, Stderr string
	Code           int
}

// CLI runs the operator CLI (COMPAT_CLI plus args) in the app container through docker exec.
// stdin is passed when it is not empty.
func (s *Stack) CLI(t *testing.T, stdin string, args ...string) CLIResult {
	t.Helper()
	argv := []string{"exec"}
	if stdin != "" {
		argv = append(argv, "-i")
	}
	argv = append(argv, s.app)
	argv = append(argv, strings.Fields(s.cli)...)
	argv = append(argv, args...)
	cmd := exec.Command("docker", argv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	err := cmd.Run()
	res := CLIResult{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Code = ee.ExitCode()
	default:
		t.Fatalf("docker exec: %v", err)
	}
	return res
}
