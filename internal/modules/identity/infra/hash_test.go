package infra

import (
	"encoding/base64"
	"testing"
)

func TestSHA256Hasher(t *testing.T) {
	h := NewSHA256Hasher()
	if got := h.Hex(""); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("Hex(empty) = %s", got)
	}
	if got := h.Hex("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("Hex(abc) = %s", got)
	}
	// PKCE S256 example of RFC 7636 appendix B.
	if got := h.Base64URL("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("Base64URL = %s", got)
	}
	// UTF-8 input: printf 'é' | shasum -a 256
	if got := h.Hex("é"); got != "4a99557e4033c3539de2eb65472017cad5f9557f7a0625a09f1c3f6e2ba69c4c" {
		t.Fatalf("Hex(é) = %s", got)
	}
}

func TestHasherEqual(t *testing.T) {
	h := NewSHA256Hasher()
	cases := []struct {
		a, b string
		want bool
	}{
		{"same", "same", true},
		{"same", "other", false},
		{"short", "a much longer string than the other one", false},
		{"", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := h.Equal(c.a, c.b); got != c.want {
			t.Errorf("Equal(%q,%q) = %v", c.a, c.b, got)
		}
	}
}

func TestCryptoRandom(t *testing.T) {
	r := NewCryptoRandom()
	a, err := r.Base64URL(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 43 {
		t.Fatalf("len = %d, want 43", len(a))
	}
	if _, err := base64.RawURLEncoding.DecodeString(a); err != nil {
		t.Fatalf("not unpadded base64url: %v", err)
	}
	b, _ := r.Base64URL(32)
	if a == b {
		t.Fatal("two draws are equal")
	}
	if s, err := r.Base64URL(0); err != nil || s != "" {
		t.Fatalf("zero bytes = %q, %v", s, err)
	}
	if _, err := r.Base64URL(-1); err == nil {
		t.Fatal("negative size must fail")
	}
}
