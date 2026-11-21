package archtest

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestNoBinaryLinksTestSupport pins that the server binaries never depend on test-support
// packages (the test identity provider, the Postgres test helper): once the identity module is
// wired into cmd/..., an import of one from non-test code would ship it.
func TestNoBinaryLinksTestSupport(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/...")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/...: %v", err)
	}
	deps := strings.Fields(string(out))
	if !slices.ContainsFunc(deps, func(d string) bool { return strings.HasSuffix(d, "/cmd/mcp-registry") }) {
		t.Fatal("go list did not report cmd/mcp-registry: the check would pass vacuously")
	}
	if bad := testSupportDeps(deps); len(bad) != 0 {
		t.Fatalf("a binary under cmd/ depends on test-support packages: %v", bad)
	}
}

// TestTestSupportDepsFindsThem is the failing fixture of that rule: a dependency list that has one.
func TestTestSupportDepsFindsThem(t *testing.T) {
	deps := []string{
		"fmt", "github.com/ai-pipes/mcp-registry/internal/kernel",
		"github.com/ai-pipes/mcp-registry/internal/modules/identity/infra/oidctest",
		"github.com/ai-pipes/mcp-registry/internal/platform/pgtest",
	}
	if bad := testSupportDeps(deps); len(bad) != 2 {
		t.Fatalf("found %v, want both test-support packages", bad)
	}
	if bad := testSupportDeps(deps[:2]); len(bad) != 0 {
		t.Fatalf("found %v in a clean list", bad)
	}
}

func testSupportDeps(deps []string) []string {
	var bad []string
	for _, d := range deps {
		if strings.HasSuffix(d, "/oidctest") || strings.HasSuffix(d, "/pgtest") {
			bad = append(bad, d)
		}
	}
	return bad
}
