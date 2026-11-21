//go:build integration

package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/apps/server/migrations"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	levelInfo  = 30
	levelWarn  = 40
	levelError = 50
	levelFatal = 60
	waitLimit  = 30 * time.Second
)

// logBuf collects log lines; the app writes to it from several goroutines.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuf) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, r)
	}
	return out
}

func (l *logBuf) find(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range l.records(t) {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

func (l *logBuf) messages(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, r := range l.records(t) {
		out = append(out, r["msg"].(string))
	}
	return out
}

// assertNoKeyMaterial fails when the log holds a key in a form a log line could carry: standard or
// URL-safe base64 (padded or not) or hex, either case. Raw bytes would never match: a log line is
// JSON text.
func assertNoKeyMaterial(t *testing.T, logs string, keys ...[]byte) {
	t.Helper()
	for _, k := range keys {
		hexForm := hex.EncodeToString(k)
		for _, form := range []string{
			base64.StdEncoding.EncodeToString(k), base64.RawStdEncoding.EncodeToString(k),
			base64.URLEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k),
			hexForm, strings.ToUpper(hexForm),
		} {
			if strings.Contains(logs, form) {
				t.Fatalf("the log contains key material (%d characters of a key form)", len(form))
			}
		}
	}
}

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func baseConfig(url string, key []byte) config.Config {
	return config.Config{
		Port: 1, DatabaseURL: config.NewSecret(url), PublicURL: "http://localhost:3000", TrustProxyHops: 1,
		SecretsKey: config.NewSecretBytes(key), LogLevel: "info",
	}
}

// instance is one running Run.
type instance struct {
	t      *testing.T
	logs   *logBuf
	cancel context.CancelCauseFunc
	done   chan int
	base   string // http://127.0.0.1:<port>, empty when Run was given a port to bind itself
}

// startInstance runs Run on a pre-made listener on port 0 and waits for `listening`.
func startInstance(t *testing.T, cfg config.Config, edit func(*Options)) *instance {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	in := launch(t, cfg, func(o *Options) {
		o.listener = ln
		if edit != nil {
			edit(o)
		}
	})
	in.waitFor("listening")
	in.base = "http://" + ln.Addr().String()
	return in
}

// launch starts Run in a goroutine without waiting for anything.
func launch(t *testing.T, cfg config.Config, edit func(*Options)) *instance {
	t.Helper()
	logs := &logBuf{}
	ctx, cancel := context.WithCancelCause(context.Background())
	o := Options{Config: cfg, Version: "9.9.9-test", WebRoot: t.TempDir(), Logger: logging.New("info", logs)}
	if edit != nil {
		edit(&o)
	}
	in := &instance{t: t, logs: logs, cancel: cancel, done: make(chan int, 1)}
	go func() { in.done <- Run(ctx, o) }()
	t.Cleanup(func() {
		cancel(nil)
		select {
		case code := <-in.done:
			in.done <- code
		case <-time.After(waitLimit):
			t.Errorf("Run did not return after cancel; log:\n%s", logs.String())
		}
	})
	return in
}

// waitFor polls the log until a record with msg appears; it fails when Run ends first.
func (in *instance) waitFor(msg string) {
	in.t.Helper()
	if err := in.waitErr(msg); err != nil {
		in.t.Fatal(err)
	}
}

// waitErr is waitFor for goroutines other than the test's: it returns the failure.
func (in *instance) waitErr(msg string) error {
	deadline := time.After(waitLimit)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if len(in.logs.find(in.t, msg)) > 0 {
			return nil
		}
		select {
		case code := <-in.done:
			in.done <- code
			if len(in.logs.find(in.t, msg)) > 0 {
				return nil
			}
			return fmt.Errorf("Run ended with %d before %q; log:\n%s", code, msg, in.logs.String())
		case <-deadline:
			return fmt.Errorf("no %q record; log:\n%s", msg, in.logs.String())
		case <-tick.C:
		}
	}
}

