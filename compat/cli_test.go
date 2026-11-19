//go:build compat

package compat_test

// Operator CLI scenarios: `--version`, create-user, create-token, login-link, create-server, sync,
// reindex, reset-key-check, rotate-secrets-key, the key guard and the refusal of a database from a
// newer version. Source: apps/server/test/cli.int.test.ts, scripts/smoke.sh and
// scripts/quickstart-smoke.sh.
//
// The CLI is part of the stable surface ("CLI commands and exit codes"), so unlike log messages the
// texts it prints are pinned exactly: one golden file per command and case under
// compat/golden/cli/<command>[-<case>].txt holds the exit code, stdout and stderr, with what varies
// between runs masked by cliNormalize (tokens, link codes, UUIDs, timestamps, the port, the version
// and the test's own unique slugs and subs). Every case also checks the EFFECT of the command in
// the database or through the API, next to a negative control. Exact texts are compared for what
// the product writes. Two things are not: the usage block that follows a bad invocation (its first
// line carries the CLI's invocation prefix, which differs between images, and it is a help text;
// the scenarios check that it is there and names every command, and the golden holds <USAGE>), and
// the wording of validator messages (zod's issue lines of an invalid server file, the configuration
// error of a missing environment): those are checked by their structure, not by their text.
//
// Stdin: the secret prompt reads the whole of a pipe, or one line without echo on a terminal. A
// black box has no terminal, so only the pipe is pinned (the no-echo path is not testable here).
// The CLI is run through `docker exec` in the app container (Stack.CLI) and, when the environment
// has to differ or the app is not running, in a one-off container of the same image against the
// same database (Stack.CLIWithEnv).
//
// Covered elsewhere and not repeated: rotate-secrets-key end to end (a stack restarted with the new
// key; the check re-sealed, "already current"), a start with a mismatching key and the key-check
// records of the app (TestPlatformMigrationsAndKeyCheck), an unknown command (the harness tests),
// the first-start link (TestFirstStartAnnouncement). The scenarios here add the CLI side of the
// key guard, the exact outputs, the failure outputs of rotate-secrets-key and what it does to the
// data the app serves afterwards.
//
// Source cases (cli.int.test.ts) and where they are ported:
//   - "--version prints the version and exits 0"                 TestCLIVersion
//   - "login-link prints a link that signs the person in"         TestCLILoginLink
//   - "login-link for an unknown sub exits 1 ..."                 TestCLILoginLink
//   - "login-link without a sub exits 2"                          TestCLILoginLink
//   - "create-user is idempotent by sub"                          TestCLICreateUser
//   - "create-token prints an sk_ token ... the token works"      TestCLICreateToken
//   - "prints usage for an unknown command or bad arguments"      the unknown command is a harness test;
//     the bad arguments are cases of every command
//   - "create-server takes the secret from --secret-env ..."      TestCLIServers (env)
//   - "create-server reads the secret from stdin ..."             TestCLIServers (stdin)
//   - "create-server does not read stdin for a mode without ..."  TestCLIServers (none)
//   - "create-server refuses invalid JSON input, an unset ..."    TestCLIServers (refusals)
//   - "a failed first sync is reported, but the server is created" TestCLIServers (sync failed)
//   - "sync re-reads the tools, as an admin or as the scheduler"  TestCLISync
//   - "reindex rebuilds the search index and reports the count"   TestCLIReindex
//   - rotate-secrets-key: "re-seals server secrets and connections", "re-seals the key check",
//     "already current on a repeat", "fails naming the key check", "refuses to run without
//     SECRETS_KEY_OLD", "fails and names what neither key opens"  TestCLIKeys (rotate)
//   - reset-key-check: three cases                                TestCLIKeys (reset)
//   - cli on a key mismatch: four cases                           TestCLIKeys (guard)
// Not ported: the cases that count how often stdin was read ("stdinRead"): a black box cannot see
// it; the effects that follow from it (the environment variable wins over stdin, stdin is ignored
// for a mode without a secret) are asserted instead.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
		t.Fatalf("%s: exit code %d, want %d\nstdout: %s\nstderr: %s", name, r.Code, wantCode, r.Stdout, r.Stderr)
	}
	harness.Golden(t, "cli/"+name+".txt", cliRender(s, r, subs...))
}

