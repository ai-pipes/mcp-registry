package http_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// zodPath is the "path: " prefix of a body error, as the compatibility suite reads it.
var zodPath = regexp.MustCompile(`^([\w.]+): `)

type golden struct {
	Status  int     `json:"status"`
	Code    string  `json:"code"`
	Path    string  `json:"path"`
	Message *string `json:"message"`
}

func goldenFile(t *testing.T, name string) golden {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "compat", "golden", "validation")
	raw, err := os.ReadFile(filepath.Join(dir, name+".json")) //nolint:gosec // G304: the name is a constant of this test, the directory the repository's golden files
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// asGolden turns a reply into what the compatibility suite records of it.
func asGolden(t *testing.T, r reply) golden {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.Body), &e); err != nil {
		t.Fatalf("not an error body: %q", r.Body)
	}
	g := golden{Status: r.Status, Code: e.Error.Code, Message: &e.Error.Message}
	if e.Error.Code == "invalid_request" && e.Error.Message != "body is not valid JSON" {
		if m := zodPath.FindStringSubmatch(e.Error.Message); m != nil {
			g.Path = m[1]
		}
		g.Message = nil
	}
	return g
}

func checkGolden(t *testing.T, name string, r reply) {
	t.Helper()
	got, want := asGolden(t, r), goldenFile(t, name)
	if got.Status != want.Status || got.Code != want.Code || got.Path != want.Path || (got.Message == nil) != (want.Message == nil) ||
		(got.Message != nil && *got.Message != *want.Message) {
		gm, wm := "<nil>", "<nil>"
		if got.Message != nil {
			gm = *got.Message
		}
		if want.Message != nil {
			wm = *want.Message
		}
		t.Fatalf("%s:\n got  %d %s path=%q message=%s (body %s)\n want %d %s path=%q message=%s", name,
			got.Status, got.Code, got.Path, gm, r.Body, want.Status, want.Code, want.Path, wm)
	}
}

func long(n int) string { return strings.Repeat("x", n) }

// bodyOf is the base body of a kind, as the compatibility suite builds it (fakes.ServerInput).
func bodyOf(kind string, mut ...func(map[string]any)) map[string]any {
	auth := map[string]any{"kind": kind}
	if kind != "none" {
		auth["header"] = "Authorization"
		auth["template"] = "Bearer {secret}"
	}
	b := validServer("val-one")
	b["auth"] = auth
	for _, m := range mut {
		m(b)
	}
	return b
}

func set(key string, v any) func(map[string]any) {
	return func(b map[string]any) { b[key] = v }
}

func authSet(key string, v any) func(map[string]any) {
	return func(b map[string]any) { b["auth"].(map[string]any)[key] = v }
}

func oauth(mut func(map[string]any)) func(map[string]any) {
	return func(b map[string]any) {
		a := b["auth"].(map[string]any)
		a["kind"] = "oauth_user"
		o := map[string]any{ //nolint:gosec // G101: made-up OAuth endpoints of a test server, not credentials
			"authorizeUrl": "https://git.example.com/authorize", "tokenUrl": "https://git.example.com/token",
			"clientId": "cid", "scopes": []string{"read"},
		}
		if mut != nil {
			mut(o)
		}
		a["oauth"] = o
	}
}

func extra(key string) func(map[string]any) {
	return oauth(func(o map[string]any) { o["extraAuthorizeParams"] = map[string]string{key: "x"} })
}

func groupsOf(g ...string) func(map[string]any) {
	if g == nil {
		g = []string{}
	}
	return set("access", map[string]any{"kind": "groups", "groups": g})
}

