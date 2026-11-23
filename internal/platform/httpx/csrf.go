package httpx

import (
	"net/http"
	"net/url"
	"path"
	"strings"
)

// OriginOf is the origin of a URL the way browsers write the Origin header (and WHATWG URL's
// origin): lower-case scheme and host, the port only when it is not the scheme's default, no path.
// ok is false for a URL without a scheme or host. CSRF and the /mcp Origin check use it.
func OriginOf(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, true
}

// CSRF requires mutating requests (POST, PUT, PATCH, DELETE) to /api and /api/** to carry an
// Origin header equal to the origin of publicURL (scheme, host, port; PUBLIC_URL may have a path).
// There is no Bearer exemption: `sk_` tokens open /mcp only. Refusal: 403 csrf. It panics at
// construction when publicURL is not an absolute URL: configuration validates it earlier.
func CSRF(publicURL string) Middleware {
	expected, ok := OriginOf(publicURL)
	if !ok {
		panic("httpx.CSRF: the public URL is not an absolute URL")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := path.Clean("/" + r.URL.Path) // so //api/x and /x/../api/x are covered
			isAPI := p == "/api" || strings.HasPrefix(p, "/api/")
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				if isAPI && r.Header.Get("Origin") != expected {
					WriteError(w, "csrf", "cross-origin request rejected")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
