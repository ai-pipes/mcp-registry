package crypto_test

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
)

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
		{"absent", nil, crypto.Keys{Current: cur}, crypto.KeyCheckAbsent},
		{"ok", sealWith(t, cur, checkPlain), crypto.Keys{Current: cur, Previous: old}, crypto.KeyCheckOK},
		{"needs rotation", sealWith(t, old, checkPlain), crypto.Keys{Current: cur, Previous: old}, crypto.KeyCheckNeedsRotation},
		{"foreign key", sealWith(t, other, checkPlain), crypto.Keys{Current: cur, Previous: old}, crypto.KeyCheckMismatch},
		{"old key without previous", sealWith(t, old, checkPlain), crypto.Keys{Current: cur}, crypto.KeyCheckMismatch},
		{"right key, other text", sealWith(t, cur, "something else"), crypto.Keys{Current: cur}, crypto.KeyCheckMismatch},
		{"garbage", []byte("short"), crypto.Keys{Current: cur}, crypto.KeyCheckMismatch},
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
	if got := crypto.Judge(sealed, crypto.Keys{Current: key}); got != crypto.KeyCheckOK {
		t.Fatalf("Judge = %q, want ok", got)
	}
	if got := crypto.Judge(sealed, crypto.Keys{Current: randKey(t)}); got != crypto.KeyCheckMismatch {
		t.Fatalf("Judge with another key = %q, want mismatch", got)
	}
}

func TestCheckTextMatchesTypeScriptSource(t *testing.T) {
	src, err := os.ReadFile("../../../apps/server/src/platform/crypto/key-check.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "const PLAIN = '"+checkPlain+"';") {
		t.Fatalf("key-check.ts no longer holds the declaration const PLAIN = %q: the Go check text must change with it", checkPlain)
	}
}
