package httpx

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// LargeBodyLimit (/mcp) does not read a body without a declared length ahead: the handler's own
// guards answer first, and a handler that reads past the limit gets the 413 path.
func TestTheLargeUploadLimiterLetsTheGuardsAnswerBeforeTheBody(t *testing.T) {
	const limit = 100
	read := make(chan int, 4)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			read <- 0
			WriteError(w, "unauthenticated", "missing token") // a guard: refuses without reading
			return
		}
		b, err := io.ReadAll(r.Body)
		read <- len(b)
		if err != nil {
			WriteDomainError(w, r, err)
			return
		}
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(LargeBodyLimit(limit)(next))
	t.Cleanup(srv.Close)
	send := func(auth bool, body string, declared bool) int {
		var rd io.Reader = strings.NewReader(body)
		if !declared {
			rd = io.NopCloser(rd)
		}
		req, err := http.NewRequest("POST", srv.URL+"/mcp", rd)
		if err != nil {
			t.Fatal(err)
		}
		if auth {
			req.Header.Set("Authorization", "Bearer x")
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	over, exact := strings.Repeat("x", limit+1), strings.Repeat("x", limit)
	for _, tc := range []struct {
		name     string
		auth     bool
		body     string
		declared bool
		want     int
		handler  bool
	}{
		{"anonymous chunked over the limit: the guard answers", false, over, false, 401, true},
		{"authenticated chunked over the limit: 413 while reading", true, over, false, 413, true},
		{"authenticated chunked within the limit", true, exact, false, 200, true},
		{"declared over the limit: 413 before the handler", false, over, true, 413, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := send(tc.auth, tc.body, tc.declared); got != tc.want {
				t.Fatalf("status %d, want %d", got, tc.want)
			}
			select {
			case n := <-read:
				if !tc.handler {
					t.Fatalf("the handler ran (read %d bytes)", n)
				}
				if n > limit {
					t.Fatalf("the handler read %d bytes past the limit", n)
				}
			default:
				if tc.handler {
					t.Fatal("the handler never ran")
				}
			}
		})
	}
}

// A stalled /mcp body is cut by the large-upload deadline, and the deadline is cleared once the
// body is in.
func TestTheLargeUploadLimiterBoundsTheBodyRead(t *testing.T) {
	spy := &deadlineSpy{ResponseRecorder: httptest.NewRecorder()}
	h := LargeBodyLimit(100)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { _, _ = io.ReadAll(r.Body) }))
	for _, length := range []int64{5, -1} {
		spy.calls = nil
		r := httptest.NewRequest("POST", "/mcp", io.NopCloser(strings.NewReader("hello")))
		r.ContentLength = length
		before := time.Now()
		h.ServeHTTP(spy, r)
		if len(spy.calls) < 2 {
			t.Fatalf("length %d: want a deadline and its clearing, got %v", length, spy.calls)
		}
		if d := spy.calls[0].Sub(before); d < largeBodyReadTimeout-time.Second || d > largeBodyReadTimeout+time.Second {
			t.Fatalf("deadline %v from now, want about %v", d, largeBodyReadTimeout)
		}
		if !spy.calls[len(spy.calls)-1].IsZero() {
			t.Fatalf("the deadline was left in place: %v", spy.calls)
		}
	}

	readErr := make(chan error, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
		w.WriteHeader(408)
	})
	srv := httptest.NewServer(newBodyLimit(1000, 150*time.Millisecond, false)(next))
	t.Cleanup(srv.Close)
	rawRequest(t, srv, "POST /mcp HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n")
	select {
	case err := <-readErr:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the body read ended with %v, want a deadline error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the stalled body was never cut")
	}
}
