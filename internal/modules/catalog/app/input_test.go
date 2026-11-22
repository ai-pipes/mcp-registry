package app_test

import (
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

func paths(issues []app.Issue) []string {
	out := []string{}
	for _, i := range issues {
		out = append(out, i.Path)
	}
	return out
}

func oauthWith(mut func(*domain.OAuthClientConfig)) domain.AuthMode {
	a := authOAuth
	o := *a.OAuth
	mut(&o)
	a.OAuth = &o
	return a
}

// TestContractLimits: what the REST contract (zod) enforces before the domain sees a request.
// Each refusal is invalid_request with the message "<dotted.path>: <reason>".
func TestContractLimits(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name string
		mut  func(*app.ServerFields)
		path string // "" = accepted
	}{
		{"name empty", func(f *app.ServerFields) { f.Name = "" }, "name"},
		{"name 100", func(f *app.ServerFields) { f.Name = long(100) }, ""},
		{"name 101", func(f *app.ServerFields) { f.Name = long(101) }, "name"},
		// zod counts code points, not UTF-16 units: 100 emoji are 100.
		{"name 100 astral characters", func(f *app.ServerFields) { f.Name = strings.Repeat("\U0001F600", 100) }, ""},
		{"name 99 astral characters and a letter", func(f *app.ServerFields) { f.Name = strings.Repeat("\U0001F600", 99) + "a" }, ""},
		{"name 101 astral characters", func(f *app.ServerFields) { f.Name = strings.Repeat("\U0001F600", 101) }, "name"},
		{"description 500 astral characters", func(f *app.ServerFields) { f.Description = strings.Repeat("\U0001F600", 500) }, ""},
		{"description 501 astral characters", func(f *app.ServerFields) { f.Description = strings.Repeat("\U0001F600", 501) }, "description"},
		{"header 100 astral characters is refused by the domain, not the contract", func(f *app.ServerFields) { f.Auth = withHeader(strings.Repeat("\U0001F600", 100), "{secret}") }, ""},
		{"access group 200 astral characters", func(f *app.ServerFields) { f.Access = groups(strings.Repeat("\U0001F600", 200)) }, ""},
		{"access group 201 astral characters", func(f *app.ServerFields) { f.Access = groups(strings.Repeat("\U0001F600", 201)) }, "access.groups.0"},
		{"oauth extra param value 1000 astral characters", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) {
				o.ExtraAuthorizeParams = []domain.AuthorizeParam{{Key: "prompt", Value: strings.Repeat("\U0001F600", 1000)}}
			})
		}, ""},
		{"description 500", func(f *app.ServerFields) { f.Description = long(500) }, ""},
		{"description 501", func(f *app.ServerFields) { f.Description = long(501) }, "description"},
		{"url empty", func(f *app.ServerFields) { f.URL = "" }, "url"},
		{"url not a url", func(f *app.ServerFields) { f.URL = "nope" }, "url"},
		{"url ftp", func(f *app.ServerFields) { f.URL = "ftp://x.example.com/" }, "url"},
		{"url with one slash parses but is not http(s)://", func(f *app.ServerFields) { f.URL = "http:/x.example.com" }, "url"},
		{"url 2048", func(f *app.ServerFields) { f.URL = "https://x.example.com/" + long(2048-len("https://x.example.com/")) }, ""},
		// 2048 code points are 4096 UTF-16 units: counting units would refuse the first.
		{"url 2048 code points with astral characters", func(f *app.ServerFields) {
			f.URL = "https://x.example.com/" + strings.Repeat("\U0001F600", 2048-len("https://x.example.com/"))
		}, ""},
		{"url 2049 code points with astral characters", func(f *app.ServerFields) {
			f.URL = "https://x.example.com/" + strings.Repeat("\U0001F600", 2049-len("https://x.example.com/"))
		}, "url"},
		{"url 2049", func(f *app.ServerFields) { f.URL = "https://x.example.com/" + long(2049-len("https://x.example.com/")) }, "url"},
		{"timeout 999", func(f *app.ServerFields) { f.TimeoutMs = ptr(999) }, "timeoutMs"},
		{"timeout 1000", func(f *app.ServerFields) { f.TimeoutMs = ptr(1000) }, ""},
		{"timeout 600000", func(f *app.ServerFields) { f.TimeoutMs = ptr(600000) }, ""},
		{"timeout 600001", func(f *app.ServerFields) { f.TimeoutMs = ptr(600001) }, "timeoutMs"},
		{"daily limit 0", func(f *app.ServerFields) { f.DailyLimit = ptr(0) }, "dailyLimit"},
		{"daily limit 1", func(f *app.ServerFields) { f.DailyLimit = ptr(1) }, ""},
		{"daily limit 100000", func(f *app.ServerFields) { f.DailyLimit = ptr(100000) }, ""},
		{"daily limit 100001", func(f *app.ServerFields) { f.DailyLimit = ptr(100001) }, "dailyLimit"},
		{"access groups empty", func(f *app.ServerFields) { f.Access = domain.Access{Kind: domain.AccessGroups} }, "access.groups"},
		{"access group empty string", func(f *app.ServerFields) { f.Access = groups("dev", "") }, "access.groups.1"},
		{"access group 200", func(f *app.ServerFields) { f.Access = groups(long(200)) }, ""},
		{"access group 201", func(f *app.ServerFields) { f.Access = groups(long(201)) }, "access.groups.0"},
		{"access 100 groups", func(f *app.ServerFields) { f.Access = groups(numbered(100)...) }, ""},
		{"access 101 groups", func(f *app.ServerFields) { f.Access = groups(numbered(101)...) }, "access.groups"},
		{"header empty", func(f *app.ServerFields) { f.Auth = withHeader("", "{secret}") }, "auth.header"},
		{"header 101", func(f *app.ServerFields) { f.Auth = withHeader(long(101), "{secret}") }, "auth.header"},
		{"template empty", func(f *app.ServerFields) { f.Auth = withHeader("X-Key", "") }, "auth.template"},
		{"template 501", func(f *app.ServerFields) { f.Auth = withHeader("X-Key", "{secret}"+long(493)) }, "auth.template"},
		{"template 500", func(f *app.ServerFields) { f.Auth = withHeader("X-Key", "{secret}"+long(492)) }, ""},
		{"oauth authorize url ftp", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.AuthorizeURL = "ftp://git.example.com/a" })
		}, "auth.oauth.authorizeUrl"},
		{"oauth token url not a url", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.TokenURL = "nope" })
		}, "auth.oauth.tokenUrl"},
		{"oauth revoke url bad", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.RevokeURL = ptr("nope") })
		}, "auth.oauth.revokeUrl"},
		{"oauth revoke url fine", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.RevokeURL = ptr("https://git.example.com/revoke") })
		}, ""},
		{"oauth client id empty", func(f *app.ServerFields) { f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.ClientID = "" }) }, "auth.oauth.clientId"},
		{"oauth client id 501", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.ClientID = long(501) })
		}, "auth.oauth.clientId"},
		{"oauth client id blank passes the contract (the domain refuses it)", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.ClientID = "   " })
		}, ""},
		{"oauth scope empty string", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.Scopes = []string{"read", ""} })
		}, "auth.oauth.scopes.1"},
		{"oauth scope 201", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.Scopes = []string{long(201)} })
		}, "auth.oauth.scopes.0"},
		{"oauth 50 scopes", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.Scopes = numbered(50) })
		}, ""},
		{"oauth 51 scopes", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.Scopes = numbered(51) })
		}, "auth.oauth.scopes"},
		{"oauth extra param key 101", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) {
				o.ExtraAuthorizeParams = []domain.AuthorizeParam{{Key: long(101), Value: "x"}}
			})
		}, "auth.oauth.extraAuthorizeParams." + long(101)},
		{"oauth extra param key 100 astral characters", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) {
				o.ExtraAuthorizeParams = []domain.AuthorizeParam{{Key: strings.Repeat("\U0001F600", 100), Value: "x"}}
			})
		}, ""},
		{"oauth extra param key 101 astral characters", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) {
				o.ExtraAuthorizeParams = []domain.AuthorizeParam{{Key: strings.Repeat("\U0001F600", 101), Value: "x"}}
			})
		}, "auth.oauth.extraAuthorizeParams." + strings.Repeat("\U0001F600", 101)},
		{"oauth extra param value 1001", func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) {
				o.ExtraAuthorizeParams = []domain.AuthorizeParam{{Key: "prompt", Value: long(1001)}}
			})
		}, "auth.oauth.extraAuthorizeParams.prompt"},
		{"oauth block missing", func(f *app.ServerFields) { a := authOAuth; a.OAuth = nil; f.Auth = a }, "auth.oauth"},
		{"none ignores header and template", func(f *app.ServerFields) { f.Auth = domain.AuthMode{Kind: domain.AuthNone, Header: "", Template: ""} }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			_, err := app.CreateServer(ctx, h.deps, createCmd("gitea", nil, c.mut))
			if c.path == "" {
				if de := code(err); de == "invalid_request" {
					t.Fatalf("refused by the contract: %v", err)
				}
				return
			}
			wantCode(t, err, "invalid_request")
			if msg := err.Error(); !strings.HasPrefix(msg, c.path+": ") {
				t.Fatalf("message %q does not start with %q", msg, c.path+": ")
			}
			if len(h.servers.rows) != 0 || len(h.audit.entries) != 0 {
				t.Fatal("a refused request changed something")
			}
		})
	}
}

