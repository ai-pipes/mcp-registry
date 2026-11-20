// Package config loads and validates the process environment. It reads neither os.Environ nor
// files itself: the entry point passes the environment and a file reader.
package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	secretsKeyBytes                = 32
	defaultUpstreamMaxResponseSize = 20 * 1024 * 1024
	// DefaultMCPMaxBodyBytes: `/mcp` request bodies can carry files (base64), well above the REST 1 MiB.
	defaultMCPMaxBodyBytes = 10 * 1024 * 1024
	// DefaultSearchMinSimilarity is the calibrated cosine floor (see CLAUDE.md, "Search threshold").
	defaultSearchMinSimilarity = 0.83
	// A year: a longer synchronisation period is a typo.
	maxSyncIntervalHours = 24 * 365
	defaultOIDCScopes    = "openid profile email groups"
)

// OIDCClient is the gateway's client at the identity provider.
type OIDCClient struct{ Issuer, ClientID, ClientSecret string }

// Config is the validated configuration.
type Config struct {
	Port                     int
	DatabaseURL              string
	DatabasePassword         *string // from PGPASSWORD_FILE
	PublicURL                string
	AllowedHosts             []string
	TrustProxy               bool
	TrustProxyHops           int
	CookieSecure             bool
	SecretsKey               []byte // 32 bytes
	SecretsKeyOld            []byte // nil when unset
	OIDC                     *OIDCClient
	OIDCLoginGroup           string
	OIDCAdminGroup           string
	OIDCScopes               string
	OIDCGroupsClaim          string
	SyncIntervalHours        int
	CallLogRetentionDays     int
	UpstreamMaxResponseBytes int
	MCPMaxBodyBytes          int
	Embedder                 string // "local" | "openai-compatible"
	EmbedderURL              string
	EmbedderModel            string
	ModelDir                 string
	SearchMinSimilarity      float64
	LogLevel                 string
}

var (
	scopeSplit = regexp.MustCompile(`[\s,]+`)
	logLevels  = []string{"fatal", "error", "warn", "info", "debug", "trace", "silent"}
)

// Load validates env (an empty value counts as unset) and returns the configuration, or an *Error
// naming every bad variable in declaration order. readFile serves the NAME_FILE variables.
func Load(env map[string]string, readFile func(string) (string, error)) (Config, error) {
	set := make(map[string]string, len(env))
	for k, v := range env {
		if v != "" {
			set[k] = v
		}
	}
	var c issues
	var cfg Config

	cfg.Port = intVar(set, "PORT", 3000, 1, 65535, &c)
	cfg.DatabaseURL = requiredVar(set, "DATABASE_URL", &c)
	if pw, ok := fromFile(set, "PGPASSWORD", readFile, &c); ok {
		cfg.DatabasePassword = &pw
	}
	cfg.PublicURL = urlVar(set, "PUBLIC_URL", true, &c)
	for _, h := range strings.Split(set["ALLOWED_HOSTS"], ",") {
		if h = jsTrim(h); h != "" {
			cfg.AllowedHosts = append(cfg.AllowedHosts, h)
		}
	}
	cfg.TrustProxy = boolVar(set, "TRUST_PROXY", false, &c)
	cfg.TrustProxyHops = intVar(set, "TRUST_PROXY_HOPS", 1, 1, 5, &c)
	cfg.CookieSecure = boolVar(set, "COOKIE_SECURE", true, &c)

	loadKeys(set, readFile, &cfg, &c)
	loadOIDC(set, &cfg, &c)

	cfg.SyncIntervalHours = intVar(set, "SYNC_INTERVAL_HOURS", 6, 1, maxSyncIntervalHours, &c)
	cfg.CallLogRetentionDays = intVar(set, "CALL_LOG_RETENTION_DAYS", 90, 1, 0, &c)
	cfg.UpstreamMaxResponseBytes = intVar(set, "UPSTREAM_MAX_RESPONSE_BYTES", defaultUpstreamMaxResponseSize, 1, 0, &c)
	cfg.MCPMaxBodyBytes = intVar(set, "MCP_MAX_BODY_BYTES", defaultMCPMaxBodyBytes, 1, 0, &c)

	cfg.Embedder = enumVar(set, "EMBEDDER", "local", []string{"local", "openai-compatible"}, &c)
	cfg.EmbedderURL = urlVar(set, "EMBEDDER_URL", false, &c)
	cfg.EmbedderModel = set["EMBEDDER_MODEL"]
	if cfg.Embedder == "openai-compatible" {
		if cfg.EmbedderURL == "" && set["EMBEDDER_URL"] == "" {
			c.add("EMBEDDER_URL", "required when EMBEDDER is openai-compatible")
		}
		if cfg.EmbedderModel == "" {
			c.add("EMBEDDER_MODEL", "required when EMBEDDER is openai-compatible")
		}
	}
	cfg.ModelDir = stringVar(set, "MODEL_DIR", "/app/models")
	cfg.SearchMinSimilarity = floatVar(set, "SEARCH_MIN_SIMILARITY", defaultSearchMinSimilarity, 0, 1, &c)
	cfg.LogLevel = enumVar(set, "LOG_LEVEL", "info", logLevels, &c)

	if len(c.list) > 0 {
		return Config{}, &Error{Issues: c.list}
	}
	return cfg, nil
}

