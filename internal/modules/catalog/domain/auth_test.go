package domain_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

func baseOAuth() domain.OAuthClientConfig {
	o := domain.OAuthClientConfig{
		AuthorizeURL: "https://git.example.com/login/oauth/authorize",
		ClientID:     "abc",
		Scopes:       []string{"read:user"},
	}
	o.TokenURL = "https://git.example.com/login/oauth/access_token"
	return o
}

func oauthMode(o domain.OAuthClientConfig) domain.AuthMode {
	return domain.AuthMode{Kind: domain.AuthOAuthUser, OAuth: &o, Header: "Authorization", Template: "Bearer {secret}"}
}

func sharedMode(header, template string) domain.AuthMode {
	return domain.AuthMode{Kind: domain.AuthSharedSecret, Header: header, Template: template}
}

func patMode(header, template string) domain.AuthMode {
	return domain.AuthMode{Kind: domain.AuthPATUser, Header: header, Template: template}
}

func TestParseAuthNone(t *testing.T) {
	got, err := domain.ParseAuthMode(domain.AuthMode{Kind: domain.AuthNone, Header: "ignored", Template: "x"})
	if err != nil || !reflect.DeepEqual(got, domain.AuthMode{Kind: domain.AuthNone}) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestParseAuthTemplate(t *testing.T) {
	const msg = "header template must contain {secret} and be a single line"
	for _, m := range []domain.AuthMode{sharedMode("Authorization", "Bearer"), patMode("Authorization", "Bearer"), oauthMode(baseOAuth())} {
		m.Template = "Bearer"
		_, err := domain.ParseAuthMode(m)
		wantMsg(t, err, "invalid_auth", msg)
	}
	for _, tpl := range []string{"{secret}\nX: 1", "{secret}\r", "{secret}\x00", ""} {
		_, err := domain.ParseAuthMode(sharedMode("X-Key", tpl))
		wantMsg(t, err, "invalid_auth", msg)
	}
}

func TestParseAuthHeaderName(t *testing.T) {
	for _, h := range []string{"", "Bad Header", "X:Y", "X\nY", "X-Key\n", "é"} {
		_, err := domain.ParseAuthMode(sharedMode(h, "{secret}"))
		wantMsg(t, err, "invalid_auth", "header name may contain letters, digits and dashes")
	}
	// the header is checked before the OAuth config
	bad := baseOAuth()
	bad.AuthorizeURL = "nope"
	m := oauthMode(bad)
	m.Header = "a b"
	_, err := domain.ParseAuthMode(m)
	wantMsg(t, err, "invalid_auth", "header name may contain letters, digits and dashes")
	got, err := domain.ParseAuthMode(patMode("X-Key", "k={secret}"))
	if err != nil || got.Header != "X-Key" {
		t.Fatal(got, err)
	}
}

func TestParseAuthOAuth(t *testing.T) {
	if _, err := domain.ParseAuthMode(oauthMode(baseOAuth())); err != nil {
		t.Fatal(err)
	}
	urlMsg := "authorize and token URLs must be http(s) URLs"
	for _, mut := range []func(*domain.OAuthClientConfig){
		func(o *domain.OAuthClientConfig) { o.AuthorizeURL = "nope" },
		func(o *domain.OAuthClientConfig) { o.TokenURL = "ftp://x/y" },
	} {
		o := baseOAuth()
		mut(&o)
		_, err := domain.ParseAuthMode(oauthMode(o))
		wantMsg(t, err, "invalid_auth", urlMsg)
	}
	o := baseOAuth()
	o.ClientID = " "
	_, err := domain.ParseAuthMode(oauthMode(o))
	wantMsg(t, err, "invalid_auth", "OAuth client id is required")

	o = baseOAuth()
	o.RevokeURL = ptr("nope")
	_, err = domain.ParseAuthMode(oauthMode(o))
	wantMsg(t, err, "invalid_auth", "revoke URL must be an http(s) URL")
	o.RevokeURL = ptr("")
	_, err = domain.ParseAuthMode(oauthMode(o))
	wantMsg(t, err, "invalid_auth", "revoke URL must be an http(s) URL")
	o.RevokeURL = ptr("https://x.example/revoke")
	if _, err = domain.ParseAuthMode(oauthMode(o)); err != nil {
		t.Fatal(err)
	}

	_, err = domain.ParseAuthMode(domain.AuthMode{Kind: domain.AuthOAuthUser, Header: "A", Template: "{secret}"})
	wantMsg(t, err, "invalid_auth", urlMsg)
}

func TestParseAuthOAuthOrder(t *testing.T) {
	// URLs, then revoke, then client id, then openid, then extra params
	o := baseOAuth()
	o.AuthorizeURL = "nope"
	o.RevokeURL = ptr("nope")
	o.ClientID = ""
	o.Scopes = []string{"openid"}
	o.ExtraAuthorizeParams = map[string]string{"state": "x"}
	_, err := domain.ParseAuthMode(oauthMode(o))
	wantMsg(t, err, "invalid_auth", "authorize and token URLs must be http(s) URLs")
	o.AuthorizeURL = "https://a.example"
	_, err = domain.ParseAuthMode(oauthMode(o))
	wantMsg(t, err, "invalid_auth", "revoke URL must be an http(s) URL")
	o.RevokeURL = nil
	_, err = domain.ParseAuthMode(oauthMode(o))
	wantMsg(t, err, "invalid_auth", "OAuth client id is required")
	o.ClientID = "c"
	_, err = domain.ParseAuthMode(oauthMode(o))
	wantMsg(t, err, "invalid_auth", "the openid scope is not supported for upstream OAuth")
	o.Scopes = nil
	_, err = domain.ParseAuthMode(oauthMode(o))
	wantMsg(t, err, "invalid_auth", `extra authorize parameter "state" is not allowed`)
}

func TestParseAuthExtraParams(t *testing.T) {
	o := baseOAuth()
	o.ExtraAuthorizeParams = map[string]string{"access_type": "offline"}
	good := oauthMode(o)
	got, err := domain.ParseAuthMode(good)
	if err != nil || !reflect.DeepEqual(got, good) {
		t.Fatalf("%+v %v", got, err)
	}
	for _, key := range []string{"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method", "", "a b", "a=b", "é"} {
		o := baseOAuth()
		o.ExtraAuthorizeParams = map[string]string{key: "x"}
		_, err := domain.ParseAuthMode(oauthMode(o))
		wantMsg(t, err, "invalid_auth", `extra authorize parameter "`+key+`" is not allowed`)
	}
	for _, key := range []string{"a.b", "A_b-1", "STATE", "Scope"} {
		o := baseOAuth()
		o.ExtraAuthorizeParams = map[string]string{key: "x"}
		if _, err := domain.ParseAuthMode(oauthMode(o)); err != nil {
			t.Errorf("%q should be allowed (the check is case-sensitive): %v", key, err)
		}
	}
}

func TestParseAuthOpenIDScope(t *testing.T) {
	for _, scopes := range [][]string{{"openid"}, {"read", "openid"}, {"OpenID"}, {"OPENID"}} {
		o := baseOAuth()
		o.Scopes = scopes
		_, err := domain.ParseAuthMode(oauthMode(o))
		wantMsg(t, err, "invalid_auth", "the openid scope is not supported for upstream OAuth")
	}
	for _, scopes := range [][]string{{"openid-connect", "read"}, {" openid"}, {"OPENİD"}, {"openid "}} {
		o := baseOAuth()
		o.Scopes = scopes
		if _, err := domain.ParseAuthMode(oauthMode(o)); err != nil {
			t.Errorf("%q: %v (only an exact ASCII-case-insensitive match is refused)", scopes, err)
		}
	}
}

func TestParseAuthUnknownKind(t *testing.T) {
	_, err := domain.ParseAuthMode(domain.AuthMode{Kind: "basic"})
	wantMsg(t, err, "invalid_auth", "unknown auth mode")
}

func TestParseAuthCopiesInput(t *testing.T) {
	o := baseOAuth()
	o.ExtraAuthorizeParams = map[string]string{"access_type": "offline"}
	in := oauthMode(o)
	got, err := domain.ParseAuthMode(in)
	if err != nil {
		t.Fatal(err)
	}
	in.OAuth.Scopes[0] = "changed"
	in.OAuth.ExtraAuthorizeParams["access_type"] = "changed"
	if got.OAuth.Scopes[0] != "read:user" || got.OAuth.ExtraAuthorizeParams["access_type"] != "offline" {
		t.Fatal("the parsed mode shares memory with its input")
	}
}

func TestHeaderFor(t *testing.T) {
	h := domain.HeaderFor(sharedMode("X-API-Key", "{secret}"), "k")
	if !reflect.DeepEqual(h, kernel.UpstreamHeaders{"X-API-Key": "k"}) {
		t.Fatal(h)
	}
	h = domain.HeaderFor(patMode("Authorization", "Bearer {secret}"), "t")
	if !reflect.DeepEqual(h, kernel.UpstreamHeaders{"Authorization": "Bearer t"}) {
		t.Fatal(h)
	}
	h = domain.HeaderFor(oauthMode(baseOAuth()), "tok")
	if !reflect.DeepEqual(h, kernel.UpstreamHeaders{"Authorization": "Bearer tok"}) {
		t.Fatal(h)
	}
	// the secret is inserted literally, every occurrence of the placeholder is replaced
	h = domain.HeaderFor(sharedMode("H", "k={secret}&{secret}"), "a$&b$'{secret}")
	if h["H"] != "k=a$&b$'{secret}&a$&b$'{secret}" {
		t.Fatal(h["H"])
	}
	for _, m := range []domain.AuthMode{{Kind: domain.AuthNone}, {Kind: domain.AuthNone, Header: "X", Template: "{secret}"}} {
		h := domain.HeaderFor(m, "ignored")
		if h == nil || len(h) != 0 {
			t.Fatalf("none gives an empty non-nil map, got %#v", h)
		}
	}
	if strings.Contains(domain.HeaderFor(sharedMode("H", "{secret}"), "topsecret").String(), "topsecret") {
		t.Fatal("headers must redact themselves")
	}
}

func TestHoldsSecretAndCredentialSource(t *testing.T) {
	cases := []struct {
		mode   domain.AuthMode
		holds  bool
		source domain.CredentialSource
	}{
		{domain.AuthMode{Kind: domain.AuthNone}, false, domain.SourceNone},
		{sharedMode("X-Key", "{secret}"), true, domain.SourceServerSecret},
		{patMode("Authorization", "token {secret}"), false, domain.SourcePersonal},
		{oauthMode(baseOAuth()), true, domain.SourcePersonal},
	}
	for _, c := range cases {
		if c.mode.HoldsSecret() != c.holds || c.mode.CredentialSource() != c.source {
			t.Errorf("%s: holds=%v source=%v", c.mode.Kind, c.mode.HoldsSecret(), c.mode.CredentialSource())
		}
	}
	if (domain.AuthMode{Kind: "x"}).CredentialSource() != domain.SourceNone {
		t.Fatal("unknown kind has no credential")
	}
}
