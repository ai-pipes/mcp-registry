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
