package http_test

import (
	"strings"
	"testing"

	cathttp "github.com/ai-pipes/mcp-registry/internal/modules/catalog/http"
)

// TestEveryRegisteredRouteStandsBehindItsGuards derives the routes from the table itself, so a
// new route cannot go unguarded unnoticed.
func TestEveryRegisteredRouteStandsBehindItsGuards(t *testing.T) {
	e := newEnv(t)
	routes, _ := cathttp.RoutesWith(testGuards(), e.deps)
	if len(routes) < 10 {
		t.Fatalf("only %d routes", len(routes))
	}
	for _, rt := range routes {
		path := strings.NewReplacer("{slug}", "gitea", "{name}", "a").Replace(rt.Path)
		r := e.do("admin", rt.Method, path, "{}", "X-Probe", "1")
		got := strings.Join(r.Header.Values("X-Guards"), ",")
		want := "session,admin"
		if strings.HasPrefix(rt.Path, "/api/catalog") {
			want = "session"
		}
		if got != want {
			t.Errorf("%s %s: guards %q, want %q", rt.Method, rt.Path, got, want)
		}
	}
}

func TestTheCatalogGuardIsOnTheRouteOnly(t *testing.T) {
	e := newEnv(t)
	notFound := errBody("not_found", "not found")
	wantReply(t, e.do("", "GET", "/api/catalog", nil), 401, unauthenticated)
	wantReply(t, e.do("", "GET", "/api/catalog/x", nil), 404, notFound)
	wantReply(t, e.do("", "POST", "/api/catalog", nil), 404, notFound)
}

func TestHideChecksOnlyTheToolNameForNUL(t *testing.T) {
	e := newEnv(t)
	e.addServer("gitea", nil, "a")
	// A NUL in the slug is not a slug: the ref is refused before any SQL, as in TypeScript.
	wantReply(t, e.admin("PUT", "/api/servers/a%00b/tools/a/hidden", map[string]any{"hidden": true}), 422,
		errBody("invalid_tool_ref", "tool name must look like <server>.<tool>"))
	wantReply(t, e.admin("PUT", "/api/servers/gitea/tools/a%00b/hidden", map[string]any{"hidden": true}), 404,
		errBody("not_found", "tool \"gitea.a\x00b\" not found"))
}

// TestNULPathParametersAreNotFoundOnEveryRoute: the fake ports fail like Postgres on a NUL, so a
// route that let one through would answer 500.
func TestNULPathParametersAreNotFoundOnEveryRoute(t *testing.T) {
	e := newEnv(t)
	e.addServer("gitea", nil, "a")
	put := map[string]any{"name": "G", "description": "", "url": "https://x.example/m", "auth": map[string]any{"kind": "none"}, "access": map[string]any{"kind": "all"}}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/servers/a%00b", nil},
		{"PUT", "/api/servers/a%00b", put},
		{"PUT", "/api/servers/a%00b/secret", map[string]any{"secret": "x"}},
		{"POST", "/api/servers/a%00b/status", map[string]any{"status": "active"}},
		{"POST", "/api/servers/a%00b/sync", nil},
		{"GET", "/api/servers/a%00b/tools", nil},
		{"PUT", "/api/servers/gitea/tools/a%00b/hidden", map[string]any{"hidden": true}},
	} {
		if r := e.admin(c.method, c.path, c.body); r.Status != 404 {
			t.Errorf("%s %s: %d %s", c.method, c.path, r.Status, r.Body)
		}
	}
}

