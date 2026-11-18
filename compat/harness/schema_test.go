//go:build compat

package harness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// The generated schemas must describe what the image really returns.
func TestSchemasMatchRealResponses(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)

	var me struct{}
	admin.Do(t, "GET", "/api/auth/me", nil).JSON(t, "MeResponse", &me)

	var servers []map[string]any
	admin.Do(t, "GET", "/api/servers", nil).JSON(t, "ServerList", &servers)

	var catalog []map[string]any
	admin.Do(t, "GET", "/api/catalog", nil).JSON(t, "CatalogResponse", &catalog)

	var e struct{}
	r := s.Anonymous().Do(t, "GET", "/api/auth/me", nil)
	if r.Status != 401 {
		t.Fatalf("anonymous /api/auth/me: %d", r.Status)
	}
	r.JSON(t, "ErrorBody", &e)

	member := s.Member(t, harness.Unique("member"))
	r = member.Do(t, "GET", "/api/servers", nil)
	if r.Status != 403 {
		t.Fatalf("member /api/servers: %d", r.Status)
	}
	r.JSON(t, "ErrorBody", &e)
}
