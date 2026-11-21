package crypto_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
)

type vectors struct {
	Secretbox struct {
		Key         string `json:"key"`
		PreviousKey string `json:"previousKey"`
		Vectors     []struct {
			Plain  string `json:"plain"`
			Sealed string `json:"sealed"`
		} `json:"vectors"`
		SealedWithPrevious struct {
			Plain  string `json:"plain"`
			Sealed string `json:"sealed"`
		} `json:"sealedWithPrevious"`
	} `json:"secretbox"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/ts-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func key(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestOpensTypeScriptVectors(t *testing.T) {
	v := loadVectors(t).Secretbox
	if len(v.Vectors) < 5 {
		t.Fatalf("expected at least 5 vectors, got %d", len(v.Vectors))
	}
	box, err := crypto.NewAESGCMBox(b64(t, v.Key), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Vectors {
		got, err := box.Open(b64(t, c.Sealed))
		if err != nil || got != c.Plain {
			t.Errorf("Open(%q...) = %q, %v", c.Plain[:min(len(c.Plain), 8)], got, err)
		}
	}
}

func TestPreviousKeyOpensTypeScriptVector(t *testing.T) {
	v := loadVectors(t).Secretbox
	box, err := crypto.NewAESGCMBox(key(t), b64(t, v.PreviousKey))
	if err != nil {
		t.Fatal(err)
	}
	got, err := box.Open(b64(t, v.SealedWithPrevious.Sealed))
	if err != nil || got != v.SealedWithPrevious.Plain {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRoundTripAndLayout(t *testing.T) {
	box, err := crypto.NewAESGCMBox(key(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"", "s3cret", "Ключ 🔐"} {
		a, err := box.Seal(plain)
		if err != nil {
			t.Fatal(err)
		}
		b, err := box.Seal(plain)
		if err != nil {
			t.Fatal(err)
		}
		if len(a) != 12+16+len(plain) {
			t.Errorf("layout length %d for %q", len(a), plain)
		}
		if bytes.Equal(a, b) {
			t.Error("two seals of one text are identical (iv reused)")
		}
		got, err := box.Open(a)
		if err != nil || got != plain {
			t.Errorf("round trip %q -> %q, %v", plain, got, err)
		}
	}
}

func TestSealsWithCurrentKeyOnly(t *testing.T) {
	oldKey, newKey := key(t), key(t)
	box, err := crypto.NewAESGCMBox(newKey, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal("x")
	if err != nil {
		t.Fatal(err)
	}
	onlyNew, err := crypto.NewAESGCMBox(newKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onlyNew.Open(sealed); err != nil {
		t.Fatalf("new key must open what the box seals: %v", err)
	}
	old, err := crypto.NewAESGCMBox(oldKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldSealed, err := old.Seal("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := box.Open(oldSealed); err != nil || got != "legacy" {
		t.Fatalf("previous key: %q, %v", got, err)
	}
	if _, err := onlyNew.Open(oldSealed); err == nil {
		t.Fatal("a box without previous must not open the old key's values")
	}
}

func TestTruncatedIsAnErrorNotAPanic(t *testing.T) {
	box, err := crypto.NewAESGCMBox(key(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 27} {
		if _, err := box.Open(make([]byte, n)); err == nil {
			t.Errorf("%d bytes must fail", n)
		}
	}
	sealed, err := box.Seal("abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Open(sealed[:len(sealed)-1]); err == nil {
		t.Error("cut ciphertext must fail")
	}
}

func TestTamperedIsRejected(t *testing.T) {
	box, err := crypto.NewAESGCMBox(key(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int{0, 12, 28} { // iv, tag, ciphertext
		sealed, err := box.Seal("abcdef")
		if err != nil {
			t.Fatal(err)
		}
		sealed[idx] ^= 1
		if _, err := box.Open(sealed); err == nil {
			t.Errorf("flipped byte %d was accepted", idx)
		}
	}
}

func TestKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := crypto.NewAESGCMBox(make([]byte, n), nil); err == nil {
			t.Errorf("key of %d bytes accepted", n)
		}
	}
	if _, err := crypto.NewAESGCMBox(key(t), make([]byte, 16)); err == nil {
		t.Error("previous key of 16 bytes accepted")
	}
}
