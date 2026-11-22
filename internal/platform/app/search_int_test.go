//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	idapp "github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	searchinfra "github.com/ai-pipes/mcp-registry/internal/modules/search/infra"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/infra/onnx"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5"
)

// fakeEmbeddingServer is an OpenAI-compatible /v1/embeddings server. The vector of a text is a
// function of it, so a stored vector says which passage it was made from.
type fakeEmbeddingServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	down     bool
	hold     bool          // requests wait until the client goes away
	entered  chan struct{} // one signal per request that started waiting
	inputs   []string
	requests int
}

func newFakeEmbeddingServer(t *testing.T) *fakeEmbeddingServer {
	t.Helper()
	f := &fakeEmbeddingServer{entered: make(chan struct{}, 64)}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEmbeddingServer) setDown(v bool) { f.mu.Lock(); f.down = v; f.mu.Unlock() }
func (f *fakeEmbeddingServer) setHold(v bool) { f.mu.Lock(); f.hold = v; f.mu.Unlock() }

func (f *fakeEmbeddingServer) seen() (inputs []string, requests int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.inputs), f.requests
}

// vectorOf is the fake's answer for a text: two numbers that depend on it, the rest zero.
func vectorOf(text string) []float32 {
	sum := 0
	for _, b := range []byte(text) {
		sum += int(b)
	}
	v := make([]float32, 384)
	v[0] = float32(len(text)%997+1) / 1000
	v[1] = float32(sum%991+1) / 1000
	return v
}

// literalOf is how Postgres prints a stored vector: what `embedding::text` returns.
func literalOf(v []float32) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(float64(x), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (f *fakeEmbeddingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var in struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	_ = json.Unmarshal(body, &in)
	f.mu.Lock()
	f.requests++
	f.inputs = append(f.inputs, in.Input...)
	down, hold := f.down, f.hold
	f.mu.Unlock()
	if down {
		http.Error(w, `{"error":"the embedder is down"}`, http.StatusServiceUnavailable)
		return
	}
	if hold {
		f.entered <- struct{}{}
		<-r.Context().Done()
		return
	}
	data := make([]map[string]any, len(in.Input))
	for i, text := range in.Input {
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": vectorOf(text)}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": in.Model, "data": data})
}

// remoteEmbedder points the configuration at the fake.
func remoteEmbedder(f *fakeEmbeddingServer) func(*config.Config) {
	return func(c *config.Config) {
		catalogConfig(c)
		c.Embedder, c.EmbedderURL, c.EmbedderModel = "openai-compatible", f.srv.URL+"/v1", "compat-model"
		c.SearchMinSimilarity = 0.1
	}
}

// waitCount polls the database until the count query returns want.
func waitCount(t *testing.T, url string, want int, sql string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	got := -1
	for time.Now().Before(deadline) {
		if got = queryInt(t, url, sql, args...); got == want {
			return
		}
		<-time.After(20 * time.Millisecond) // polling a database row: there is nothing to wait on
	}
	t.Fatalf("%s: got %d after %s, want %d", sql, got, waitLimit, want)
}

const (
	docsOf       = `select count(*)::int from search.documents where server = $1`
	vectorsOf    = `select count(embedding)::int from search.documents where server = $1`
	unprocessed  = `select count(*)::int from platform.outbox where processed_at is null`
	embeddingsOf = `select coalesce(string_agg(embedding::text, '|' order by ref), '') from search.documents where server = $1`
)

func registerFiles(t *testing.T, admin person, up *mcpFake) {
	t.Helper()
	if status, _, raw := admin.do("POST", "/api/servers", serverBody("files", up.url(), map[string]any{"kind": "none"})); status != 201 {
		t.Fatalf("POST /api/servers: %d %s", status, raw)
	}
}

