package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	idapp "github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

// failingUsers is a user repository whose List fails with a text that quotes secrets.
type failingUsers struct {
	idapp.UserRepository
	called int
}

const leakyError = "secret-db-detail #link=ll_abc"

func (f *failingUsers) List(context.Context) ([]domain.User, error) {
	f.called++
	return nil, errors.New(leakyError)
}

func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, r)
	}
	return out
}

// bootstrap.ts: a failure is one error record with a fixed message and the neutral error fields;
// the text of the error (it may quote values) and any code never reach the log.
func TestFirstStartFailureIsOneErrorRecordWithoutTheErrorText(t *testing.T) {
	var buf bytes.Buffer
	users := &failingUsers{}
	w := identityWiring{
		rt:   Runtime{Config: config.Config{PublicURL: "http://localhost:3000"}, Logger: logging.New("info", &buf)},
		deps: idapp.Deps{Users: users},
	}
	w.firstStart(context.Background())

	recs := records(t, &buf)
	if len(recs) != 1 || recs[0]["msg"] != "first start: could not issue the sign-in link" || recs[0]["level"] != float64(50) {
		t.Fatalf("records: %v", recs)
	}
	if _, ok := recs[0]["err"]; !ok {
		t.Errorf("no err field: %v", recs[0])
	}
	if strings.Contains(buf.String(), "secret-db-detail") || strings.Contains(buf.String(), "ll_") || strings.Contains(buf.String(), "#link=") {
		t.Errorf("the record quotes the error text: %s", buf.String())
	}
}

// With OIDC configured nothing is created and nothing is printed: the store is not even asked.
func TestFirstStartDoesNothingWithOIDC(t *testing.T) {
	var buf bytes.Buffer
	users := &failingUsers{}
	cfg := config.Config{PublicURL: "http://localhost:3000", OIDC: &config.OIDCClient{Issuer: "https://idp.example"}}
	w := identityWiring{rt: Runtime{Config: cfg, Logger: logging.New("info", &buf)}, deps: idapp.Deps{Users: users}}
	w.firstStart(context.Background())
	if users.called != 0 || buf.Len() != 0 {
		t.Fatalf("store asked %d times, log %q", users.called, buf.String())
	}
}

// container.ts unconfiguredOidc: a plain error, not the identity module's idp_unavailable.
func TestUnconfiguredOIDCFailsWithAPlainError(t *testing.T) {
	p := unconfiguredOIDC{}
	_, err := p.AuthorizationURL(context.Background(), idapp.OIDCAuthorizationInput{})
	if err == nil || err.Error() != "OIDC provider is not configured" {
		t.Fatalf("AuthorizationURL: %v", err)
	}
	_, err = p.Exchange(context.Background(), idapp.OIDCExchangeInput{})
	if err == nil || err.Error() != "OIDC provider is not configured" {
		t.Fatalf("Exchange: %v", err)
	}
	if _, ok := domain.AsError(err); ok {
		t.Errorf("a domain error: %v", err)
	}
	if _, ok := kernel.AsDomainError(err); ok {
		t.Errorf("a kernel domain error: %v", err)
	}
}

// new URL(path, publicUrl).toString(): the path replaces any path of PUBLIC_URL; the host is
// lower-cased and converted to ASCII (punycode), a default port (read as a number) is dropped.
// Every expectation is the output of Node 22.21: new URL(path, base).toString().
func TestResolveURLMatchesTheURLConstructor(t *testing.T) {
	const cb = "/api/auth/callback"
	for _, tc := range []struct{ base, path, want string }{
		{"http://localhost:3000", cb, "http://localhost:3000/api/auth/callback"},
		{"http://localhost:3000/", "/mcp", "http://localhost:3000/mcp"},
		{"https://gw.example.com/registry/", "/mcp", "https://gw.example.com/mcp"},
		{"https://GW.Example.com:443/a/b?x=1#f", "/mcp", "https://gw.example.com/mcp"},
		{"http://gw.example.com:80", "/mcp", "http://gw.example.com/mcp"},
		{"http://gw.example.com:8080", "/mcp", "http://gw.example.com:8080/mcp"},
		{"https://gw.example.com:80", "/mcp", "https://gw.example.com:80/mcp"},
		{"http://[::1]:3000/x", "/mcp", "http://[::1]:3000/mcp"},
		{"https://gw.example.com:0443/x", cb, "https://gw.example.com/api/auth/callback"},
		{"http://gw.example.com:00080", cb, "http://gw.example.com/api/auth/callback"},
		{"http://gw.example.com:008080", cb, "http://gw.example.com:8080/api/auth/callback"},
		{"https://Bücher.example:443", cb, "https://xn--bcher-kva.example/api/auth/callback"},
		{"http://münchen.de:8080/a?b#c", cb, "http://xn--mnchen-3ya.de:8080/api/auth/callback"},
		{"http://[::1]:80", cb, "http://[::1]/api/auth/callback"},
		{"https://EXAMPLE.com.", cb, "https://example.com./api/auth/callback"},
		{"http://user:pw@host:80/p", cb, "http://user:pw@host/api/auth/callback"},
		{"https://xn--bcher-kva.example", cb, "https://xn--bcher-kva.example/api/auth/callback"},
	} {
		if got := resolveURL(tc.base, tc.path); got != tc.want {
			t.Errorf("resolveURL(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

// The rotation warning names the command as it is invoked in this image (plan decision: not
// `node dist/cli.js`).
func TestKeyRotationWarningNamesTheCommandOfThisImage(t *testing.T) {
	const want = "SECRETS_KEY_OLD is set: key rotation in progress. Run `mcp-registry rotate-secrets-key`, then remove SECRETS_KEY_OLD and restart"
	if msgKeyOldSet != want {
		t.Fatalf("%q", msgKeyOldSet)
	}
}

// The provider gets the redirect URI, scopes and groups claim of the configuration.
func TestOIDCConfigComesFromTheConfiguration(t *testing.T) {
	cfg := config.Config{
		PublicURL:  "https://gw.example.com:443/base/",
		OIDC:       &config.OIDCClient{Issuer: "https://idp.example", ClientID: "cid", ClientSecret: config.NewSecret("csecret")},
		OIDCScopes: "openid profile roles", OIDCGroupsClaim: "roles",
	}
	got := oidcConfigFor(cfg, nil)
	if got.Issuer != "https://idp.example" || got.ClientID != "cid" || got.ClientSecret.Reveal() != "csecret" ||
		got.RedirectURI != "https://gw.example.com/api/auth/callback" || got.Scopes != "openid profile roles" || got.GroupsClaim != "roles" {
		t.Errorf("%+v", got)
	}
}