func TestRepeatedExtraParamsAreJudgedByTheirLastValue(t *testing.T) {
	e := newEnv(t)
	body := func(params string) string {
		return `{"slug":"gitea","name":"G","description":"","url":"https://x.example/m","auth":{"kind":"oauth_user","header":"Authorization","template":"Bearer {secret}","oauth":{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":[],"extraAuthorizeParams":` + params + `}},"access":{"kind":"all"}}`
	}
	if r := e.admin("POST", "/api/servers", body(`{"a":1,"a":"ok"}`)); r.Status != 201 || !strings.Contains(r.Body, `"extraAuthorizeParams":{"a":"ok"}`) {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	wantReply(t, e.admin("POST", "/api/servers", body(`{"a":"ok","a":1}`)), 422,
		errBody("invalid_request", "auth.oauth.extraAuthorizeParams.a: expected a string"))
}

func TestOnlyAWholeBlankBodyIsEmpty(t *testing.T) {
	e := newEnv(t)
	// JavaScript's trim() decides emptiness (NBSP counts), but JSON.parse sees the raw text.
	wantReply(t, e.admin("POST", "/api/servers", "\u00a0\ufeff \n"), 422, errBody("invalid_request", "name: is required"))
	wantReply(t, e.admin("POST", "/api/servers", "\u00a0{}"), 422, errBody("invalid_request", "body is not valid JSON"))
	wantReply(t, e.admin("POST", "/api/servers", "{}\u00a0"), 422, errBody("invalid_request", "body is not valid JSON"))
	// The body is decoded as UTF-8 first, which drops a leading byte order mark.
	if r := e.admin("POST", "/api/servers", "\ufeff"+`{"slug":"bom","name":"G","description":"","url":"https://x.example/m","auth":{"kind":"none"},"access":{"kind":"all"}}`); r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

func TestAnOversizedListIsRefusedAsOversized(t *testing.T) {
	e := newEnv(t)
	scopes := `[` + strings.Repeat(`"s",`, 50) + `5]`
	oauth := `{"slug":"gitea","name":"G","description":"","url":"https://x.example/m","auth":{"kind":"oauth_user","header":"Authorization","template":"Bearer {secret}","oauth":{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":` + scopes + `}},"access":{"kind":"all"}}`
	wantReply(t, e.admin("POST", "/api/servers", oauth), 422, errBody("invalid_request", "auth.oauth.scopes: must have at most 50 items"))
	groups := `{"slug":"gitea","name":"G","description":"","url":"https://x.example/m","auth":{"kind":"none"},"access":{"kind":"groups","groups":[` + strings.Repeat(`"g",`, 100) + `null]}}`
	wantReply(t, e.admin("POST", "/api/servers", groups), 422, errBody("invalid_request", "access.groups: must have 1-100 items"))
	// Within the limit a wrong entry is its own issue.
	small := `{"slug":"gitea","name":"G","description":"","url":"https://x.example/m","auth":{"kind":"none"},"access":{"kind":"groups","groups":["g",null]}}`
	wantReply(t, e.admin("POST", "/api/servers", small), 422, errBody("invalid_request", "access.groups.1: expected a string"))
}

func TestNULInBodyStringsIsLeftToTheDomainWhereItHasAPattern(t *testing.T) {
	e := newEnv(t)
	slug := bodyOf("none", set("slug", "a\u0000b"))
	wantReply(t, e.admin("POST", "/api/servers", slug), 422,
		errBody("invalid_slug", "server slug must be 2-32 characters: lowercase letters, digits and dashes, starting with a letter"))
	header := bodyOf("shared_secret", authSet("header", "X\u0000Key"), set("slug", "nulhdr"))
	wantReply(t, e.admin("POST", "/api/servers", header), 422, errBody("invalid_auth", "header name may contain letters, digits and dashes"))
	template := bodyOf("shared_secret", authSet("template", "Bearer {secret}\u0000"), set("slug", "nultpl"))
	wantReply(t, e.admin("POST", "/api/servers", template), 422, errBody("invalid_auth", "header template must contain {secret} and be a single line"))
}

func TestStringLimitsCountCodePoints(t *testing.T) {
	e := newEnv(t)
	emoji := func(n int) string { return strings.Repeat("\U0001F600", n) }
	// 50 emoji are 100 UTF-16 units: within both the contract (code points) and the domain (units).
	if r := e.admin("POST", "/api/servers", bodyOf("none", set("slug", "emoji-ok"), set("name", emoji(50)))); r.Status != 201 {
		t.Fatalf("50 emoji: %d %s", r.Status, r.Body)
	}
	// 100 emoji pass the contract (100 code points); the domain, which counts UTF-16 units as
	// the TypeScript domain does, then refuses them with its own code.
	r := e.admin("POST", "/api/servers", bodyOf("none", set("slug", "emoji-dom"), set("name", emoji(100))))
	wantReply(t, r, 422, errBody("invalid_server", "server name must be 1-100 characters"))
	r = e.admin("POST", "/api/servers", bodyOf("none", set("slug", "emoji-no"), set("name", emoji(101))))
	wantReply(t, r, 422, errBody("invalid_request", "name: must be 1-100 characters"))
	wantReply(t, e.admin("PUT", "/api/servers/emoji-ok/secret", map[string]any{"secret": emoji(4097)}), 422,
		errBody("invalid_request", "secret: must be 1-4096 characters"))
}
