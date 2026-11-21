package httpx_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

// syncBuf is a log sink a server goroutine and the test may touch concurrently.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) records(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var buf bytes.Buffer
	buf.Write(s.b.Bytes())
	return logRecords(t, &buf)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// serve runs h behind RequestID on a real server. The returned channel receives one value per
// request, after RequestID has returned (so after its access record), also for aborted requests.
func serve(t *testing.T, h http.Handler, extra ...httpx.Middleware) (url string, logs *syncBuf, done <-chan struct{}) {
	t.Helper()
	logs = &syncBuf{}
	ch := make(chan struct{}, 8)
	signal := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { ch <- struct{}{} }()
			next.ServeHTTP(w, r)
		})
	}
	mws := append([]httpx.Middleware{signal, httpx.RequestID(logging.New("info", logs)), httpx.SecurityHeaders()}, extra...)
	srv := httptest.NewServer(httpx.Chain(h, mws...))
	t.Cleanup(srv.Close)
	return srv.URL, logs, ch
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not finish")
	}
}

func byMsg(recs []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// Item 1: a panic after the response started must abort the connection, not end the body cleanly.
func TestPanicAfterTheResponseStartedAbortsTheConnection(t *testing.T) {
	url, logs, done := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		_ = http.NewResponseController(w).Flush()
		panic(errors.New("secret-token-in-panic"))
	}))
	res, err := fetch(url)
	if err == nil {
		body, rerr := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if rerr == nil {
			t.Fatalf("the client saw a complete response: %d %q", res.StatusCode, body)
		}
	}
	waitDone(t, done)
	recs := logs.records(t)
	if len(byMsg(recs, "unhandled error")) != 1 || len(byMsg(recs, "request")) != 1 {
		t.Fatalf("records: %v", recs)
	}
	if byMsg(recs, "request")[0]["status"] != float64(200) {
		t.Fatalf("access record: %v", recs)
	}
	if strings.Contains(logs.String(), "secret-token-in-panic") {
		t.Fatalf("panic text in the log: %s", logs)
	}
}

func TestAbortHandlerIsPassedOnWithoutAnErrorRecord(t *testing.T) {
	url, logs, done := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	if res, err := fetch(url); err == nil {
		_ = res.Body.Close()
		t.Fatalf("the client got a response: %d", res.StatusCode)
	}
	waitDone(t, done)
	recs := logs.records(t)
	if len(byMsg(recs, "unhandled error")) != 0 || len(byMsg(recs, "request")) != 1 {
		t.Fatalf("records: %v", recs)
	}
}

// Item 3: the 500 after a panic carries only the security headers and the request id.
func TestPanicAnswerDropsTheHandlersHeaders(t *testing.T) {
	url, _, done := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Encoding", "gzip")
		h.Set("Cache-Control", "public, max-age=31536000")
		h.Set("Content-Type", "text/html")
		h.Set("Content-Length", "999")
		h.Set("Set-Cookie", "a=b")
		h.Set("Etag", `"x"`)
		panic("boom")
	}))
	res, err := fetch(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	waitDone(t, done)
	if res.StatusCode != 500 || string(body) != `{"error":{"code":"internal","message":"Internal error"}}` {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
	for _, name := range []string{"Content-Encoding", "Cache-Control", "Set-Cookie", "Etag"} {
		if v := res.Header.Values(name); len(v) != 0 {
			t.Errorf("%s = %q survived", name, v)
		}
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	for name, want := range required {
		if got := res.Header.Get(name); got != want {
			t.Errorf("%s = %q", name, got)
		}
	}
	if !uuidRE.MatchString(res.Header.Get("X-Request-Id")) {
		t.Errorf("X-Request-Id = %q", res.Header.Get("X-Request-Id"))
	}
}

// Item 2: the error log of WriteDomainError is request-scoped and has no error text.
func TestWriteDomainErrorLogsWithTheRequestID(t *testing.T) {
	url, logs, done := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteDomainError(w, r, errors.New("secret-value in message"))
	}))
	res, err := fetch(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	waitDone(t, done)
	id := res.Header.Get("X-Request-Id")
	recs := byMsg(logs.records(t), "unhandled error")
	if res.StatusCode != 500 || len(recs) != 1 || recs[0]["reqId"] != id {
		t.Fatalf("%d, id %q, records %v", res.StatusCode, id, logs)
	}
	if strings.Contains(logs.String(), "secret-value") {
		t.Fatalf("error text in the log: %s", logs)
	}
}

