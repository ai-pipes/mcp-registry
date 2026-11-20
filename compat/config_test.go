//go:build compat

package compat_test

// Configuration scenarios (requirement 7 of the migration plan): every environment variable the
// operator may set, what the app refuses and how it says so, and the settings that change what the
// app does. Nothing here is a log message or a validator's wording: a refusal pins the exit code,
// the operator-facing prefix `Invalid configuration:` and that each offending variable is NAMED (with
// a reason after its name, and for a *_FILE setting its path), and that neither the offending value
// nor any secret is printed.
//
//   - TestConfigRefused: one stack, one app start per refused configuration (SwapImageExpectingExit
//     on the same database), then the same stack starts healthy again.
//   - TestConfigEmptyMeansUnset: an empty OIDC_* counts as unset, also for the "all three or none" rule.
//   - TestConfigSecretsFromFiles: SECRETS_KEY_FILE and PGPASSWORD_FILE.
//   - TestConfigNetworkAndRetention: PORT, ALLOWED_HOSTS, TRUST_PROXY_HOPS, CALL_LOG_RETENTION_DAYS.
//   - TestConfigOpenAICompatibleEmbedder: EMBEDDER, EMBEDDER_URL, EMBEDDER_MODEL against a fake.
//   - SEARCH_MIN_SIMILARITY is in search_vectors_test.go (it shares the stack with the index of the
//     fixture servers).
//   - MODEL_DIR: the image bakes the model in at /app/models, and a model directory that holds no
//     model cannot be made from outside without changing the image: the variable is not exercised
//     (reported as a gap); EMBEDDER=local with the default directory is what every other scenario runs.
//
// Findings about the image that these scenarios pin (the real behaviour differs from CLAUDE.md or is
// worth the owner's attention): see the report; each is marked "FINDING" below.

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// configRefusal is one refused configuration. env is applied on top of the stack's settings for one
// start (an empty value removes the variable).
type configRefusal struct {
	name string
	env  map[string]string
	// vars are the variables the output must name, each followed by a reason on the same line.
	vars []string
	// paths are file paths the output must name (the *_FILE cases).
	paths []string
	// values must not appear in the output: the offending values (recognisable markers) and any
	// secret of the stack are always checked as well.
	values []string
	// noPrefix: the refusal is not a configuration error report (see the FINDING on EMBEDDER).
	noPrefix bool
}

// cfgEnv builds an environment override from NAME, value pairs.
func cfgEnv(pairs ...string) map[string]string {
	out := make(map[string]string, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = pairs[i+1]
	}
	return out
}

