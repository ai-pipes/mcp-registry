package httpx

import (
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
	id string
	ip string
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

// recorder remembers the status of the response without buffering the body. Unwrap (and Flush)
// keep streaming and http.ResponseController working for handlers further in.
type recorder struct {
	http.ResponseWriter
	status int
	wrote  bool
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

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// RequestID assigns each request a UUID (header X-Request-Id on every response, also refusals),
// turns a panic in the handlers further in into a 500 "internal" answer, and writes one access
// log record per request after the response: message "request", fields reqId, method, path
// (URL path only), status, ms, ip. Headers, query strings and bodies never reach the log.
func RequestID(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			st := &reqState{id: newRequestID(), ip: peerAddr(r)}
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, st))
			w.Header().Set("X-Request-Id", st.id)
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()

			defer func() {
				v := recover()
				if v != nil && v != http.ErrAbortHandler {
					// The message may carry user input or secrets: log only what was thrown, by type.
					if err, ok := v.(error); ok {
						logger.Error("unhandled error", "reqId", st.id, logging.ErrAttr(err))
					} else {
						logger.Error("unhandled error", "reqId", st.id, "err", fmt.Sprintf("panic: %T", v))
					}
					if !rec.wrote {
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
				if v == http.ErrAbortHandler {
					panic(v)
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

func contextWithState(r *http.Request, s *reqState) context.Context {
	return context.WithValue(r.Context(), ctxKey{}, s)
}
