package httpx

import "net/http"

// The values are pinned by compat/platform_test.go (securityHeaders) and mirror the reference
// server's securityHeaders() (apps/server/src/platform/http/security.ts). The CSP has no inline
// scripts or styles and no hashes; Server and X-Powered-By are never set.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'"

// keptOnPanic are the headers an answer to a panic keeps: the security headers and the request id.
var keptOnPanic = map[string]bool{
	"Content-Security-Policy":      true,
	"Cross-Origin-Opener-Policy":   true,
	"Cross-Origin-Resource-Policy": true,
	"Referrer-Policy":              true,
	"Strict-Transport-Security":    true,
	"X-Content-Type-Options":       true,
	"X-Frame-Options":              true,
	"X-Request-Id":                 true,
}

// SecurityHeaders sets the seven required security headers on every response.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Strict-Transport-Security", "max-age=15552000; includeSubDomains")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "SAMEORIGIN")
			next.ServeHTTP(w, r)
		})
	}
}
