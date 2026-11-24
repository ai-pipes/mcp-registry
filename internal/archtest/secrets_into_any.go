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
// Generated files (a "Code generated ... DO NOT EDIT." header) are scanned like any other: they
// leak as well as hand-written code.
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
	return holds(t, seen, isSecretType)
}

func isSecretType(t types.Type) bool {
	n, ok := t.(*types.Named)
	if !ok {
		return false
	}
	o := n.Obj()
	return o.Pkg() != nil && o.Pkg().Path() == kernelPkg && (o.Name() == "Secret" || o.Name() == "SecretBytes")
}

func isTypeParam(t types.Type) bool {
	_, ok := t.(*types.TypeParam)
	return ok
}

// holds walks t (pointers, slices, arrays, maps, channels, struct fields, function parameters and
// results, named wrappers) and reports whether match is true for t or anything inside it.
func holds(t types.Type, seen map[types.Type]bool, match func(types.Type) bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	if match(t) {
		return true
	}
	switch x := t.(type) {
	case *types.Named:
		return holds(x.Underlying(), seen, match)
	case *types.Alias:
		return holds(types.Unalias(x), seen, match)
	case *types.Pointer:
		return holds(x.Elem(), seen, match)
	case *types.Slice:
		return holds(x.Elem(), seen, match)
	case *types.Array:
		return holds(x.Elem(), seen, match)
	case *types.Map:
		return holds(x.Key(), seen, match) || holds(x.Elem(), seen, match)
	case *types.Chan:
		return holds(x.Elem(), seen, match)
	case *types.Struct:
		for i := range x.NumFields() {
			if holds(x.Field(i).Type(), seen, match) {
				return true
			}
		}
	case *types.Signature: // a function value whose parameters or results hold the type
		return holdsTuple(x.Params(), seen, match) || holdsTuple(x.Results(), seen, match)
	}
	return false
}

func holdsTuple(t *types.Tuple, seen map[types.Type]bool, match func(types.Type) bool) bool {
	for i := range t.Len() {
		if holds(t.At(i).Type(), seen, match) {
			return true
		}
	}
	return false
}

// boundToEmptyParam reports whether a parameter of type t of a generic function (or of a method of
// a generic type) accepts, through a type parameter whose constraint has no methods, a value of any
// type the type argument allows: the body can format or store it as any. Constraints with methods
// are ports. Conservative on purpose: a constraint's type set is not looked at.
func boundToEmptyParam(t types.Type) bool {
	switch x := types.Unalias(t).(type) {
	case *types.TypeParam:
		i, ok := x.Constraint().Underlying().(*types.Interface)
		return ok && i.NumMethods() == 0
	case *types.Slice:
		return boundToEmptyParam(x.Elem())
	case *types.Array:
		return boundToEmptyParam(x.Elem())
	case *types.Pointer:
		return boundToEmptyParam(x.Elem())
	case *types.Chan:
		return boundToEmptyParam(x.Elem())
	case *types.Map:
		return boundToEmptyParam(x.Key()) || boundToEmptyParam(x.Elem())
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
	decls := map[*types.Func]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fn, ok := info.Defs[fd.Name].(*types.Func); ok {
					decls[fn] = fd
				}
			}
		}
	}
	memo := map[*types.Func]bool{}
	// bodySinks reports whether a generic function declared in these files lets a value of its
	// type parameters reach an empty interface (fmt, slog, any, or another such generic). One
	// declared elsewhere (another package, the standard library) is unknown: assumed to.
	var bodySinks func(fn *types.Func) bool
	bodySinks = func(fn *types.Func) bool {
		if v, ok := memo[fn]; ok {
			return v
		}
		fd := decls[fn]
		if fd == nil {
			return true
		}
		memo[fn] = false // recursion: judged by the rest of the body
		v := len(scanNodes(fset, []ast.Node{fd}, info, isTypeParam, bodySinks)) > 0
		memo[fn] = v
		return v
	}
	nodes := make([]ast.Node, len(files))
	for i, f := range files {
		nodes[i] = f
	}
	return scanNodes(fset, nodes, info, isSecretType, bodySinks)
}

