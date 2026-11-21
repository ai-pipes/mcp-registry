package archtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const repoRoot = "../.."

// pinnedTool returns the module@version reference the Makefile pins for a tool target.
func pinnedTool(t *testing.T, target string) string {
	t.Helper()
	out, err := exec.Command("make", "-s", "-C", repoRoot, target).Output()
	if err != nil {
		t.Fatalf("make %s: %v", target, err)
	}
	return strings.TrimSpace(string(out))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// copyTree copies every file below src into dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		writeFile(t, filepath.Join(dst, rel), readFile(t, path))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// skeleton writes an empty package (doc.go) for every directory of the repository's internal/
// tree that holds Go code, except this package, so the real .go-arch-lint.yml finds every
// component. A fixture is overlaid on it and adds its single violation.
func skeleton(t *testing.T, dst string) {
	t.Helper()
	internal := filepath.Join(repoRoot, "internal")
	// The migrations package sits outside internal/ but is a component of its own.
	writeFile(t, filepath.Join(dst, "apps", "server", "migrations", "doc.go"), []byte("package migrations\n"))
	err := filepath.WalkDir(internal, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "archtest" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(internal, filepath.Dir(path))
		pkg := filepath.Base(rel)
		writeFile(t, filepath.Join(dst, "internal", rel, "doc.go"), []byte("package "+pkg+"\n"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type archReport struct {
	Payload struct {
		ExecutionWarnings      []json.RawMessage
		ArchWarningsDeps       []struct{ ComponentName, ResolvedImportName string }
		ArchWarningsNotMatched []json.RawMessage
		ArchWarningsDeepScan   []json.RawMessage
	}
}

// archLint runs the pinned go-arch-lint with the repository's real .go-arch-lint.yml against
// project, a directory holding go.mod and internal/. It returns the parsed report and the exit
// code (0 = clean).
func archLint(t *testing.T, project string) (archReport, int) {
	t.Helper()
	writeFile(t, filepath.Join(project, ".go-arch-lint.yml"), readFile(t, filepath.Join(repoRoot, ".go-arch-lint.yml")))
	cmd := exec.Command("go", "run", pinnedTool(t, "print-go-arch-lint"), "check", "--project-path", project, "--json", "--output-json-one-line")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("go-arch-lint did not run: %v\n%s", err, stderr.String())
		}
		code = exit.ExitCode()
	}
	var rep archReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("go-arch-lint output is not the expected JSON (%v):\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return rep, code
}

// fixtureProject builds a temp project from the repo's go.mod, the skeleton and the fixture.
func fixtureProject(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), readFile(t, filepath.Join(repoRoot, "go.mod")))
	skeleton(t, dir)
	copyTree(t, filepath.Join("testdata", fixture), dir)
	return dir
}

const modPath = "github.com/ai-pipes/mcp-registry/internal/"

// archFixtures maps each rule enforced by go-arch-lint to its fixture and the single import the
// fixture violates. go-arch-lint names components and import paths, not our rule names, so the
// import is what proves the fixture fails for the intended reason.
var archFixtures = []struct{ rule, fixture, component, badImport string }{
	{"domain-is-pure", "domain-is-pure", "catalog-domain", modPath + "modules/identity/domain"},
	{"application-only-domain", "application-only-domain", "catalog-app", modPath + "modules/search/app"},
	{"no-inward-infra", "no-inward-infra", "catalog-app", modPath + "modules/catalog/http"},
	{"deps-only-in-infra (vendors)", "deps-only-in-infra", "catalog-app", "github.com/jackc/pgx/v5"},
	{"modules-via-index", "modules-via-index", "gateway-infra", modPath + "modules/catalog/domain"},
	{"migrations-only-platform", "migrations-only-platform", "catalog-infra", "github.com/ai-pipes/mcp-registry/apps/server/migrations"},
	// platform-leaves: modules reach platform only through the leaves, and only from http and infra.
	{"platform-leaves (app imports a leaf)", "app-imports-leaf", "catalog-app", modPath + "platform/logging"},
	{"platform-leaves (domain imports a leaf)", "domain-imports-leaf", "catalog-domain", modPath + "platform/db"},
	{"platform-leaves (a leaf imports a module)", "leaf-imports-module", "platform-httpx", modPath + "modules/catalog"},
	{"platform-leaves (a leaf imports the composition)", "leaf-imports-composition", "platform-db", modPath + "platform/config"},
	{"platform-leaves (a leaf imports a leaf it does not need)", "leaf-imports-leaf", "platform-db", modPath + "platform/httpx"},
	{"platform-leaves (http imports the composition)", "http-imports-composition", "catalog-http", modPath + "platform/app"},
	{"platform-leaves (http imports a storage leaf)", "http-imports-db", "catalog-http", modPath + "platform/db"},
	{"platform-leaves (infra imports the composition)", "infra-imports-composition", "catalog-infra", modPath + "platform/crypto"},
	{"platform-leaves (infra imports httpx)", "infra-imports-httpx", "catalog-infra", modPath + "platform/httpx"},
	{"platform-leaves (module root imports a leaf other than httpx)", "root-imports-leaf-other-than-httpx", "catalog-root", modPath + "platform/db"},
	{"no-inward-infra (module root imports infra)", "root-imports-infra", "catalog-root", modPath + "modules/catalog/infra"},
	// layer-self-dependency: a layer may import itself, nothing else follows from it.
	{"layer-self-dependency (domain still may not import its own app)", "domain-imports-own-app", "catalog-domain", modPath + "modules/catalog/app"},
	{"layer-self-dependency (an external test package is still checked)", "external-test-imports-app", "catalog-domain", modPath + "modules/catalog/app"},
}

func TestEveryRuleHasAFailingFixture(t *testing.T) {
	for _, tc := range archFixtures {
		t.Run(tc.rule, func(t *testing.T) {
			rep, code := archLint(t, fixtureProject(t, tc.fixture))
			if code == 0 {
				t.Fatalf("fixture %s passed; a rule without a failing fixture does not count", tc.fixture)
			}
			deps := rep.Payload.ArchWarningsDeps
			if len(deps) != 1 || deps[0].ResolvedImportName != tc.badImport || deps[0].ComponentName != tc.component {
				t.Fatalf("want exactly one violation, %s importing %s; got %+v", tc.component, tc.badImport, deps)
			}
			if n := len(rep.Payload.ArchWarningsNotMatched) + len(rep.Payload.ArchWarningsDeepScan) + len(rep.Payload.ExecutionWarnings); n != 0 {
				t.Fatalf("fixture %s raises %d unrelated warnings: %+v", tc.fixture, n, rep.Payload)
			}
		})
	}
}

// TestInjectingAnAdapterIntoAPortIsAllowed pins allow.deepScan: false in .go-arch-lint.yml. The
// deep scan reads a concrete argument passed to an interface parameter as a dependency of the
// callee's package, which forbids dependency inversion itself. The fixture does exactly that
// (platform -> app.Use(infra adapter); app -> kernel function taking error with a domain error)
// and must raise nothing.
func TestInjectingAnAdapterIntoAPortIsAllowed(t *testing.T) {
	rep, code := archLint(t, fixtureProject(t, "adapter-into-port"))
	if code != 0 {
		t.Fatalf("injecting an adapter into a port was reported (is allow.deepScan back on?): %+v", rep.Payload)
	}
	if n := len(rep.Payload.ArchWarningsDeps) + len(rep.Payload.ArchWarningsNotMatched) + len(rep.Payload.ArchWarningsDeepScan) + len(rep.Payload.ExecutionWarnings); n != 0 {
		t.Fatalf("unexpected warnings: %+v", rep.Payload)
	}
}

// TestLeavesAndSelfImportsAreAllowed is the positive side of platform-leaves and of
// layer-self-dependency: http imports httpx and logging, infra imports db, outbox and logging, the
// module root imports httpx, a leaf imports kernel and the leaf it needs, the composition imports
// anything, and every layer's external test package (package domain_test) imports its own layer.
// It must raise nothing.
func TestLeavesAndSelfImportsAreAllowed(t *testing.T) {
	rep, code := archLint(t, fixtureProject(t, "leaves-allowed"))
	if code != 0 {
		t.Fatalf("an allowed import was reported: %+v", rep.Payload)
	}
	if n := len(rep.Payload.ArchWarningsDeps) + len(rep.Payload.ArchWarningsNotMatched) + len(rep.Payload.ArchWarningsDeepScan) + len(rep.Payload.ExecutionWarnings); n != 0 {
		t.Fatalf("unexpected warnings: %+v", rep.Payload)
	}
}

func TestRepositoryPassesArchLint(t *testing.T) {
	rep, code := archLint(t, repoRoot)
	if code != 0 {
		t.Fatalf("the repository violates its architecture rules: %+v", rep.Payload)
	}
}

// TestDepguardDeniesStdlibCrypto proves the crypto/* half of deps-only-in-infra, which
// go-arch-lint cannot see (it ignores the standard library), through the repository's
// real .golangci.yml.
func TestDepguardDeniesStdlibCrypto(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), readFile(t, filepath.Join(repoRoot, "go.mod")))
	writeFile(t, filepath.Join(dir, ".golangci.yml"), readFile(t, filepath.Join(repoRoot, ".golangci.yml")))
	copyTree(t, filepath.Join("testdata", "deps-only-in-infra-stdlib"), dir)

	cmd := exec.Command("go", "run", pinnedTool(t, "print-golangci-lint"), "run", "--enable-only", "depguard",
		"--output.json.path=stdout", "--output.text.path=", "--show-stats=false")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("depguard accepted the fixture (err=%v); a rule without a failing fixture does not count\n%s", err, stdout.String())
	}
	var rep struct {
		Issues []struct {
			FromLinter, Text string
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("unexpected output (%v):\n%s\n%s", err, stdout.String(), stderr.String())
	}
	if len(rep.Issues) != 1 || rep.Issues[0].FromLinter != "depguard" || !strings.Contains(rep.Issues[0].Text, "'crypto/sha256'") {
		t.Fatalf("want exactly one depguard issue about crypto/sha256, got %+v", rep.Issues)
	}
}
