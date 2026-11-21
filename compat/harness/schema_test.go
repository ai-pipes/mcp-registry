//go:build compat

package harness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func TestSchemaRejectsExtraAndMissingFields(t *testing.T) {
	ok := []byte(`{"error":{"code":"x","message":"y"}}`)
	if err := harness.ValidateSchema("ErrorBody", ok); err != nil {
		t.Fatal(err)
	}
	if harness.ValidateSchema("ErrorBody", []byte(`{"error":{"code":"x"}}`)) == nil {
		t.Fatal("missing message accepted")
	}
	if harness.ValidateSchema("ErrorBody", []byte(`{"error":{"code":"x","message":"y","extra":1}}`)) == nil {
		t.Fatal("undeclared field accepted")
	}
}

func TestEveryContractHasASchema(t *testing.T) {
	for _, n := range []string{"ErrorBody", "ServerView", "CatalogResponse", "CallList", "AuditList", "MeResponse"} {
		if _, err := os.Stat(filepath.Join("..", "schemas", n+".json")); err != nil {
			t.Fatal(err)
		}
	}
}

func serverView() map[string]any {
	return map[string]any{
		"slug": "gitea", "name": "Gitea", "description": "d", "url": "http://x/mcp",
		"auth": map[string]any{"kind": "none"}, "access": map[string]any{"kind": "all"},
		"timeoutMs": 30000, "dailyLimit": nil, "status": "active", "secretSet": false,
		"toolCount": 0, "lastSyncAt": nil, "lastSyncError": nil,
	}
}

// A secret next to secretSet must be rejected: the server view never carries one.
func TestServerViewWithSecretFieldIsRejected(t *testing.T) {
	v := serverView()
	body, _ := json.Marshal(v)
	if err := harness.ValidateSchema("ServerView", body); err != nil {
		t.Fatalf("fixture does not match ServerView: %v", err)
	}
	v["secret"] = "hunter2"
	body, _ = json.Marshal(v)
	err := harness.ValidateSchema("ServerView", body)
	if err == nil {
		t.Fatal("ServerView with an undeclared secret field accepted")
	}
	if !strings.Contains(err.Error(), "ServerView") {
		t.Fatalf("error does not name the schema: %v", err)
	}
}