// scanNodes reports where a value whose type holds a type matched by match reaches an empty
// interface. sinks decides, for a generic callee, whether a value bound to its type parameter can.
func scanNodes(fset *token.FileSet, files []ast.Node, info *types.Info, match func(types.Type) bool, sinks func(*types.Func) bool) []SecretLeak {
	var out []SecretLeak
	reportType := func(e ast.Expr, typ, target types.Type, what string) {
		if e == nil || typ == nil || !emptyInterface(target) || emptyInterface(typ) || !holds(typ, map[types.Type]bool{}, match) {
			return
		}
		out = append(out, SecretLeak{Pos: fset.Position(e.Pos()), Type: typ.String(), What: what})
	}
	report := func(e ast.Expr, target types.Type, what string) {
		if e == nil {
			return
		}
		if tv, ok := info.Types[e]; ok {
			reportType(e, tv.Type, target, what)
		}
	}
	// reportTuple handles `x, y = f()`, `var x, y T = f()` and `return f()`: the call's results are
	// matched with the targets one by one.
	reportTuple := func(call ast.Expr, targets []types.Type, what string) {
		tup, ok := info.TypeOf(call).(*types.Tuple)
		if !ok || tup.Len() != len(targets) {
			return
		}
		for i := range targets {
			reportType(call, tup.At(i).Type(), targets[i], what)
		}
	}
	anyType := types.Universe.Lookup("any").Type()
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
				callArgs(info, x, report, anyType, sinks)
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) && x.Tok != token.DEFINE {
					for i := range x.Lhs {
						report(x.Rhs[i], info.TypeOf(x.Lhs[i]), "assignment")
					}
				} else if len(x.Rhs) == 1 && len(x.Lhs) > 1 && x.Tok != token.DEFINE {
					targets := make([]types.Type, len(x.Lhs))
					for i, l := range x.Lhs {
						targets[i] = info.TypeOf(l)
					}
					reportTuple(x.Rhs[0], targets, "assignment")
				}
			case *ast.ValueSpec:
				if x.Type != nil && len(x.Values) == len(x.Names) {
					for i := range x.Values {
						report(x.Values[i], info.TypeOf(x.Type), "variable declaration")
					}
				} else if x.Type != nil && len(x.Values) == 1 && len(x.Names) > 1 {
					targets := make([]types.Type, len(x.Names))
					for i := range targets {
						targets[i] = info.TypeOf(x.Type)
					}
					reportTuple(x.Values[0], targets, "variable declaration")
				}
			case *ast.ReturnStmt:
				if len(funcs) > 0 && funcs[len(funcs)-1] != nil {
					res := funcs[len(funcs)-1].Results()
					if res.Len() == len(x.Results) {
						for i, r := range x.Results {
							report(r, res.At(i).Type(), "return")
						}
					} else if len(x.Results) == 1 && res.Len() > 1 {
						targets := make([]types.Type, res.Len())
						for i := range targets {
							targets[i] = res.At(i).Type()
						}
						reportTuple(x.Results[0], targets, "return")
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

func callArgs(info *types.Info, call *ast.CallExpr, report func(ast.Expr, types.Type, string), anyType types.Type, sinks func(*types.Func) bool) {
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
	// The generic signature of the callee, if it has one: after instantiation its type parameters
	// are replaced and an argument bound to `T any` no longer looks like an empty interface.
	fn, origin := genericCallee(info, call.Fun)
	for i, a := range call.Args {
		report(a, paramType(sig, i, call.Ellipsis.IsValid()), "argument of "+name)
		if origin != nil && sinks(fn) {
			if pt := paramType(origin, i, call.Ellipsis.IsValid()); pt != nil && boundToEmptyParam(pt) {
				report(a, anyType, "argument of "+name+" (type parameter)")
			}
		}
	}
}

// paramType is the type the i-th argument is passed as (the element type for a variadic tail).
func paramType(sig *types.Signature, i int, ellipsis bool) types.Type {
	params := sig.Params()
	switch {
	case sig.Variadic() && i >= params.Len()-1:
		last := params.At(params.Len() - 1).Type()
		if ellipsis {
			return last
		}
		if s, ok := last.Underlying().(*types.Slice); ok {
			return s.Elem()
		}
	case i < params.Len():
		return params.At(i).Type()
	}
	return nil
}

// genericCallee returns the uninstantiated function and signature of the function or method called through
// fun when it is generic or belongs to a generic type, else nil.
func genericCallee(info *types.Info, fun ast.Expr) (*types.Func, *types.Signature) {
	for {
		switch x := fun.(type) {
		case *ast.ParenExpr:
			fun = x.X
			continue
		case *ast.IndexExpr:
			fun = x.X
			continue
		case *ast.IndexListExpr:
			fun = x.X
			continue
		}
		break
	}
	var id *ast.Ident
	switch x := fun.(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	default:
		return nil, nil
	}
	f, ok := info.Uses[id].(*types.Func)
	if !ok {
		return nil, nil
	}
	f = f.Origin()
	sig, _ := f.Type().(*types.Signature)
	if sig == nil || (sig.TypeParams().Len() == 0 && sig.RecvTypeParams().Len() == 0) {
		return nil, nil
	}
	return f, sig
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
