package http_test

import (
	"strings"
	"testing"
)

// TestBodyIssuesComeInSchemaOrder: zod validates a whole body and lists the problems in schema
// order; the first is the answer. Type problems and limit problems are ordered together.
func TestBodyIssuesComeInSchemaOrder(t *testing.T) {
	e := newEnv(t)
	oauthBody := func(oauth string, rest string) string {
		return `{"slug":"gitea","name":"G","description":"","url":"https://x.example/m","auth":{"kind":"oauth_user","header":"Authorization","template":"Bearer {secret}"` + rest + `,"oauth":` + oauth + `},"access":{"kind":"all"}}`
	}
	const okOAuth = `{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":["s"]}`
	with := func(oauth string) string { return oauthBody(oauth, "") }
	cases := []struct {
		name, body, want string
	}{
		// Across fields: the earlier field wins, whatever kind of problem it is.
		{"limit before limit", `{"name":"","url":"ftp://x"}`, "name: must be 1-100 characters"},
		{"limit before shape", `{"name":"","description":5}`, "name: must be 1-100 characters"},
		{"shape before limit", `{"name":5,"description":"` + long(501) + `"}`, "name: expected a string"},
		{"later shape error loses to an earlier limit", `{"name":"` + long(101) + `","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"timeoutMs":"x"}`, "name: must be 1-100 characters"},
		{"missing description before missing url", `{"name":"x"}`, "description: is required"},
		{"url of the wrong type", `{"name":"x","description":"","url":7}`, "url: expected a string"},
		{"timeout limit before slug shape", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"timeoutMs":5,"slug":7}`, "timeoutMs: must be an integer from 1000 to 600000"},
		{"slug shape before secret limit", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"slug":7,"secret":""}`, "slug: expected a string"},
		{"slug limit before secret shape", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"slug":"","secret":{}}`, "slug: must be 1-64 characters"},
		{"secret an object", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"slug":"abc","secret":{}}`, "secret: expected a string"},
		// auth
		{"auth an array", `{"name":"x","description":"","url":"https://x.example","auth":[]}`, "auth: expected an object"},
		{"auth kind a number", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":1}}`, "auth.kind: must be one of none, shared_secret, pat_user, oauth_user"},
		{"auth kind missing", `{"name":"x","description":"","url":"https://x.example","auth":{}}`, "auth.kind: must be one of none, shared_secret, pat_user, oauth_user"},
		{"header missing", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"shared_secret","template":"{secret}"}}`, "auth.header: is required"},
		{"template a number", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"pat_user","header":"X","template":3}}`, "auth.template: expected a string"},
		{"oauth block before header", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"oauth_user"}}`, "auth.oauth: is required"},
		{"oauth null", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"oauth_user","oauth":null,"header":"X","template":"{secret}"}}`, "auth.oauth: expected an object"},
		{"oauth url missing", with(`{"tokenUrl":"https://x.example/t","clientId":"c","scopes":[]}`), "auth.oauth.authorizeUrl: is required"},
		{"oauth url of the wrong type", with(`{"authorizeUrl":1,"tokenUrl":"nope","clientId":"c","scopes":[]}`), "auth.oauth.authorizeUrl: expected a string"},
		{"oauth token url limit before the client id shape", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"nope","clientId":4,"scopes":[]}`), "auth.oauth.tokenUrl: must be an http(s) URL"},
		{"revoke url null", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","revokeUrl":null,"clientId":"c","scopes":[]}`), "auth.oauth.revokeUrl: expected a string"},
		{"scopes missing", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c"}`), "auth.oauth.scopes: is required"},
		{"scopes an object", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":{}}`), "auth.oauth.scopes: expected an array"},
		{"scope of the wrong type", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":["a",2]}`), "auth.oauth.scopes.1: expected a string"},
		{"scope limit before a later scope shape", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":["",2]}`), "auth.oauth.scopes.0: must be 1-200 characters"},
		{"too many scopes after the element problems", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":["` + strings.Repeat(`a","`, 50) + `"]}`), "auth.oauth.scopes: must have at most 50 items"},
		{"extra params an array", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":[],"extraAuthorizeParams":[]}`), "auth.oauth.extraAuthorizeParams: expected an object"},
		{"extra params null", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":[],"extraAuthorizeParams":null}`), "auth.oauth.extraAuthorizeParams: expected an object"},
		{"extra param of the wrong type", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":[],"extraAuthorizeParams":{"a.b":1}}`), "auth.oauth.extraAuthorizeParams.a.b: expected a string"},
		{"extra param too long", with(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":[],"extraAuthorizeParams":{"a":"` + long(1001) + `"}}`), "auth.oauth.extraAuthorizeParams.a: must be at most 1000 characters"},
		{"oauth header after the oauth block", oauthBody(`{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"","scopes":[]}`, ``), "auth.oauth.clientId: must be 1-500 characters"},
		// access
		{"access a string", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":"all"}`, "access: expected an object"},
		{"access kind missing", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{}}`, "access.kind: must be one of all, groups"},
		{"groups missing", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"groups"}}`, "access.groups: is required"},
		{"groups not an array", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"groups","groups":"dev"}}`, "access.groups: expected an array"},
		{"group of the wrong type", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"groups","groups":["a",null]}}`, "access.groups.1: expected a string"},
		{"group limit before the array's", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"groups","groups":["` + long(201) + `"]}}`, "access.groups.0: must be 1-200 characters"},
		{"too many groups", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"groups","groups":[` + strings.Repeat(`"g",`, 100) + `"g"]}}`, "access.groups: must have 1-100 items"},
		// numbers
		{"timeout true", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"timeoutMs":true}`, "timeoutMs: expected a number"},
		{"daily limit a fraction", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"dailyLimit":1.5}`, "dailyLimit: expected an integer"},
		{"daily limit a string", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"dailyLimit":"3"}`, "dailyLimit: expected a number"},
		{"timeout beyond every limit", `{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"timeoutMs":123456789012345678901234567890}`, "timeoutMs: must be an integer from 1000 to 600000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.admin("POST", "/api/servers", c.body)
			wantReply(t, r, 422, errBody("invalid_request", c.want))
		})
	}
	if len(e.servers.rows) != 0 {
		t.Fatal("a refused request created a server")
	}
}

func TestSmallBodiesAndOddRoots(t *testing.T) {
	e := newEnv(t)
	e.addServer("gitea", nil, "a")
	cases := []struct {
		name, method, path, body, want string
	}{
		{"secret a number", "PUT", "/api/servers/gitea/secret", `{"secret":5}`, "secret: expected a string"},
		{"secret too long", "PUT", "/api/servers/gitea/secret", `{"secret":"` + long(4097) + `"}`, "secret: must be 1-4096 characters"},
		{"secret NUL", "PUT", "/api/servers/gitea/secret", `{"secret":"a\u0000b"}`, "secret: must not contain a NUL character"},
		{"secret an array body", "PUT", "/api/servers/gitea/secret", `[]`, "expected an object"},
		{"status a number", "POST", "/api/servers/gitea/status", `{"status":1}`, "status: must be active or disabled"},
		{"status missing", "POST", "/api/servers/gitea/status", `{}`, "status: must be active or disabled"},
		{"status null body", "POST", "/api/servers/gitea/status", `null`, "expected an object"},
		{"hidden null", "PUT", "/api/servers/gitea/tools/a/hidden", `{"hidden":null}`, "hidden: expected a boolean"},
		{"hidden a number", "PUT", "/api/servers/gitea/tools/a/hidden", `{"hidden":1}`, "hidden: expected a boolean"},
		{"put body null", "PUT", "/api/servers/gitea", `null`, "expected an object"},
		{"put body missing name", "PUT", "/api/servers/gitea", `{}`, "name: is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantReply(t, e.admin(c.method, c.path, c.body), 422, errBody("invalid_request", c.want))
		})
	}
	t.Run("a PUT ignores slug and secret", func(t *testing.T) {
		body := `{"slug":5,"secret":{},"name":"G","description":"","url":"https://x.example/m","auth":{"kind":"none"},"access":{"kind":"all"}}`
		if r := e.admin("PUT", "/api/servers/gitea", body); r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})
	t.Run("secret null clears, null secret on create is none", func(t *testing.T) {
		body := `{"slug":"nullsecret","secret":null,"name":"G","description":"","url":"https://x.example/m","auth":{"kind":"none"},"access":{"kind":"all"}}`
		if r := e.admin("POST", "/api/servers", body); r.Status != 201 || !strings.Contains(r.Body, `"secretSet":false`) {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})
	t.Run("invalid UTF-8 is replaced as JSON.parse does", func(t *testing.T) {
		body := "{\"slug\":\"utf\",\"name\":\"a\xffb\",\"description\":\"\",\"url\":\"https://x.example/m\",\"auth\":{\"kind\":\"none\"},\"access\":{\"kind\":\"all\"}}"
		if r := e.admin("POST", "/api/servers", body); r.Status != 201 || !strings.Contains(r.Body, "a�b") {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})
	t.Run("absurd nesting is not JSON for us and not a crash", func(t *testing.T) {
		deep := strings.Repeat("[", 20000) + strings.Repeat("]", 20000)
		wantReply(t, e.admin("POST", "/api/servers", deep), 422, errBody("invalid_request", "body is not valid JSON"))
	})
	t.Run("the URL is stored cleaned", func(t *testing.T) {
		body := "{\"slug\":\"cleaned\",\"name\":\"G\",\"description\":\"\",\"url\":\"  https://x.example/\\tm\\n \",\"auth\":{\"kind\":\"none\"},\"access\":{\"kind\":\"all\"}}"
		if r := e.admin("POST", "/api/servers", body); r.Status != 201 || !strings.Contains(r.Body, `"url":"https://x.example/m"`) {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})
}

func TestACatalogReadFailureIsAnInternalError(t *testing.T) {
	e := newEnv(t)
	e.servers.failFind = errString("boom")
	wantReply(t, e.do("member", "GET", "/api/catalog", nil), 500, errBody("internal", "Internal error"))
	wantReply(t, e.admin("GET", "/api/servers/x", nil), 500, errBody("internal", "Internal error"))
}

type errString string

func (e errString) Error() string { return string(e) }
