package infra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

var _ app.Embedder = (*OpenAIEmbedder)(nil)

// recorded is one request as the server saw it.
type recorded struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

// embeddingServer answers like the compat suite's fake (and OpenAI): an object list with one
// 384-number vector per input. It records every request.
type embeddingServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []recorded
	// respond overrides the answer; nil means the 200 answer with vector(0.5).
	respond func(w http.ResponseWriter, r *http.Request)
}

func vector384(x float64) []float64 {
	v := make([]float64, app.Dimensions)
	for i := range v {
		v[i] = x
	}
	return v
}

func startEmbeddingServer(t *testing.T) *embeddingServer {
	t.Helper()
	s := &embeddingServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: string(body)})
		respond := s.respond
		s.mu.Unlock()
		if respond != nil {
			respond(w, r)
			return
		}
		writeVectors(w, 1, 0.5)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *embeddingServer) requests() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.seen...)
}

func (s *embeddingServer) answer(f func(w http.ResponseWriter, r *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = f
}

func writeVectors(w http.ResponseWriter, n int, x float64) {
	type item struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	}
	data := make([]item, n)
	for i := range data {
		data[i] = item{Object: "embedding", Index: i, Embedding: vector384(x)}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": "m", "data": data})
}

func rawAnswer(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestOpenAIEmbedderSendsExactlyWhatTypeScriptSends(t *testing.T) {
	s := startEmbeddingServer(t)
	e := NewOpenAIEmbedder(s.URL+"/v1/", "e5", nil)
	got, err := e.EmbedPassage(t.Context(), "hello <world> & more")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != app.Dimensions || got[0] != 0.5 {
		t.Fatalf("vector %d long, first %v", len(got), got[0])
	}
	reqs := s.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/v1/embeddings" || r.Query != "" {
		t.Errorf("%s %s?%s", r.Method, r.Path, r.Query)
	}
	// The body is JSON.stringify({ model, input: [text] }): compact, model first, the text as is.
	const want = `{"model":"e5","input":["passage: hello <world> & more"]}`
	if r.Body != want {
		t.Errorf("body:\n got %s\nwant %s", r.Body, want)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	// There is no key variable in this code base: nothing that authenticates is sent.
	for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key", "Api-Key"} {
		if v := r.Header.Values(h); len(v) != 0 {
			t.Errorf("%s header sent: %q", h, v)
		}
	}
}

func TestOpenAIEmbedderPrefixesQueriesAndPassages(t *testing.T) {
	s := startEmbeddingServer(t)
	e := NewOpenAIEmbedder(s.URL, "m", nil)
	if _, err := e.EmbedQuery(t.Context(), "create issue"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.EmbedPassage(t.Context(), "create issue"); err != nil {
		t.Fatal(err)
	}
	reqs := s.requests()
	if reqs[0].Body != `{"model":"m","input":["query: create issue"]}` || reqs[1].Body != `{"model":"m","input":["passage: create issue"]}` {
		t.Fatalf("%s\n%s", reqs[0].Body, reqs[1].Body)
	}
	if reqs[0].Path != "/embeddings" {
		t.Fatalf("path %q", reqs[0].Path)
	}
}

func TestOpenAIEmbedderSendsOneTextPerRequestAsAnArrayOfOne(t *testing.T) {
	s := startEmbeddingServer(t)
	e := NewOpenAIEmbedder(s.URL, "m", nil)
	for _, text := range []string{"a", "b", "c"} {
		if _, err := e.EmbedPassage(t.Context(), text); err != nil {
			t.Fatal(err)
		}
	}
	reqs := s.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests for 3 texts", len(reqs))
	}
	for i, r := range reqs {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.Unmarshal([]byte(r.Body), &body); err != nil || len(body.Input) != 1 {
			t.Errorf("request %d: %v %s", i, err, r.Body)
		}
	}
}

func TestOpenAIEmbedderJoinsTheURLLikeTypeScript(t *testing.T) {
	s := startEmbeddingServer(t)
	for _, tc := range []struct{ suffix, path string }{
		{"", "/embeddings"},
		{"/", "/embeddings"},
		{"///", "/embeddings"},
		{"/v1", "/v1/embeddings"},
		{"/v1/", "/v1/embeddings"},
		{"/a/b//", "/a/b/embeddings"},
	} {
		before := len(s.requests())
		if _, err := NewOpenAIEmbedder(s.URL+tc.suffix, "m", nil).EmbedQuery(t.Context(), "q"); err != nil {
			t.Fatal(err)
		}
		if got := s.requests()[before].Path; got != tc.path {
			t.Errorf("%q: path %q, want %q", tc.suffix, got, tc.path)
		}
	}
}

func TestOpenAIEmbedderReturnsTheNumbersAsGiven(t *testing.T) {
	s := startEmbeddingServer(t)
	v := vector384(0)
	v[0], v[1], v[383] = 0.10000000149011612, -3.5e-8, 1
	body, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": v}}})
	s.answer(rawAnswer(200, string(body)))
	got, err := NewOpenAIEmbedder(s.URL, "m", nil).EmbedQuery(t.Context(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != float32(0.10000000149011612) || got[1] != float32(-3.5e-8) || got[383] != 1 || got[100] != 0 {
		t.Fatalf("%v %v %v", got[0], got[1], got[383])
	}
	// Not normalised, not checked for unit length: used as given.
	s.answer(func(w http.ResponseWriter, _ *http.Request) { writeVectors(w, 1, 7) })
	got, _ = NewOpenAIEmbedder(s.URL, "m", nil).EmbedQuery(t.Context(), "q")
	if got[0] != 7 {
		t.Fatalf("%v", got[0])
	}
}

func TestOpenAIEmbedderAcceptsWhatTheSchemaAccepts(t *testing.T) {
	s := startEmbeddingServer(t)
	vec, _ := json.Marshal(vector384(1))
	for name, body := range map[string]string{
		"no index":                  `{"data":[{"embedding":` + string(vec) + `}]}`,
		"index 0":                   `{"data":[{"index":0,"embedding":` + string(vec) + `}]}`,
		"index 3 of one (resorted)": `{"data":[{"index":3,"embedding":` + string(vec) + `}]}`,
		"index 2.0":                 `{"data":[{"index":2.0,"embedding":` + string(vec) + `}]}`,
		"extra keys":                `{"object":"list","usage":{"n":1},"data":[{"object":"embedding","embedding":` + string(vec) + `,"x":null}]}`,
	} {
		s.answer(rawAnswer(200, body))
		if _, err := NewOpenAIEmbedder(s.URL, "m", nil).EmbedQuery(t.Context(), "q"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOpenAIEmbedderFailures(t *testing.T) {
	s := startEmbeddingServer(t)
	vec, _ := json.Marshal(vector384(1))
	short, _ := json.Marshal(vector384(1)[:383])
	long, _ := json.Marshal(append(vector384(1), 1))
	withNull := strings.Replace(string(vec), "1,", "null,", 1)
	withString := strings.Replace(string(vec), "1,", `"1",`, 1)
	shape := "embedder must return 1 vectors of 384 numbers"
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"500", 500, `{"error":"SECRETBODY"}`, "embedder responded with HTTP 500"},
		{"503", 503, `SECRETBODY`, "embedder responded with HTTP 503"},
		{"404", 404, `SECRETBODY`, "embedder responded with HTTP 404"},
		{"a redirect without a Location is not ok", 302, `SECRETBODY`, "embedder responded with HTTP 302"},
		{"invalid json", 200, `SECRETBODY <html>`, "embedder response is not valid JSON"},
		{"empty body", 200, ``, "embedder response is not valid JSON"},
		{"trailing garbage", 200, `{"data":[{"embedding":` + string(vec) + `}]} SECRETBODY`, "embedder response is not valid JSON"},
		{"not an object", 200, `[1,2,3]`, shape},
		{"null", 200, `null`, shape},
		{"no data", 200, `{"SECRETBODY":1}`, shape},
		{"data not an array", 200, `{"data":"SECRETBODY"}`, shape},
		{"empty data", 200, `{"data":[]}`, shape},
		{"two vectors for one text", 200, `{"data":[{"embedding":` + string(vec) + `},{"embedding":` + string(vec) + `}]}`, shape},
		{"383 numbers", 200, `{"data":[{"embedding":` + string(short) + `}]}`, shape},
		{"385 numbers", 200, `{"data":[{"embedding":` + string(long) + `}]}`, shape},
		{"no embedding", 200, `{"data":[{"index":0}]}`, shape},
		{"embedding null", 200, `{"data":[{"embedding":null}]}`, shape},
		{"a null component", 200, `{"data":[{"embedding":` + withNull + `}]}`, shape},
		{"a string component", 200, `{"data":[{"embedding":` + withString + `}]}`, shape},
		{"index not an integer", 200, `{"data":[{"index":1.5,"embedding":` + string(vec) + `}]}`, shape},
		{"index null", 200, `{"data":[{"index":null,"embedding":` + string(vec) + `}]}`, shape},
		{"index a string", 200, `{"data":[{"index":"0","embedding":` + string(vec) + `}]}`, shape},
		{"a component beyond float32", 200, `{"data":[{"embedding":` + strings.Replace(string(vec), "1,", "1e39,", 1) + `}]}`, shape},
		{"a component beyond float64", 200, `{"data":[{"embedding":` + strings.Replace(string(vec), "1,", "1e999,", 1) + `}]}`, shape},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.answer(rawAnswer(tc.status, tc.body))
			_, err := NewOpenAIEmbedder(s.URL, "m", nil).EmbedPassage(t.Context(), "TEXTSECRET")
			if err == nil {
				t.Fatal("no error")
			}
			if err.Error() != tc.want {
				t.Errorf("error %q, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "SECRETBODY") || strings.Contains(err.Error(), "TEXTSECRET") {
				t.Errorf("the error carries the response or the request text: %q", err)
			}
		})
	}
}

func TestOpenAIEmbedderTransportErrorsCarryNoURLOrText(t *testing.T) {
	s := startEmbeddingServer(t)
	url := s.URL + "/v1"
	s.Close()
	_, err := NewOpenAIEmbedder(url, "m", nil).EmbedPassage(t.Context(), "TEXTSECRET")
	if err == nil {
		t.Fatal("no error")
	}
	for _, leak := range []string{"TEXTSECRET", "127.0.0.1", "/v1"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q contains %q", err, leak)
		}
	}
	if err.Error() != "embedder request failed" {
		t.Errorf("error %q", err)
	}
}

func TestOpenAIEmbedderTimeout(t *testing.T) {
	s := startEmbeddingServer(t)
	s.answer(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	e := NewOpenAIEmbedder(s.URL, "m", nil)
	if e.timeout != 20*time.Second {
		t.Fatalf("timeout %v, want 20s as in TypeScript", e.timeout)
	}
	e.timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := e.EmbedQuery(t.Context(), "q")
	if err == nil || err.Error() != "embedder request timed out" {
		t.Fatalf("error %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("the timeout does not unwrap to context.DeadlineExceeded")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestOpenAIEmbedderTimeoutCoversReadingTheBody(t *testing.T) {
	s := startEmbeddingServer(t)
	s.answer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"data":[`)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the rest never comes
	})
	e := NewOpenAIEmbedder(s.URL, "m", nil)
	e.timeout = 100 * time.Millisecond
	if _, err := e.EmbedQuery(t.Context(), "q"); err == nil || err.Error() != "embedder request timed out" {
		t.Fatalf("error %v", err)
	}
}

func TestOpenAIEmbedderCancellationReturnsTheContextError(t *testing.T) {
	s := startEmbeddingServer(t)
	arrived := make(chan struct{})
	var once sync.Once
	s.answer(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-arrived
		cancel()
	}()
	_, err := NewOpenAIEmbedder(s.URL, "m", nil).EmbedPassage(ctx, "q")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
}

func TestOpenAIEmbedderResponseSizeIsBounded(t *testing.T) {
	s := startEmbeddingServer(t)
	s.answer(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"data":[{"embedding":[`)
		chunk := strings.Repeat("0,", 1<<16)
		for i := 0; i < (maxEmbedResponseBytes/len(chunk))+2; i++ {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	})
	_, err := NewOpenAIEmbedder(s.URL, "m", nil).EmbedQuery(t.Context(), "q")
	if err == nil || err.Error() != "embedder response is too large" {
		t.Fatalf("error %v", err)
	}
}

func TestOpenAIEmbedderFollowsRedirectsLikeFetch(t *testing.T) {
	target := startEmbeddingServer(t)
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/moved/embeddings", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(hop.Close)
	if _, err := NewOpenAIEmbedder(hop.URL, "m", nil).EmbedQuery(t.Context(), "q"); err != nil {
		t.Fatal(err)
	}
	r := target.requests()
	// 307 keeps the method and the body, as fetch does.
	if len(r) != 1 || r[0].Method != http.MethodPost || r[0].Path != "/moved/embeddings" || r[0].Body != `{"model":"m","input":["query: q"]}` {
		t.Fatalf("%+v", r)
	}
}

func TestOpenAIEmbedderIsAlwaysReady(t *testing.T) {
	e := NewOpenAIEmbedder("http://127.0.0.1:1", "m", nil)
	if !e.Ready(t.Context()) {
		t.Fatal("not ready")
	}
}

func TestOpenAIEmbedderSplitPairStandIn(t *testing.T) {
	s := startEmbeddingServer(t)
	e := NewOpenAIEmbedder(s.URL, "m", nil)
	stand := domain.SplitPairStandIn
	for _, tc := range []struct {
		name, text, wantInput string
	}{
		{"a trailing stand-in is sent as U+FFFD", "abc" + stand, "passage: abc�"},
		{"only the last rune: an interior one is sent as it is", "a" + stand + "b", "passage: a" + stand + "b"},
		{"only one trailing occurrence is replaced", "a" + stand + stand, "passage: a" + stand + "�"},
		{"only a stand-in is replaced too", stand, "passage: �"},
		{"a real U+FFFD stays", "x�", "passage: x�"},
		{"no stand-in, nothing changes", "plain текст 😀", "passage: plain текст 😀"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(s.requests())
			if _, err := e.EmbedPassage(t.Context(), tc.text); err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(tc.wantInput)
			want := `{"model":"m","input":[` + string(input) + `]}`
			got := s.requests()[before].Body
			if got != want {
				t.Errorf("body %q, want %q", got, want)
			}
		})
	}
	// The same for queries.
	before := len(s.requests())
	if _, err := e.EmbedQuery(t.Context(), "q"+stand); err != nil {
		t.Fatal(err)
	}
	if got := s.requests()[before].Body; !strings.Contains(got, "query: q�") || strings.Contains(got, stand) {
		t.Errorf("query body %q", got)
	}
}

func TestOpenAIEmbedderWorksWhenCalledConcurrently(t *testing.T) {
	s := startEmbeddingServer(t)
	e := NewOpenAIEmbedder(s.URL, "m", nil)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.EmbedPassage(context.Background(), fmt.Sprint("t", i))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := len(s.requests()); n != 16 {
		t.Fatalf("%d requests", n)
	}
}

// TestOpenAIEmbedderConvertsNumbersAsTheReferenceDoes: TypeScript prints each JSON double as its
// shortest decimal and pgvector parses that text as float4, so a double exactly halfway between two
// float32 values goes where the decimal text points, not to the even neighbour.
func TestOpenAIEmbedderConvertsNumbersAsTheReferenceDoes(t *testing.T) {
	s := startEmbeddingServer(t)
	e := NewOpenAIEmbedder(s.URL, "m", nil)
	above := 1 + math.Ldexp(1, -24)   // halfway between 1 and 1+2^-23; its shortest text, 1.0000000596046448, is above it
	below := 1 + 3*math.Ldexp(1, -24) // halfway between 1+2^-23 and 1+2^-22; its text, 1.0000001788139343, is below it
	for name, tc := range map[string]struct {
		x    float64
		bits uint32
	}{
		"halfway, text above (Go's direct rounding gives 1)":           {above, 0x3f800001},
		"halfway, text below (Go's direct rounding gives 1+2^-22)":     {below, 0x3f800001},
		"halfway, negative, text above":                                {-above, 0xbf800001},
		"an exactly representable value":                               {0.25, 0x3e800000},
		"underflow to zero is positive or negative zero, kept as zero": {1e-60, 0},
	} {
		v := vector384(0)
		v[7] = tc.x
		body, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"embedding": v}}})
		s.answer(rawAnswer(200, string(body)))
		got, err := e.EmbedQuery(t.Context(), "q")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if math.Float32bits(got[7]) != tc.bits {
			t.Errorf("%s: %v became %08x, want %08x", name, tc.x, math.Float32bits(got[7]), tc.bits)
		}
	}
	// The JSON the test sent really holds the halfway double (the encoder prints the shortest text).
	if strconv.FormatFloat(above, 'g', -1, 64) != "1.0000000596046448" || strconv.FormatFloat(below, 'g', -1, 64) != "1.0000001788139343" {
		t.Fatal("the halfway values are not the ones the test names")
	}
}

