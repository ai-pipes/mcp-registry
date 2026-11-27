package archtest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// CoverageThreshold is the floor, in percent of statements, for every gated package. It mirrors
// the TypeScript gate (`pnpm test`: 90 for domain and application) and is never lowered.
const CoverageThreshold = 90.0

// GatedModules are the modules whose domain and app packages the gate covers. The list is
// explicit so that a module that loses a layer, or is renamed, fails the gate instead of dropping out.
var GatedModules = []string{"identity", "catalog", "connections", "search", "gateway"}

// GatedLayers are the packages of a module under the gate.
var GatedLayers = []string{"domain", "app"}

// PackageCoverage counts the statements of one package.
type PackageCoverage struct {
	Statements int
	Covered    int
}

// Percent is the covered share of the statements; a package without statements has none.
func (c PackageCoverage) Percent() float64 {
	if c.Statements == 0 {
		return 0
	}
	return 100 * float64(c.Covered) / float64(c.Statements)
}

// ExpectedPackages lists "<module>/<layer>" for every gated package and fails when a directory
// is missing under root/internal/modules.
func ExpectedPackages(root string) ([]string, error) {
	var out []string
	for _, m := range GatedModules {
		for _, l := range GatedLayers {
			dir := filepath.Join(root, "internal", "modules", m, l)
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return nil, fmt.Errorf("gated package directory %s is missing", dir)
			}
			out = append(out, m+"/"+l)
		}
	}
	return out, nil
}

// ParseCoverProfile reads a `go test -coverprofile` file (mode set, count or atomic) and sums the
// statements per package, keyed "<module>/<layer>" for files under internal/modules. A block that
// appears more than once (a merged profile) counts once and is covered if any copy ran.
func ParseCoverProfile(r io.Reader) (map[string]PackageCoverage, error) {
	type block struct {
		stmts   int
		covered bool
	}
	blocks := map[string]*block{} // "file:span" -> block
	pkgOf := map[string]string{}
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			first = false
			if !strings.HasPrefix(line, "mode:") {
				return nil, fmt.Errorf("not a coverage profile: first line %q", line)
			}
			continue
		}
		if line == "" {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("bad profile line %q", line)
		}
		count, err := strconv.Atoi(line[sp+1:])
		if err != nil {
			return nil, fmt.Errorf("bad count in %q", line)
		}
		rest := line[:sp]
		sp = strings.LastIndexByte(rest, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("bad profile line %q", line)
		}
		stmts, err := strconv.Atoi(rest[sp+1:])
		if err != nil {
			return nil, fmt.Errorf("bad statement count in %q", line)
		}
		key := rest[:sp]
		file := key[:strings.LastIndexByte(key, ':')]
		b := blocks[key]
		if b == nil {
			b = &block{stmts: stmts}
			blocks[key] = b
			pkgOf[key] = packageKey(file)
		}
		if count > 0 {
			b.covered = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := map[string]PackageCoverage{}
	for key, b := range blocks {
		p := pkgOf[key]
		if p == "" {
			continue
		}
		c := out[p]
		c.Statements += b.stmts
		if b.covered {
			c.Covered += b.stmts
		}
		out[p] = c
	}
	return out, nil
}

// packageKey maps a profile file path to "<module>/<layer>", or "" for a file outside the modules.
func packageKey(file string) string {
	const marker = "internal/modules/"
	i := strings.Index(file, marker)
	if i < 0 {
		return ""
	}
	parts := strings.Split(file[i+len(marker):], "/")
	if len(parts) < 3 { // module/layer/file.go
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// CheckCoverage judges every expected package against threshold. It returns one report line per
// package and one failure per package that is missing from the profile, has no statements, or is
// under the threshold; the failure names the package and its number.
func CheckCoverage(cov map[string]PackageCoverage, expected []string, threshold float64) (report, failures []string) {
	exp := append([]string(nil), expected...)
	sort.Strings(exp)
	for _, p := range exp {
		c, ok := cov[p]
		switch {
		case !ok || c.Statements == 0:
			report = append(report, fmt.Sprintf("%-22s no statements in the profile", p))
			failures = append(failures, fmt.Sprintf("%s: no statements measured (package missing, empty or not tested)", p))
		case c.Percent() < threshold:
			report = append(report, fmt.Sprintf("%-22s %5.1f%% (%d/%d) FAIL", p, c.Percent(), c.Covered, c.Statements))
			failures = append(failures, fmt.Sprintf("%s: %.1f%% is under %.0f%% (%d of %d statements)", p, c.Percent(), threshold, c.Covered, c.Statements))
		default:
			report = append(report, fmt.Sprintf("%-22s %5.1f%% (%d/%d)", p, c.Percent(), c.Covered, c.Statements))
		}
	}
	return report, failures
}
