package archtest

import (
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// kernelPkg is the package of the secret types.
const kernelPkg = "github.com/ai-pipes/mcp-registry/internal/kernel"

// SecretLeak is an expression whose type holds a kernel.Secret or kernel.SecretBytes (directly or
// anywhere inside it) converted to an interface with no methods: fmt, slog, pgx and encoding/json
// take such values and print or store their redaction marker instead of the value, silently.
type SecretLeak struct {
	Pos  token.Position
	Type string // the expression's type
	What string // the place: "argument of fmt.Sprintf", "assignment", "return", "element", ...
}

func (l SecretLeak) String() string {
	return fmt.Sprintf("%s: a value of type %s reaches an empty interface (%s); call Reveal() where the value is meant to go, or pass something that is not a secret",
		l.Pos, l.Type, l.What)
}

// SecretsIntoEmptyInterfaces type-checks the non-test packages matching patterns (run from dir,
// the module root) once per build tag set and reports every SecretLeak. Passing a secret to an
// interface WITH methods (a port, a service) is not reported. Leaks found under several tag sets
// are reported once.
//
// The loader is golang.org/x/tools/go/packages: it asks the go command for the packages and their
// dependencies' export data, so third-party and cgo-free dependencies type-check exactly as the
// build sees them, in seconds.
func SecretsIntoEmptyInterfaces(dir string, tagSets [][]string, patterns ...string) ([]SecretLeak, error) {
	seen := map[string]SecretLeak{}
	for _, tags := range tagSets {
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes |
				packages.NeedTypesInfo | packages.NeedImports,
			Dir: dir,
		}
		if len(tags) > 0 {
			cfg.BuildFlags = []string{"-tags=" + strings.Join(tags, ",")}
		}
		pkgs, err := packages.Load(cfg, patterns...)
		if err != nil {
			return nil, fmt.Errorf("load %v (tags %v): %w", patterns, tags, err)
		}
		var loadErrs []string
		packages.Visit(pkgs, nil, func(p *packages.Package) {
			for _, e := range p.Errors {
				loadErrs = append(loadErrs, e.Error())
			}
		})
		if len(loadErrs) > 0 {
			return nil, fmt.Errorf("type errors (tags %v): %s", tags, strings.Join(loadErrs, "; "))
		}
		for _, p := range pkgs {
			for _, l := range secretsIntoAny(p.Fset, p.Syntax, p.TypesInfo) {
				seen[l.Pos.String()] = l
			}
		}
	}
	out := make([]SecretLeak, 0, len(seen))
	for _, l := range seen {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pos.String() < out[j].Pos.String() })
	return out, nil
}

