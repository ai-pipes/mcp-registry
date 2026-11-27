//go:build compat && load

package compat_test

// The load comparison (make load-compare): the same fixtures and the same traffic against two
// images, one after another, reported as a Markdown table. It reports numbers and asserts
// nothing, except that a stack boots and that an image answers no error while nothing else runs
// (a broken setup is not a measurement). Not part of make compat or go-verify (build tag load).
//
// Decisions (each is repeated in the table's notes):
//   - Sessions: ONE MCP session per sequential series, reused for every call; for the concurrent
//     bursts one session per worker. Sessions are opened right before a series or burst and each
//     makes one untimed request first (the harness transport drops idle connections after 1 s, so
//     a connection opened earlier would be re-dialled inside the timed region). The measurement is
//     the tools/call round trip, not the handshake; the same for both images.
//   - Readiness: before any measurement a vector-only query (a Russian one the text search cannot
//     match against the English tool texts) must return its recorded first hit. It is a guard
//     only: registering the servers already waits for every document to be embedded, so the
//     model is loaded by then in both images and the query answers at once.
//   - The fake upstream of the echo server holds every tools/call for loadUpstreamDelay, so the
//     fake is never the bottleneck and the call has a fixed floor.
//   - Memory is the cgroup figure of `docker stats` (usage minus the inactive file cache), as
//     Docker reports it; the peak is the maximum of a stream sampled about once a second, so it can
//     miss a shorter spike. It is taken only over the search rounds: the call rounds last a second
//     or less, too short for a 1 Hz sampler to say anything.
//   - Limits: the server has no daily limit (null) and /mcp has no rate limit for valid tokens
//     (the only 429 is for failed token checks), so nothing is configured; a 429 would be
//     counted as an error and shown.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

const (
	loadUpstreamDelay = 5 * time.Millisecond
	loadSequential    = 200
	loadWarmup        = 20
	loadWorkers       = 50
	loadRounds        = 5
	loadIdleWait      = 30 * time.Second
	loadIdleSamples   = 3
	loadIdleGap       = 5 * time.Second
	loadReadyQuery    = "создать задачу в трекере"
	loadReadyHit      = "youtrack.create_issue"
)

type loadImage struct {
	label, image, cli, fixed string
}

type loadStats struct {
	n             int
	p50, p95, max time.Duration
	errors        int
	errorKinds    map[string]int
}

type loadConcurrent struct {
	loadStats
	peakMemMiB                float64
	healthzMedian, healthzMax time.Duration
	healthzFailed             int
	wall                      time.Duration
}

type loadResult struct {
	img     loadImage
	digest  string
	idle    []float64
	seqSrch loadStats
	seqCall loadStats
	conCall loadConcurrent
	conSrch loadConcurrent
	ready   time.Duration
}

func TestLoadCompare(t *testing.T) {
	imgs := []loadImage{
		{"A", loadEnv(t, "LOAD_IMAGE_A"), loadEnv(t, "LOAD_CLI_A"), os.Getenv("LOAD_FIXED_A")},
		{"B", loadEnv(t, "LOAD_IMAGE_B"), loadEnv(t, "LOAD_CLI_B"), os.Getenv("LOAD_FIXED_B")},
	}
	var results []loadResult
	for _, img := range imgs {
		// One stack at a time: a subtest's cleanup removes the stack before the next one boots.
		t.Run("image "+img.label, func(t *testing.T) {
			// The harness reads COMPAT_FIXED at boot; it only selects which branch of a decided
			// deviation a scenario asserts, and this test asserts none. Set for fidelity.
			t.Setenv("COMPAT_FIXED", img.fixed)
			results = append(results, loadRun(t, img))
		})
	}
	if t.Failed() || len(results) != len(imgs) {
		return
	}
	report := loadReport(results)
	fmt.Println(report)
	out := os.Getenv("LOAD_OUT")
	if out == "" {
		out = filepath.Join(os.TempDir(), "load-compare-"+time.Now().Format("20060102-150405")+".md")
	}
	if err := os.WriteFile(out, []byte(report), 0o644); err != nil { //nolint:gosec // G306: a report of numbers, readable on purpose
		t.Fatalf("write %s: %v", out, err)
	}
	fmt.Println("report written to", out)
}

func loadEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set: run make load-compare", name)
	}
	return v
}

func loadRun(t *testing.T, img loadImage) loadResult {
	res := loadResult{img: img, digest: loadDigest(img.image)}
	s := harness.Start(t, harness.Options{Image: img.image, CLI: img.cli})
	admin := s.Admin(t)
	token := admin.Token(t, "load")

	gitea := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.FixtureTools("gitea")})
	youtrack := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.FixtureTools("youtrack")})
	echo := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	catalogRegister(t, admin, fakes.ServerInput(gitea, "gitea", "none", fakes.WithName("Gitea"), catalogWithDescription("Git hosting: repositories, pull requests")))
	catalogRegister(t, admin, fakes.ServerInput(youtrack, "youtrack", "none", fakes.WithName("YouTrack"), catalogWithDescription("Issue tracker")))
	catalogRegister(t, admin, fakes.ServerInput(echo, "echo", "shared_secret", fakes.WithName("Echo"), fakes.WithSecret("load-secret"), catalogWithDescription("Echoes its argument")))
	catalogWaitDocs(t, s, "gitea", 61)
	catalogWaitDocs(t, s, "youtrack", 23)
	catalogWaitDocs(t, s, "echo", 1)
	echo.Delay(loadUpstreamDelay) // after the sync: only calls are slowed

	open := func() *mcp.ClientSession {
		sess, err := s.MCPOpen(token, nil)
		if err != nil {
			t.Fatalf("mcp connect: %v", err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		return sess
	}
	searchOp := func(sess *mcp.ClientSession, i int) (string, bool) {
		q := catalogSearchQueries[i%len(catalogSearchQueries)].query
		return loadCall(sess, "search_tools", map[string]any{"query": q, "limit": 10})
	}
	callOp := func(sess *mcp.ClientSession, i int) (string, bool) {
		return loadCall(sess, "call_tool", map[string]any{"tool": "echo.echo", "arguments": map[string]any{"text": "ping " + strconv.Itoa(i)}})
	}

	// Readiness: the vector search answers.
	sess := open()
	harness.Eventually(t, "search_tools ranks a vector-only query", func() bool {
		r, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "search_tools", Arguments: catalogSearchArgs(loadReadyQuery, 10, nil)})
		if err != nil || r.IsError {
			return false
		}
		var out struct {
			Results []catalogHit `json:"results"`
		}
		catalogStructured(t, r, &out)
		return len(out.Results) > 0 && out.Results[0].Tool == loadReadyHit
	})

	// Warm-up, and the "no errors under no load" guard.
	for i := range loadWarmup {
		if kind, ok := searchOp(sess, i); !ok {
			t.Fatalf("search_tools answered an error under no load: %s", kind)
		}
		if kind, ok := callOp(sess, i); !ok {
			t.Fatalf("call_tool answered an error under no load: %s", kind)
		}
	}

	// (1) Idle memory.
	time.Sleep(loadIdleWait)
	for i := range loadIdleSamples {
		if i > 0 {
			time.Sleep(loadIdleGap)
		}
		res.idle = append(res.idle, loadMemMiB(t, s.AppContainer()))
	}

	// (2) Sequential latency on one reused session, opened and warmed right before each series.
	fresh := func(op func(*mcp.ClientSession, int) (string, bool)) *mcp.ClientSession {
		sn := open()
		if kind, ok := op(sn, 0); !ok {
			t.Fatalf("an untimed request answered an error under no load: %s", kind)
		}
		return sn
	}
	res.seqSrch = loadSeries(fresh(searchOp), loadSequential, searchOp)
	res.seqCall = loadSeries(fresh(callOp), loadSequential, callOp)

	// (3) Concurrency: loadWorkers sessions, loadRounds rounds each, sessions opened and warmed
	// right before each burst.
	freshWorkers := func(op func(*mcp.ClientSession, int) (string, bool)) []*mcp.ClientSession {
		ws := make([]*mcp.ClientSession, loadWorkers)
		for i := range ws {
			ws[i] = fresh(op)
		}
		return ws
	}
	res.conCall = loadBursts(s, freshWorkers(callOp), callOp, false)
	res.conSrch = loadBursts(s, freshWorkers(searchOp), searchOp, true)
	return res
}

