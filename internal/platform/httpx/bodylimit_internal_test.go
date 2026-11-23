package httpx

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testOrigin = "http://localhost:3000"

// limitServer is a real server around: body limit (limit bytes), CSRF, and a handler that echoes
// the body length it was able to read. Unknown paths are the handler's 404.
func limitServer(t *testing.T, limit int64, seen func(*http.Request, []byte)) *httptest.Server {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			WriteDomainError(w, r, err)
			return
		}
		if seen != nil {
			seen(r, b)
		}
		if r.URL.Path != "/api/echo" {
			WriteError(w, "not_found", "no such route")
			return
		}
		_, _ = w.Write([]byte(strconv.Itoa(len(b))))
	})
	srv := httptest.NewServer(Chain(next, newBodyLimit(limit, 0, true), CSRF(testOrigin)))
	t.Cleanup(srv.Close)
	return srv
}

// chunked sends body without a declared length.
func chunked(t *testing.T, srv *httptest.Server, method, path, origin string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	if req.ContentLength != 0 {
		t.Fatalf("test bug: the request has a declared length %d", req.ContentLength)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

// hono reads a body without Content-Length up to the limit before the later middleware runs, so
// the refusal order does not depend on whether the length was declared.
func TestChunkedBodyIsRefusedBeforeLaterMiddleware(t *testing.T) {
	const limit = 100
	srv := limitServer(t, limit, nil)
	over, exact := bytes.Repeat([]byte("x"), limit+1), bytes.Repeat([]byte("x"), limit)

	for _, tc := range []struct {
		name, path, origin string
		body               []byte
		want               int
	}{
		{"foreign origin, oversized: 413 not 403", "/api/echo", "https://evil.example", over, 413},
		{"no origin, oversized: 413 not 403", "/api/echo", "", over, 413},
		{"unknown route, oversized: 413 not 404", "/api/no-such", testOrigin, over, 413},
		{"exactly the limit passes", "/api/echo", testOrigin, exact, 200},
		{"one byte over", "/api/echo", testOrigin, over, 413},
		{"foreign origin within the limit is still 403", "/api/echo", "https://evil.example", exact, 403},
		{"unknown route within the limit is still 404", "/api/no-such", testOrigin, exact, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := chunked(t, srv, "POST", tc.path, tc.origin, tc.body)
			if code != tc.want {
				t.Fatalf("status %d (%s), want %d", code, body, tc.want)
			}
			if tc.want == 413 && !strings.Contains(body, `"limit_exceeded"`) {
				t.Fatalf("body %s", body)
			}
		})
	}
}

func TestHandlersSeeTheSameBytesAfterTheReadAhead(t *testing.T) {
	var got []byte
	var length int64 = -2
	srv := limitServer(t, 100, func(r *http.Request, b []byte) { got, length = b, r.ContentLength })
	want := []byte(`{"name":"a body that was read ahead"}`)
	if code, _ := chunked(t, srv, "POST", "/api/echo", testOrigin, want); code != 200 {
		t.Fatalf("status %d", code)
	}
	if !bytes.Equal(got, want) || length != -1 {
		t.Fatalf("handler saw %q with ContentLength %d", got, length)
	}
}

// hono does nothing for GET and HEAD (they have no body there) and TRACE is a GET; every other
// method is limited, by its declared length too.
func TestMethodsTheTypeScriptLimiterIgnores(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), newBodyLimit(100, 0, true))
	for _, tc := range []struct {
		method string
		want   int
	}{
		{"GET", 204},
		{"HEAD", 204},
		{"TRACE", 204},
		{"POST", 413},
		{"PUT", 413},
		{"PATCH", 413},
		{"DELETE", 413},
		{"OPTIONS", 413},
	} {
		t.Run(tc.method, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/x", strings.NewReader(strings.Repeat("x", 2000)))
			r.ContentLength = 2000
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("%s with Content-Length 2000: %d, want %d", tc.method, w.Code, tc.want)
			}
		})
	}
}

