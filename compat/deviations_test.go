//go:build compat

package compat_test

// Docker-free tests of the helpers that judge the decided behaviours (deviations, see
// compat/harness/deviations.go). The fixed branches cannot run against the 1.0.0 image, so each
// helper they depend on is tested here on synthetic data, with a case that must be refused next to
// the case that must be accepted.

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func TestAuditPagingProblem(t *testing.T) {
	want := []string{"1", "2", "3", "9", "10", "11", "12"}
	for _, c := range []struct {
		name string
		read []string
		bad  bool
	}{
		{"newest first, every record once", []string{"12", "11", "10", "9", "3", "2", "1"}, false},
		{"text order: 9 after 12", []string{"9", "3", "2", "12", "11", "10", "1"}, true},
		{"ascending", []string{"1", "2", "3", "9", "10", "11", "12"}, true},
		{"a record skipped at a page boundary", []string{"12", "11", "10", "3", "2", "1"}, true},
		{"a record returned twice", []string{"12", "11", "11", "10", "9", "3", "2", "1"}, true},
		{"a record that is not in the trail", []string{"13", "12", "11", "10", "9", "3", "2", "1"}, true},
		{"not a number", []string{"12", "x", "10", "9", "3", "2", "1"}, true},
		{"nothing read", nil, true},
	} {
		if got := auditPagingProblem(c.read, want); (got != "") != c.bad {
			t.Errorf("%s: problem %q, want a problem: %v", c.name, got, c.bad)
		}
	}
}

func TestCreateServerSyncFailureProblem(t *testing.T) {
	ok := harness.CLIResult{Code: 1, Stdout: "created server \"x\" (shared_secret); sync failed: Could not reach the upstream\n"}
	if p := createServerSyncFailureProblem(ok); p != "" {
		t.Errorf("the decided outcome was refused: %s", p)
	}
	onStderr := harness.CLIResult{Code: 1, Stderr: "error: sync failed\n"}
	if p := createServerSyncFailureProblem(onStderr); p != "" {
		t.Errorf("the decided outcome on stderr was refused: %s", p)
	}
	for name, r := range map[string]harness.CLIResult{
		"1.0.0: exit 0":     {Code: 0, Stdout: "created server \"x\"; sync failed: nope\n"},
		"exit 2":            {Code: 2, Stderr: "sync failed"},
		"exit 1, no report": {Code: 1, Stdout: "created server \"x\"\n"},
	} {
		if p := createServerSyncFailureProblem(r); p == "" {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestSearchReadyProblem(t *testing.T) {
	q := "create an issue"
	refs := []string{"youtrack.create_issue", "youtrack.update_issue"}
	if p := searchReadyProblem([]int{200, 200}, map[string][]string{q: refs}, map[string][]string{q: refs}); p != "" {
		t.Errorf("a ready start was refused: %s", p)
	}
	for name, c := range map[string]struct {
		healthz        []int
		first, settled map[string][]string
	}{
		"a 503":                  {[]int{200, 503}, map[string][]string{q: refs}, map[string][]string{q: refs}},
		"text-only first answer": {[]int{200}, map[string][]string{q: nil}, map[string][]string{q: refs}},
		"another order":          {[]int{200}, map[string][]string{q: {refs[1], refs[0]}}, map[string][]string{q: refs}},
		"both empty":             {[]int{200}, map[string][]string{q: nil}, map[string][]string{q: nil}},
	} {
		if p := searchReadyProblem(c.healthz, c.first, c.settled); p == "" {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestRetryAfterProblem(t *testing.T) {
	for _, ok := range [][]string{{"1"}, {"60"}, {"3600"}} {
		if p := retryAfterProblem(ok); p != "" {
			t.Errorf("%v refused: %s", ok, p)
		}
	}
	for _, bad := range [][]string{nil, {}, {"0"}, {"-3"}, {"abc"}, {"1.5"}, {" 7"}, {"07"}, {"60", "60"}, {"Wed, 21 Oct 2026 07:28:00 GMT"}} {
		if p := retryAfterProblem(bad); p == "" {
			t.Errorf("%v accepted", bad)
		}
	}
}
