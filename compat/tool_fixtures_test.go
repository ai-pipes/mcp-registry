//go:build compat

package compat_test

// The recorded tools/list responses of the spike (CLAUDE.md "Upstream auth findings") are read by
// the suite, the Go tests and the generators from compat/fixtures/tools. They are copies of
// apps/server/test/fixtures/tools, which the TypeScript tests still read; while both exist they
// must stay byte-equal, so that the two implementations are judged on the same data. The test goes
// together with the originals when the TypeScript server is deleted. Docker-free (make go-verify).

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestToolFixtureCopiesEqualTheOriginals(t *testing.T) {
	for _, name := range []string{"gitea.json", "youtrack.json"} {
		original, err := os.ReadFile(filepath.Join("..", "apps", "server", "test", "fixtures", "tools", name))
		if err != nil {
			t.Fatalf("the original fixture: %v", err)
		}
		cp, err := os.ReadFile(filepath.Join("fixtures", "tools", name))
		if err != nil {
			t.Fatalf("the copy: %v", err)
		}
		if !bytes.Equal(original, cp) {
			t.Errorf("compat/fixtures/tools/%s differs from apps/server/test/fixtures/tools/%s: copy the original again (both implementations must be judged on the same recorded responses)", name, name)
		}
	}
}
