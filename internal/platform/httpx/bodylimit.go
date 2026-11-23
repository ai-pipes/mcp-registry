package httpx

import (
	"bytes"
	"io"
	"net/http"
	"time"
)

const (
	defaultBodyLimit = 1 << 20 // 1 MiB
	// bodyReadTimeout bounds how long a client may take to deliver a request body (ordinary
	// routes only): a body trickled byte by byte would otherwise hold a goroutine and a connection
	// for as long as the client likes.
	bodyReadTimeout = 30 * time.Second
	// largeBodyReadTimeout bounds the delivery of a large upload (/mcp): the 300 seconds of
	// Node's default server.requestTimeout, which bounds the reference's whole request.
	largeBodyReadTimeout = 300 * time.Second
)

// BodyLimit caps request bodies at limit bytes (a value below 1 means the default, 1 MiB) and
// bounds the time a client may take to send one (30 seconds, for routes that do not carry large
// uploads; LargeBodyLimit has no deadline). It follows hono's bodyLimit of the TypeScript server:
//
//   - GET, HEAD and TRACE are not looked at (hono sees no body for them), whatever their
//     Content-Length says.
//   - With a Content-Length above the cap the request is refused with 413 limit_exceeded before
//     the body is read, and the connection is closed afterwards.
//   - Without a declared length (chunked) the body is read up to limit+1 bytes before any later
//     middleware runs, so an oversized one is a 413 before the origin check or the router could
//     answer 403 or 404; within the cap the handlers get the same bytes back, still without a
//     declared length.
//   - A declared length within the cap is not read ahead: the body is cut at the cap while it is
//     read, and the read then fails with *http.MaxBytesError, which DecodeJSON and
//     WriteDomainError turn into the same 413 answer.
//
// /mcp is not in this chain: it has its own, larger limit.
func BodyLimit(limit int64) Middleware { return newBodyLimit(limit, bodyReadTimeout, true) }

// LargeBodyLimit is BodyLimit for routes that carry large uploads (/mcp): the delivery may take
// up to largeBodyReadTimeout, and a body without a declared length is not read ahead (an
// anonymous client must not make the server buffer up to the limit before the route's own
// guards answer): it is cut at the limit while the handler reads it, which gives the handler the
// same 413 path as a declared length. A declared length over the limit is still refused first.
func LargeBodyLimit(limit int64) Middleware {
	return newBodyLimit(limit, largeBodyReadTimeout, false)
}

// newBodyLimit is BodyLimit with the read deadline (0: none; tests pass a short one) and the
// read-ahead of a body without a declared length as parameters.
func newBodyLimit(limit int64, readTimeout time.Duration, readAhead bool) Middleware {
	if limit < 1 {
		limit = defaultBodyLimit
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ignoresBody(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			if r.ContentLength > limit {
				refuseOversized(w)
				return
			}
			if r.Body == nil || r.Body == http.NoBody {
				next.ServeHTTP(w, r)
				return
			}
			relax := func() {}
			if readTimeout > 0 {
				rc := http.NewResponseController(w)
				if err := rc.SetReadDeadline(time.Now().Add(readTimeout)); err == nil {
					// Once the body is in, the deadline must go: left in place it would fire on
					// net/http's background read and cancel the request while a slow handler works.
					relax = func() { _ = rc.SetReadDeadline(time.Time{}) }
					defer relax()
				}
				// A writer without deadline support (http.ErrNotSupported) is served without one.
			}
			if r.ContentLength < 0 && readAhead {
				buffered, readErr := io.ReadAll(io.LimitReader(r.Body, limit+1))
				relax()
				if int64(len(buffered)) > limit {
					refuseOversized(w)
					return
				}
				r.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(buffered), errReader{readErr}), closer: r.Body}
			} else {
				r.Body = &clearingBody{ReadCloser: http.MaxBytesReader(w, r.Body, limit), clear: relax}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ignoresBody: the methods for which hono's bodyLimit sees no body and does nothing.
func ignoresBody(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodTrace
}

func refuseOversized(w http.ResponseWriter) {
	// Not drained: the connection is closed instead of reading an oversized body.
	w.Header().Set("Connection", "close")
	WriteError(w, "limit_exceeded", errBodyTooLarge)
}

// replayBody serves the bytes read ahead, then the outcome of that read (nil means the body ended).
type replayBody struct {
	io.Reader
	closer io.Closer
}

func (b *replayBody) Close() error { return b.closer.Close() }

// errReader reports err (io.EOF when nil) once the bytes before it are consumed.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) {
	if e.err == nil {
		return 0, io.EOF
	}
	return 0, e.err
}

// clearingBody calls its function when the body ends or fails.
type clearingBody struct {
	io.ReadCloser
	clear func()
}

func (b *clearingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.clear()
	}
	return n, err
}
