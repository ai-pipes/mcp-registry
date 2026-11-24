package infra

import (
	"crypto/sha256"
	"crypto/subtle"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
)

// ConstantTimeSecrets is app.SecretComparer: the time it takes depends on neither where two
// secrets first differ nor how long they are (both are hashed to a fixed size first).
type ConstantTimeSecrets struct{}

var _ app.SecretComparer = ConstantTimeSecrets{}

// Equal reports whether a and b are the same secret.
func (ConstantTimeSecrets) Equal(a, b kernel.Secret) bool {
	ha, hb := sha256.Sum256([]byte(a.Reveal())), sha256.Sum256([]byte(b.Reveal()))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
