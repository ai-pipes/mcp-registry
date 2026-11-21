package infra

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
)

type sha256Hasher struct{}

// NewSHA256Hasher is the SHA-256 implementation of app.Hasher.
func NewSHA256Hasher() app.Hasher { return sha256Hasher{} }

func (sha256Hasher) Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (sha256Hasher) Base64URL(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Equal hashes both sides first, so strings of different lengths compare in constant time too.
func (sha256Hasher) Equal(a, b string) bool {
	x := sha256.Sum256([]byte(a))
	y := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
