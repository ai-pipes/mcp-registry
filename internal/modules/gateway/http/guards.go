package http

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// AllowedHosts is the Host allow-list of /mcp (mcpAllowedHosts in server.ts): the ALLOWED_HOSTS
// entries, the host name of PUBLIC_URL, localhost and 127.0.0.1, lower-cased, blanks dropped,
// each once. An IPv6 name is written in brackets, as WHATWG URL's hostname writes it.
func AllowedHosts(publicURL string, extra []string) []string {
	var out []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	for _, h := range extra {
		add(h)
	}
	if u, err := url.Parse(publicURL); err == nil {
		name := u.Hostname()
		if strings.Contains(name, ":") {
			name = "[" + name + "]"
		}
		add(name)
	}
	add("localhost")
	add("127.0.0.1")
	return out
}

// hostnameOf is the host name of a Host header value as `new URL('http://' + host).hostname`
// gives it for the values a client sends: lower case, the port (digits, at most 65535) removed,
// an IPv6 address kept in its brackets. ok is false for a value that is no host.
func hostnameOf(host string) (string, bool) {
	name, port := host, ""
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return "", false
		}
		name, port = host[:end+1], host[end+1:]
		if port != "" {
			if port[0] != ':' {
				return "", false
			}
			port = port[1:]
		}
	} else if i := strings.IndexByte(host, ':'); i >= 0 {
		name, port = host[:i], host[i+1:]
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 || strings.ContainsAny(port, "+-") {
			return "", false
		}
	}
	if name == "" || strings.ContainsAny(name, " \t/\\?#@%<>^|") {
		return "", false
	}
	return strings.ToLower(name), true
}

// hostCheck refuses a request whose Host is not on the allow-list: 403 with a JSON-RPC error body
// (the shape of the reference's transport refusal). The refusal never quotes the header's value.
func hostCheck(allowed []string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host == "" {
				writeTransportError(w, http.StatusForbidden, codeServerError, "Missing Host header")
				return
			}
			if name, ok := hostnameOf(r.Host); !ok || !slices.Contains(allowed, name) {
				writeTransportError(w, http.StatusForbidden, codeServerError, "Invalid Host header")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// originCheck refuses a request that carries an Origin other than PUBLIC_URL's origin, compared
// exactly (a request without the header passes): 403 forbidden "origin not allowed".
func originCheck(expected string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if values, present := r.Header["Origin"]; present && (len(values) != 1 || values[0] != expected) {
				httpx.WriteError(w, "forbidden", "origin not allowed")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