func numbered(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "g" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("0", i/26)
	}
	return out
}

func withHeader(header, template string) domain.AuthMode {
	return domain.AuthMode{Kind: domain.AuthSharedSecret, Header: header, Template: template}
}

func TestContractLimitsApplyToUpdateToo(t *testing.T) {
	h := newHarness()
	h.mustCreate(t, "gitea", nil)
	h.clean()
	for name, mut := range map[string]func(*app.ServerFields){
		"timeout": func(f *app.ServerFields) { f.TimeoutMs = ptr(10) },
		"name":    func(f *app.ServerFields) { f.Name = "" },
		"url":     func(f *app.ServerFields) { f.URL = "ftp://x" },
	} {
		_, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields(mut)})
		if code(err) != "invalid_request" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(h.servers.writes) != 0 {
		t.Fatal("written")
	}
}

func TestContractFirstIssueIsInSchemaOrder(t *testing.T) {
	bad := fields(func(f *app.ServerFields) {
		f.Name, f.URL, f.TimeoutMs, f.DailyLimit = "", "nope", ptr(1), ptr(0)
		f.Access = domain.Access{Kind: domain.AccessGroups}
	})
	issues := app.ValidateServerInput(app.ServerInput{Slug: "", Fields: bad})
	equalStrings(t, paths(issues), []string{"name", "url", "access.groups", "timeoutMs", "dailyLimit", "slug"})
	if got := issues[1].Error(); got != "url: must be an http(s) URL" {
		t.Fatalf("message = %q", got)
	}

	oauthBad := fields(func(f *app.ServerFields) {
		f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.AuthorizeURL, o.ClientID = "nope", "" })
		f.Auth.Header = ""
	})
	equalStrings(t, paths(app.ValidateServerInput(input("gitea", func(f *app.ServerFields) { *f = oauthBad }))),
		[]string{"auth.oauth.authorizeUrl", "auth.oauth.clientId", "auth.header"})

	h := newHarness()
	_, err := app.CreateServer(ctx, h.deps, app.CreateServerCommand{Principal: admin, Input: app.ServerInput{Slug: "gitea", Fields: bad}})
	if !strings.HasPrefix(err.Error(), "name: ") {
		t.Fatalf("err = %v", err)
	}
}

