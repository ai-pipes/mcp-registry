package infra_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/infra"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/infra/oidctest"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

const (
	redirectURI = "http://gateway.test/api/auth/callback"
	verifier    = "verifier-0123456789-0123456789-0123456789"
	nonce       = "nonce-abc"
)

var _ app.OIDCProvider = (*infra.OIDCProvider)(nil)

type rig struct {
	idp  *oidctest.Provider
	prov *infra.OIDCProvider
	logs *bytes.Buffer
}

func newRig(t *testing.T, mutate func(*infra.OIDCConfig)) *rig {
	t.Helper()
	idp := oidctest.Start(t)
	buf := &bytes.Buffer{}
	cfg := infra.OIDCConfig{
		Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: idp.ClientSecret,
		RedirectURI: redirectURI, Logger: logging.New("trace", &lockedWriter{w: buf}),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return &rig{idp: idp, prov: infra.NewOIDCProvider(cfg), logs: buf}
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// code issues a code at the provider as if the browser step had happened.
func (r *rig) code() string {
	return r.idp.IssueCode(oidctest.Grant{RedirectURI: redirectURI, Nonce: nonce, CodeChallenge: oidctest.Challenge(verifier)})
}

func (r *rig) exchange(code string, iss *string) (domain.OIDCClaims, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.prov.Exchange(ctx, app.OIDCExchangeInput{Code: code, CodeVerifier: verifier, Nonce: nonce, Iss: iss})
}

func (r *rig) login(t *testing.T) domain.OIDCClaims {
	t.Helper()
	iss := r.idp.Issuer
	c, err := r.exchange(r.code(), &iss)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	return c
}

func strs(v ...string) []string { return v }

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func wantUnavailable(t *testing.T, err error) {
	t.Helper()
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeIDPUnavailable || de.Message != "the identity provider is unavailable" {
		t.Fatalf("want idp_unavailable, got %v", err)
	}
}

func wantRejected(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want a refusal, got success")
	}
	if _, ok := domain.AsError(err); ok {
		t.Fatalf("want an ordinary error, got a domain error: %v", err)
	}
}

func TestAuthorizationURL(t *testing.T) {
	r := newRig(t, nil)
	u, err := r.prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "st", CodeChallenge: "chal", Nonce: "nn"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.String(), r.idp.Issuer+"/authorize?") {
		t.Fatalf("url %s", u)
	}
	q := u.Query()
	want := map[string]string{
		"response_type": "code", "client_id": r.idp.ClientID, "redirect_uri": redirectURI,
		"scope": "openid profile email groups", "state": "st", "nonce": "nn",
		"code_challenge": "chal", "code_challenge_method": "S256",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q) != len(want) {
		t.Errorf("parameters %v, want exactly %d", q, len(want))
	}
	if strings.Contains(u.String(), r.idp.ClientSecret) || q.Has("client_secret") {
		t.Fatal("the client secret is in the authorization URL")
	}
}

func TestConfiguredScopes(t *testing.T) {
	r := newRig(t, func(c *infra.OIDCConfig) { c.Scopes = "openid email" })
	u, err := r.prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "s", CodeChallenge: "c", Nonce: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("scope"); got != "openid email" {
		t.Fatalf("scope %q", got)
	}
}

func TestLoginWithGroupsAsArray(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Name: "Anna", Email: "anna@example.test", Groups: strs("zeta", "alpha")})
	c := r.login(t)
	if c.Sub != "u1" || c.Name != "Anna" || c.Email == nil || *c.Email != "anna@example.test" || !equal(c.Groups, strs("zeta", "alpha")) {
		t.Fatalf("claims %+v", c)
	}
	if r.idp.Requests(oidctest.UserInfo) != 0 {
		t.Fatal("UserInfo was asked although the ID token had the groups")
	}
	form := r.idp.TokenForms()[0]
	if form.Get("client_secret") != r.idp.ClientSecret || form.Get("code_verifier") != verifier || form.Get("redirect_uri") != redirectURI {
		t.Fatalf("token request %v", form)
	}
	if len(r.idp.TokenForms()) != 1 {
		t.Fatalf("%d token requests, want 1 (no auth style probing)", len(r.idp.TokenForms()))
	}
}

