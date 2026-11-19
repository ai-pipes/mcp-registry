//go:build compat && e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/compat/e2e"
	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// hostResolverRules is what Chromium is told so that the names the fakes and the same-site stack
// use reach this machine: the fakes listen on every interface of the test process, the stacks on
// 127.0.0.1.
const hostResolverRules = "MAP host.docker.internal 127.0.0.1, MAP *.compat.test 127.0.0.1"

// TestBrowserFlows runs the Playwright flows (e2e/flows) in Chromium against the image under test
// (COMPAT_IMAGE) and fails when Playwright fails. Three stacks are booted: without OIDC (the
// sign-in by link and most flows), with the fake IdP, and one whose public address is
// gateway.compat.test with an OAuth provider at idp.compat.test, which makes the connect flow the
// same site as the gateway (the session cookie is SameSite=Strict).
func TestBrowserFlows(t *testing.T) {
	// E2E_PLAYWRIGHT_ARGS (space separated) selects flows, for example a file name. Arguments that
	// would let a run that tested nothing pass, or change the reporters this test reads, are refused.
	extra := strings.Fields(os.Getenv("E2E_PLAYWRIGHT_ARGS"))
	for _, a := range extra {
		if a == "--list" || a == "--pass-with-no-tests" || strings.HasPrefix(a, "--reporter") {
			t.Fatalf("E2E_PLAYWRIGHT_ARGS: %s is not allowed (it could make a run that tested nothing pass)", a)
		}
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("E2E_OUTPUT_DIR")
	if out == "" {
		out = filepath.Join(root, "node_modules", ".cache", "playwright", "flows-results")
	}

	idp := fakes.StartIdP(t)
	// Both providers show a consent page and wait for the click: the navigation that brings the
	// person back is then initiated by the provider's site, which is what decides whether the
	// SameSite=Strict session cookie travels with it (a provider that redirects at once would
	// let the gateway's own request be the initiator and hide the difference).
	localProvider := fakes.StartOAuthProvider(t, fakes.WithConsentPage())
	sameSiteProvider := fakes.StartOAuthProvider(t, fakes.WithConsentPage(), fakes.WithAdvertisedHost("idp.compat.test"))
	plain := harness.Start(t, harness.Options{})
	withIDP := harness.Start(t, harness.Options{OIDC: idp})
	sameSite := harness.Start(t, harness.Options{
		PublicHost: "gateway.compat.test",
		ExtraHosts: []string{"idp.compat.test:host-gateway"},
	})
	control := e2e.NewControl(t.Cleanup, []*e2e.Stack{
		{Name: "plain", Harness: plain, Provider: localProvider},
		{Name: "oidc", Harness: withIDP, OIDC: true},
		{Name: "samesite", Harness: sameSite, Provider: sameSiteProvider},
	}, idp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controlURL, err := control.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}

	report := filepath.Join(t.TempDir(), "report.json")
	args := []string{"exec", "playwright", "test", "-c", "e2e/flows/playwright.config.ts", "--reporter=list,json"}
	args = append(args, extra...)
	cmd := exec.CommandContext(ctx, "pnpm", args...) //nolint:gosec // G204: the arguments are the developer's own, from their environment
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"E2E_CONTROL_URL="+controlURL,
		"E2E_CONTROL_TOKEN="+control.Token(),
		"E2E_HOST_RESOLVER_RULES="+hostResolverRules,
		"E2E_OUTPUT_DIR="+out,
		"PLAYWRIGHT_JSON_OUTPUT_NAME="+report,
	)
	w := &logWriter{t: t}
	cmd.Stdout, cmd.Stderr = w, w // the same writer: exec copies both through one goroutine
	cmd.WaitDelay = 10 * time.Second
	t.Logf("running the browser flows against image %s", harness.DefaultImage())
	runErr := cmd.Run()
	w.flush()
	if ran, err := testsRun(report); err != nil {
		t.Errorf("reading the Playwright report: %v", err)
	} else if ran == 0 {
		t.Errorf("Playwright ran no test: a run that tested nothing is a failure")
	} else {
		t.Logf("Playwright ran %d tests", ran)
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			t.Errorf("playwright failed (exit %d). Traces and screenshots of the failed flows: %s (open one with `pnpm exec playwright show-trace <dir>/trace.zip`)", ee.ExitCode(), out)
		} else {
			t.Errorf("could not run playwright (is `pnpm install` done?): %v", runErr)
		}
	}
}

// testsRun counts the tests Playwright ran (expected, unexpected and flaky outcomes; skipped ones
// do not count) from its JSON report.
func testsRun(path string) (int, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a file in this test's temporary directory
	if err != nil {
		return 0, err
	}
	var rep struct {
		Stats struct{ Expected, Unexpected, Flaky int }
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return 0, err
	}
	return rep.Stats.Expected + rep.Stats.Unexpected + rep.Stats.Flaky, nil
}

// logWriter passes the lines of the subprocess to the test log.
type logWriter struct {
	t   *testing.T
	buf []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.t.Log(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}

func (w *logWriter) flush() {
	if len(w.buf) > 0 {
		w.t.Log(string(w.buf))
		w.buf = nil
	}
}
