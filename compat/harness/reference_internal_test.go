//go:build compat

package harness

import (
	"slices"
	"testing"
)

func TestReferenceImageFollowsItsVariable(t *testing.T) {
	t.Setenv(referenceImageVar, "")
	if got := ReferenceImage(); got != defaultImage {
		t.Fatalf("default reference %q, want %q", got, defaultImage)
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
