package kernel_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
)

// However a Secret or SecretBytes is held (bare, behind a pointer, in an unexported field, nested,
// in a slice, as a map value, in an any), no verb, slog handler or JSON encoder shows the value:
// plaintext, hex, base64 or the decimal byte list.
func TestSecretsAreUnprintableHoweverHeld(t *testing.T) {
	text := "ZZ-plain-text-secret-0042"
	raw := bytes.Repeat([]byte{0xA7, 0x5C, 0x01}, 12)
	forbidden := append(secrettest.Strings(t, text), secrettest.Forbidden(t, raw)...)
	secrettest.AssertUnprintable(t, "Secret", kernel.NewSecret(text), forbidden)
	secrettest.AssertUnprintable(t, "SecretBytes", kernel.NewSecretBytes(raw), forbidden)
}

func TestSecretMarkersAndReveal(t *testing.T) {
	s, b := kernel.NewSecret("ZZ-s"), kernel.NewSecretBytes([]byte("ZZ-b"))
	if s.Reveal() != "ZZ-s" || string(b.Reveal()) != "ZZ-b" || !s.IsSet() || !b.IsSet() || b.Len() != 4 {
		t.Fatal("Reveal must return the value of a set secret")
	}
	for _, v := range []any{s, b, &s, &b} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			if out := fmt.Sprintf(verb, v); out != kernel.SecretRedacted {
				t.Errorf("%s of %T: %q", verb, v, out)
			}
		}
	}
	for _, m := range []interface {
		MarshalText() ([]byte, error)
		MarshalJSON() ([]byte, error)
	}{s, b} {
		if txt, err := m.MarshalText(); err != nil || string(txt) != "[Redacted]" {
			t.Errorf("MarshalText: %q %v", txt, err)
		}
		if j, err := m.MarshalJSON(); err != nil || string(j) != `"[Redacted]"` {
			t.Errorf("MarshalJSON: %q %v", j, err)
		}
	}
}

func TestZeroSecretIsUnset(t *testing.T) {
	var s kernel.Secret
	var b kernel.SecretBytes
	if s.IsSet() || b.IsSet() || s.Reveal() != "" || b.Reveal() != nil || b.Len() != 0 {
		t.Fatal("the zero value is not set and reveals nothing")
	}
	for _, v := range []any{s, b} {
		if out := fmt.Sprint(v); out != "[unset]" {
			t.Errorf("%T: %q", v, out)
		}
		if j, err := json.Marshal(v); err != nil || string(j) != `"[unset]"` {
			t.Errorf("json of an unset %T: %s %v", v, j, err)
		}
	}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "s", s, "b", b)
	if !strings.Contains(logs.String(), "s=[unset] b=[unset]") {
		t.Errorf("slog: %s", logs.String())
	}
}

// An empty value is a set secret: the zero value is the only unset one (a server secret that is
// set to "" is not "no secret").
func TestEmptyValuesAreSet(t *testing.T) {
	if !kernel.NewSecret("").IsSet() || !kernel.NewSecretBytes(nil).IsSet() || kernel.NewSecretBytes(nil).Reveal() != nil {
		t.Fatal("an empty value wrapped is set and reveals as empty")
	}
}

func TestSecretBytesCopies(t *testing.T) {
	raw := []byte("ZZ-orig")
	b := kernel.NewSecretBytes(raw)
	raw[0] = 'X'
	b.Reveal()[1] = 'Y'
	if string(b.Reveal()) != "ZZ-orig" {
		t.Fatalf("the secret changed with its source or through Reveal: %q", b.Reveal())
	}
}

func TestSecretsAreNotComparable(t *testing.T) {
	for _, v := range []any{kernel.Secret{}, kernel.SecretBytes{}} {
		if reflect.TypeOf(v).Comparable() {
			t.Fatalf("%T is comparable with ==", v)
		}
	}
}

// The check itself: a plain string field in an unexported holder is caught.
func TestSecrettestCatchesARawField(t *testing.T) {
	type leaky struct{ Token string }
	text := "ZZ-plain-text-secret-0042"
	leaks := secrettest.Leaks(leaky{Token: text}, secrettest.Strings(t, text))
	for _, want := range []string{"bare %v", "unexported field %s", "pointer to holder %s", "slice %x", "json", "map value slog text"} {
		found := false
		for _, l := range leaks {
			found = found || l == want
		}
		if !found {
			t.Errorf("not reported: %q (got %v)", want, leaks)
		}
	}
	hexOnly := secrettest.Leaks(struct{ b []byte }{[]byte(text)}, secrettest.Strings(t, text))
	if len(hexOnly) == 0 {
		t.Error("a []byte field was not caught")
	}
}
