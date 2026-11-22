//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/config"
)

// A test value, not a credential.
const cliSecret = "sekret-cli-0123456789" //nolint:gosec // G101: a made-up value the tests look for in output

// serverEnv is the CLI environment with a catalog: an admin "owner", an upstream and the files.
type serverEnv struct {
	*cliEnv
	up  *mcpFake
	dir string
}

func newServerEnv(t *testing.T) *serverEnv {
	t.Helper()
	e := newCLIEnv(t)
	catalogConfig(&e.cfg)
	s := &serverEnv{cliEnv: e, up: newMCPFake(t, "echo"), dir: t.TempDir()}
	e.ok("create-user", "--sub", "owner", "--name", "Owner", "--admin")
	return s
}

// runIn is RunCLI over the real modules with stdin and environment.
func (e *cliEnv) runIn(cfg config.Config, stdin string, env map[string]string, args ...string) cliRun {
	e.t.Helper()
	return e.runCtx(context.Background(), cfg, strings.NewReader(stdin), env, args...)
}

func (e *cliEnv) runCtx(ctx context.Context, cfg config.Config, stdin io.Reader, env map[string]string, args ...string) cliRun {
	e.t.Helper()
	var out, errOut bytes.Buffer
	code := RunCLI(ctx, CLIOptions{
		Config: cfg, Version: "9.9.9-test", Args: args, Stdin: stdin,
		Stdout: &out, Stderr: &errOut, Env: env,
	})
	return cliRun{code, out.String(), errOut.String()}
}

func (s *serverEnv) file(name string, over map[string]any) string {
	s.t.Helper()
	body := map[string]any{
		"slug": "fake", "name": "Fake", "description": "A fake upstream.", "url": s.up.url(),
		"auth":   map[string]any{"kind": "shared_secret", "header": "Authorization", "template": "Bearer {secret}"},
		"access": map[string]any{"kind": "all"},
	}
	for k, v := range over {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		s.t.Fatal(err)
	}
	return s.raw(name, string(raw))
}

func (s *serverEnv) raw(name, text string) string {
	s.t.Helper()
	p := filepath.Join(s.dir, name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		s.t.Fatal(err)
	}
	return p
}

func (s *serverEnv) create(stdin string, env map[string]string, args ...string) cliRun {
	return s.runIn(s.cfg, stdin, env, append([]string{"create-server"}, args...)...)
}

func (s *serverEnv) servers() int { return s.count(`select count(*) from catalog.servers`) }

func (s *serverEnv) noSecretAnywhere(r cliRun) {
	s.t.Helper()
	if strings.Contains(r.stdout+r.stderr, cliSecret) {
		s.t.Errorf("the secret was printed: %+v", r)
	}
	if n := s.count(`select count(*) from platform.audit_log where meta::text like '%' || $1 || '%' or coalesce(target,'') like '%' || $1 || '%'`, cliSecret); n != 0 {
		s.t.Errorf("the secret reached the audit log")
	}
	if n := s.count(`select count(*) from catalog.servers where position(convert_to($1, 'UTF8') in secret) > 0`, cliSecret); n != 0 {
		s.t.Errorf("the secret is stored in the clear")
	}
}

func TestCLICreateServerWithoutASecret(t *testing.T) {
	s := newServerEnv(t)
	f := s.file("none.json", map[string]any{"auth": map[string]any{"kind": "none"}})
	r := s.create("", nil, "--json", f, "--as", "owner")
	golden(t, "create-server-none", r, "fake", "<SLUG>")
	if s.servers() != 1 || s.count(`select count(*) from catalog.tools where server = 'fake'`) != 1 {
		t.Fatal("the server or its tool is missing")
	}
	// Audit: the actor is the --as person; create, then sync; no secret_set.
	for _, a := range []string{"server.create", "server.sync"} {
		if s.count(`select count(*) from platform.audit_log a join identity.users u on u.id = a.actor_id where a.action = $1 and u.oidc_sub = 'owner'`, a) != 1 {
			t.Errorf("audit %s missing or not by owner", a)
		}
	}
	if s.count(`select count(*) from platform.audit_log where action = 'server.secret_set'`) != 0 {
		t.Error("a secret was audited for a none server")
	}
}