func TestLoginWithGroupsAsObjectSorted(t *testing.T) {
	r := newRig(t, nil)
	// A fixed, unsorted wire order: sorting is ours, not an accident of the provider's encoder.
	r.idp.SetClaims(oidctest.Claims{
		Sub: "u1", Shape: oidctest.GroupsNone,
		IDTokenExtra: map[string]any{"groups": json.RawMessage(`{"zeta":{},"alpha":{},"mid":{},"beta":{}}`)},
	})
	if c := r.login(t); !equal(c.Groups, strs("alpha", "beta", "mid", "zeta")) {
		t.Fatalf("groups %v", c.Groups)
	}
}

func TestEmptyGroupArrayDoesNotAskUserInfo(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: []string{}})
	c := r.login(t)
	if c.Groups == nil || len(c.Groups) != 0 || r.idp.Requests(oidctest.UserInfo) != 0 {
		t.Fatalf("groups %#v, userinfo calls %d", c.Groups, r.idp.Requests(oidctest.UserInfo))
	}
}

func TestGroupsFallBackToUserInfo(t *testing.T) {
	for name, shape := range map[string]oidctest.GroupsShape{"array": oidctest.GroupsList, "object": oidctest.GroupsObject} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.idp.SetClaims(oidctest.Claims{Sub: "u1", Name: "From Info", Groups: strs("b", "a"), Shape: shape, Where: oidctest.InUserInfo})
			c := r.login(t)
			want := strs("b", "a")
			if shape == oidctest.GroupsObject {
				want = strs("a", "b")
			}
			if !equal(c.Groups, want) || r.idp.Requests(oidctest.UserInfo) != 1 {
				t.Fatalf("claims %+v, userinfo calls %d", c, r.idp.Requests(oidctest.UserInfo))
			}
		})
	}
}

func TestGroupClaimIsConfigurable(t *testing.T) {
	r := newRig(t, func(c *infra.OIDCConfig) { c.GroupsClaim = "roles" })
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: strs("staff"), GroupsClaim: "roles", Where: oidctest.InUserInfo})
	if c := r.login(t); !equal(c.Groups, strs("staff")) {
		t.Fatalf("groups %v", c.Groups)
	}
}

func TestNonStringGroupsAreNotGroups(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{
		Sub: "u1", Shape: oidctest.GroupsNone,
		IDTokenExtra:  map[string]any{"groups": []any{"a", 1}},
		UserInfoExtra: map[string]any{"groups": "a,b"},
	})
	c := r.login(t)
	if c.Groups == nil || len(c.Groups) != 0 || r.idp.Requests(oidctest.UserInfo) != 1 {
		t.Fatalf("groups %#v, userinfo calls %d", c.Groups, r.idp.Requests(oidctest.UserInfo))
	}
}