// Without the middleware the answer is the same (the log goes to slog's default logger, which
// this test does not touch).
func TestWriteDomainErrorWithoutTheMiddleware(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.WriteDomainError(w, httptest.NewRequest("GET", "/", nil), errors.New("x"))
	if w.Code != 500 || errorOf(t, w) != "internal|Internal error" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// Item 8: the status is the first one sent; Write and Flush without WriteHeader mean 200.
func TestRecordedStatusIsTheFirstOneSent(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"write then WriteHeader": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("x"))
			w.WriteHeader(http.StatusInternalServerError) // superfluous: net/http ignores it
		},
		"flush then WriteHeader": func(w http.ResponseWriter, _ *http.Request) {
			w.(http.Flusher).Flush()
			w.WriteHeader(http.StatusInternalServerError)
		},
		"interim then final": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			_, _ = w.Write([]byte("x"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			url, logs, done := serve(t, h)
			res, err := fetch(url)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			waitDone(t, done)
			recs := byMsg(logs.records(t), "request")
			if res.StatusCode != 200 || len(recs) != 1 || recs[0]["status"] != float64(200) {
				t.Fatalf("client %d, records %v", res.StatusCode, recs)
			}
		})
	}
}

// Item 8: a hijacked connection still gets exactly one access record. No HTTP status passes
// through net/http then, so the record says status 0 unless the handler sent one before.
func TestHijackedConnectionIsLoggedOnce(t *testing.T) {
	url, logs, done := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the wrapped writer is not an http.Hijacker")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		_ = rw.Flush()
	}))
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET /hijack HTTP/1.1\r\nHost: x\r\n\r\n")
	line, _ := bufio.NewReader(conn).ReadString('\n')
	waitDone(t, done)
	recs := byMsg(logs.records(t), "request")
	if !strings.HasPrefix(line, "HTTP/1.1 200") || len(recs) != 1 || recs[0]["path"] != "/hijack" || recs[0]["status"] != float64(0) {
		t.Fatalf("line %q, records %v", line, recs)
	}
}

// Item 5.
func TestDecodeJSONIsStrictAboutNullAndFieldCase(t *testing.T) {
	type in struct {
		Name string `json:"name"`
	}
	dec := func(body string) (in, error) {
		var v in
		return v, httpx.DecodeJSON(httptest.NewRequest("POST", "/", strings.NewReader(body)), &v)
	}
	for _, body := range []string{"null", "  null\n"} {
		_, err := dec(body)
		w := httptest.NewRecorder()
		httpx.WriteDomainError(w, httptest.NewRequest("POST", "/", nil), err)
		if w.Code != 422 || !strings.HasPrefix(errorOf(t, w), "invalid_request|") {
			t.Errorf("%q: %d %s", body, w.Code, w.Body)
		}
	}
	if v, err := dec(`{"NAME":"x","Name":"y"}`); err != nil || v.Name != "" {
		t.Errorf("case-insensitive match: %v %v", v, err)
	}
	if v, err := dec(`{"name":"ok","NAME":"x"}`); err != nil || v.Name != "ok" {
		t.Errorf("exact key lost: %v %v", v, err)
	}
	if _, err := dec(`{"name":"a"} {}`); err == nil {
		t.Error("trailing data accepted")
	}
}

// Item 6.
func TestCSRFDecidesOnTheCleanedPath(t *testing.T) {
	a := newApp(t, false, 1)
	for _, p := range []string{"//api/x", "/x/../api/x", "/./api/x", "/api//x", "/api/./x", "//api"} {
		w := a.do("POST", p, nil, "Origin: http://evil.example")
		if w.Code != 403 {
			t.Errorf("%s: %d, want 403", p, w.Code)
		}
	}
	if w := a.do("POST", "//apix", nil); w.Code == 403 {
		t.Error("/apix is not under /api")
	}
}

func fetch(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}
