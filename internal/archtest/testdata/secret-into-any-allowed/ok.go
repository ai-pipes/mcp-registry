package fixture

import (
	"fmt"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

func exec(sql string, args ...any) {}

func describe(s fmt.Stringer) string { return s.String() }

// Revealed values, and a secret passed to an interface WITH methods, are fine.
func fine(s kernel.Secret, b kernel.SecretBytes) any {
	_ = fmt.Sprintf("%s", s.Reveal())
	exec("insert into t values ($1, $2)", s.Reveal(), b.Reveal())
	_ = describe(s)
	_ = []any{s.IsSet()}
	return s.Reveal()
}
