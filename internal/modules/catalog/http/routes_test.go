package http_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

//nolint:gosec // G101: the text of an error answer, not a credential
const (
	unauthenticated = `{"error":{"code":"unauthenticated","message":"sign in required"}}`
	bearerRefused   = `{"error":{"code":"unauthenticated","message":"tokens do not work on the web API"}}`
	adminOnly       = `{"error":{"code":"forbidden","message":"admin only"}}`
)

// adminRoutes is every route under /api/servers.
var adminRoutes = []struct {
	method, path string
	body         any
}{
	{"GET", "/api/servers", nil},
	{"POST", "/api/servers", "{bad"},
	{"GET", "/api/servers/gitea", nil},
	{"PUT", "/api/servers/gitea", "{bad"},
	{"PUT", "/api/servers/gitea/secret", "{bad"},
	{"POST", "/api/servers/gitea/status", "{bad"},
	{"POST", "/api/servers/gitea/sync", nil},
	{"GET", "/api/servers/gitea/tools", nil},
	{"PUT", "/api/servers/gitea/tools/a/hidden", "{bad"},
	// Paths and methods that no route serves: the guard still answers first.
	{"GET", "/api/servers/gitea/nothing", nil},
	{"DELETE", "/api/servers/gitea", nil},
	{"GET", "/api/servers/", nil},
}

func TestEveryRouteIsGuarded(t *testing.T) {
	e := newEnv(t)
	e.addServer("gitea", nil, "a")
	for _, r := range adminRoutes {
		name := r.method + " " + r.path
		t.Run(name, func(t *testing.T) {
			wantReply(t, e.do("", r.method, r.path, r.body), 401, unauthenticated)
			wantReply(t, e.do("admin", r.method, r.path, r.body, "Authorization", "Bearer sk_abcdef"), 401, bearerRefused)
			// A member is refused before the body is read (a broken one is not a 422) and before
			// the server is looked up.
			wantReply(t, e.do("member", r.method, r.path, r.body), 403, adminOnly)
			wantReply(t, e.do("dev", r.method, r.path, r.body), 403, adminOnly)
		})
	}
	t.Run("the catalog is for every signed-in person", func(t *testing.T) {
		wantReply(t, e.do("", "GET", "/api/catalog", nil), 401, unauthenticated)
		wantReply(t, e.do("member", "GET", "/api/catalog", nil, "Authorization", "Bearer x"), 401, bearerRefused)
		if r := e.do("member", "GET", "/api/catalog", nil); r.Status != 200 {
			t.Fatalf("member: %d %s", r.Status, r.Body)
		}
		if r := e.do("admin", "GET", "/api/catalog", nil); r.Status != 200 {
			t.Fatalf("admin: %d %s", r.Status, r.Body)
		}
	})
	t.Run("an admin reaches the routes (and unknown ones are an ordinary 404)", func(t *testing.T) {
		wantReply(t, e.admin("GET", "/api/servers/gitea/nothing", nil), 404, errBody("not_found", "not found"))
		wantReply(t, e.admin("DELETE", "/api/servers/gitea", nil), 404, errBody("not_found", "not found"))
	})
}

const noneServerJSON = `{"slug":"gitea","name":"Gitea","description":"Git hosting","url":"https://mcp.example.com/gitea","auth":{"kind":"none"},"secretSet":false,"access":{"kind":"all"},"timeoutMs":120000,"dailyLimit":null,"status":"active","lastSyncAt":"2026-10-30T10:00:00.123Z","lastSyncError":null,"toolCount":2}`

func rawTool(name string) kernel.UpstreamTool {
	return kernel.UpstreamTool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func TestCreateAnswers201WithTheViewAfterTheFirstSync(t *testing.T) {
	e := newEnv(t)
	e.upstream.Tools = []kernel.UpstreamTool{rawTool("a"), rawTool("b")}
	r := e.admin("POST", "/api/servers", validServer("gitea"))
	wantReply(t, r, 201, noneServerJSON)
	if r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content type %q", r.Header.Get("Content-Type"))
	}
	want := []string{"server.create", "server.sync"}
	if got := e.audit.actions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("audit %v, want %v", got, want)
	}
	// GET returns the same view; the list is an array of it.
	wantReply(t, e.admin("GET", "/api/servers/gitea", nil), 200, noneServerJSON)
	wantReply(t, e.admin("GET", "/api/servers", nil), 200, "["+noneServerJSON+"]")
}