func TestCLICreateServerSecretFromTheEnvironment(t *testing.T) {
	s := newServerEnv(t)
	f := s.file("env.json", nil)
	r := s.create("never read", map[string]string{"K": cliSecret}, "--json", f, "--as", "owner", "--secret-env", "K")
	golden(t, "create-server-env", r, "fake", "<SLUG>")
	s.noSecretAnywhere(r)
	if got := s.up.authHeaders(); len(got) != 1 || got[0] != "Bearer "+cliSecret {
		t.Errorf("the upstream got %v", got)
	}
	if s.count(`select count(*) from platform.audit_log where action = 'server.secret_set'`) != 1 {
		t.Error("server.secret_set missing")
	}
}

func TestCLICreateServerSecretFromStdin(t *testing.T) {
	s := newServerEnv(t)
	f := s.file("stdin.json", nil)
	r := s.create(cliSecret+"\n", nil, "--json", f, "--as", "owner")
	golden(t, "create-server-stdin", r, "fake", "<SLUG>")
	s.noSecretAnywhere(r)
	if got := s.up.authHeaders(); len(got) != 1 || got[0] != "Bearer "+cliSecret {
		t.Errorf("one trailing newline is dropped, the upstream got %q", got)
	}
}

func TestCLICreateServerIgnoresASecretInTheFile(t *testing.T) {
	s := newServerEnv(t)
	f := s.file("secret.json", map[string]any{"secret": "from-the-file"})
	r := s.create("", nil, "--json", f, "--as", "owner")
	goldenSyncExit(t, "create-server-no-secret", r, "fake", "<SLUG>")
	if got := s.up.authHeaders(); len(got) != 0 {
		t.Errorf("a secret from the file was sent: %v", got)
	}
}

func TestCLICreateServerRefusals(t *testing.T) {
	s := newServerEnv(t)
	s.ok("create-user", "--sub", "mia", "--name", "Mia")
	usage := func(args ...string) cliRun { return s.create("", nil, args...) }

	golden(t, "create-server-missing-json", usage("--as", "owner"))
	golden(t, "create-server-missing-as", usage("--json", "x"))
	r := usage("--json", s.file("e.json", nil), "--as", "owner", "--secret-env", "CLI_SECRET_UNSET")
	golden(t, "create-server-secret-env-unset", r)

	missing := filepath.Join(s.dir, "no-such-file.json")
	golden(t, "create-server-missing-file", usage("--json", missing, "--as", "owner"), missing, "/tmp/compat-no-such-file.json")
	nj := s.raw("nj.json", "not json")
	golden(t, "create-server-not-json", usage("--json", nj, "--as", "owner"), nj, "/tmp/compat-not-json.json")
	bad := s.file("bad.json", map[string]any{"url": "ftp://x"})
	golden(t, "create-server-invalid-url", usage("--json", bad, "--as", "owner"), bad, "/tmp/compat-bad-url.json")
	empty := s.raw("empty.json", `{"slug":"fake"}`)
	r = usage("--json", empty, "--as", "owner")
	golden(t, "create-server-incomplete", maskIssues(r), empty, "/tmp/compat-empty-def.json")

	// The file is judged before the person is looked up.
	if r := usage("--json", bad, "--as", "nobody"); !strings.Contains(r.stderr, "not a valid server") {
		t.Errorf("validation comes after the person: %+v", r)
	}
	golden(t, "create-server-unknown-sub", usage("--json", s.file("ok.json", nil), "--as", "nobody"), "nobody", "<SUB>")
	r = s.create(cliSecret, nil, "--json", s.file("ok2.json", nil), "--as", "mia")
	golden(t, "create-server-member", r)
	s.noSecretAnywhere(r)
	golden(t, "create-server-invalid-slug", usage("--json", s.file("slug.json", map[string]any{"slug": "A"}), "--as", "owner"))
	if s.servers() != 0 {
		t.Fatalf("%d servers after refusals", s.servers())
	}
}

