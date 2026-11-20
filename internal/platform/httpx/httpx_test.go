package httpx_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

const publicURL = "http://localhost:3000"

// required mirrors compat/platform_test.go securityHeaders.
var required = map[string]string{
	"Content-Security-Policy":      "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Referrer-Policy":              "no-referrer",
	"Strict-Transport-Security":    "max-age=15552000; includeSubDomains",
	"X-Content-Type-Options":       "nosniff",
	"X-Frame-Options":              "SAMEORIGIN",
}

var uuidRE = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// app is the chain in production order around a small mux.
type app struct {
	h   http.Handler
	log *bytes.Buffer
}

func newApp(t *testing.T, trustProxy bool, hops int) *app {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := logging.New("info", buf)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/echo", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name string }
		if err := httpx.DecodeJSON(r, &in); err != nil {
			httpx.WriteDomainError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"` + in.Name + `"}`))
	})
	mux.HandleFunc("POST /api/raw", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			httpx.WriteDomainError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /api/id", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Id", httpx.RequestIDOf(r))
	})
	mux.HandleFunc("GET /api/panic", func(_ http.ResponseWriter, _ *http.Request) {
		panic(errors.New("secret-token-in-panic"))
	})
	mux.HandleFunc("GET /api/panic-string", func(_ http.ResponseWriter, _ *http.Request) {
		panic("secret-token-in-panic")
	})
	mux.HandleFunc("GET /api/plain", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hi"))
	})
	mux.Handle("/", httpx.NotFound())
	h := httpx.Chain(mux,
		httpx.RequestID(logger),
		httpx.SecurityHeaders(),
		httpx.BodyLimit(1<<20),
		httpx.ClientIP(trustProxy, hops),
		httpx.CSRF(publicURL),
	)
	return &app{h: h, log: buf}
}

func (a *app) do(method, path string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = "192.0.2.7:5555"
	for _, kv := range hdr {
		k, v, _ := strings.Cut(kv, ": ")
		r.Header.Add(k, v)
	}
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	return w
}

func errorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error body: %q", w.Body.String())
	}
	return e.Error.Code + "|" + e.Error.Message
}

func assertHeaders(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	for name, want := range required {
		if got := w.Header().Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("%s: %s = %q, want %q", what, name, got, want)
		}
	}
	for _, name := range []string{"Server", "X-Powered-By"} {
		if got := w.Header().Values(name); len(got) != 0 {
			t.Errorf("%s: %s = %q must be absent", what, name, got)
		}
	}
	if id := w.Header().Get("X-Request-Id"); !uuidRE.MatchString(id) {
		t.Errorf("%s: X-Request-Id = %q", what, id)
	}
}

// compat: TestPlatformSecurityHeaders (200, 404 text, api 404, csrf 403, 413, each with a request id).
func TestSecurityHeadersAndRequestIDOnEveryResponse(t *testing.T) {
	a := newApp(t, false, 1)
	big := strings.NewReader(strings.Repeat(" ", 1<<20+1))
	cases := []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
	}{
		{"200", a.do("GET", "/api/plain", nil), 200},
		{"404 text", a.do("GET", "/no-such-page", nil), 404},
		{"api 404", a.do("GET", "/api/no-such", nil), 404},
		{"csrf 403", a.do("POST", "/api/echo", nil), 403},
		{"413", a.do("POST", "/api/echo", big, "Content-Type: application/json"), 413},
		{"500 panic", a.do("GET", "/api/panic", nil), 500},
	}
	for _, c := range cases {
		if c.w.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.name, c.w.Code, c.status)
		}
		assertHeaders(t, c.name, c.w)
	}
	// compat: "every response has its own request id".
	x := a.do("GET", "/api/plain", nil).Header().Get("X-Request-Id")
	y := a.do("GET", "/api/plain", nil).Header().Get("X-Request-Id")
	if x == y {
		t.Fatalf("two responses share the request id %q", x)
	}
	// The handler sees the same id.
	w := a.do("GET", "/api/id", nil)
	if w.Header().Get("X-Seen-Id") != w.Header().Get("X-Request-Id") {
		t.Fatalf("RequestIDOf %q, header %q", w.Header().Get("X-Seen-Id"), w.Header().Get("X-Request-Id"))
	}
}

func TestStatusTable(t *testing.T) {
	want := map[string]int{
		"not_found": 404, "invalid_token": 401, "unauthenticated": 401, "forbidden": 403, "csrf": 403,
		"limit_exceeded": 413, "rate_limited": 429, "server_exists": 409, "server_conflict": 409,
		"sync_failed": 502, "invalid_request": 422, "invalid_anything": 422, "whatever": 400, "": 400,
		"internal": 500,
	}
	for code, status := range want {
		if got := httpx.StatusFor(code); got != status {
			t.Errorf("StatusFor(%q) = %d, want %d", code, got, status)
		}
	}
}

func TestInvalidRequestIsA422Error(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.WriteDomainError(w, httpx.InvalidRequest("limit: must be a number"))
	// invalid_request starts with invalid_, so the table gives 422 like the reference (body and
	// query validation errors are 422), not the generic 400.
	if w.Code != 422 || errorOf(t, w) != "invalid_request|limit: must be a number" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestWriteErrorBodyShape(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.WriteError(w, "not_found", "a <b> & c")
	if w.Body.String() != `{"error":{"code":"not_found","message":"a <b> & c"}}` {
		t.Fatalf("%q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q", ct)
	}
}

func TestWriteDomainErrorOther(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(logging.New("info", &logs))
	defer slog.SetDefault(old)
	w := httptest.NewRecorder()
	httpx.WriteDomainError(w, errors.New("secret-value in message"))
	if w.Code != 500 || errorOf(t, w) != "internal|Internal error" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if logs.Len() == 0 || strings.Contains(logs.String(), "secret-value") {
		t.Fatalf("log: %q", logs.String())
	}
}

// compat: TestPlatformAccessLog.
func TestAccessLogRecord(t *testing.T) {
	a := newApp(t, false, 1)
	w := a.do("GET", "/api/plain?probe=abc123", nil, "Authorization: Bearer sk_secret-token", "Cookie: mcp_session=zzz")
	id := w.Header().Get("X-Request-Id")
	recs := logRecords(t, a.log)
	if len(recs) != 1 {
		t.Fatalf("%d records: %s", len(recs), a.log)
	}
	r := recs[0]
	if r["msg"] != "request" || r["reqId"] != id || r["method"] != "GET" || r["path"] != "/api/plain" ||
		r["status"] != float64(200) || r["ip"] != "192.0.2.7" {
		t.Fatalf("record: %v", r)
	}
	if ms, ok := r["ms"].(float64); !ok || ms < 0 || ms != float64(int64(ms)) {
		t.Fatalf("ms: %v", r["ms"])
	}
	for _, f := range []string{"headers", "query", "authorization", "req", "url"} {
		if _, ok := r[f]; ok {
			t.Errorf("record has %q", f)
		}
	}
	if s := a.log.String(); strings.Contains(s, "sk_secret-token") || strings.Contains(s, "probe=") || strings.Contains(s, "abc123") || strings.Contains(s, "zzz") {
		t.Fatalf("log leaks request data: %s", s)
	}
}

func TestAccessLogCoversRefusalsOfLaterMiddleware(t *testing.T) {
	a := newApp(t, false, 1)
	a.do("POST", "/api/echo", nil)                                                                   // csrf
	a.do("POST", "/api/echo", strings.NewReader(strings.Repeat(" ", 1<<20+1)), "Origin: "+publicURL) // 413
	recs := logRecords(t, a.log)
	if len(recs) != 2 || recs[0]["status"] != float64(403) || recs[1]["status"] != float64(413) {
		t.Fatalf("records: %v", recs)
	}
	for _, r := range recs {
		if r["ip"] != "192.0.2.7" || r["reqId"] == "" {
			t.Fatalf("record: %v", r)
		}
	}
}

func TestAccessLogUsesResolvedClientIP(t *testing.T) {
	a := newApp(t, true, 1)
	a.do("GET", "/api/plain", nil, "X-Forwarded-For: 203.0.113.9")
	if r := logRecords(t, a.log)[0]; r["ip"] != "203.0.113.9" {
		t.Fatalf("record: %v", r)
	}
}

func TestPanicIs500AndLoggedWithoutText(t *testing.T) {
	for _, path := range []string{"/api/panic", "/api/panic-string"} {
		a := newApp(t, false, 1)
		w := a.do("GET", path, nil)
		if w.Code != 500 || w.Body.String() != `{"error":{"code":"internal","message":"Internal error"}}` {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		id := w.Header().Get("X-Request-Id")
		recs := logRecords(t, a.log)
		if len(recs) != 2 || recs[0]["reqId"] != id || recs[0]["level"] != float64(50) || recs[1]["msg"] != "request" || recs[1]["status"] != float64(500) {
			t.Fatalf("%s: records %v", path, recs)
		}
		if strings.Contains(a.log.String(), "secret-token-in-panic") {
			t.Fatalf("%s: panic text in the log: %s", path, a.log)
		}
	}
}

func TestStatusRecorderKeepsStreaming(t *testing.T) {
	buf := &bytes.Buffer{}
	h := httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("the wrapped writer is not an http.Flusher")
			return
		}
		_, _ = w.Write([]byte("a"))
		f.Flush()
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController.Flush: %v", err)
		}
	}), httpx.RequestID(logging.New("info", buf)))
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.Status)
	}
}

func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// compat: TestPlatformBodyLimit.
func TestBodyLimit(t *testing.T) {
	a := newApp(t, false, 1)
	const want = "limit_exceeded|request body is too large"
	ok := "Origin: " + publicURL
	pad := func(n int) string { return `{"name":"x"}` + strings.Repeat(" ", n-len(`{"name":"x"}`)) }

	t.Run("with Content-Length over the limit: 413 before reading, connection closed", func(t *testing.T) {
		body := &trackingReader{r: strings.NewReader(pad(1<<20 + 1))}
		r := httptest.NewRequest("POST", "/api/echo", body)
		r.ContentLength = 1<<20 + 1
		r.Header.Set("Origin", publicURL)
		w := httptest.NewRecorder()
		a.h.ServeHTTP(w, r)
		if w.Code != 413 || errorOf(t, w) != want {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if body.read != 0 {
			t.Fatalf("%d bytes of the body were read", body.read)
		}
		if w.Header().Get("Connection") != "close" {
			t.Fatalf("Connection %q", w.Header().Get("Connection"))
		}
	})
	t.Run("without a length: cut at the limit, same 413", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/echo", io.NopCloser(strings.NewReader(pad(1<<20+1))))
		r.ContentLength = -1
		r.Header.Set("Origin", publicURL)
		w := httptest.NewRecorder()
		a.h.ServeHTTP(w, r)
		if w.Code != 413 || errorOf(t, w) != want {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("a handler reading the body itself maps the cut through WriteDomainError", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/raw", io.NopCloser(strings.NewReader(pad(1<<20+1))))
		r.ContentLength = -1
		r.Header.Set("Origin", publicURL)
		w := httptest.NewRecorder()
		a.h.ServeHTTP(w, r)
		if w.Code != 413 || errorOf(t, w) != want {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("exactly the limit passes (control)", func(t *testing.T) {
		w := a.do("POST", "/api/echo", strings.NewReader(pad(1<<20)), ok)
		if w.Code != 200 || w.Body.String() != `{"name":"x"}` {
			t.Fatalf("%d %.100s", w.Code, w.Body)
		}
	})
	t.Run("the limit applies before sign-in and before CSRF order is irrelevant: limit first", func(t *testing.T) {
		// No Origin: CSRF would say 403, but the body limit is earlier in the chain.
		w := a.do("POST", "/api/echo", strings.NewReader(pad(1<<20+1)))
		if w.Code != 413 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
}

type trackingReader struct {
	r    io.Reader
	read int
}

func (t *trackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.read += n
	return n, err
}

// compat: TestPlatformCSRF.
func TestCSRF(t *testing.T) {
	a := newApp(t, false, 1)
	const want = "csrf|cross-origin request rejected"
	t.Run("without Origin", func(t *testing.T) {
		w := a.do("POST", "/api/echo", nil)
		if w.Code != 403 || errorOf(t, w) != want {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("foreign Origin", func(t *testing.T) {
		for _, o := range []string{"http://evil.example", "https://localhost:3000", "http://localhost:3000.evil.example", "null", publicURL + "/"} {
			w := a.do("POST", "/api/echo", nil, "Origin: "+o)
			if w.Code != 403 || errorOf(t, w) != want {
				t.Fatalf("Origin %s: %d %s", o, w.Code, w.Body)
			}
		}
	})
	t.Run("every mutating method", func(t *testing.T) {
		for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			for _, p := range []string{"/api", "/api/x"} {
				w := a.do(m, p, nil, "Origin: http://evil.example")
				if w.Code != 403 || errorOf(t, w) != want {
					t.Fatalf("%s %s: %d %s", m, p, w.Code, w.Body)
				}
			}
		}
	})
	t.Run("Bearer is no exemption", func(t *testing.T) {
		w := a.do("POST", "/api/x", nil, "Authorization: Bearer sk_x")
		if w.Code != 403 {
			t.Fatalf("%d", w.Code)
		}
	})
	t.Run("safe methods need no Origin", func(t *testing.T) {
		for _, m := range []string{"GET", "HEAD", "OPTIONS"} {
			if w := a.do(m, "/api/plain", nil); w.Code == 403 {
				t.Fatalf("%s refused", m)
			}
		}
	})
	t.Run("paths outside /api are not checked", func(t *testing.T) {
		for _, p := range []string{"/mcp", "/apix", "/other"} {
			if w := a.do("POST", p, nil); w.Code != 404 {
				t.Fatalf("%s: %d", p, w.Code)
			}
		}
	})
	t.Run("the right Origin passes, and an unknown /api path is then a 404", func(t *testing.T) {
		if w := a.do("POST", "/api/echo", nil, "Origin: "+publicURL); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		w := a.do("POST", "/api/x", nil, "Origin: "+publicURL)
		if w.Code != 404 || errorOf(t, w) != "not_found|not found" {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
}

func TestCSRFComparesTheOriginOfPublicURL(t *testing.T) {
	for _, c := range []struct{ public, origin string }{
		{"https://mcp.example.com/gateway/", "https://mcp.example.com"},
		{"HTTPS://MCP.Example.com:443", "https://mcp.example.com"},
		{"http://localhost:3000/", "http://localhost:3000"},
		{"http://[::1]:3000", "http://[::1]:3000"},
	} {
		h := httpx.Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), httpx.CSRF(c.public))
		for origin, want := range map[string]int{c.origin: 200, c.origin + "/": 403, "https://other.example": 403} {
			r := httptest.NewRequest("POST", "/api/x", nil)
			r.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != want {
				t.Errorf("public %q, Origin %q: %d, want %d", c.public, origin, w.Code, want)
			}
		}
	}
}

func TestCSRFPanicsOnANonURL(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	httpx.CSRF("not a url")
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		trust  bool
		hops   int
		remote string
		xff    []string
		want   string
	}{
		{"no proxy trust ignores the header", false, 1, "192.0.2.7:1", []string{"203.0.113.1"}, "192.0.2.7"},
		{"hops 1", true, 1, "192.0.2.7:1", []string{"1.1.1.1, 2.2.2.2, 3.3.3.3"}, "3.3.3.3"},
		{"hops 2", true, 2, "192.0.2.7:1", []string{"1.1.1.1, 2.2.2.2, 3.3.3.3"}, "2.2.2.2"},
		{"hops 3", true, 3, "192.0.2.7:1", []string{"1.1.1.1, 2.2.2.2, 3.3.3.3"}, "1.1.1.1"},
		{"list shorter than hops", true, 4, "192.0.2.7:1", []string{"1.1.1.1, 2.2.2.2"}, "192.0.2.7"},
		{"no header", true, 1, "192.0.2.7:1", nil, "192.0.2.7"},
		{"garbage hop", true, 1, "192.0.2.7:1", []string{"1.1.1.1, not-an-ip"}, "192.0.2.7"},
		{"empty hop", true, 1, "192.0.2.7:1", []string{"1.1.1.1,"}, "192.0.2.7"},
		{"hops 0 is not trusted", true, 0, "192.0.2.7:1", []string{"1.1.1.1"}, "192.0.2.7"},
		{"ipv6 hop", true, 1, "192.0.2.7:1", []string{"2001:db8::1"}, "2001:db8::1"},
		{"ipv6 peer without brackets", false, 1, "[2001:db8::7]:4444", nil, "2001:db8::7"},
		{"several header lines are joined in order", true, 2, "192.0.2.7:1", []string{"1.1.1.1", "2.2.2.2, 3.3.3.3"}, "2.2.2.2"},
		{"spoofed left entries are not used", true, 1, "192.0.2.7:1", []string{"9.9.9.9", "3.3.3.3"}, "3.3.3.3"},
		{"peer without a port", false, 1, "192.0.2.7", nil, "192.0.2.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			h := httpx.Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = httpx.ClientIPOf(r) }),
				httpx.ClientIP(c.trust, c.hops))
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			for _, l := range c.xff {
				r.Header.Add("X-Forwarded-For", l)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestDecodeJSON(t *testing.T) {
	type in struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	dec := func(body string) (in, error) {
		var v in
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		return v, httpx.DecodeJSON(r, &v)
	}
	for _, empty := range []string{"", "  \n\t "} {
		if v, err := dec(empty); err != nil || v != (in{}) {
			t.Errorf("empty %q: %v %v", empty, v, err)
		}
	}
	if v, err := dec(`{"name":"a","n":2,"extra":{"x":1}}`); err != nil || v != (in{"a", 2}) {
		t.Errorf("unknown fields: %v %v", v, err)
	}
	for _, bad := range []string{"not json", `{"name":`, `{"name":"a"} {"name":"b"}`, `{"name":"a"} x`, `{"name":"a"}]`} {
		_, err := dec(bad)
		w := httptest.NewRecorder()
		httpx.WriteDomainError(w, err)
		if w.Code != 422 || errorOf(t, w) != "invalid_request|body is not valid JSON" {
			t.Errorf("%q: %d %s", bad, w.Code, w.Body)
		}
	}
	if _, err := dec(`{"name":"a"}  `); err != nil {
		t.Errorf("trailing whitespace: %v", err)
	}
	_, err := dec(`{"n":"secret-value"}`)
	w := httptest.NewRecorder()
	httpx.WriteDomainError(w, err)
	if w.Code != 422 || errorOf(t, w) != "invalid_request|n: invalid type" {
		t.Errorf("type error: %d %s", w.Code, w.Body)
	}
}

func TestNotFound(t *testing.T) {
	h := httpx.NotFound()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/unknown", nil))
	if w.Code != 404 || errorOf(t, w) != "not_found|not found" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/no-such", "/api", "/mcp/sub", "/healthz/x"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 404 || w.Body.String() != "Not found" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("%s: %d %q %q", p, w.Code, w.Body, w.Header().Get("Content-Type"))
		}
	}
}