func TestAFailedFirstSyncDoesNotUndoTheCreation(t *testing.T) {
	e := newEnv(t)
	e.upstream.Err = kernel.NewUpstreamError(kernel.UpstreamTransport)
	r := e.admin("POST", "/api/servers", validServer("gitea"))
	want := `{"slug":"gitea","name":"Gitea","description":"Git hosting","url":"https://mcp.example.com/gitea","auth":{"kind":"none"},"secretSet":false,"access":{"kind":"all"},"timeoutMs":120000,"dailyLimit":null,"status":"active","lastSyncAt":"2026-10-30T10:00:00.123Z","lastSyncError":"Could not reach the upstream","toolCount":0}`
	wantReply(t, r, 201, want)
}

func TestListIsAnEmptyArrayWithoutServers(t *testing.T) {
	e := newEnv(t)
	wantReply(t, e.admin("GET", "/api/servers", nil), 200, "[]")
	wantReply(t, e.do("member", "GET", "/api/catalog", nil), 200, "[]")
}

func TestViewsOfEveryAuthKindAndAccess(t *testing.T) {
	e := newEnv(t)
	post := func(body map[string]any) string {
		t.Helper()
		r := e.admin("POST", "/api/servers", body)
		if r.Status != 201 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		return r.Body
	}
	pat := bodyOf("pat_user", set("slug", "pat"), set("access", map[string]any{"kind": "groups", "groups": []string{"dev", "ops"}}),
		set("timeoutMs", 5000), set("dailyLimit", 7), set("secret", "tok"))
	delete(pat, "secret")
	got := post(pat)
	want := `{"slug":"pat","name":"Gitea","description":"Git hosting","url":"https://mcp.example.com/gitea","auth":{"kind":"pat_user","header":"Authorization","template":"Bearer {secret}"},"secretSet":false,"access":{"kind":"groups","groups":["dev","ops"]},"timeoutMs":5000,"dailyLimit":7,"status":"active","lastSyncAt":"2026-10-30T10:00:00.123Z","lastSyncError":"An administrator needs to connect this server","toolCount":0}`
	if got != want {
		t.Fatalf("pat_user:\n got %s\nwant %s", got, want)
	}

	shared := post(bodyOf("shared_secret", set("slug", "shared"), set("secret", "s3cret-value")))
	if !strings.Contains(shared, `"auth":{"kind":"shared_secret","header":"Authorization","template":"Bearer {secret}"},"secretSet":true`) {
		t.Fatalf("shared_secret: %s", shared)
	}
	if strings.Contains(shared, "s3cret-value") {
		t.Fatalf("the secret is in the response: %s", shared)
	}

	full := post(bodyOf("oauth_user", set("slug", "oa"), set("secret", "client-secret"), oauth(func(o map[string]any) {
		o["revokeUrl"] = "https://git.example.com/revoke"
		o["scopes"] = []string{"read", "write"}
	})))
	if strings.Contains(full, "client-secret") || strings.Contains(full, "version") {
		t.Fatalf("leak: %s", full)
	}
	wantAuth := `"auth":{"kind":"oauth_user","oauth":{"authorizeUrl":"https://git.example.com/authorize","tokenUrl":"https://git.example.com/token","revokeUrl":"https://git.example.com/revoke","clientId":"cid","scopes":["read","write"]},"header":"Authorization","template":"Bearer {secret}"},"secretSet":true`
	if !strings.Contains(full, wantAuth) {
		t.Fatalf("oauth_user:\n got %s\nwant %s", full, wantAuth)
	}

	// Absent revokeUrl and extraAuthorizeParams are omitted; `{}` stays `{}`; the order of the
	// parameters is the document's.
	bare := post(bodyOf("oauth_user", set("slug", "bare"), oauth(func(o map[string]any) { o["scopes"] = []string{} })))
	if !strings.Contains(bare, `"oauth":{"authorizeUrl":"https://git.example.com/authorize","tokenUrl":"https://git.example.com/token","clientId":"cid","scopes":[]}`) {
		t.Fatalf("bare oauth: %s", bare)
	}
	raw := `{"slug":"ordered","name":"G","description":"","url":"https://x.example/m","auth":{"kind":"oauth_user","header":"Authorization","template":"Bearer {secret}","oauth":{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":["s"],"extraAuthorizeParams":{"zeta":"1","alpha":"2","zeta":"3","a.b":"<&>"}}},"access":{"kind":"all"}}`
	ordered := e.admin("POST", "/api/servers", raw)
	if ordered.Status != 201 || !strings.Contains(ordered.Body, `"extraAuthorizeParams":{"zeta":"3","alpha":"2","a.b":"<&>"}`) {
		t.Fatalf("ordered params: %d %s", ordered.Status, ordered.Body)
	}
	empty := `{"slug":"emptyx","name":"G","description":"","url":"https://x.example/m","auth":{"kind":"oauth_user","header":"Authorization","template":"Bearer {secret}","oauth":{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":[],"extraAuthorizeParams":{}}},"access":{"kind":"all"}}`
	if r := e.admin("POST", "/api/servers", empty); r.Status != 201 || !strings.Contains(r.Body, `"scopes":[],"extraAuthorizeParams":{}}`) {
		t.Fatalf("empty params: %d %s", r.Status, r.Body)
	}
}

