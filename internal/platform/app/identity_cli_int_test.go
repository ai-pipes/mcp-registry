//go:build integration

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5"
)

var (
	goldUsage = regexp.MustCompile(`(?s)\nUsage: .*$`)
	goldToken = regexp.MustCompile(`\bsk_[A-Za-z0-9_-]+`)
	goldCode  = regexp.MustCompile(`\bll_[A-Za-z0-9_-]+`)
	goldUUID  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// render is the form of compat/golden/cli/*.txt (compat's cliRender and cliNormalize, which sit
// behind the compat tag): subs are (value, placeholder) pairs.
func render(r cliRun, subs ...string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "exit: %d\n--- stdout ---\n", r.code)
	section := func(text string) {
		text = goldUsage.ReplaceAllString(text, "\n<USAGE>\n")
		text = strings.ReplaceAll(text, "http://localhost:3000", "http://localhost:<PORT>")
		for i := 0; i+1 < len(subs); i += 2 {
			text = strings.ReplaceAll(text, subs[i], subs[i+1])
		}
		text = goldToken.ReplaceAllString(text, "sk_<TOKEN>")
		text = goldCode.ReplaceAllString(text, "ll_<CODE>")
		text = goldUUID.ReplaceAllString(text, "<UUID>")
		b.WriteString(text)
		if text != "" && !strings.HasSuffix(text, "\n") {
			b.WriteString("\n<no newline at the end>\n")
		}
	}
	section(r.stdout)
	b.WriteString("--- stderr ---\n")
	section(r.stderr)
	return b.String()
}

func golden(t *testing.T, name string, r cliRun, subs ...string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "compat", "golden", "cli", name+".txt")) //nolint:gosec // G304: a fixed directory, names from the tests
	if err != nil {
		t.Fatal(err)
	}
	if got := render(r, subs...); got != string(raw) {
		t.Errorf("%s differs from the golden:\n--- got\n%s--- want\n%s", name, got, raw)
	}
}

type cliEnv struct {
	t   *testing.T
	url string
	key []byte
	cfg config.Config
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	url := pgtest.StartURL(t)
	key := newKey(t)
	cfg := baseConfig(url, key)
	cfg.OIDCLoginGroup, cfg.OIDCAdminGroup = "mcp-users", "mcp-admins"
	return &cliEnv{t: t, url: url, key: key, cfg: cfg}
}

// run is RunCLI over the real module list, the entry point the binary uses.
func (e *cliEnv) run(args ...string) cliRun {
	e.t.Helper()
	var out, errOut bytes.Buffer
	code := RunCLI(context.Background(), CLIOptions{
		Config: e.cfg, Version: "9.9.9-test", Args: args, Stdin: strings.NewReader(""),
		Stdout: &out, Stderr: &errOut, Env: map[string]string{},
	})
	return cliRun{code, out.String(), errOut.String()}
}

func (e *cliEnv) ok(args ...string) cliRun {
	e.t.Helper()
	r := e.run(args...)
	if r.code != 0 {
		e.t.Fatalf("%v: %+v", args, r)
	}
	return r
}

func (e *cliEnv) text(sql string, args ...any) string {
	e.t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, e.url)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	var s string
	if err := c.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *cliEnv) count(sql string, args ...any) int {
	e.t.Helper()
	return queryInt(e.t, e.url, sql, args...)
}

func (e *cliEnv) role(sub string) string {
	e.t.Helper()
	return e.text(`select role from identity.users where oidc_sub = $1`, sub)
}

