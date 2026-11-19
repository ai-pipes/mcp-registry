//go:build compat

package compat_test

// Helpers of the operator CLI scenarios (cli_users_test.go, cli_servers_test.go, cli_keys_test.go):
// goldens of CLI results and the masks they need, people and tokens, servers, and the key checks.

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// ---- goldens of CLI results ----

var (
	cliUsageBlock = regexp.MustCompile(`(?s)\nUsage: .*$`)
	cliTokenRe    = regexp.MustCompile(`\bsk_[A-Za-z0-9_-]+`)
	cliCodeRe     = regexp.MustCompile(`\bll_[A-Za-z0-9_-]+`)
	cliUUIDRe     = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	cliTimeRe     = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z`)
	// cliIssueLine is one issue line of an invalid server file: two spaces, the field path, the
	// validator's wording.
	cliIssueLine = regexp.MustCompile(`(?m)^  ([A-Za-z0-9_.()]+): .*$`)
)

// cliCommands are the commands the usage block names.
var cliCommands = []string{"create-user", "create-token", "login-link", "create-server", "sync", "reindex", "rotate-secrets-key", "reset-key-check"}

// cliNormalize masks what varies between runs. subs are (value, placeholder) pairs of the test's own
// unique names, applied first and in order.
func cliNormalize(s *harness.Stack, text string, subs ...string) string {
	if len(subs)%2 != 0 {
		panic("cliNormalize: subs are (value, placeholder) pairs")
	}
	text = cliUsageBlock.ReplaceAllString(text, "\n<USAGE>\n")
	text = strings.ReplaceAll(text, s.BaseURL, "http://localhost:<PORT>")
	for i := 0; i < len(subs); i += 2 {
		text = strings.ReplaceAll(text, subs[i], subs[i+1])
	}
	text = cliTokenRe.ReplaceAllString(text, "sk_<TOKEN>")
	text = cliCodeRe.ReplaceAllString(text, "ll_<CODE>")
	text = cliUUIDRe.ReplaceAllString(text, "<UUID>")
	text = cliTimeRe.ReplaceAllString(text, "<TIME>")
	return text
}

// cliRender is the readable form of a result that goldens hold.
func cliRender(s *harness.Stack, r harness.CLIResult, subs ...string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "exit: %d\n--- stdout ---\n", r.Code)
	section := func(text string) {
		text = cliNormalize(s, text, subs...)
		b.WriteString(text)
		if text != "" && !strings.HasSuffix(text, "\n") {
			b.WriteString("\n<no newline at the end>\n")
		}
	}
	section(r.Stdout)
	b.WriteString("--- stderr ---\n")
	section(r.Stderr)
	return b.Bytes()
}

// cliGolden checks the exit code and compares the result with compat/golden/cli/<name>.txt.
func cliGolden(t *testing.T, s *harness.Stack, name string, r harness.CLIResult, wantCode int, subs ...string) {
	t.Helper()
	if r.Code != wantCode {
		t.Fatalf("%s: exit code %d, want %d: %v", name, r.Code, wantCode, r)
	}
	harness.Golden(t, "cli/"+name+".txt", cliRender(s, r, subs...))
}

// cliExpectUsage checks that a bad invocation printed the usage after its error line, and nothing
// on stdout.
func cliExpectUsage(t *testing.T, what string, r harness.CLIResult) {
	t.Helper()
	if r.Code != 2 || r.Stdout != "" || !strings.HasPrefix(r.Stderr, "error: ") || !strings.Contains(r.Stderr, "\nUsage: ") {
		t.Fatalf("%s: want exit 2, an error line and the usage: %v", what, r)
	}
	for _, c := range cliCommands {
		if !strings.Contains(r.Stderr, c) {
			t.Errorf("%s: the usage does not name %s", what, c)
		}
	}
}

// ---- people and tokens ----

type cliPerson struct{ ID, Role, Name string }

func cliUser(t *testing.T, s *harness.Stack, sub string) (cliPerson, bool) {
	t.Helper()
	var p cliPerson
	rows, err := s.DB.Query(t.Context(), `select id::text, role, name from identity.users where oidc_sub = $1`, sub)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		return p, false
	}
	if err := rows.Scan(&p.ID, &p.Role, &p.Name); err != nil {
		t.Fatal(err)
	}
	return p, true
}

func cliMustUser(t *testing.T, s *harness.Stack, sub string) cliPerson {
	t.Helper()
	p, ok := cliUser(t, s, sub)
	if !ok {
		t.Fatalf("no person with sub %s", sub)
	}
	return p
}

// cliOK runs a command that must succeed.
func cliOK(t *testing.T, s *harness.Stack, stdin string, args ...string) harness.CLIResult {
	t.Helper()
	r := s.CLI(t, stdin, args...)
	if r.Code != 0 {
		t.Fatalf("cli %v: %v", args[:1], r)
	}
	return r
}

// cliSignIn creates (or updates) a person and signs them in with a link.
func cliSignIn(t *testing.T, s *harness.Stack, sub, name string, admin bool) *harness.Client {
	t.Helper()
	args := []string{"create-user", "--sub", sub, "--name", name}
	if admin {
		args = append(args, "--admin")
	}
	cliOK(t, s, "", args...)
	c := s.Anonymous()
	if r := identityPostLink(t, c, identityLinkFor(t, s, sub)); r.Status != 204 {
		t.Fatalf("sign in %s: %d %s", sub, r.Status, r.Body)
	}
	return c
}

// cliMetaIs compares an audit entry's meta with the expected JSON (keys are sorted by encoding/json).
func cliMetaIs(t *testing.T, e auditEntry, want string) {
	t.Helper()
	if got := mustJSON(t, e.Meta); got != want {
		t.Errorf("audit %s meta %s, want %s", e.Action, got, want)
	}
}

func cliAudit(t *testing.T, s *harness.Stack, action, target string) []auditEntry {
	t.Helper()
	return auditRows(t, s, "where action = $1 and target = $2 order by id", action, target)
}

// cliToolNames lists the tools an MCP session shows, sorted.
func cliToolNames(t *testing.T, sess *mcp.ClientSession) []string {
	t.Helper()
	res, err := sess.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

var cliMetaTools = []string{"call_tool", "describe_tool", "list_servers", "search_tools"}

var cliLinkRe = regexp.MustCompile(`^(.+)/login#link=(ll_[A-Za-z0-9_-]{43})\n$`)

// ---- create-server, sync, reindex ----

// cliServerFile places a ServerInput file inside the app container and returns its path.
func cliServerFile(t *testing.T, s *harness.Stack, up *fakes.Upstream, slug, authKind string, opts ...fakes.ServerOpt) string {
	t.Helper()
	path := "/tmp/" + slug + ".json"
	s.PutFile(t, path, []byte(mustJSON(t, fakes.ServerInput(up, slug, authKind, opts...))))
	return path
}

// cliExpectNoSecret checks that a secret is nowhere it must not be: in the CLI's output, in the
// app's log, in the audit trail, and in the database in the clear (it is stored sealed).
func cliExpectNoSecret(t *testing.T, s *harness.Stack, secret string, results ...harness.CLIResult) {
	t.Helper()
	for i, r := range results {
		if strings.Contains(r.Stdout, secret) || strings.Contains(r.Stderr, secret) {
			t.Errorf("the CLI output #%d contains the secret %s", i, harness.Redact(secret))
		}
	}
	if strings.Contains(s.Logs(t), secret) {
		t.Errorf("the app log contains the secret %s", harness.Redact(secret))
	}
	if n := countSQL(t, s, `select count(*) from platform.audit_log a where position($1 in a::text) > 0`, secret); n != 0 {
		t.Errorf("%d audit record(s) contain the secret %s", n, harness.Redact(secret))
	}
	if n := countSQL(t, s, `select count(*) from catalog.servers where position(convert_to($1, 'UTF8') in secret) > 0`, secret); n != 0 {
		t.Errorf("the secret %s is stored in the clear in %d server row(s)", harness.Redact(secret), n)
	}
}

func cliServerCount(t *testing.T, s *harness.Stack) int {
	t.Helper()
	return countSQL(t, s, `select count(*) from catalog.servers`)
}

// cliSyncAudit returns the server.sync audit entries of a server, oldest first.
func cliSyncAudit(t *testing.T, s *harness.Stack, slug string) []auditEntry {
	t.Helper()
	return auditRows(t, s, "where action = 'server.sync' and target = $1 order by id", slug)
}

func cliSealed(t *testing.T, s *harness.Stack, slug string) []byte {
	t.Helper()
	var b []byte
	if err := s.DB.QueryRow(t.Context(), `select secret from catalog.servers where slug = $1`, slug).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- the keys: reset-key-check, the key guard, a newer database, rotate-secrets-key ----

const (
	cliKeyWarning = `warning: SECRETS_KEY does not match the data in this database. See "Recovering from a key mismatch" in the README.`
	cliKeyRefusal = `error: SECRETS_KEY does not match the data in this database; refusing to seal a new secret with it. See "Recovering from a key mismatch" in the README.`
)

// cliExpectWarning checks that stderr is exactly the one warning line of the key guard (and an
// optional own error line after it).
func cliExpectWarning(t *testing.T, what string, r harness.CLIResult) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(r.Stderr, "\n"), "\n")
	var warnings int
	for _, l := range lines {
		if strings.HasPrefix(l, "warning:") {
			warnings++
		}
	}
	if warnings != 1 || lines[0] != cliKeyWarning {
		t.Errorf("%s: want one warning line first, got stderr %q", what, r.Stderr)
	}
}

