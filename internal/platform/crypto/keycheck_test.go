package crypto_test

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
)

// checkPlain is the check text of the reference 1.0.x: PLAIN in
// v1.0.1:apps/server/src/platform/crypto/key-check.ts (unchanged since 1.0.0). It is pinned here
// rather than read from that file, so the test does not depend on the TypeScript tree; whether the
// two images really accept each other's key check is judged by the compat suite.
const checkPlain = "mcp-registry key check v1"

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func sealWith(t *testing.T, key []byte, plain string) []byte {
	t.Helper()
	box, err := crypto.NewAESGCMBox(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestJudgeFourOutcomes(t *testing.T) {
	cur, old, other := randKey(t), randKey(t), randKey(t)
	cases := []struct {
		name   string
		sealed []byte
		keys   crypto.Keys
		want   string
	}{
		{"absent", nil, crypto.NewKeys(cur, nil), crypto.KeyCheckAbsent},
		{"ok", sealWith(t, cur, checkPlain), crypto.NewKeys(cur, old), crypto.KeyCheckOK},
		{"needs rotation", sealWith(t, old, checkPlain), crypto.NewKeys(cur, old), crypto.KeyCheckNeedsRotation},
		{"foreign key", sealWith(t, other, checkPlain), crypto.NewKeys(cur, old), crypto.KeyCheckMismatch},
		{"old key without previous", sealWith(t, old, checkPlain), crypto.NewKeys(cur, nil), crypto.KeyCheckMismatch},
		{"right key, other text", sealWith(t, cur, "something else"), crypto.NewKeys(cur, nil), crypto.KeyCheckMismatch},
		{"garbage", []byte("short"), crypto.NewKeys(cur, nil), crypto.KeyCheckMismatch},
	}
	for _, c := range cases {
		if got := crypto.Judge(c.sealed, c.keys); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestJudgeOpensTheValueSealedByTypeScript(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		KeyCheck struct {
			Key    string `json:"key"`
			Plain  string `json:"plain"`
			Sealed string `json:"sealed"`
		} `json:"keyCheck"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(v.KeyCheck.Key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := base64.StdEncoding.DecodeString(v.KeyCheck.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	if v.KeyCheck.Plain != checkPlain {
		t.Fatalf("vector plain %q", v.KeyCheck.Plain)
	}
	if got := crypto.Judge(sealed, crypto.NewKeys(key, nil)); got != crypto.KeyCheckOK {
		t.Fatalf("Judge = %q, want ok", got)
	}
	if got := crypto.Judge(sealed, crypto.NewKeys(randKey(t), nil)); got != crypto.KeyCheckMismatch {
		t.Fatalf("Judge with another key = %q, want mismatch", got)
	}
}

// The check text is the reference's, not a free choice: a database whose key check the reference
// sealed must judge ok here and the reverse (compat TestPlatformStartsOnReferenceDatabase and
// TestPlatformReverseHandOver judge both images on one database). checkPlain is pinned above; this
// test proves Judge accepts exactly that text and nothing near it.
func TestCheckTextIsPinnedToTheReference(t *testing.T) {
	key := randKey(t)
	if got := crypto.Judge(sealWith(t, key, checkPlain), crypto.NewKeys(key, nil)); got != crypto.KeyCheckOK {
		t.Fatalf("Judge of the reference text = %q, want ok", got)
	}
	for _, near := range []string{"mcp-registry key check v2", "mcp-registry key check v1 ", "MCP-registry key check v1", ""} {
		if got := crypto.Judge(sealWith(t, key, near), crypto.NewKeys(key, nil)); got != crypto.KeyCheckMismatch {
			t.Errorf("Judge of %q = %q, want mismatch: the check text must be exactly %q", near, got, checkPlain)
		}
	}
}
