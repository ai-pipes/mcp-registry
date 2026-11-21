package archtest

import (
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

// impureStd lists standard-library packages (and their subtrees) a domain package must not
// import: network, process and file-system access, and database drivers' entry point.
var impureStd = []string{"net", "os", "database/sql", "io/fs", "io/ioutil", "syscall"}

// pureStd are subtrees of impureStd that are plain value packages.
var pureStd = []string{"net/url", "net/netip"}

func isImpure(path string) bool {
	for _, p := range pureStd {
		if path == p || strings.HasPrefix(path, p+"/") {
			return false
		}
	}
	for _, p := range impureStd {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// ImpureDomainImports reports imports of I/O-capable standard-library packages in any Go file
// below `<modulesDir>/<module>/domain`. It is the standard-library half of the domain-is-pure
// rule; go-arch-lint enforces the layer half.
func ImpureDomainImports(modulesDir string) ([]Violation, error) {
	var out []Violation
	err := eachModuleFile(modulesDir, func(module, path string) error {
		rel, err := filepath.Rel(filepath.Join(modulesDir, module), path)
		if err != nil || !strings.HasPrefix(rel, "domain"+string(filepath.Separator)) {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, spec := range file.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err == nil && isImpure(imp) {
				out = append(out, Violation{Module: module, File: path, Line: fset.Position(spec.Pos()).Line, Import: imp})
			}
		}
		return nil
	})
	return out, err
}