func TestNameAndEmailFallbacks(t *testing.T) {
	cases := []struct {
		name      string
		claims    oidctest.Claims
		wantName  string
		wantEmail *string
	}{
		{"name", oidctest.Claims{Sub: "s", Name: "N", PreferredUsername: "pu", Email: "e@x"}, "N", ptr("e@x")},
		{"preferred_username", oidctest.Claims{Sub: "s", PreferredUsername: "pu", Email: "e@x"}, "pu", ptr("e@x")},
		{"email", oidctest.Claims{Sub: "s", Email: "e@x"}, "e@x", ptr("e@x")},
		{"sub", oidctest.Claims{Sub: "s"}, "s", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			tc.claims.Groups = strs("g")
			r.idp.SetClaims(tc.claims)
			c := r.login(t)
			if c.Name != tc.wantName || (c.Email == nil) != (tc.wantEmail == nil) || (c.Email != nil && *c.Email != *tc.wantEmail) {
				t.Fatalf("claims %+v", c)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestUserInfoProfileWinsWhenFallingBack(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Name: "Info Name", Email: "i@x", Groups: strs("g"), Where: oidctest.InUserInfo})
	if c := r.login(t); c.Name != "Info Name" || c.Email == nil || *c.Email != "i@x" {
		t.Fatalf("claims %+v", c)
	}
}

func TestUserInfoFailureLogsOneWarnWithoutSecrets(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{Sub: "sub-nina", Name: "Nina", Email: "nina@example.test", Groups: strs("g"), Where: oidctest.InUserInfo})
	r.idp.SetFault(oidctest.UserInfo, oidctest.Fault500)
	c := r.login(t)
	if len(c.Groups) != 0 || c.Groups == nil {
		t.Fatalf("groups %#v", c.Groups)
	}
	recs := warns(t, r.logs)
	if len(recs) != 1 || recs[0]["msg"] != "oidc userinfo failed; treating the person as having no groups" || recs[0]["sub"] != "sub-nina" {
		t.Fatalf("records %v", recs)
	}
	line := r.logs.String()
	if strings.Contains(line, "access-") || strings.Contains(line, "nina@example.test") {
		t.Fatalf("log leaks: %s", line)
	}
}

func TestUserInfoWithoutGroupsLogsOneWarn(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Shape: oidctest.GroupsNone})
	if c := r.login(t); c.Groups == nil || len(c.Groups) != 0 {
		t.Fatalf("groups %#v", c.Groups)
	}
	if n := len(warns(t, r.logs)); n != 1 {
		t.Fatalf("%d warn records, want 1", n)
	}
}

func TestUserInfoForAnotherSubjectIsNotUsed(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{
		Sub: "u1", Groups: strs("g"), Where: oidctest.InUserInfo,
		UserInfoExtra: map[string]any{"sub": "someone-else"},
	})
	if c := r.login(t); len(c.Groups) != 0 {
		t.Fatalf("groups %v taken from another person's UserInfo", c.Groups)
	}
}

func warns(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		rec := parseJSON(t, line)
		if lvl, _ := rec["level"].(float64); lvl == 40 {
			out = append(out, rec)
		}
	}
	return out
}

func TestRefusedIDTokens(t *testing.T) {
	cases := map[string]oidctest.Tamper{
		"signed with another key": {WrongKey: true},
		"foreign issuer":          {Issuer: "http://evil.example"},
		"foreign audience":        {Audience: "another-client"},
		"foreign nonce":           {Nonce: "not-ours"},
		"expired":                 {Expired: true},
		"no id token":             {OmitIDToken: true},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.idp.SetTamper(tamper)
			iss := r.idp.Issuer
			_, err := r.exchange(r.code(), &iss)
			wantRejected(t, err)
		})
	}
}