// SecretsIntoEmptyInterfacesInFiles is the same check for the .go files of one directory that is
// not a package of the module (a fixture under testdata). Its imports are resolved from the
// standard library's export data and from the module's packages, loaded once.
func SecretsIntoEmptyInterfacesInFiles(moduleDir, fixtureDir string) ([]SecretLeak, error) {
	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	imports := map[string]bool{}
	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		for _, s := range f.Imports {
			imports[strings.Trim(s.Path.Value, `"`)] = true
		}
	}
	var mod []string
	for imp := range imports {
		if strings.HasPrefix(imp, "github.com/ai-pipes/mcp-registry/") {
			mod = append(mod, imp)
		}
	}
	byPath := map[string]*types.Package{}
	if len(mod) > 0 {
		pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedTypes, Dir: moduleDir}, mod...)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			if len(p.Errors) > 0 {
				return nil, errors.New(p.Errors[0].Error())
			}
			byPath[p.PkgPath] = p.Types
		}
	}
	std := importer.Default()
	conf := types.Config{Importer: importerFunc(func(path string) (*types.Package, error) {
		if p, ok := byPath[path]; ok {
			return p, nil
		}
		return std.Import(path)
	})}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	if _, err := conf.Check("fixture", fset, files, info); err != nil {
		return nil, err
	}
	return secretsIntoAny(fset, files, info), nil
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// holdsSecret reports whether t is, or holds anywhere inside it (struct fields, pointers, slices,
// arrays, maps, channels, named wrappers), kernel.Secret or kernel.SecretBytes.
func holdsSecret(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	switch x := t.(type) {
	case *types.Named:
		if o := x.Obj(); o.Pkg() != nil && o.Pkg().Path() == kernelPkg && (o.Name() == "Secret" || o.Name() == "SecretBytes") {
			return true
		}
		return holdsSecret(x.Underlying(), seen)
	case *types.Alias:
		return holdsSecret(types.Unalias(x), seen)
	case *types.Pointer:
		return holdsSecret(x.Elem(), seen)
	case *types.Slice:
		return holdsSecret(x.Elem(), seen)
	case *types.Array:
		return holdsSecret(x.Elem(), seen)
	case *types.Map:
		return holdsSecret(x.Key(), seen) || holdsSecret(x.Elem(), seen)
	case *types.Chan:
		return holdsSecret(x.Elem(), seen)
	case *types.Struct:
		for i := range x.NumFields() {
			if holdsSecret(x.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// emptyInterface reports whether t is an interface with no methods (any, interface{}), not a type
// parameter.
func emptyInterface(t types.Type) bool {
	if t == nil {
		return false
	}
	if _, isParam := types.Unalias(t).(*types.TypeParam); isParam {
		return false
	}
	i, ok := t.Underlying().(*types.Interface)
	return ok && i.NumMethods() == 0
}

// secretsIntoAny walks the files and reports each place where a secret-holding expression is
// converted to an empty interface: call arguments (variadic ...any included, builtins such as
// append too), explicit conversions any(x), assignments and variable declarations, returns,
// composite literal elements (slices, arrays, maps, struct fields) and channel sends.
func secretsIntoAny(fset *token.FileSet, files []*ast.File, info *types.Info) []SecretLeak {
	var out []SecretLeak
	report := func(e ast.Expr, target types.Type, what string) {
		if e == nil || !emptyInterface(target) {
			return
		}
		tv, ok := info.Types[e]
		if !ok || tv.Type == nil || emptyInterface(tv.Type) || !holdsSecret(tv.Type, map[types.Type]bool{}) {
			return
		}
		out = append(out, SecretLeak{Pos: fset.Position(e.Pos()), Type: tv.Type.String(), What: what})
	}
	for _, f := range files {
		var funcs []*types.Signature // enclosing function signatures, for returns
		var visit func(n ast.Node) bool
		visit = func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				var sig *types.Signature
				if o, ok := info.Defs[x.Name]; ok && o != nil {
					sig, _ = o.Type().(*types.Signature)
				}
				funcs = append(funcs, sig)
				if x.Body != nil {
					ast.Inspect(x.Body, visit)
				}
				funcs = funcs[:len(funcs)-1]
				return false
			case *ast.FuncLit:
				sig, _ := info.Types[x].Type.(*types.Signature)
				funcs = append(funcs, sig)
				ast.Inspect(x.Body, visit)
				funcs = funcs[:len(funcs)-1]
				return false
			case *ast.CallExpr:
				callArgs(info, x, report)
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) && x.Tok != token.DEFINE {
					for i := range x.Lhs {
						report(x.Rhs[i], info.TypeOf(x.Lhs[i]), "assignment")
					}
				}
			case *ast.ValueSpec:
				if x.Type != nil && len(x.Values) == len(x.Names) {
					for i := range x.Values {
						report(x.Values[i], info.TypeOf(x.Type), "variable declaration")
					}
				}
			case *ast.ReturnStmt:
				if len(funcs) > 0 && funcs[len(funcs)-1] != nil {
					res := funcs[len(funcs)-1].Results()
					if res.Len() == len(x.Results) {
						for i, r := range x.Results {
							report(r, res.At(i).Type(), "return")
						}
					}
				}
			case *ast.SendStmt:
				if ch, ok := info.TypeOf(x.Chan).Underlying().(*types.Chan); ok {
					report(x.Value, ch.Elem(), "channel send")
				}
			case *ast.CompositeLit:
				compositeElems(info, x, report)
			}
			return true
		}
		ast.Inspect(f, visit)
	}
	return out
}

func callArgs(info *types.Info, call *ast.CallExpr, report func(ast.Expr, types.Type, string)) {
	tv, ok := info.Types[call.Fun]
	if !ok {
		return
	}
	if tv.IsType() { // a conversion T(x)
		if len(call.Args) == 1 {
			report(call.Args[0], tv.Type, "conversion")
		}
		return
	}
	sig, ok := tv.Type.Underlying().(*types.Signature)
	if !ok {
		return
	}
	name := calleeName(call.Fun)
	params := sig.Params()
	for i, a := range call.Args {
		var pt types.Type
		switch {
		case sig.Variadic() && i >= params.Len()-1:
			last := params.At(params.Len() - 1).Type()
			if call.Ellipsis.IsValid() {
				pt = last
			} else if s, ok := last.Underlying().(*types.Slice); ok {
				pt = s.Elem()
			}
		case i < params.Len():
			pt = params.At(i).Type()
		}
		report(a, pt, "argument of "+name)
	}
}

func calleeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return calleeName(x.X) + "." + x.Sel.Name
	case *ast.IndexExpr:
		return calleeName(x.X)
	}
	return "a call"
}

func compositeElems(info *types.Info, lit *ast.CompositeLit, report func(ast.Expr, types.Type, string)) {
	t := info.TypeOf(lit)
	if t == nil {
		return
	}
	switch u := t.Underlying().(type) {
	case *types.Slice:
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				e = kv.Value
			}
			report(e, u.Elem(), "element")
		}
	case *types.Array:
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				e = kv.Value
			}
			report(e, u.Elem(), "element")
		}
	case *types.Map:
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				report(kv.Key, u.Key(), "map key")
				report(kv.Value, u.Elem(), "map value")
			}
		}
	case *types.Struct:
		for i, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok {
					for j := range u.NumFields() {
						if u.Field(j).Name() == id.Name {
							report(kv.Value, u.Field(j).Type(), "field "+id.Name)
						}
					}
				}
				continue
			}
			if i < u.NumFields() {
				report(e, u.Field(i).Type(), "field "+u.Field(i).Name())
			}
		}
	}
}
