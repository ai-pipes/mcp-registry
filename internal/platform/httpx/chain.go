// Package httpx is the HTTP core of the platform: the middleware chain (request id and access
// log, security headers, body limit, client address, CSRF), the JSON error format, request body
// decoding, the health check, the not-found handler and static files with the SPA fallback.
// It registers no routes: the composition root assembles the chain and the mux.
package httpx

import "net/http"

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain wraps h in m. The first middleware is the outermost: it sees the request first and the
// response last, so Chain(h, a, b) runs a, then b, then h.
func Chain(h http.Handler, m ...Middleware) http.Handler {
	for i := len(m) - 1; i >= 0; i-- {
		h = m[i](h)
	}
	return h
}