// configBase64 is base64 of n random bytes.
func configBase64(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestConfigRefused(t *testing.T) {
	key := configBase64(t, 32)
	s := harness.Start(t, harness.Options{
		Env: map[string]string{"SECRETS_KEY": key},
		Files: map[string][]byte{
			"/tmp/config-empty-key": nil,                           // an empty file
			"/tmp/config-blank-pw":  []byte("  \n\t\n"),            // only whitespace: empty after trimming
			"/tmp/config-bad-key":   []byte("ZZ-key-file-content"), // not a key
		},
	})
	image := harness.DefaultImage()
	idpURL := "http://idp.compat.test"

	cases := []configRefusal{
		// --- OIDC: all three or none; an empty value is unset ---
		{
			name: "OIDC_ISSUER alone", env: cfgEnv("OIDC_ISSUER", idpURL),
			vars: []string{"OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET"},
		},
		{
			name: "OIDC_ISSUER missing", env: cfgEnv("OIDC_CLIENT_ID", "cid", "OIDC_CLIENT_SECRET", "ZZ-oidc-secret-1"),
			vars: []string{"OIDC_ISSUER"}, values: []string{"ZZ-oidc-secret-1"},
		},
		{
			name: "OIDC_CLIENT_ID missing", env: cfgEnv("OIDC_ISSUER", idpURL, "OIDC_CLIENT_SECRET", "ZZ-oidc-secret-2"),
			vars: []string{"OIDC_CLIENT_ID"}, values: []string{"ZZ-oidc-secret-2"},
		},
		{
			name: "OIDC_CLIENT_SECRET missing", env: cfgEnv("OIDC_ISSUER", idpURL, "OIDC_CLIENT_ID", "cid"),
			vars: []string{"OIDC_CLIENT_SECRET"},
		},
		{
			name: "OIDC_ISSUER is not a URL", env: cfgEnv("OIDC_ISSUER", "ZZ-not-a-url", "OIDC_CLIENT_ID", "cid", "OIDC_CLIENT_SECRET", "ZZ-oidc-secret-3"),
			vars: []string{"OIDC_ISSUER"}, values: []string{"ZZ-not-a-url", "ZZ-oidc-secret-3"},
		},
		// FINDING: OIDC_SCOPES is validated even when OIDC is not configured.
		{
			name: "OIDC_SCOPES without openid", env: cfgEnv("OIDC_SCOPES", "profile,ZZ-scope"),
			vars: []string{"OIDC_SCOPES"}, values: []string{"ZZ-scope"},
		},

		// --- NAME and NAME_FILE ---
		// FINDING: the message names SECRETS_KEY_FILE and both variables in its text, but not the path
		// (CLAUDE.md: "errors name the variable and the path").
		{
			name: "SECRETS_KEY together with SECRETS_KEY_FILE", env: cfgEnv("SECRETS_KEY_FILE", "/tmp/ZZ-both-key"),
			vars: []string{"SECRETS_KEY_FILE"}, values: []string{key},
		},
		{
			name: "PGPASSWORD together with PGPASSWORD_FILE", env: cfgEnv("PGPASSWORD", "ZZ-pg-secret", "PGPASSWORD_FILE", "/tmp/ZZ-both-pw"),
			vars: []string{"PGPASSWORD_FILE"}, values: []string{"ZZ-pg-secret"},
		},
		{
			name: "unreadable files", env: cfgEnv("SECRETS_KEY", "", "SECRETS_KEY_FILE", "/run/ZZ-missing/key", "PGPASSWORD_FILE", "/run/ZZ-missing/pw"),
			vars: []string{"SECRETS_KEY_FILE", "PGPASSWORD_FILE"}, paths: []string{"/run/ZZ-missing/key", "/run/ZZ-missing/pw"},
		},
		{
			name: "empty files", env: cfgEnv("SECRETS_KEY", "", "SECRETS_KEY_FILE", "/tmp/config-empty-key", "PGPASSWORD_FILE", "/tmp/config-blank-pw"),
			vars: []string{"SECRETS_KEY_FILE", "PGPASSWORD_FILE"}, paths: []string{"/tmp/config-empty-key", "/tmp/config-blank-pw"},
		},
		{
			name: "a key file that holds no key", env: cfgEnv("SECRETS_KEY", "", "SECRETS_KEY_FILE", "/tmp/config-bad-key"),
			vars: []string{"SECRETS_KEY_FILE"}, paths: []string{"/tmp/config-bad-key"}, values: []string{"ZZ-key-file-content"},
		},

		// --- the secrets key ---
		{
			name: "SECRETS_KEY is not base64", env: cfgEnv("SECRETS_KEY", "ZZ-not-base64!"),
			vars: []string{"SECRETS_KEY"}, values: []string{"ZZ-not-base64!"},
		},
		{
			name: "SECRETS_KEY is 31 bytes", env: cfgEnv("SECRETS_KEY", configBase64(t, 31)),
			vars: []string{"SECRETS_KEY"},
		},
		{
			name: "SECRETS_KEY_OLD equals SECRETS_KEY", env: cfgEnv("SECRETS_KEY_OLD", key),
			vars: []string{"SECRETS_KEY_OLD"},
		},
		{
			name: "SECRETS_KEY_OLD is not a key", env: cfgEnv("SECRETS_KEY_OLD", "ZZ-old-key"),
			vars: []string{"SECRETS_KEY_OLD"}, values: []string{"ZZ-old-key"},
		},

		// --- URLs, enums, booleans ---
		{
			name: "PUBLIC_URL is not a URL", env: cfgEnv("PUBLIC_URL", "ZZ-not-a-url"),
			vars: []string{"PUBLIC_URL"}, values: []string{"ZZ-not-a-url"},
		},
		{
			name: "DATABASE_URL is unset", env: cfgEnv("DATABASE_URL", ""),
			vars: []string{"DATABASE_URL"},
		},
		{
			name: "EMBEDDER is not a known one", env: cfgEnv("EMBEDDER", "ZZ-embedder"),
			vars: []string{"EMBEDDER"}, values: []string{"ZZ-embedder"},
		},
		{
			name: "EMBEDDER_URL is not a URL", env: cfgEnv("EMBEDDER", "openai-compatible", "EMBEDDER_MODEL", "m", "EMBEDDER_URL", "ZZ-embedder-url"),
			vars: []string{"EMBEDDER_URL"}, values: []string{"ZZ-embedder-url"},
		},
		{
			name: "TRUST_PROXY is not true or false", env: cfgEnv("TRUST_PROXY", "ZZ-yes"),
			vars: []string{"TRUST_PROXY"}, values: []string{"ZZ-yes"},
		},
		{
			name: "LOG_LEVEL is not a level", env: cfgEnv("LOG_LEVEL", "ZZ-loud"),
			vars: []string{"LOG_LEVEL"}, values: []string{"ZZ-loud"},
		},

		// --- numbers out of range or not numbers ---
		{name: "PORT is 0", env: cfgEnv("PORT", "0"), vars: []string{"PORT"}},
		{name: "PORT is not a number", env: cfgEnv("PORT", "ZZ99"), vars: []string{"PORT"}, values: []string{"ZZ99"}},
		{name: "PORT is above 65535", env: cfgEnv("PORT", "65536"), vars: []string{"PORT"}},
		{name: "TRUST_PROXY_HOPS is 9", env: cfgEnv("TRUST_PROXY_HOPS", "9"), vars: []string{"TRUST_PROXY_HOPS"}},
		{name: "TRUST_PROXY_HOPS is 0", env: cfgEnv("TRUST_PROXY_HOPS", "0"), vars: []string{"TRUST_PROXY_HOPS"}},
		{name: "SEARCH_MIN_SIMILARITY is 2", env: cfgEnv("SEARCH_MIN_SIMILARITY", "2"), vars: []string{"SEARCH_MIN_SIMILARITY"}},
		{name: "SEARCH_MIN_SIMILARITY is negative", env: cfgEnv("SEARCH_MIN_SIMILARITY", "-0.5"), vars: []string{"SEARCH_MIN_SIMILARITY"}},
		{name: "CALL_LOG_RETENTION_DAYS is 0", env: cfgEnv("CALL_LOG_RETENTION_DAYS", "0"), vars: []string{"CALL_LOG_RETENTION_DAYS"}},
		{name: "SYNC_INTERVAL_HOURS is not a number", env: cfgEnv("SYNC_INTERVAL_HOURS", "ZZ6h"), vars: []string{"SYNC_INTERVAL_HOURS"}, values: []string{"ZZ6h"}},
		{
			name: "several at once are all named", env: cfgEnv("PORT", "0", "TRUST_PROXY_HOPS", "9", "SEARCH_MIN_SIMILARITY", "2"),
			vars: []string{"PORT", "TRUST_PROXY_HOPS", "SEARCH_MIN_SIMILARITY"},
		},

		// FINDING: a missing EMBEDDER_URL / EMBEDDER_MODEL is not a configuration error report: the
		// image logs `starting` and then dies on an unhandled exception with a stack trace (exit 1).
		// The text names both variables. The report prefix is not pinned here.
		{
			name: "EMBEDDER=openai-compatible without URL and model", env: cfgEnv("EMBEDDER", "openai-compatible"),
			vars: []string{"EMBEDDER_URL", "EMBEDDER_MODEL"}, noPrefix: true,
		},
		{
			name: "EMBEDDER=openai-compatible without a model", env: cfgEnv("EMBEDDER", "openai-compatible", "EMBEDDER_URL", "http://embedder.compat.test/v1"),
			vars: []string{"EMBEDDER_MODEL"}, noPrefix: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, logs := s.SwapImageExpectingExit(t, image, "", c.env)
			if code != 1 {
				t.Fatalf("exit code %d, want 1; output:\n%s", code, s.Scrub(logs))
			}
			if !c.noPrefix && !strings.Contains(logs, "Invalid configuration:\n") {
				t.Errorf("the output has no `Invalid configuration:` line:\n%s", s.Scrub(logs))
			}
			for _, name := range c.vars {
				// A line that starts with the variable name, a colon and a reason; the reason's wording is
				// the validator's and is not pinned. Without the report prefix the name only has to appear.
				pattern := `(?m)^\s*` + regexp.QuoteMeta(name) + `: \S`
				if c.noPrefix {
					pattern = `\b` + regexp.QuoteMeta(name) + `\b`
				}
				if !regexp.MustCompile(pattern).MatchString(logs) {
					t.Errorf("the output does not name %s with a reason:\n%s", name, s.Scrub(logs))
				}
			}
			for _, path := range c.paths {
				if !strings.Contains(logs, path) {
					t.Errorf("the output does not name the path %s:\n%s", path, s.Scrub(logs))
				}
			}
			for _, v := range append(slices.Clone(c.values), s.SecretValues()...) {
				if strings.Contains(logs, v) {
					t.Errorf("the output contains a value that must not be printed (%d characters, starts %q)", len(v), harness.Redact(v))
				}
			}
			// The settings of this case do not exist for the next one (the override is for one start).
		})
	}

	// Control: the stack's own configuration is valid. Without it every refusal above could be a
	// stack that never starts.
	s.SwapImage(t, image, "")
	s.Answers(t)
}

