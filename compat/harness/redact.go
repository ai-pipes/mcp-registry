//go:build compat

package harness

// Redact shortens a secret or token to its first ten characters so that it can be logged.
func Redact(s string) string {
	const keep = 10
	if len(s) <= keep {
		return s
	}
	return s[:keep] + "..."
}