// loadCall runs one meta-tool call; ok is false for a transport error or an isError answer, and
// kind then names it (cut, with digits kept: a 429 or an HTTP status stays visible).
func loadCall(sess *mcp.ClientSession, name string, args map[string]any) (kind string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "transport: " + loadCut(err.Error()), false
	}
	if r.IsError {
		return "isError: " + loadCut(catalogText(r)), false
	}
	return "", true
}

func loadCut(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

func loadSeries(sess *mcp.ClientSession, n int, op func(*mcp.ClientSession, int) (string, bool)) loadStats {
	d := make([]time.Duration, 0, n)
	kinds := map[string]int{}
	for i := range n {
		start := time.Now()
		kind, ok := op(sess, i)
		d = append(d, time.Since(start))
		if !ok {
			kinds[kind]++
		}
	}
	return loadSummarise(d, kinds)
}

func loadSummarise(d []time.Duration, kinds map[string]int) loadStats {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	st := loadStats{n: len(d), errorKinds: kinds}
	for _, c := range kinds {
		st.errors += c
	}
	if len(d) > 0 {
		st.p50, st.p95, st.max = loadPct(d, 50), loadPct(d, 95), d[len(d)-1]
	}
	return st
}

// loadPct is the nearest-rank percentile of a sorted slice.
func loadPct(d []time.Duration, p int) time.Duration {
	i := int(math.Ceil(float64(p)/100*float64(len(d)))) - 1
	return d[max(0, min(i, len(d)-1))]
}

// loadBursts runs rounds of one call per worker, all released together, one /healthz probe per
// round, with the container's memory streamed throughout.
func loadBursts(s *harness.Stack, workers []*mcp.ClientSession, op func(*mcp.ClientSession, int) (string, bool), measurePeak bool) loadConcurrent {
	peak := func() float64 { return math.NaN() }
	if measurePeak {
		peak = loadPeakWatcher(s.AppContainer())
	}
	var all []time.Duration
	kinds := map[string]int{}
	var probes []time.Duration
	failedProbes := 0
	began := time.Now()
	hc := &http.Client{Timeout: 30 * time.Second}
	for round := range loadRounds {
		var mu sync.Mutex
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for w, sess := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				start := time.Now()
				kind, ok := op(sess, round*len(workers)+w)
				d := time.Since(start)
				mu.Lock()
				all = append(all, d)
				if !ok {
					kinds[kind]++
				}
				mu.Unlock()
			}()
		}
		close(gate)
		time.Sleep(15 * time.Millisecond) // let the burst arrive, then probe the event loop
		ps := time.Now()
		resp, err := hc.Get(s.LoopbackURL() + "/healthz")
		pd := time.Since(ps)
		if err != nil || resp.StatusCode != 200 {
			failedProbes++
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		probes = append(probes, pd)
		wg.Wait()
	}
	wall := time.Since(began)
	time.Sleep(1500 * time.Millisecond) // one more sample after the last round
	out := loadConcurrent{loadStats: loadSummarise(all, kinds), peakMemMiB: peak(), wall: wall, healthzFailed: failedProbes}
	sort.Slice(probes, func(i, j int) bool { return probes[i] < probes[j] })
	out.healthzMedian, out.healthzMax = loadPct(probes, 50), probes[len(probes)-1]
	return out
}