func TestConfigEmptyMeansUnset(t *testing.T) {
	idp := fakes.StartIdP(t)
	// All three OIDC variables empty: unset, so the app starts without OIDC; an empty *_FILE twin
	// beside a set SECRETS_KEY / PGPASSWORD is unset too, and so is every optional URL and string.
	s := harness.Start(t, harness.Options{Env: map[string]string{
		"OIDC_ISSUER": "", "OIDC_CLIENT_ID": "", "OIDC_CLIENT_SECRET": "",
		"SECRETS_KEY_FILE": "", "PGPASSWORD_FILE": "", "SECRETS_KEY_OLD": "", "PGPASSWORD": "",
		"EMBEDDER_URL": "", "EMBEDDER_MODEL": "", "ALLOWED_HOSTS": "", "OIDC_SCOPES": "",
	}})
	methods := func() bool {
		var m struct {
			OIDC bool `json:"oidc"`
		}
		r := s.Anonymous().Do(t, "GET", "/api/auth/methods", nil)
		if r.Status != 200 {
			t.Fatalf("GET /api/auth/methods: %d %.200s", r.Status, r.Body)
		}
		r.JSON(t, "AuthMethods", &m)
		return m.OIDC
	}
	if methods() {
		t.Fatal("empty OIDC_* variables count as configured")
	}
	// Control: the same stack with the three set is a stack with OIDC.
	oidc := map[string]string{}
	oidc["OIDC_ISSUER"], oidc["OIDC_CLIENT_ID"], oidc["OIDC_CLIENT_SECRET"] = idp.OIDCSettings()
	s.SwapImage(t, harness.DefaultImage(), "", oidc)
	if !methods() {
		t.Fatal("control: methods.oidc is false with all three OIDC variables set")
	}
	// An empty value is unset for the "all three or none" rule as well: one set and two empty is partial.
	code, logs := harness.StartExpectingExit(t, harness.Options{Env: map[string]string{
		"OIDC_ISSUER": idp.Issuer, "OIDC_CLIENT_ID": "", "OIDC_CLIENT_SECRET": "",
	}})
	if code != 1 || !strings.Contains(logs, "Invalid configuration:\n") ||
		!regexp.MustCompile(`(?m)^\s*OIDC_CLIENT_ID: \S`).MatchString(logs) || !regexp.MustCompile(`(?m)^\s*OIDC_CLIENT_SECRET: \S`).MatchString(logs) {
		t.Fatalf("one OIDC variable set and two empty: exit %d, output:\n%s", code, logs)
	}
}

