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
//   - "sync re-reads the tools, as an admin or as the scheduler"  TestCLIServers (sync)
//   - "reindex rebuilds the search index and reports the count"   TestCLIServers (reindex), TestCLIReindexEmpty
//   - rotate-secrets-key: "re-seals server secrets and connections", "re-seals the key check",
//     "already current on a repeat", "fails naming the key check", "refuses to run without
//     SECRETS_KEY_OLD", "fails and names what neither key opens"  TestCLIKeys (rotate)
//   - reset-key-check: three cases                                TestCLIKeys (reset)
//   - cli on a key mismatch: four cases                           TestCLIKeys (guard)
// Not ported: the cases that count how often stdin was read ("stdinRead"): a black box cannot see
// it; the effects that follow from it (the environment variable wins over stdin, stdin is ignored
// for a mode without a secret) are asserted instead.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

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

// TestCLICreateUser: the subtests are sequential steps of one scenario (one person is created,
// re-run, renamed, promoted, demoted); run the test as a whole. Its goldens hold no counts.
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

// TestCLICreateToken: sequential steps of one scenario (one person, their first token, a second);
// run the test as a whole.
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

// TestCLILoginLink: sequential steps of one scenario (one person, their links); run the test as a whole.
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
		// Exactly three links were issued for the person: the first, the second, and this one.
		if len(entries) != 3 || entries[len(entries)-1].ActorID != nil {
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