func TestSlugLimits(t *testing.T) {
	h := newHarness()
	for slug, want := range map[string]string{
		"":                      "invalid_request",
		strings.Repeat("a", 65): "invalid_request",
		strings.Repeat("a", 33): "invalid_slug", // the contract allows 1..64, the domain 2..32
		strings.Repeat("a", 64): "invalid_slug",
		"a":                     "invalid_slug",
		"Bad":                   "invalid_slug",
		strings.Repeat("a", 32): "ok",
		"with-dash":             "ok",
	} {
		_, err := app.CreateServer(ctx, h.deps, createCmd(slug, nil))
		if code(err) != want {
			t.Errorf("slug %q: %v, want %s", slug, err, want)
		}
	}
}

func TestSecretContractLimits(t *testing.T) {
	// REST create body: secret 1..4096, checked last in schema order.
	issues := app.ValidateCreateRequest(input("gitea"), ptr(""))
	equalStrings(t, paths(issues), []string{"secret"})
	equalStrings(t, paths(app.ValidateCreateRequest(input("gitea"), ptr(strings.Repeat("a", 4097)))), []string{"secret"})
	equalStrings(t, paths(app.ValidateCreateRequest(input("gitea"), ptr(strings.Repeat("a", 4096)))), []string{})
	equalStrings(t, paths(app.ValidateCreateRequest(input("gitea"), nil)), []string{})
	equalStrings(t, paths(app.ValidateCreateRequest(input("", func(f *app.ServerFields) { f.Name = "" }), ptr(""))), []string{"name", "slug", "secret"})

	// A CLI create does not go through the REST body contract: a blank secret reaches the domain.
	h := newHarness()
	_, err := app.CreateServer(ctx, h.deps, createCmd("gitea", ptr(strings.Repeat("a", 4097)), func(f *app.ServerFields) { f.Auth = authShared }))
	wantCode(t, err, "invalid_server")
}