func TestIssParameter(t *testing.T) {
	t.Run("wrong iss is refused before the code is spent", func(t *testing.T) {
		r := newRig(t, nil)
		bad := "http://evil.example"
		_, err := r.exchange(r.code(), &bad)
		wantRejected(t, err)
		if n := r.idp.Requests(oidctest.Token); n != 0 {
			t.Fatalf("%d token requests", n)
		}
	})
	t.Run("missing iss is refused when the provider supports the parameter", func(t *testing.T) {
		r := newRig(t, nil)
		_, err := r.exchange(r.code(), nil)
		wantRejected(t, err)
	})
	t.Run("missing iss is fine when the provider does not announce it", func(t *testing.T) {
		r := newRig(t, nil)
		r.idp.SetIssParamSupported(false)
		if _, err := r.exchange(r.code(), nil); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRejectedCodeIsAnOrdinaryError(t *testing.T) {
	r := newRig(t, nil)
	iss := r.idp.Issuer
	_, err := r.exchange("unknown-code", &iss)
	wantRejected(t, err)
	r.idp.SetFault(oidctest.Token, oidctest.Fault400JSON)
	_, err = r.exchange(r.code(), &iss)
	wantRejected(t, err)
}

func TestWrongPKCEVerifierIsRefused(t *testing.T) {
	r := newRig(t, nil)
	iss := r.idp.Issuer
	ctx := context.Background()
	_, err := r.prov.Exchange(ctx, app.OIDCExchangeInput{Code: r.code(), CodeVerifier: "another-verifier", Nonce: nonce, Iss: &iss})
	wantRejected(t, err)
}

func TestOutagesAreUnavailable(t *testing.T) {
	for name, fault := range map[string]struct {
		ep oidctest.Endpoint
		f  oidctest.Fault
	}{
		"token 500":  {oidctest.Token, oidctest.Fault500},
		"token drop": {oidctest.Token, oidctest.FaultDrop},
		"jwks 500":   {oidctest.JWKS, oidctest.Fault500},
		"jwks drop":  {oidctest.JWKS, oidctest.FaultDrop},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.idp.SetFault(fault.ep, fault.f)
			iss := r.idp.Issuer
			_, err := r.exchange(r.code(), &iss)
			wantUnavailable(t, err)
		})
	}
}

func TestRejectionsAreNotOutages(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetTamper(oidctest.Tamper{WrongKey: true})
	iss := r.idp.Issuer
	_, err := r.exchange(r.code(), &iss)
	wantRejected(t, err) // the JWKS answered fine; the signature is simply wrong
}

func TestDiscoveryIsLazyRetriedAndThenCached(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetFault(oidctest.Discovery, oidctest.Fault500)
	if n := r.idp.Requests(oidctest.Discovery); n != 0 {
		t.Fatalf("%d discovery requests at construction, want 0", n)
	}
	_, err := r.prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "s", CodeChallenge: "c", Nonce: "n"})
	wantUnavailable(t, err)
	iss := r.idp.Issuer
	_, err = r.exchange(r.code(), &iss)
	wantUnavailable(t, err)
	if n := r.idp.Requests(oidctest.Discovery); n != 2 {
		t.Fatalf("%d discovery requests, want 2 (a failure is not cached)", n)
	}
	recs := warns(t, r.logs)
	if len(recs) != 2 || recs[0]["msg"] != "oidc discovery failed" {
		t.Fatalf("records %v", recs)
	}
	if _, ok := recs[0]["err"].(map[string]any); !ok {
		t.Fatalf("err attribute %v", recs[0]["err"])
	}

	r.idp.SetFault(oidctest.Discovery, oidctest.FaultNone)
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: strs("g")})
	r.login(t) // the same object works now
	if _, err := r.prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "s", CodeChallenge: "c", Nonce: "n"}); err != nil {
		t.Fatal(err)
	}
	if n := r.idp.Requests(oidctest.Discovery); n != 3 {
		t.Fatalf("%d discovery requests, want 3 (success is cached)", n)
	}
}

func TestDiscoveryDroppedConnectionIsUnavailable(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetFault(oidctest.Discovery, oidctest.FaultDrop)
	_, err := r.prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "s", CodeChallenge: "c", Nonce: "n"})
	wantUnavailable(t, err)
}

func TestDiscoveryGivesUpAfterFiveSeconds(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetFault(oidctest.Discovery, oidctest.FaultHang)
	start := time.Now()
	_, err := r.prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "s", CodeChallenge: "c", Nonce: "n"})
	wantUnavailable(t, err)
	if d := time.Since(start); d < 4*time.Second || d > 8*time.Second {
		t.Fatalf("gave up after %v, want about 5s", d)
	}
}

func TestARealIssuerMismatchInMetadataIsUnavailable(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetMetadataIssuer("http://other.example")
	_, err := r.prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "s", CodeChallenge: "c", Nonce: "n"})
	wantUnavailable(t, err)
}

func TestConcurrentFirstUseDiscoversOnce(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: strs("g")})
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "s", CodeChallenge: "c", Nonce: "n"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := r.idp.Requests(oidctest.Discovery); n != 1 {
		t.Fatalf("%d discovery requests, want exactly 1", n)
	}
}

