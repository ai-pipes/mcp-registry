package domain

import (
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValidHTTPURL reports whether JavaScript's `new URL(raw)` parses raw and gives the protocol
// http: or https:, which is what the TypeScript domain accepts for upstream and OAuth URLs.
// net/url is not the WHATWG parser (it accepts and rejects other inputs), so the part of the
// WHATWG algorithm that decides acceptance is written out here: input trimming, the scheme,
// the authority (credentials, host, port) and the host rules (IPv6, IPv4 numbers, forbidden
// host code points). Path, query and fragment never make a URL invalid and are not looked at.
//
// Accepted difference: the IDNA (UTS #46) step of host parsing is not run, because the domain and
// the kernel are standard-library only. It matters for non-ASCII hosts, for ASCII "xn--" labels
// (the reference decodes and validates punycode) and for percent-encoded bytes of the host (the
// reference maps the decoded text); the reference would reject some of these inputs, and this
// check accepts them. A host whose percent-decoding is not valid UTF-8 is rejected, as the
// reference does. The stored URL is never rewritten; this only decides validity.
func ValidHTTPURL(raw string) bool {
	// Leading and trailing C0 controls and spaces are stripped, tab and newlines removed anywhere.
	s := strings.TrimFunc(raw, func(r rune) bool { return r <= 0x20 })
	s = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(s)

	colon := strings.IndexByte(s, ':')
	if colon < 1 || !isASCIIAlpha(s[0]) {
		return false // no scheme: there is no base URL to resolve against
	}
	for i := 1; i < colon; i++ {
		c := s[i]
		if !isASCIIAlpha(c) && (c < '0' || c > '9') && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	if scheme := asciiLower(s[:colon]); scheme != "http" && scheme != "https" {
		return false
	}

	// Special schemes skip any number of slashes (and backslashes) before the authority.
	rest := strings.TrimLeft(s[colon+1:], `/\`)
	end := strings.IndexAny(rest, `/\?#`)
	if end < 0 {
		end = len(rest)
	}
	authority := rest[:end]
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	host, port, ok := splitHostPort(authority)
	if !ok || host == "" || !validPort(port) {
		return false
	}
	return validHost(host)
}

func isASCIIAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// splitHostPort splits "host:port"; an IPv6 literal is delimited by brackets.
func splitHostPort(a string) (host, port string, ok bool) {
	if strings.HasPrefix(a, "[") {
		closing := strings.IndexByte(a, ']')
		if closing < 0 {
			return "", "", false
		}
		host, rest := a[:closing+1], a[closing+1:]
		switch {
		case rest == "":
			return host, "", true
		case rest[0] == ':':
			return host, rest[1:], true
		default:
			return "", "", false
		}
	}
	if i := strings.IndexByte(a, ':'); i >= 0 {
		return a[:i], a[i+1:], true
	}
	return a, "", true
}

func validPort(p string) bool {
	if p == "" {
		return true
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(strings.TrimLeft(p, "0")+"0", 10, 64) // the extra 0 keeps an all-zero port parseable
	if err != nil {
		return false
	}
	return n/10 <= 65535
}

func validHost(host string) bool {
	if strings.HasPrefix(host, "[") {
		inner := host[1 : len(host)-1]
		if strings.ContainsAny(inner, "%") {
			return false
		}
		a, err := netip.ParseAddr(inner)
		return err == nil && a.Is6()
	}
	decoded := percentDecode(host)
	if decoded == "" || !utf8.ValidString(decoded) {
		return false
	}
	for i := 0; i < len(decoded); i++ {
		c := decoded[i]
		if c <= 0x20 || c == 0x7f || strings.IndexByte("#%/:<>?@[\\]^|", c) >= 0 {
			return false
		}
	}
	if endsInNumber(decoded) {
		return validIPv4(decoded)
	}
	return true
}

func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			n, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			b.WriteByte(byte(n))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// endsInNumber is the WHATWG "ends in a number checker": the last non-empty label is all
// digits or a 0x hex number, so the host must then be a valid IPv4 address.
func endsInNumber(host string) bool {
	parts := strings.Split(host, ".")
	if parts[len(parts)-1] == "" {
		if len(parts) == 1 {
			return false
		}
		parts = parts[:len(parts)-1]
	}
	last := parts[len(parts)-1]
	if last != "" && strings.Trim(last, "0123456789") == "" {
		return true
	}
	if len(last) >= 2 && last[0] == '0' && (last[1] == 'x' || last[1] == 'X') {
		return strings.Trim(last[2:], "0123456789abcdefABCDEF") == ""
	}
	return false
}

// validIPv4 is the WHATWG IPv4 parser's accept/reject decision (decimal, octal, hex parts).
func validIPv4(host string) bool {
	parts := strings.Split(host, ".")
	if parts[len(parts)-1] == "" && len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 4 {
		return false
	}
	var nums []uint64
	for _, p := range parts {
		n, ok := parseIPv4Number(p)
		if !ok {
			return false
		}
		nums = append(nums, n)
	}
	for _, n := range nums[:len(nums)-1] {
		if n > 255 {
			return false
		}
	}
	limit := uint64(1) << (8 * (5 - uint(len(nums))))
	return nums[len(nums)-1] < limit
}

func parseIPv4Number(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	base := 10
	switch {
	case len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X'):
		s, base = s[2:], 16
	case len(s) >= 2 && s[0] == '0':
		s, base = s[1:], 8
	}
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		// A value too large for 64 bits is still a syntactically valid number, and too big.
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return 1 << 62, true
		}
		return 0, false
	}
	return n, true
}
