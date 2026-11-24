//go:build compat

package harness

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// ReferenceImage is the reference implementation the suite compares the image under test with on
// the same machine: COMPAT_REFERENCE_IMAGE when set, else the published 1.0.1 pinned by digest
// (defaultReferenceImage; the same notion the -update guard uses). It may be the image under test
// itself.
func ReferenceImage() string { return envOr(referenceImageVar, defaultReferenceImage) }

// ReferenceCLI is the invocation prefix of the reference image's CLI. The reference is always the
// TypeScript image, whatever COMPAT_CLI says about the image under test.
func ReferenceCLI() string { return referenceCLI }

// RequireReferenceImage fails the test with a clear message when the reference image is not in the
// local Docker image store. Nothing is pulled here: `make compat-reference` pulls the published
// reference and checks its digest.
func RequireReferenceImage(t *testing.T) string {
	t.Helper()
	image := ReferenceImage()
	if _, err := dockerErr("image", "inspect", "-f", "{{.Id}}", image); err != nil {
		t.Fatalf("the reference image %q is not present locally (%v): pull it with `make compat-reference`, or name another one in COMPAT_REFERENCE_IMAGE; vectors and rankings are compared with the reference image on the same machine", image, err)
	}
	return image
}

// ReferenceRun is a one-off container of the reference image.
type ReferenceRun struct {
	Mounts []string // read-only bind mounts "host path:container path"
	Stdin  []byte
	Args   []string // the command and its arguments; the image's entrypoint is replaced by Args[0]
}

// referenceRunArgv builds the docker argument list: no network, removed on exit, labelled like the
// suite's containers.
func referenceRunArgv(image string, r ReferenceRun) []string {
	argv := []string{"run", "--rm", "-i", "--network", "none", "--label", suiteLabel, "--label", "compat.session=" + session}
	for _, m := range r.Mounts {
		argv = append(argv, "-v", m+":ro")
	}
	return append(argv, "--entrypoint", r.Args[0], image) // the rest of Args follows below
}

// RunInReference runs r in a container of the reference image with no network access and returns
// its stdout. A non-zero exit fails the test with the stderr text.
func RunInReference(t *testing.T, r ReferenceRun) []byte {
	t.Helper()
	if len(r.Args) == 0 {
		t.Fatal("RunInReference: no command")
	}
	image := RequireReferenceImage(t)
	argv := append(referenceRunArgv(image, r), r.Args[1:]...)
	cmd := exec.Command("docker", argv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Stdin = bytes.NewReader(r.Stdin)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("%s in the reference image failed (exit %d): %s", r.Args[0], ee.ExitCode(), strings.TrimSpace(errb.String()))
		}
		t.Fatal(fmt.Errorf("docker run: %w", err))
	}
	return out.Bytes()
}