// cliSwap restarts the app with these keys (the stack's own SECRETS_KEY is not changed by a swap).
func cliSwap(t *testing.T, s *harness.Stack, key string) {
	t.Helper()
	s.SwapImage(t, platformImage(), "", map[string]string{"SECRETS_KEY": key})
}

// cliSecretCounts is the number of server secrets and of personal connections that rotate-secrets-key
// counts, read from the database.
func cliSecretCounts(t *testing.T, s *harness.Stack) (secrets, conns int) {
	t.Helper()
	return countSQL(t, s, `select count(*) from catalog.servers where secret is not null`),
		countSQL(t, s, `select count(*) from connections.connections`)
}

// cliRotateSubs masks the counts of a rotate-secrets-key result: its lines are checked against the
// numbers a, b (re-sealed, already under the new key) the test derived from the database, and the
// golden holds the placeholders pa, pb instead of them.
func cliRotateSubs(label string, a, b int, pa, pb string) []string {
	return []string{
		fmt.Sprintf("%s: %d re-sealed, %d already under", label, a, b),
		fmt.Sprintf("%s: %s re-sealed, %s already under", label, pa, pb),
	}
}

func cliRotateStdout(servers, conns [2]int, keyCheck string) string {
	return fmt.Sprintf("server secrets: %d re-sealed, %d already under the new key\nconnections: %d re-sealed, %d already under the new key\nkey check: %s\n",
		servers[0], servers[1], conns[0], conns[1], keyCheck)
}