// exit waits for Run to return.
func (in *instance) exit() int {
	in.t.Helper()
	select {
	case code := <-in.done:
		in.done <- code
		return code
	case <-time.After(waitLimit):
		in.t.Fatalf("Run did not return; log:\n%s", in.logs.String())
		return -1
	}
}

func (in *instance) stop(signal string) int {
	in.t.Helper()
	in.cancel(Signal(signal))
	return in.exit()
}

func get(t *testing.T, url string, header ...string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

// fetch is get for goroutines other than the test's: it returns the error.
func fetch(url string) (int, string, error) {
	res, err := http.Get(url) //nolint:gosec // G107: the test's own server
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), nil
}

// await receives from ch or fails the test after waitLimit.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitLimit):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func levelOf(r map[string]any) int { return int(r["level"].(float64)) }

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec // G102: the server binds every interface; the port probe must match it
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// exec runs one statement on its own short-lived connection.
func exec(t *testing.T, url, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

// appConnections counts the other sessions on the database of url.
func appConnections(t *testing.T, url string) int {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	var n int
	if err := c.QueryRow(ctx, "select count(*) from pg_stat_activity where datname = current_database() and pid <> pg_backend_pid()").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eventuallyNoConnections(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for appConnections(t, url) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the pool is still open")
		}
		time.Sleep(20 * time.Millisecond) //nolint:forbidigo // polling a server-side count with a deadline
	}
}

func TestCleanStartAndStop(t *testing.T) {
	url := pgtest.StartURL(t)
	in := startInstance(t, baseConfig(url, newKey(t)), nil)

	want := []string{"starting", "migrations applied", msgKeyCreated, msgNoOutboxWorker, "listening"}
	if got := in.logs.messages(t); !slices.Equal(got, want) {
		t.Fatalf("records: %q, want %q", got, want)
	}
	recs := in.logs.records(t)
	if recs[0]["version"] != "9.9.9-test" || levelOf(recs[0]) != levelInfo {
		t.Errorf("starting: %v", recs[0])
	}
	if applied, ok := recs[1]["applied"].([]any); !ok || len(applied) == 0 {
		t.Errorf("applied on an empty database: %v", recs[1]["applied"])
	}
	port := in.base[strings.LastIndex(in.base, ":")+1:]
	if recs[4]["version"] != "9.9.9-test" || fmt.Sprint(recs[4]["port"]) != port {
		t.Errorf("listening: %v (port %s)", recs[4], port)
	}

	if code, body, _ := get(t, in.base+"/healthz"); code != 200 || body != "ok" {
		t.Fatalf("/healthz: %d %q", code, body)
	}

	if code := in.stop("SIGINT"); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, in.logs.String())
	}
	tail := in.logs.messages(t)
	if !slices.Equal(tail[len(tail)-2:], []string{"shutting down", "stopped"}) {
		t.Fatalf("tail: %q", tail)
	}
	if sd := in.logs.find(t, "shutting down")[0]; sd["signal"] != "SIGINT" {
		t.Errorf("shutting down: %v", sd)
	}
	t.Logf("one clean start and stop:\n%s", in.logs.String())
	eventuallyNoConnections(t, url)
}

// compat: TestPlatformMigrationsAndKeyCheck "a restart with the same key applies nothing".
func TestRestartAppliesNothingAndSaysNothingAboutTheKey(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	first := startInstance(t, baseConfig(url, key), nil)
	first.stop("SIGTERM")

	second := startInstance(t, baseConfig(url, key), nil)
	if raw := second.logs.String(); !strings.Contains(raw, `"applied":[]`) {
		t.Fatalf("applied must be an empty array, log:\n%s", raw)
	}
	if got := second.logs.messages(t); !slices.Equal(got, []string{"starting", "migrations applied", msgNoOutboxWorker, "listening"}) {
		t.Fatalf("records: %q", got)
	}
	if code := second.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if sd := second.logs.find(t, "shutting down")[0]; sd["signal"] != "SIGTERM" {
		t.Errorf("signal: %v", sd)
	}
}