// ---- memory (docker stats) ----

var loadMemRe = regexp.MustCompile(`([0-9.]+)\s*(B|KiB|MiB|GiB)\s*/`)

func loadParseMem(line string) (float64, bool) {
	m := loadMemRe.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	switch m[2] {
	case "B":
		v /= 1024 * 1024
	case "KiB":
		v /= 1024
	case "GiB":
		v *= 1024
	}
	return v, true
}

func loadMemMiB(t *testing.T, container string) float64 {
	t.Helper()
	out, err := exec.Command("docker", "stats", "--no-stream", "--format", "{{.MemUsage}}", container).Output()
	if err != nil {
		t.Fatalf("docker stats: %v", err)
	}
	v, ok := loadParseMem(string(out))
	if !ok {
		t.Fatalf("docker stats: cannot read %q", out)
	}
	return v
}

// loadPeakWatcher streams docker stats and returns a function that stops it and gives the peak.
func loadPeakWatcher(container string) func() float64 {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "docker", "stats", "--format", "{{.MemUsage}}", container)
	pipe, err := cmd.StdoutPipe()
	var mu sync.Mutex
	peak := 0.0
	done := make(chan struct{})
	if err != nil || cmd.Start() != nil {
		close(done)
		return func() float64 { cancel(); return math.NaN() }
	}
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			if v, ok := loadParseMem(sc.Text()); ok {
				mu.Lock()
				peak = max(peak, v)
				mu.Unlock()
			}
		}
	}()
	return func() float64 {
		cancel()
		_ = cmd.Wait()
		<-done
		mu.Lock()
		defer mu.Unlock()
		if peak == 0 {
			return math.NaN()
		}
		return peak
	}
}

// ---- report ----

func loadSh(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func loadDigest(image string) string {
	id := loadSh("docker", "image", "inspect", "-f", "{{.Id}}", image)
	rd := loadSh("docker", "image", "inspect", "-f", "{{range .RepoDigests}}{{.}} {{end}}", image)
	return strings.TrimSpace(rd + " id " + id)
}

func loadCPU() string {
	if runtime.GOOS == "darwin" {
		return loadSh("sysctl", "-n", "machdep.cpu.brand_string")
	}
	raw, _ := os.ReadFile("/proc/cpuinfo")
	for _, l := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000) }

