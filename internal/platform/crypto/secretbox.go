// Package crypto holds the platform's encryption adapters.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

const (
	keyBytes = 32
	ivBytes  = 12
	tagBytes = 16
)

var _ kernel.SecretBox = (*AESGCMBox)(nil)

// AESGCMBox is AES-256-GCM. Sealed layout: iv (12) | auth tag (16) | ciphertext, the same as the
// TypeScript implementation. Go's cipher.AEAD puts the tag after the ciphertext, so the bytes
// are rearranged explicitly. While SECRETS_KEY rotates, previous (SECRETS_KEY_OLD) still opens
// what is not re-sealed yet; sealing always uses the current key.
type AESGCMBox struct {
	current  cipher.AEAD
	previous cipher.AEAD // nil without a previous key
}

// NewAESGCMBox returns a box for key; previous may be nil. Keys must be exactly 32 bytes.
func NewAESGCMBox(key []byte, previous []byte) (*AESGCMBox, error) {
	cur, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	box := &AESGCMBox{current: cur}
	if previous != nil {
		if box.previous, err = newAEAD(previous); err != nil {
			return nil, err
		}
	}
	return box, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != keyBytes {
		return nil, fmt.Errorf("secrets key must be %d bytes", keyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plain with the current key and a fresh random iv.
func (b *AESGCMBox) Seal(plain string) ([]byte, error) {
	iv := make([]byte, ivBytes)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	ct := b.current.Seal(nil, iv, []byte(plain), nil) // ciphertext | tag
	body, tag := ct[:len(ct)-tagBytes], ct[len(ct)-tagBytes:]
	out := make([]byte, 0, ivBytes+tagBytes+len(body))
	out = append(out, iv...)
	out = append(out, tag...)
	return append(out, body...), nil
}

// Open tries the current key, then the previous one. It fails on a value shorter than
// iv + tag, on altered bytes and on a foreign key.
func (b *AESGCMBox) Open(sealed []byte) (string, error) {
	if len(sealed) < ivBytes+tagBytes {
		return "", errors.New("sealed secret is truncated")
	}
	if plain, err := openWith(b.current, sealed); err == nil {
		return plain, nil
	} else if b.previous == nil {
		return "", err
	}
	return openWith(b.previous, sealed)
}

func openWith(aead cipher.AEAD, sealed []byte) (string, error) {
	iv := sealed[:ivBytes]
	tag := sealed[ivBytes : ivBytes+tagBytes]
	body := sealed[ivBytes+tagBytes:]
	ct := make([]byte, 0, len(body)+tagBytes)
	ct = append(ct, body...)
	ct = append(ct, tag...)
	plain, err := aead.Open(nil, iv, ct, nil)
	if err != nil {
		return "", errors.New("secret cannot be opened")
	}
	return string(plain), nil
}
