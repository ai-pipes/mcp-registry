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
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultImage = "mcp-registry:compat-ts"
	defaultCLI   = "node dist/cli.js"
	dbImage      = "pgvector/pgvector:pg17"
	dbUser       = "mcp"
	dbName       = "mcp"
)

// FakeIdP describes an OpenID provider for the app to use. A later task gives it behaviour; the
// harness only turns it into the OIDC_* settings.
type FakeIdP struct { //nolint:revive // the name is fixed by the suite interface shared with later tasks
	Issuer       string
	ClientID     string
	ClientSecret string
}

// Options configure a stack.
type Options struct {
	OIDC *FakeIdP          // nil = no OIDC (sign-in by links)
	Env  map[string]string // added on top of the defaults
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
	env     map[string]string
}

// Start boots a database and the app image (COMPAT_IMAGE) and waits for /healthz. The stack is
// removed through t.Cleanup.
func Start(t *testing.T, o Options) *Stack {
	t.Helper()
	s := start(t, o)
	t.Cleanup(func() { s.teardown() })
	return s
}

var (
	sharedMu    sync.Mutex
	sharedStack *Stack
)

// Shared returns the stack of this test binary, built on first use, without OIDC. TestMain
// (see Main) removes it.
func Shared(t *testing.T) *Stack {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedStack == nil {
		sharedStack = start(t, Options{})
	}
	return sharedStack
}

// Main is the TestMain of every package that uses Shared: it runs the tests and then removes the
// shared stack.
func Main(m *testing.M) {
	code := m.Run()
	sharedMu.Lock()
	if sharedStack != nil {
		sharedStack.teardown()
	}
	sharedMu.Unlock()
	os.Exit(code)
}

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

func freePort(t testing.TB) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	tcp, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("free port: unexpected address %v", l.Addr())
	}
	return tcp.Port
}

// docker runs the docker CLI and returns its trimmed stdout.
func docker(t testing.TB, args ...string) string {
	t.Helper()
	out, err := dockerErr(args...)
	if err != nil {
		t.Fatalf("docker %s: %v", args[0], err)
	}
	return out
}

func dockerErr(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func start(t testing.TB, o Options) *Stack {
	t.Helper()
	image := envOr("COMPAT_IMAGE", defaultImage)
	s := &Stack{
		id:    randomHex(4),
		cli:   envOr("COMPAT_CLI", defaultCLI),
		image: image,
		port:  freePort(t),
	}
	s.network = "compat-" + s.id
	s.dbName = s.network + "-db"
	s.app = s.network + "-app"
	s.BaseURL = fmt.Sprintf("http://localhost:%d", s.port)
	s.Arch = docker(t, "image", "inspect", "-f", "{{.Architecture}}", image)

	pgPassword := randomHex(12)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s.env = map[string]string{
		"DATABASE_URL":  fmt.Sprintf("postgres://%s:%s@db:5432/%s", dbUser, pgPassword, dbName),
		"PUBLIC_URL":    s.BaseURL,
		"COOKIE_SECURE": "false",
		"SECRETS_KEY":   base64.StdEncoding.EncodeToString(key),
		"LOG_LEVEL":     "debug",
	}
	if o.OIDC != nil {
		s.env["OIDC_ISSUER"] = o.OIDC.Issuer
		s.env["OIDC_CLIENT_ID"] = o.OIDC.ClientID
		s.env["OIDC_CLIENT_SECRET"] = o.OIDC.ClientSecret
	}
	for k, v := range o.Env {
		s.env[k] = v
	}

	docker(t, "network", "create", s.network)
	docker(t, "run", "-d", "--name", s.dbName, "--network", s.network, "--network-alias", "db",
		"-p", "127.0.0.1::5432",
		"-e", "POSTGRES_USER="+dbUser, "-e", "POSTGRES_DB="+dbName, "-e", "POSTGRES_PASSWORD="+pgPassword,
		dbImage)
	hostPort := docker(t, "port", s.dbName, "5432/tcp")
	hostPort = strings.Split(strings.Split(hostPort, "\n")[0], "->")[0] // "127.0.0.1:49153"
	dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", dbUser, pgPassword, strings.TrimSpace(hostPort), dbName)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		s.teardown()
		t.Fatalf("database pool: %v", err)
	}
	s.DB = pool
	eventuallyWithin(t, "Postgres to accept connections", readyLimit, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return pool.Ping(ctx) == nil
	})

	s.runApp(t)
	return s
}

// runApp starts the app container (the name is fixed per stack) and waits until it is healthy.
func (s *Stack) runApp(t testing.TB) {
	t.Helper()
	names := make([]string, 0, len(s.env))
	for k := range s.env {
		names = append(names, k)
	}
	sort.Strings(names)
	args := []string{
		"run", "-d", "--name", s.app, "--network", s.network,
		"--add-host", "host.docker.internal:host-gateway",
		"-p", fmt.Sprintf("127.0.0.1:%d:3000", s.port),
	}
	for _, k := range names {
		args = append(args, "-e", k+"="+s.env[k])
	}
	args = append(args, s.image)
	if _, err := dockerErr(args...); err != nil {
		s.teardown()
		t.Fatalf("start app: %v", err)
	}
	c := newHTTPClient(nil)
	eventuallyWithin(t, "/healthz to answer ok", readyLimit, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/healthz", nil)
		if err != nil {
			return false
		}
		resp, err := c.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK && string(body) == "ok"
	})
}

// SwapImage stops the app, then starts image on the same database. cli is the CLI command for
// that image ("" keeps the current one).
func (s *Stack) SwapImage(t *testing.T, image, cli string) {
	t.Helper()
	docker(t, "rm", "-f", s.app)
	s.image = image
	if cli != "" {
		s.cli = cli
	}
	s.Arch = docker(t, "image", "inspect", "-f", "{{.Architecture}}", image)
	s.runApp(t)
}

// Logs returns what the app container wrote to stdout and stderr.
func (s *Stack) Logs(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("docker", "logs", s.app)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker logs: %v", err)
	}
	return out.String()
}

// teardown removes the containers and the network; failures are ignored (the resources may not
// exist yet when a start failed half way).
func (s *Stack) teardown() {
	if s.DB != nil {
		s.DB.Close()
	}
	_, _ = dockerErr("rm", "-f", "-v", s.app, s.dbName)
	_, _ = dockerErr("network", "rm", s.network)
}

// MCP connects to /mcp with a bearer token (an sk_ value from Client.Token). The session is
// closed when the test ends.
func (s *Stack) MCP(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	hc := newHTTPClient(nil)
	hc.Transport = &bearerTransport{base: hc.Transport, token: token}
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
	base  http.RoundTripper
	token string
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}