func TestURLIsStoredCleaned(t *testing.T) {
	h := newHarness()
	_, err := app.CreateServer(ctx, h.deps, createCmd("gitea", nil, func(f *app.ServerFields) {
		f.URL = " \thttps://mcp.example.com/gi\ntea\r "
		f.Auth = authOAuth
		f.Auth.OAuth = &domain.OAuthClientConfig{ClientID: "cid", Scopes: []string{}, RevokeURL: ptr(" https://git.example.com/revoke ")}
		f.Auth.OAuth.AuthorizeURL = "\thttps://git.example.com/auth\n"
		f.Auth.OAuth.TokenURL = " https://git.example.com/to\nken "
	}))
	if err != nil {
		t.Fatal(err)
	}
	st := h.servers.row(t, "gitea")
	if st.URL != "https://mcp.example.com/gitea" {
		t.Fatalf("url = %q", st.URL)
	}
	o := st.Auth.OAuth
	if o.AuthorizeURL != "https://git.example.com/auth" || o.TokenURL != "https://git.example.com/token" || *o.RevokeURL != "https://git.example.com/revoke" {
		t.Fatalf("oauth = %+v", o)
	}

	// The same cleaning on update; the secret rule then compares the cleaned strings.
	h.mustCreate(t, "shared", ptr("k"), func(f *app.ServerFields) { f.Auth = authShared })
	if _, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "shared", Fields: fields(func(f *app.ServerFields) {
		f.Auth, f.URL = authShared, "  https://mcp.example.com/gitea\t"
	})}); err != nil {
		t.Fatal(err)
	}
	if h.servers.row(t, "shared").SealedSecret == nil || h.servers.row(t, "shared").URL != "https://mcp.example.com/gitea" {
		t.Fatalf("row = %+v", h.servers.row(t, "shared"))
	}
}