// goldenSyncExit compares with a golden of reference 1.0.1 whose first sync failed: stdout is the
// reference's, the exit code is the decided 1 (create-server-sync-exit) where the golden has 0, and
// the one line the decision adds to stderr is taken off.
func goldenSyncExit(t *testing.T, name string, r cliRun, subs ...string) {
	t.Helper()
	if r.code != 1 || r.stderr != "error: the server was created, but its first sync failed\n" {
		t.Errorf("%s: exit %d, stderr %q", name, r.code, r.stderr)
	}
	r.code, r.stderr = 0, ""
	golden(t, name, r, subs...)
}

func (f *mcpFake) setTools(names ...string) { f.mu.Lock(); f.tools = names; f.mu.Unlock() }

// maskIssues turns the wording of each issue into <issue> as the golden does: only the paths and
// their order are pinned.
func maskIssues(r cliRun) cliRun {
	lines := strings.Split(r.stderr, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "  ") {
			path, _, _ := strings.Cut(l, ": ")
			lines[i] = path + ": <issue>"
		}
	}
	r.stderr = strings.Join(lines, "\n")
	return r
}

func TestCLICreateServerDuplicateSlug(t *testing.T) {
	s := newServerEnv(t)
	f := s.file("d.json", nil)
	s.create(cliSecret, nil, "--json", f, "--as", "owner")
	r := s.create(cliSecret, nil, "--json", f, "--as", "owner")
	golden(t, "create-server-duplicate", r, "fake", "<SLUG>")
	s.noSecretAnywhere(r)
	if s.servers() != 1 {
		t.Error("the duplicate was created")
	}
}

// The decided deviation create-server-sync-exit: the server is created and the report is the
// reference's line on stdout, but the exit code is 1.
func TestCLICreateServerFirstSyncFails(t *testing.T) {
	s := newServerEnv(t)
	s.up.setDown(true)
	r := s.create(cliSecret, nil, "--json", s.file("f.json", nil), "--as", "owner")
	goldenSyncExit(t, "create-server-sync-failed", r, "fake", "<SLUG>")
	if r.stderr != "error: the server was created, but its first sync failed\n" {
		t.Errorf("stderr %q", r.stderr)
	}
	if r.stdout != "created server \"fake\" (shared_secret, secret set); sync failed: Could not reach the upstream\n" {
		t.Errorf("stdout %q", r.stdout)
	}
	s.noSecretAnywhere(r)
	if s.servers() != 1 || s.count(`select count(*) from catalog.servers where secret is not null`) != 1 {
		t.Error("the server (with its secret) must exist")
	}
	// A personal-auth server cannot sync with the CLI's nobody-in-particular: same exit.
	pat := s.file("p.json", map[string]any{"slug": "pat1", "auth": map[string]any{"kind": "pat_user", "header": "Authorization", "template": "Bearer {secret}"}})
	s.up.setDown(false)
	r = s.create("", nil, "--json", pat, "--as", "owner")
	goldenSyncExit(t, "create-server-pat-needs-connection", r, "pat1", "<SLUG>")
}

func TestCLICreateServerIsRefusedOnAKeyMismatch(t *testing.T) {
	s := newServerEnv(t)
	startInstance(t, s.cfg, nil).stop("SIGTERM") // records the key check
	wrong := s.cfg
	wrong.SecretsKey = config.NewSecretBytes(newKey(t))
	f := s.file("k.json", nil)
	r := s.runIn(wrong, cliSecret+"\n", nil, "create-server", "--json", f, "--as", "owner")
	golden(t, "key-guard-refused", r)
	if s.servers() != 0 || len(s.up.authHeaders()) != 0 {
		t.Error("something was created or contacted")
	}
	// Refused before anything is read: not even the argument errors are reported.
	if r := s.runIn(wrong, "", nil, "create-server"); r.code != 1 || !strings.HasPrefix(r.stderr, "error: SECRETS_KEY does not match") {
		t.Errorf("%+v", r)
	}
	// sync only warns and goes on.
	r = s.runIn(wrong, "", nil, "sync", "fake")
	if !strings.HasPrefix(r.stderr, warningLine) || !strings.Contains(r.stderr, "not_found") {
		t.Errorf("%+v", r)
	}
}

