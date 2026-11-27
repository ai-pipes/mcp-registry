//go:build compat && selfcheck

// Package selfcheck proves that the compat suite can fail, and fail for the right reason. It
// builds mcp-registry:compat-broken (compat/selfcheck/Dockerfile: the reference image with an
// optional distorting proxy in front of the app, selected by BREAK), then runs named scenarios of
// the suite against it:
//
//   - two controls that must pass: the image without a proxy (BREAK unset: proves the derived image
//     itself works; it is NOT a control for the distortion runs, which go through the proxy) and
//     the proxy with no distortion (BREAK=passthrough), which must pass every scenario a distortion
//     case relies on except one the proxy cannot relay (see proxy.mjs);
//   - one run per distortion (BREAK=<name>). Such a run passes only if the named subtests fail,
//     their failure text contains the reason only that distortion produces, and a canary scenario
//     that the distortion cannot affect passes in the same run. A failure for any other reason (the
//     stack did not boot, the proxy died, a flake of an unrelated subtest) does not satisfy it.
//
// It is not part of `make compat`: run it with `make compat-selfcheck` (tag selfcheck).
//
// SELFCHECK_BASE is the image the derived one is built from (default: the reference image,
// harness.ReferenceImage, i.e. the published 1.0.1 pinned by digest; make compat-selfcheck pulls it).
package selfcheck

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/compat/harness"
)

const (
	brokenImage = "mcp-registry:compat-broken"

	// canary is a cheap scenario no distortion can affect; it runs next to the expected ones and
	// must pass in the same run, so a proxy that broke something else cannot pass for a catch.
	canary = "TestPlatformHealthz"

	// bodyLimit is the one subtest an HTTP proxy in Node cannot relay reliably (the app answers
	// 413 to a 10 MiB upload and closes; see proxy.mjs). The passthrough control skips it.
	bodyLimitSkip = "TestMCPGuards/the_body_limit_of_"
)

// distortion is one BREAK value, the subtests that must fail and why.
type distortion struct {
	name string
	// mustFail are the full paths of the subtests, as go test prints them after "--- FAIL: ". All
	// of them must fail.
	mustFail []string
	// reason must match the output of the run; only this distortion can produce it.
	reason *regexp.Regexp
}

var distortions = []distortion{
	{
		"csp",
		[]string{"TestPlatformSecurityHeaders/index"},
		regexp.MustCompile(`index: header Content-Security-Policy = \[[^\]]*img-src 'self' data:"\], want exactly .*frame-ancestors 'none'`),
	},
	{
		"origin",
		[]string{"TestPlatformCSRF/without_Origin", "TestPlatformCSRF/foreign_Origin"},
		regexp.MustCompile(`Origin http://evil\.example: 201 `),
	},
	{
		"extra-field",
		[]string{"TestCatalogServers/the_server_list_is_ordered_by_slug_and_counts_visible_tools_only"},
		regexp.MustCompile(`schema ServerList: at /0: additional properties 'secret' not allowed`),
	},
	{
		"status",
		[]string{"TestCatalogOptimisticConcurrency/PUT_/api/servers/:slug_answers_409_server_conflict_when_another_change_commits_meanwhile"},
		regexp.MustCompile(`racing a change: status 200, want 409 \(body \{"error":\{"code":"server_conflict"`),
	},
	{
		"mcp-desc",
		[]string{"TestMCPSurface/the_golden_holds_the_four_tools,_the_instructions,_the_server_info_and_the_capabilities"},
		regexp.MustCompile(`(?s)golden mcp-surface\.json differs.*\(distorted\)`),
	},
	{
		"www-auth",
		[]string{"TestMCPGuards/without_a_token_POST_/mcp_is_401_with_a_Bearer_challenge"},
		regexp.MustCompile(`WWW-Authenticate = "", want "Bearer"`),
	},
	{
		"audit-drop",
		[]string{"TestGatewayAuditAPI/the_trail_holds_what_was_done,_by_whom,_to_what"},
		regexp.MustCompile(`the member's connection\.pat_saved: 0 records \[\], want 1`),
	},
	{
		"token-in-list",
		[]string{"TestIdentityNoOIDC/an_sk__token:_shape,_stored_only_as_SHA-256,_listed_without_its_value,_works_on_/mcp"},
		regexp.MustCompile(`schema TokenSummary\[\]: at /0: additional properties 'token' not allowed`),
	},
}

// bootFailure are the harness messages of a stack that did not come up; seeing one means the
// distortion (or the proxy) broke the app's start, not the property under test.
var bootFailure = []string{"start stack:", "shared stack", "expected the app to exit"}

var resultLine = regexp.MustCompile(`(?m)^\s*--- (FAIL|PASS): (\S+)`)

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
		base = harness.ReferenceImage()
	}
	cmd := exec.CommandContext(t.Context(), "docker", "build", "-q", "--build-arg", "BASE="+base, //nolint:gosec // G702: SELFCHECK_BASE is the operator's own image name
		"-t", brokenImage, "-f", "compat/selfcheck/Dockerfile", "compat/selfcheck")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build %s: %v\n%s", brokenImage, err, out)
	}
}