// The index follows the catalog through the outbox: a registered server leads to documents with
// the vectors of the embedder, and its events are processed.
func TestRegisteringAServerIndexesItWithTheEmbeddersVectors(t *testing.T) {
	url := pgtest.StartURL(t)
	emb := newFakeEmbeddingServer(t)
	rs := startReal(t, url, newKey(t), remoteEmbedder(emb))
	admin := person{rs, rs.person("alice", idapp.RoleAdmin)}
	registerFiles(t, admin, newMCPFake(t, "alpha", "beta"))

	waitCount(t, url, 2, vectorsOf, "files")
	waitCount(t, url, 0, unprocessed)

	inputs, _ := emb.seen()
	want := map[string]bool{}
	for _, in := range inputs {
		if !strings.HasPrefix(in, "passage: ") {
			t.Errorf("an input without the passage prefix: %q", in)
		}
		want[literalOf(vectorOf(in))] = true
	}
	for _, stored := range strings.Split(queryText(t, url, embeddingsOf, "files"), "|") {
		if !want[stored] {
			t.Errorf("a stored vector is not one the embedder returned: %.60s", stored)
		}
	}
	if recs := rs.logs.find(t, msgEmbedderUnavailable); len(recs) != 0 {
		t.Errorf("the remote embedder needs no loading, yet: %v", recs)
	}
	if len(rs.logs.find(t, msgNoOutboxWorker)) != 0 {
		t.Error("the worker was reported as not started")
	}
	if code := rs.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d\n%s", code, rs.logs.String())
	}
}

func queryText(t *testing.T, url, sql string, args ...any) string {
	t.Helper()
	e := &cliEnv{t: t, url: url}
	return e.text(sql, args...)
}

// With the embedder down the text rows are written, the events stay (to be retried), and the
// sweep fills the vectors at the next start, once the embedder is back.
func TestAnEmbedderOutageLeavesTextRowsAndTheSweepFillsTheVectors(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	emb := newFakeEmbeddingServer(t)
	emb.setDown(true)
	rs := startReal(t, url, key, remoteEmbedder(emb))
	admin := person{rs, rs.person("alice", idapp.RoleAdmin)}
	registerFiles(t, admin, newMCPFake(t, "alpha", "beta"))

	waitCount(t, url, 2, docsOf, "files")
	if err := rs.waitErr("outbox: handler failed, will retry"); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, url, vectorsOf, "files"); n != 0 {
		t.Fatalf("%d vectors although the embedder was down", n)
	}
	if n := queryInt(t, url, unprocessed); n == 0 {
		t.Fatal("the events were processed although the embedder was down")
	}
	if code := rs.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}

	emb.setDown(false)
	again := startReal(t, url, key, remoteEmbedder(emb))
	waitCount(t, url, 2, vectorsOf, "files") // the sweep of the worker's first pass
	if len(again.logs.find(t, "outbox: sweep failed")) != 0 {
		t.Errorf("the sweep failed: %s", again.logs.String())
	}
	if code := again.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// Decided deviation vector-kept-on-failed-reembedding. A server change while the embedder is down
// rewrites the text of every document but keeps the vectors that exist and marks the rows; once
// the embedder is back the sweep of the next worker pass refreshes them, with no reindex. The
// outbox retries are taken out of the picture (the events are marked processed, as when the ten
// attempts were exhausted) and the mark is aged past the sweep's one-hour back-off.
func TestAFailedReembeddingKeepsTheVectorsAndTheSweepRefreshesThem(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	emb := newFakeEmbeddingServer(t)
	rs := startReal(t, url, key, remoteEmbedder(emb))
	admin := person{rs, rs.person("alice", idapp.RoleAdmin)}
	up := newMCPFake(t, "alpha", "beta")
	registerFiles(t, admin, up)
	waitCount(t, url, 2, vectorsOf, "files")
	waitCount(t, url, 0, unprocessed)
	before := queryText(t, url, embeddingsOf, "files")

	emb.setDown(true)
	body := serverBody("files", up.url(), map[string]any{"kind": "none"})
	body["description"] = "changed while the embedder is down"
	if status, _, raw := admin.do("PUT", "/api/servers/files", body); status != 200 {
		t.Fatalf("PUT: %d %s", status, raw)
	}
	const marked = `select count(*)::int from search.documents where server = $1 and embed_failed_at is not null`
	waitCount(t, url, 2, marked, "files")
	if n := queryInt(t, url, vectorsOf, "files"); n != 2 {
		t.Fatalf("%d vectors left after the failed re-embedding, want 2", n)
	}
	if after := queryText(t, url, embeddingsOf, "files"); after != before {
		t.Fatal("the failed re-embedding changed the stored vectors")
	}
	if code := rs.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}

	if _, err := execSQL(url, `update platform.outbox set processed_at = now() where processed_at is null`); err != nil {
		t.Fatal(err)
	}
	if _, err := execSQL(url, `update search.documents set embed_failed_at = now() - interval '2 hours' where server = 'files'`); err != nil {
		t.Fatal(err)
	}
	emb.setDown(false)
	again := startReal(t, url, key, remoteEmbedder(emb))
	waitCount(t, url, 0, marked, "files")
	if after := queryText(t, url, embeddingsOf, "files"); after == before || queryInt(t, url, vectorsOf, "files") != 2 {
		t.Error("the sweep did not refresh the vectors")
	}
	if code := again.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// Hiding a tool, unhiding it, disabling a server and enabling it again reach the index.
