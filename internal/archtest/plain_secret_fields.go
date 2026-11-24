package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// FieldViolation is a struct field (or a named type) whose name says it carries a secret and whose
// type prints it.
type FieldViolation struct {
	// Key is "<directory relative to the scanned root>.<Type>.<Field>" ("<dir>.<Type>" for a named
	// type), the allowlist key.
	Key  string
	File string
	Line int
	Type string // the field's type as written
}

func (v FieldViolation) String() string {
	return fmt.Sprintf("%s:%d: %s has type %s; hold it as kernel.Secret / kernel.SecretBytes", v.File, v.Line, v.Key, v.Type)
}

// secretWords end a field name that carries a secret: AccessToken, ClientSecret, CodeVerifier,
// SealedSecret, Nonce, Password... A name is split into its CamelCase (or snake_case) words and
// only the LAST word counts, so TokenURL, TokenID, CodeHash and SecretSet are not secrets.
var secretWords = map[string]bool{
	"secret": true, "secrets": true, "token": true, "tokens": true, "password": true, "passwd": true,
	"apikey": true, "verifier": true, "nonce": true, "cookie": true, "sealed": true, "plain": true,
	"credential": true, "credentials": true, "code": true, "state": true, "access": true, "refresh": true,
	// An API or encryption key, an Authorization header value, a sign-in link (its URL carries the
	// code). Header (a header NAME in every field of the tree today), URL alone and keys (the JSON
	// object keys of two parsers) are not words.
	"key": true, "authorization": true, "link": true,
}

// printableTypes are the field types fmt, slog and encoding/json write raw.
var printableTypes = map[string]bool{"string": true, "*string": true, "[]byte": true, "*[]byte": true, "[]string": true}

// PlainSecretFields reports every field of a struct type declared in a non-test Go file below root
// (testdata excluded) whose name ends with a secret word (see secretWords; SessionID and APIKey
// too) and whose type is string, *string, []byte, *[]byte or []string, and every named type with
// such a name defined as one of those types (type Sealed []byte), unless allow has its key (a
// reason per entry). Anonymous struct types inside functions count as well. Purely syntactic: a
// field of a named type that happens to be a string is not seen (the named type is, by its name).
func PlainSecretFields(root string, allow map[string]string) ([]FieldViolation, error) {
	var out []FieldViolation
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		vs, err := plainSecretFieldsOf(path, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		for _, v := range vs {
			if _, ok := allow[v.Key]; !ok {
				out = append(out, v)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func plainSecretFieldsOf(path, dir string) ([]FieldViolation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var out []FieldViolation
	check := func(typeName string, st *ast.StructType) {
		for _, f := range st.Fields.List {
			typ := typeText(f.Type)
			if !printableTypes[typ] {
				continue
			}
			for _, n := range f.Names {
				if secretName(n.Name) {
					out = append(out, FieldViolation{
						Key: dir + "." + typeName + "." + n.Name, File: path,
						Line: fset.Position(n.Pos()).Line, Type: typ,
					})
				}
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.TypeSpec:
			if st, ok := x.Type.(*ast.StructType); ok {
				check(x.Name.Name, st)
				return false
			}
			if typ := typeText(x.Type); printableTypes[typ] && x.Assign == 0 && secretName(x.Name.Name) {
				out = append(out, FieldViolation{
					Key: dir + "." + x.Name.Name, File: path, Line: fset.Position(x.Name.Pos()).Line, Type: typ,
				})
			}
		case *ast.StructType:
			check("<anonymous>", x)
		}
		return true
	})
	return out, nil
}

func typeText(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + typeText(t.X)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + typeText(t.Elt)
		}
	}
	return ""
}

// words splits a Go identifier into lower-case words: CodeVerifier -> code, verifier;
// SessionID -> session, id; APIKey -> api, key; access_token -> access, token.
func words(name string) []string {
	var out []string
	var cur []rune
	rs := []rune(name)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	for i, r := range rs {
		switch {
		case r == '_':
			flush()
			continue
		case unicode.IsUpper(r) && len(cur) > 0:
			prevLower := unicode.IsLower(cur[len(cur)-1]) || unicode.IsDigit(cur[len(cur)-1])
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if prevLower || nextLower {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

func secretName(name string) bool {
	w := words(name)
	if len(w) == 0 {
		return false
	}
	last := w[len(w)-1]
	if secretWords[last] {
		return true
	}
	if len(w) >= 2 {
		pair := w[len(w)-2] + last
		return pair == "sessionid" || pair == "linkurl"
	}
	return false
}
