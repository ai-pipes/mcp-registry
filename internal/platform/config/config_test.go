package config_test

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/config"
)

func b64(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

type env map[string]string

func (e env) with(pairs ...string) env {
	out := env{}
	for k, v := range e {
		out[k] = v
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = pairs[i+1]
	}
	return out
}

func (e env) without(names ...string) env {
	out := env{}
	for k, v := range e {
		if !slices.Contains(names, k) {
			out[k] = v
		}
	}
	return out
}

func files(m map[string]string) func(string) (string, error) {
	return func(p string) (string, error) {
		if s, ok := m[p]; ok {
			return s, nil
		}
		return "", fmt.Errorf("ENOENT %s", p)
	}
}

func noFiles(string) (string, error) { return "", errors.New("must not read") }

func baseEnv(t *testing.T) env {
	return env{
		"DATABASE_URL":       "postgres://u@h/db",
		"PUBLIC_URL":         "http://localhost:3000",
		"SECRETS_KEY":        b64(t, 32),
		"OIDC_ISSUER":        "https://id.example.com",
		"OIDC_CLIENT_ID":     "mcp-gateway",
		"OIDC_CLIENT_SECRET": "client-secret",
	}
}

func issueOf(err error) []config.Issue {
	var ce *config.Error
	if errors.As(err, &ce) {
		return ce.Issues
	}
	return nil
}

func TestLoadDefaults(t *testing.T) {
	e := baseEnv(t)
	c, err := config.Load(e, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 3000 || c.TrustProxy || c.TrustProxyHops != 1 || !c.CookieSecure || c.Embedder != "local" ||
		c.ModelDir != "/app/models" || c.LogLevel != "info" || len(c.AllowedHosts) != 0 ||
		c.SearchMinSimilarity != 0.83 || c.SyncIntervalHours != 6 || c.CallLogRetentionDays != 90 ||
		c.UpstreamMaxResponseBytes != 20*1024*1024 || c.MCPMaxBodyBytes != 10*1024*1024 ||
		c.OIDCLoginGroup != "mcp-users" || c.OIDCAdminGroup != "mcp-admins" ||
		c.OIDCScopes != "openid profile email groups" || c.OIDCGroupsClaim != "groups" {
		t.Fatalf("defaults: %+v", c)
	}
	if c.OIDC == nil || *c.OIDC != (config.OIDCClient{Issuer: "https://id.example.com", ClientID: "mcp-gateway", ClientSecret: "client-secret"}) {
		t.Fatalf("oidc: %+v", c.OIDC)
	}
	if base64.StdEncoding.EncodeToString(c.SecretsKey) != e["SECRETS_KEY"] || c.SecretsKeyOld != nil || c.DatabasePassword != nil {
		t.Fatal("keys or password")
	}
}

func TestLoadMinimalWithoutOIDC(t *testing.T) {
	e := baseEnv(t).without("OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET")
	c, err := config.Load(e, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.OIDC != nil || c.OIDCLoginGroup != "mcp-users" || c.OIDCAdminGroup != "mcp-admins" {
		t.Fatalf("%+v", c)
	}
	// An empty value is unset, for the three OIDC variables too.
	c, err = config.Load(baseEnv(t).with("OIDC_ISSUER", "", "OIDC_CLIENT_ID", "", "OIDC_CLIENT_SECRET", ""), noFiles)
	if err != nil || c.OIDC != nil {
		t.Fatalf("empty OIDC: %v %+v", err, c.OIDC)
	}
}

func TestLoadOverrides(t *testing.T) {
	e := baseEnv(t).with(
		"PORT", "8080", "ALLOWED_HOSTS", "a.com, b.com,", "TRUST_PROXY", "true", "TRUST_PROXY_HOPS", "2",
		"COOKIE_SECURE", "false", "SEARCH_MIN_SIMILARITY", "0.5", "OIDC_LOGIN_GROUP", "staff",
		"OIDC_ADMIN_GROUP", "owners", "OIDC_SCOPES", "openid, profile  roles openid",
		"OIDC_GROUPS_CLAIM", "urn:zitadel:iam:org:project:roles", "SYNC_INTERVAL_HOURS", "12",
		"CALL_LOG_RETENTION_DAYS", "30", "UPSTREAM_MAX_RESPONSE_BYTES", "1048576", "MCP_MAX_BODY_BYTES", "2097152",
		"EMBEDDER", "openai-compatible", "EMBEDDER_URL", "http://e/v1", "EMBEDDER_MODEL", "m", "LOG_LEVEL", "debug",
		"MODEL_DIR", "/m",
	)
	c, err := config.Load(e, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8080 || !slices.Equal(c.AllowedHosts, []string{"a.com", "b.com"}) || !c.TrustProxy || c.TrustProxyHops != 2 ||
		c.CookieSecure || c.SearchMinSimilarity != 0.5 || c.OIDCLoginGroup != "staff" || c.OIDCAdminGroup != "owners" ||
		c.OIDCScopes != "openid profile roles" || c.OIDCGroupsClaim != "urn:zitadel:iam:org:project:roles" ||
		c.SyncIntervalHours != 12 || c.CallLogRetentionDays != 30 || c.UpstreamMaxResponseBytes != 1048576 ||
		c.MCPMaxBodyBytes != 2097152 || c.Embedder != "openai-compatible" || c.EmbedderURL != "http://e/v1" ||
		c.EmbedderModel != "m" || c.LogLevel != "debug" || c.ModelDir != "/m" {
		t.Fatalf("%+v", c)
	}
	if _, err := config.Load(baseEnv(t).with("SYNC_INTERVAL_HOURS", "8760"), noFiles); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEmptyMeansUnset(t *testing.T) {
	c, err := config.Load(baseEnv(t).with(
		"EMBEDDER_URL", "", "EMBEDDER_MODEL", "", "ALLOWED_HOSTS", "", "TRUST_PROXY", "", "PORT", "",
		"OIDC_LOGIN_GROUP", "", "SYNC_INTERVAL_HOURS", "", "OIDC_SCOPES", "",
		"SECRETS_KEY_FILE", "", "PGPASSWORD_FILE", "", "SECRETS_KEY_OLD", "", "PGPASSWORD", "",
	), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.EmbedderURL != "" || c.EmbedderModel != "" || c.TrustProxy || c.Port != 3000 || c.OIDCLoginGroup != "mcp-users" ||
		c.SyncIntervalHours != 6 || c.OIDCScopes != "openid profile email groups" || c.DatabasePassword != nil || c.SecretsKeyOld != nil {
		t.Fatalf("%+v", c)
	}
}

func TestLoadFiles(t *testing.T) {
	key := b64(t, 32)
	noKey := baseEnv(t).without("SECRETS_KEY")
	c, err := config.Load(noKey.with("SECRETS_KEY_FILE", "/k"), files(map[string]string{"/k": key + "\n"}))
	if err != nil || base64.StdEncoding.EncodeToString(c.SecretsKey) != key {
		t.Fatalf("key from file: %v", err)
	}
	c, err = config.Load(baseEnv(t).with("PGPASSWORD_FILE", "/p"), files(map[string]string{"/p": "pw\n"}))
	if err != nil || c.DatabasePassword == nil || *c.DatabasePassword != "pw" {
		t.Fatalf("password from file: %v", err)
	}
	// PGPASSWORD alone is the client's business: Config does not carry it.
	c, err = config.Load(baseEnv(t).with("PGPASSWORD", "x"), noFiles)
	if err != nil || c.DatabasePassword != nil {
		t.Fatalf("PGPASSWORD alone: %v", err)
	}
}

// refusal is one rejected configuration: the variables that must be named with a reason, paths that
// must appear, and values that must not.
type refusal struct {
	name   string
	env    func(e env) env
	files  map[string]string
	vars   []string
	paths  []string
	values []string
}

func TestLoadRefused(t *testing.T) {
	key := b64(t, 32)
	short := b64(t, 16)
	long := b64(t, 33)
	idp := "https://id.example.com"
	set := func(pairs ...string) func(env) env { return func(e env) env { return e.with(pairs...) } }
	noKey := func(pairs ...string) func(env) env {
		return func(e env) env { return e.without("SECRETS_KEY").with(pairs...) }
	}
	noOIDC := func(pairs ...string) func(env) env {
		return func(e env) env {
			return e.without("OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET").with(pairs...)
		}
	}
	cases := []refusal{
		{name: "OIDC_ISSUER alone", env: noOIDC("OIDC_ISSUER", idp), vars: []string{"OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET"}},
		{name: "OIDC_ISSUER missing", env: noOIDC("OIDC_CLIENT_ID", "cid", "OIDC_CLIENT_SECRET", "ZZ-oidc-secret-1"), vars: []string{"OIDC_ISSUER"}, values: []string{"ZZ-oidc-secret-1"}},
		{name: "OIDC_CLIENT_ID missing", env: noOIDC("OIDC_ISSUER", idp, "OIDC_CLIENT_SECRET", "ZZ-oidc-secret-2"), vars: []string{"OIDC_CLIENT_ID"}, values: []string{"ZZ-oidc-secret-2"}},
		{name: "OIDC_CLIENT_SECRET missing", env: noOIDC("OIDC_ISSUER", idp, "OIDC_CLIENT_ID", "cid"), vars: []string{"OIDC_CLIENT_SECRET"}},
		{name: "one set, two empty", env: set("OIDC_CLIENT_ID", "", "OIDC_CLIENT_SECRET", ""), vars: []string{"OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET"}},
		{name: "OIDC_ISSUER empty with the others set", env: set("OIDC_ISSUER", ""), vars: []string{"OIDC_ISSUER"}},
		{name: "OIDC_ISSUER is not a URL", env: set("OIDC_ISSUER", "ZZ-not-a-url", "OIDC_CLIENT_SECRET", "ZZ-oidc-secret-3"), vars: []string{"OIDC_ISSUER"}, values: []string{"ZZ-not-a-url", "ZZ-oidc-secret-3"}},
		{name: "OIDC_SCOPES without openid, OIDC configured", env: set("OIDC_SCOPES", "profile,ZZ-scope", "OIDC_CLIENT_SECRET", "ZZ-oidc-secret-4"), vars: []string{"OIDC_SCOPES"}, values: []string{"ZZ-scope", "ZZ-oidc-secret-4"}},
		{name: "OIDC_SCOPES without openid, OIDC half set", env: noOIDC("OIDC_ISSUER", idp, "OIDC_SCOPES", "profile"), vars: []string{"OIDC_SCOPES", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET"}},

		{name: "SECRETS_KEY together with SECRETS_KEY_FILE", env: set("SECRETS_KEY", key, "SECRETS_KEY_FILE", "/k"), files: map[string]string{"/k": key}, vars: []string{"SECRETS_KEY_FILE"}, values: []string{key}},
		{name: "PGPASSWORD together with PGPASSWORD_FILE", env: set("PGPASSWORD", "ZZ-pg-secret", "PGPASSWORD_FILE", "/p"), files: map[string]string{"/p": "pw"}, vars: []string{"PGPASSWORD_FILE"}, values: []string{"ZZ-pg-secret"}},
		{name: "unreadable files", env: noKey("SECRETS_KEY_FILE", "/run/ZZ-missing/key", "PGPASSWORD_FILE", "/run/ZZ-missing/pw"), vars: []string{"SECRETS_KEY_FILE", "PGPASSWORD_FILE"}, paths: []string{"/run/ZZ-missing/key", "/run/ZZ-missing/pw"}},
		{name: "empty files", env: noKey("SECRETS_KEY_FILE", "/k", "PGPASSWORD_FILE", "/p"), files: map[string]string{"/k": "", "/p": "  \n\t\n"}, vars: []string{"SECRETS_KEY_FILE", "PGPASSWORD_FILE"}, paths: []string{"/k", "/p"}},
		{name: "a key file that holds no key", env: noKey("SECRETS_KEY_FILE", "/k"), files: map[string]string{"/k": "ZZ-key-file-content\n"}, vars: []string{"SECRETS_KEY_FILE"}, paths: []string{"/k"}, values: []string{"ZZ-key-file-content"}},
		{name: "a 16-byte key file", env: noKey("SECRETS_KEY_FILE", "/k"), files: map[string]string{"/k": short + "\n"}, vars: []string{"SECRETS_KEY_FILE"}, paths: []string{"/k"}, values: []string{short}},

		{name: "SECRETS_KEY unset", env: func(e env) env { return e.without("SECRETS_KEY") }, vars: []string{"SECRETS_KEY"}},
		{name: "SECRETS_KEY empty", env: set("SECRETS_KEY", ""), vars: []string{"SECRETS_KEY"}},
		{name: "SECRETS_KEY is not base64", env: set("SECRETS_KEY", "ZZ-not-base64!"), vars: []string{"SECRETS_KEY"}, values: []string{"ZZ-not-base64!"}},
		{name: "SECRETS_KEY is 16 bytes", env: set("SECRETS_KEY", short), vars: []string{"SECRETS_KEY"}, values: []string{short}},
		{name: "SECRETS_KEY is 33 bytes", env: set("SECRETS_KEY", long), vars: []string{"SECRETS_KEY"}, values: []string{long}},
		{name: "SECRETS_KEY is not canonical base64", env: set("SECRETS_KEY", strings.TrimRight(key, "=")), vars: []string{"SECRETS_KEY"}},
		{name: "SECRETS_KEY_OLD equals SECRETS_KEY", env: set("SECRETS_KEY", key, "SECRETS_KEY_OLD", key), vars: []string{"SECRETS_KEY_OLD"}, values: []string{key}},
		{name: "SECRETS_KEY_OLD is not a key", env: set("SECRETS_KEY_OLD", "ZZ-old-key"), vars: []string{"SECRETS_KEY_OLD"}, values: []string{"ZZ-old-key"}},

		// URLs: not a url, relative, scheme without host, and the accepted https://ok.example below.
		{name: "PUBLIC_URL is not a URL", env: set("PUBLIC_URL", "not a url"), vars: []string{"PUBLIC_URL"}, values: []string{"not a url"}},
		{name: "PUBLIC_URL is relative", env: set("PUBLIC_URL", "/relative"), vars: []string{"PUBLIC_URL"}},
		{name: "PUBLIC_URL has no host", env: set("PUBLIC_URL", "http://"), vars: []string{"PUBLIC_URL"}},
		{name: "PUBLIC_URL unset", env: func(e env) env { return e.without("PUBLIC_URL") }, vars: []string{"PUBLIC_URL"}},
		{name: "DATABASE_URL is unset", env: set("DATABASE_URL", ""), vars: []string{"DATABASE_URL"}},
		{name: "EMBEDDER is not a known one", env: set("EMBEDDER", "ZZ-embedder"), vars: []string{"EMBEDDER"}, values: []string{"ZZ-embedder"}},
		{name: "EMBEDDER_URL is not a URL", env: set("EMBEDDER", "openai-compatible", "EMBEDDER_MODEL", "m", "EMBEDDER_URL", "ZZ-embedder-url"), vars: []string{"EMBEDDER_URL"}, values: []string{"ZZ-embedder-url"}},
		{name: "TRUST_PROXY is not true or false", env: set("TRUST_PROXY", "ZZ-yes"), vars: []string{"TRUST_PROXY"}, values: []string{"ZZ-yes"}},
		{name: "COOKIE_SECURE is 1", env: set("COOKIE_SECURE", "1"), vars: []string{"COOKIE_SECURE"}},
		{name: "LOG_LEVEL is not a level", env: set("LOG_LEVEL", "ZZ-loud"), vars: []string{"LOG_LEVEL"}, values: []string{"ZZ-loud"}},

		{name: "PORT is 0", env: set("PORT", "0"), vars: []string{"PORT"}},
		{name: "PORT is not a number", env: set("PORT", "ZZ99"), vars: []string{"PORT"}, values: []string{"ZZ99"}},
		{name: "PORT is above 65535", env: set("PORT", "65536"), vars: []string{"PORT"}},
		{name: "PORT is fractional", env: set("PORT", "3000.5"), vars: []string{"PORT"}},
		{name: "PORT is an exponent", env: set("PORT", "1e3"), vars: []string{"PORT"}},
		{name: "PORT overflows", env: set("PORT", "99999999999999999999999"), vars: []string{"PORT"}},
		{name: "TRUST_PROXY_HOPS is 9", env: set("TRUST_PROXY_HOPS", "9"), vars: []string{"TRUST_PROXY_HOPS"}},
		{name: "TRUST_PROXY_HOPS is 0", env: set("TRUST_PROXY_HOPS", "0"), vars: []string{"TRUST_PROXY_HOPS"}},
		{name: "TRUST_PROXY_HOPS is 1.5", env: set("TRUST_PROXY_HOPS", "1.5"), vars: []string{"TRUST_PROXY_HOPS"}},
		{name: "SEARCH_MIN_SIMILARITY is 2", env: set("SEARCH_MIN_SIMILARITY", "2"), vars: []string{"SEARCH_MIN_SIMILARITY"}},
		{name: "SEARCH_MIN_SIMILARITY is negative", env: set("SEARCH_MIN_SIMILARITY", "-0.5"), vars: []string{"SEARCH_MIN_SIMILARITY"}},
		{name: "SEARCH_MIN_SIMILARITY is NaN", env: set("SEARCH_MIN_SIMILARITY", "NaN"), vars: []string{"SEARCH_MIN_SIMILARITY"}},
		{name: "CALL_LOG_RETENTION_DAYS is 0", env: set("CALL_LOG_RETENTION_DAYS", "0"), vars: []string{"CALL_LOG_RETENTION_DAYS"}},
		{name: "UPSTREAM_MAX_RESPONSE_BYTES is 0", env: set("UPSTREAM_MAX_RESPONSE_BYTES", "0"), vars: []string{"UPSTREAM_MAX_RESPONSE_BYTES"}},
		{name: "MCP_MAX_BODY_BYTES is 0", env: set("MCP_MAX_BODY_BYTES", "0"), vars: []string{"MCP_MAX_BODY_BYTES"}},
		{name: "SYNC_INTERVAL_HOURS is 0", env: set("SYNC_INTERVAL_HOURS", "0"), vars: []string{"SYNC_INTERVAL_HOURS"}},
		{name: "SYNC_INTERVAL_HOURS is 8761", env: set("SYNC_INTERVAL_HOURS", "8761"), vars: []string{"SYNC_INTERVAL_HOURS"}},
		{name: "SYNC_INTERVAL_HOURS is not a number", env: set("SYNC_INTERVAL_HOURS", "ZZ6h"), vars: []string{"SYNC_INTERVAL_HOURS"}, values: []string{"ZZ6h"}},
		{name: "several at once", env: set("PORT", "0", "TRUST_PROXY_HOPS", "9", "SEARCH_MIN_SIMILARITY", "2"), vars: []string{"PORT", "TRUST_PROXY_HOPS", "SEARCH_MIN_SIMILARITY"}},

		// Owner decision (config-errors): an ordinary configuration error naming each missing variable.
		{name: "openai-compatible without URL and model", env: set("EMBEDDER", "openai-compatible"), vars: []string{"EMBEDDER_URL", "EMBEDDER_MODEL"}},
		{name: "openai-compatible without a model", env: set("EMBEDDER", "openai-compatible", "EMBEDDER_URL", "http://embedder.compat.test/v1"), vars: []string{"EMBEDDER_MODEL"}},
		{name: "openai-compatible without a URL", env: set("EMBEDDER", "openai-compatible", "EMBEDDER_MODEL", "m"), vars: []string{"EMBEDDER_URL"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(tc.env(baseEnv(t).with("SECRETS_KEY", key)), files(tc.files))
			if err == nil {
				t.Fatal("no error")
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "Invalid configuration:\n") {
				t.Fatalf("prefix: %q", msg)
			}
			lines := strings.Split(msg, "\n")[1:]
			for _, l := range lines {
				if !strings.HasPrefix(l, "  ") || !strings.Contains(l, ": ") {
					t.Errorf("line is not `  NAME: reason`: %q", l)
				}
			}
			for _, v := range tc.vars {
				if !slices.ContainsFunc(lines, func(l string) bool {
					rest, ok := strings.CutPrefix(l, "  "+v+": ")
					return ok && rest != ""
				}) {
					t.Errorf("%s is not named with a reason:\n%s", v, msg)
				}
			}
			if len(lines) != len(tc.vars) && tc.name != "OIDC_SCOPES without openid, OIDC half set" {
				t.Errorf("want %d lines, got %d:\n%s", len(tc.vars), len(lines), msg)
			}
			for _, p := range tc.paths {
				if !strings.Contains(msg, p) {
					t.Errorf("path %s is not named:\n%s", p, msg)
				}
			}
			for _, v := range append(slices.Clone(tc.values), "client-secret", "postgres://u@h/db") {
				if strings.Contains(msg, v) {
					t.Errorf("the message contains %q:\n%s", v, msg)
				}
			}
		})
	}
}

func TestLoadKeyFromFileIsReportedAgainstTheFileVariable(t *testing.T) {
	_, err := config.Load(baseEnv(t).without("SECRETS_KEY").with("SECRETS_KEY_FILE", "/k"), files(map[string]string{"/k": b64(t, 16)}))
	is := issueOf(err)
	if len(is) != 1 || is[0].Name != "SECRETS_KEY_FILE" || !strings.Contains(is[0].Message, "32 bytes") {
		t.Fatalf("%+v", is)
	}
}

func TestLoadOIDCScopesWithoutOpenidWithoutOIDC(t *testing.T) {
	// Owner decision (config-errors): validated only when OIDC is configured.
	e := baseEnv(t).without("OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET").with("OIDC_SCOPES", "profile,ZZ-scope")
	if _, err := config.Load(e, noFiles); err != nil {
		t.Fatal(err)
	}
}

func TestLoadURLs(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"not a url", false},
		{"/relative", false},
		{"http://", false},
		{"https://ok.example", true},
		{"http://localhost:3000", true},
		{"example.com", false},
		{"http://:80", false},
	} {
		_, err := config.Load(baseEnv(t).with("PUBLIC_URL", tc.url), noFiles)
		if (err == nil) != tc.ok {
			t.Errorf("%q: ok=%v err=%v", tc.url, tc.ok, err)
		}
	}
}

func TestLoadIssueOrderIsDeclarationOrder(t *testing.T) {
	_, err := config.Load(env{"PGPASSWORD_FILE": "/nope", "LOG_LEVEL": "x", "PORT": "x", "PUBLIC_URL": "x"}, files(nil))
	var names []string
	for _, i := range issueOf(err) {
		names = append(names, i.Name)
	}
	want := []string{"PORT", "DATABASE_URL", "PGPASSWORD_FILE", "PUBLIC_URL", "SECRETS_KEY", "LOG_LEVEL"}
	if !slices.Equal(names, want) {
		t.Fatalf("got %v want %v", names, want)
	}
}
