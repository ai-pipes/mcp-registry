package domain_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

// Every expected string below was produced by node 22: JSON.stringify([url, authKind,
// authorizeUrl ?? null, tokenUrl ?? null, clientId ?? null]).
func TestServerFingerprintIsJSONStringifyOfFiveElements(t *testing.T) {
	ep := func(a, tk, c string) *domain.OAuthEndpoints {
		return &domain.OAuthEndpoints{AuthorizeURL: a, TokenURL: tk, ClientID: c}
	}
	for _, c := range []struct {
		name string
		in   domain.ServerConfigOrigin
		want string
	}{
		{"pat", domain.ServerConfigOrigin{URL: "https://x/mcp", AuthKind: "pat_user"}, `["https://x/mcp","pat_user",null,null,null]`},
		{
			"oauth",
			domain.ServerConfigOrigin{URL: "https://mcp.example.com/gitea", AuthKind: "oauth_user", OAuth: ep("https://git.example.com/a", "https://git.example.com/t", "cid")},
			`["https://mcp.example.com/gitea","oauth_user","https://git.example.com/a","https://git.example.com/t","cid"]`,
		},
		{"html characters are not escaped", domain.ServerConfigOrigin{URL: "https://h/mcp?a=1&b=<2>", AuthKind: "none"}, `["https://h/mcp?a=1&b=<2>","none",null,null,null]`},
		{"shared secret", domain.ServerConfigOrigin{URL: "https://h/mcp", AuthKind: "shared_secret"}, `["https://h/mcp","shared_secret",null,null,null]`},
		{
			"unicode, quotes, backslash, U+2028/2029 raw",
			domain.ServerConfigOrigin{URL: "https://h/ü/日本/😀", AuthKind: "oauth_user", OAuth: ep("https://i/a?x=\"q\"&y=\\z", "https://i/t\u2028\u2029", "c/id")},
			"[\"https://h/ü/日本/😀\",\"oauth_user\",\"https://i/a?x=\\\"q\\\"&y=\\\\z\",\"https://i/t\u2028\u2029\",\"c/id\"]",
		},
		{
			"control characters, empty string is not null, DEL raw",
			domain.ServerConfigOrigin{URL: "a\x00b\x01\x1f\b\f\n\r\t\x7f", AuthKind: "oauth_user", OAuth: ep("", "'", "é")},
			"[\"a\\u0000b\\u0001\\u001f\\b\\f\\n\\r\\t\x7f\",\"oauth_user\",\"\",\"'\",\"é\"]",
		},
		{"invalid UTF-8 becomes U+FFFD like a decoded JavaScript string", domain.ServerConfigOrigin{URL: "a\xffb", AuthKind: "none"}, "[\"a\ufffdb\",\"none\",null,null,null]"},
	} {
		if got := domain.ServerFingerprint(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestServerFingerprintCoversWhereTokensGoButNotRevokeURLOrScopes(t *testing.T) {
	base := domain.ServerConfigOrigin{URL: "https://mcp.example.com/gitea", AuthKind: "oauth_user", OAuth: &oauthCfg}
	if domain.ServerFingerprint(base) != fp {
		t.Fatal("unstable")
	}
	with := func(f func(c *domain.ServerConfigOrigin, o *domain.OAuthEndpoints)) string {
		c, o := base, oauthCfg
		c.OAuth = &o
		f(&c, &o)
		return domain.ServerFingerprint(c)
	}
	others := []string{
		with(func(c *domain.ServerConfigOrigin, _ *domain.OAuthEndpoints) { c.URL = "https://evil.example.com/mcp" }),
		with(func(c *domain.ServerConfigOrigin, _ *domain.OAuthEndpoints) { c.AuthKind = "pat_user"; c.OAuth = nil }),
		with(func(_ *domain.ServerConfigOrigin, o *domain.OAuthEndpoints) { o.AuthorizeURL = "https://x/a" }),
		with(func(_ *domain.ServerConfigOrigin, o *domain.OAuthEndpoints) { o.TokenURL = "https://evil/t" }),
		with(func(_ *domain.ServerConfigOrigin, o *domain.OAuthEndpoints) { o.ClientID = "other" }),
	}
	seen := map[string]bool{fp: true}
	for _, o := range others {
		seen[o] = true
	}
	if len(seen) != 6 {
		t.Fatal("each field must change the fingerprint")
	}
}