// loadKeys handles SECRETS_KEY (or SECRETS_KEY_FILE) and SECRETS_KEY_OLD.
func loadKeys(set map[string]string, readFile func(string) (string, error), cfg *Config, c *issues) {
	name, suffix := "SECRETS_KEY", ""
	raw, fromFileOK := fromFile(set, "SECRETS_KEY", readFile, c)
	_, hasFile := set["SECRETS_KEY_FILE"]
	switch {
	case fromFileOK:
		name, suffix = "SECRETS_KEY_FILE", " (the file "+set["SECRETS_KEY_FILE"]+")"
	case hasFile:
		// The file variable is already reported; do not add a second line about the key.
	default:
		v, ok := set["SECRETS_KEY"]
		if !ok {
			c.add("SECRETS_KEY", "required (or SECRETS_KEY_FILE); base64 of 32 bytes, e.g. openssl rand -base64 32")
		}
		raw = v
	}
	var key []byte
	if raw != "" {
		k, ok := decodeKey(raw)
		if !ok {
			c.add(name, "must be base64 of exactly 32 bytes (openssl rand -base64 32)"+suffix)
		} else {
			key, cfg.SecretsKey = k, k
		}
	}
	if old, ok := set["SECRETS_KEY_OLD"]; ok {
		k, valid := decodeKey(old)
		switch {
		case !valid:
			c.add("SECRETS_KEY_OLD", "must be base64 of exactly 32 bytes (openssl rand -base64 32)")
		case key != nil && bytes.Equal(k, key):
			c.add("SECRETS_KEY_OLD", "must differ from SECRETS_KEY")
		default:
			cfg.SecretsKeyOld = k
		}
	}
}

// decodeKey accepts only canonical padded base64 of exactly 32 bytes.
func decodeKey(s string) ([]byte, bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != secretsKeyBytes || base64.StdEncoding.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}