func TestDomainRulesKeepTheirCodesAndMessages(t *testing.T) {
	h := newHarness()
	cases := []struct {
		mut     func(*app.ServerFields)
		code    string
		message string
	}{
		{func(f *app.ServerFields) { f.Name = "   " }, "invalid_server", "server name must be 1-100 characters"},
		{func(f *app.ServerFields) { f.Auth = withHeader("X Key", "{secret}") }, "invalid_auth", "header name may contain letters, digits and dashes"},
		{func(f *app.ServerFields) { f.Auth = withHeader("X-Key", "no placeholder") }, "invalid_auth", "header template must contain {secret} and be a single line"},
		{func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.ClientID = "   " })
		}, "invalid_auth", "OAuth client id is required"},
		{func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.Scopes = []string{"OpenID"} })
		}, "invalid_auth", "the openid scope is not supported for upstream OAuth"},
		{func(f *app.ServerFields) {
			f.Auth = oauthWith(func(o *domain.OAuthClientConfig) {
				o.ExtraAuthorizeParams = []domain.AuthorizeParam{{Key: "state", Value: "x"}}
			})
		}, "invalid_auth", `extra authorize parameter "state" is not allowed`},
		{func(f *app.ServerFields) { f.Access = groups("  ") }, "invalid_access", "list at least one non-empty group"},
	}
	for _, c := range cases {
		_, err := app.CreateServer(ctx, h.deps, createCmd("gitea", nil, c.mut))
		wantError(t, err, c.code, c.message)
	}
}

func TestIssueTextAndFieldsValidation(t *testing.T) {
	if got := (app.Issue{Message: "body is wrong"}).Error(); got != "body is wrong" {
		t.Fatalf("got %q", got)
	}
	equalStrings(t, paths(app.ValidateServerFields(fields(func(f *app.ServerFields) { f.Name = ""; f.TimeoutMs = ptr(1) }))), []string{"name", "timeoutMs"})
	equalStrings(t, paths(app.ValidateServerFields(fields())), []string{})
}

type failingBox struct{ fakeBox }

func (failingBox) Seal(string) ([]byte, error) { return nil, errBoom{} }

func TestSealFailureIsReturnedAndNothingIsWritten(t *testing.T) {
	h := newHarness()
	h.deps.Box = failingBox{}
	_, err := app.CreateServer(ctx, h.deps, createCmd("gitea", ptr("k"), func(f *app.ServerFields) { f.Auth = authShared }))
	if err != (errBoom{}) {
		t.Fatalf("err = %v", err)
	}
	if len(h.servers.rows) != 0 {
		t.Fatal("created")
	}
}

// TestAnOversizedListReportsOnlyItself: a failed max of an array skips the checks of its
// elements (zod), so the first issue is the array's, not that of an empty element.
func TestAnOversizedListReportsOnlyItself(t *testing.T) {
	scopes := append(numbered(50), "")
	f := fields(func(f *app.ServerFields) {
		f.Auth = oauthWith(func(o *domain.OAuthClientConfig) { o.Scopes = scopes })
	})
	equalStrings(t, paths(app.ValidateServerFields(f)), []string{"auth.oauth.scopes"})
	g := fields(func(f *app.ServerFields) { f.Access = groups(append(numbered(100), long201())...) })
	equalStrings(t, paths(app.ValidateServerFields(g)), []string{"access.groups"})
	// Within the limit the elements are checked.
	h := fields(func(f *app.ServerFields) { f.Access = groups("a", "") })
	equalStrings(t, paths(app.ValidateServerFields(h)), []string{"access.groups.1"})
}

func long201() string { return strings.Repeat("a", 201) }

func TestSecretAndSlugCountCodePoints(t *testing.T) {
	if got := app.ValidateSecret(strings.Repeat("\U0001F600", 4096)); len(got) != 0 {
		t.Errorf("4096 emoji: %v", got)
	}
	if got := app.ValidateSecret(strings.Repeat("\U0001F600", 4097)); len(got) != 1 {
		t.Errorf("4097 emoji: %v", got)
	}
	in := app.ServerInput{Slug: strings.Repeat("\U0001F600", 64), Fields: fields()}
	if got := app.ValidateServerInput(in); len(got) != 0 {
		t.Errorf("64 emoji slug: %v", got)
	}
	in.Slug = strings.Repeat("\U0001F600", 65)
	if got := paths(app.ValidateServerInput(in)); len(got) != 1 || got[0] != "slug" {
		t.Errorf("65 emoji slug: %v", got)
	}
}