func TestCLICreateUser(t *testing.T) {
	e := newCLIEnv(t)
	subs := func(sub string) []string { return []string{sub, "<SUB>"} }

	r := e.run("create-user", "--sub", "ann", "--name", "Ann Example")
	golden(t, "create-user-member", r, subs("ann")...)
	id := e.text(`select id::text from identity.users where oidc_sub = 'ann'`)
	if want := fmt.Sprintf("created member \"Ann Example\" (sub ann, id %s)\n", id); r.stdout != want || r.stderr != "" {
		t.Fatalf("%+v, want %q", r, want)
	}
	// user.provision: actor empty, meta says how.
	if n := e.count(`select count(*) from platform.audit_log where action = 'user.provision' and actor_id is null and target = $1 and meta = '{"via":"cli","role":"member","created":true}'::jsonb`, id); n != 1 {
		t.Errorf("user.provision rows: %d", n)
	}

	golden(t, "create-user-again", e.run("create-user", "--sub", "ann", "--name", "Ann Example"), subs("ann")...)
	golden(t, "create-user-rename", e.run("create-user", "--sub", "ann", "--name", "Ann Renamed"), subs("ann")...)
	if e.count(`select count(*) from identity.users where oidc_sub = 'ann'`) != 1 || e.text(`select name from identity.users where oidc_sub = 'ann'`) != "Ann Renamed" {
		t.Error("the person was duplicated or not renamed")
	}
	golden(t, "create-user-promote", e.run("create-user", "--sub", "ann", "--name", "Ann Renamed", "--admin"), subs("ann")...)
	if e.role("ann") != "admin" {
		t.Fatal("--admin did not promote")
	}

	// Decided: without a flag the role of an existing person stays; --member demotes.
	r = e.run("create-user", "--sub", "ann", "--name", "Ann Renamed")
	if r.code != 0 || r.stdout != fmt.Sprintf("updated admin \"Ann Renamed\" (sub ann, id %s)\n", id) || e.role("ann") != "admin" {
		t.Errorf("flagless run on an admin: %+v, role %s", r, e.role("ann"))
	}
	r = e.run("create-user", "--sub", "ann", "--name", "Ann Renamed", "--member")
	if r.code != 0 || r.stdout != fmt.Sprintf("updated member \"Ann Renamed\" (sub ann, id %s)\n", id) || e.role("ann") != "member" {
		t.Errorf("--member: %+v, role %s", r, e.role("ann"))
	}
	if r := e.run("create-user", "--sub", "ann", "--name", "Ann Renamed"); r.code != 0 || e.role("ann") != "member" || !strings.HasPrefix(r.stdout, "updated member ") {
		t.Errorf("flagless run on a member: %+v", r)
	}
	if n := e.count(`select count(*) from platform.audit_log where action = 'user.provision' and target = $1 and meta @> '{"created":false,"role":"member"}'`, id); n != 4 {
		t.Errorf("audit rows with role member and created false: %d", n)
	}

	// A new person with --member is a member; with --admin an admin.
	if r := e.run("create-user", "--sub", "newbie", "--name", "N", "--member"); r.stdout != fmt.Sprintf("created member \"N\" (sub newbie, id %s)\n", e.text(`select id::text from identity.users where oidc_sub = 'newbie'`)) {
		t.Errorf("--member on a new person: %+v", r)
	}
	golden(t, "create-user-admin", e.run("create-user", "--sub", "root", "--name", "Root Example", "--admin"), "root", "<ADMIN-SUB>")

	// The name is printed raw between double quotes (TypeScript's template string), not escaped.
	r = e.ok("create-user", "--sub", "quoted", "--name", `  Ann "Q" \ B `)
	if !strings.HasPrefix(r.stdout, `created member "Ann "Q" \ B" (sub quoted, id `) {
		t.Errorf("quoted name: %q", r.stdout)
	}

	// sub and name are trimmed (JavaScript trim).
	r = e.ok("create-user", "--sub", "  pad ", "--name", "  Padded Name ")
	if !strings.HasPrefix(r.stdout, `created member "Padded Name" (sub pad, id `) || e.count(`select count(*) from identity.users where oidc_sub = 'pad'`) != 1 {
		t.Errorf("trim: %+v", r)
	}

	// Refusals: the order of the argument errors is the reference's.
	before := e.count(`select count(*) from identity.users`)
	for _, c := range []struct {
		golden string
		args   []string
	}{
		{"create-user-missing-sub", []string{"create-user", "--name", "x"}},
		{"create-user-missing-name", []string{"create-user", "--sub", "gone"}},
		{"create-user-empty-name", []string{"create-user", "--sub", "gone", "--name", ""}},
		{"create-user-stray-argument", []string{"create-user", "stray", "--sub", "gone", "--name", "x"}},
	} {
		golden(t, c.golden, e.run(c.args...), "gone", "<SUB>")
	}
	// The positional count comes before the required options; an empty --sub is a missing one.
	if r := e.run("create-user", "stray"); r.code != 2 || !strings.HasPrefix(r.stderr, "error: expected 0 argument(s), got 1\n") {
		t.Errorf("stray before required: %+v", r)
	}
	if r := e.run("create-user", "--sub", "", "--name", "x"); r.code != 2 || !strings.HasPrefix(r.stderr, "error: --sub is required\n") {
		t.Errorf("empty sub: %+v", r)
	}
	for _, args := range [][]string{
		{"create-user", "--sub", "gone", "--name", "x", "--bogus"},
		{"create-user", "--sub", "gone", "--name", "x", "--admin", "--member"},
		{"create-user", "--sub"},
	} {
		r := e.run(args...)
		if r.code != 2 || r.stdout != "" || !strings.HasPrefix(r.stderr, "error: ") || !strings.Contains(r.stderr, "\nUsage: ") {
			t.Errorf("%v: %+v", args, r)
		}
	}
	if r := e.run("create-user", "--sub", "gone", "--name", "x", "--admin", "--member"); !strings.HasPrefix(r.stderr, "error: --admin and --member cannot be used together\n") {
		t.Errorf("both flags: %q", r.stderr)
	}
	if r := e.run("create-user", "--sub", "  ", "--name", "x"); r.code != 1 || r.stdout != "" || r.stderr != "error: invalid_input: sub and name must not be blank\n" {
		t.Errorf("blank sub: %+v", r)
	}
	if n := e.count(`select count(*) from identity.users`); n != before {
		t.Errorf("a refused invocation changed the number of people: %d -> %d", before, n)
	}
	// Control: the same arguments, complete.
	e.ok("create-user", "--sub", "gone", "--name", "x")

	// A disabled person is refused and left as they were.
	e.ok("create-user", "--sub", "dis", "--name", "Gone Example")
	exec(t, e.url, `update identity.users set disabled = true where oidc_sub = 'dis'`)
	golden(t, "create-user-disabled", e.run("create-user", "--sub", "dis", "--name", "Changed Name"))
	if e.text(`select name from identity.users where oidc_sub = 'dis'`) != "Gone Example" {
		t.Error("a refused run changed the person")
	}
	eventuallyNoConnections(t, e.url)
}