// compat: TestPlatformMigrationsAndKeyCheck "another key: exit 1, one fatal record, no key in the log".
func TestAnotherKeyIsRefusedWithOneFatalRecord(t *testing.T) {
	url := pgtest.StartURL(t)
	right, wrong := newKey(t), newKey(t)
	startInstance(t, baseConfig(url, right), nil).stop("SIGTERM")

	cfg := baseConfig(url, wrong)
	cfg.Port = freePort(t)
	in := launch(t, cfg, nil)
	if code := in.exit(); code != 1 {
		t.Fatalf("exit %d; log:\n%s", code, in.logs.String())
	}
	var fatal []map[string]any
	for _, r := range in.logs.records(t) {
		if levelOf(r) == levelFatal {
			fatal = append(fatal, r)
		}
	}
	if len(fatal) != 1 || fatal[0]["msg"] != msgKeyMismatch {
		t.Fatalf("fatal records: %v", fatal)
	}
	if !strings.Contains(msgKeyMismatch, `See "Recovering from a key mismatch" in the README.`) {
		t.Fatal("the fatal text does not point at the README section")
	}
	if len(in.logs.find(t, "listening")) != 0 {
		t.Fatal("listening after a refused start")
	}
	assertNoKeyMaterial(t, in.logs.String(), right, wrong)
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", cfg.Port))
	if err != nil {
		t.Fatalf("the port was opened by a refused start: %v", err)
	}
	_ = ln.Close()
	eventuallyNoConnections(t, url)

	// The right key still starts: the refused start changed nothing.
	startInstance(t, baseConfig(url, right), nil).stop("SIGTERM")
}

// compat: TestPlatformMigrationsAndKeyCheck "a database from a newer version is refused".
func TestDatabaseFromANewerVersionIsRefused(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	startInstance(t, baseConfig(url, key), nil).stop("SIGTERM")

	const future = "9999_from_the_future.sql"
	exec(t, url, "insert into platform.schema_migrations (name) values ($1)", future)

	cfg := baseConfig(url, key)
	cfg.Port = freePort(t)
	in := launch(t, cfg, nil)
	if code := in.exit(); code != 1 {
		t.Fatalf("exit %d; log:\n%s", code, in.logs.String())
	}
	var fatal []map[string]any
	for _, r := range in.logs.records(t) {
		if levelOf(r) == levelFatal {
			fatal = append(fatal, r)
		}
	}
	if len(fatal) != 1 {
		t.Fatalf("fatal records: %v", fatal)
	}
	msg := fatal[0]["msg"].(string)
	if !strings.Contains(msg, "migrated by a newer version of mcp-registry (unknown migration: "+future+")") ||
		!strings.Contains(msg, "restore the backup taken before the upgrade") {
		t.Fatalf("fatal message: %q", msg)
	}
	if len(in.logs.find(t, "migrations applied")) != 0 || len(in.logs.find(t, "listening")) != 0 {
		t.Fatalf("the start went on: %q", in.logs.messages(t))
	}
	eventuallyNoConnections(t, url)

	exec(t, url, "delete from platform.schema_migrations where name = $1", future)
	startInstance(t, baseConfig(url, key), nil).stop("SIGTERM") // the lock was released
}