// loadOIDC: all of OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET or none; the group policy and
// the claim keep their defaults without a provider. OIDC_SCOPES must include openid only when at
// least one OIDC_* client variable is set (so one pass reports everything for a half-set provider).
func loadOIDC(set map[string]string, cfg *Config, c *issues) {
	names := []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET"}
	given := 0
	for _, n := range names {
		if _, ok := set[n]; ok {
			given++
		}
	}
	issuer := urlVar(set, "OIDC_ISSUER", false, c)
	if given > 0 && given < len(names) {
		for _, n := range names {
			if _, ok := set[n]; !ok {
				c.add(n, "required when another OIDC_* variable is set (all three or none)")
			}
		}
	}
	if given == len(names) && !hasIssue(c, "OIDC_ISSUER") {
		cfg.OIDC = &OIDCClient{Issuer: issuer, ClientID: set["OIDC_CLIENT_ID"], ClientSecret: set["OIDC_CLIENT_SECRET"]}
	}
	cfg.OIDCLoginGroup = stringVar(set, "OIDC_LOGIN_GROUP", "mcp-users")
	cfg.OIDCAdminGroup = stringVar(set, "OIDC_ADMIN_GROUP", "mcp-admins")

	var scopes []string
	for _, s := range scopeSplit.Split(stringVar(set, "OIDC_SCOPES", defaultOIDCScopes), -1) {
		if s != "" && !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	if given > 0 && !slices.Contains(scopes, "openid") {
		c.add("OIDC_SCOPES", "must include openid")
	}
	cfg.OIDCScopes = strings.Join(scopes, " ")
	cfg.OIDCGroupsClaim = stringVar(set, "OIDC_GROUPS_CLAIM", "groups")
}

func hasIssue(c *issues, name string) bool {
	return slices.ContainsFunc(c.list, func(i Issue) bool { return i.Name == name })
}

func stringVar(set map[string]string, name, fallback string) string {
	if v, ok := set[name]; ok {
		return v
	}
	return fallback
}

func requiredVar(set map[string]string, name string, c *issues) string {
	v, ok := set[name]
	if !ok {
		c.add(name, "required")
	}
	return v
}

// urlVar cleans the value as zod's z.url() does (JavaScript trim, then tab, CR and LF removed
// anywhere), stores the cleaned value, and requires a scheme, a host and a port of at most 65535.
// Requiring a host is deliberately stricter than the TypeScript server, which also accepts
// hostless forms (mailto:a@b, file:///x, http:example.com): they cannot give a working gateway.
// An unset optional variable yields "".
func urlVar(set map[string]string, name string, required bool, c *issues) string {
	v, ok := set[name]
	if !ok {
		if required {
			c.add(name, "required; an absolute URL such as https://example.com")
		}
		return ""
	}
	v = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(jsTrim(v))
	u, err := url.Parse(v)
	if err == nil && u.Scheme != "" && u.Hostname() != "" {
		if p := u.Port(); p == "" {
			return v
		} else if n, perr := strconv.Atoi(p); perr == nil && n <= 65535 {
			return v
		}
	}
	c.add(name, "must be an absolute URL with a scheme, a host and a port of at most 65535, such as https://example.com")
	return ""
}

func boolVar(set map[string]string, name string, fallback bool, c *issues) bool {
	switch set[name] {
	case "":
		return fallback
	case "true":
		return true
	case "false":
		return false
	}
	c.add(name, "must be true or false")
	return fallback
}

func enumVar(set map[string]string, name, fallback string, allowed []string, c *issues) string {
	v, ok := set[name]
	if !ok {
		return fallback
	}
	if !slices.Contains(allowed, v) {
		c.add(name, "must be one of: "+strings.Join(allowed, ", "))
		return fallback
	}
	return v
}

// intVar parses like z.coerce.number().int(): JavaScript Number() syntax, finite, a safe integer,
// then the bounds. maximum <= 0 means no upper bound.
func intVar(set map[string]string, name string, fallback, minimum, maximum int, c *issues) int {
	v, ok := set[name]
	if !ok {
		return fallback
	}
	f, valid := jsNumber(v)
	if !valid || f != math.Trunc(f) || math.Abs(f) > maxSafeInteger || f < float64(minimum) || (maximum > 0 && f > float64(maximum)) {
		if maximum > 0 {
			c.add(name, fmt.Sprintf("must be an integer from %d to %d", minimum, maximum))
		} else {
			c.add(name, fmt.Sprintf("must be an integer of at least %d", minimum))
		}
		return fallback
	}
	return int(f)
}

func floatVar(set map[string]string, name string, fallback, minimum, maximum float64, c *issues) float64 {
	v, ok := set[name]
	if !ok {
		return fallback
	}
	f, valid := jsNumber(v)
	if !valid || math.IsInf(f, 0) || f < minimum || f > maximum {
		c.add(name, fmt.Sprintf("must be a number from %g to %g", minimum, maximum))
		return fallback
	}
	return f
}