// TestCreateValidationGoldens replays every create case of the compatibility suite against the
// route table and compares status, code and path (and the exact message of every refusal that is
// not zod's) with the golden recorded from the reference server.
func TestCreateValidationGoldens(t *testing.T) {
	cases := []struct {
		name string
		body any
	}{
		{"slug_uppercase_and_space", bodyOf("none", set("slug", "Bad Slug"))},
		{"slug_empty", bodyOf("none", set("slug", ""))},
		{"slug_too_short", bodyOf("none", set("slug", "a"))},
		{"slug_starts_with_digit", bodyOf("none", set("slug", "1abc"))},
		{"slug_33_characters", bodyOf("none", set("slug", "a"+long(32)))},
		{"slug_too_long", bodyOf("none", set("slug", "a"+long(64)))},
		{"slug_underscore", bodyOf("none", set("slug", "my_server"))},
		{"name_empty", bodyOf("none", set("name", ""))},
		{"name_too_long", bodyOf("none", set("name", long(101)))},
		{"description_too_long", bodyOf("none", set("description", long(501)))},
		{"url_ftp", bodyOf("none", set("url", "ftp://example.com/mcp"))},
		{"url_not_a_url", bodyOf("none", set("url", "not a url"))},
		{"url_empty", bodyOf("none", set("url", ""))},
		{"url_too_long", bodyOf("none", set("url", "http://example.com/"+long(2048)))},
		{"auth_unknown_kind", bodyOf("shared_secret", authSet("kind", "magic"))},
		{"auth_missing", bodyOf("none", set("auth", nil))},
		{"oauth_without_oauth_block", bodyOf("shared_secret", authSet("kind", "oauth_user"))},
		{"oauth_client_id_empty", bodyOf("oauth_user", oauth(func(o map[string]any) { o["clientId"] = "" }))},
		{"oauth_client_id_blank", bodyOf("oauth_user", oauth(func(o map[string]any) { o["clientId"] = "   " }))},
		{"oauth_authorize_url_ftp", bodyOf("oauth_user", oauth(func(o map[string]any) { o["authorizeUrl"] = "ftp://x.example/a" }))},
		{"oauth_token_url_not_a_url", bodyOf("oauth_user", oauth(func(o map[string]any) { o["tokenUrl"] = "nope" }))},
		{"oauth_revoke_url_bad", bodyOf("oauth_user", oauth(func(o map[string]any) { o["revokeUrl"] = "nope" }))},
		{"oauth_scope_openid", bodyOf("oauth_user", oauth(func(o map[string]any) { o["scopes"] = []string{"read", "openid"} }))},
		{"oauth_scope_openid_uppercase", bodyOf("oauth_user", oauth(func(o map[string]any) { o["scopes"] = []string{"OpenID"} }))},
		{"oauth_scope_empty_string", bodyOf("oauth_user", oauth(func(o map[string]any) { o["scopes"] = []string{""} }))},
		{"oauth_extra_param_state", bodyOf("oauth_user", extra("state"))},
		{"oauth_extra_param_redirect_uri", bodyOf("oauth_user", extra("redirect_uri"))},
		{"oauth_extra_param_scope", bodyOf("oauth_user", extra("scope"))},
		{"oauth_extra_param_client_id", bodyOf("oauth_user", extra("client_id"))},
		{"oauth_extra_param_response_type", bodyOf("oauth_user", extra("response_type"))},
		{"oauth_extra_param_code_challenge", bodyOf("oauth_user", extra("code_challenge"))},
		{"oauth_extra_param_code_challenge_method", bodyOf("oauth_user", extra("code_challenge_method"))},
		{"oauth_extra_param_bad_name", bodyOf("oauth_user", extra("a b"))},
		{"access_groups_empty", bodyOf("none", groupsOf())},
		{"access_groups_blank_name", bodyOf("none", groupsOf(""))},
		{"access_unknown_kind", bodyOf("none", set("access", map[string]any{"kind": "some"}))},
		{"timeout_below_minimum", bodyOf("none", set("timeoutMs", 999))},
		{"timeout_above_maximum", bodyOf("none", set("timeoutMs", 600_001))},
		{"timeout_not_an_integer", bodyOf("none", set("timeoutMs", 1500.5))},
		{"timeout_not_a_number", bodyOf("none", set("timeoutMs", "abc"))},
		{"daily_limit_negative", bodyOf("none", set("dailyLimit", -1))},
		{"daily_limit_zero", bodyOf("none", set("dailyLimit", 0))},
		{"daily_limit_above_maximum", bodyOf("none", set("dailyLimit", 100_001))},
		{"daily_limit_not_a_number", bodyOf("none", set("dailyLimit", "ten"))},
		{"header_empty", bodyOf("shared_secret", authSet("header", ""))},
		{"header_with_space", bodyOf("shared_secret", authSet("header", "X Key"))},
		{"header_with_colon", bodyOf("shared_secret", authSet("header", "X:Key"))},
		{"header_too_long", bodyOf("shared_secret", authSet("header", long(101)))},
		{"template_empty", bodyOf("shared_secret", authSet("template", ""))},
		{"template_without_secret", bodyOf("shared_secret", authSet("template", "Bearer token"))},
		{"template_with_newline", bodyOf("shared_secret", authSet("template", "Bearer\n{secret}"))},
		{"template_pat_user_without_secret", bodyOf("pat_user", authSet("template", "Bearer"))},
		{"template_oauth_user_without_secret", bodyOf("oauth_user", oauth(nil), authSet("template", "Bearer"))},
		{"secret_empty", bodyOf("shared_secret", set("secret", ""))},
		{"secret_too_long", bodyOf("shared_secret", set("secret", long(4097)))},
		{"secret_not_a_string", bodyOf("shared_secret", set("secret", 5))},
		// Bodies that are not a server at all.
		{"body_not_json", "{bad"},
		{"body_empty_object", `{}`},
		{"body_missing_name", `{"slug":"abc"}`},
		{"body_array", `[]`},
		{"body_null", `null`},
		{"body_timeout_not_a_number", `{"slug":"abc","name":"x","description":"","url":"http://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"timeoutMs":"x"}`},
		{"body_access_missing", `{"slug":"abc","name":"x","description":"","url":"http://x.example","auth":{"kind":"none"}}`},
		{"body_description_missing", `{"slug":"abc","name":"x","url":"http://x.example","auth":{"kind":"none"},"access":{"kind":"all"}}`},
	}
	e := newEnv(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkGolden(t, "create_"+c.name, e.admin("POST", "/api/servers", c.body))
		})
	}
	if n := len(e.servers.rows); n != 0 {
		t.Fatalf("refused requests created %d server(s)", n)
	}
	if len(e.audit.actions()) != 0 {
		t.Fatalf("refused requests were audited: %v", e.audit.actions())
	}
}