// compat: TestPlatformMigrationsAndKeyCheck "the old key alone: healthy, one warning".
func TestSecretsKeyOldWarnings(t *testing.T) {
	url := pgtest.StartURL(t)
	oldKey, next := newKey(t), newKey(t)

	// A fresh database with both keys set: the rotation warning, the check is created with the new key.
	cfg := baseConfig(url, oldKey)
	cfg.SecretsKeyOld = config.NewSecretBytes(newKey(t))
	in := startInstance(t, cfg, nil)
	warn := in.logs.find(t, msgKeyOldSet)
	if len(warn) != 1 || levelOf(warn[0]) != levelWarn {
		t.Fatalf("SECRETS_KEY_OLD warnings: %v", warn)
	}
	if got := in.logs.messages(t); got[0] != "starting" || got[1] != msgKeyOldSet {
		t.Fatalf("the warning comes right after starting: %q", got)
	}
	in.stop("SIGTERM")

	// The check opens with the old key only: one more warning, and the server runs.
	cfg = baseConfig(url, next)
	cfg.SecretsKeyOld = config.NewSecretBytes(oldKey)
	in = startInstance(t, cfg, nil)
	rot := in.logs.find(t, msgKeyNeedsRotation)
	if len(rot) != 1 || levelOf(rot[0]) != levelWarn {
		t.Fatalf("needs-rotation warnings: %v", rot)
	}
	assertNoKeyMaterial(t, in.logs.String(), oldKey, next)
	in.stop("SIGTERM")
}

// A request in flight when the context is cancelled still completes, also when it uses the pool:
// the pool is closed only after the request has drained.
func TestInFlightRequestCompletesOnShutdown(t *testing.T) {
	url := pgtest.StartURL(t)
	started := make(chan struct{})
	in := startInstance(t, baseConfig(url, newKey(t)), func(o *Options) {
		o.mount = func(mux *http.ServeMux, pool *pgxpool.Pool) {
			mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
				close(started)
				if _, err := pool.Exec(r.Context(), "select pg_sleep(0.3)"); err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = w.Write([]byte("done"))
			})
		}
	})

	type result struct {
		code int
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		code, body, err := fetch(in.base + "/slow")
		res <- result{code, body, err}
	}()
	await(t, started, "the slow handler")
	in.cancel(Signal("SIGTERM"))
	if r := await(t, res, "the slow response"); r.err != nil || r.code != 200 || r.body != "done" {
		t.Fatalf("in-flight request: %d %q %v", r.code, r.body, r.err)
	}
	if code := in.exit(); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, in.logs.String())
	}
	tail := in.logs.messages(t)
	if tail[len(tail)-1] != "stopped" {
		t.Fatalf("tail: %q", tail)
	}
	if _, _, err := fetch(in.base + "/healthz"); err == nil {
		t.Fatal("the server still accepts connections after stopped")
	}
}

// A request that outlives the limit: shutdown timed out, exit 1.
func TestShutdownTimesOut(t *testing.T) {
	url := pgtest.StartURL(t)
	started, release := make(chan struct{}), make(chan struct{})
	in := startInstance(t, baseConfig(url, newKey(t)), func(o *Options) {
		o.shutdownTimeout = 300 * time.Millisecond
		o.mount = func(mux *http.ServeMux, _ *pgxpool.Pool) {
			mux.HandleFunc("GET /stuck", func(_ http.ResponseWriter, _ *http.Request) {
				close(started)
				<-release
			})
		}
	})
	t.Cleanup(func() { close(release) })
	go func() { _, _, _ = fetch(in.base + "/stuck") }()
	await(t, started, "the stuck handler")
	if code := in.stop("SIGTERM"); code != 1 {
		t.Fatalf("exit %d; log:\n%s", code, in.logs.String())
	}
	to := in.logs.find(t, "shutdown timed out")
	if len(to) != 1 || levelOf(to[0]) != levelError {
		t.Fatalf("shutdown timed out records: %v", to)
	}
	if len(in.logs.find(t, "stopped")) != 0 {
		t.Fatal("stopped after a timeout")
	}
}

