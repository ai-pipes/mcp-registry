package app

import (
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// hostToASCII is the host conversion of the WHATWG URL standard: UTS 46 mapping, non-transitional,
// no STD3 or length checks.
var hostToASCII = idna.New(idna.MapForLookup(), idna.Transitional(false), idna.StrictDomainName(false), idna.VerifyDNSLength(false))

// resolveURL is `new URL(path, base).toString()` for an absolute path: the path replaces any path,
// query and fragment of base, the host is lower-cased and a default port is dropped. (An
// unparsable base cannot happen for a configured PUBLIC_URL; it falls back to plain joining.)
func resolveURL(base, path string) string {
	b, err := url.Parse(base)
	if err != nil || b.Host == "" {
		return strings.TrimSuffix(base, "/") + path
	}
	u := &url.URL{Scheme: strings.ToLower(b.Scheme), User: b.User, Path: path}
	host := strings.ToLower(b.Hostname())
	if ascii, err := hostToASCII.ToASCII(host); err == nil {
		host = ascii
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	// The port is a number: "0443" is the default port of https. The configuration accepts only
	// numbers up to 65535, so a failure here cannot happen (the text is kept then).
	port := b.Port()
	if n, err := strconv.Atoi(port); err == nil {
		if (u.Scheme == "http" && n == 80) || (u.Scheme == "https" && n == 443) {
			port = ""
		} else {
			port = strconv.Itoa(n)
		}
	}
	if port == "" {
		u.Host = host
	} else {
		u.Host = host + ":" + port
	}
	return u.String()
}