// deadlineSpy records the read deadlines the limiter sets, like net/http's writer would take them.
type deadlineSpy struct {
	*httptest.ResponseRecorder
	mu    sync.Mutex
	calls []time.Time
}

func (d *deadlineSpy) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, t)
	return nil
}

func TestReadDeadlineIsSetForABodyAndClearedWhenItIsRead(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limiter     Middleware
		method      string
		body        string
		length      int64
		wantTimeout bool
	}{
		{"declared body", BodyLimit(100), "POST", "hello", 5, true},
		{"chunked body", BodyLimit(100), "POST", "hello", -1, true},
		{"no body", BodyLimit(100), "POST", "", 0, false},
		{"GET", BodyLimit(100), "GET", "hello", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &deadlineSpy{ResponseRecorder: httptest.NewRecorder()}
			h := tc.limiter(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { _, _ = io.ReadAll(r.Body) }))
			r := httptest.NewRequest(tc.method, "/x", io.NopCloser(strings.NewReader(tc.body)))
			r.ContentLength = tc.length
			if tc.length == 0 {
				r.Body = http.NoBody
			}
			before := time.Now()
			h.ServeHTTP(spy, r)
			if !tc.wantTimeout {
				if len(spy.calls) != 0 {
					t.Fatalf("deadline calls: %v", spy.calls)
				}
				return
			}
			if len(spy.calls) < 2 {
				t.Fatalf("want a deadline and its clearing, got %v", spy.calls)
			}
			if d := spy.calls[0].Sub(before); d < bodyReadTimeout-time.Second || d > bodyReadTimeout+time.Second {
				t.Fatalf("deadline %v from now, want about %v", d, bodyReadTimeout)
			}
			if !spy.calls[len(spy.calls)-1].IsZero() {
				t.Fatalf("the deadline was left in place: %v", spy.calls)
			}
		})
	}
}

// rawRequest opens a connection to srv, writes head and returns the connection.
func rawRequest(t *testing.T, srv *httptest.Server, head string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatal(err)
	}
	return conn
}

// A client that sends the headers and then stalls must not hold the handler forever: the body
// read fails with a timeout.
func TestStalledBodyTimesOut(t *testing.T) {
	for _, tc := range []struct {
		name, head, partial string
	}{
		{"declared length", "POST /x HTTP/1.1\r\nHost: h\r\nContent-Length: 100\r\n\r\n", "abc"},
		{"chunked", "POST /x HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n", "3\r\nabc\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readErr := make(chan error, 1)
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.ReadAll(r.Body)
				readErr <- err
				w.WriteHeader(408)
			})
			srv := httptest.NewServer(newBodyLimit(1000, 150*time.Millisecond, true)(next))
			t.Cleanup(srv.Close)
			rawRequest(t, srv, tc.head+tc.partial)
			select {
			case err := <-readErr:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("the body read ended with %v, want a deadline error", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the stalled body was never cut")
			}
		})
	}
}

// With the body delivered the deadline must not stay: a handler that works longer than the
// timeout must not lose its request context to net/http's background read.
func TestSlowHandlerKeepsItsContextAfterTheBody(t *testing.T) {
	const timeout = 100 * time.Millisecond
	ctxErr := make(chan error, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * timeout):
		}
		ctxErr <- r.Context().Err()
		w.WriteHeader(204)
	})
	srv := httptest.NewServer(newBodyLimit(1000, timeout, true)(next))
	t.Cleanup(srv.Close)
	for _, body := range []io.Reader{strings.NewReader("hello"), io.NopCloser(strings.NewReader("hello"))} {
		res, err := srv.Client().Post(srv.URL+"/x", "text/plain", body)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if err := <-ctxErr; err != nil {
			t.Fatalf("the request context was cancelled: %v", err)
		}
	}
}
