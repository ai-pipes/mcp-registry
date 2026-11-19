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
	// Control: a plain session (no recorder) hands nothing over: the count does not move.
	before := len(seen)
	mu.Unlock()
	if _, err := s.MCP(t, token).ListTools(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(seen) != before {
		t.Fatalf("a plain session recorded %d answers", len(seen)-before)
	}
	// And the recording session still records (so the equality above is not a dead recorder).
	mu.Unlock()
	if _, err := sess.ListTools(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(seen) <= before {
		t.Fatal("the recording session recorded nothing for a second call")
	}
}

func TestCLIWithEnvRunsInAOneOffContainerWithTheStacksFiles(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	if r := s.CLIWithEnv(t, nil, "", "--version"); r.Code != 0 || strings.TrimSpace(r.Stdout) == "" {
		t.Fatalf("--version in a one-off container: %+v", r)
	}
	// The environment is the stack's with the overrides on top; an empty value removes a variable.
	bare := s.CLIWithEnv(t, map[string]string{"DATABASE_URL": ""}, "", "create-user", "--sub", "x", "--name", "x")
	if bare.Code != 1 || !strings.Contains(bare.Stderr, "DATABASE_URL") {
		t.Fatalf("without DATABASE_URL: %+v", bare)
	}
	// A file placed with PutFile is visible to the app's CLI and to a one-off container, and the
	// CLI's own answer about a missing file differs (the control).
	s.PutFile(t, "/tmp/harness-put.json", []byte("{nope"))
	want := "/tmp/harness-put.json is not valid JSON"
	for name, r := range map[string]harness.CLIResult{
		"docker exec": s.CLI(t, "", "create-server", "--json", "/tmp/harness-put.json", "--as", "x"),
		"one-off":     s.CLIWithEnv(t, nil, "", "create-server", "--json", "/tmp/harness-put.json", "--as", "x"),
	} {
		if r.Code != 1 || !strings.Contains(r.Stderr, want) {
			t.Fatalf("%s with the placed file: %+v", name, r)
		}
	}
	if r := s.CLIWithEnv(t, nil, "", "create-server", "--json", "/tmp/harness-absent.json", "--as", "x"); !strings.Contains(r.Stderr, "cannot read /tmp/harness-absent.json") {
		t.Fatalf("a file that was never placed: %+v", r)
	}
	// The file follows the app to a new container.
	s.SwapImage(t, testImage(), "")
	if r := s.CLI(t, "", "create-server", "--json", "/tmp/harness-put.json", "--as", "x"); !strings.Contains(r.Stderr, want) {
		t.Fatalf("the placed file after SwapImage: %+v", r)
	}
}

func TestCLIWithEnvRunsWhileTheAppIsDown(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	if code, logs := s.SwapImageExpectingExit(t, testImage(), "", map[string]string{"SECRETS_KEY": "not-base64"}); code != 1 {
		t.Fatalf("exit %d, logs %q", code, logs)
	}
	if r := s.CLIWithEnv(t, nil, "", "--version"); r.Code != 0 {
		t.Fatalf("the CLI with the app down: %+v", r)
	}
	s.PutFile(t, "/tmp/harness-down.json", []byte("{nope")) // no app container to copy into: only remembered
	if r := s.CLIWithEnv(t, nil, "", "create-server", "--json", "/tmp/harness-down.json", "--as", "x"); !strings.Contains(r.Stderr, "is not valid JSON") {
		t.Fatalf("a file placed while the app is down: %+v", r)
	}
	s.SwapImage(t, testImage(), "")
	if r := s.CLI(t, "", "create-server", "--json", "/tmp/harness-down.json", "--as", "x"); !strings.Contains(r.Stderr, "is not valid JSON") {
		t.Fatalf("the file after the app came back: %+v", r)
	}
}

func TestStartWithAnExplicitImageAndCLI(t *testing.T) {
	s := harness.Start(t, harness.Options{Image: testImage(), CLI: "node   dist/cli.js"})
	if r := s.CLI(t, "", "--version"); r.Code != 0 || strings.TrimSpace(r.Stdout) == "" {
		t.Fatalf("--version: %+v", r)
	}
}
