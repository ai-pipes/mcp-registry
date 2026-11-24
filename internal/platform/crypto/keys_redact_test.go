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
	k := crypto.NewKeys(cur, old)
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

type (
	unexportedHolder struct{ k crypto.Keys }
	nestedHolder     struct{ inner unexportedHolder }
	ptrHolder        struct{ k *crypto.Keys }
	sliceHolder      struct{ ks []crypto.Keys }
	mapHolder        struct{ m map[string]crypto.Keys }
)

// ExportedHolder carries the keys in an exported field, for JSON.
type ExportedHolder struct{ K crypto.Keys }

// However Keys is held (an unexported field, nested, behind a pointer, in a slice or a map), no
// verb, JSON encoder or slog handler shows a key: plaintext, hex, base64, or the decimal byte list.
func TestKeysAreUnprintableHoweverHeld(t *testing.T) {
	cur, old := bytes.Repeat([]byte{0xA7}, 32), bytes.Repeat([]byte{0x5C}, 32)
	k := crypto.NewKeys(cur, old)
	var forbidden []string
	for _, key := range [][]byte{cur, old} {
		forbidden = append(forbidden, string(key), hex.EncodeToString(key),
			base64.StdEncoding.EncodeToString(key), base64.RawStdEncoding.EncodeToString(key),
			base64.URLEncoding.EncodeToString(key), fmt.Sprintf("%v", key),
			strings.TrimSuffix(strings.TrimPrefix(fmt.Sprintf("%v", key), "["), "]"))
	}
	check := func(what, text string) {
		t.Helper()
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s shows key material: %.120q", what, text)
			}
		}
	}
	holders := map[string]any{
		"unexported field":  unexportedHolder{k},
		"nested":            nestedHolder{unexportedHolder{k}},
		"behind a pointer":  ptrHolder{&k},
		"pointer to holder": &ptrHolder{&k},
		"slice":             sliceHolder{[]crypto.Keys{k}},
		"map value":         mapHolder{map[string]crypto.Keys{"a": k}},
		"bare":              k,
	}
	for name, h := range holders {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
			check(name+" "+verb, fmt.Sprintf(verb, h))
		}
		var text, js bytes.Buffer
		slog.New(slog.NewTextHandler(&text, nil)).Info("m", "h", h)
		slog.New(slog.NewJSONHandler(&js, nil)).Info("m", "h", h)
		check(name+" slog text", text.String())
		check(name+" slog json", js.String())
	}
	j, err := json.Marshal(ExportedHolder{k})
	if err != nil {
		t.Fatal(err)
	}
	check("json", string(j))
}

// The zero value is "no keys" and the constructor copies its inputs.
func TestKeysZeroValueAndCopy(t *testing.T) {
	var zero crypto.Keys
	if v := crypto.Judge(nil, zero); v != crypto.KeyCheckAbsent {
		t.Errorf("zero Keys, no stored check: %q", v)
	}
	if v := crypto.Judge([]byte("x"), zero); v != crypto.KeyCheckMismatch {
		t.Errorf("zero Keys opens nothing: %q", v)
	}
	cur := randKey(t)
	sealed := sealWith(t, cur, checkPlain)
	k := crypto.NewKeys(cur, nil)
	for i := range cur {
		cur[i] = 0
	}
	if v := crypto.Judge(sealed, k); v != crypto.KeyCheckOK {
		t.Errorf("a caller changing its slice changed the keys: %q", v)
	}
}
