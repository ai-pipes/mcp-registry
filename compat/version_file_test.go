//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// The version of the gateway is the root VERSION file (the Makefile, the Dockerfile, the release
// check, the smoke scripts and the TypeScript build read it). While apps/server/package.json
// exists it must say the same; the test goes away with that file (milestone 8, the deletion of
// apps/server). It lives here and not in apps/server so that the TypeScript tree gets no new file.
func TestVersionFileEqualsTheServerPackage(t *testing.T) {
	raw, err := os.ReadFile("../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	version := string(raw)
	if len(version) == 0 || version[len(version)-1] != '\n' {
		t.Fatalf("VERSION must be one line ending with a newline: %q", version)
	}
	version = version[:len(version)-1]
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(version) {
		t.Fatalf("VERSION is not MAJOR.MINOR.PATCH: %q", version)
	}
	pkgRaw, err := os.ReadFile("../apps/server/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(pkgRaw, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Version != version {
		t.Fatalf("VERSION is %q, apps/server/package.json says %q: change both", version, pkg.Version)
	}
}