func TestCLISync(t *testing.T) {
	s := newServerEnv(t)
	s.create(cliSecret, nil, "--json", s.file("s.json", nil), "--as", "owner")
	s.up.setTools("echo", "second")
	golden(t, "sync-added", s.run("sync", "fake", "--as", "owner"), "fake", "<SLUG>")
	golden(t, "sync", s.run("sync", "fake"), "fake", "<SLUG>")
	// Without --as nobody's audit and no sync owner change.
	if n := s.count(`select count(*) from platform.audit_log where action = 'server.sync'`); n != 2 {
		t.Errorf("server.sync records: %d (create, the admin run; the scheduler-style run has none)", n)
	}

	golden(t, "sync-unknown", s.run("sync", "ghost"), "ghost", "<SLUG>")
	golden(t, "sync-missing-slug", s.run("sync"))
	golden(t, "sync-as-unknown", s.run("sync", "fake", "--as", "nobody"), "nobody", "<SUB>")
	s.ok("create-user", "--sub", "mia", "--name", "Mia")
	golden(t, "sync-as-member", s.run("sync", "fake", "--as", "mia"))

	s.up.setDown(true)
	golden(t, "sync-upstream-failed", s.run("sync", "fake", "--as", "owner"))
	s.up.setDown(false)

	pat := s.file("pat.json", map[string]any{"slug": "pat1", "auth": map[string]any{"kind": "pat_user", "header": "Authorization", "template": "Bearer {secret}"}})
	s.create("", nil, "--json", pat, "--as", "owner")
	golden(t, "sync-needs-connection", s.run("sync", "pat1", "--as", "owner"), "pat1", "<SLUG>")
}

// The person is looked up before the secret is read (readSecret comes after principalFor in
// cli-commands.ts): an unknown --as with an unset --secret-env variable is the person's error,
// a known one with the same variable is the usage error.
func TestCLICreateServerLooksUpThePersonBeforeTheSecret(t *testing.T) {
	s := newServerEnv(t)
	f := s.file("o.json", nil)
	r := s.create("", nil, "--json", f, "--as", "nobody", "--secret-env", "CLI_SECRET_UNSET")
	golden(t, "create-server-unknown-sub", r, "nobody", "<SUB>")
	r = s.create("", nil, "--json", f, "--as", "owner", "--secret-env", "CLI_SECRET_UNSET")
	golden(t, "create-server-secret-env-unset", r)
	// And the same for stdin: an unknown person never gets the prompt or the read.
	cio := s.runCtx(context.Background(), s.cfg, tripReader{t}, nil, "create-server", "--json", f, "--as", "nobody")
	if cio.code != 1 || !strings.Contains(cio.stderr, "not_found") {
		t.Errorf("%+v", cio)
	}
}

// cancelOnRead is a stdin that interrupts the command when it is read and then never answers.
type cancelOnRead struct {
	cancel context.CancelFunc
	block  chan struct{}
}

func (c cancelOnRead) Read([]byte) (int, error) { c.cancel(); <-c.block; return 0, io.EOF }

// An interrupt while the secret is read aborts: nothing is created, nothing contacted.
func TestCLICreateServerInterruptedDuringTheSecretReadCreatesNothing(t *testing.T) {
	s := newServerEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	r := s.runCtx(ctx, s.cfg, cancelOnRead{cancel, block}, nil, "create-server", "--json", s.file("i.json", nil), "--as", "owner")
	if r.code != 1 || r.stdout != "" || r.stderr != "error: interrupted\n" {
		t.Errorf("%+v", r)
	}
	if s.servers() != 0 || len(s.up.authHeaders()) != 0 {
		t.Error("something was created or contacted")
	}
}

// holdUpstream makes tools/list wait and returns a function that waits until it has arrived.
func (s *serverEnv) holdUpstream() (arrived func()) {
	s.t.Helper()
	for len(s.up.arrived) > 0 { // signals of earlier syncs
		<-s.up.arrived
	}
	hold := make(chan struct{})
	s.up.setHold(hold)
	s.t.Cleanup(func() { close(hold) })
	return func() {
		select {
		case <-s.up.arrived:
		case <-time.After(30 * time.Second):
			s.t.Error("tools/list never arrived")
		}
	}
}

