// Package archtest checks the architecture rules that go-arch-lint and depguard cannot express
// (schema ownership, standard-library purity of domain packages) and proves, through failing
// fixtures under testdata, that every rule of CLAUDE.md is really enforced.
package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Violation is one broken architecture rule found by a scanner in this package.
type Violation struct {
	Module string // module directory name, for example "gateway"
	File   string
	Line   int
	Schema string // foreign database schema that was referenced (ForeignSchemaRefs)
	Import string // forbidden import path (ImpureDomainImports)
}

var schemaRef = regexp.MustCompile(`\b(identity|catalog|connections|search|gateway)\.[a-z_]+\b`)

// ForeignSchemaRefs reports string literals in non-test Go files of every module under
// modulesDir that mention a table of another module's database schema (`<schema>.<table>`).
// Literals joined with `+` are folded first, so a reference cannot hide in a concatenation.
func ForeignSchemaRefs(modulesDir string) ([]Violation, error) {
	var out []Violation
	err := eachModuleFile(modulesDir, func(module, path string) error {
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		scan := func(node ast.Node, text string) {
			for _, m := range schemaRef.FindAllStringSubmatch(text, -1) {
				if m[1] == module {
					continue
				}
				out = append(out, Violation{Module: module, File: path, Line: fset.Position(node.Pos()).Line, Schema: m[1]})
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ImportSpec:
				return false
			case *ast.BinaryExpr:
				if text, ok := foldString(x); ok {
					scan(x, text)
					return false
				}
			case *ast.BasicLit:
				if text, ok := foldString(x); ok {
					scan(x, text)
				}
			}
			return true
		})
		return nil
	})
	return out, err
}

// foldString returns the value of a string literal or of a `+` chain of string literals.
func foldString(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return foldString(x.X)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, lok := foldString(x.X)
		r, rok := foldString(x.Y)
		return l + r, lok && rok
	}
	return "", false
}

// eachModuleFile calls fn for every .go file below every direct subdirectory of modulesDir.
func eachModuleFile(modulesDir string, fn func(module, path string) error) error {
	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		return fmt.Errorf("read modules dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		module := e.Name()
		err := filepath.WalkDir(filepath.Join(modulesDir, module), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			return fn(module, path)
		})
		if err != nil {
			return fmt.Errorf("walk module %s: %w", module, err)
		}
	}
	return nil
}