func TestConfigSecretsFromFiles(t *testing.T) {
	s := harness.Start(t, harness.Options{SecretsKeyFile: "/tmp/config-secrets-key", PasswordFile: "/tmp/config-pg-password"}) //nolint:gosec // G101: file paths in a container, not credentials
	led := newMCPLedger(t, s)
	admin := s.Admin(t)
	image := harness.DefaultImage()
	led.note(s.SecretValues()...)

	t.Run("the app is healthy and a sealed secret round-trips", func(t *testing.T) {
		s.Answers(t)
		secret := "config-file-key-" + harness.Unique("s")
		led.note(secret)
		e := gwNewServer(t, led, admin, nil, "shared_secret", fakes.WithSecret(secret))
		sess := led.session(admin.Token(t, "config-files"))
		res := led.call(sess, "call_tool", map[string]any{"tool": e.ref("echo"), "arguments": map[string]any{"text": "hi"}})
		if res.IsError {
			t.Fatalf("call_tool: %s", catalogText(res))
		}
		if c := e.up.Calls(); len(c) != 1 || c[0].Header.Get("Authorization") != "Bearer "+secret {
			t.Fatalf("the upstream got %d call(s) and a credential that is not the stored secret", len(c))
		}
		// The secret is in the database sealed, never as plain text.
		var sealed []byte
		if err := s.DB.QueryRow(t.Context(), "select secret from catalog.servers where slug = $1", e.slug).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if len(sealed) == 0 || strings.Contains(string(sealed), secret) {
			t.Fatalf("the stored secret is empty or plain (%d bytes)", len(sealed))
		}
	})

	t.Run("the operator CLI reads the password file too", func(t *testing.T) {
		if r := s.CLI(t, "", "create-user", "--sub", harness.Unique("filecli"), "--name", "File Cli"); r.Code != 0 {
			t.Fatalf("create-user: %v", r)
		}
		if r := s.CLIWithEnv(t, nil, "", "create-user", "--sub", harness.Unique("fileone"), "--name", "File One"); r.Code != 0 {
			t.Fatalf("create-user in a one-off container: %v", r)
		}
	})

	t.Run("the password really comes from the file: another file's password is refused by the database", func(t *testing.T) {
		const wrong = "ZZ-wrong-database-password"
		s.PutFile(t, "/tmp/config-wrong-pw", []byte(wrong+"\n"))
		code, logs := s.SwapImageExpectingExit(t, image, "", cfgEnv("PGPASSWORD_FILE", "/tmp/config-wrong-pw"))
		if code != 1 {
			t.Fatalf("exit code %d with a wrong password file, output:\n%s", code, s.Scrub(logs))
		}
		for _, v := range append([]string{wrong}, s.SecretValues()...) {
			if strings.Contains(logs, v) {
				t.Fatalf("the output contains a password (%d characters)", len(v))
			}
		}
		s.SwapImage(t, image, "")
	})

	t.Run("the key really comes from the file: another file's key is refused by the key check", func(t *testing.T) {
		other := configBase64(t, 32)
		s.PutFile(t, "/tmp/config-other-key", []byte(other+"\n"))
		code, logs := s.SwapImageExpectingExit(t, image, "", cfgEnv("SECRETS_KEY_FILE", "/tmp/config-other-key"))
		if code != 1 {
			t.Fatalf("exit code %d with another key file, output:\n%s", code, s.Scrub(logs))
		}
		recs, _ := logRecords(t, logs)
		if fatal := recordsAt(recs, "fatal"); len(fatal) != 1 || fatal[0]["msg"] != keyMismatchMsg {
			t.Fatalf("fatal records with another key: %v", fatal)
		}
		assertNoKey(t, logs, other)
		s.SwapImage(t, image, "")
		s.Answers(t)
		// The secret stored before still opens with the file's key.
		sess := led.session(admin.Token(t, "config-files-2"))
		if len(catalogListServers(t, sess)) == 0 {
			t.Fatal("no server after the restart")
		}
	})
}

