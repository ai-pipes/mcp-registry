package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/search"
	searchapp "github.com/ai-pipes/mcp-registry/internal/modules/search/app"
	searchinfra "github.com/ai-pipes/mcp-registry/internal/modules/search/infra"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/infra/onnx"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

// fakeIndex keeps documents in memory; the ports it does not implement panic (nil embedded).
type fakeIndex struct {
	searchapp.SearchIndex
	mu         sync.Mutex
	docs       map[string]searchapp.Document
	upsertFail error
}

func newFakeIndex() *fakeIndex { return &fakeIndex{docs: map[string]searchapp.Document{}} }

func (i *fakeIndex) Upsert(_ context.Context, d searchapp.Document) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.upsertFail != nil {
		return i.upsertFail
	}
	i.docs[d.Ref] = d
	return nil
}

// UpsertEmbedFailed stores the document without a vector (the fake has no earlier vector to keep).
func (i *fakeIndex) UpsertEmbedFailed(ctx context.Context, d searchapp.Document) error {
	d.Embedding = nil
	return i.Upsert(ctx, d)
}

func (i *fakeIndex) Remove(_ context.Context, refs []string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, r := range refs {
		delete(i.docs, r)
	}
	return nil
}

func (i *fakeIndex) RefsOfServer(_ context.Context, server string) ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := []string{}
	for ref, d := range i.docs {
		if d.Server == server {
			out = append(out, ref)
		}
	}
	return out, nil
}

func (i *fakeIndex) Servers(context.Context) ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, d := range i.docs {
		if !seen[d.Server] {
			seen[d.Server] = true
			out = append(out, d.Server)
		}
	}
	return out, nil
}

func (i *fakeIndex) Count(context.Context) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.docs), nil
}

func (i *fakeIndex) WithoutEmbedding(context.Context, int) ([]search.Candidate, error) {
	return []search.Candidate{}, nil
}

// fakeTools is the catalog: slug -> tool names.
type fakeTools struct {
	searchapp.ToolSource
	servers map[string][]string
	order   []string
}

func (f fakeTools) ListServerSlugs(context.Context) ([]string, error) { return f.order, nil }

func (f fakeTools) ListToolsForIndex(_ context.Context, slug string) (*catalog.ServerIndexSource, error) {
	names, ok := f.servers[slug]
	if !ok {
		return nil, nil
	}
	src := &catalog.ServerIndexSource{Slug: slug, Name: slug, Description: "the " + slug + " server"}
	for _, n := range names {
		src.Tools = append(src.Tools, catalog.IndexedTool{Ref: slug + "." + n, Name: n, Description: "does " + n, InputSchema: json.RawMessage(`{}`)})
	}
	return src, nil
}

// fakeEmbedder answers a fixed vector, or the error it is given.
type fakeEmbedder struct {
	mu    sync.Mutex
	err   error
	ready bool
	calls int
}

func (e *fakeEmbedder) Ready(context.Context) bool { return e.ready }

func (e *fakeEmbedder) EmbedPassage(context.Context, string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	return make([]float32, searchapp.Dimensions), nil
}

func (e *fakeEmbedder) EmbedQuery(ctx context.Context, q string) ([]float32, error) {
	return e.EmbedPassage(ctx, q)
}

func searchRuntime(buf *logBuf) Runtime {
	cfg := testConfig()
	cfg.SearchMinSimilarity = 0.83
	return Runtime{Config: cfg, Logger: logging.New("info", buf), Clock: kernel.SystemClock{}}
}

func wiringOver(buf *logBuf, idx search.Index, emb embedderHandle, tools search.ToolSource) searchWiring {
	return searchWiringOver(searchRuntime(buf), idx, emb, tools)
}