func TestOpenAIEmbedderRefusesACredentialedURLWithoutSendingAnything(t *testing.T) {
	s := startEmbeddingServer(t)
	for _, base := range []string{
		strings.Replace(s.URL, "http://", "http://user:pw-SECRET@", 1) + "/v1",
		strings.Replace(s.URL, "http://", "http://user@", 1),
		strings.Replace(s.URL, "http://", "http://:pw-SECRET@", 1),
	} {
		e := NewOpenAIEmbedder(base, "m", nil)
		for _, call := range []func() error{
			func() error { _, err := e.EmbedPassage(t.Context(), "TEXTSECRET"); return err },
			func() error { _, err := e.EmbedQuery(t.Context(), "TEXTSECRET"); return err },
		} {
			err := call()
			if err == nil {
				t.Fatal("a URL with credentials was used")
			}
			if err.Error() != "embedder URL must not contain credentials" {
				t.Errorf("error %q", err)
			}
		}
	}
	if n := len(s.requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}

func TestOpenAIEmbedderNamesNoProductInItsHeaders(t *testing.T) {
	s := startEmbeddingServer(t)
	if _, err := NewOpenAIEmbedder(s.URL, "m", nil).EmbedQuery(t.Context(), "q"); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"Content-Type": true, "Content-Length": true, "User-Agent": true, "Accept-Encoding": true}
	for name, values := range s.requests()[0].Header {
		if !allowed[name] {
			t.Errorf("unexpected header %s: %q", name, values)
		}
		for _, v := range values {
			if name != "Content-Type" && strings.Contains(strings.ToLower(v), "mcp-registry") {
				t.Errorf("header %s names the product: %q", name, v)
			}
		}
	}
}

// Node's fetch ignores HTTP_PROXY and HTTPS_PROXY, so the reference sends the passage text straight
// to EMBEDDER_URL; the default client's transport would route it through the proxy of the
// environment. Go's proxy-from-environment skips loopback targets, so the target here is a
// made-up host name that a custom dialer maps to the test server: with the environment proxy in
// play the request would go to the recording proxy instead. The transport is the default client's
// own, cloned, with only its dialer replaced, so its Proxy field is what the adapter ships.
func TestOpenAIEmbedderIgnoresProxyVariables(t *testing.T) {
	target := startEmbeddingServer(t)
	proxy := startEmbeddingServer(t)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	def := newDefaultClient()
	tr, ok := def.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("the default client's transport must be an http.Transport with no Proxy: %#v", def.Transport)
	}
	tr = tr.Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target.Listener.Addr().String())
	}
	e := NewOpenAIEmbedder("http://embedder.example.test:8080/v1", "m", &http.Client{Transport: tr})
	if _, err := e.EmbedPassage(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if n := len(target.requests()); n != 1 {
		t.Fatalf("the target saw %d requests, want 1", n)
	}
	if n := len(proxy.requests()); n != 0 {
		t.Fatalf("the environment proxy saw %d requests: the adapter must ignore the proxy variables", n)
	}
	if got := NewOpenAIEmbedder("http://x/v1", "m", nil).client.Transport.(*http.Transport).Proxy; got != nil {
		t.Fatal("NewOpenAIEmbedder(nil client) must use a transport without Proxy")
	}
}
