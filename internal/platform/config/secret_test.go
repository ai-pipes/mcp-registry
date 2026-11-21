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
	s, b := config.Secret("ZZ-s"), config.SecretBytes("ZZ-b")
	for _, v := range []any{s, b, &s, &b} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
			if out := fmt.Sprintf(verb, v); strings.Contains(out, "ZZ") || strings.Contains(out, "5a5a") {
				t.Fatalf("%s of %T printed the value: %q", verb, v, out)
			}
		}
	}
	if s.Reveal() != "ZZ-s" || string(b.Reveal()) != "ZZ-b" {
		t.Fatal("Reveal must return the value")
	}
	for _, m := range []interface{ MarshalText() ([]byte, error) }{s, b} {
		if txt, err := m.MarshalText(); err != nil || string(txt) != "[Redacted]" {
			t.Fatalf("MarshalText: %q %v", txt, err)
		}
	}
}

func TestUnsetSecretsStayDistinguishable(t *testing.T) {
	cfg, err := config.Load(baseEnv(t), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePassword != nil || cfg.SecretsKeyOld != nil {
		t.Fatal("unset secrets must be nil")
	}
}