func TestCLILoginLink(t *testing.T) {
	e := newCLIEnv(t)
	e.ok("create-user", "--sub", "link-person", "--name", "Link Person")
	uid := e.text(`select id::text from identity.users where oidc_sub = 'link-person'`)

	r := e.run("login-link", "link-person")
	golden(t, "login-link", r, "link-person", "<SUB>")
	m := regexp.MustCompile(`^http://localhost:3000/login#link=(ll_[A-Za-z0-9_-]{43})\n$`).FindStringSubmatch(r.stdout)
	if m == nil || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	sum := sha256.Sum256([]byte(m[1]))
	if e.count(`select count(*) from identity.login_links where user_id = $1::uuid and code_hash = $2`, uid, hex.EncodeToString(sum[:])) != 1 ||
		e.count(`select count(*) from identity.login_links l where position($1 in l::text) > 0`, m[1]) != 0 {
		t.Error("only the hash of the code must be stored")
	}
	// A second run replaces the first link.
	r2 := e.ok("login-link", "link-person")
	if r2.stdout == r.stdout || e.count(`select count(*) from identity.login_links where user_id = $1::uuid`, uid) != 1 {
		t.Error("a second link did not replace the first")
	}
	// Audit: no actor, the person as target, the code nowhere.
	if n := e.count(`select count(*) from platform.audit_log where action = 'auth.link_issued' and actor_id is null and target = $1`, uid); n != 2 {
		t.Errorf("auth.link_issued rows: %d", n)
	}
	if n := e.count(`select count(*) from platform.audit_log where position($1 in meta::text || coalesce(target, '')) > 0`, m[1]); n != 0 {
		t.Error("the code is in the audit")
	}

	if r := e.run("login-link", "ghost"); r.stdout != "" {
		t.Errorf("stdout of a refused login-link: %q", r.stdout)
	} else {
		golden(t, "login-link-unknown", r, "ghost", "<SUB>")
	}
	e.ok("create-user", "--sub", "linkdis", "--name", "Gone")
	exec(t, e.url, `update identity.users set disabled = true where oidc_sub = 'linkdis'`)
	golden(t, "login-link-disabled", e.run("login-link", "linkdis"))
	golden(t, "login-link-missing-sub", e.run("login-link"))
	golden(t, "login-link-two-arguments", e.run("login-link", "link-person", "extra"), "link-person", "<SUB>")

	// With OIDC configured the command works too.
	e.cfg.OIDC = &config.OIDCClient{Issuer: "http://127.0.0.1:1", ClientID: "id", ClientSecret: config.NewSecret("s")}
	e.cfg.OIDCScopes = "openid"
	golden(t, "login-link-oidc", e.run("login-link", "link-person"), "link-person", "<SUB>")
	eventuallyNoConnections(t, e.url)
}

