package infra

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"

	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
)

// pkce is app.Pkce on crypto/rand and SHA-256 (nodePkce of the TypeScript server): state and code
// verifier are 32 random bytes as 43 unpadded base64url characters (the RFC 7636 minimum length),
// the challenge is BASE64URL(SHA-256(verifier)) (S256).
type pkce struct{ random io.Reader }

var _ app.Pkce = (*pkce)(nil)

// NewPkce is the production Pkce.
func NewPkce() app.Pkce { return &pkce{random: rand.Reader} }

var errRandom = errors.New("the random source failed")

// RandomToken fails (it never panics) when the random source does; the text is fixed.
func (p *pkce) RandomToken() (string, error) {
	var b [32]byte
	if _, err := io.ReadFull(p.random, b[:]); err != nil {
		return "", errRandom
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Challenge is the S256 challenge of the verifier.
func (p *pkce) Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
