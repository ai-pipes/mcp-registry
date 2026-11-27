//go:build compat

package compat_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The upgrade scenario's source under make compat and make compat-reference-run (CI's jobs compat and
// compat-reference) is the published 1.0.0
// pinned by the digest of its multi-arch index, named once in the Makefile (UPGRADE_FROM_IMAGE);
// CI takes it from there (make compat-upgrade-from, make -s print-upgrade-from-image) and never
// pulls or names the release by its tag alone, which a re-pushed tag could change. Docker-free.
func TestUpgradeSourceIsThePublished100PinnedByDigest(t *testing.T) {
	raw, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for line := range strings.Lines(string(raw)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "UPGRADE_FROM_IMAGE := "); ok {
			found = append(found, v)
		}
	}
	pinned := regexp.MustCompile(`^ghcr\.io/ai-pipes/mcp-registry:1\.0\.0@sha256:[0-9a-f]{64}$`)
	if len(found) != 1 || !pinned.MatchString(found[0]) {
		t.Fatalf("Makefile: UPGRADE_FROM_IMAGE lines %q, want exactly one naming the published 1.0.0 by digest", found)
	}
	ci, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ci), "mcp-registry:1.0.0") {
		t.Error("ci.yml names the 1.0.0 image itself: take it from the Makefile (make compat-upgrade-from, print-upgrade-from-image)")
	}
	if n := strings.Count(string(ci), "make compat-upgrade-from"); n != 2 {
		t.Errorf("ci.yml runs make compat-upgrade-from %d time(s), want 2 (the jobs compat and compat-reference)", n)
	}
}
