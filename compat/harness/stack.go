//go:build compat

// Package harness boots an application Docker image with a database and drives it only from the
// outside: HTTP, the operator CLI, MCP and SQL. It knows nothing about the language of the image.
package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dbImage is Postgres with pgvector, pinned by digest like the Dockerfile pins Node (the tag pg17 is
// kept for the reader; the multi-arch index digest was resolved with `docker buildx imagetools
// inspect pgvector/pgvector:pg17`). Dependabot does not watch a Go constant: bump it by hand.
const dbImage = "pgvector/pgvector:pg17@sha256:b513c48566e4777cd3370451400000c78eda6a08131d68f4c31d4a76f27bd8fd"

const (
	defaultImage = "mcp-registry:compat-ts"
	defaultCLI   = "node dist/cli.js"
	dbUser       = "mcp"
	dbName       = "mcp"

	defaultContainerPort = 3000

	suiteLabel   = "compat.suite=mcp-registry"
	staleAfter   = time.Hour
	exitDeadline = 60 * time.Second
	logTail      = "50"
)

// session identifies this test process; every container and network carries it as a label.
var session = randomHex(6)

// OIDCProvider is an OpenID provider the app signs in with. fakes.FakeIdP implements it, so the
// harness does not import the fakes (and the fakes do not import the harness).
type OIDCProvider interface {
	// OIDCSettings returns the values of OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET.
	OIDCSettings() (issuer, clientID, clientSecret string)
}

// Options configure a stack.
type Options struct {
	OIDC  OIDCProvider      // nil = no OIDC (sign-in by links)
	Env   map[string]string // added on top of the defaults
	Unset []string          // default variables to remove (applied after Env)
	// Image and CLI override COMPAT_IMAGE and COMPAT_CLI for this stack (the upgrade scenario boots
	// the old image first). Empty = the environment variable, then the default.
	Image string
	CLI   string
	// PublicHost replaces "localhost" in PUBLIC_URL and Stack.BaseURL (http://<PublicHost>:<port>).
	// The test process resolves any *.compat.test name to the loopback address itself (the same
	// rule a browser run needs through --host-resolver-rules); nothing else resolves it. Empty =
	// localhost.
	PublicHost string
	// ExtraHosts are "name:host-gateway" entries (docker --add-host) of the app and one-off CLI
	// containers, for names a fake advertises that the container must resolve to the host.
	ExtraHosts []string
	// Files are placed inside the app container (and every one-off CLI container) BEFORE the app
	// starts: absolute path -> content, mode 0644. The values of the *_FILE variables need this;
	// content never goes on a command line.
	Files map[string][]byte
	// SecretsKeyFile, when set, is the path of a file that receives the stack's SECRETS_KEY (base64,
	// with a trailing newline), SECRETS_KEY_FILE points at it and SECRETS_KEY is not set.
	SecretsKeyFile string
	// PasswordFile, when set, is the path of a file that receives the database password,
	// PGPASSWORD_FILE points at it and DATABASE_URL carries no password.
	PasswordFile string
	// ContainerPort is the port the app listens on inside its container (PORT is set to it); the
	// published host port maps to it. 0 = 3000, the image's default.
	ContainerPort int

	testName string // set by Start, StartExpectingExit and Shared: names the log file of COMPAT_LOG_DIR
}

// Stack is a running app container with its Postgres.
type Stack struct {
	BaseURL string        // http://localhost:<port>, also PUBLIC_URL
	DB      *pgxpool.Pool // direct database access for checks and time shifting
	Arch    string        // architecture of the image under test: amd64 | arm64

	id      string
	network string
	dbName  string
	app     string
	cli     string
	image   string
	port    int
	cport   int      // the port the app listens on inside the container
	host    string   // host part of BaseURL
	hosts   []string // docker --add-host values given by Options.ExtraHosts
	env     map[string]string
	files   map[string][]byte  // placed by PutFile: path inside the container -> content
	fixed   map[Deviation]bool // the deviations COMPAT_FIXED selects (see deviations.go)
	dbPass  string             // the database password, whichever way the app is given it

	fetchLog func(container string) (string, error) // the app log for COMPAT_LOG_DIR; nil = docker logs
}

// Start boots a database and the app image (COMPAT_IMAGE) and waits for /healthz. The stack is
// removed through t.Cleanup, also when the boot fails.
func Start(t *testing.T, o Options) *Stack {
	t.Helper()
	o.testName = t.Name()
	s, err := boot(o, false)
	if err != nil {
		t.Fatalf("start stack: %v", err)
	}
	t.Cleanup(func() { s.finish(t.Name(), t.Failed()) })
	return s
}

// StartExpectingExit starts the app and waits for it to exit (60 s); it fails if the app becomes
// healthy or keeps running. It returns the exit code and the container's logs, then cleans up.
func StartExpectingExit(t *testing.T, o Options) (code int, logs string) {
	t.Helper()
	o.testName = t.Name()
	s, err := boot(o, true)
	if err != nil {
		t.Fatalf("start stack: %v", err)
	}
	t.Cleanup(func() { s.finish(t.Name(), t.Failed()) })
	code, logs, err = s.awaitExit()
	if err != nil {
		t.Fatalf("expected the app to exit: %v", err)
	}
	return code, logs
}

var (
	sharedMu    sync.Mutex
	sharedStack *Stack
	sharedErr   error
)

// Shared returns the stack of this test binary, built on first use, without OIDC. A failed boot is
// remembered: later callers fail at once with the same error. TestMain (see Main) removes it.
func Shared(t *testing.T) *Stack {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedErr != nil {
		t.Fatalf("shared stack failed to boot earlier: %v", sharedErr)
	}
	if sharedStack == nil {
		s, err := boot(Options{testName: t.Name()}, false)
		if err != nil {
			sharedErr = err
			t.Fatalf("shared stack: %v", err)
		}
		sharedStack = s
	}
	return sharedStack
}

// Main is the TestMain of every package that uses Shared: it removes leftovers of dead runs
// (older than an hour, another session), runs the tests, then removes everything of this session.
func Main(m *testing.M) {
	sweepStale()
	code := m.Run()
	sharedMu.Lock()
	if sharedStack != nil {
		sharedStack.finish("shared", code != 0)
	}
	sharedMu.Unlock()
	removeByLabel("compat.session=" + session)
	os.Exit(code)
}

// DefaultImage is the image under test: COMPAT_IMAGE, else the built-in default. Scenarios use it
// instead of repeating the default.
func DefaultImage() string { return envOr("COMPAT_IMAGE", defaultImage) }

// DefaultCLI is the invocation prefix of the CLI of the image under test: COMPAT_CLI, else the
// built-in default. Scenarios never write the prefix themselves.
func DefaultCLI() string { return envOr("COMPAT_CLI", defaultCLI) }

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system random source failing is not recoverable
	}
	return hex.EncodeToString(b)
}