// configXFFRecordIP sends GET /api/auth/methods (no database, no model: see Stack.Answers) with
// X-Forwarded-For and returns the "ip" of its access-log record (found by the request id the
// response carries).
func configXFFRecordIP(t *testing.T, s *harness.Stack, xff string) string {
	t.Helper()
	r := s.Anonymous().Do(t, "GET", "/api/auth/methods", nil, "X-Forwarded-For: "+xff)
	id := r.Header.Get("X-Request-Id")
	if r.Status != 200 || id == "" {
		t.Fatalf("GET /api/auth/methods: %d, X-Request-Id %q", r.Status, id)
	}
	var ip string
	harness.Eventually(t, "the access-log record of request "+id, func() bool {
		recs, _ := logRecords(t, s.Logs(t))
		for _, rec := range recs {
			if rec["reqId"] == id {
				ip, _ = rec["ip"].(string)
				return true
			}
		}
		return false
	})
	return ip
}

func TestConfigNetworkAndRetention(t *testing.T) {
	image := harness.DefaultImage()
	const port = 8123
	s := harness.Start(t, harness.Options{
		ContainerPort: port, // PORT: the app listens there and the harness publishes it
		Env: map[string]string{
			"ALLOWED_HOSTS":    "extra.compat.test, spare.compat.test", // comma-separated, spaces trimmed
			"TRUST_PROXY":      "true",
			"TRUST_PROXY_HOPS": "2",
		},
	})
	led := newMCPLedger(t, s)
	admin := s.Admin(t)
	token := admin.Token(t, "config-network")
	anon := s.Anonymous()
	listBody := mcpClassicBody(2, "tools/list", nil)

	t.Run("PORT: the app answers on the port it was told to listen on", func(t *testing.T) {
		// Start waited for /healthz on the published port, which maps to 8123 inside the container.
		s.Answers(t)
		if r := admin.Do(t, "GET", "/api/auth/me", nil); r.Status != 200 {
			t.Fatalf("the admin session on the other port: %d", r.Status)
		}
		if got := cliToolNames(t, led.session(token)); !slices.Equal(got, cliMetaTools) {
			t.Fatalf("tools on the other port: %v", got)
		}
	})

	t.Run("ALLOWED_HOSTS: a listed host is served on /mcp, an unlisted one is refused", func(t *testing.T) {
		for _, host := range []string{"extra.compat.test", "spare.compat.test"} {
			if r := mcpPost(t, led, anon, token, listBody, "Host: "+host); r.Status != 200 {
				t.Errorf("Host %s: %d %.200s, want 200", host, r.Status, r.Body)
			}
		}
		for _, host := range []string{"unlisted.compat.test", "extra.compat.test.evil.example", "xextra.compat.test"} {
			r := anon.Do(t, "POST", "/mcp", listBody, mcpJSON, mcpAccept, "Authorization: Bearer "+token, "Host: "+host)
			assertHostRefused(t, "Host "+host, r)
		}
		// The default hosts stay served: the public address and loopback.
		if r := mcpPost(t, led, anon, token, listBody, "Host: localhost"); r.Status != 200 {
			t.Errorf("Host localhost: %d", r.Status)
		}
	})

	t.Run("ALLOWED_HOSTS: without it the same host is refused (control)", func(t *testing.T) {
		s.SwapImage(t, image, "", map[string]string{"ALLOWED_HOSTS": ""})
		r := anon.Do(t, "POST", "/mcp", listBody, mcpJSON, mcpAccept, "Authorization: Bearer "+token, "Host: extra.compat.test")
		assertHostRefused(t, "Host extra.compat.test without ALLOWED_HOSTS", r)
		s.SwapImage(t, image, "")
	})

	t.Run("TRUST_PROXY_HOPS: the client is the entry that many hops from the right of X-Forwarded-For", func(t *testing.T) {
		const chain = "198.51.100.1, 198.51.100.2, 198.51.100.3"
		if ip := configXFFRecordIP(t, s, chain); ip != "198.51.100.2" {
			t.Errorf("with TRUST_PROXY_HOPS=2 the client of %q is %q, want 198.51.100.2", chain, ip)
		}
		s.SwapImage(t, image, "", map[string]string{"TRUST_PROXY_HOPS": "1"})
		if ip := configXFFRecordIP(t, s, chain); ip != "198.51.100.3" {
			t.Errorf("with TRUST_PROXY_HOPS=1 the client of %q is %q, want 198.51.100.3", chain, ip)
		}
		s.SwapImage(t, image, "", map[string]string{"TRUST_PROXY_HOPS": "3"})
		if ip := configXFFRecordIP(t, s, chain); ip != "198.51.100.1" {
			t.Errorf("with TRUST_PROXY_HOPS=3 the client of %q is %q, want 198.51.100.1", chain, ip)
		}
		s.SwapImage(t, image, "")
	})

	t.Run("CALL_LOG_RETENTION_DAYS: older call-log rows are pruned at start, newer ones stay", func(t *testing.T) {
		const server = "cfg-retention"
		userID := whoami(t, admin).UserID
		for _, c := range []struct{ tool, age string }{{"older", "49 hours"}, {"newer", "23 hours"}} {
			execSQL(t, s, `insert into gateway.calls (user_id, server, tool, started_at, duration_ms, outcome)
				values ($1::uuid, $2, $3, now() - $4::interval, 5, 'ok')`, userID, server, c.tool, c.age)
		}
		// Control: with the default (90 days) a restart prunes neither.
		s.SwapImage(t, image, "")
		catalogWaitDelivered(t, s)
		if n := countSQL(t, s, "select count(*) from gateway.calls where server = $1", server); n != 2 {
			t.Fatalf("%d rows after a restart with the default retention, want 2", n)
		}
		s.SwapImage(t, image, "", map[string]string{"CALL_LOG_RETENTION_DAYS": "1"})
		harness.Eventually(t, "the row older than one day to be pruned at start", func() bool {
			return countSQL(t, s, "select count(*) from gateway.calls where server = $1", server) == 1
		})
		var tool string
		if err := s.DB.QueryRow(t.Context(), "select tool from gateway.calls where server = $1", server).Scan(&tool); err != nil || tool != "newer" {
			t.Fatalf("the row left is %q (%v), want the one of 23 hours ago", tool, err)
		}
		s.SwapImage(t, image, "")
	})
}

