package fixture

import "github.com/ai-pipes/mcp-registry/internal/kernel"

// A constraint with methods is a port: the function can only call them.
func name[T interface{ IsSet() bool }](v T) bool { return v.IsSet() }

// A container of values that are not secrets, and a revealed secret in one.
type stack[T any] struct{ items []T }

func (s *stack[T]) push(v T) { s.items = append(s.items, v) }

// A generic function that only stores or returns its argument never formats it as any: a secret
// may be bound to it.
func one[T any](v T, err error) (*T, error) { return &v, err }

func generic(s kernel.Secret) {
	_ = name(s)
	_, _ = one(s, nil)
	var plain stack[string]
	plain.push("a")
	plain.push(s.Reveal())
	var f func() string = s.Reveal
	exec("x", f)
}
