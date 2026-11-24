package upstream

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
)

// An operation keeps the target's credential headers for its requests: printing it (it is held by
// pointer: it carries atomics) shows no header value.
func TestOperationIsUnprintableHoweverHeld(t *testing.T) {
	const value = "Bearer ZZ-upstream-token-01"
	op := &operation{headers: maps(kernel.UpstreamHeaders{"Authorization": kernel.NewSecret(value)})}
	secrettest.AssertUnprintable(t, "operation", op, secrettest.Strings(t, value))
}