func TestACancelledCallerDoesNotPoisonTheSharedDiscovery(t *testing.T) {
	r := newRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.prov.AuthorizationURL(ctx, app.OIDCAuthorizationInput{State: "s", CodeChallenge: "c", Nonce: "n"}); err == nil {
		t.Fatal("a cancelled caller got a URL")
	}
	if _, err := r.prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "s", CodeChallenge: "c", Nonce: "n"}); err != nil {
		t.Fatalf("the next caller failed: %v", err)
	}
}

func TestNothingSecretIsLoggedOrReturned(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: strs("g"), Where: oidctest.InUserInfo})
	secrets := []string{r.idp.ClientSecret, verifier}
	var all []error
	iss := r.idp.Issuer

	// rejected code, token outage, bad signature, userinfo outage, discovery outage
	code := r.code()
	secrets = append(secrets, code)
	_, e := r.exchange(code+"x", &iss)
	all = append(all, e)
	r.idp.SetFault(oidctest.Token, oidctest.Fault500)
	_, e = r.exchange(r.code(), &iss)
	all = append(all, e)
	r.idp.SetFault(oidctest.Token, oidctest.FaultNone)
	r.idp.SetTamper(oidctest.Tamper{WrongKey: true})
	_, e = r.exchange(r.code(), &iss)
	all = append(all, e)
	r.idp.SetTamper(oidctest.Tamper{})
	r.idp.SetFault(oidctest.UserInfo, oidctest.Fault500)
	r.login(t)
	r.idp.SetFault(oidctest.UserInfo, oidctest.FaultNone)
	r.idp.SetFault(oidctest.Discovery, oidctest.Fault500)
	fresh := infra.NewOIDCProvider(infra.OIDCConfig{
		Issuer: r.idp.Issuer, ClientID: r.idp.ClientID, ClientSecret: r.idp.ClientSecret,
		RedirectURI: redirectURI, Logger: slog.New(slog.NewJSONHandler(&lockedWriter{w: r.logs}, nil)),
	})
	_, e = fresh.Exchange(context.Background(), app.OIDCExchangeInput{Code: code, CodeVerifier: verifier, Nonce: nonce, Iss: &iss})
	all = append(all, e)

	// access tokens and ID tokens as the provider issued them
	for _, f := range r.idp.TokenForms() {
		secrets = append(secrets, f.Get("code"))
	}
	hay := r.logs.String()
	for _, err := range all {
		if err != nil {
			hay += "\n" + err.Error()
		}
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(hay, s) {
			t.Fatalf("%q leaked into logs or errors:\n%s", s, hay)
		}
	}
	for _, marker := range []string{"access-", "eyJ"} {
		if strings.Contains(hay, marker) {
			t.Fatalf("token material (%s) leaked:\n%s", marker, hay)
		}
	}
}

func parseJSON(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("log line %q: %v", line, err)
	}
	return m
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	for _, ep := range []oidctest.Endpoint{oidctest.Discovery, oidctest.JWKS, oidctest.Token, oidctest.UserInfo} {
		t.Run(string(ep), func(t *testing.T) {
			var hits atomic.Int64
			var seen atomic.Value
			other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				body, _ := io.ReadAll(r.Body)
				seen.Store(string(body))
			}))
			defer other.Close()
			// The injected client follows redirects; the adapter must not trust it.
			r := newRig(t, func(c *infra.OIDCConfig) {
				c.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
			})
			r.idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: strs("g"), Where: oidctest.InUserInfo})
			r.idp.SetRedirect(ep, other.URL)
			iss := r.idp.Issuer
			_, err := r.exchange(r.code(), &iss)
			if ep == oidctest.UserInfo {
				if err != nil {
					t.Fatalf("a refused UserInfo is only a missing group list: %v", err)
				}
			} else if err == nil {
				t.Fatal("a redirect was accepted")
			}
			if n := hits.Load(); n != 0 {
				t.Fatalf("the redirect target received %d requests: %v", n, seen.Load())
			}
		})
	}
}

