//go:build compat

package harness_test

import (
	"context"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

func TestStackBootsAndIsReachableBothWays(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	if r := s.Anonymous().Do(t, "GET", "/healthz", nil); r.Status != 200 || string(r.Body) != "ok" {
		t.Fatalf("healthz: %d %q", r.Status, r.Body)
	}
	if v := s.CLI(t, "", "--version"); v.Code != 0 || strings.TrimSpace(v.Stdout) == "" {
		t.Fatalf("cli --version: %+v", v)
	}
	var one int
	if err := s.DB.QueryRow(context.Background(), "select count(*) from platform.schema_migrations").Scan(&one); err != nil || one == 0 {
		t.Fatalf("migrations: n=%d err=%v", one, err)
	}
	if s.Arch != "amd64" && s.Arch != "arm64" {
		t.Fatalf("arch %q", s.Arch)
	}
}

func TestUpdateRefusesNonReferenceImage(t *testing.T) {
	t.Setenv("COMPAT_REFERENCE", "")
	if err := harness.CheckUpdateAllowed(true); err == nil {
		t.Fatal("update must be refused without COMPAT_REFERENCE=1")
	}
	if err := harness.CheckUpdateAllowed(false); err != nil {
		t.Fatalf("no update requested: %v", err)
	}
	t.Setenv("COMPAT_REFERENCE", "1")
	if err := harness.CheckUpdateAllowed(true); err != nil {
		t.Fatalf("reference image: %v", err)
	}
}

func TestRedactTruncates(t *testing.T) {
	if got := harness.Redact("sk_0123456789abcdef"); got != "sk_0123456..." {
		t.Fatalf("got %q", got)
	}
	if got := harness.Redact("short"); got != "short" {
		t.Fatalf("got %q", got)
	}
}

type me struct {
	Role string `json:"role"`
}

func TestAdminAndMemberSessions(t *testing.T) {
	s := harness.Shared(t)
	var a, m me
	r := s.Admin(t).Do(t, "GET", "/api/auth/me", nil)
	r.JSON(t, "", &a)
	r = s.Member(t, harness.Unique("member")).Do(t, "GET", "/api/auth/me", nil)
	r.JSON(t, "", &m)
	if a.Role != "admin" || m.Role != "member" {
		t.Fatalf("roles: admin client %q, member client %q", a.Role, m.Role)
	}
	if r := s.Anonymous().Do(t, "GET", "/api/auth/me", nil); r.Status != 401 {
		t.Fatalf("anonymous /api/auth/me: %d", r.Status)
	}
}

func TestCLIReportsFailure(t *testing.T) {
	s := harness.Shared(t)
	r := s.CLI(t, "", "no-such-command")
	if r.Code == 0 || strings.TrimSpace(r.Stderr) == "" {
		t.Fatalf("unknown command: %+v", r)
	}
}

func TestMCPListsFourMetaTools(t *testing.T) {
	s := harness.Shared(t)
	token := s.Admin(t).Token(t, harness.Unique("tok"))
	res, err := s.MCP(t, token).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"call_tool", "describe_tool", "list_servers", "search_tools"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools: %v", names)
	}
}

func TestSwapImageKeepsTheDatabase(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	sub := harness.Unique("keeper")
	if r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Keeper"); r.Code != 0 {
		t.Fatalf("create-user: %+v", r)
	}
	count := func() int {
		var n int
		if err := s.DB.QueryRow(context.Background(), "select count(*) from identity.users where oidc_sub = $1", sub).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 1 {
		t.Fatal("user row missing before the swap")
	}
	image := os.Getenv("COMPAT_IMAGE")
	if image == "" {
		image = "mcp-registry:compat-ts"
	}
	s.SwapImage(t, image, "")
	if r := s.Anonymous().Do(t, "GET", "/healthz", nil); r.Status != 200 || string(r.Body) != "ok" {
		t.Fatalf("healthz after swap: %d %q", r.Status, r.Body)
	}
	if count() != 1 {
		t.Fatal("user row lost by the swap")
	}
}