// cliExpectUsage checks that a bad invocation printed the usage after its error line, and nothing
// on stdout.
func cliExpectUsage(t *testing.T, what string, r harness.CLIResult) {
	t.Helper()
	if r.Code != 2 || r.Stdout != "" || !strings.HasPrefix(r.Stderr, "error: ") || !strings.Contains(r.Stderr, "\nUsage: ") {
		t.Fatalf("%s: want exit 2, an error line and the usage: code %d stdout %q stderr %.200q", what, r.Code, r.Stdout, r.Stderr)
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
		t.Fatalf("cli %v: exit %d stdout %q stderr %q", args[:1], r.Code, harness.Redact(r.Stdout), r.Stderr)
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

// ---- --version ----

func TestCLIVersion(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	var v struct {
		Version string `json:"version"`
	}
	r := admin.Do(t, "GET", "/api/version", nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/version: %d %s", r.Status, r.Body)
	}
	r.JSON(t, "VersionResponse", &v)
	if v.Version == "" {
		t.Fatal("GET /api/version: empty version")
	}

	res := s.CLI(t, "", "--version")
	if res.Stdout != v.Version+"\n" || res.Stderr != "" {
		t.Fatalf("--version: stdout %q stderr %q, want the version %q alone (GET /api/version)", res.Stdout, res.Stderr, v.Version)
	}
	cliGolden(t, s, "version", res, 0, v.Version, "<VERSION>")

	// The app says which version it runs in its log (the record of the start).
	recs, _ := logRecords(t, s.Logs(t))
	if !slices.ContainsFunc(recs, func(rec map[string]any) bool { return rec["version"] == v.Version }) {
		t.Errorf("no log record carries version %q", v.Version)
	}

	// --version needs neither the configuration nor the database; every other command needs both.
	bare := map[string]string{"DATABASE_URL": "", "SECRETS_KEY": "", "PUBLIC_URL": ""}
	alone := s.CLIWithEnv(t, bare, "", "--version")
	if alone.Code != 0 || alone.Stdout != v.Version+"\n" || alone.Stderr != "" {
		t.Errorf("--version without configuration: %+v", alone)
	}
	control := s.CLIWithEnv(t, bare, "", "create-user", "--sub", harness.Unique("nocfg"), "--name", "x")
	if control.Code != 1 || control.Stdout != "" || !strings.HasPrefix(control.Stderr, "Invalid configuration:\n") {
		t.Fatalf("a command without configuration: %+v", control)
	}
	for _, name := range []string{"DATABASE_URL", "SECRETS_KEY", "PUBLIC_URL"} {
		if !strings.Contains(control.Stderr, "  "+name+": ") {
			t.Errorf("the configuration error does not name %s: %q", name, control.Stderr)
		}
	}

	// "version" without the dashes is not a command.
	if u := s.CLI(t, "", "version"); u.Code != 2 || !strings.HasPrefix(u.Stderr, `error: unknown command "version"`) {
		t.Errorf("version without dashes: %+v", u)
	}
}

// ---- create-user ----

func TestCLICreateUser(t *testing.T) {
	s := harness.Shared(t)
	sub := harness.Unique("cli-user")
	subs := []string{sub, "<SUB>"}
	var id string

	t.Run("creates a member who can sign in; the audit says who and how", func(t *testing.T) {
		r := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Ann Example")
		cliGolden(t, s, "create-user-member", r, 0, subs...)
		p := cliMustUser(t, s, sub)
		id = p.ID
		if p.Role != "member" || p.Name != "Ann Example" {
			t.Fatalf("stored person: %+v", p)
		}
		want := fmt.Sprintf("created member \"Ann Example\" (sub %s, id %s)\n", sub, id)
		if r.Stdout != want || r.Stderr != "" {
			t.Fatalf("output %q, want %q", r.Stdout, want)
		}
		c := s.Anonymous()
		if lr := identityPostLink(t, c, identityLinkFor(t, s, sub)); lr.Status != 204 {
			t.Fatalf("sign in: %d", lr.Status)
		}
		if me := whoami(t, c); me.UserID != id || me.Role != "member" || me.Name != "Ann Example" {
			t.Fatalf("me: %+v", me)
		}
		// Control: the member is not an administrator.
		if lr := c.Do(t, "GET", "/api/servers", nil); lr.Status != 403 {
			t.Fatalf("a member listing /api/servers: %d", lr.Status)
		}
		entries := cliAudit(t, s, "user.provision", id)
		if len(entries) != 1 || entries[0].ActorID != nil {
			t.Fatalf("user.provision audit: %+v", entries)
		}
		cliMetaIs(t, entries[0], `{"created":true,"role":"member","via":"cli"}`)
	})

	t.Run("is idempotent by sub: the same person, updated, and a new name replaces the old", func(t *testing.T) {
		again := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Ann Example")
		cliGolden(t, s, "create-user-again", again, 0, subs...)
		renamed := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Ann Renamed")
		cliGolden(t, s, "create-user-rename", renamed, 0, subs...)
		if n := countSQL(t, s, `select count(*) from identity.users where oidc_sub = $1`, sub); n != 1 {
			t.Fatalf("%d people with sub %s", n, sub)
		}
		if p := cliMustUser(t, s, sub); p.ID != id || p.Name != "Ann Renamed" || p.Role != "member" {
			t.Fatalf("after the re-runs: %+v (id was %s)", p, id)
		}
		if !strings.Contains(again.Stdout, "id "+id+")") || !strings.Contains(renamed.Stdout, "id "+id+")") {
			t.Errorf("a re-run named another id: %q %q", again.Stdout, renamed.Stdout)
		}
		entries := cliAudit(t, s, "user.provision", id)
		if len(entries) != 3 {
			t.Fatalf("user.provision audit rows: %d, want 3 (one per run)", len(entries))
		}
		cliMetaIs(t, entries[1], `{"created":false,"role":"member","via":"cli"}`)
		// Control: another sub is another person.
		other := harness.Unique("cli-other")
		o := cliOK(t, s, "", "create-user", "--sub", other, "--name", "Ann Example")
		if !strings.HasPrefix(o.Stdout, "created member ") || strings.Contains(o.Stdout, id) {
			t.Fatalf("another sub: %q", o.Stdout)
		}
	})

	t.Run("an existing member run again with --admin is promoted, and without it an admin is demoted", func(t *testing.T) {
		promoted := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Ann Renamed", "--admin")
		cliGolden(t, s, "create-user-promote", promoted, 0, subs...)
		if p := cliMustUser(t, s, sub); p.ID != id || p.Role != "admin" {
			t.Fatalf("after --admin: %+v", p)
		}
		c := s.Anonymous()
		if lr := identityPostLink(t, c, identityLinkFor(t, s, sub)); lr.Status != 204 {
			t.Fatalf("sign in: %d", lr.Status)
		}
		if me := whoami(t, c); me.Role != "admin" {
			t.Fatalf("me after --admin: %+v", me)
		}
		if lr := c.Do(t, "GET", "/api/servers", nil); lr.Status != 200 {
			t.Fatalf("an admin listing /api/servers: %d", lr.Status)
		}
		entries := cliAudit(t, s, "user.provision", id)
		cliMetaIs(t, entries[len(entries)-1], `{"created":false,"role":"admin","via":"cli"}`)

		// The usage says it: without --admin an existing admin becomes a member.
		demoted := s.CLI(t, "", "create-user", "--sub", sub, "--name", "Ann Renamed")
		cliGolden(t, s, "create-user-demote", demoted, 0, subs...)
		if p := cliMustUser(t, s, sub); p.Role != "member" {
			t.Fatalf("after the run without --admin: %+v", p)
		}
	})

	t.Run("a new person with --admin is an admin", func(t *testing.T) {
		adminSub := harness.Unique("cli-admin")
		r := s.CLI(t, "", "create-user", "--sub", adminSub, "--name", "Root Example", "--admin")
		cliGolden(t, s, "create-user-admin", r, 0, adminSub, "<ADMIN-SUB>")
		if p := cliMustUser(t, s, adminSub); p.Role != "admin" || p.Name != "Root Example" {
			t.Fatalf("stored: %+v", p)
		}
	})

	t.Run("sub and name are trimmed", func(t *testing.T) {
		padded := harness.Unique("cli-pad")
		r := cliOK(t, s, "", "create-user", "--sub", "  "+padded+" ", "--name", "  Padded Name ")
		p := cliMustUser(t, s, padded)
		want := fmt.Sprintf("created member \"Padded Name\" (sub %s, id %s)\n", padded, p.ID)
		if r.Stdout != want || p.Name != "Padded Name" {
			t.Fatalf("output %q, stored %+v, want %q", r.Stdout, p, want)
		}
	})

	t.Run("a bad invocation exits 2 with the usage and creates nobody", func(t *testing.T) {
		missing := harness.Unique("cli-missing")
		before := countSQL(t, s, `select count(*) from identity.users`)
		cases := []struct {
			name, golden string
			args         []string
		}{
			{"without --sub", "create-user-missing-sub", []string{"create-user", "--name", "x"}},
			{"without --name", "create-user-missing-name", []string{"create-user", "--sub", missing}},
			{"an empty --name", "create-user-empty-name", []string{"create-user", "--sub", missing, "--name", ""}},
			{"a stray argument", "create-user-stray-argument", []string{"create-user", "stray", "--sub", missing, "--name", "x"}},
		}
		for _, c := range cases {
			r := s.CLI(t, "", c.args...)
			cliExpectUsage(t, c.name, r)
			cliGolden(t, s, c.golden, r, 2, missing, "<SUB>")
		}
		// An unknown flag is refused too; the wording is node's, so only the shape is pinned.
		r := s.CLI(t, "", "create-user", "--sub", missing, "--name", "x", "--bogus")
		cliExpectUsage(t, "an unknown flag", r)
		if n := countSQL(t, s, `select count(*) from identity.users`); n != before {
			t.Fatalf("a refused invocation changed the number of people: %d -> %d", before, n)
		}
		// Control: the same arguments, complete, succeed.
		cliOK(t, s, "", "create-user", "--sub", missing, "--name", "x")
	})

	t.Run("a disabled person is refused with exit 1 and stays as they were", func(t *testing.T) {
		dsub := harness.Unique("cli-disabled")
		cliOK(t, s, "", "create-user", "--sub", dsub, "--name", "Gone Example")
		admin := s.Admin(t)
		did := cliMustUser(t, s, dsub).ID
		if r := admin.Do(t, "POST", "/api/users/"+did+"/disabled", map[string]bool{"disabled": true}); r.Status != 204 {
			t.Fatalf("disable: %d %s", r.Status, r.Body)
		}
		r := s.CLI(t, "", "create-user", "--sub", dsub, "--name", "Changed Name")
		cliGolden(t, s, "create-user-disabled", r, 1)
		if p := cliMustUser(t, s, dsub); p.Name != "Gone Example" {
			t.Fatalf("a refused run changed the person: %+v", p)
		}
	})
}

// ---- create-token ----

func TestCLICreateToken(t *testing.T) {
	s := harness.Shared(t)
	sub := harness.Unique("cli-tok")
	cliOK(t, s, "", "create-user", "--sub", sub, "--name", "Token Owner")
	owner := cliMustUser(t, s, sub)
	subs := []string{sub, "<SUB>"}

	r := s.CLI(t, "", "create-token", sub, "--name", "laptop")
	cliGolden(t, s, "create-token", r, 0, subs...)
	lines := strings.Split(strings.TrimSuffix(r.Stdout, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "sk_") {
		t.Fatalf("create-token stdout: %d lines", len(lines))
	}
	token := lines[0]
	wantCommand := fmt.Sprintf(`claude mcp add --transport http mcp-registry %s/mcp --header "Authorization: Bearer %s"`, s.BaseURL, token)
	if lines[1] != wantCommand {
		t.Fatalf("connect command %q, want %q", harness.Redact(lines[1]), harness.Redact(wantCommand))
	}
	if r.Stderr != "The token is shown only once; store it now.\n" {
		t.Fatalf("stderr %q", r.Stderr)
	}

	t.Run("the token opens /mcp; a changed token does not", func(t *testing.T) {
		if names := cliToolNames(t, s.MCP(t, token)); !slices.Equal(names, cliMetaTools) {
			t.Fatalf("tools of the token's session: %v", names)
		}
		bad := token[:len(token)-1] + map[bool]string{true: "A", false: "B"}[token[len(token)-1] != 'A']
		if r := identityMCPInitialize(t, s.Anonymous(), bad); r.Status != 401 {
			t.Fatalf("a changed token: %d, want 401", r.Status)
		}
		if r := identityMCPInitialize(t, s.Anonymous(), token); r.Status != 200 {
			t.Fatalf("the token: %d, want 200", r.Status)
		}
	})

	t.Run("only the hash is stored; the audit names the owner as the actor", func(t *testing.T) {
		var id, hash, name, prefix string
		if err := s.DB.QueryRow(t.Context(), `select id::text, hash, name, prefix from identity.tokens where user_id = $1::uuid`, owner.ID).Scan(&id, &hash, &name, &prefix); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(token))
		if hash != hex.EncodeToString(sum[:]) || name != "laptop" || prefix != token[:10] {
			t.Fatalf("stored token row: hash ok %v, name %q, prefix %q", hash == hex.EncodeToString(sum[:]), name, prefix)
		}
		if n := countSQL(t, s, `select count(*) from identity.tokens t where position($1 in t::text) > 0`, token); n != 0 {
			t.Fatalf("the token itself is stored in %d row(s)", n)
		}
		entries := cliAudit(t, s, "token.create", id)
		if len(entries) != 1 || !ptrIs(entries[0].ActorID, owner.ID) {
			t.Fatalf("token.create audit: %+v (the actor must be the token's owner %s)", entries, owner.ID)
		}
		cliMetaIs(t, entries[0], fmt.Sprintf(`{"name":"laptop","prefix":%q}`, token[:10]))
	})

	t.Run("a second token for the same person is another token that works too", func(t *testing.T) {
		second := cliOK(t, s, "", "create-token", sub, "--name", "laptop")
		other := strings.SplitN(second.Stdout, "\n", 2)[0]
		if other == token || !strings.HasPrefix(other, "sk_") {
			t.Fatalf("second token %q", harness.Redact(other))
		}
		if names := cliToolNames(t, s.MCP(t, other)); !slices.Equal(names, cliMetaTools) {
			t.Fatalf("tools of the second token: %v", names)
		}
		if n := countSQL(t, s, `select count(*) from identity.tokens where user_id = $1::uuid`, owner.ID); n != 2 {
			t.Fatalf("%d token rows, want 2", n)
		}
	})

	t.Run("refusals: an unknown person (1), a disabled one (1), missing arguments (2); nothing is created", func(t *testing.T) {
		before := countSQL(t, s, `select count(*) from identity.tokens`)
		ghost := harness.Unique("cli-ghost")
		unknown := s.CLI(t, "", "create-token", ghost, "--name", "x")
		cliGolden(t, s, "create-token-unknown", unknown, 1, ghost, "<SUB>")

		dsub := harness.Unique("cli-tokdis")
		cliOK(t, s, "", "create-user", "--sub", dsub, "--name", "Gone")
		execSQL(t, s, `update identity.users set disabled = true where oidc_sub = $1`, dsub)
		disabled := s.CLI(t, "", "create-token", dsub, "--name", "x")
		cliGolden(t, s, "create-token-disabled", disabled, 1)

		noName := s.CLI(t, "", "create-token", sub)
		cliExpectUsage(t, "create-token without --name", noName)
		cliGolden(t, s, "create-token-missing-name", noName, 2)
		noSub := s.CLI(t, "", "create-token", "--name", "x")
		cliExpectUsage(t, "create-token without a sub", noSub)
		cliGolden(t, s, "create-token-missing-sub", noSub, 2)
		if n := countSQL(t, s, `select count(*) from identity.tokens`); n != before {
			t.Fatalf("refused runs created tokens: %d -> %d", before, n)
		}
	})
}

// ---- login-link ----

var cliLinkRe = regexp.MustCompile(`^(.+)/login#link=(ll_[A-Za-z0-9_-]{43})\n$`)

func TestCLILoginLink(t *testing.T) {
	s := harness.Shared(t)
	sub := harness.Unique("cli-link")
	cliOK(t, s, "", "create-user", "--sub", sub, "--name", "Link Person")
	person := cliMustUser(t, s, sub)
	subs := []string{sub, "<SUB>"}

	first := s.CLI(t, "", "login-link", sub)
	cliGolden(t, s, "login-link", first, 0, subs...)
	m := cliLinkRe.FindStringSubmatch(first.Stdout)
	if m == nil || m[1] != s.BaseURL || first.Stderr != "" {
		t.Fatalf("login-link output %q stderr %q, want <PUBLIC_URL>/login#link=ll_<43 characters>", harness.Redact(first.Stdout), first.Stderr)
	}
	firstCode := m[2]

	t.Run("a second run invalidates the first link; the code works once", func(t *testing.T) {
		second := cliOK(t, s, "", "login-link", sub)
		m2 := cliLinkRe.FindStringSubmatch(second.Stdout)
		if m2 == nil || m2[2] == firstCode {
			t.Fatalf("second link %q", harness.Redact(second.Stdout))
		}
		if r := identityPostLink(t, s.Anonymous(), firstCode); r.Status != 401 || errorCode(t, r) != identityInvalidLink {
			t.Fatalf("the first link after a second one: %d %s", r.Status, r.Body)
		}
		c := s.Anonymous()
		if r := identityPostLink(t, c, m2[2]); r.Status != 204 {
			t.Fatalf("the second link: %d %s", r.Status, r.Body)
		}
		if me := whoami(t, c); me.UserID != person.ID {
			t.Fatalf("signed in as %+v, want %s", me, person.ID)
		}
		if r := identityPostLink(t, s.Anonymous(), m2[2]); r.Status != 401 {
			t.Fatalf("the second link used twice: %d", r.Status)
		}
	})

	t.Run("only a hash is stored; the issue is audited without an actor", func(t *testing.T) {
		code := identityLinkFor(t, s, sub)
		if n := countSQL(t, s, `select count(*) from identity.login_links where user_id = $1::uuid`, person.ID); n != 1 {
			t.Fatalf("%d live links for the person, want 1", n)
		}
		if n := countSQL(t, s, `select count(*) from identity.login_links l where position($1 in l::text) > 0`, code); n != 0 {
			t.Fatalf("the code is stored in %d row(s)", n)
		}
		entries := cliAudit(t, s, "auth.link_issued", person.ID)
		if len(entries) < 3 || entries[len(entries)-1].ActorID != nil {
			t.Fatalf("auth.link_issued audit: %+v", entries)
		}
	})

	t.Run("refusals: unknown (1), disabled (1), no sub (2), two subs (2)", func(t *testing.T) {
		ghost := harness.Unique("cli-ghost")
		unknown := s.CLI(t, "", "login-link", ghost)
		cliGolden(t, s, "login-link-unknown", unknown, 1, ghost, "<SUB>")
		if unknown.Stdout != "" {
			t.Fatalf("stdout of a refused login-link: %q", unknown.Stdout)
		}
		dsub := harness.Unique("cli-linkdis")
		cliOK(t, s, "", "create-user", "--sub", dsub, "--name", "Gone")
		execSQL(t, s, `update identity.users set disabled = true where oidc_sub = $1`, dsub)
		disabled := s.CLI(t, "", "login-link", dsub)
		cliGolden(t, s, "login-link-disabled", disabled, 1)
		if n := countSQL(t, s, `select count(*) from identity.login_links where user_id = $1::uuid`, cliMustUser(t, s, dsub).ID); n != 0 {
			t.Fatalf("a link was stored for a disabled person")
		}
		none := s.CLI(t, "", "login-link")
		cliExpectUsage(t, "login-link without a sub", none)
		cliGolden(t, s, "login-link-missing-sub", none, 2)
		two := s.CLI(t, "", "login-link", sub, "extra")
		cliExpectUsage(t, "login-link with two arguments", two)
		cliGolden(t, s, "login-link-two-arguments", two, 2, sub, "<SUB>")
	})
}

// ---- login-link with OIDC configured ----

func TestCLILoginLinkWithOIDC(t *testing.T) {
	idp := fakes.StartIdP(t)
	s := harness.Start(t, harness.Options{OIDC: idp})
	sub := harness.Unique("cli-oidc")
	cliOK(t, s, "", "create-user", "--sub", sub, "--name", "Break Glass")
	var methods struct {
		OIDC bool `json:"oidc"`
	}
	s.Anonymous().Do(t, "GET", "/api/auth/methods", nil).JSON(t, "AuthMethods", &methods)
	if !methods.OIDC {
		t.Fatal("the stack has no OIDC: the scenario proves nothing")
	}
	r := s.CLI(t, "", "login-link", sub)
	cliGolden(t, s, "login-link-oidc", r, 0, sub, "<SUB>")
	m := cliLinkRe.FindStringSubmatch(r.Stdout)
	if m == nil {
		t.Fatalf("output %q", harness.Redact(r.Stdout))
	}
	c := s.Anonymous()
	if lr := identityPostLink(t, c, m[2]); lr.Status != 204 {
		t.Fatalf("the link with OIDC configured: %d %s", lr.Status, lr.Body)
	}
	if me := whoami(t, c); me.Name != "Break Glass" {
		t.Fatalf("me: %+v", me)
	}
	// Control: a person that does not exist gets no link here either.
	if u := s.CLI(t, "", "login-link", harness.Unique("cli-ghost")); u.Code != 1 {
		t.Fatalf("unknown person with OIDC configured: %d", u.Code)
	}
}

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

func TestCLIServers(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	ownerSub := harness.Unique("cli-owner")
	memberSub := harness.Unique("cli-member")
	owner := cliSignIn(t, s, ownerSub, "Owner", true)
	cliOK(t, s, "", "create-user", "--sub", memberSub, "--name", "Member")
	ownerID := cliMustUser(t, s, ownerSub).ID
	cliSignIn(t, s, harness.Unique("cli-owner"), "Second Admin", true) // keeps the stack's admin count realistic

	t.Run("create-server reads the secret from stdin, syncs, and never prints it", func(t *testing.T) {
		secret := harness.Unique("sekret-stdin")
		slug := harness.Unique("cli-stdin")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo"), RequireHeader: [2]string{"Authorization", "Bearer " + secret}})
		file := cliServerFile(t, s, up, slug, "shared_secret")
		r := s.CLI(t, secret+"\n", "create-server", "--json", file, "--as", ownerSub)
		cliGolden(t, s, "create-server-stdin", r, 0, slug, "<SLUG>")
		if want := fmt.Sprintf("created server %q (shared_secret, secret set); 1 tool(s)\n", slug); r.Stdout != want || r.Stderr != "" {
			t.Fatalf("output %q stderr %q, want %q", r.Stdout, r.Stderr, want)
		}
		v := catalogGet(t, owner, slug)
		if !v.SecretSet || v.ToolCount != 1 || v.LastSyncError != nil || v.Status != "active" {
			t.Fatalf("server after create-server: %+v", v)
		}
		reqs := up.Requests()
		if len(reqs) == 0 {
			t.Fatal("the upstream was not contacted: the creation did not sync")
		}
		for _, q := range reqs {
			if got := q.Header.Get("Authorization"); got != "Bearer "+secret {
				t.Errorf("an upstream request carried %q, want the secret from stdin without its newline", harness.Redact(got))
			}
		}
		cliExpectNoSecret(t, s, secret, r)
		var actions []string
		for _, e := range auditRows(t, s, "where target = $1 order by id", slug) {
			actions = append(actions, e.Action)
			if !ptrIs(e.ActorID, ownerID) {
				t.Errorf("audit %s: actor %v, want the --as person %s", e.Action, e.ActorID, ownerID)
			}
		}
		if !slices.Equal(actions, []string{"server.create", "server.secret_set", "server.sync"}) {
			t.Fatalf("audit actions of the server: %v", actions)
		}
		syncs := cliSyncAudit(t, s, slug)
		cliMetaIs(t, syncs[0], `{"added":1,"changed":0,"removed":0}`)

		// Control: a wrong secret is refused by the same upstream, so the success above proves that
		// the secret from stdin is the one that reached it.
		wrongSlug := harness.Unique("cli-wrong")
		wrongFile := cliServerFile(t, s, up, wrongSlug, "shared_secret")
		wrong := s.CLI(t, "not-the-secret\n", "create-server", "--json", wrongFile, "--as", ownerSub)
		cliGolden(t, s, "create-server-sync-401", wrong, 0, wrongSlug, "<SLUG>")
		if v := catalogGet(t, owner, wrongSlug); v.ToolCount != 0 || v.LastSyncError == nil {
			t.Fatalf("server with a wrong secret: %+v", v)
		}
	})

	t.Run("create-server takes the secret from --secret-env, which wins over stdin", func(t *testing.T) {
		secret := harness.Unique("sekret-env")
		stdinSecret := harness.Unique("sekret-ignored")
		slug := harness.Unique("cli-env")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo"), RequireHeader: [2]string{"Authorization", "Bearer " + secret}})
		file := cliServerFile(t, s, up, slug, "shared_secret")
		r := s.CLIWithEnv(t, map[string]string{"CLI_SECRET": secret}, stdinSecret+"\n",
			"create-server", "--json", file, "--as", ownerSub, "--secret-env", "CLI_SECRET")
		cliGolden(t, s, "create-server-env", r, 0, slug, "<SLUG>")
		if v := catalogGet(t, owner, slug); !v.SecretSet || v.ToolCount != 1 || v.LastSyncError != nil {
			t.Fatalf("server after create-server --secret-env: %+v (the upstream accepts only the variable's value)", v)
		}
		cliExpectNoSecret(t, s, secret, r)
		cliExpectNoSecret(t, s, stdinSecret, r)
		// Control: the variable must exist; without it nothing is created.
		before := cliServerCount(t, s)
		missing := s.CLIWithEnv(t, nil, "", "create-server", "--json", file, "--as", ownerSub, "--secret-env", "CLI_SECRET_UNSET")
		cliExpectUsage(t, "an unset --secret-env", missing)
		cliGolden(t, s, "create-server-secret-env-unset", missing, 2)
		if n := cliServerCount(t, s); n != before {
			t.Fatalf("an unset --secret-env created a server: %d -> %d", before, n)
		}
	})

	t.Run("a mode without a secret ignores stdin and stores nothing", func(t *testing.T) {
		ignored := harness.Unique("sekret-none")
		slug := harness.Unique("cli-none")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		file := cliServerFile(t, s, up, slug, "none")
		r := s.CLI(t, ignored+"\n", "create-server", "--json", file, "--as", ownerSub)
		cliGolden(t, s, "create-server-none", r, 0, slug, "<SLUG>")
		if v := catalogGet(t, owner, slug); v.SecretSet || v.ToolCount != 1 {
			t.Fatalf("server: %+v", v)
		}
		if n := countSQL(t, s, `select count(*) from catalog.servers where slug = $1 and secret is null`, slug); n != 1 {
			t.Fatal("a secret is stored for a server without one")
		}
		cliExpectNoSecret(t, s, ignored, r)
	})

	t.Run("an empty stdin leaves the server without a secret; a failed first sync does not undo the creation", func(t *testing.T) {
		slug := harness.Unique("cli-nosecret")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		file := cliServerFile(t, s, up, slug, "shared_secret")
		r := s.CLI(t, "", "create-server", "--json", file, "--as", ownerSub)
		// Exit 0: the server exists; the failed sync is part of the report, not of the exit code.
		cliGolden(t, s, "create-server-no-secret", r, 0, slug, "<SLUG>")
		v := catalogGet(t, owner, slug)
		if v.SecretSet || v.ToolCount != 0 || v.syncError() != "The server secret is not set" {
			t.Fatalf("server: %+v (sync error %q)", v, v.syncError())
		}

		failing := harness.Unique("cli-failing")
		fup := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		fup.FailWith(500)
		ffile := cliServerFile(t, s, fup, failing, "shared_secret")
		secret := harness.Unique("sekret-fail")
		fr := s.CLI(t, secret+"\n", "create-server", "--json", ffile, "--as", ownerSub)
		cliGolden(t, s, "create-server-sync-failed", fr, 0, failing, "<SLUG>")
		fv := catalogGet(t, owner, failing)
		if !fv.SecretSet || fv.ToolCount != 0 || fv.syncError() != catalogMsgUnreachable {
			t.Fatalf("server after a failed first sync: %+v (sync error %q)", fv, fv.syncError())
		}
		cliExpectNoSecret(t, s, secret, fr)
		// Control: once the upstream works, a sync fills the tools and clears the error.
		fup.Reset()
		cliOK(t, s, "", "sync", failing)
		if fv := catalogGet(t, owner, failing); fv.ToolCount != 1 || fv.LastSyncError != nil {
			t.Fatalf("server after the upstream recovered: %+v", fv)
		}
	})

	t.Run("an administrator is required; invalid input is refused with exit 1 or 2 and creates nothing", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		slug := harness.Unique("cli-refused")
		file := cliServerFile(t, s, up, slug, "none")
		before := cliServerCount(t, s)
		subs := []string{slug, "<SLUG>"}

		member := s.CLI(t, "", "create-server", "--json", file, "--as", memberSub)
		cliGolden(t, s, "create-server-member", member, 1, subs...)
		ghost := harness.Unique("cli-ghost")
		unknown := s.CLI(t, "", "create-server", "--json", file, "--as", ghost)
		cliGolden(t, s, "create-server-unknown-sub", unknown, 1, ghost, "<SUB>")

		missing := s.CLI(t, "", "create-server", "--json", "/tmp/compat-no-such-file.json", "--as", ownerSub)
		cliGolden(t, s, "create-server-missing-file", missing, 1)
		s.PutFile(t, "/tmp/compat-not-json.json", []byte("{nope"))
		notJSON := s.CLI(t, "", "create-server", "--json", "/tmp/compat-not-json.json", "--as", ownerSub)
		cliGolden(t, s, "create-server-not-json", notJSON, 1)

		s.PutFile(t, "/tmp/compat-bad-url.json", []byte(mustJSON(t, fakes.ServerInput(up, slug, "none", fakes.WithURL("ftp://nope")))))
		badURL := s.CLI(t, "", "create-server", "--json", "/tmp/compat-bad-url.json", "--as", ownerSub)
		cliGolden(t, s, "create-server-invalid-url", badURL, 1, subs...)

		// A file with several missing fields: the issues are listed by field path; their wording is
		// the validator's and is not pinned.
		s.PutFile(t, "/tmp/compat-empty-def.json", []byte(`{"slug":"`+slug+`"}`))
		empty := s.CLI(t, "", "create-server", "--json", "/tmp/compat-empty-def.json", "--as", ownerSub)
		if empty.Code != 1 || empty.Stdout != "" || !strings.HasPrefix(empty.Stderr, "error: /tmp/compat-empty-def.json is not a valid server:\n") {
			t.Fatalf("an incomplete server file: %+v", empty)
		}
		var paths []string
		for _, m := range cliIssueLine.FindAllStringSubmatch(empty.Stderr, -1) {
			paths = append(paths, m[1])
		}
		if !slices.Equal(paths, []string{"name", "description", "url", "auth", "access"}) {
			t.Errorf("fields named by the issues: %v", paths)
		}
		emptyMasked := empty
		emptyMasked.Stderr = cliIssueLine.ReplaceAllString(empty.Stderr, "  $1: <issue>")
		cliGolden(t, s, "create-server-incomplete", emptyMasked, 1, subs...)

		s.PutFile(t, "/tmp/compat-bad-slug.json", []byte(mustJSON(t, fakes.ServerInput(up, "Not A Slug", "none"))))
		badSlug := s.CLI(t, "", "create-server", "--json", "/tmp/compat-bad-slug.json", "--as", ownerSub)
		cliGolden(t, s, "create-server-invalid-slug", badSlug, 1)

		noJSON := s.CLI(t, "", "create-server", "--as", ownerSub)
		cliExpectUsage(t, "create-server without --json", noJSON)
		cliGolden(t, s, "create-server-missing-json", noJSON, 2)
		noAs := s.CLI(t, "", "create-server", "--json", file)
		cliExpectUsage(t, "create-server without --as", noAs)
		cliGolden(t, s, "create-server-missing-as", noAs, 2)

		if n := cliServerCount(t, s); n != before {
			t.Fatalf("refused runs created servers: %d -> %d", before, n)
		}
		if r := owner.Do(t, "GET", "/api/servers/"+slug, nil); r.Status != 404 {
			t.Fatalf("GET /api/servers/%s after the refusals: %d", slug, r.Status)
		}
		// Control: the same file as an administrator creates the server.
		ok := cliOK(t, s, "", "create-server", "--json", file, "--as", ownerSub)
		if ok.Stdout != fmt.Sprintf("created server %q (none); 1 tool(s)\n", slug) {
			t.Fatalf("the control: %q", ok.Stdout)
		}

		// A slug that exists is refused and the first server stays as it was.
		dupSecret := harness.Unique("sekret-dup")
		dupSlug := harness.Unique("cli-dup")
		dup := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		dupFile := cliServerFile(t, s, dup, dupSlug, "shared_secret")
		cliOK(t, s, dupSecret+"\n", "create-server", "--json", dupFile, "--as", ownerSub)
		sealed := cliSealed(t, s, dupSlug)
		again := s.CLI(t, harness.Unique("sekret-other")+"\n", "create-server", "--json", dupFile, "--as", ownerSub)
		cliGolden(t, s, "create-server-duplicate", again, 1, dupSlug, "<SLUG>")
		if !bytes.Equal(sealed, cliSealed(t, s, dupSlug)) {
			t.Fatal("a refused duplicate changed the stored secret")
		}
	})

	t.Run("sync re-reads the tools; with --as it is audited and records the administrator", func(t *testing.T) {
		slug := harness.Unique("cli-sync")
		subs := []string{slug, "<SLUG>"}
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		cliOK(t, s, "", "create-server", "--json", cliServerFile(t, s, up, slug, "none"), "--as", ownerSub)
		creation := len(cliSyncAudit(t, s, slug)) // the first sync of create-server is audited too
		if creation != 1 {
			t.Fatalf("server.sync audit rows after create-server: %d", creation)
		}
		up.Reset()

		// As the scheduler does: no administrator, no audit record.
		r := s.CLI(t, "", "sync", slug)
		cliGolden(t, s, "sync", r, 0, subs...)
		if up.ListRequests() == 0 {
			t.Fatal("sync did not contact the upstream")
		}
		if n := len(cliSyncAudit(t, s, slug)); n != creation {
			t.Fatalf("a sync without --as wrote %d audit record(s)", n-creation)
		}
		if id := catalogSyncUser(t, s, slug); id != ownerID {
			t.Fatalf("sync_user_id %s, want the administrator of the creation %s", id, ownerID)
		}

		up.SetTools(fakes.Tools("echo", "other"))
		added := s.CLI(t, "", "sync", slug, "--as", ownerSub)
		cliGolden(t, s, "sync-added", added, 0, subs...)
		if _, tools := catalogTools(t, owner, slug); !slices.Equal(catalogToolNames(tools), []string{"echo", "other"}) {
			t.Fatalf("tools after the sync: %v", catalogToolNames(tools))
		}
		syncs := cliSyncAudit(t, s, slug)
		if len(syncs) != creation+1 || !ptrIs(syncs[1].ActorID, ownerID) {
			t.Fatalf("server.sync audit after --as: %+v", syncs)
		}
		cliMetaIs(t, syncs[1], `{"added":1,"changed":0,"removed":0}`)

		changed := fakes.Tool{Name: "echo", Description: "A new description.", InputSchema: map[string]any{"type": "object"}}
		up.SetTools([]fakes.Tool{changed})
		both := s.CLI(t, "", "sync", slug, "--as", ownerSub)
		cliGolden(t, s, "sync-changed-removed", both, 0, subs...)
		_, tools := catalogTools(t, owner, slug)
		if !slices.Equal(catalogToolNames(tools), []string{"echo"}) || tools[0].Description != "A new description." {
			t.Fatalf("tools after the second sync: %+v", tools)
		}
	})

	t.Run("sync of a personal-auth server uses the connection of the administrator it names", func(t *testing.T) {
		slug := harness.Unique("cli-pat")
		subs := []string{slug, "<SLUG>"}
		pat := harness.Unique("pat-owner")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		file := cliServerFile(t, s, up, slug, "pat_user", fakes.WithHeader("X-Token", "pat {secret}"))
		created := s.CLI(t, "", "create-server", "--json", file, "--as", ownerSub)
		cliGolden(t, s, "create-server-pat-needs-connection", created, 0, subs...)

		// No connection yet: refused, by the administrator and by the scheduler alike.
		denied := s.CLI(t, "", "sync", slug, "--as", ownerSub)
		cliGolden(t, s, "sync-needs-connection", denied, 1, subs...)
		if sch := s.CLI(t, "", "sync", slug); sch.Code != 1 || sch.Stderr != denied.Stderr {
			t.Fatalf("the scheduler's sync without a connection: %+v", sch)
		}
		failed := cliSyncAudit(t, s, slug)
		cliMetaIs(t, failed[len(failed)-1], `{"error":"needs_connection"}`)
		if v := catalogGet(t, owner, slug); v.syncError() != catalogMsgNeedsConnection || v.ToolCount != 0 {
			t.Fatalf("server after the refused sync: %+v", v)
		}

		if r := owner.Do(t, "PUT", "/api/connections/"+slug+"/pat", map[string]string{"token": pat}); r.Status != 204 {
			t.Fatalf("PUT pat: %d %s", r.Status, r.Body)
		}
		up.Reset()
		ok := s.CLI(t, "", "sync", slug, "--as", ownerSub)
		cliGolden(t, s, "sync-pat", ok, 0, subs...)
		if reqs := up.Requests(); len(reqs) == 0 || slices.ContainsFunc(reqs, func(q fakes.Request) bool { return q.Header.Get("X-Token") != "pat "+pat }) {
			t.Fatalf("the upstream did not get the administrator's token on every request: %d request(s)", len(reqs))
		}
		if v := catalogGet(t, owner, slug); v.ToolCount != 1 || v.LastSyncError != nil {
			t.Fatalf("server after the sync: %+v", v)
		}
		if id := catalogSyncUser(t, s, slug); id != ownerID {
			t.Fatalf("sync_user_id %s, want %s", id, ownerID)
		}

		// The scheduler's sync now lends the last successful administrator's connection.
		up.Reset()
		sched := s.CLI(t, "", "sync", slug)
		if sched.Code != 0 || sched.Stdout != fmt.Sprintf("synced %q: 0 added, 0 changed, 0 removed\n", slug) {
			t.Fatalf("sync without --as after an administrator synced: %+v", sched)
		}
		if reqs := up.Requests(); len(reqs) == 0 || reqs[0].Header.Get("X-Token") != "pat "+pat {
			t.Fatalf("the scheduler's sync did not use the last administrator's token")
		}
		// Control: another administrator without a connection of their own gets none.
		otherSub := harness.Unique("cli-admin2")
		cliOK(t, s, "", "create-user", "--sub", otherSub, "--name", "Other Admin", "--admin")
		other := s.CLI(t, "", "sync", slug, "--as", otherSub)
		cliGolden(t, s, "sync-other-admin-needs-connection", other, 1, subs...)
	})

	t.Run("sync failures and refusals: upstream down, unknown, disabled, not an administrator", func(t *testing.T) {
		slug := harness.Unique("cli-syncerr")
		subs := []string{slug, "<SLUG>"}
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		cliOK(t, s, "", "create-server", "--json", cliServerFile(t, s, up, slug, "none"), "--as", ownerSub)

		up.FailWith(500)
		down := s.CLI(t, "", "sync", slug, "--as", ownerSub)
		cliGolden(t, s, "sync-upstream-failed", down, 1, subs...)
		if v := catalogGet(t, owner, slug); v.syncError() != catalogMsgUnreachable || v.ToolCount != 1 {
			t.Fatalf("server after a failed sync: %+v (the tools must stay)", v)
		}
		syncs := cliSyncAudit(t, s, slug)
		last := syncs[len(syncs)-1]
		if !ptrIs(last.ActorID, ownerID) || last.Meta["error"] == nil {
			t.Fatalf("audit of the failed sync: %+v", last)
		}
		up.Reset()
		cliOK(t, s, "", "sync", slug)
		if v := catalogGet(t, owner, slug); v.LastSyncError != nil {
			t.Fatalf("the error stays after a good sync: %q", v.syncError())
		}

		ghost := harness.Unique("cli-noserver")
		unknown := s.CLI(t, "", "sync", ghost)
		cliGolden(t, s, "sync-unknown", unknown, 1, ghost, "<SLUG>")
		asGhost := harness.Unique("cli-ghost")
		noPerson := s.CLI(t, "", "sync", slug, "--as", asGhost)
		cliGolden(t, s, "sync-as-unknown", noPerson, 1, asGhost, "<SUB>")
		asMember := s.CLI(t, "", "sync", slug, "--as", memberSub)
		cliGolden(t, s, "sync-as-member", asMember, 1)
		missing := s.CLI(t, "", "sync")
		cliExpectUsage(t, "sync without a slug", missing)
		cliGolden(t, s, "sync-missing-slug", missing, 2)

		if r := owner.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]string{"status": "disabled"}); r.Status != 204 {
			t.Fatalf("disable: %d", r.Status)
		}
		off := s.CLI(t, "", "sync", slug)
		cliGolden(t, s, "sync-disabled-server", off, 1, subs...)
		if r := owner.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]string{"status": "active"}); r.Status != 204 {
			t.Fatalf("enable: %d", r.Status)
		}
		if on := s.CLI(t, "", "sync", slug); on.Code != 0 {
			t.Fatalf("sync after enabling: %+v", on)
		}
	})

	t.Run("reindex rebuilds the search index and prints the number of indexed tools", func(t *testing.T) {
		slug := harness.Unique("cli-index")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo", "ping")})
		cliOK(t, s, "", "create-server", "--json", cliServerFile(t, s, up, slug, "none"), "--as", ownerSub)
		catalogWaitDocs(t, s, slug, 2)
		catalogWaitDelivered(t, s)

		type docs map[string]string // ref -> embedding digest
		snapshot := func() (docs, int) {
			rows, err := s.DB.Query(t.Context(), `select ref, md5(embedding::text) from search.documents order by ref`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			d := docs{}
			for rows.Next() {
				var ref string
				var digest *string
				if err := rows.Scan(&ref, &digest); err != nil {
					t.Fatal(err)
				}
				if digest == nil {
					t.Fatalf("%s has no embedding", ref)
				}
				d[ref] = *digest
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			return d, len(d)
		}
		before, total := snapshot()
		sess := s.MCP(t, owner.Token(t, "cli-reindex"))
		if got := catalogHitRefs(catalogSearch(t, sess, "ping", 10, []string{slug})); !slices.Contains(got, slug+".ping") {
			t.Fatalf("search before: %v", got)
		}

		execSQL(t, s, `delete from search.documents where server = $1`, slug)
		if got := catalogHitRefs(catalogSearch(t, sess, "ping", 10, []string{slug})); len(got) != 0 {
			t.Fatalf("search finds %v after the documents were deleted: the control proves nothing", got)
		}

		r := s.CLI(t, "", "reindex")
		// The number is the count of indexed TOOLS, although the text says "server(s)": a known
		// defect of the image, pinned as it is (see the report); the owner has not decided whether
		// the next implementation keeps it.
		cliGolden(t, s, "reindex", r, 0)
		if want := fmt.Sprintf("reindexed %d server(s)\n", total); r.Stdout != want {
			t.Fatalf("reindex printed %q, want %q (the number of rows of search.documents)", r.Stdout, want)
		}
		after, _ := snapshot()
		if len(after) != total {
			t.Fatalf("documents after reindex: %d, before %d", len(after), total)
		}
		for ref, digest := range before {
			if after[ref] != digest {
				t.Errorf("%s: the embedding after reindex differs from the one before", ref)
			}
		}
		if got := catalogHitRefs(catalogSearch(t, sess, "ping", 10, []string{slug})); !slices.Contains(got, slug+".ping") {
			t.Fatalf("search after reindex: %v", got)
		}

		extra := s.CLI(t, "", "reindex", "extra")
		cliExpectUsage(t, "reindex with an argument", extra)
		cliGolden(t, s, "reindex-extra-argument", extra, 2)
	})
}

// ---- the keys: reset-key-check, the key guard, a newer database, rotate-secrets-key ----

const (
	cliKeyWarning = `warning: SECRETS_KEY does not match the data in this database. See "Recovering from a key mismatch" in the README.`
	cliKeyRefusal = `error: SECRETS_KEY does not match the data in this database; refusing to seal a new secret with it. See "Recovering from a key mismatch" in the README.`
)

// cliOnlyWarning checks that stderr is exactly the one warning line of the key guard (and an
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

func TestCLIKeys(t *testing.T) {
	k1, k2, k3 := newKey(t), newKey(t), newKey(t)
	s := harness.Start(t, harness.Options{Env: map[string]string{"SECRETS_KEY": k1}})
	ownerSub := harness.Unique("cli-keys-owner")
	owner := cliSignIn(t, s, ownerSub, "Owner", true)
	ownerID := cliMustUser(t, s, ownerSub).ID
	keyed := func(key string) map[string]string { return map[string]string{"SECRETS_KEY": key} }

	t.Run("reindex of an empty index prints 0", func(t *testing.T) {
		r := s.CLI(t, "", "reindex")
		cliGolden(t, s, "reindex-empty", r, 0)
	})

	t.Run("reset-key-check needs --yes; with it the check goes, and the next start records the key again", func(t *testing.T) {
		before := keyCheckBytes(t, s)
		no := s.CLI(t, "", "reset-key-check")
		cliExpectUsage(t, "reset-key-check without --yes", no)
		cliGolden(t, s, "reset-key-check-no-yes", no, 2)
		if !bytes.Equal(before, keyCheckBytes(t, s)) {
			t.Fatal("a refused reset-key-check changed the check")
		}
		if n := countSQL(t, s, `select count(*) from platform.audit_log where action = 'secrets.key_check_reset'`); n != 0 {
			t.Fatalf("a refused reset-key-check was audited (%d)", n)
		}

		r := s.CLI(t, "", "reset-key-check", "--yes")
		cliGolden(t, s, "reset-key-check", r, 0)
		if n := rowCount(t, s, "select count(*) from platform.key_check"); n != 0 {
			t.Fatalf("key_check rows after the reset: %d", n)
		}
		resets := auditRows(t, s, "where action = 'secrets.key_check_reset' order by id")
		if len(resets) != 1 || resets[0].ActorID != nil || resets[0].Target != nil {
			t.Fatalf("audit of the reset: %+v", resets)
		}
		cliMetaIs(t, resets[0], `{"removed":true,"via":"cli"}`)

		// The CLI never creates the check, with the right key or any other.
		cliOK(t, s, "", "create-user", "--sub", harness.Unique("cli-nocheck"), "--name", "x")
		silent := s.CLIWithEnv(t, keyed(k3), "", "create-user", "--sub", harness.Unique("cli-nocheck"), "--name", "x")
		if silent.Code != 0 || silent.Stderr != "" {
			t.Fatalf("a command with another key while no check is stored: %+v", silent)
		}
		if n := rowCount(t, s, "select count(*) from platform.key_check"); n != 0 {
			t.Fatalf("a CLI command created the check (%d rows)", n)
		}

		nothing := s.CLI(t, "", "reset-key-check", "--yes")
		cliGolden(t, s, "reset-key-check-nothing", nothing, 0)
		resets = auditRows(t, s, "where action = 'secrets.key_check_reset' order by id")
		if len(resets) != 2 {
			t.Fatalf("audit rows after the second reset: %d", len(resets))
		}
		cliMetaIs(t, resets[1], `{"removed":false,"via":"cli"}`)

		cliSwap(t, s, k1) // the next start records the configured key
		if n := rowCount(t, s, "select count(*) from platform.key_check"); n != 1 {
			t.Fatalf("key_check rows after the restart: %d", n)
		}
	})

	t.Run("on a mismatching key the CLI warns and proceeds, except create-server; it runs with the app down", func(t *testing.T) {
		// The app refuses to start with another key: its container is gone, and the CLI still runs in
		// a one-off container of the same image.
		if code, logs := s.SwapImageExpectingExit(t, platformImage(), "", keyed(k3)); code != 1 {
			t.Fatalf("the app with another key: exit %d: %s", code, logs)
		}
		// Control: with the right key the CLI is silent.
		subOK := harness.Unique("cli-guard-ok")
		ok := s.CLIWithEnv(t, keyed(k1), "", "create-user", "--sub", subOK, "--name", "Right Key")
		if ok.Code != 0 || ok.Stderr != "" {
			t.Fatalf("create-user with the right key: %+v", ok)
		}

		sub := harness.Unique("cli-guard")
		warn := s.CLIWithEnv(t, keyed(k3), "", "create-user", "--sub", sub, "--name", "Wrong Key")
		cliGolden(t, s, "key-guard-warning", warn, 0, sub, "<SUB>")
		cliExpectWarning(t, "create-user", warn)
		if _, found := cliUser(t, s, sub); !found {
			t.Fatal("create-user did not proceed after the warning")
		}
		for _, args := range [][]string{
			{"login-link", sub},
			{"create-token", sub, "--name", "guard"},
			{"reindex"},
			{"sync", harness.Unique("cli-noserver")},
		} {
			r := s.CLIWithEnv(t, keyed(k3), "", args...)
			cliExpectWarning(t, args[0], r)
			if args[0] != "sync" && r.Code != 0 {
				t.Errorf("%s with another key: exit %d, want it to proceed", args[0], r.Code)
			}
			if args[0] == "sync" && (r.Code != 1 || !strings.Contains(r.Stderr, "not_found")) {
				t.Errorf("sync with another key: %+v, want the command's own error after the warning", r)
			}
		}

		// create-server is the one command that seals a new secret: refused, nothing created.
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		slug := harness.Unique("cli-guard-srv")
		file := cliServerFile(t, s, up, slug, "shared_secret")
		secret := harness.Unique("sekret-guard")
		before := cliServerCount(t, s)
		refused := s.CLIWithEnv(t, keyed(k3), secret+"\n", "create-server", "--json", file, "--as", ownerSub)
		cliGolden(t, s, "key-guard-refused", refused, 1)
		if refused.Stderr != cliKeyRefusal+"\n" || refused.Stdout != "" {
			t.Fatalf("create-server with another key: %+v", refused)
		}
		if n := cliServerCount(t, s); n != before || len(up.Requests()) != 0 {
			t.Fatalf("a refused create-server created %d server(s) and contacted the upstream %d time(s)", n-before, len(up.Requests()))
		}
		// Control: the same command with the right key works.
		created := s.CLIWithEnv(t, keyed(k1), secret+"\n", "create-server", "--json", file, "--as", ownerSub)
		if created.Code != 0 || created.Stderr != "" {
			t.Fatalf("create-server with the right key: %+v", created)
		}

		// rotate-secrets-key and reset-key-check handle the key themselves: no warning.
		rot := s.CLIWithEnv(t, keyed(k3), "", "rotate-secrets-key")
		cliGolden(t, s, "rotate-secrets-key-no-old", rot, 1)
		reset := s.CLIWithEnv(t, keyed(k3), "", "reset-key-check")
		if reset.Code != 2 || strings.Contains(reset.Stderr, "warning:") {
			t.Fatalf("reset-key-check with another key: %+v", reset)
		}
		// A previous key that opens the check is not a mismatch: the rotation is pending, nothing to warn about.
		pending := s.CLIWithEnv(t, map[string]string{"SECRETS_KEY": k3, "SECRETS_KEY_OLD": k1}, "", "create-user", "--sub", harness.Unique("cli-pending"), "--name", "x")
		if pending.Code != 0 || pending.Stderr != "" {
			t.Fatalf("create-user with the previous key configured: %+v", pending)
		}
		cliSwap(t, s, k1)
		cliExpectNoSecret(t, s, secret, refused, created) // the app is back, so its log can be read
	})

	t.Run("a database from a newer version: the message and exit 1, nothing applied; --version still works", func(t *testing.T) {
		sub := harness.Unique("cli-newer")
		migrations := rowCount(t, s, "select count(*) from platform.schema_migrations")
		execSQL(t, s, `insert into platform.schema_migrations (name) values ($1)`, newerDBMigrName)
		r := s.CLIWithEnv(t, nil, "", "create-user", "--sub", sub, "--name", "Newer")
		cliGolden(t, s, "newer-database", r, 1)
		if _, found := cliUser(t, s, sub); found {
			t.Fatal("a command ran against a database from a newer version")
		}
		if v := s.CLIWithEnv(t, nil, "", "--version"); v.Code != 0 {
			t.Errorf("--version with a newer database: %+v", v)
		}
		execSQL(t, s, `delete from platform.schema_migrations where name = $1`, newerDBMigrName)
		if n := rowCount(t, s, "select count(*) from platform.schema_migrations"); n != migrations {
			t.Fatalf("migration rows %d, want %d", n, migrations)
		}
		// Control: without the unknown migration the same command works.
		if r := s.CLIWithEnv(t, nil, "", "create-user", "--sub", sub, "--name", "Newer"); r.Code != 0 {
			t.Fatalf("create-user after restoring: %+v", r)
		}
	})

	t.Run("rotate-secrets-key re-seals what the app then opens with the new key", func(t *testing.T) {
		secret := harness.Unique("sekret-rotate")
		pat := harness.Unique("pat-rotate")
		sharedUp := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo"), RequireHeader: [2]string{"Authorization", "Bearer " + secret}})
		patUp := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo"), RequireHeader: [2]string{"X-Token", "pat " + pat}})
		shared, personal := harness.Unique("cli-rot-shared"), harness.Unique("cli-rot-pat")
		cliOK(t, s, secret+"\n", "create-server", "--json", cliServerFile(t, s, sharedUp, shared, "shared_secret"), "--as", ownerSub)
		cliOK(t, s, "", "create-server", "--json", cliServerFile(t, s, patUp, personal, "pat_user", fakes.WithHeader("X-Token", "pat {secret}")), "--as", ownerSub)
		if r := owner.Do(t, "PUT", "/api/connections/"+personal+"/pat", map[string]string{"token": pat}); r.Status != 204 {
			t.Fatalf("PUT pat: %d %s", r.Status, r.Body)
		}
		cliOK(t, s, "", "sync", personal, "--as", ownerSub)
		token := owner.Token(t, "cli-rotate")

		callBoth := func(what string) {
			t.Helper()
			sharedUp.Reset()
			patUp.Reset()
			sess := s.MCP(t, token)
			for _, slug := range []string{shared, personal} {
				res := catalogCallMCP(t, sess, "call_tool", map[string]any{"tool": slug + ".echo", "arguments": map[string]any{"text": "hi"}})
				if res.IsError || catalogText(res) != `{"text":"hi"}` {
					t.Fatalf("%s: call on %s: isError %v %q", what, slug, res.IsError, catalogText(res))
				}
			}
			if c := sharedUp.Calls(); len(c) != 1 || c[0].Header.Get("Authorization") != "Bearer "+secret {
				t.Fatalf("%s: the shared-secret upstream got %d call(s) without the secret", what, len(c))
			}
			if c := patUp.Calls(); len(c) != 1 || c[0].Header.Get("X-Token") != "pat "+pat {
				t.Fatalf("%s: the personal-token upstream got %d call(s) without the token", what, len(c))
			}
		}
		callBoth("before the rotation (control)")

		secretBefore := cliSealed(t, s, shared)
		connBefore := connRowOf(t, s, ownerID, personal).Access
		rotation := map[string]string{"SECRETS_KEY": k2, "SECRETS_KEY_OLD": k1}
		r := s.CLIWithEnv(t, rotation, "", "rotate-secrets-key")
		cliGolden(t, s, "rotate-secrets-key", r, 0)
		if bytes.Equal(secretBefore, cliSealed(t, s, shared)) || bytes.Equal(connBefore, connRowOf(t, s, ownerID, personal).Access) {
			t.Fatal("the sealed values did not change")
		}
		for _, v := range []string{secret, pat} {
			if strings.Contains(r.Stdout, v) || strings.Contains(r.Stderr, v) {
				t.Errorf("the rotation printed %s", harness.Redact(v))
			}
		}
		cliExpectNoSecret(t, s, secret, r)

		// With the new key alone the app starts (the check was re-sealed) and opens both.
		cliSwap(t, s, k2)
		callBoth("after the rotation")

		again := s.CLIWithEnv(t, rotation, "", "rotate-secrets-key")
		cliGolden(t, s, "rotate-secrets-key-again", again, 0)
		runs := auditRows(t, s, "where action = 'secrets.rotate' order by id")
		if len(runs) != 2 || runs[0].ActorID != nil || runs[0].Target != nil {
			t.Fatalf("secrets.rotate audit: %+v", runs)
		}
		// Two servers hold a secret on this stack: the one of the key guard scenario and this one.
		cliMetaIs(t, runs[0], `{"connections":{"current":0,"failed":0,"resealed":1},"keyCheck":"resealed","servers":{"current":0,"failed":0,"resealed":2},"via":"cli"}`)
		cliMetaIs(t, runs[1], `{"connections":{"current":1,"failed":0,"resealed":0},"keyCheck":"current","servers":{"current":2,"failed":0,"resealed":0},"via":"cli"}`)

		// A value that neither key opens is reported by name and left as it was; the exit code is 1.
		garbage := bytes.Repeat([]byte{0xab}, 48)
		goodSecret, goodConn := cliSealed(t, s, shared), connRowOf(t, s, ownerID, personal).Access
		execSQL(t, s, `update catalog.servers set secret = $2 where slug = $1`, shared, garbage)
		execSQL(t, s, `update connections.connections set access = $3 where user_id = $1::uuid and server = $2`, ownerID, personal, garbage)
		bad := s.CLIWithEnv(t, rotation, "", "rotate-secrets-key")
		cliGolden(t, s, "rotate-secrets-key-unreadable", bad, 1, shared, "<SERVER-1>", personal, "<SERVER-2>")
		if !bytes.Equal(cliSealed(t, s, shared), garbage) || !bytes.Equal(connRowOf(t, s, ownerID, personal).Access, garbage) {
			t.Fatal("a value that neither key opens was changed")
		}
		failedRun := auditRows(t, s, "where action = 'secrets.rotate' order by id desc limit 1")[0]
		cliMetaIs(t, failedRun, `{"connections":{"current":0,"failed":1,"resealed":0},"keyCheck":"current","servers":{"current":1,"failed":1,"resealed":0},"via":"cli"}`)
		execSQL(t, s, `update catalog.servers set secret = $2 where slug = $1`, shared, goodSecret)
		execSQL(t, s, `update connections.connections set access = $3 where user_id = $1::uuid and server = $2`, ownerID, personal, goodConn)

		// The same for the key check.
		goodCheck := keyCheckBytes(t, s)
		execSQL(t, s, `update platform.key_check set sealed = $1`, garbage)
		badCheck := s.CLIWithEnv(t, rotation, "", "rotate-secrets-key")
		cliGolden(t, s, "rotate-secrets-key-unreadable-key-check", badCheck, 1)
		if !bytes.Equal(keyCheckBytes(t, s), garbage) {
			t.Fatal("an unreadable key check was changed")
		}
		execSQL(t, s, `update platform.key_check set sealed = $1`, goodCheck)
		if r := s.CLIWithEnv(t, rotation, "", "rotate-secrets-key"); r.Code != 0 {
			t.Fatalf("rotate-secrets-key after restoring the values: %+v", r)
		}
	})
}