func TestTheEmbedderFollowsTheConfiguration(t *testing.T) {
	cfg := testConfig()
	cfg.Embedder, cfg.EmbedderURL, cfg.EmbedderModel = "openai-compatible", "http://emb.invalid/v1", "m"
	remote := embedderFor(cfg)
	if _, ok := remote.Embedder.(*searchinfra.OpenAIEmbedder); !ok || remote.load != nil || remote.close != nil {
		t.Errorf("openai-compatible: %T, load %v, close %v", remote.Embedder, remote.load != nil, remote.close != nil)
	}
	cfg.Embedder = "local"
	local := embedderFor(cfg)
	if _, ok := local.Embedder.(*onnx.Embedder); !ok || local.load == nil || local.close == nil {
		t.Errorf("local: %T", local.Embedder)
	}
	if err := local.close(); err != nil { // never loaded: closing is cheap and safe, also twice
		t.Errorf("close of an embedder that never loaded: %v", err)
	}
	if err := local.close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	cfg.Embedder = ""
	if _, ok := embedderFor(cfg).Embedder.(*onnx.Embedder); !ok {
		t.Error("the default embedder is the local one")
	}
}

func TestTheModuleOfTheOpenAICompatibleEmbedderLoadsNothingAndClosesNothing(t *testing.T) {
	cfg := testConfig()
	cfg.Embedder, cfg.EmbedderURL, cfg.EmbedderModel = "openai-compatible", "http://emb.invalid/v1", "m"
	rt := searchRuntime(&logBuf{})
	rt.Config = cfg
	m := searchWiringOver(rt, newFakeIndex(), embedderFor(cfg), fakeTools{}).module()
	if m.BeforeServing != nil || len(m.Closers) != 0 {
		t.Errorf("BeforeServing %v, closers %d", m.BeforeServing != nil, len(m.Closers))
	}
}

func TestAFailedModelLoadIsOneWarningWithAFixedReasonAndStopsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"binary without the embedder", onnx.ErrNotBuilt, "not_built"},
		{"missing model", errors.New("open /nonexistent/model.onnx: no such file or directory secret-path"), "load_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf logBuf
			w := wiringOver(&buf, newFakeIndex(), embedderHandle{Embedder: &fakeEmbedder{}, load: func(context.Context) error { return tc.err }}, fakeTools{})
			w.module().BeforeServing(context.Background())
			recs := buf.find(t, msgEmbedderUnavailable)
			if len(recs) != 1 || len(buf.records(t)) != 1 {
				t.Fatalf("records: %v", buf.messages(t))
			}
			if recs[0]["reason"] != tc.reason || recs[0]["level"] != float64(40) {
				t.Errorf("record %v", recs[0])
			}
			if strings.Contains(buf.String(), "secret-path") || strings.Contains(buf.String(), "nonexistent") {
				t.Errorf("the load error's text reached the log: %s", buf.String())
			}
		})
	}
}

func TestASuccessfulModelLoadLogsNothing(t *testing.T) {
	var buf logBuf
	w := wiringOver(&buf, newFakeIndex(), embedderHandle{Embedder: &fakeEmbedder{}, load: func(context.Context) error { return nil }}, fakeTools{})
	w.module().BeforeServing(context.Background())
	if len(buf.records(t)) != 0 {
		t.Fatalf("records: %v", buf.messages(t))
	}
}

func TestALoadCutShortByTheShutdownLogsNothing(t *testing.T) {
	var buf logBuf
	ctx, cancel := context.WithCancel(context.Background())
	w := wiringOver(&buf, newFakeIndex(), embedderHandle{Embedder: &fakeEmbedder{}, load: func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	}}, fakeTools{})
	w.module().BeforeServing(ctx)
	if len(buf.records(t)) != 0 {
		t.Fatalf("records: %v", buf.messages(t))
	}
}

// The stub embedder of the default build: not ready, Load fails with ErrNotBuilt. The start then
// logs the one record and indexing writes text rows.
func TestTheDefaultBuildLoadsWithTheNotBuiltReason(t *testing.T) {
	if onnx.Built {
		t.Skip("the binary has the local embedder")
	}
	var buf logBuf
	cfg := testConfig()
	rt := searchRuntime(&buf)
	rt.Config = cfg
	w := searchWiringOver(rt, newFakeIndex(), embedderFor(cfg), fakeTools{})
	w.module().BeforeServing(context.Background())
	recs := buf.find(t, msgEmbedderUnavailable)
	if len(recs) != 1 || recs[0]["reason"] != "not_built" {
		t.Fatalf("records: %v", buf.records(t))
	}
}