func TestUnknownFieldsAreStrippedAtEveryLevel(t *testing.T) {
	e := newEnv(t)
	body := `{"slug":"gitea","extra":1,"name":"Gitea","description":"Git hosting","url":"https://mcp.example.com/gitea","auth":{"kind":"none","header":"X","oops":true},"access":{"kind":"all","groups":["a"]},"version":9,"secretSet":true}`
	r := e.admin("POST", "/api/servers", body)
	wantReply(t, r, 201, strings.Replace(noneServerJSON, `"lastSyncAt":"2026-10-30T10:00:00.123Z","lastSyncError":null,"toolCount":2`, `"lastSyncAt":"2026-10-30T10:00:00.123Z","lastSyncError":null,"toolCount":0`, 1))
}

func TestNumbersAndDefaults(t *testing.T) {
	e := newEnv(t)
	base := func(timeout, limit string) string {
		return `{"slug":"gitea","name":"G","description":"","url":"https://x.example/m","auth":{"kind":"none"},"access":{"kind":"all"}` + timeout + limit + `}`
	}
	// Absent: the defaults. 1000.0 and 1e3 are the integer 1000.
	for i, c := range []struct{ timeout, limit, wantT, wantL string }{
		{"", "", `"timeoutMs":120000`, `"dailyLimit":null`},
		{`,"timeoutMs":1000.0`, `,"dailyLimit":null`, `"timeoutMs":1000`, `"dailyLimit":null`},
		{`,"timeoutMs":1e3`, `,"dailyLimit":5e0`, `"timeoutMs":1000`, `"dailyLimit":5`},
	} {
		slug := string(rune('a'+i)) + "gitea"
		body := strings.Replace(base(c.timeout, c.limit), `"gitea"`, `"`+slug+`"`, 1)
		r := e.admin("POST", "/api/servers", body)
		if r.Status != 201 || !strings.Contains(r.Body, c.wantT) || !strings.Contains(r.Body, c.wantL) {
			t.Fatalf("case %d: %d %s", i, r.Status, r.Body)
		}
	}
	// null for the timeout is a refusal: only an absent key takes the default.
	r := e.admin("POST", "/api/servers", base(`,"timeoutMs":null`, ""))
	if r.Status != 422 || !strings.HasPrefix(r.Body, `{"error":{"code":"invalid_request","message":"timeoutMs: `) {
		t.Fatalf("null timeout: %d %s", r.Status, r.Body)
	}
	for _, v := range []string{`1e999`, `-1e999`, `true`, `[]`, `{}`} {
		r := e.admin("POST", "/api/servers", base(`,"timeoutMs":`+v, ""))
		if r.Status != 422 || !strings.Contains(r.Body, `"message":"timeoutMs: `) {
			t.Fatalf("timeout %s: %d %s", v, r.Status, r.Body)
		}
	}
}