func TestHidingAndDisablingReachTheIndex(t *testing.T) {
	url := pgtest.StartURL(t)
	emb := newFakeEmbeddingServer(t)
	rs := startReal(t, url, newKey(t), remoteEmbedder(emb))
	admin := person{rs, rs.person("alice", idapp.RoleAdmin)}
	registerFiles(t, admin, newMCPFake(t, "alpha", "beta"))
	waitCount(t, url, 2, vectorsOf, "files")

	step := func(what, method, path string, body any, docs int) {
		t.Helper()
		if status, _, raw := admin.do(method, path, body); status != 204 {
			t.Fatalf("%s: %d %s", what, status, raw)
		}
		waitCount(t, url, docs, vectorsOf, "files")
		waitCount(t, url, docs, docsOf, "files")
	}
	step("hide", "PUT", "/api/servers/files/tools/alpha/hidden", map[string]any{"hidden": true}, 1)
	if n := queryInt(t, url, `select count(*)::int from search.documents where ref = 'files.alpha'`); n != 0 {
		t.Fatal("the hidden tool is still indexed")
	}
	step("unhide", "PUT", "/api/servers/files/tools/alpha/hidden", map[string]any{"hidden": false}, 2)
	step("disable", "POST", "/api/servers/files/status", map[string]any{"status": "disabled"}, 0)
	step("enable", "POST", "/api/servers/files/status", map[string]any{"status": "active"}, 2)
	if code := rs.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// Events written before the search module existed (or while no worker ran) are delivered at the
// first start that has the handlers.
func TestABacklogOfCatalogEventsIsIndexedAtTheFirstStartWithSearch(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	emb := newFakeEmbeddingServer(t)

	// A server without the search module: the events pile up in the outbox.
	without := startRealWith(t, url, key, remoteEmbedder(emb), func(rt Runtime) []Module { return modules(rt)[:3] })
	admin := person{without, without.person("alice", idapp.RoleAdmin)}
	registerFiles(t, admin, newMCPFake(t, "alpha", "beta"))
	if len(without.logs.find(t, msgNoOutboxWorker)) != 1 {
		t.Fatalf("no worker was expected\n%s", without.logs.String())
	}
	if code := without.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if queryInt(t, url, docsOf, "files") != 0 || queryInt(t, url, unprocessed) == 0 {
		t.Fatal("the backlog is missing or was consumed")
	}

	with := startReal(t, url, key, remoteEmbedder(emb))
	waitCount(t, url, 2, vectorsOf, "files")
	waitCount(t, url, 0, unprocessed)
	if code := with.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// The default build has no local embedder: the start is healthy, logs the one record, and the
// index gets text rows.
func TestTheDefaultBuildStartsHealthyAndIndexesTextOnly(t *testing.T) {
	if onnx.Built {
		t.Skip("the binary has the local embedder")
	}
	url := pgtest.StartURL(t)
	rs := startReal(t, url, newKey(t), catalogConfig) // EMBEDDER is unset: the local one
	recs := rs.logs.find(t, msgEmbedderUnavailable)
	if len(recs) != 1 || recs[0]["reason"] != "not_built" || recs[0]["level"] != float64(levelWarn) {
		t.Fatalf("records: %v", recs)
	}
	// The record comes before `listening`: the load is part of the start.
	msgs := rs.logs.messages(t)
	if slices.Index(msgs, msgEmbedderUnavailable) > slices.Index(msgs, "listening") {
		t.Fatalf("the embedder record came after listening: %q", msgs)
	}
	if code, body, _ := get(t, rs.base+"/healthz"); code != 200 || body != "ok" {
		t.Fatalf("/healthz: %d %q", code, body)
	}
	admin := person{rs, rs.person("alice", idapp.RoleAdmin)}
	registerFiles(t, admin, newMCPFake(t, "alpha", "beta"))
	waitCount(t, url, 2, docsOf, "files")
	if n := queryInt(t, url, vectorsOf, "files"); n != 0 {
		t.Fatalf("%d vectors without an embedder", n)
	}
	if code := rs.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// probeEmbedder gives the lifecycle something to watch: it can hold a run, and it records whether
// a run was in flight when it was closed and whether it was used afterwards.
type probeEmbedder struct {
	mu          sync.Mutex
	block       bool
	inFlight    int
	closed      int
	flightAtEnd int
	usedClosed  bool
	entered     chan struct{}
}

func (p *probeEmbedder) Ready(context.Context) bool { return true }

func (p *probeEmbedder) EmbedPassage(ctx context.Context, text string) ([]float32, error) {
	p.mu.Lock()
	if p.closed > 0 {
		p.usedClosed = true
		p.mu.Unlock()
		return nil, fmt.Errorf("closed")
	}
	p.inFlight++
	block := p.block
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.inFlight--; p.mu.Unlock() }()
	if block {
		p.entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return vectorOf(text), nil
}

func (p *probeEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return p.EmbedPassage(ctx, text)
}

func (p *probeEmbedder) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed++
	p.flightAtEnd = p.inFlight
	return nil
}

// A shutdown with an indexing run in flight stops the worker first and closes the embedder after
// it, and the vectors that exist stay.
func TestShutdownWithAnIndexingRunInFlightKeepsTheVectorsAndClosesTheEmbedderLast(t *testing.T) {
	url := pgtest.StartURL(t)
	probe := &probeEmbedder{entered: make(chan struct{}, 8)}
	build := func(rt Runtime) []Module {
		id := newIdentityWiring(rt)
		cat := newCatalogWiring(rt, id)
		w := searchWiringOver(rt, searchinfra.NewPgIndex(rt.Pool), embedderHandle{Embedder: probe, close: probe.close}, cat.service)
		return []Module{id.module(), platformModule(rt, id), cat.module(), w.module()}
	}
	rs := startRealWith(t, url, newKey(t), catalogConfig, build)
	admin := person{rs, rs.person("alice", idapp.RoleAdmin)}
	up := newMCPFake(t, "alpha", "beta")
	registerFiles(t, admin, up)
	waitCount(t, url, 2, vectorsOf, "files")
	waitCount(t, url, 0, unprocessed)
	before := queryText(t, url, embeddingsOf, "files")

	// A change of the server re-embeds every tool: hold the first run.
	probe.mu.Lock()
	probe.block = true
	probe.mu.Unlock()
	body := serverBody("files", up.url(), map[string]any{"kind": "none"})
	body["description"] = "changed while the embedder is slow"
	if status, _, raw := admin.do("PUT", "/api/servers/files", body); status != 200 {
		t.Fatalf("PUT: %d %s", status, raw)
	}
	await(t, probe.entered, "the indexing run to reach the embedder")

	if code := rs.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d\n%s", code, rs.logs.String())
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.closed != 1 || probe.flightAtEnd != 0 || probe.usedClosed {
		t.Errorf("closed %d times, %d runs in flight at the close, used after the close %v", probe.closed, probe.flightAtEnd, probe.usedClosed)
	}
	if after := queryText(t, url, embeddingsOf, "files"); after != before || queryInt(t, url, vectorsOf, "files") != 2 {
		t.Error("the shutdown changed the vectors that existed")
	}
	if queryInt(t, url, unprocessed) == 0 {
		t.Error("the interrupted event was marked processed: its run never finished")
	}
}

// reindex, through the entry point of the binary, against a database with registered servers.
func TestReindexCommand(t *testing.T) {
	s := newServerEnv(t)
	emb := newFakeEmbeddingServer(t)
	remoteEmbedder(emb)(&s.cfg)
	s.up = newMCPFake(t, "echo", "ping")
	r := s.create(cliSecret+"\n", nil, "--json", s.file("a.json", nil), "--as", "owner")
	if r.code != 0 {
		t.Fatalf("create-server: %+v", r)
	}
	// create-server does not run the worker: nothing is indexed yet.
	if s.count(docsOf, "fake") != 0 {
		t.Fatal("documents before reindex")
	}

	t.Run("fills the index with vectors and drops documents of vanished servers", func(t *testing.T) {
		s.t = t
		if _, err := execSQL(s.url, `insert into search.documents (ref, server, description, tsv) values ('ghost.x', 'ghost', 'd', to_tsvector('d'))`); err != nil {
			t.Fatal(err)
		}
		r := s.run("reindex")
		if r.code != 0 || r.stdout != "reindexed 2 tool(s)\n" || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
		if s.count(docsOf, "fake") != 2 || s.count(vectorsOf, "fake") != 2 || s.count(docsOf, "ghost") != 0 {
			t.Fatal("the index is not what the catalog says")
		}
		if again := s.run("reindex"); again.code != 0 || again.stdout != "reindexed 2 tool(s)\n" || again.stderr != "" {
			t.Fatalf("a repeat: %+v", again)
		}
	})

	t.Run("with the embedder down: text rows are written, then it fails", func(t *testing.T) {
		s.t = t
		if _, err := execSQL(s.url, `delete from search.documents`); err != nil {
			t.Fatal(err)
		}
		emb.setDown(true)
		r := s.run("reindex")
		if r.code != 1 || r.stdout != "" || !failedUnexpectedly(r.stderr) {
			t.Fatalf("%+v", r)
		}
		if s.count(docsOf, "fake") != 2 || s.count(vectorsOf, "fake") != 0 {
			t.Fatalf("rows: %d, vectors %d", s.count(docsOf, "fake"), s.count(vectorsOf, "fake"))
		}
		if strings.Contains(r.stdout+r.stderr, emb.srv.URL) {
			t.Error("the embedder's address was printed")
		}
		emb.setDown(false)
		if r := s.run("reindex"); r.code != 0 || r.stdout != "reindexed 2 tool(s)\n" || s.count(vectorsOf, "fake") != 2 {
			t.Fatalf("after the embedder is back: %+v", r)
		}
	})

	t.Run("takes no arguments", func(t *testing.T) {
		s.t = t
		r := s.run("reindex", "now")
		if r.code != 2 || !strings.HasPrefix(r.stderr, "error: expected 0 argument(s), got 1\nUsage:") {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("an empty catalog prints zero", func(t *testing.T) {
		e := newCLIEnv(t)
		remoteEmbedder(emb)(&e.cfg)
		if r := e.run("reindex"); r.code != 0 || r.stdout != "reindexed 0 tool(s)\n" || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
	})
}

// With the local embedder not built into the binary, reindex fails like a missing model does in
// the reference: the text rows are written, the command exits 1.
func TestReindexWithTheLocalEmbedderNotBuilt(t *testing.T) {
	if onnx.Built {
		t.Skip("the binary has the local embedder")
	}
	s := newServerEnv(t)
	r := s.create(cliSecret+"\n", nil, "--json", s.file("a.json", nil), "--as", "owner")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	got := s.run("reindex")
	if got.code != 1 || got.stdout != "" || !failedUnexpectedly(got.stderr) || s.count(docsOf, "fake") != 1 {
		t.Fatalf("%+v, rows %d", got, s.count(docsOf, "fake"))
	}
}

// execSQL runs one statement on its own connection.
func execSQL(url, sql string, args ...any) (int64, error) {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close(ctx) }()
	tag, err := c.Exec(ctx, sql, args...)
	return tag.RowsAffected(), err
}

// failedUnexpectedly is the CLI's answer to an error it does not know: one error record with the
// error's type (stderr carries the logs of the CLI at warn and above), then the neutral line.
func failedUnexpectedly(stderr string) bool {
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	return len(lines) == 2 && strings.Contains(lines[0], `"msg":"command failed"`) && lines[1] == "error: unexpected failure"
}
