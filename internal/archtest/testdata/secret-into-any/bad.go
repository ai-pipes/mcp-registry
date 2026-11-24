package fixture

import (
	"fmt"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

type holder struct{ token kernel.Secret }

func exec(sql string, args ...any) {}

// Each line marked "reported" hands a secret (or a value holding one) to an empty interface.
func leaks(s kernel.Secret, b kernel.SecretBytes) any {
	_ = fmt.Sprintf("%s", s)                     // reported: variadic ...any argument
	exec("insert into t values ($1)", holder{s}) // reported: a struct holding a secret
	var v any = b                                // reported: variable declaration
	_ = v
	_ = []any{s}                // reported: element
	_ = map[string]any{"k": &s} // reported: map value, a pointer to a secret
	return s                    // reported: return
}

func show[T any](v T) { fmt.Println(v) }

func pair() (kernel.Secret, error) { return kernel.Secret{}, nil }

func moreLeaks(s kernel.Secret, f func() kernel.Secret) (any, error) {
	show(s)                // reported: a secret bound to an unconstrained type parameter
	show[kernel.Secret](s) // reported: the same, instantiated explicitly
	exec("x", f)           // reported: a function value returning a secret
	var a, b any = pair()  // reported: a multi-value call into any variables
	a, b = pair()          // reported: the same by assignment
	_, _ = a, b
	return pair() // reported: the same by return
}
