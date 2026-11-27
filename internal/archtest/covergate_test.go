package archtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const m = "github.com/ai-pipes/mcp-registry/internal/modules/"

func profile(lines ...string) string { return "mode: set\n" + strings.Join(lines, "\n") + "\n" }

func TestParseCoverProfileSumsStatementsPerPackage(t *testing.T) {
	cov, err := ParseCoverProfile(strings.NewReader(profile(
		m+"catalog/domain/a.go:1.1,2.2 4 1",
		m+"catalog/domain/a.go:3.1,4.2 6 0",
		m+"catalog/domain/b.go:1.1,2.2 10 1",
		m+"catalog/app/c.go:1.1,2.2 5 3",
		"github.com/ai-pipes/mcp-registry/internal/kernel/k.go:1.1,2.2 9 1",
	)))
	if err != nil {
		t.Fatal(err)
	}
	if got := cov["catalog/domain"]; got != (PackageCoverage{Statements: 20, Covered: 14}) {
		t.Fatalf("catalog/domain = %+v", got)
	}
	if got := cov["catalog/app"]; got != (PackageCoverage{Statements: 5, Covered: 5}) {
		t.Fatalf("catalog/app = %+v", got)
	}
	if len(cov) != 2 {
		t.Fatalf("files outside the modules must be ignored: %+v", cov)
	}
}

func TestParseCoverProfileCountsAMergedBlockOnce(t *testing.T) {
	cov, err := ParseCoverProfile(strings.NewReader(profile(
		m+"search/app/a.go:1.1,2.2 4 0",
		m+"search/app/a.go:1.1,2.2 4 2",
	)))
	if err != nil {
		t.Fatal(err)
	}
	if got := cov["search/app"]; got != (PackageCoverage{Statements: 4, Covered: 4}) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseCoverProfileRefusesGarbage(t *testing.T) {
	for _, in := range []string{"hello\n", "mode: set\nnot a block\n", "mode: set\n" + m + "x/app/a.go:1.1,2.2 x 1\n", "mode: set\n" + m + "x/app/a.go:1.1,2.2 4 y\n"} {
		if _, err := ParseCoverProfile(strings.NewReader(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestCheckCoverageThresholdBoundary(t *testing.T) {
	exp := []string{"a/domain", "b/app"}
	cov := map[string]PackageCoverage{
		"a/domain": {Statements: 100, Covered: 90}, // exactly the floor passes
		"b/app":    {Statements: 1000, Covered: 899},
	}
	_, failures := CheckCoverage(cov, exp, 90)
	if len(failures) != 1 || !strings.HasPrefix(failures[0], "b/app: 89.9%") {
		t.Fatalf("failures = %v", failures)
	}
}

func TestCheckCoverageFailsAMissingOrEmptyPackage(t *testing.T) {
	exp := []string{"a/domain", "b/app", "c/app"}
	cov := map[string]PackageCoverage{
		"a/domain": {Statements: 10, Covered: 10},
		"b/app":    {Statements: 0, Covered: 0},
	}
	report, failures := CheckCoverage(cov, exp, 90)
	if len(failures) != 2 || !strings.HasPrefix(failures[0], "b/app:") || !strings.HasPrefix(failures[1], "c/app:") {
		t.Fatalf("failures = %v", failures)
	}
	if len(report) != 3 {
		t.Fatalf("one report line per package, got %v", report)
	}
}

func TestCheckCoverageNamesEveryFailingPackage(t *testing.T) {
	cov := map[string]PackageCoverage{"a/domain": {10, 1}, "b/app": {10, 2}}
	_, failures := CheckCoverage(cov, []string{"a/domain", "b/app"}, 90)
	if len(failures) != 2 || !strings.Contains(failures[0], "10.0%") || !strings.Contains(failures[1], "20.0%") {
		t.Fatalf("failures = %v", failures)
	}
}

func TestExpectedPackagesExistInTheRepository(t *testing.T) {
	got, err := ExpectedPackages("../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(GatedModules)*len(GatedLayers) {
		t.Fatalf("got %v", got)
	}
}

func TestExpectedPackagesFailsOnAMissingDirectory(t *testing.T) {
	if _, err := ExpectedPackages(t.TempDir()); err == nil {
		t.Fatal("an empty root must fail")
	}
}

// TestCoverageThresholdIsNinety pins the floor. The gate is never lowered (CLAUDE.md), and a
// constant that can be edited without any test noticing is not a rule. Lowering it must mean
// editing this test too, which a reviewer sees; raising it is a deliberate edit of the same kind.
func TestCoverageThresholdIsNinety(t *testing.T) {
	if CoverageThreshold != 90.0 {
		t.Fatalf("CoverageThreshold = %v, want exactly 90: the gate is never lowered", CoverageThreshold)
	}
}

func TestModuleGateProblemsOfTheRealTreeAreNone(t *testing.T) {
	if p := ModuleGateProblems("../modules", GatedModules, GateExemptions); len(p) != 0 {
		t.Fatalf("problems: %v", p)
	}
}

func TestEveryGateExemptionHasAReason(t *testing.T) {
	for k, why := range GateExemptions {
		if strings.TrimSpace(why) == "" {
			t.Errorf("exemption %s has no reason", k)
		}
	}
}

func TestModuleGateProblemsNamesWhatIsWrong(t *testing.T) {
	gated := []string{"alpha", "beta"}
	for _, tc := range []struct {
		name, dir string
		exempt    map[string]string
		want      string // substring of the one problem, "" = none
	}{
		{"an unlisted module", "testdata/gate-module-unlisted/modules", map[string]string{"beta/app": "no use cases yet"}, "rogue"},
		{"a missing layer", "testdata/gate-module-ok/modules", nil, "beta/app"},
		{"an exemption without a reason", "testdata/gate-module-ok/modules", map[string]string{"beta/app": " "}, "beta/app"},
		{"an exempted layer", "testdata/gate-module-ok/modules", map[string]string{"beta/app": "no use cases yet"}, ""},
		{"a listed module that does not exist", "testdata/gate-module-ok/modules", map[string]string{"beta/app": "x", "gamma/domain": "x"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gated
			if tc.name == "a listed module that does not exist" {
				g = []string{"alpha", "beta", "gamma"}
				tc.want = "gamma"
			}
			p := ModuleGateProblems(tc.dir, g, tc.exempt)
			if tc.want == "" {
				if len(p) != 0 {
					t.Fatalf("problems: %v", p)
				}
				return
			}
			if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), tc.want) {
				t.Fatalf("problems = %v, want one naming %q", p, tc.want)
			}
		})
	}
}

func TestExpectedPackagesFailsForAnUnlistedModule(t *testing.T) {
	root := t.TempDir()
	for _, m := range append(append([]string(nil), GatedModules...), "sixth") {
		for _, l := range GatedLayers {
			if err := os.MkdirAll(filepath.Join(root, "internal", "modules", m, l), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, err := ExpectedPackages(root)
	if err == nil || !strings.Contains(err.Error(), "sixth") {
		t.Fatalf("err = %v, want one naming the unlisted module", err)
	}
}