func loadErrs(st loadStats) string {
	if st.errors == 0 {
		return "0"
	}
	var parts []string
	for k, c := range st.errorKinds {
		parts = append(parts, fmt.Sprintf("%d x %s", c, k))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%d (%s)", st.errors, strings.Join(parts, "; "))
}

func loadMiB(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.0f", v)
}

func loadReport(rs []loadResult) string {
	var b strings.Builder
	a, c := rs[0], rs[1]
	fmt.Fprintf(&b, "## Load comparison, %s\n\n", time.Now().Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "Machine: %s, %d logical CPUs; %s %s; Docker server %s (%s).\n\n",
		loadCPU(), runtime.NumCPU(), loadSh("uname", "-sr"), runtime.GOARCH,
		loadSh("docker", "version", "-f", "{{.Server.Version}}"),
		loadSh("docker", "info", "-f", "{{.NCPU}} CPUs, {{.MemTotal}} bytes memory in the Docker VM"))
	for _, r := range rs {
		fmt.Fprintf(&b, "- Image %s: `%s` (%s), CLI `%s`; digest %s\n", r.img.label, r.img.image, loadSh("docker", "image", "inspect", "-f", "{{.Architecture}}", r.img.image), r.img.cli, r.digest)
	}
	b.WriteString("\n| Metric | A | B |\n|---|---|---|\n")
	row := func(name, x, y string) { fmt.Fprintf(&b, "| %s | %s | %s |\n", name, x, y) }
	idle := func(r loadResult) string {
		var p []string
		for _, v := range r.idle {
			p = append(p, loadMiB(v))
		}
		return strings.Join(p, " / ")
	}
	row("Idle memory, 3 samples (MiB)", idle(a), idle(c))
	row("search_tools sequential x200: p50 / p95 / max (ms)", fmt.Sprintf("%s / %s / %s", ms(a.seqSrch.p50), ms(a.seqSrch.p95), ms(a.seqSrch.max)), fmt.Sprintf("%s / %s / %s", ms(c.seqSrch.p50), ms(c.seqSrch.p95), ms(c.seqSrch.max)))
	row("search_tools sequential errors", loadErrs(a.seqSrch), loadErrs(c.seqSrch))
	row("call_tool sequential x200: p50 / p95 / max (ms)", fmt.Sprintf("%s / %s / %s", ms(a.seqCall.p50), ms(a.seqCall.p95), ms(a.seqCall.max)), fmt.Sprintf("%s / %s / %s", ms(c.seqCall.p50), ms(c.seqCall.p95), ms(c.seqCall.max)))
	row("call_tool sequential errors", loadErrs(a.seqCall), loadErrs(c.seqCall))
	con := func(name string, x, y loadConcurrent) {
		row(name+" 50 x 5: p50 / p95 / max (ms)", fmt.Sprintf("%s / %s / %s", ms(x.p50), ms(x.p95), ms(x.max)), fmt.Sprintf("%s / %s / %s", ms(y.p50), ms(y.p95), ms(y.max)))
		row(name+" 50 x 5: errors of 250", loadErrs(x.loadStats), loadErrs(y.loadStats))
		row(name+" 50 x 5: wall time of 5 rounds (s)", fmt.Sprintf("%.2f", x.wall.Seconds()), fmt.Sprintf("%.2f", y.wall.Seconds()))
		if !math.IsNaN(x.peakMemMiB) {
			row(name+" 50 x 5: peak memory, approximate (MiB)", loadMiB(x.peakMemMiB), loadMiB(y.peakMemMiB))
		}
		row(name+" 50 x 5: /healthz during a round, median / max (ms), failed", fmt.Sprintf("%s / %s, %d", ms(x.healthzMedian), ms(x.healthzMax), x.healthzFailed), fmt.Sprintf("%s / %s, %d", ms(y.healthzMedian), ms(y.healthzMax), y.healthzFailed))
	}
	con("call_tool", a.conCall, c.conCall)
	con("search_tools", a.conSrch, c.conSrch)
	fmt.Fprintf(&b, `
Notes. Fixtures: the recorded Gitea (61) and YouTrack (23) tools plus one echo tool (85 indexed documents), one admin, one sk_ token; the echo server uses shared_secret, its fake upstream holds every call for %s. Search queries rotate through the 18 queries of TestCatalogSearch (5 of them match nothing on purpose), limit 10. One MCP session is reused for each sequential series; the concurrent rounds use 50 sessions; every session is opened right before its series or burst and makes one untimed request first (handshake and dial not timed). The load driver runs on the same host as the containers. Warm-up: a vector-only query must return its recorded hit (it already does once the index is embedded: the catalog waits for the embeddings, so the model is loaded before this point in both images), then %d searches and %d calls. Idle memory: `+"`docker stats`"+` (cgroup usage without inactive file cache) %s after the warm-up, no traffic, samples %s apart. Peak memory (search rounds only; the call rounds are too short for a 1 Hz sampler): maximum of a `+"`docker stats`"+` stream (about 1 Hz) over the %d rounds and a 1.5 s tail, so it can miss a shorter spike. /healthz: one request %d ms after the burst of each round. No daily limit and no rate limit were configured or hit; any 429 would be listed under errors. Percentiles are nearest-rank over client-observed times, including the host's Docker port mapping; A runs with COMPAT_FIXED=%q, B with %q.
`, loadUpstreamDelay, loadWarmup, loadWarmup, loadIdleWait, loadIdleGap, loadRounds, 15, a.img.fixed, c.img.fixed)
	return b.String()
}