// The sweep is indexMissingEmbeddings with limit 20; it returns its context's error when the
// worker stops it.
func TestTheSweepUsesTheTypeScriptLimitAndHonoursItsContext(t *testing.T) {
	idx := &limitIndex{fakeIndex: newFakeIndex()}
	w := wiringOver(&logBuf{}, idx, embedderHandle{Embedder: &fakeEmbedder{ready: true}}, fakeTools{})
	if err := w.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(idx.limits) != 1 || idx.limits[0] != 20 {
		t.Fatalf("limits %v, want [20]", idx.limits)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	idx.pending = []search.Candidate{{Ref: "a.x", Server: "a"}}
	if err := w.sweep(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled sweep returned %v, want context.Canceled", err)
	}
	m := w.module()
	if len(m.Sweeps) != 1 || m.Sweeps[0].Every != 5*time.Minute {
		t.Fatalf("sweeps %+v", m.Sweeps)
	}
}

// limitIndex records the limit and serves a fixed list of documents without a vector.
type limitIndex struct {
	*fakeIndex
	pending []search.Candidate
	limits  []int
}

func (i *limitIndex) WithoutEmbedding(_ context.Context, limit int) ([]search.Candidate, error) {
	i.limits = append(i.limits, limit)
	return i.pending, nil
}

func runReindex(t *testing.T, w searchWiring, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := w.reindexCommand()
	code = runCommand(context.Background(), cmd, CLIIO{Stdout: &out, Stderr: &errb}, args, discardLogger(), "USAGE\n")
	return code, out.String(), errb.String()
}

func TestReindexPrintsTheNumberOfToolsInTheIndex(t *testing.T) {
	idx := newFakeIndex()
	// A document of a server the catalog no longer has: reindex drops it.
	idx.docs["ghost.x"] = searchapp.Document{Ref: "ghost.x", Server: "ghost"}
	tools := fakeTools{order: []string{"gitea"}, servers: map[string][]string{"gitea": {"a", "b", "c"}}}
	w := wiringOver(&logBuf{}, idx, embedderHandle{Embedder: &fakeEmbedder{}}, tools)
	code, stdout, stderr := runReindex(t, w)
	if code != 0 || stdout != "reindexed 3 tool(s)\n" || stderr != "" {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
	if _, ghost := idx.docs["ghost.x"]; ghost || len(idx.docs) != 3 {
		t.Errorf("documents %v", idx.docs)
	}
	for _, d := range idx.docs {
		if len(d.Embedding) != searchapp.Dimensions {
			t.Errorf("%s has no vector", d.Ref)
		}
	}
}

func TestReindexOfAnEmptyCatalogPrintsZero(t *testing.T) {
	w := wiringOver(&logBuf{}, newFakeIndex(), embedderHandle{Embedder: &fakeEmbedder{}}, fakeTools{})
	if code, stdout, _ := runReindex(t, w); code != 0 || stdout != "reindexed 0 tool(s)\n" {
		t.Fatalf("%d %q", code, stdout)
	}
}

// What the reference does when the embedder fails: every text row is written, then the command
// fails (there an unhandled rejection, here the neutral unexpected-failure line), exit 1, nothing
// on stdout, and nothing of the error's text anywhere.
func TestReindexWithTheEmbedderDownWritesTextRowsThenFails(t *testing.T) {
	idx := newFakeIndex()
	tools := fakeTools{order: []string{"gitea"}, servers: map[string][]string{"gitea": {"a", "b"}}}
	w := wiringOver(&logBuf{}, idx, embedderHandle{Embedder: &fakeEmbedder{err: errors.New("dial tcp 10.0.0.9: secret-embedder-host")}}, tools)
	code, stdout, stderr := runReindex(t, w)
	if code != 1 || stdout != "" || stderr != "error: unexpected failure\n" {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
	if len(idx.docs) != 2 {
		t.Fatalf("text rows: %v", idx.docs)
	}
	for _, d := range idx.docs {
		if d.Embedding != nil {
			t.Errorf("%s has a vector", d.Ref)
		}
	}
}

func TestReindexWithTheLocalEmbedderNotBuiltBehavesLikeAMissingModel(t *testing.T) {
	if onnx.Built {
		t.Skip("the binary has the local embedder")
	}
	cfg := testConfig()
	idx := newFakeIndex()
	tools := fakeTools{order: []string{"gitea"}, servers: map[string][]string{"gitea": {"a"}}}
	rt := searchRuntime(&logBuf{})
	rt.Config = cfg
	w := searchWiringOver(rt, idx, embedderFor(cfg), tools)
	code, stdout, stderr := runReindex(t, w)
	if code != 1 || stdout != "" || stderr != "error: unexpected failure\n" || len(idx.docs) != 1 {
		t.Fatalf("%d %q %q, docs %d", code, stdout, stderr, len(idx.docs))
	}
}

func TestReindexTakesNoArguments(t *testing.T) {
	w := wiringOver(&logBuf{}, newFakeIndex(), embedderHandle{Embedder: &fakeEmbedder{}}, fakeTools{})
	code, stdout, stderr := runReindex(t, w, "now")
	if code != 2 || stdout != "" || stderr != "error: expected 0 argument(s), got 1\nUSAGE\n" {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
	code, _, stderr = runReindex(t, w, "--all")
	if code != 2 || !strings.HasPrefix(stderr, "error: Unknown option '--all'") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestReindexInterruptedBySignalSaysSoAndExitsOne(t *testing.T) {
	idx := newFakeIndex()
	ctx, cancel := context.WithCancel(context.Background())
	emb := &cancellingEmbedder{cancel: cancel}
	tools := fakeTools{order: []string{"gitea"}, servers: map[string][]string{"gitea": {"a", "b"}}}
	w := wiringOver(&logBuf{}, idx, embedderHandle{Embedder: emb}, tools)
	var out, errb bytes.Buffer
	code := runCommand(ctx, w.reindexCommand(), CLIIO{Stdout: &out, Stderr: &errb}, nil, discardLogger(), "USAGE\n")
	if code != 1 || out.String() != "" || errb.String() != "error: "+msgInterrupted+"\n" {
		t.Fatalf("%d %q %q", code, out.String(), errb.String())
	}
}

type cancellingEmbedder struct{ cancel context.CancelFunc }

func (e *cancellingEmbedder) Ready(context.Context) bool { return true }
func (e *cancellingEmbedder) EmbedPassage(ctx context.Context, _ string) ([]float32, error) {
	e.cancel()
	return nil, ctx.Err()
}

func (e *cancellingEmbedder) EmbedQuery(ctx context.Context, q string) ([]float32, error) {
	return e.EmbedPassage(ctx, q)
}

func TestTheReindexCommandUsesTheWarnGuardAndItsOwnUsage(t *testing.T) {
	c := searchWiring{}.reindexCommand()
	if c.Guard != GuardWarn || !strings.HasPrefix(c.Usage, "  reindex\n") {
		t.Fatalf("%+v", c)
	}
}

func TestTheSearchWiringRefusesToBeBuiltWithoutItsParts(t *testing.T) {
	for name, build := range map[string]func(){
		"index": func() {
			searchWiringOver(searchRuntime(&logBuf{}), nil, embedderHandle{Embedder: &fakeEmbedder{}}, fakeTools{})
		},
		"embedder": func() { searchWiringOver(searchRuntime(&logBuf{}), newFakeIndex(), embedderHandle{}, fakeTools{}) },
		"tools": func() {
			searchWiringOver(searchRuntime(&logBuf{}), newFakeIndex(), embedderHandle{Embedder: &fakeEmbedder{}}, nil)
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("no panic without the %s", name)
				}
			}()
			build()
		}()
	}
}

func TestAModelLoadThatNeverFinishesIsBoundedAndTheStartGoesOn(t *testing.T) {
	var buf logBuf
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w := wiringOver(&buf, newFakeIndex(), embedderHandle{Embedder: &fakeEmbedder{}, load: func(context.Context) error {
		<-release // a loader that never returns on its own and ignores its context
		return nil
	}}, fakeTools{})
	w.loadBound = 20 * time.Millisecond
	returned := make(chan struct{})
	go func() { w.module().BeforeServing(context.Background()); close(returned) }()
	recv(t, returned, "BeforeServing to give up waiting")
	recs := buf.find(t, msgEmbedderUnavailable)
	if len(recs) != 1 || len(buf.records(t)) != 1 || recs[0]["reason"] != "load_timeout" || recs[0]["level"] != float64(40) {
		t.Fatalf("records: %v", buf.records(t))
	}
}

func TestTheDefaultLoadBoundIsTwoMinutes(t *testing.T) {
	if w := wiringOver(&logBuf{}, newFakeIndex(), embedderHandle{Embedder: &fakeEmbedder{}}, fakeTools{}); w.loadBound != 2*time.Minute {
		t.Fatalf("bound %s", w.loadBound)
	}
}

func TestTheEmbedderCloserReturnsAtTheEndOfItsContextWhileCloseIsStuck(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w := wiringOver(&logBuf{}, newFakeIndex(), embedderHandle{
		Embedder: &fakeEmbedder{}, load: func(context.Context) error { return nil },
		close: func() error { <-release; return nil },
	}, fakeTools{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got := make(chan error, 1)
	go func() { got <- w.module().Closers[0](ctx) }()
	if err := recv(t, got, "the closer to return"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closer returned %v, want the context's error", err)
	}
}

func TestTheEmbedderCloserReturnsWhatCloseReturns(t *testing.T) {
	bad := errors.New("close failed")
	for _, want := range []error{nil, bad} {
		w := wiringOver(&logBuf{}, newFakeIndex(), embedderHandle{
			Embedder: &fakeEmbedder{}, load: func(context.Context) error { return nil },
			close: func() error { return want },
		}, fakeTools{})
		if err := w.module().Closers[0](context.Background()); !errors.Is(err, want) && err != want {
			t.Errorf("got %v, want %v", err, want)
		}
	}
}

// SEARCH_MIN_SIMILARITY reaches the vector query through the wiring.
func TestTheSimilarityFloorOfTheConfigurationReachesTheVectorQuery(t *testing.T) {
	for _, floor := range []float64{0.83, 0.42} {
		idx := &floorIndex{fakeIndex: newFakeIndex()}
		rt := searchRuntime(&logBuf{})
		rt.Config.SearchMinSimilarity = floor
		w := searchWiringOver(rt, idx, embedderHandle{Embedder: &fakeEmbedder{ready: true}}, fakeTools{})
		if _, err := w.service.SearchTools(context.Background(), "create issue", 5, []string{"gitea"}); err != nil {
			t.Fatal(err)
		}
		if len(idx.floors) != 1 || idx.floors[0] != floor {
			t.Fatalf("floor %v reached the query as %v", floor, idx.floors)
		}
	}
}

type floorIndex struct {
	*fakeIndex
	floors []float64
}

func (i *floorIndex) FTSCandidates(context.Context, string, int, []string) ([]search.Candidate, error) {
	return []search.Candidate{}, nil
}

func (i *floorIndex) VectorCandidates(_ context.Context, _ []float32, _ int, floor float64, _ []string) ([]search.Candidate, error) {
	i.floors = append(i.floors, floor)
	return []search.Candidate{}, nil
}

// recv receives from ch or fails the test after a generous limit.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}