// runInterrupted runs a command whose context ends once tools/list has reached the held upstream.
// It must come back long before the call timeout.
func (s *serverEnv) runInterrupted(stdin string, args ...string) cliRun {
	s.t.Helper()
	arrived := s.holdUpstream()
	ctx, cancel := context.WithCancel(context.Background())
	s.t.Cleanup(cancel)
	go func() { arrived(); cancel() }()
	start := time.Now()
	r := s.runCtx(ctx, s.cfg, strings.NewReader(stdin), nil, args...)
	if d := time.Since(start); d > 10*time.Second {
		s.t.Errorf("the command took %v to notice the interrupt", d)
	}
	return r
}

// A signal while the first sync waits on the upstream ends the command: the server stays, with
// its creation records, and nothing about the unfinished attempt is recorded.
func TestCLICreateServerInterruptedDuringTheFirstSync(t *testing.T) {
	s := newServerEnv(t)
	r := s.runInterrupted(cliSecret+"\n", "create-server", "--json", s.file("c.json", nil), "--as", "owner")
	if r.code != 1 || r.stdout != "created server \"fake\" (shared_secret, secret set); 0 tool(s)\n" ||
		r.stderr != "error: interrupted; the server was created, its first sync did not finish\n" {
		t.Fatalf("%+v", r)
	}
	for action, want := range map[string]int{"server.create": 1, "server.secret_set": 1, "server.sync": 0} {
		if got := s.count(`select count(*) from platform.audit_log where action = $1`, action); got != want {
			t.Errorf("audit %s: %d, want %d", action, got, want)
		}
	}
	if s.servers() != 1 || s.count(`select count(*) from catalog.servers where last_sync_at is null and last_sync_error is null`) != 1 {
		t.Error("the server must exist with no sync recorded")
	}
	s.noSecretAnywhere(r)
}

// sync interrupted while it waits: the use case records nothing, as for any cancelled attempt.
func TestCLISyncInterruptedWhileWaitingOnTheUpstream(t *testing.T) {
	s := newServerEnv(t)
	s.create(cliSecret, nil, "--json", s.file("s.json", nil), "--as", "owner")
	syncs := s.count(`select count(*) from platform.audit_log where action = 'server.sync'`)
	before := s.count(`select extract(epoch from last_sync_at)::bigint from catalog.servers where slug = 'fake'`)
	s.up.setTools("echo", "second")
	r := s.runInterrupted("", "sync", "fake", "--as", "owner")
	if r.code != 1 || r.stdout != "" || r.stderr != "error: interrupted\n" {
		t.Fatalf("%+v", r)
	}
	if got := s.count(`select count(*) from platform.audit_log where action = 'server.sync'`); got != syncs {
		t.Errorf("a cancelled attempt was audited")
	}
	if got := s.count(`select extract(epoch from last_sync_at)::bigint from catalog.servers where slug = 'fake'`); got != before {
		t.Errorf("a cancelled attempt changed last_sync_at")
	}
	if s.count(`select count(*) from catalog.tools where server = 'fake'`) != 1 {
		t.Error("the tools changed")
	}
}

// The counts of a sync are printed in the golden's wording with each number in its own place.
func TestCLISyncPrintsDistinctCounts(t *testing.T) {
	s := newServerEnv(t)
	s.up.setTools("t1", "t2", "t3", "t4")
	s.create(cliSecret, nil, "--json", s.file("n.json", nil), "--as", "owner")
	s.up.mu.Lock()
	s.up.suffix = " v2"
	s.up.mu.Unlock()
	s.up.setTools("t1", "n1", "n2") // t1 changed, n1 and n2 added, t2..t4 removed
	r := s.run("sync", "fake", "--as", "owner")
	golden(t, "sync-changed-removed", r, "fake", "<SLUG>", "2 added, 1 changed, 3 removed", "0 added, 1 changed, 1 removed")
}
