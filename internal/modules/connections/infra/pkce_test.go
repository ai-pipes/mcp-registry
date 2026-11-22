package infra

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestPkceRandomTokenIs43Base64URLCharacters(t *testing.T) {
	p := NewPkce()
	re := regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for range 200 {
		tok, err := p.RandomToken()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(tok) {
			t.Fatalf("token %q has the wrong shape", tok)
		}
		if seen[tok] {
			t.Fatalf("token repeated: %q", tok)
		}
		seen[tok] = true
	}
}

// RFC 7636 appendix B.
func TestPkceChallengeIsS256OfTheVerifier(t *testing.T) {
	p := NewPkce()
	got := p.Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
	if want := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"; got != want {
		t.Errorf("challenge = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "=+/") {
		t.Errorf("challenge %q is not unpadded base64url", got)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy source SECRETMARK is gone")
}

func TestPkceRandomFailureIsAnErrorWithAFixedText(t *testing.T) {
	p := &pkce{random: failingReader{}}
	tok, err := p.RandomToken()
	if err == nil || tok != "" {
		t.Fatalf("token %q, error %v", tok, err)
	}
	if strings.Contains(err.Error(), "SECRETMARK") {
		t.Errorf("error leaks the cause: %v", err)
	}
}
