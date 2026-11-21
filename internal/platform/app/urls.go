package app

import (
	"net/url"
	"strings"
)

// resolveURL is `new URL(path, base).toString()` for an absolute path: the path replaces any path,
// query and fragment of base, the host is lower-cased and a default port is dropped. (An
// unparsable base cannot happen for a configured PUBLIC_URL; it falls back to plain joining.)
func resolveURL(base, path string) string {
	b, err := url.Parse(base)
	if err != nil || b.Host == "" {
		return strings.TrimSuffix(base, "/") + path
	}
	u := &url.URL{Scheme: strings.ToLower(b.Scheme), User: b.User, Path: path}
	host, port := strings.ToLower(b.Hostname()), b.Port()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" || (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		u.Host = host
	} else {
		u.Host = host + ":" + port
	}
	return u.String()
}