func TestUpdateResetsOmittedFields(t *testing.T) {
	e := newEnv(t)
	e.addServer("gitea", nil, "a")
	r := e.admin("PUT", "/api/servers/gitea", map[string]any{
		"name": "Renamed", "description": "d", "url": "https://mcp.example.com/other", "auth": map[string]any{"kind": "none"},
		"access": map[string]any{"kind": "groups", "groups": []string{"dev"}}, "timeoutMs": 2000, "dailyLimit": 3,
	})
	wantReply(t, r, 200, `{"slug":"gitea","name":"Renamed","description":"d","url":"https://mcp.example.com/other","auth":{"kind":"none"},"secretSet":false,"access":{"kind":"groups","groups":["dev"]},"timeoutMs":2000,"dailyLimit":3,"status":"active","lastSyncAt":null,"lastSyncError":null,"toolCount":1}`)
	// A PUT that omits the timeout and the limit resets them.
	r = e.admin("PUT", "/api/servers/gitea", map[string]any{
		"name": "Renamed", "description": "d", "url": "https://mcp.example.com/other", "auth": map[string]any{"kind": "none"}, "access": map[string]any{"kind": "all"},
	})
	if r.Status != 200 || !strings.Contains(r.Body, `"timeoutMs":120000,"dailyLimit":null`) {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if got := e.audit.actions(); strings.Join(got, ",") != "server.create,server.update,server.update" {
		t.Errorf("audit %v", got)
	}
}

func TestSecretStatusAndSyncRoutes(t *testing.T) {
	e := newEnv(t)
	e.addServer("shared", nil, "a")
	// The stored mode has no secret: change it first.
	r := e.admin("PUT", "/api/servers/shared", map[string]any{
		"name": "G", "description": "", "url": "https://mcp.example.com/gitea",
		"auth":   map[string]any{"kind": "shared_secret", "header": "X-Key", "template": "{secret}"},
		"access": map[string]any{"kind": "all"},
	})
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	wantReply(t, e.admin("PUT", "/api/servers/shared/secret", map[string]any{"secret": "abc"}), 204, "")
	if st, _ := e.servers.row("shared"); len(st.SealedSecret.Reveal()) == 0 {
		t.Fatal("the secret was not stored")
	}
	wantReply(t, e.admin("PUT", "/api/servers/shared/secret", map[string]any{"secret": nil}), 204, "")
	if st, _ := e.servers.row("shared"); len(st.SealedSecret.Reveal()) != 0 {
		t.Fatal("the secret was not cleared")
	}
	wantReply(t, e.admin("PUT", "/api/servers/shared/secret", map[string]any{"secret": "   "}), 422,
		errBody("invalid_server", "secret must be non-blank and at most 4096 characters"))
	wantReply(t, e.admin("PUT", "/api/servers/gitea-x/secret", map[string]any{"secret": "x"}), 404, errBody("not_found", `server "gitea-x" not found`))

	wantReply(t, e.admin("POST", "/api/servers/shared/status", map[string]any{"status": "disabled"}), 204, "")
	if st, _ := e.servers.row("shared"); st.Status != domain.StatusDisabled {
		t.Fatalf("status %q", st.Status)
	}
	wantReply(t, e.admin("POST", "/api/servers/shared/status", map[string]any{"status": "disabled"}), 204, "")
	wantReply(t, e.admin("POST", "/api/servers/nope-x/status", map[string]any{"status": "active"}), 404, errBody("not_found", `server "nope-x" not found`))
	wantReply(t, e.admin("POST", "/api/servers/shared/sync", nil), 404, errBody("not_found", `active server "shared" not found`))
	wantReply(t, e.admin("POST", "/api/servers/shared/status", map[string]any{"status": "active"}), 204, "")

	e.upstream.Tools = []kernel.UpstreamTool{rawTool("a"), rawTool("b"), rawTool("c")}
	// shared_secret without a stored secret is a sync failure, not a crash.
	wantReply(t, e.admin("POST", "/api/servers/shared/sync", nil), 502, errBody("sync_failed", "The server secret is not set"))
	wantReply(t, e.admin("PUT", "/api/servers/shared/secret", map[string]any{"secret": "abc"}), 204, "")
	// b and c are new; a was stored with another description, so it counts as changed (not as added).
	wantReply(t, e.admin("POST", "/api/servers/shared/sync", nil), 200, `{"added":2,"changed":1,"removed":0}`)
	e.upstream.Tools = []kernel.UpstreamTool{{Name: "a", Description: ptr("now documented"), InputSchema: json.RawMessage(`{"type":"object"}`)}, rawTool("c")}
	wantReply(t, e.admin("POST", "/api/servers/shared/sync", nil), 200, `{"added":0,"changed":1,"removed":1}`)
	wantReply(t, e.admin("POST", "/api/servers/nope-x/sync", nil), 404, errBody("not_found", `active server "nope-x" not found`))
}

func TestSyncOfAPersonalServerWithoutAConnectionIsA400(t *testing.T) {
	e := newEnv(t)
	body := bodyOf("pat_user", set("slug", "pat"))
	if r := e.admin("POST", "/api/servers", body); r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	wantReply(t, e.admin("POST", "/api/servers/pat/sync", nil), 400, errBody("needs_connection", "An administrator needs to connect this server"))
}

func TestStatusMapping(t *testing.T) {
	e := newEnv(t)
	e.upstream.Tools = []kernel.UpstreamTool{rawTool("a")}
	if r := e.admin("POST", "/api/servers", validServer("gitea")); r.Status != 201 {
		t.Fatal(r.Body)
	}
	t.Run("server_exists is 409", func(t *testing.T) {
		wantReply(t, e.admin("POST", "/api/servers", validServer("gitea")), 409, errBody("server_exists", `server "gitea" already exists`))
	})
	t.Run("server_conflict is 409", func(t *testing.T) {
		e.servers.beforeSave = func(slug string) {
			e.servers.beforeSave = nil
			// Someone else saves between this request's read and write.
			s, _ := e.servers.FindBySlug(context.Background(), slug)
			s.SetStatus(domain.StatusDisabled, t0)
			if err := e.servers.Save(context.Background(), s, nil); err != nil {
				t.Fatal(err)
			}
		}
		r := e.admin("PUT", "/api/servers/gitea", map[string]any{
			"name": "Mine", "description": "", "url": "https://mcp.example.com/gitea", "auth": map[string]any{"kind": "none"}, "access": map[string]any{"kind": "all"},
		})
		wantReply(t, r, 409, errBody("server_conflict", `server "gitea" was changed meanwhile; reload it and try again`))
	})
	t.Run("an unexpected failure is a 500 that tells nothing", func(t *testing.T) {
		e.servers.failFind = errors.New("db password is hunter2")
		defer func() { e.servers.failFind = nil }()
		r := e.admin("GET", "/api/servers", nil)
		wantReply(t, r, 500, errBody("internal", "Internal error"))
		if strings.Contains(e.logs.String(), "hunter2") {
			t.Fatalf("the failure text reached the log: %s", e.logs.String())
		}
	})
}

func TestGetAnUnknownServerHasTheUnquotedMessage(t *testing.T) {
	e := newEnv(t)
	wantReply(t, e.admin("GET", "/api/servers/nope-x", nil), 404, errBody("not_found", "server not found"))
}

func TestToolsRoutes(t *testing.T) {
	e := newEnv(t)
	e.addServer("gitea", nil)
	snap := func(name string, mut func(*domain.ToolSnapshot)) domain.ToolSnapshot {
		s := domain.ToolSnapshot{Name: name, Description: "does " + name, InputSchema: []byte(`{"type":"object","maximum":12345678901234567890}`)}
		if mut != nil {
			mut(&s)
		}
		return s
	}
	yes := true
	_, err := e.tools.ApplySnapshot(context.Background(), "gitea", []domain.ToolSnapshot{
		snap("a", func(s *domain.ToolSnapshot) {
			s.Title = ptr("Tool A")
			s.OutputSchema = []byte(`{"type":"string"}`)
			s.Annotations = domain.ToolAnnotations{ReadOnlyHint: &yes, DestructiveHint: ptr(false)}
		}),
		snap("b.dotted", nil),
		snap("c/slash", nil),
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	wantTools := `[{"ref":"gitea.a","title":"Tool A","description":"does a","annotations":{"readOnlyHint":true,"destructiveHint":false},"name":"a","inputSchema":{"type":"object","maximum":12345678901234567890},"outputSchema":{"type":"string"},"hidden":false},` +
		`{"ref":"gitea.b.dotted","title":null,"description":"does b.dotted","annotations":{},"name":"b.dotted","inputSchema":{"type":"object","maximum":12345678901234567890},"outputSchema":null,"hidden":false},` +
		`{"ref":"gitea.c/slash","title":null,"description":"does c/slash","annotations":{},"name":"c/slash","inputSchema":{"type":"object","maximum":12345678901234567890},"outputSchema":null,"hidden":false}]`
	wantReply(t, e.admin("GET", "/api/servers/gitea/tools", nil), 200, wantTools)
	wantReply(t, e.admin("GET", "/api/servers/nope-x/tools", nil), 404, errBody("not_found", `server "nope-x" not found`))

	// The catalog shows the tools without schemas.
	wantCatalog := `[{"slug":"gitea","name":"Gitea","description":"Git hosting","tools":[{"ref":"gitea.a","title":"Tool A","description":"does a","annotations":{"readOnlyHint":true,"destructiveHint":false}},{"ref":"gitea.b.dotted","title":null,"description":"does b.dotted","annotations":{}},{"ref":"gitea.c/slash","title":null,"description":"does c/slash","annotations":{}}]}]`
	wantReply(t, e.do("member", "GET", "/api/catalog", nil), 200, wantCatalog)

	// Hide: a name with a dot, and one with an encoded slash.
	wantReply(t, e.admin("PUT", "/api/servers/gitea/tools/b.dotted/hidden", map[string]any{"hidden": true}), 204, "")
	wantReply(t, e.admin("PUT", "/api/servers/gitea/tools/c%2Fslash/hidden", map[string]any{"hidden": true}), 204, "")
	st, _ := e.tools.Find(context.Background(), "gitea", "c/slash")
	if !st.Hidden() {
		t.Fatal("the tool with an encoded slash was not hidden")
	}
	// Hidden ones leave the member's catalog and stay in the admin list.
	if r := e.do("member", "GET", "/api/catalog", nil); strings.Contains(r.Body, "dotted") || strings.Contains(r.Body, "slash") {
		t.Fatalf("hidden tools in the catalog: %s", r.Body)
	}
	if r := e.admin("GET", "/api/servers/gitea/tools", nil); !strings.Contains(r.Body, `"name":"b.dotted"`) || !strings.Contains(r.Body, `"hidden":true`) {
		t.Fatalf("admin list: %s", r.Body)
	}
	wantReply(t, e.admin("PUT", "/api/servers/gitea/tools/b.dotted/hidden", map[string]any{"hidden": false}), 204, "")
	wantReply(t, e.admin("PUT", "/api/servers/gitea/tools/nope/hidden", map[string]any{"hidden": true}), 404, errBody("not_found", `tool "gitea.nope" not found`))
	wantReply(t, e.admin("PUT", "/api/servers/nope-x/tools/a/hidden", map[string]any{"hidden": true}), 404, errBody("not_found", `tool "nope-x.a" not found`))
	// A body error wins over an unknown server.
	if r := e.admin("PUT", "/api/servers/nope-x/tools/a/hidden", map[string]any{}); r.Status != 422 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if got := e.audit.actions(); strings.Join(got, ",") != "server.create,tool.hide,tool.hide,tool.unhide" {
		t.Errorf("audit %v", got)
	}
}

func TestACatalogForAMemberShowsOnlyWhatTheyMayUse(t *testing.T) {
	e := newEnv(t)
	e.addServer("open", nil, "a")
	e.addServer("closed", nil, "b")
	r := e.admin("PUT", "/api/servers/closed", map[string]any{
		"name": "Closed", "description": "", "url": "https://mcp.example.com/c", "auth": map[string]any{"kind": "none"},
		"access": map[string]any{"kind": "groups", "groups": []string{"dev"}},
	})
	if r.Status != 200 {
		t.Fatal(r.Body)
	}
	slugs := func(who string) string {
		var out []struct{ Slug string }
		if err := json.Unmarshal([]byte(e.do(who, "GET", "/api/catalog", nil).Body), &out); err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, x := range out {
			s = append(s, x.Slug)
		}
		return strings.Join(s, ",")
	}
	if got := slugs("member"); got != "open" {
		t.Errorf("member sees %q", got)
	}
	if got := slugs("dev"); got != "closed,open" {
		t.Errorf("dev sees %q", got)
	}
	if got := slugs("admin"); got != "closed,open" {
		t.Errorf("admin sees %q", got)
	}
}

func TestPathParametersThatCannotExistAreNotFound(t *testing.T) {
	e := newEnv(t)
	e.addServer("gitea", nil, "a")
	valid := map[string]any{"hidden": true}
	putBody := map[string]any{"name": "G", "description": "", "url": "https://x.example/m", "auth": map[string]any{"kind": "none"}, "access": map[string]any{"kind": "all"}}
	cases := []struct {
		name, method, path string
		body               any
		status             int
		message            string
	}{
		{"get nul", "GET", "/api/servers/a%00b", nil, 404, "server not found"},
		{"put nul", "PUT", "/api/servers/a%00b", putBody, 404, "server \"a\x00b\" not found"},
		{"secret nul", "PUT", "/api/servers/a%00b/secret", map[string]any{"secret": "x"}, 404, "server \"a\x00b\" not found"},
		{"status nul", "POST", "/api/servers/a%00b/status", map[string]any{"status": "active"}, 404, "server \"a\x00b\" not found"},
		{"sync nul", "POST", "/api/servers/a%00b/sync", nil, 404, "active server \"a\x00b\" not found"},
		{"tools nul", "GET", "/api/servers/a%00b/tools", nil, 404, "server \"a\x00b\" not found"},
		{"hidden nul in the tool", "PUT", "/api/servers/gitea/tools/a%00b/hidden", valid, 404, "tool \"gitea.a\x00b\" not found"},

		{"invalid utf-8", "GET", "/api/servers/a%ffb", nil, 404, "not found"},
		{"dot segment", "GET", "/api/servers/%2e%2e", nil, 404, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantReply(t, e.admin(c.method, c.path, c.body), c.status, errBody("not_found", c.message))
		})
	}
	t.Run("a bad body on such a slug is still a 422", func(t *testing.T) {
		r := e.admin("PUT", "/api/servers/a%00b", "{bad")
		wantReply(t, r, 422, errBody("invalid_request", "body is not valid JSON"))
	})
}

func TestBodyReadingEdgeCases(t *testing.T) {
	e := newEnv(t)
	t.Run("an empty body is an empty object", func(t *testing.T) {
		r := e.admin("POST", "/api/servers", "")
		wantReply(t, r, 422, errBody("invalid_request", "name: is required"))
		r = e.admin("POST", "/api/servers", "  \n ")
		wantReply(t, r, 422, errBody("invalid_request", "name: is required"))
	})
	t.Run("trailing data is not JSON", func(t *testing.T) {
		wantReply(t, e.admin("POST", "/api/servers", `{} {}`), 422, errBody("invalid_request", "body is not valid JSON"))
	})
	t.Run("a duplicate member takes the last value, as JSON.parse does", func(t *testing.T) {
		body := `{"slug":"first","slug":"gitea","name":"G","description":"","url":"https://x.example/m","auth":{"kind":"none"},"access":{"kind":"all"}}`
		r := e.admin("POST", "/api/servers", body)
		if r.Status != 201 || !strings.HasPrefix(r.Body, `{"slug":"gitea"`) {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})
	t.Run("names match case-sensitively", func(t *testing.T) {
		wantReply(t, e.admin("POST", "/api/servers", `{"NAME":"x"}`), 422, errBody("invalid_request", "name: is required"))
	})
	t.Run("a NUL in a text is refused, not stored", func(t *testing.T) {
		b := validServer("nulled")
		b["name"] = "a\u0000b"
		r := e.admin("POST", "/api/servers", b)
		if r.Status != 422 || !strings.Contains(r.Body, `"message":"name: `) {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})
	t.Run("a body over the limit is a 413", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/servers", strings.NewReader(strings.Repeat("x", 100)))
		req.Header.Set(who, "admin")
		req.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Body, 10)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != 413 || !strings.Contains(rec.Body.String(), "limit_exceeded") {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
}

// TestAWriteOutlivesItsRequest: a client that goes away must not leave a half-recorded change.
func TestAWriteOutlivesItsRequest(t *testing.T) {
	e := newEnv(t)
	e.upstream.Tools = []kernel.UpstreamTool{rawTool("a")}
	send := func(method, path string, body string) (*httptest.ResponseRecorder, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		req.Header.Set(who, "admin")
		rec := httptest.NewRecorder()
		e.upstream.onList = cancel // the client goes away while the upstream is being asked
		e.h.ServeHTTP(rec, req)
		return rec, cancel
	}
	body, _ := json.Marshal(validServer("gitea"))
	rec, cancel := send("POST", "/api/servers", string(body))
	defer cancel()
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"lastSyncError":null,"toolCount":1`) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if st, _ := e.servers.row("gitea"); st.LastSyncAt == nil || st.LastSyncError != nil {
		t.Fatalf("the first sync was not recorded: %+v", st)
	}
	rec, cancel2 := send("POST", "/api/servers/gitea/sync", "")
	defer cancel2()
	if rec.Code != 200 || rec.Body.String() != `{"added":0,"changed":0,"removed":0}` {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}
}