func freePort() (int, error) {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	tcp, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected address %v", l.Addr())
	}
	return tcp.Port, nil
}

func dockerErr(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (s *Stack) labels() []string {
	return []string{"--label", suiteLabel, "--label", "compat.session=" + session, "--label", "compat.stack=" + s.id}
}

func inspectArch(image string) (string, error) {
	return dockerErr("image", "inspect", "-f", "{{.Architecture}}", image)
}

// boot creates the network, the database and the app. On any error everything created so far is
// removed again. With expectExit the app is only started, not waited for.
func boot(o Options, expectExit bool) (s *Stack, err error) {
	image := o.Image
	if image == "" {
		image = DefaultImage()
	}
	cli := o.CLI
	if cli == "" {
		cli = DefaultCLI()
	}
	fixed, ferr := fixedFromEnv()
	if ferr != nil {
		return nil, ferr
	}
	s = &Stack{fixed: fixed, id: randomHex(4), cli: cli, image: image, host: "localhost", hosts: o.ExtraHosts, cport: defaultContainerPort}
	if o.ContainerPort != 0 {
		s.cport = o.ContainerPort
	}
	for path, content := range o.Files {
		s.rememberFile(path, content)
	}
	if o.PublicHost != "" {
		s.host = o.PublicHost
	}
	defer func() {
		if err != nil {
			s.finish(o.testName, true) // a failed boot always keeps the app log (when COMPAT_LOG_DIR is set)
		}
	}()
	if s.port, err = freePort(); err != nil {
		return s, fmt.Errorf("free port: %w", err)
	}
	s.network = "compat-" + s.id
	s.dbName = s.network + "-db"
	s.app = s.network + "-app"
	s.BaseURL = fmt.Sprintf("http://%s:%d", s.host, s.port)
	if s.Arch, err = inspectArch(image); err != nil {
		return s, err
	}

	pgPassword := randomHex(12)
	s.dbPass = pgPassword
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return s, err
	}
	s.env = map[string]string{
		"DATABASE_URL":  fmt.Sprintf("postgres://%s:%s@db:5432/%s", dbUser, pgPassword, dbName),
		"PUBLIC_URL":    s.BaseURL,
		"COOKIE_SECURE": "false",
		"SECRETS_KEY":   base64.StdEncoding.EncodeToString(key),
		"LOG_LEVEL":     "debug",
	}
	if s.cport != defaultContainerPort {
		s.env["PORT"] = strconv.Itoa(s.cport)
	}
	if o.SecretsKeyFile != "" {
		s.rememberFile(o.SecretsKeyFile, []byte(s.env["SECRETS_KEY"]+"\n"))
		delete(s.env, "SECRETS_KEY")
		s.env["SECRETS_KEY_FILE"] = o.SecretsKeyFile
	}
	if o.PasswordFile != "" {
		s.rememberFile(o.PasswordFile, []byte(pgPassword+"\n"))
		s.env["DATABASE_URL"] = fmt.Sprintf("postgres://%s@db:5432/%s", dbUser, dbName)
		s.env["PGPASSWORD_FILE"] = o.PasswordFile
	}
	if o.OIDC != nil {
		s.env["OIDC_ISSUER"], s.env["OIDC_CLIENT_ID"], s.env["OIDC_CLIENT_SECRET"] = o.OIDC.OIDCSettings()
	}
	extra, perr := appEnv()
	if perr != nil {
		return s, perr
	}
	for k, v := range extra {
		s.env[k] = v
	}
	for k, v := range o.Env {
		s.env[k] = v
	}
	for _, k := range o.Unset {
		delete(s.env, k)
	}

	if _, err = dockerErr(append([]string{"network", "create"}, append(s.labels(), s.network)...)...); err != nil {
		return s, err
	}
	run := []string{
		"run", "-d", "--name", s.dbName, "--network", s.network, "--network-alias", "db",
		"-p", "127.0.0.1::5432",
		"-e", "POSTGRES_USER=" + dbUser, "-e", "POSTGRES_DB=" + dbName, "-e", "POSTGRES_PASSWORD=" + pgPassword,
	}
	run = append(run, s.labels()...)
	if _, err = dockerErr(append(run, dbImage)...); err != nil {
		return s, err
	}
	hostPort, err := dockerErr("port", s.dbName, "5432/tcp")
	if err != nil {
		return s, err
	}
	hostPort = strings.Split(strings.Split(hostPort, "\n")[0], "->")[0] // "127.0.0.1:49153"
	dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", dbUser, pgPassword, strings.TrimSpace(hostPort), dbName)
	if s.DB, err = pgxpool.New(context.Background(), dsn); err != nil {
		return s, fmt.Errorf("database pool: %w", err)
	}
	err = waitFor("Postgres to accept connections", readyLimit, func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return s.DB.Ping(ctx) == nil, nil
	})
	if err != nil {
		return s, err
	}

	if err = s.startAppOnAFreePort(); err != nil {
		return s, err
	}
	if expectExit {
		return s, nil
	}
	return s, s.awaitHealthy()
}

