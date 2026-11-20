package kernel

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
)

var codeRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// Coder is implemented by errors that carry a short, value-free code (a SQLSTATE, an errno).
type Coder interface{ Code() string }

// ErrorLabel is a one-line, content-free classification of an error for storage: the dynamic
// type name, plus ": <code>" when an error in the chain exposes a short code. Never the message.
func ErrorLabel(err error) string {
	if err == nil {
		return "nil"
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		c, ok := e.(Coder)
		if !ok {
			continue
		}
		if code := safeCode(c); codeRE.MatchString(code) {
			return typeName(e) + ": " + code
		}
		return typeName(e)
	}
	return typeName(err)
}

func typeName(err error) string {
	return strings.TrimPrefix(reflect.TypeOf(err).String(), "*")
}

func safeCode(c Coder) (code string) {
	defer func() {
		if recover() != nil {
			code = ""
		}
	}()
	return c.Code()
}