// TestUpdateAndSmallBodyValidationGoldens: PUT, secret, status and hidden.
func TestUpdateAndSmallBodyValidationGoldens(t *testing.T) {
	e := newEnv(t)
	e.addServer("edit", nil, "a")
	put := func(mut func(map[string]any)) map[string]any {
		b := map[string]any{
			"name": "Gitea", "description": "Git hosting", "url": "https://mcp.example.com/gitea",
			"auth": map[string]any{"kind": "none"}, "access": map[string]any{"kind": "all"}, "timeoutMs": 120000, "dailyLimit": nil,
		}
		if mut != nil {
			mut(b)
		}
		return b
	}
	cases := []struct {
		name, golden, method, path string
		body                       any
	}{
		{"name_empty", "update_name_empty", "PUT", "/api/servers/edit", put(set("name", ""))},
		{"url_ftp", "update_url_ftp", "PUT", "/api/servers/edit", put(set("url", "ftp://example.com"))},
		{"auth_unknown_kind", "update_auth_unknown_kind", "PUT", "/api/servers/edit", put(set("auth", map[string]any{"kind": "magic"}))},
		{
			"template_without_secret", "update_template_without_secret", "PUT", "/api/servers/edit",
			put(set("auth", map[string]any{"kind": "shared_secret", "header": "Authorization", "template": "Bearer"})),
		},
		{"oauth_scope_openid", "update_oauth_scope_openid", "PUT", "/api/servers/edit", put(set("auth", map[string]any{
			"kind": "oauth_user", "header": "Authorization", "template": "Bearer {secret}",
			"oauth": map[string]any{"authorizeUrl": "https://a.example/a", "tokenUrl": "https://a.example/t", "clientId": "c", "scopes": []string{"openid"}}, //nolint:gosec // G101: made-up OAuth endpoints of a test server, not credentials
		}))},
		{"access_groups_empty", "update_access_groups_empty", "PUT", "/api/servers/edit", put(set("access", map[string]any{"kind": "groups", "groups": []string{}}))},
		{"timeout_below_minimum", "update_timeout_below_minimum", "PUT", "/api/servers/edit", put(set("timeoutMs", 10))},
		{"daily_limit_zero", "update_daily_limit_zero", "PUT", "/api/servers/edit", put(set("dailyLimit", 0))},
		{"body_not_json", "update_body_not_json", "PUT", "/api/servers/edit", "{bad"},
		{"secret_empty_string", "put_secret_empty_string", "PUT", "/api/servers/edit/secret", map[string]any{"secret": ""}},
		{"secret_missing", "put_secret_missing", "PUT", "/api/servers/edit/secret", map[string]any{}},
		{"status_unknown", "status_unknown_value", "POST", "/api/servers/edit/status", map[string]any{"status": "bogus"}},
		{"hidden_not_a_boolean", "hidden_not_a_boolean", "PUT", "/api/servers/edit/tools/a/hidden", map[string]any{"hidden": "yes"}},
		{"hidden_missing", "hidden_missing", "PUT", "/api/servers/edit/tools/a/hidden", map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkGolden(t, c.golden, e.admin(c.method, c.path, c.body))
		})
	}
	if st, _ := e.servers.row("edit"); st.Version != 0 {
		t.Fatalf("a refused request changed the server: version %d", st.Version)
	}
}

// TestTheBaseBodiesAreValid is the negative control of the two tests above.
func TestTheBaseBodiesAreValid(t *testing.T) {
	e := newEnv(t)
	for i, kind := range []string{"none", "shared_secret", "pat_user"} {
		b := bodyOf(kind, set("slug", "ok-"+string(rune('a'+i))))
		if r := e.admin("POST", "/api/servers", b); r.Status != 201 {
			t.Fatalf("%s: %d %s", kind, r.Status, r.Body)
		}
	}
	b := bodyOf("oauth_user", oauth(nil), set("slug", "ok-oauth"))
	if r := e.admin("POST", "/api/servers", b); r.Status != 201 {
		t.Fatalf("oauth_user: %d %s", r.Status, r.Body)
	}
}
