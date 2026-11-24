//go:build compat

package harness

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestReferenceImageFollowsItsVariable(t *testing.T) {
	t.Setenv(referenceImageVar, "")
	t.Setenv("COMPAT_IMAGE", "")
	if got := ReferenceImage(); got != defaultReferenceImage {
		t.Fatalf("default reference %q, want %q", got, defaultReferenceImage)
	}
	if ReferenceImage() == DefaultImage() {
		t.Fatalf("the default reference %q is the default image under test", ReferenceImage())
	}
	if got := ReferenceCLI(); got != "node dist/cli.js" {
		t.Fatalf("reference CLI %q", got)
	}
	t.Setenv(referenceImageVar, "ref:custom")
	t.Setenv("COMPAT_IMAGE", "under:test")
	if got := ReferenceImage(); got != "ref:custom" {
		t.Fatalf("reference %q, want the value of %s and not the image under test", got, referenceImageVar)
	}
}

func TestReferenceRunArgvHasNoNetworkAndReadOnlyMounts(t *testing.T) {
	argv := referenceRunArgv("ref:img", ReferenceRun{Mounts: []string{"/a:/b"}, Args: []string{"node", "x.mjs"}})
	for _, want := range [][]string{{"--network", "none"}, {"-v", "/a:/b:ro"}, {"--entrypoint", "node", "ref:img"}} {
		if i := slices.Index(argv, want[0]); i < 0 || !slices.Equal(argv[i:i+len(want)], want) {
			t.Fatalf("%v is missing %v", argv, want)
		}
	}
	if argv[len(argv)-1] != "ref:img" {
		t.Fatalf("the image must come last before the arguments: %v", argv)
	}
}

// The reference is the published 1.0.1, pinned by the digest of its multi-arch index, and every
// place that names it names the same pin: the Makefile (compat-reference, the parity run, the
// self-check base) and the default base of compat/selfcheck/Dockerfile.
func TestTheReferenceIsPinnedByDigestTheSameEverywhere(t *testing.T) {
	pinned := regexp.MustCompile(`^ghcr\.io/ai-pipes/mcp-registry:1\.0\.1@sha256:[0-9a-f]{64}$`)
	if !pinned.MatchString(defaultReferenceImage) {
		t.Fatalf("defaultReferenceImage %q is not the published 1.0.1 pinned by digest", defaultReferenceImage)
	}
	for _, c := range []struct{ file, prefix string }{
		{filepath.Join("..", "..", "Makefile"), "REFERENCE_IMAGE := "},
		{filepath.Join("..", "selfcheck", "Dockerfile"), "ARG BASE="},
	} {
		raw, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for line := range strings.Lines(string(raw)) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), c.prefix); ok {
				found = append(found, v)
			}
		}
		if len(found) != 1 || found[0] != defaultReferenceImage {
			t.Errorf("%s: %q lines %q, want exactly one with %q", c.file, c.prefix, found, defaultReferenceImage)
		}
	}
}
