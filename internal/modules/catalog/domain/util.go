package domain

import (
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// utf16Len is JavaScript's String.length. Only the TypeScript DOMAIN counts UTF-16 code units (name,
// description, secret); zod, which sets the limits of the request fields, counts code points.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// asciiLower lower-cases ASCII letters only. JavaScript's toLowerCase maps some non-ASCII
// characters in ways Go's unicode tables do not (U+0130), so comparisons with ASCII words
// must not use strings.ToLower.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func trim(s string) string { return kernel.JSTrim(s) }

func hasAny(s, chars string) bool { return strings.ContainsAny(s, chars) }

// Opt is an optional value in a change set: the zero value means "not given".
type Opt[T any] struct {
	Value T
	Set   bool
}

// Some marks v as given.
func Some[T any](v T) Opt[T] { return Opt[T]{Value: v, Set: true} }

func (o Opt[T]) or(current T) T {
	if o.Set {
		return o.Value
	}
	return current
}
