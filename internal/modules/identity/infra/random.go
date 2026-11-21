package infra

import (
	"crypto/rand"
	"encoding/base64"
	"errors"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
)

type cryptoRandom struct{}

// NewCryptoRandom is the crypto/rand implementation of app.Random.
func NewCryptoRandom() app.Random { return cryptoRandom{} }

func (cryptoRandom) Base64URL(nBytes int) (string, error) {
	if nBytes < 0 {
		return "", errors.New("random: negative size")
	}
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
