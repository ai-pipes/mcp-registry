//go:build compat && selfcheck

// Package selfcheck proves that the compat suite can fail. It builds mcp-registry:compat-broken
// (compat/selfcheck/Dockerfile: the reference image with a reverse proxy in front of the app),
// then runs named scenarios of the suite against it: once with the proxy transparent (the
// control: they must pass) and once per distortion (BREAK=<name>: the scenario that guards that
// property must fail, and fail for that reason, not because the stack did not boot).
//
// It is not part of `make compat`: run it with `make compat-selfcheck` (tag selfcheck).
//
// SELFCHECK_BASE is the image the derived one is built from (default mcp-registry:compat-ts).
package selfcheck

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	brokenImage = "mcp-registry:compat-broken"
	baseDefault = "mcp-registry:compat-ts"
)

// distortion is one BREAK value and the scenarios of which at least one must fail.
type distortion struct {
	name string // the value of BREAK; the proxy applies exactly this distortion
	// expected are the tests that guard the property: top-level names, or Test/subtest names (as
	// go test prints them) where the top-level test also holds scenarios the proxy itself can
	// break. One of them must print "--- FAIL".
	expected []string
}

var distortions = []distortion{
	{"csp", []string{"TestPlatformSecurityHeaders"}},
	{"origin", []string{"TestPlatformCSRF"}},
	{"extra-field", []string{"TestCatalogServers"}},
	{"status", []string{"TestCatalogOptimisticConcurrency"}},
	{"mcp-desc", []string{"TestMCPSurface"}},
	{"www-auth", []string{"TestMCPGuards/without_a_token_POST_/mcp_is_401_with_a_Bearer_challenge"}},
	{"audit-drop", []string{"TestGatewayAuditAPI"}},
	{"token-in-list", []string{"TestIdentityNoOIDC"}},
}

// bootFailure are the harness messages of a stack that did not come up; seeing one means the
// distortion broke the app's start, not the property under test.
var bootFailure = []string{"start stack:", "shared stack", "expected the app to exit"}

var (
	failLine   = regexp.MustCompile(`(?m)^\s*--- FAIL: (\S+)`)
	reasonLine = regexp.MustCompile(`(?m)^\s+\w+_test\.go:\d+: .{0,200}`)
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the sources")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func buildBrokenImage(t *testing.T, root string) {
	t.Helper()
	base := os.Getenv("SELFCHECK_BASE")
	if base == "" {
		base = baseDefault
	}
	cmd := exec.CommandContext(t.Context(), "docker", "build", "-q", "--build-arg", "BASE="+base, //nolint:gosec // G702: SELFCHECK_BASE is the operator's own image name
		"-t", brokenImage, "-f", "compat/selfcheck/Dockerfile", "compat/selfcheck")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build %s: %v\n%s", brokenImage, err, out)
	}
}

// runSuite runs the named top-level tests of ./compat/ against the broken image. breakValue "" is
// the control. COMPAT_REFERENCE is removed so a distorted image can never rewrite a golden file.
func runSuite(t *testing.T, root string, tests []string, breakValue string) (output string, exitCode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	run := "^(" + strings.Join(tests, "|") + ")$"
	cmd := exec.CommandContext(ctx, "go", "test", "-tags", "compat", "-count=1", "-timeout", "20m", "-run", run, "./compat/")
	cmd.Dir = root
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "COMPAT_REFERENCE=") && !strings.HasPrefix(kv, "COMPAT_IMAGE=") && !strings.HasPrefix(kv, "COMPAT_APP_ENV=") {
			env = append(env, kv)
		}
	}
	env = append(env, "COMPAT_IMAGE="+brokenImage)
	if breakValue != "" {
		env = append(env, "COMPAT_APP_ENV=BREAK="+breakValue)
	}
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok { //nolint:errorlint // ExitError is returned unwrapped by Run
		return buf.String(), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("go test did not run: %v\n%s", err, buf.String())
	}
	return buf.String(), 0
}

func TestSuiteFailsOnABrokenImage(t *testing.T) {
	root := repoRoot(t)
	buildBrokenImage(t, root)

	// Every top-level test named above, once.
	var all, withoutGuards []string
	seen := map[string]bool{}
	for _, d := range distortions {
		for _, e := range d.expected {
			top, _, _ := strings.Cut(e, "/")
			if !seen[top] {
				seen[top] = true
				all = append(all, top)
				if top != "TestMCPGuards" {
					withoutGuards = append(withoutGuards, top)
				}
			}
		}
	}

	t.Run("control: no proxy, the image as built", func(t *testing.T) {
		start := time.Now()
		out, code := runSuite(t, root, all, "")
		t.Logf("control took %s", time.Since(start).Round(time.Second))
		if code != 0 {
			t.Fatalf("the unproxied derived image fails the suite (exit %d), so a failure below proves nothing:\n%s", code, out)
		}
	})

	// The HTTP proxy with no distortion. TestMCPGuards is left out: its 10 MiB body-limit scenario
	// has the app answer 413 and close while the client still uploads, and an HTTP proxy in Node
	// loses that answer to the reset in about one run in ten (see proxy.mjs).
	t.Run("control: the proxy without a distortion", func(t *testing.T) {
		start := time.Now()
		out, code := runSuite(t, root, withoutGuards, "passthrough")
		t.Logf("control took %s", time.Since(start).Round(time.Second))
		if code != 0 {
			t.Fatalf("the proxy without a distortion fails the suite (exit %d), so a failure below proves nothing:\n%s", code, out)
		}
	})

	for _, d := range distortions {
		t.Run("BREAK="+d.name, func(t *testing.T) {
			start := time.Now()
			var tops []string
			for _, e := range d.expected {
				top, _, _ := strings.Cut(e, "/")
				tops = append(tops, top)
			}
			out, code := runSuite(t, root, tops, d.name)
			t.Logf("BREAK=%s took %s", d.name, time.Since(start).Round(time.Second))
			if code == 0 {
				t.Fatalf("the suite passes with BREAK=%s: no scenario guards this property (expected one of %v to fail)\n%s", d.name, d.expected, out)
			}
			for _, msg := range bootFailure {
				if strings.Contains(out, msg) {
					t.Fatalf("BREAK=%s failed because the stack did not boot (%q), not because of the distortion:\n%s", d.name, msg, out)
				}
			}
			var failed []string
			for _, m := range failLine.FindAllStringSubmatch(out, -1) {
				failed = append(failed, m[1])
			}
			for _, f := range failed {
				for _, e := range d.expected {
					if f == e {
						t.Logf("caught: --- FAIL: %s\n    first reason: %s", f, strings.TrimSpace(reasonLine.FindString(out)))
						return
					}
				}
			}
			t.Fatalf("BREAK=%s: exit %d, but none of %v failed (failed: %v):\n%s", d.name, code, d.expected, failed, out)
		})
	}
}
