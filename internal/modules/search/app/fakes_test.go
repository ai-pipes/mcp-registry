package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

var ctx = context.Background()

// journal records the order of calls across all fake ports.
type journal struct {
	mu    sync.Mutex
	calls []string
}

func (j *journal) add(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, fmt.Sprintf(format, args...))
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.calls)
}

func (j *journal) count(prefix string) int {
	n := 0
	for _, c := range j.all() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// memIndex is an in-memory SearchIndex. Every error field makes that operation fail.
type memIndex struct {
	j  *journal
	mu sync.Mutex

	docs   map[string]app.Document
	failed map[string]bool
	// servers argument of every candidate query.
	ftsServers, vecServers [][]string
	// the floor and limit the vector query received
	lastFloor float64
	lastLimit int

	// onFTS runs at the start of FTSCandidates; panicFTS makes it panic.
	onFTS    func()
	panicFTS bool

	errUpsert, errRemove, errRefs, errServers, errCount, errWithout, errMark, errFTS, errVec error
}

func newIndex(j *journal) *memIndex {
	return &memIndex{j: j, docs: map[string]app.Document{}, failed: map[string]bool{}}
}

func (m *memIndex) Upsert(_ context.Context, doc app.Document) error {
	m.j.add("upsert %s", doc.Ref)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errUpsert != nil {
		return m.errUpsert
	}
	m.docs[doc.Ref] = doc
	if doc.Embedding != nil {
		delete(m.failed, doc.Ref)
	}
	return nil
}

func (m *memIndex) Remove(_ context.Context, refs []string) error {
	m.j.add("remove %s", strings.Join(refs, ","))
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errRemove != nil {
		return m.errRemove
	}
	for _, r := range refs {
		delete(m.docs, r)
	}
	return nil
}

func (m *memIndex) RefsOfServer(_ context.Context, server string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errRefs != nil {
		return nil, m.errRefs
	}
	var out []string
	for _, d := range m.docs {
		if d.Server == server {
			out = append(out, d.Ref)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *memIndex) Servers(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errServers != nil {
		return nil, m.errServers
	}
	set := map[string]bool{}
	for _, d := range m.docs {
		set[d.Server] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func (m *memIndex) Count(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.docs), m.errCount
}

func (m *memIndex) WithoutEmbedding(_ context.Context, limit int) ([]domain.Candidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errWithout != nil {
		return nil, m.errWithout
	}
	var out []domain.Candidate
	for _, d := range m.docs {
		if d.Embedding == nil && !m.failed[d.Ref] {
			out = append(out, domain.Candidate{Ref: d.Ref, Server: d.Server})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memIndex) MarkEmbedFailed(_ context.Context, ref string) error {
	m.j.add("mark %s", ref)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errMark != nil {
		return m.errMark
	}
	m.failed[ref] = true
	return nil
}

func (m *memIndex) FTSCandidates(_ context.Context, query string, limit int, servers []string) ([]domain.Candidate, error) {
	m.j.add("fts")
	if m.onFTS != nil {
		m.onFTS()
	}
	if m.panicFTS {
		panic("fts exploded on: " + query)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ftsServers = append(m.ftsServers, servers)
	if m.errFTS != nil {
		return nil, m.errFTS
	}
	words := strings.Fields(strings.ToLower(query))
	var out []domain.Candidate
	for _, d := range m.docs {
		if !slices.Contains(servers, d.Server) {
			continue
		}
		title := ""
		if d.Title != nil {
			title = *d.Title
		}
		text := strings.ToLower(d.Ref + " " + title + " " + d.Description + " " + d.Body)
		all := true
		for _, w := range words {
			all = all && strings.Contains(text, w)
		}
		if all {
			out = append(out, domain.Candidate{Ref: d.Ref, Server: d.Server})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

func (m *memIndex) VectorCandidates(_ context.Context, emb []float32, limit int, floor float64, servers []string) ([]domain.Candidate, error) {
	m.j.add("vector")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vecServers = append(m.vecServers, servers)
	m.lastFloor, m.lastLimit = floor, limit
	if m.errVec != nil {
		return nil, m.errVec
	}
	var docs []app.Document
	for _, d := range m.docs {
		if d.Embedding != nil && slices.Contains(servers, d.Server) && dot(d.Embedding, emb) >= floor {
			docs = append(docs, d)
		}
	}
	sort.Slice(docs, func(i, j int) bool {
		di, dj := dot(docs[i].Embedding, emb), dot(docs[j].Embedding, emb)
		if di != dj {
			return di > dj
		}
		return docs[i].Ref < docs[j].Ref
	})
	if len(docs) > limit {
		docs = docs[:limit]
	}
	out := make([]domain.Candidate, 0, len(docs))
	for _, d := range docs {
		out = append(out, domain.Candidate{Ref: d.Ref, Server: d.Server})
	}
	return out, nil
}

// fakeEmbedder maps a text to a normalised bag-of-words vector, so related texts are close. It
// is scripted: not ready, or failing on chosen calls.
type fakeEmbedder struct {
	j  *journal
	mu sync.Mutex

	notReady bool
	// fail decides, from the 1-based number of the passage call and its text, whether it fails.
	fail      func(n int, text string) error
	failQuery error
	short     bool // answer vectors of the wrong length
	// onQuery runs at the start of EmbedQuery; panicQuery makes it panic.
	onQuery    func()
	panicQuery bool

	passages []string
	queries  []string
	readyN   int
}

var errEmbedder = errors.New("embedder unavailable")

func (e *fakeEmbedder) Ready(context.Context) bool {
	e.j.add("ready")
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readyN++
	return !e.notReady
}

func (e *fakeEmbedder) EmbedPassage(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	e.passages = append(e.passages, text)
	n := len(e.passages)
	fail, short := e.fail, e.short
	e.mu.Unlock()
	e.j.add("embed %d", n)
	if fail != nil {
		if err := fail(n, text); err != nil {
			return nil, err
		}
	}
	if short {
		return make([]float32, 3), nil
	}
	return vectorOf(text), nil
}

func (e *fakeEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	e.queries = append(e.queries, text)
	err := e.failQuery
	e.mu.Unlock()
	e.j.add("embedq")
	if e.onQuery != nil {
		e.onQuery()
	}
	if e.panicQuery {
		panic("embedder exploded on: " + text)
	}
	if err != nil {
		return nil, err
	}
	return vectorOf(text), nil
}

func vectorOf(text string) []float32 {
	v := make([]float32, app.Dimensions)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		v[h.Sum32()%app.Dimensions]++
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / math.Sqrt(norm))
	}
	return v
}

// memCatalog is a ToolSource over a map of servers.
type memCatalog struct {
	j       *journal
	mu      sync.Mutex
	servers map[string]*catalog.ServerIndexSource
	order   []string
	errFor  error
	errList error
}

func newCatalog(j *journal) *memCatalog {
	return &memCatalog{j: j, servers: map[string]*catalog.ServerIndexSource{}}
}

func (c *memCatalog) ListToolsForIndex(_ context.Context, slug string) (*catalog.ServerIndexSource, error) {
	c.j.add("catalog %s", slug)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.errFor != nil {
		return nil, c.errFor
	}
	return c.servers[slug], nil
}

func (c *memCatalog) ListServerSlugs(context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.order), c.errList
}

const defaultSchema = `{"type":"object","properties":{"query":{"description":"search words"}}}`

// put (re)defines a server; tools maps name to description, in sorted name order.
func (c *memCatalog) put(slug string, tools map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	src := &catalog.ServerIndexSource{Slug: slug, Name: slug, Description: slug + " server", Tools: []catalog.IndexedTool{}}
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		src.Tools = append(src.Tools, catalog.IndexedTool{Ref: slug + "." + n, Name: n, Description: tools[n], InputSchema: json.RawMessage(defaultSchema)})
	}
	if _, ok := c.servers[slug]; !ok {
		c.order = append(c.order, slug)
	}
	c.servers[slug] = src
}

func (c *memCatalog) hide(slug, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.servers[slug]
	s.Tools = slices.DeleteFunc(s.Tools, func(t catalog.IndexedTool) bool { return t.Name == name })
}

func (c *memCatalog) tool(slug, name string) *catalog.IndexedTool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.servers[slug].Tools {
		if c.servers[slug].Tools[i].Name == name {
			return &c.servers[slug].Tools[i]
		}
	}
	return nil
}

type recordedLog struct {
	mu   sync.Mutex
	logs []logRecord
}

type logRecord struct {
	msg  string
	args []any
}

func (l *recordedLog) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logs = append(l.logs, logRecord{msg, args})
}

// stepClock advances by step on every reading.
type stepClock struct {
	t    time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.t = c.t.Add(c.step)
	return c.t
}

type world struct {
	j   *journal
	idx *memIndex
	emb *fakeEmbedder
	cat *memCatalog
	log *recordedLog
	d   app.Deps
}

func newWorld(t *testing.T) *world {
	t.Helper()
	j := &journal{}
	w := &world{j: j, idx: newIndex(j), emb: &fakeEmbedder{j: j}, cat: newCatalog(j), log: &recordedLog{}}
	w.d = app.Deps{Index: w.idx, Embedder: w.emb, Tools: w.cat, Logger: w.log, MinSimilarity: 0.5}
	return w
}

func (w *world) refs() []string {
	w.idx.mu.Lock()
	defer w.idx.mu.Unlock()
	out := make([]string, 0, len(w.idx.docs))
	for r := range w.idx.docs {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func (w *world) missing() int {
	w.idx.mu.Lock()
	defer w.idx.mu.Unlock()
	n := 0
	for _, d := range w.idx.docs {
		if d.Embedding == nil {
			n++
		}
	}
	return n
}

func (w *world) doc(ref string) app.Document {
	w.idx.mu.Lock()
	defer w.idx.mu.Unlock()
	return w.idx.docs[ref]
}

func (w *world) seed(ref string, withVector bool) {
	server, _, _ := strings.Cut(ref, ".")
	d := app.Document{Ref: ref, Server: server}
	if withVector {
		d.Embedding = vectorOf(ref)
	}
	w.idx.mu.Lock()
	defer w.idx.mu.Unlock()
	w.idx.docs[ref] = d
}

func (w *world) failAll() { w.emb.fail = func(int, string) error { return errEmbedder } }

func contextWithCancel() (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }
