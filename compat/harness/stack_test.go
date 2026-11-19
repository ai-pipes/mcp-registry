//go:build compat

package harness_test

import (
	"context"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
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
	r.Decode(t, &a)
	r = s.Member(t, harness.Unique("member")).Do(t, "GET", "/api/auth/me", nil)
	r.Decode(t, &m)
	if a.Role != "admin" || m.Role != "member" {
		t.Fatalf("roles: admin client %q, member client %q", a.Role, m.Role)
	}
	if r := s.Anonymous().Do(t, "GET", "/api/auth/me", nil); r.Status != 401 {
		t.Fatalf("anonymous /api/auth/me: %d", r.Status)
	}
}

func TestCLIReportsItsOwnFailure(t *testing.T) {
	s := harness.Shared(t)
	r := s.CLI(t, "", "no-such-command")
	// 2 = bad invocation, with the CLI's own message: the CLI was reached, not just docker.
	if r.Code != 2 || !strings.HasPrefix(r.Stderr, `error: unknown command "no-such-command"`) {
		t.Fatalf("unknown command: code %d stderr %q", r.Code, r.Stderr)
	}
	if r.Stdout != "" {
		t.Fatalf("stdout: %q", r.Stdout)
	}
}

func TestOriginIsCheckedOnMutatingRequests(t *testing.T) {
	s := harness.Shared(t)
	c := s.Admin(t)
	body := map[string]string{"name": harness.Unique("tok")}
	if r := c.Do(t, "POST", "/api/tokens", body); r.Status != 201 {
		t.Fatalf("default Origin: %d %s", r.Status, r.Body)
	}
	for name, hdr := range map[string]string{"no Origin": "Origin:", "foreign Origin": "Origin: http://evil.example"} {
		r := c.Do(t, "POST", "/api/tokens", body, hdr)
		if r.Status != 403 || !strings.Contains(string(r.Body), `"code":"csrf"`) {
			t.Fatalf("%s: %d %s", name, r.Status, r.Body)
		}
	}
}

func TestMCPRefusesAForeignHost(t *testing.T) {
	s := harness.Shared(t)
	token := s.Admin(t).Token(t, harness.Unique("tok"))
	hdr := []string{"Authorization: Bearer " + token, "Content-Type: application/json", "Accept: application/json, text/event-stream"}
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if r := s.Anonymous().Do(t, "POST", "/mcp", call, hdr...); r.Status != 200 {
		t.Fatalf("own Host: %d %.200s", r.Status, r.Body)
	}
	r := s.Anonymous().Do(t, "POST", "/mcp", call, append(hdr, "Host: evil.example")...)
	if r.Status != 403 || !strings.Contains(string(r.Body), "Invalid Host: evil.example") {
		t.Fatalf("foreign Host: %d %s", r.Status, r.Body)
	}
}

func TestResponseDecode(t *testing.T) {
	var out struct{ A int }
	harness.Response{Body: []byte(`{"A":3}`)}.Decode(t, &out)
	if out.A != 3 {
		t.Fatalf("decoded %+v", out)
	}
}

func TestStartExpectingExitWithABadKey(t *testing.T) {
	const bad = "not-base64"
	code, logs := harness.StartExpectingExit(t, harness.Options{Env: map[string]string{"SECRETS_KEY": bad}})
	if code != 1 || !strings.Contains(logs, "SECRETS_KEY: must be base64 of exactly 32 bytes") {
		t.Fatalf("exit %d, logs %q", code, logs)
	}
	if strings.Contains(logs, bad) {
		t.Fatalf("the logs quote the key value: %q", logs)
	}
}

func TestSwapImageExpectingExitThenRecovers(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	code, logs := s.SwapImageExpectingExit(t, testImage(), "", map[string]string{"SECRETS_KEY": "not-base64"})
	if code != 1 || !strings.Contains(logs, "SECRETS_KEY") {
		t.Fatalf("exit %d, logs %q", code, logs)
	}
	s.SwapImage(t, testImage(), "") // the override was for one run only
	if r := s.Anonymous().Do(t, "GET", "/healthz", nil); r.Status != 200 {
		t.Fatalf("healthz: %d", r.Status)
	}
}

func TestUnsetRemovesADefault(t *testing.T) {
	code, logs := harness.StartExpectingExit(t, harness.Options{Unset: []string{"SECRETS_KEY"}})
	if code != 1 || !strings.Contains(logs, "SECRETS_KEY") {
		t.Fatalf("exit %d, logs %q", code, logs)
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
	s.SwapImage(t, testImage(), "")
	if r := s.Anonymous().Do(t, "GET", "/healthz", nil); r.Status != 200 || string(r.Body) != "ok" {
		t.Fatalf("healthz after swap: %d %q", r.Status, r.Body)
	}
	if count() != 1 {
		t.Fatal("user row lost by the swap")
	}
}

func testImage() string {
	if image := os.Getenv("COMPAT_IMAGE"); image != "" {
		return image
	}
	return "mcp-registry:compat-ts"
}

func TestMCPRecordingHandsOverTheRawAnswers(t *testing.T) {
	s := harness.Shared(t)
	token := s.Admin(t).Token(t, "recording")
	var mu sync.Mutex
	var seen []string
	sess := s.MCPRecording(t, token, func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, string(b))
	})
	if _, err := sess.ListTools(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(seen, "\n")
	// The SDK decodes into its own types and drops what it does not know; the raw bytes keep all.
	if !strings.Contains(all, "list_servers") || !strings.Contains(all, "mcp-registry") {
		t.Fatalf("the recorded answers (%d) do not hold the raw tools/list: %.300s", len(seen), all)
	}
	// Control: a plain session records nothing.
	if _, err := s.MCP(t, token).ListTools(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}
