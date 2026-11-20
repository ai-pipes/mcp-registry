package httpx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

type ctxKey struct{}

// reqState is shared by the middleware of one request: the access log is written by the
// outermost one after the inner ones have run, so the client address they resolve is passed back
// through this pointer.
type reqState struct {
	id     string
	ip     string
	logger *slog.Logger
}

func stateOf(r *http.Request) *reqState {
	s, _ := r.Context().Value(ctxKey{}).(*reqState)
	return s
}

// RequestIDOf returns the request id RequestID assigned, or "" outside that middleware.
func RequestIDOf(r *http.Request) string {
	if s := stateOf(r); s != nil {
		return s.id
	}
	return ""
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; an id is not worth refusing a request.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// peerAddr is the socket peer: RemoteAddr without the port (IPv6 without brackets).
func peerAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "" {
		return "unknown"
	}
	return host
}

// recorder remembers the status of the response without buffering the body. Unwrap, Flush and
// Hijack keep streaming, upgrades and http.ResponseController working for handlers further in.
type recorder struct {
	http.ResponseWriter
	status int
	wrote  bool // a status has been sent (or the connection was taken over)
}

func (r *recorder) WriteHeader(code int) {
	// 1xx (except 101) are interim responses: the final status follows.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		r.ResponseWriter.WriteHeader(code)
		return
	}
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	return r.ResponseWriter.Write(b)
}

// Flush sends buffered data to the client (a streaming response).
func (r *recorder) Flush() {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

// Hijack hands the connection over. net/http sees no status after that, so the access record
// says status 0 unless the handler sent one before.
func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil && !r.wrote {
		r.status, r.wrote = 0, true
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// RequestID assigns each request a UUID (header X-Request-Id on every response, also refusals)
// and writes one access log record per request after the response: message "request", fields
// reqId, method, path (URL path only), status, ms, ip. Headers, query strings and bodies never
// reach the log. A panic in the handlers further in is logged (by type, never the message) and
// answered with a 500 "internal" carrying only the security headers and the request id; when the
// response has already started the connection is aborted instead (a clean end would look like a
// complete response). http.ErrAbortHandler is passed on as such, without an error record.
func RequestID(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			st := &reqState{id: newRequestID(), ip: peerAddr(r), logger: logger}
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, st))
			w.Header().Set("X-Request-Id", st.id)
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()

			defer func() {
				v := recover()
				abort := v == http.ErrAbortHandler
				if v != nil && !abort {
					if err, ok := v.(error); ok {
						logger.Error("unhandled error", "reqId", st.id, logging.ErrAttr(err))
					} else {
						logger.Error("unhandled error", "reqId", st.id, "err", fmt.Sprintf("panic: %T", v))
					}
					if rec.wrote {
						abort = true
					} else {
						dropHandlerHeaders(w.Header())
						WriteError(rec, "internal", "Internal error")
					}
				}
				logger.Info("request",
					"reqId", st.id,
					"method", r.Method,
					"path", r.URL.Path,
					"status", rec.status,
					"ms", int64(time.Since(start).Seconds()*1000+0.5),
					"ip", st.ip,
				)
				if abort {
					panic(http.ErrAbortHandler)
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// dropHandlerHeaders removes everything but the security headers and the request id: a leftover
// Content-Encoding or Cache-Control would make the JSON 500 undecodable or cacheable.
func dropHandlerHeaders(h http.Header) {
	for k := range h {
		if !keptOnPanic[http.CanonicalHeaderKey(k)] {
			delete(h, k)
		}
	}
}

func contextWithState(r *http.Request, s *reqState) context.Context {
	return context.WithValue(r.Context(), ctxKey{}, s)
}
