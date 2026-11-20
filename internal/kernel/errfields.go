package kernel

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
)

var codeRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// SQLStateError is implemented by errors that carry a Postgres SQLSTATE (pgx's *pgconn.PgError
// has SQLState(); its Code is a field). The kernel does not import pgx, it matches the method.
type SQLStateError interface{ SQLState() string }

// Coder is implemented by errors that carry another short, value-free code (an errno).
type Coder interface{ Code() string }

// ErrorLabel is a one-line, content-free classification of an error for storage: the dynamic
// type name, plus ": <code>" when an error in the tree (wrap chains, errors.Join, multiple %w)
// exposes a short code through SQLState() or Code(). Never the message.
func ErrorLabel(err error) string {
	if err == nil {
		return "nil"
	}
	var sq SQLStateError
	if errors.As(err, &sq) {
		return labelWith(sq, safeCall(sq.SQLState))
	}
	var c Coder
	if errors.As(err, &c) {
		return labelWith(c, safeCall(c.Code))
	}
	return typeName(err)
}

func labelWith(e any, code string) string {
	if codeRE.MatchString(code) {
		return typeNameOf(e) + ": " + code
	}
	return typeNameOf(e)
}

func typeName(err error) string { return typeNameOf(err) }

func typeNameOf(v any) string {
	return strings.TrimPrefix(reflect.TypeOf(v).String(), "*")
}

func safeCall(f func() string) (code string) {
	defer func() {
		if recover() != nil {
			code = ""
		}
	}()
	return f()
}