// A signal that arrives before Run (or during its start) stops it before the port is opened.
func TestSignalDuringStartStopsBeforeListening(t *testing.T) {
	url := pgtest.StartURL(t)
	cfg := baseConfig(url, newKey(t))
	cfg.Port = freePort(t)
	logs := &logBuf{}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(Signal("SIGINT"))
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, Options{Config: cfg, Version: "v", WebRoot: t.TempDir(), Logger: logging.New("info", logs)})
	}()
	if code := await(t, done, "Run"); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, logs.String())
	}
	msgs := logs.messages(t)
	if len(msgs) < 3 || msgs[0] != "starting" || msgs[len(msgs)-2] != "shutting down" || msgs[len(msgs)-1] != "stopped" {
		t.Fatalf("records: %q", msgs)
	}
	if sd := logs.find(t, "shutting down")[0]; sd["signal"] != "SIGINT" {
		t.Errorf("shutting down: %v", sd)
	}
	if len(logs.find(t, "listening")) != 0 {
		t.Fatal("listening after a signal during the start")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", cfg.Port)) //nolint:gosec // G102: the server binds every interface
	if err != nil {
		t.Fatalf("the port was opened: %v", err)
	}
	_ = ln.Close()
	eventuallyNoConnections(t, url)
}

// A header block above 16 KiB is refused (Node's limit), not read up to Go's 1 MiB default.
func TestOversizedHeadersAreRefused(t *testing.T) {
	in := startInstance(t, baseConfig(pgtest.StartURL(t), newKey(t)), nil)
	req, err := http.NewRequest("GET", in.base+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Big", strings.Repeat("a", 32<<10))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status %d, want 431", res.StatusCode)
	}
	if code, body, _ := get(t, in.base+"/healthz"); code != 200 || body != "ok" {
		t.Fatalf("/healthz after: %d %q", code, body)
	}
}

// compat: the port is taken: a fatal record, exit 1, the pool closed.
func TestPortInUseIsFatal(t *testing.T) {
	url := pgtest.StartURL(t)
	busy, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec // G102: the server binds every interface; the port in use must match it
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	cfg := baseConfig(url, newKey(t))
	cfg.Port = busy.Addr().(*net.TCPAddr).Port

	in := launch(t, cfg, nil)
	if code := in.exit(); code != 1 {
		t.Fatalf("exit %d; log:\n%s", code, in.logs.String())
	}
	var fatal []map[string]any
	for _, r := range in.logs.records(t) {
		if levelOf(r) == levelFatal {
			fatal = append(fatal, r)
		}
	}
	if len(fatal) != 1 || len(in.logs.find(t, "listening")) != 0 {
		t.Fatalf("records: %v", in.logs.messages(t))
	}
	eventuallyNoConnections(t, url)
}