func TestIssuerDifferingOnlyByNormalisationIsAccepted(t *testing.T) {
	for name, tc := range map[string]struct{ configured, announced func(string) string }{
		"configured with a trailing slash": {func(i string) string { return i + "/" }, func(i string) string { return i }},
		"announced with a trailing slash":  {func(i string) string { return i }, func(i string) string { return i + "/" }},
	} {
		t.Run(name, func(t *testing.T) {
			idp := oidctest.Start(t)
			idp.SetMetadataIssuer(tc.announced(idp.Issuer))
			idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: strs("g")})
			prov := infra.NewOIDCProvider(infra.OIDCConfig{
				Issuer: tc.configured(idp.Issuer), ClientID: idp.ClientID,
				ClientSecret: idp.ClientSecret, RedirectURI: redirectURI,
			})
			u, err := prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "s", CodeChallenge: oidctest.Challenge(verifier), Nonce: nonce})
			if err != nil {
				t.Fatal(err)
			}
			// The whole flow, with the DISCOVERED issuer as the ID token's iss and the callback's iss.
			code, iss := browse(t, u)
			if iss != tc.announced(idp.Issuer) {
				t.Fatalf("callback iss %q", iss)
			}
			c, err := prov.Exchange(context.Background(), app.OIDCExchangeInput{Code: code, CodeVerifier: verifier, Nonce: nonce, Iss: &iss})
			if err != nil || c.Sub != "u1" {
				t.Fatalf("claims %+v, err %v", c, err)
			}
		})
	}
}

// browse plays the browser: the authorization request, and the code and iss of the redirect back.
func browse(t *testing.T, u *url.URL) (code, iss string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("no redirect (status %d): %v", resp.StatusCode, err)
	}
	return loc.Query().Get("code"), loc.Query().Get("iss")
}

func TestFullFlowThroughTheAuthorizeStep(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Name: "Anna", Groups: strs("g")})
	u, err := r.prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "st", CodeChallenge: oidctest.Challenge(verifier), Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	code, iss := browse(t, u)
	if code == "" || iss != r.idp.Issuer {
		t.Fatalf("code %q iss %q", code, iss)
	}
	c, err := r.exchange(code, &iss)
	if err != nil || c.Sub != "u1" || c.Name != "Anna" || !equal(c.Groups, strs("g")) {
		t.Fatalf("claims %+v err %v", c, err)
	}
}

// stubDiscovery serves a discovery document over TLS with the given endpoint scheme.
func stubDiscovery(t *testing.T, endpointScheme string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		host := strings.TrimPrefix(srv.URL, "https://")
		base := endpointScheme + "://" + host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": srv.URL, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
			"jwks_uri": base + "/jwks", "userinfo_endpoint": base + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"ES256"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnHTTPSIssuerRefusesInsecureEndpoints(t *testing.T) {
	for scheme, ok := range map[string]bool{"https": true, "http": false} {
		t.Run(scheme+" endpoints", func(t *testing.T) {
			srv := stubDiscovery(t, scheme)
			prov := infra.NewOIDCProvider(infra.OIDCConfig{
				Issuer: srv.URL, ClientID: "c", ClientSecret: "s",
				RedirectURI: redirectURI, HTTPClient: srv.Client(),
			})
			_, err := prov.AuthorizationURL(context.Background(), app.OIDCAuthorizationInput{State: "s", CodeChallenge: "c", Nonce: "n"})
			if ok && err != nil {
				t.Fatal(err)
			}
			if !ok {
				wantUnavailable(t, err)
			}
		})
	}
}

func TestAnHTTPIssuerKeepsWorkingWithHTTPEndpoints(t *testing.T) {
	r := newRig(t, nil) // the test provider is http everywhere
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: strs("g")})
	r.login(t)
}

