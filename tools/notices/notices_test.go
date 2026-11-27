package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repo = "../.."

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every Go module the image's binary links is named, at its exact version, in the notices the image
// ships. A dependency update fails here until `go run ./tools/notices go` is run.
func TestEveryLinkedGoModuleIsInTheNotices(t *testing.T) {
	mods, err := linkedModules(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) == 0 {
		t.Fatal("go list found no linked module")
	}
	text := read(t, goNotices)
	for _, m := range mods {
		if !strings.Contains(text, rule+goHeading(m)+"\n") {
			t.Errorf("%s does not name %s: run `go run ./tools/notices go`", goNotices, goHeading(m))
		}
	}
}

// The committed file is exactly what the tool writes: no module too many, no licence text edited.
func TestGoNoticesAreCurrent(t *testing.T) {
	want, err := renderGo(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, goNotices); got != string(want) {
		t.Errorf("%s is stale or edited by hand: run `go run ./tools/notices go`", goNotices)
	}
}

// ONNX Runtime's files are the pinned release's and unchanged since the tool wrote them. What it
// detects: an edit of a file by hand or a partial regeneration (each file against the SHA-256 recorded
// in SOURCE), and a pin of another ONNX Runtime version than the one the image loads (the wheel URL
// against ORT_NPM_URL). What it cannot detect: that the files really came from that wheel; the
// provenance is checked once, at regeneration, where the tool verifies the wheel's SHA-256
// (ORT_NOTICES_WHEEL_SHA256) before it extracts anything. This test needs no network.
func TestONNXRuntimeNoticesMatchThePinsAndTheirHashes(t *testing.T) {
	p, err := readPins(repo)
	if err != nil {
		t.Fatal(err)
	}
	version := versionIn(p["ORT_NPM_URL"])
	if version == "" || !strings.Contains(p["ORT_NOTICES_WHEEL_URL"], "onnxruntime-"+version+"-") {
		t.Fatalf("pins.env: the notices wheel %q is not ONNX Runtime %q", p["ORT_NOTICES_WHEEL_URL"], version)
	}
	sums := map[string]string{}
	for _, m := range ortMembers {
		s := sha256.Sum256([]byte(read(t, ortDir+"/"+m[1])))
		sums[m[1]] = hex.EncodeToString(s[:])
	}
	if got, want := read(t, ortDir+"/SOURCE"), ortSource(version, sums); got != want {
		t.Errorf("%s/SOURCE does not match the pins or the files: run `go run ./tools/notices onnxruntime`\ngot:\n%s\nwant:\n%s", ortDir, got, want)
	}
}

// The Rust notices were made from the pinned tokenizers release.
func TestTokenizersNoticesMatchThePin(t *testing.T) {
	p, err := readPins(repo)
	if err != nil {
		t.Fatal(err)
	}
	tag := "v" + versionIn(p["TOK_BASE_URL"])
	if !strings.Contains(read(t, rustNotices), "\nSource: github.com/daulet/tokenizers "+tag+" (commit ") {
		t.Errorf("%s was not made from %s: run `go run ./tools/notices rust <checkout of %s>`", rustNotices, tag, tag)
	}
	for _, must := range []string{rule + "tokenizers ", rule + "onig_sys ", "oniguruma/COPYING:\n"} {
		if !strings.Contains(read(t, rustNotices), must) {
			t.Errorf("%s lacks %q", rustNotices, must)
		}
	}
}

// Every production dependency apps/web declares is in the web notices at the version it pins (the
// transitive ones change with them and are regenerated together).
func TestWebNoticesNameEveryDirectDependency(t *testing.T) {
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(read(t, "apps/web/package.json")), &pkg); err != nil {
		t.Fatal(err)
	}
	text := read(t, webNotices)
	for name, version := range pkg.Dependencies {
		if strings.HasPrefix(version, "workspace:") {
			continue
		}
		if !strings.Contains(text, rule+name+"@"+version+" - ") {
			t.Errorf("%s does not name %s@%s: run `go run ./tools/notices web`", webNotices, name, version)
		}
	}
}

// The overview the image ships points at every generated file.
func TestTheOverviewNamesEveryNoticesFile(t *testing.T) {
	overview := read(t, "THIRD_PARTY_NOTICES")
	for _, f := range []string{goNotices, rustNotices, webNotices, ortDir + "/LICENSE", ortDir + "/ThirdPartyNotices.txt"} {
		if !strings.Contains(overview, f) {
			t.Errorf("THIRD_PARTY_NOTICES does not name %s", f)
		}
	}
}

func TestNormaliseIsStable(t *testing.T) {
	if got := normalise("a  \r\nb\t\n\n\n"); got != "a\nb\n" {
		t.Errorf("%q", got)
	}
}

func TestLicenceFileNames(t *testing.T) {
	for name, want := range map[string]bool{
		"LICENSE": true, "LICENSE-MIT": true, "LICENSE.txt": true, "license.md": true, "COPYING": true,
		"NOTICE": true, "PATENTS": true, "UNLICENSE": true, "LICENCE": true,
		"README.md": false, "licenses.go": false, "go.mod": false,
	} {
		if licenceFile.MatchString(name) != want {
			t.Errorf("%s: %v", name, !want)
		}
	}
}

// The web notices record a hash of the lockfile entries they were made from (the web client's and
// the contracts' importers, and the package and snapshot entries of every package listed): a
// transitive dependency that moves changes a listed package's snapshot, so the hash changes.
func TestWebNoticesFollowTheLockfile(t *testing.T) {
	text := read(t, webNotices)
	recorded := webInputsRe.FindStringSubmatch(text)
	if recorded == nil {
		t.Fatalf("%s records no lockfile hash: run `go run ./tools/notices web`", webNotices)
	}
	got, err := webLockHash([]byte(read(t, "pnpm-lock.yaml")), webListed(text))
	if err != nil {
		t.Fatalf("%v: run `go run ./tools/notices web`", err)
	}
	if got != recorded[1] {
		t.Errorf("%s was made from another pnpm-lock.yaml (its bundled packages changed): run `pnpm install`, then `go run ./tools/notices web`", webNotices)
	}
}

func TestLockHashSeesATransitiveChange(t *testing.T) {
	lock := "lockfileVersion: '9.0'\n\nimporters:\n\n  apps/web:\n    dependencies:\n      a:\n        version: 1.0.0\n\n  packages/contracts: {}\n\n" +
		"packages:\n\n  a@1.0.0:\n    resolution: {integrity: x}\n\n  b@2.0.0:\n    resolution: {integrity: y}\n\n" +
		"snapshots:\n\n  a@1.0.0:\n    dependencies:\n      b: 2.0.0\n\n  b@2.0.0: {}\n"
	h1, err := webLockHash([]byte(lock), []string{"a@1.0.0", "b@2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.ReplaceAll(lock, "b: 2.0.0", "b: 2.0.1")
	if h2, _ := webLockHash([]byte(moved), []string{"a@1.0.0", "b@2.0.0"}); h2 == h1 {
		t.Error("a moved transitive dependency does not change the hash")
	}
	if _, err := webLockHash([]byte(lock), []string{"c@3.0.0"}); err == nil {
		t.Error("a listed package missing from the lockfile is not an error")
	}
}
