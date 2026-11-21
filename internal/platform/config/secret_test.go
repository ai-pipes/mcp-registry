package config_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

// loadSecrets loads a configuration in which every secret field is set, and returns the values
// that must never be printed (as given, base64 and hex of the keys).
func loadSecrets(t *testing.T) (config.Config, []string) {
	t.Helper()
	key, oldKey := b64(t, 32), b64(t, 32)
	e := baseEnv(t).with(
		"DATABASE_URL", "postgres://dbuser:ZZ-db-pass-7731@db.example/app",
		"OIDC_CLIENT_SECRET", "ZZ-client-secret-4410",
		"SECRETS_KEY", key, "SECRETS_KEY_OLD", oldKey,
		"PGPASSWORD_FILE", "/run/dbfile",
	)
	cfg, err := config.Load(e, files(map[string]string{"/run/dbfile": "ZZ-file-value-9902\n"}))
	if err != nil {
		t.Fatal(err)
	}
	values := []string{"ZZ-db-pass-7731", "ZZ-client-secret-4410", "ZZ-file-value-9902", key, oldKey}
	for _, k := range []string{key, oldKey} {
		raw, err := base64.StdEncoding.DecodeString(k)
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, hex.EncodeToString(raw), strings.TrimRight(k, "="), base64.RawURLEncoding.EncodeToString(raw))
	}
	return cfg, values
}

func assertNoSecrets(t *testing.T, what, text string, values []string) {
	t.Helper()
	for _, v := range values {
		if strings.Contains(text, v) {
			t.Fatalf("%s contains a secret value (%d chars): %q", what, len(v), text)
		}
	}
	if !strings.Contains(text, "[Redacted]") {
		t.Fatalf("%s has no [Redacted] marker: %q", what, text)
	}
}

func TestConfigNeverPrintsItsSecrets(t *testing.T) {
	cfg, values := loadSecrets(t)

	var logs bytes.Buffer
	logging.New("info", &logs).Info("loaded", "config", cfg)
	assertNoSecrets(t, "slog record", logs.String(), values)
	logs.Reset()
	logging.New("info", &logs).Info("loaded", "config", &cfg)
	assertNoSecrets(t, "slog record of a pointer", logs.String(), values)

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%q"} {
		assertNoSecrets(t, "Sprintf "+verb, fmt.Sprintf(verb, cfg), values)
	}
	assertNoSecrets(t, "Sprintf %v of a pointer", fmt.Sprintf("%v", &cfg), values)
	assertNoSecrets(t, "Sprint of the OIDC client", fmt.Sprint(*cfg.OIDC), values)

	v1, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "encoding/json", string(v1), values)
	v2, err := jsonv2.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "encoding/json/v2", string(v2), values)
}

func TestSecretTypesAreRedactedOnEveryPath(t *testing.T) {
	s, b := config.NewSecret("ZZ-s"), config.NewSecretBytes([]byte("ZZ-b"))
	for _, v := range []any{s, b, &s, &b} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%p"} {
			if out := fmt.Sprintf(verb, v); strings.Contains(out, "ZZ") || strings.Contains(out, "5a5a") {
				t.Fatalf("%s of %T printed the value: %q", verb, v, out)
			}
		}
	}
	if s.Reveal() != "ZZ-s" || string(b.Reveal()) != "ZZ-b" || !s.IsSet() || !b.IsSet() {
		t.Fatal("Reveal must return the value of a set secret")
	}
	for _, m := range []interface{ MarshalText() ([]byte, error) }{s, b} {
		if txt, err := m.MarshalText(); err != nil || string(txt) != "[Redacted]" {
			t.Fatalf("MarshalText: %q %v", txt, err)
		}
	}
}

// holder has unexported fields: fmt reaches them by reflection with every method switched off, the
// path on which a string- or slice-based secret type would be printed raw.
type holder struct {
	s config.Secret
	b config.SecretBytes
}

func TestSecretsInsideStructsWithUnexportedFieldsAreNotPrinted(t *testing.T) {
	h := holder{s: config.NewSecret("ZZ-inner"), b: config.NewSecretBytes([]byte("ZZ-bytes"))}
	for _, v := range []any{h, &h, []holder{h}, map[string]holder{"k": h}, struct{ in holder }{h}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%p"} {
			out := fmt.Sprintf(verb, v)
			if strings.Contains(out, "ZZ") || strings.Contains(out, "5a5a") || strings.Contains(out, "5A5A") {
				t.Fatalf("%s of %T printed a secret: %q", verb, v, out)
			}
		}
	}
}

func TestZeroSecretIsUnsetEverywhere(t *testing.T) {
	var s config.Secret
	var b config.SecretBytes
	if s.IsSet() || b.IsSet() || s.Reveal() != "" || b.Reveal() != nil {
		t.Fatal("the zero value is not set and reveals nothing")
	}
	for _, v := range []any{s, b} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d"} {
			if out := fmt.Sprintf(verb, v); out != "[unset]" {
				t.Fatalf("%s of an unset %T: %q", verb, v, out)
			}
		}
		j, err := json.Marshal(v)
		if err != nil || string(j) != `"[unset]"` {
			t.Fatalf("json of an unset %T: %s %v", v, j, err)
		}
	}
	var logs bytes.Buffer
	logging.New("info", &logs).Info("x", "s", s, "b", b)
	if !strings.Contains(logs.String(), "[unset]") || strings.Contains(logs.String(), "[Redacted]") {
		t.Fatalf("slog: %s", logs.String())
	}
	if config.NewSecret("").IsSet() || config.NewSecretBytes(nil).IsSet() || config.NewSecretBytes([]byte{}).IsSet() {
		t.Fatal("an empty value is not a set secret")
	}
}

func TestNewSecretBytesCopiesItsInput(t *testing.T) {
	raw := []byte("ZZ-orig")
	b := config.NewSecretBytes(raw)
	raw[0] = 'X'
	if string(b.Reveal()) != "ZZ-orig" {
		t.Fatalf("the secret changed with its source: %q", b.Reveal())
	}
}

func TestUnsetSecretsStayDistinguishable(t *testing.T) {
	cfg, err := config.Load(baseEnv(t), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePassword.IsSet() || cfg.SecretsKeyOld.IsSet() || !cfg.SecretsKey.IsSet() || !cfg.DatabaseURL.IsSet() {
		t.Fatal("unset secrets must be the zero value, set ones must report IsSet")
	}
}