func TestCLICreateToken(t *testing.T) {
	e := newCLIEnv(t)
	e.cfg.PublicURL = "http://localhost:3000/some/prefix/" // the path of PUBLIC_URL is replaced, like new URL('/mcp', ...)
	e.ok("create-user", "--sub", "token-owner", "--name", "Token Owner")
	uid := e.text(`select id::text from identity.users where oidc_sub = 'token-owner'`)

	r := e.run("create-token", "token-owner", "--name", "laptop")
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "sk_") || r.stderr != "The token is shown only once; store it now.\n" {
		t.Fatalf("%+v", r)
	}
	token := lines[0]
	if want := fmt.Sprintf(`claude mcp add --transport http mcp-registry http://localhost:3000/mcp --header "Authorization: Bearer %s"`, token); lines[1] != want {
		t.Errorf("connect command %q", lines[1])
	}
	sum := sha256.Sum256([]byte(token))
	if e.count(`select count(*) from identity.tokens where user_id = $1::uuid and hash = $2 and name = 'laptop' and prefix = $3`, uid, hex.EncodeToString(sum[:]), token[:10]) != 1 ||
		e.count(`select count(*) from identity.tokens t where position($1 in t::text) > 0`, token) != 0 {
		t.Error("stored token row")
	}
	// The actor of token.create is the OWNER.
	if n := e.count(`select count(*) from platform.audit_log where action = 'token.create' and actor_id = $1::uuid and meta ->> 'name' = 'laptop' and meta ->> 'prefix' = $2`, uid, token[:10]); n != 1 {
		t.Errorf("token.create rows with the owner as actor: %d", n)
	}
	if n := e.count(`select count(*) from platform.audit_log where position($1 in meta::text) > 0`, token); n != 0 {
		t.Error("the token is in the audit")
	}

	e.cfg.PublicURL = "http://localhost:3000"
	golden(t, "create-token", e.run("create-token", "token-owner", "--name", "laptop"), "token-owner", "<SUB>")
	if e.count(`select count(*) from identity.tokens where user_id = $1::uuid`, uid) != 2 {
		t.Error("a second token is another token")
	}

	before := e.count(`select count(*) from identity.tokens`)
	golden(t, "create-token-unknown", e.run("create-token", "ghost", "--name", "x"), "ghost", "<SUB>")
	e.ok("create-user", "--sub", "tokdis", "--name", "Gone")
	exec(t, e.url, `update identity.users set disabled = true where oidc_sub = 'tokdis'`)
	golden(t, "create-token-disabled", e.run("create-token", "tokdis", "--name", "x"))
	golden(t, "create-token-missing-name", e.run("create-token", "token-owner"))
	golden(t, "create-token-missing-sub", e.run("create-token", "--name", "x"))
	// The name is checked before the person is looked up: an unknown person without a name is a bad invocation.
	if r := e.run("create-token", "ghost"); r.code != 2 || !strings.HasPrefix(r.stderr, "error: --name is required\n") {
		t.Errorf("unknown sub without a name: %+v", r)
	}
	if n := e.count(`select count(*) from identity.tokens`); n != before {
		t.Errorf("refused runs created tokens: %d -> %d", before, n)
	}
	eventuallyNoConnections(t, e.url)
}

// GuardWarn: on a key mismatch the three commands warn and still run (golden key-guard-warning).
func TestCLIIdentityCommandsWarnOnAKeyMismatch(t *testing.T) {
	e := newCLIEnv(t)
	startInstance(t, e.cfg, nil).stop("SIGTERM") // records the check with the right key
	wrong := newCLIEnv(t)
	wrong.url, wrong.cfg = e.url, e.cfg
	wrong.cfg.SecretsKey = config.NewSecretBytes(newKey(t))

	r := wrong.run("create-user", "--sub", "wk", "--name", "Wrong Key")
	golden(t, "key-guard-warning", r, "wk", "<SUB>")
	for _, args := range [][]string{{"login-link", "wk"}, {"create-token", "wk", "--name", "x"}} {
		r := wrong.run(args...)
		if r.code != 0 || !strings.HasPrefix(r.stderr, warningLine) {
			t.Errorf("%v: %+v", args, r)
		}
	}
	// The warning comes before the argument errors.
	if r := wrong.run("create-user"); r.code != 2 || !strings.HasPrefix(r.stderr, warningLine+"error: --sub is required\n") {
		t.Errorf("%+v", r)
	}
}

// provisionUser stamps last_login_at on every run, an existing person too: that is what stops the
// first-start link from being reissued for local-admin (a side effect of the reference).
func TestCLICreateUserOnAnExistingPersonSetsLastLogin(t *testing.T) {
	e := newCLIEnv(t)
	e.ok("create-user", "--sub", "stamped", "--name", "S")
	exec(t, e.url, `update identity.users set last_login_at = null where oidc_sub = 'stamped'`)
	e.ok("create-user", "--sub", "stamped", "--name", "S")
	if n := e.count(`select count(*) from identity.users where oidc_sub = 'stamped' and last_login_at is not null`); n != 1 {
		t.Error("last_login_at was not set")
	}
}