// portTaken reports whether a docker run error says that the published host port is not free
// (the port was probed by freePort and taken by someone else before docker bound it).
func portTaken(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "port is already allocated") || strings.Contains(msg, "address already in use")
}

// startAppOnAFreePort runs the app. freePort probes a port and releases it before docker binds
// it, so another process can take it in between; that failure is explained by docker's own error
// and is the only one retried (3 attempts in all, each with a new port, the reason on stderr).
func (s *Stack) startAppOnAFreePort() error {
	const attempts = 3
	for attempt := 1; ; attempt++ {
		err := s.startApp(s.image, s.env)
		if err == nil || attempt == attempts || !portTaken(err) {
			return err
		}
		fmt.Fprintf(os.Stderr, "harness: host port %d was taken before docker published it (attempt %d of %d): %v\n", s.port, attempt, attempts, err)
		_, _ = dockerErr("rm", "-f", s.app)
		port, perr := freePort()
		if perr != nil {
			return perr
		}
		s.port = port
		s.BaseURL = fmt.Sprintf("http://%s:%d", s.host, port)
		s.env["PUBLIC_URL"] = s.BaseURL
	}
}

// appArgs builds the docker arguments that run (or, with create, only create) the app container;
// env values are on the command line like the harness's own settings: secrets that must not be
// there travel as files (Options.Files).
func (s *Stack) appArgs(verb, image string, env map[string]string) []string {
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	args := []string{verb}
	if verb == "run" {
		args = append(args, "-d")
	}
	args = append(args, "--name", s.app, "--network", s.network,
		"--add-host", "host.docker.internal:host-gateway",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", s.port, s.cport))
	for _, h := range s.hosts {
		args = append(args, "--add-host", h)
	}
	args = append(args, s.labels()...)
	for _, k := range names {
		args = append(args, "-e", k+"="+env[k])
	}
	return append(args, image)
}

// startApp runs the app container (its name is fixed per stack) with env. Files placed with
// PutFile or Options.Files are copied in BEFORE the process starts (create, cp, start), so an app
// that reads a *_FILE variable at once finds them.
func (s *Stack) startApp(image string, env map[string]string) error {
	if len(s.files) == 0 {
		_, err := dockerErr(s.appArgs("run", image, env)...)
		return err
	}
	if _, err := dockerErr(s.appArgs("create", image, env)...); err != nil {
		return err
	}
	for path, content := range s.files {
		if err := copyInto(s.app, path, content); err != nil {
			return err
		}
	}
	_, err := dockerErr("start", s.app)
	return err
}

// rememberFile records a file for every container of the stack (see startApp).
func (s *Stack) rememberFile(path string, content []byte) {
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[path] = bytes.Clone(content)
}

// loopbackURL is where the test process reaches the app whatever PublicHost is.
func (s *Stack) loopbackURL() string { return fmt.Sprintf("http://127.0.0.1:%d", s.port) }

// LoopbackURL is the address the published port answers on, for a caller that must not depend on
// a name resolving (BaseURL may use a PublicHost that only a browser rule resolves).
func (s *Stack) LoopbackURL() string { return s.loopbackURL() }

func (s *Stack) running() (bool, error) {
	out, err := dockerErr("inspect", "-f", "{{.State.Running}}", s.app)
	return out == "true", err
}

// scrub removes the values of secret-looking settings, and anything shaped like a sign-in link
// code (ll_...) or an API token (sk_...), from text that goes into failure messages and logs.
func (s *Stack) scrub(text string) string {
	text = credentialShape.ReplaceAllString(text, "<redacted>")
	for _, v := range s.SecretValues() {
		text = strings.ReplaceAll(text, v, "<redacted>")
	}
	return text
}

// Scrub is scrub for scenarios: text for a failure message with the stack's secrets and credential
// shapes removed.
func (s *Stack) Scrub(text string) string { return s.scrub(text) }

// SecretValues returns the values of the stack's secret-looking settings (names with KEY, SECRET or
// PASSWORD, and DATABASE_URL), the trimmed content of the files that such a NAME_FILE setting
// points at, and the bare database password (also while it sits inside DATABASE_URL, so an output
// that prints only the password is caught as well as one that prints the whole URL), for
// assertions that an output carries none of them.
func (s *Stack) SecretValues() []string {
	var out []string
	secretName := func(u string) bool {
		return strings.Contains(u, "KEY") || strings.Contains(u, "SECRET") || strings.Contains(u, "PASSWORD")
	}
	for k, v := range s.env {
		u := strings.ToUpper(k)
		if v == "" {
			continue
		}
		if secretName(u) && !strings.HasSuffix(u, "_FILE") || u == "DATABASE_URL" {
			out = append(out, v)
		}
		if content, ok := s.files[v]; ok && strings.HasSuffix(u, "_FILE") && secretName(u) {
			if secret := strings.TrimSpace(string(content)); secret != "" {
				out = append(out, secret)
			}
		}
	}
	if s.dbPass != "" {
		out = append(out, s.dbPass)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func (s *Stack) tailLogs() string {
	out, err := dockerLogs(s.app, logTail)
	if err != nil {
		return "(no logs: " + err.Error() + ")"
	}
	return s.scrub(out)
}

func dockerLogs(container, tail string) (string, error) {
	args := []string{"logs"}
	if tail != "" {
		args = append(args, "--tail", tail)
	}
	cmd := exec.Command("docker", append(args, container)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// awaitHealthy polls /healthz until it answers ok; it fails at once if the container stops, with
// the last 50 log lines (secrets removed).
func (s *Stack) awaitHealthy() error {
	c := newHTTPClient(nil)
	err := waitFor("/healthz to answer ok", readyLimit, func() (bool, error) {
		if up, err := s.running(); err != nil || !up {
			return false, errors.New("the app container is not running")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.loopbackURL()+"/healthz", nil)
		if err != nil {
			return false, err
		}
		resp, err := c.Do(req)
		if err != nil {
			return false, nil //nolint:nilerr // not reachable yet: keep polling
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK && string(body) == "ok", nil
	})
	if err != nil {
		return fmt.Errorf("%w\n--- last %s lines of the app log ---\n%s", err, logTail, s.tailLogs())
	}
	return nil
}

// awaitExit waits for the app container to exit; it is an error if /healthz answers ok first or the
// container is still running after 60 s. The container is removed afterwards.
func (s *Stack) awaitExit() (code int, logs string, err error) {
	c := newHTTPClient(nil)
	err = waitFor("the app to exit", exitDeadline, func() (bool, error) {
		up, ierr := s.running()
		if ierr != nil {
			return false, ierr
		}
		if !up {
			return true, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, s.loopbackURL()+"/healthz", nil); rerr == nil {
			if resp, derr := c.Do(req); derr == nil {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK && string(body) == "ok" {
					return false, errors.New("the app became healthy")
				}
			}
		}
		return false, nil
	})
	if err != nil {
		return 0, "", fmt.Errorf("%w\n--- last %s lines of the app log ---\n%s", err, logTail, s.tailLogs())
	}
	out, err := dockerErr("inspect", "-f", "{{.State.ExitCode}}", s.app)
	if err != nil {
		return 0, "", err
	}
	if code, err = strconv.Atoi(out); err != nil {
		return 0, "", err
	}
	logs, err = dockerLogs(s.app, "")
	if err != nil {
		return 0, "", err
	}
	_, _ = dockerErr("rm", "-f", s.app)
	return code, logs, nil
}

// SwapImage stops the app, then starts image on the same database. cli is the CLI command for
// that image ("" keeps the current one). The optional env maps are applied on top of the stack's
// settings for this run only (an empty value removes the variable); they are not remembered.
func (s *Stack) SwapImage(t *testing.T, image, cli string, env ...map[string]string) {
	t.Helper()
	if err := s.swap(image, cli, env); err != nil {
		t.Fatalf("swap image: %v", err)
	}
	if err := s.awaitHealthy(); err != nil {
		t.Fatalf("swap image: %v", err)
	}
}

// SwapImageExpectingExit is SwapImage for a start that must fail: the new app runs with env on top
// of the stack's settings, the call waits for it to exit (60 s) and returns its exit code and logs.
// The database stays; a later SwapImage brings a good app back.
func (s *Stack) SwapImageExpectingExit(t *testing.T, image, cli string, env map[string]string) (code int, logs string) {
	t.Helper()
	if err := s.swap(image, cli, []map[string]string{env}); err != nil {
		t.Fatalf("swap image: %v", err)
	}
	code, logs, err := s.awaitExit()
	if err != nil {
		t.Fatalf("expected the app to exit: %v", err)
	}
	return code, logs
}

func (s *Stack) swap(image, cli string, overrides []map[string]string) error {
	_, _ = dockerErr("rm", "-f", s.app)
	arch, err := inspectArch(image)
	if err != nil {
		return err
	}
	s.image, s.Arch = image, arch
	if cli != "" {
		s.cli = cli
	}
	env := make(map[string]string, len(s.env))
	for k, v := range s.env {
		env[k] = v
	}
	for _, o := range overrides {
		for k, v := range o {
			if v == "" {
				delete(env, k)
			} else {
				env[k] = v
			}
		}
	}
	return s.startApp(image, env) // files placed with PutFile survive: startApp copies them in
}

// TryLogs is Logs for callers that are not the test goroutine: it returns the error instead of
// failing the test.
func (s *Stack) TryLogs() (string, error) { return dockerLogs(s.app, "") }

// Logs returns what the app container wrote to stdout and stderr.
func (s *Stack) Logs(t *testing.T) string {
	t.Helper()
	out, err := dockerLogs(s.app, "")
	if err != nil {
		t.Fatalf("docker logs: %v", err)
	}
	return out
}

// finish saves the app log when the test failed (see saveLog), then tears the stack down.
func (s *Stack) finish(testName string, failed bool) {
	if failed {
		s.saveLog(testName)
	}
	s.teardown()
}

// teardown removes this stack's containers and network (by label) and closes the pool; it is
// safe on a half-built stack.
func (s *Stack) teardown() {
	if s.DB != nil {
		s.DB.Close()
	}
	removeByLabel("compat.stack=" + s.id)
}

// removeByLabel removes containers and then networks that carry the label.
func removeByLabel(label string) {
	if ids, err := dockerErr("ps", "-aq", "--filter", "label="+label); err == nil && ids != "" {
		_, _ = dockerErr(append([]string{"rm", "-f", "-v"}, strings.Fields(ids)...)...)
	}
	if ids, err := dockerErr("network", "ls", "-q", "--filter", "label="+label); err == nil && ids != "" {
		_, _ = dockerErr(append([]string{"network", "rm"}, strings.Fields(ids)...)...)
	}
}

// sweepStale removes suite containers and networks of other sessions older than an hour, left by
// runs that died without cleaning up. Younger ones may belong to a parallel run.
func sweepStale() {
	stale := func(kind string) []string {
		list := []string{"ps", "-aq"}
		inspect := []string{"inspect"}
		if kind == "network" {
			list = []string{"network", "ls", "-q"}
			inspect = []string{"network", "inspect"}
		}
		ids, err := dockerErr(append(list, "--filter", "label="+suiteLabel)...)
		if err != nil || ids == "" {
			return nil
		}
		raw, err := dockerErr(append(inspect, strings.Fields(ids)...)...)
		if err != nil {
			return nil
		}
		var items []struct {
			ID      string
			Created string
			Labels  map[string]string
			Config  struct{ Labels map[string]string }
		}
		if json.Unmarshal([]byte(raw), &items) != nil {
			return nil
		}
		var old []string
		for _, it := range items {
			labels := it.Labels
			if kind == "container" {
				labels = it.Config.Labels
			}
			created, perr := time.Parse(time.RFC3339Nano, it.Created)
			if perr == nil && labels["compat.session"] != session && time.Since(created) > staleAfter {
				old = append(old, it.ID)
			}
		}
		return old
	}
	if ids := stale("container"); len(ids) > 0 {
		_, _ = dockerErr(append([]string{"rm", "-f", "-v"}, ids...)...)
	}
	if ids := stale("network"); len(ids) > 0 {
		_, _ = dockerErr(append([]string{"network", "rm"}, ids...)...)
	}
}

// MCP connects to /mcp with a bearer token (an sk_ value from Client.Token). The session is
// closed when the test ends.
func (s *Stack) MCP(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	return s.MCPRecording(t, token, nil)
}

// MCPRecording is MCP that also hands every HTTP response body of the session to record, as the
// raw bytes the server wrote (an event stream or a JSON message, before the SDK decodes and drops
// what it does not know), once the body has been read to its end or closed. record may be called
// from several goroutines. A nil record is MCP.
func (s *Stack) MCPRecording(t *testing.T, token string, record func(body []byte)) *mcp.ClientSession {
	t.Helper()
	hc := newHTTPClient(nil)
	hc.Transport = &bearerTransport{base: hc.Transport, token: token, record: record}
	client := mcp.NewClient(&mcp.Implementation{Name: "compat-harness", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             s.BaseURL + "/mcp",
		HTTPClient:           hc,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("mcp connect (token %s): %v", Redact(token), err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

type bearerTransport struct {
	base   http.RoundTripper
	token  string
	record func([]byte)
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := b.base.RoundTrip(r)
	if err == nil && b.record != nil && resp.Body != nil {
		resp.Body = &recordingBody{ReadCloser: resp.Body, record: b.record}
	}
	return resp, err
}

// recordingBody copies what is read and reports it once, at the end of the body or at Close.
type recordingBody struct {
	io.ReadCloser
	record func([]byte)
	mu     sync.Mutex
	buf    bytes.Buffer
	done   bool
}

func (r *recordingBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	r.buf.Write(p[:n])
	r.mu.Unlock()
	if err != nil {
		r.flush()
	}
	return n, err
}

func (r *recordingBody) Close() error {
	r.flush()
	return r.ReadCloser.Close()
}

func (r *recordingBody) flush() {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true
	data := bytes.Clone(r.buf.Bytes())
	r.mu.Unlock()
	if len(data) > 0 {
		r.record(data)
	}
}