// topLevel is the Test... part of a test path.
func topLevel(path string) string {
	top, _, _ := strings.Cut(path, "/")
	return top
}

// runSuite runs the named top-level tests of ./compat/ against the broken image, verbosely, with
// skip (a go test -skip pattern, "" for none). breakValue "" leaves BREAK unset. COMPAT_REFERENCE
// is removed so a distorted image can never rewrite a golden file.
func runSuite(t *testing.T, root string, tests []string, skip, breakValue string) (output string, exitCode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	args := []string{"test", "-tags", "compat", "-count=1", "-v", "-timeout", "20m", "-run", "^(" + strings.Join(tests, "|") + ")$"}
	if skip != "" {
		args = append(args, "-skip", skip)
	}
	cmd := exec.CommandContext(ctx, "go", append(args, "./compat/")...)
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
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return buf.String(), exitErr.ExitCode()
	}
	if err != nil {
		t.Fatalf("go test did not run: %v\n%s", err, buf.String())
	}
	return buf.String(), 0
}

// results maps every "--- PASS|FAIL: name" line of a verbose run to its verdict.
func results(out string) map[string]string {
	m := map[string]string{}
	for _, r := range resultLine.FindAllStringSubmatch(out, -1) {
		m[r[2]] = r[1]
	}
	return m
}

func checkNoBootFailure(t *testing.T, out string) {
	t.Helper()
	for _, msg := range bootFailure {
		if strings.Contains(out, msg) {
			t.Fatalf("the stack did not boot (%q), so nothing below is about the distortion:\n%s", msg, out)
		}
	}
}

func TestSuiteFailsOnABrokenImage(t *testing.T) {
	root := repoRoot(t)
	buildBrokenImage(t, root)

	// Every top-level test a case relies on, once, and every subtest a case names.
	tops := []string{canary}
	var named []string
	seen := map[string]bool{canary: true}
	for _, d := range distortions {
		for _, p := range d.mustFail {
			named = append(named, p)
			if top := topLevel(p); !seen[top] {
				seen[top] = true
				tops = append(tops, top)
			}
		}
	}

	control := func(t *testing.T, skip, breakValue string) {
		t.Helper()
		start := time.Now()
		out, code := runSuite(t, root, tops, skip, breakValue)
		t.Logf("took %s", time.Since(start).Round(time.Second))
		if code != 0 {
			t.Fatalf("the control fails the suite (exit %d), so a failure in the distortion runs would prove nothing:\n%s", code, out)
		}
		got := results(out)
		for _, p := range append([]string{canary}, named...) {
			if got[p] != "PASS" {
				t.Fatalf("control: %s is %q, want PASS:\n%s", p, got[p], out)
			}
		}
		t.Logf("every named subtest and the canary printed --- PASS (%d subtests and the canary)", len(named))
	}

	t.Run("control: the image without a proxy (BREAK unset)", func(t *testing.T) { control(t, "", "") })
	t.Run("control: the proxy without a distortion (BREAK=passthrough)", func(t *testing.T) {
		control(t, bodyLimitSkip, "passthrough")
	})

	for _, d := range distortions {
		t.Run("BREAK="+d.name, func(t *testing.T) {
			start := time.Now()
			var run []string
			for _, p := range d.mustFail {
				run = append(run, topLevel(p))
			}
			out, code := runSuite(t, root, append(run, canary), "", d.name)
			t.Logf("took %s", time.Since(start).Round(time.Second))
			if code == 0 {
				t.Fatalf("the suite passes with BREAK=%s: no scenario guards this property (expected %v to fail)\n%s", d.name, d.mustFail, out)
			}
			checkNoBootFailure(t, out)
			got := results(out)
			if got[canary] != "PASS" {
				t.Fatalf("wrong reason: the canary %s is %q (want PASS): the proxy broke something besides the distortion:\n%s", canary, got[canary], out)
			}
			for _, p := range d.mustFail {
				if got[p] != "FAIL" {
					t.Fatalf("BREAK=%s: %s is %q, want FAIL (failed: %v):\n%s", d.name, p, got[p], failedOf(got), out)
				}
			}
			if !d.reason.MatchString(out) {
				t.Fatalf("wrong reason: the output has no text matching %s:\n%s", d.reason, out)
			}
			t.Logf("caught: --- FAIL: %s\n    reason: %s\n    canary: --- PASS: %s", strings.Join(d.mustFail, "\n            --- FAIL: "), firstLine(d.reason.FindString(out)), canary)
		})
	}
}

func failedOf(got map[string]string) []string {
	var out []string
	for name, verdict := range got {
		if verdict == "FAIL" {
			out = append(out, name)
		}
	}
	return out
}

// firstLine is the first line of s, cut to 200 bytes (a reason may span a long diff).
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > 200 {
		line = line[:200] + "..."
	}
	return line
}
