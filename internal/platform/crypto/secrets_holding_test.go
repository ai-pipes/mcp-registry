package crypto_test

import (
	"bytes"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
)

// The secret box holds its keys only inside cipher.AEAD values: printing the box shows no key
// byte, however it is held.
func TestBoxIsUnprintableHoweverHeld(t *testing.T) {
	cur, prev := bytes.Repeat([]byte{0xA7, 0x5C}, 16), bytes.Repeat([]byte{0x3E, 0x91}, 16)
	box, err := crypto.NewAESGCMBox(cur, prev)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := secrettest.Forbidden(t, cur, prev)
	secrettest.AssertUnprintable(t, "AESGCMBox", box, forbidden)
	secrettest.AssertUnprintable(t, "Keys", crypto.NewKeys(cur, prev), forbidden)
}
