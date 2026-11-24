package crypto_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
)

// Keys carry the raw configured keys: no way of printing or encoding them may show a key, its hex
// or its base64.
func TestKeysAreUnprintable(t *testing.T) {
	cur, old := bytes.Repeat([]byte{0xA7}, 32), bytes.Repeat([]byte{0x5C}, 32)
	k := crypto.Keys{Current: cur, Previous: old}
	var forbidden []string
	for _, key := range [][]byte{cur, old} {
		forbidden = append(forbidden, string(key), hex.EncodeToString(key),
			base64.StdEncoding.EncodeToString(key), base64.RawStdEncoding.EncodeToString(key),
			base64.URLEncoding.EncodeToString(key), fmt.Sprintf("%v", key))
	}
	check := func(what, text string) {
		t.Helper()
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s shows key material: %q", what, text)
			}
		}
	}
	for _, v := range []any{k, &k, []crypto.Keys{k}, struct{ K crypto.Keys }{k}, map[string]crypto.Keys{"a": k}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%X", "%q"} {
			check(verb+" of "+fmt.Sprintf("%T", v), fmt.Sprintf(verb, v))
		}
	}
	check("Sprint", fmt.Sprint(k))
	check("String", k.String())
	j, err := json.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	check("json", string(j))
	var text, js bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("keys", "keys", k, "p", &k)
	slog.New(slog.NewJSONHandler(&js, nil)).Info("keys", "keys", k, "p", &k)
	check("slog text", text.String())
	check("slog json", js.String())
	if !strings.Contains(fmt.Sprint(k), "[Redacted]") {
		t.Errorf("no marker: %q", fmt.Sprint(k))
	}
}