func TestListSuffixAndUnknownName(t *testing.T) {
	if err := harness.ValidateSchema("TokenSummary[]", []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if harness.ValidateSchema("TokenSummary[]", []byte(`{}`)) == nil {
		t.Fatal("object accepted as a list")
	}
	if harness.ValidateSchema("NoSuchContract", []byte(`{}`)) == nil {
		t.Fatal("unknown schema name accepted")
	}
	if harness.ValidateSchema("ErrorBody", []byte(`not json`)) == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestErrorNamesSchemaAndPath(t *testing.T) {
	err := harness.ValidateSchema("ErrorBody", []byte(`{"error":{"code":1,"message":"y"}}`))
	if err == nil || !strings.Contains(err.Error(), "ErrorBody") || !strings.Contains(err.Error(), "/error/code") {
		t.Fatalf("want schema name and JSON path in %v", err)
	}
}

func oauthServerView(authorize, token string) map[string]any {
	v := serverView()
	v["auth"] = map[string]any{
		"kind": "oauth_user", "header": "Authorization", "template": "Bearer {secret}",
		"oauth": map[string]any{
			"authorizeUrl": authorize, "tokenUrl": token, "clientId": "c", "scopes": []string{},
		},
	}
	return v
}

func TestURLFormatsAreAsserted(t *testing.T) {
	check := func(authorize, token string) error {
		body, _ := json.Marshal(oauthServerView(authorize, token))
		return harness.ValidateSchema("ServerView", body)
	}
	if err := check("https://idp.example/authorize", "http://idp.example/token"); err != nil {
		t.Fatalf("valid http(s) URLs rejected: %v", err)
	}
	if check("not a url", "https://idp.example/token") == nil {
		t.Fatal("authorizeUrl \"not a url\" accepted")
	}
	if check("https://idp.example/authorize", "ftp://x") == nil {
		t.Fatal("tokenUrl ftp://x accepted: only http(s) is allowed")
	}
	if check("https://idp.example/authorize", "https://") == nil {
		t.Fatal("URL without a host accepted")
	}
}

// Every other format the generated schemas use must be asserted too (CallsQuery: date-time, uuid).
func TestOtherFormatsAreAsserted(t *testing.T) {
	cases := []struct {
		name, good, bad string
	}{
		{"before", `{"limit":1,"before":"2026-01-02T03:04:05Z"}`, `{"limit":1,"before":"yesterday"}`},
		{"user", `{"limit":1,"user":"123e4567-e89b-12d3-a456-426614174000"}`, `{"limit":1,"user":"not-a-uuid"}`},
	}
	for _, c := range cases {
		if err := harness.ValidateSchema("CallsQuery", []byte(c.good)); err != nil {
			t.Errorf("%s: valid value rejected: %v", c.name, err)
		}
		if harness.ValidateSchema("CallsQuery", []byte(c.bad)) == nil {
			t.Errorf("%s: malformed value accepted", c.name)
		}
	}
}

// The generated schemas must describe what the image really returns, on non-empty data: a
// vacuous "[]" would prove nothing about the item schemas.
func TestSchemasMatchRealResponses(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)

	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	slug := harness.Unique("schema")
	var created struct {
		Slug string `json:"slug"`
	}
	r := admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, slug, "none"))
	if r.Status != 201 {
		t.Fatalf("POST /api/servers: %d %s", r.Status, r.Body)
	}
	r.JSON(t, "ServerView", &created)

	var me struct{}
	admin.Do(t, "GET", "/api/auth/me", nil).JSON(t, "MeResponse", &me)

	type serverRow struct {
		Slug string `json:"slug"`
	}
	var servers []serverRow
	r = admin.Do(t, "GET", "/api/servers", nil)
	r.JSON(t, "ServerList", &servers)
	if !slices.ContainsFunc(servers, func(v serverRow) bool { return v.Slug == slug }) {
		t.Fatalf("GET /api/servers does not list %s: %s", slug, r.Body)
	}

	type catalogEntry struct {
		Slug  string `json:"slug"`
		Tools []any  `json:"tools"`
	}
	var catalog []catalogEntry
	harness.Eventually(t, "the catalog shows the tool of "+slug, func() bool {
		r := admin.Do(t, "GET", "/api/catalog", nil)
		r.JSON(t, "CatalogResponse", &catalog)
		return slices.ContainsFunc(catalog, func(e catalogEntry) bool { return e.Slug == slug && len(e.Tools) > 0 })
	})

	var tools []any
	r = admin.Do(t, "GET", "/api/servers/"+slug+"/tools", nil)
	r.JSON(t, "AdminToolList", &tools)
	if len(tools) == 0 {
		t.Fatalf("GET /api/servers/%s/tools is empty", slug)
	}

	// Bare-array endpoints, each with at least one item.
	admin.Token(t, harness.Unique("tok"))
	var tokens []any
	r = admin.Do(t, "GET", "/api/tokens", nil)
	r.JSON(t, "TokenSummary[]", &tokens)
	if len(tokens) == 0 {
		t.Fatal("GET /api/tokens is empty after creating a token")
	}
	var users []any
	r = admin.Do(t, "GET", "/api/users", nil)
	r.JSON(t, "UserSummary[]", &users)
	if len(users) == 0 {
		t.Fatal("GET /api/users is empty")
	}
	patSlug := harness.Unique("schemapat")
	if r := admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, patSlug, "pat_user")); r.Status != 201 {
		t.Fatalf("POST /api/servers pat_user: %d %s", r.Status, r.Body)
	}
	var conns []any
	r = admin.Do(t, "GET", "/api/connections", nil)
	r.JSON(t, "ConnectionSummary[]", &conns)
	if len(conns) == 0 {
		t.Fatalf("GET /api/connections has no entry for the pat_user server: %s", r.Body)
	}

	var e struct{}
	r = s.Anonymous().Do(t, "GET", "/api/auth/me", nil)
	if r.Status != 401 {
		t.Fatalf("anonymous /api/auth/me: %d", r.Status)
	}
	r.JSON(t, "ErrorBody", &e)
	r = s.Member(t, harness.Unique("member")).Do(t, "GET", "/api/servers", nil)
	if r.Status != 403 {
		t.Fatalf("member /api/servers: %d", r.Status)
	}
	r.JSON(t, "ErrorBody", &e)
}
