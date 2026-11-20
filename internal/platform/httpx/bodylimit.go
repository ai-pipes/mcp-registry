package httpx

import "net/http"

const defaultBodyLimit = 1 << 20 // 1 MiB

// BodyLimit caps request bodies at limit bytes (a value below 1 means the default, 1 MiB). With a
// Content-Length above the cap the request is refused with 413 limit_exceeded before the body is
// read and the connection is closed afterwards. Without a length the body is cut at the cap while
// it is read: the read then fails with *http.MaxBytesError, which DecodeJSON and WriteDomainError
// turn into the same 413 answer (a handler that reads the body itself passes that error to
// WriteDomainError). The caller leaves /mcp out of the chain: it has its own, larger limit.
func BodyLimit(limit int64) Middleware {
	if limit < 1 {
		limit = defaultBodyLimit
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				// Not drained: the connection is closed instead of reading an oversized body.
				w.Header().Set("Connection", "close")
				WriteError(w, "limit_exceeded", errBodyTooLarge)
				return
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}