// Two servers started at once on an empty database both come up.
func TestTwoServersOnAnEmptyDatabase(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	var ins [2]*instance
	errs := make(chan error, len(ins))
	for i := range ins {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		ins[i] = launch(t, baseConfig(url, key), func(o *Options) { o.listener = ln })
		ins[i].base = "http://" + ln.Addr().String()
		go func() { errs <- ins[i].waitErr("listening") }()
	}
	for range ins {
		if err := await(t, errs, "a server to listen"); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range ins {
		if code, body, _ := get(t, in.base+"/healthz"); code != 200 || body != "ok" {
			t.Fatalf("/healthz: %d %q", code, body)
		}
		if n := len(in.logs.find(t, "migrations applied")); n != 1 {
			t.Fatalf("migrations applied records: %d", n)
		}
		for _, r := range in.logs.records(t) {
			if levelOf(r) >= levelFatal {
				t.Fatalf("fatal record: %v", r)
			}
		}
	}
	// Between the two: one key check created, and each bundled file applied exactly once (the
	// advisory lock made one run apply everything and the other nothing).
	created := 0
	var applied []string
	for _, in := range ins {
		created += len(in.logs.find(t, msgKeyCreated))
		for _, f := range in.logs.find(t, "migrations applied")[0]["applied"].([]any) {
			applied = append(applied, f.(string))
		}
	}
	if created != 1 {
		t.Fatalf("%d `secrets key check created` records across both servers, want 1", created)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var bundled []string
	for _, e := range entries {
		bundled = append(bundled, e.Name())
	}
	slices.Sort(applied)
	if len(bundled) != 10 || !slices.Equal(applied, bundled) {
		t.Fatalf("applied %v across both servers, bundled %v: each file exactly once", applied, bundled)
	}
	for _, in := range ins {
		if code := in.stop("SIGTERM"); code != 0 {
			t.Fatalf("exit %d", code)
		}
	}
}

// compat: TestPlatformAccessLog, TestPlatformStaticFilesAndSPA on the running server.
func TestAccessLogAndStaticFiles(t *testing.T) {
	url := pgtest.StartURL(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(`<div id="root"></div>`), 0o600); err != nil {
		t.Fatal(err)
	}
	in := startInstance(t, baseConfig(url, newKey(t)), func(o *Options) { o.WebRoot = root })

	const marker = "sk_secret-token-marker"
	code, _, h := get(t, in.base+"/api/auth/methods?probe=query-marker", "Authorization", "Bearer "+marker)
	id := h.Get("X-Request-Id")
	if code != 404 || id == "" { // no module serves /api yet
		t.Fatalf("probe: %d, id %q", code, id)
	}
	var rec map[string]any
	for _, r := range in.logs.records(t) {
		if r["reqId"] == id {
			rec = r
		}
	}
	if rec == nil || levelOf(rec) != levelInfo || rec["method"] != "GET" || rec["path"] != "/api/auth/methods" || rec["status"] != float64(404) {
		t.Fatalf("access log record: %v", rec)
	}
	if strings.Contains(in.logs.String(), marker) || strings.Contains(in.logs.String(), "query-marker") {
		t.Fatal("a header or a query string reached the log")
	}

	code, body, h := get(t, in.base+"/catalog", "Accept", "text/html")
	if code != 200 || !strings.Contains(body, `<div id="root">`) || h.Get("Cache-Control") != "no-cache" {
		t.Fatalf("/catalog: %d %q %q", code, body, h.Get("Cache-Control"))
	}
	if code, body, _ := get(t, in.base+"/healthz/x", "Accept", "text/html"); code != 404 || strings.Contains(body, `<div id="root">`) {
		t.Fatalf("/healthz/x: %d %q", code, body)
	}
	in.stop("SIGTERM")
}

// A Go image pointed at a database with pending events (a 1.0.x one) must not consume them while
// it has no handlers: the worker is not started.
func TestPendingOutboxEventsSurviveAStartWithoutHandlers(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	startInstance(t, baseConfig(url, key), nil).stop("SIGTERM") // creates the schema

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var id int64
	if err := conn.QueryRow(ctx, `insert into platform.outbox (type, payload) values ('catalog.tools_changed', '{}') returning id`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	in := startInstance(t, baseConfig(url, key), nil)
	if n := len(in.logs.find(t, msgNoOutboxWorker)); n != 1 {
		t.Fatalf("want one record saying the worker is not started, got %d", n)
	}
	if code := in.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var processed bool
	var attempts int
	if err := conn.QueryRow(ctx, `select processed_at is not null, attempts from platform.outbox where id = $1`, id).Scan(&processed, &attempts); err != nil {
		t.Fatal(err)
	}
	if processed || attempts != 0 {
		t.Fatalf("the pending event was touched: processed=%v attempts=%d", processed, attempts)
	}
}

// holdMigrationLock takes the migration advisory lock on its own session, as another server that
// is migrating would; the lock is released by the returned function.
func holdMigrationLock(t *testing.T, url string) func() {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `select pg_advisory_lock(727002)`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = c.Close(ctx) }) }
	t.Cleanup(release)
	return release
}