func TestAudienceListNeedsAzp(t *testing.T) {
	cases := map[string]struct {
		tamper oidctest.Tamper
		ok     bool
	}{
		"two audiences, no azp":            {oidctest.Tamper{Audiences: []string{"mcp-gateway", "other"}}, false},
		"two audiences, foreign azp":       {oidctest.Tamper{Audiences: []string{"mcp-gateway", "other"}, Azp: "other"}, false},
		"two audiences, azp is the client": {oidctest.Tamper{Audiences: []string{"mcp-gateway", "other"}, Azp: "mcp-gateway"}, true},
		"one audience, no azp":             {oidctest.Tamper{Audiences: []string{"mcp-gateway"}}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: strs("g")})
			r.idp.SetTamper(tc.tamper)
			iss := r.idp.Issuer
			_, err := r.exchange(r.code(), &iss)
			if tc.ok && err != nil {
				t.Fatal(err)
			}
			if !tc.ok {
				wantRejected(t, err)
			}
		})
	}
}

func TestAnotherLoginsFailureDoesNotMakeThisRefusalAnOutage(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: strs("g"), Where: oidctest.InUserInfo})
	r.login(t) // warms the key set
	iss := r.idp.Issuer
	bad := oidctest.Tamper{WrongKey: true}
	badCode := r.idp.IssueCode(oidctest.Grant{RedirectURI: redirectURI, Nonce: nonce, CodeChallenge: oidctest.Challenge(verifier), Tamper: &bad})

	// The bad-signature login stops at the key set (it re-fetches the keys); meanwhile another login
	// meets a 5xx on UserInfo; then the key set answers.
	r.idp.SetFault(oidctest.JWKS, oidctest.FaultGate)
	r.idp.Drain()
	result := make(chan error, 1)
	go func() {
		_, err := r.exchange(badCode, &iss)
		result <- err
	}()
	if !r.idp.WaitFor(oidctest.JWKS, 10*time.Second) {
		t.Fatal("the bad-signature login never asked for the key set")
	}
	r.idp.SetFault(oidctest.UserInfo, oidctest.Fault500)
	r.login(t)
	if n := r.idp.Requests(oidctest.UserInfo); n == 0 {
		t.Fatal("the other login never met the 5xx")
	}
	r.idp.ReleaseGate()
	wantRejected(t, <-result)
}

func TestIDTokenWeaknessesAreRefused(t *testing.T) {
	cases := map[string]struct {
		tamper oidctest.Tamper
		claims oidctest.Claims
	}{
		"no nonce claim":        {oidctest.Tamper{OmitNonce: true}, oidctest.Claims{Sub: "u1", Groups: strs("g")}},
		"empty subject":         {oidctest.Tamper{}, oidctest.Claims{Sub: "", Groups: strs("g")}},
		"alg none":              {oidctest.Tamper{Alg: "none"}, oidctest.Claims{Sub: "u1", Groups: strs("g")}},
		"HS256 with our secret": {oidctest.Tamper{Alg: "HS256"}, oidctest.Claims{Sub: "u1", Groups: strs("g")}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			// The provider even advertises the weak algorithms.
			r.idp.SetAdvertisedAlgs("ES256", "HS256", "none")
			r.idp.SetClaims(tc.claims)
			r.idp.SetTamper(tc.tamper)
			iss := r.idp.Issuer
			_, err := r.exchange(r.code(), &iss)
			wantRejected(t, err)
		})
	}
}

func TestACancelledTokenRequestIsNotAnOutage(t *testing.T) {
	r := newRig(t, nil)
	r.idp.SetClaims(oidctest.Claims{Sub: "u1", Groups: strs("g")})
	r.login(t) // discovery done
	r.idp.SetFault(oidctest.Token, oidctest.FaultHang)
	r.idp.Drain()
	ctx, cancel := context.WithCancel(context.Background())
	iss := r.idp.Issuer
	result := make(chan error, 1)
	go func() {
		_, err := r.prov.Exchange(ctx, app.OIDCExchangeInput{Code: r.code(), CodeVerifier: verifier, Nonce: nonce, Iss: &iss})
		result <- err
	}()
	if !r.idp.WaitFor(oidctest.Token, 10*time.Second) {
		t.Fatal("the token request never arrived")
	}
	cancel()
	wantRejected(t, <-result)
	r.idp.SetFault(oidctest.Token, oidctest.FaultNone)
	r.login(t) // nothing was left behind
}
