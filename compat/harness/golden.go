//go:build compat

package harness

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files (only with COMPAT_REFERENCE=1)")

// referenceImageVar names another reference image than the built-in default (the published 1.0.1
// pinned by digest), e.g. a differently tagged build of the TypeScript image.
const referenceImageVar = "COMPAT_REFERENCE_IMAGE"

// CheckUpdateAllowed refuses -update unless COMPAT_REFERENCE=1 AND the image under test (see
// DefaultImage) is the reference image: the built-in default reference (the published 1.0.1 pinned
// by digest, which is not the default image under test), or exactly the value of
// COMPAT_REFERENCE_IMAGE when that is set. Without the second condition `COMPAT_IMAGE=<go image>
// make compat` with -update would rewrite the goldens from the server they are meant to judge.
func CheckUpdateAllowed(update bool) error {
	return checkUpdate(update, os.Getenv("COMPAT_REFERENCE"), os.Getenv(referenceImageVar), DefaultImage())
}

// checkUpdate is CheckUpdateAllowed over explicit inputs.
func checkUpdate(update bool, reference, referenceImage, image string) error {
	if !update {
		return nil
	}
	if reference != "1" {
		return errors.New("-update needs COMPAT_REFERENCE=1: golden files come only from the reference image")
	}
	want := defaultReferenceImage
	if referenceImage != "" {
		want = referenceImage
	}
	if image != want {
		return fmt.Errorf("-update refused: the image under test is %q, the reference image is %q (set COMPAT_REFERENCE_IMAGE to name another reference; goldens never come from the server they judge)", image, want)
	}
	return nil
}

func goldenDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("cannot locate the harness sources")
	}
	return filepath.Join(filepath.Dir(file), "..", "golden")
}

// Golden compares got with compat/golden/<name>; with -update it rewrites the file instead.
func Golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(goldenDir(), name)
	if err := CheckUpdateAllowed(*update); err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // goldens are committed files, readable like the rest of the tree
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (run with -update and COMPAT_REFERENCE=1 against the reference image)", name, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("golden %s differs\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}