// waitForLockWaiter blocks until some session on the database waits for the advisory lock.
func waitForLockWaiter(t *testing.T, url string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	deadline := time.Now().Add(waitLimit)
	for {
		var n int
		if err := c.QueryRow(ctx, `select count(*) from pg_locks where locktype = 'advisory' and not granted`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("nobody waits for the migration lock")
		}
		time.Sleep(5 * time.Millisecond) //nolint:forbidigo // polling a server-side lock queue with a deadline
	}
}

// A signal while another server holds the migration lock ends the start: nothing is applied, the
// pool is closed, the port was never opened, the exit code is 0.
func TestSignalWhileWaitingForTheMigrationLock(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	startInstance(t, baseConfig(url, key), nil).stop("SIGTERM") // schema exists
	exec(t, url, "delete from platform.schema_migrations where name = '0010_key_check.sql'")
	exec(t, url, "drop table platform.key_check")
	release := holdMigrationLock(t, url)

	cfg := baseConfig(url, key)
	cfg.Port = freePort(t)
	in := launch(t, cfg, nil)
	waitForLockWaiter(t, url)
	in.cancel(Signal("SIGTERM"))
	if code := in.exit(); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, in.logs.String())
	}
	if got, want := in.logs.messages(t), []string{"starting", "shutting down", "stopped"}; !slices.Equal(got, want) {
		t.Fatalf("records %q, want %q", got, want)
	}
	if sig := in.logs.find(t, "shutting down")[0]["signal"]; sig != "SIGTERM" {
		t.Fatalf("signal %v", sig)
	}
	release()
	eventuallyNoConnections(t, url)
	if n := countRows(t, url, `select count(*) from platform.schema_migrations where name = '0010_key_check.sql'`); n != 0 {
		t.Fatal("a cancelled start applied a migration")
	}
	startInstance(t, baseConfig(url, key), nil).stop("SIGTERM") // the lock is free again
}

// A signal before the database answers ends the start without a failure record.
func TestSignalBeforeTheDatabaseAnswers(t *testing.T) {
	// A listener that never accepts: the connection is made (the backlog takes it) and the
	// start-up handshake never gets an answer.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	cfg := baseConfig("postgres://u@"+silent.Addr().String()+"/db?sslmode=disable", newKey(t))
	in := launch(t, cfg, nil)
	in.waitFor("starting")
	in.cancel(Signal("SIGINT"))
	if code := in.exit(); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, in.logs.String())
	}
	if got, want := in.logs.messages(t), []string{"starting", "shutting down", "stopped"}; !slices.Equal(got, want) {
		t.Fatalf("records %q, want %q", got, want)
	}
}

func countRows(t *testing.T, url, query string) int {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	var n int
	if err := c.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// net/http answers `OPTIONS *` itself with a bare 200 unless told not to; the chain must see it.
func TestOptionsStarGoesThroughTheChain(t *testing.T) {
	in := startInstance(t, baseConfig(pgtest.StartURL(t), newKey(t)), nil)
	conn, err := net.Dial("tcp", strings.TrimPrefix(in.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "OPTIONS * HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.Header.Get("X-Request-Id") == "" || res.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("OPTIONS * bypassed the chain: %d %v", res.StatusCode, res.Header)
	}
}

// A connection string with the Node client's PgBouncer settings starts the server: migrations,
// key check and the HTTP side all work with prepare=false.
func TestStartWithNodeClientOptionsInTheDatabaseURL(t *testing.T) {
	url := pgtest.StartURL(t) + "&prepare=false&max=5&idle_timeout=20"
	cfg := baseConfig(url, newKey(t))
	in := startInstance(t, cfg, nil)
	if code, body, _ := get(t, in.base+"/healthz"); code != 200 || body != "ok" {
		t.Fatalf("/healthz: %d %q", code, body)
	}
	if strings.Contains(in.logs.String(), "prepare=false") {
		t.Fatal("the connection string reached the log")
	}
	if code := in.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