func TestConfigOpenAICompatibleEmbedder(t *testing.T) {
	embedder := fakes.StartEmbedder(t)
	const model = "compat-e5-model"
	// The fake's vectors are a bag of words: a floor of 0.1 lets a query that shares one word with a
	// passage through (the real model's floor is calibrated for another embedder).
	s := harness.Start(t, harness.Options{Env: map[string]string{
		"EMBEDDER": "openai-compatible", "EMBEDDER_URL": embedder.URL, "EMBEDDER_MODEL": model,
		"SEARCH_MIN_SIMILARITY": "0.1",
	}})
	admin := s.Admin(t)
	token := admin.Token(t, "config-embedder")
	passageOf := func(slug, tool, desc string) string {
		return fmt.Sprintf("passage: %s. Compat test server %s\n%s\n%s", fakes.DisplayName(slug), slug, tool, desc)
	}
	storedVector := func(ref string) []float64 {
		var text *string
		if err := s.DB.QueryRow(t.Context(), "select embedding::text from search.documents where ref = $1", ref).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if text == nil {
			return nil
		}
		return parseVector(t, *text)
	}

	slug := harness.Unique("emb")
	tools := []fakes.Tool{
		{Name: "calibrate_flux", Description: "Calibrates the flux capacitor.", InputSchema: map[string]any{"type": "object"}},
		{Name: "brew_tea", Description: "Brews a pot of jasmine tea.", InputSchema: map[string]any{"type": "object"}},
		{Name: "paint_fence", Description: "Paints the garden fence white.", InputSchema: map[string]any{"type": "object"}},
	}
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: tools})
	catalogRegister(t, admin, fakes.ServerInput(up, slug, "none"))
	catalogWaitDocs(t, s, slug, 3)

	t.Run("indexing sends one passage: text per request, with the configured model", func(t *testing.T) {
		reqs := embedder.Requests()
		sent := map[string]bool{}
		for _, r := range reqs {
			if r.Model != model {
				t.Errorf("a request names the model %q, want %q", r.Model, model)
			}
			if len(r.Input) != 1 {
				t.Errorf("a request carries %d texts, want one document per call: %q", len(r.Input), r.Input)
				continue
			}
			if !strings.HasPrefix(r.Input[0], "passage: ") {
				t.Errorf("an indexing text has no passage: prefix: %.80q", r.Input[0])
			}
			sent[r.Input[0]] = true
		}
		for _, tool := range tools {
			want := passageOf(slug, tool.Name, tool.Description)
			if !sent[want] {
				t.Errorf("the embedder never got the passage of %s:\n%q", tool.Name, want)
			}
			// What the embedder answered is what is stored (pgvector keeps float4).
			got := storedVector(slug + "." + tool.Name)
			if problems := compareVectors(map[string][]float64{"v": got}, map[string][]float64{"v": fakes.EmbeddingOf(want)}, vectorTolerance); len(problems) != 0 {
				t.Errorf("the stored vector of %s is not the embedder's answer: %v", tool.Name, problems)
			}
		}
	})

	t.Run("a query is embedded with the query: prefix and the vector hits are returned", func(t *testing.T) {
		c := s.MCP(t, token)
		queries := func() []string {
			var out []string
			for _, r := range embedder.Requests() {
				for _, in := range r.Input {
					if strings.HasPrefix(in, "query: ") {
						out = append(out, in)
					}
				}
			}
			return out
		}
		before := len(queries())
		// "flux" is in one passage, "zzzunmatched" in none: the full-text query (all words) finds
		// nothing, so a hit can only come from the vectors.
		const q = "flux zzzunmatched"
		if n := countSQL(t, s, "select count(*) from search.documents where server = $1 and tsv @@ plainto_tsquery('simple', $2)", slug, q); n != 0 {
			t.Fatalf("control: %d documents match %q in full text", n, q)
		}
		hits := catalogHitRefs(catalogSearch(t, c, q, 5, []string{slug}))
		if len(hits) == 0 || hits[0] != slug+".calibrate_flux" {
			t.Fatalf("search %q: %v, want the flux tool first (a vector-only hit)", q, hits)
		}
		got := queries()
		if len(got) != before+1 || got[len(got)-1] != "query: "+q {
			t.Fatalf("query texts sent to the embedder: %q", got[before:])
		}
		// Control: a query that shares no word with any passage finds nothing.
		if hits := catalogHitRefs(catalogSearch(t, c, "zzzunmatched qqqnothing", 5, []string{slug})); len(hits) != 0 {
			t.Fatalf("a query with no common word finds %v", hits)
		}
	})

	t.Run("with the embedder down documents are stored without vectors and text search still works", func(t *testing.T) {
		embedder.SetDown(true)
		down := harness.Unique("embdown")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: []fakes.Tool{
			{Name: "tame_zebra", Description: "Tames a striped zebra.", InputSchema: map[string]any{"type": "object"}},
			{Name: "milk_gnu", Description: "Milks a patient gnu.", InputSchema: map[string]any{"type": "object"}},
		}})
		catalogRegister(t, admin, fakes.ServerInput(up, down, "none"))
		// The outbox events of the registration are not delivered while the embedder fails (the
		// worker retries them with a backoff), so delivery is not waited for here.
		harness.Eventually(t, "two documents without vectors", func() bool {
			var total, embedded int
			err := s.DB.QueryRow(t.Context(), "select count(*), count(embedding) from search.documents where server = $1", down).Scan(&total, &embedded)
			return err == nil && total == 2 && embedded == 0
		})
		c := s.MCP(t, token)
		if hits := catalogHitRefs(catalogSearch(t, c, "striped zebra", 5, []string{down})); !slices.Contains(hits, down+".tame_zebra") {
			t.Fatalf("text search with the embedder down: %v", hits)
		}
		// The embedder was asked and refused (the control that "down" was in effect).
		refused := 0
		for _, r := range embedder.Requests() {
			if r.Status == 503 {
				refused++
			}
		}
		if refused == 0 {
			t.Fatal("the embedder was never asked while it was down")
		}
		if n := countSQL(t, s, "select count(embedding) from search.documents where server = $1", down); n != 0 {
			t.Fatalf("%d documents got a vector from a down embedder", n)
		}

		// Back up: reindex fills the vectors in.
		embedder.SetDown(false)
		if r := s.CLI(t, "", "reindex"); r.Code != 0 {
			t.Fatalf("reindex: %v", r)
		}
		catalogWaitDocs(t, s, down, 2)
		want := fakes.EmbeddingOf(passageOf(down, "tame_zebra", "Tames a striped zebra."))
		got := storedVector(down + ".tame_zebra")
		if problems := compareVectors(map[string][]float64{"v": got}, map[string][]float64{"v": want}, vectorTolerance); len(problems) != 0 {
			t.Fatalf("the vector after the reindex: %v", problems)
		}
	})
}
