//go:build compat

package harness

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files (only with COMPAT_REFERENCE=1)")

// CheckUpdateAllowed refuses -update unless the image under test is declared the reference.
func CheckUpdateAllowed(update bool) error {
	if update && os.Getenv("COMPAT_REFERENCE") != "1" {
		return errors.New("-update needs COMPAT_REFERENCE=1: golden files come only from the reference image")
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
		if err := os.WriteFile(path, got, 0o600); err != nil {
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
