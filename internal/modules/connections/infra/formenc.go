package infra

import (
	"encoding/base64"
	"strings"
)

const upperHex = "0123456789ABCDEF"

// formEncode is the WHATWG application/x-www-form-urlencoded serialiser of one value, the one
// `new URLSearchParams({v: s}).toString()` applies: space becomes `+`, the bytes `*-._` and ASCII
// letters and digits stay, every other byte of the UTF-8 text becomes %XX with upper-case hex.
// It differs from net/url's QueryEscape for exactly two characters: `*` stays and `~` is encoded.
// A byte sequence that is not UTF-8 is read as U+FFFD (the reference's strings are USVStrings).
func formEncode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s { // ranging over a string turns each invalid byte into U+FFFD
		for _, c := range []byte(string(r)) {
			switch {
			case c == ' ':
				b.WriteByte('+')
			case c == '*' || c == '-' || c == '.' || c == '_' ||
				'0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
				b.WriteByte(c)
			default:
				b.WriteByte('%')
				b.WriteByte(upperHex[c>>4])
				b.WriteByte(upperHex[c&15])
			}
		}
	}
	return b.String()
}

// formDecode is the inverse: `+` is a space, %XX is a byte; a `%` not followed by two hex digits
// stays as it is (the WHATWG parser's rule). Used for the query a configured authorize URL
// already carries.
func formDecode(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '+':
			b = append(b, ' ')
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
		default:
			b = append(b, c)
		}
	}
	return strings.ToValidUTF8(string(b), "�")
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// basicHeader is `Basic base64(formEncode(id) ":" formEncode(secret))`: RFC 6749 section 2.3.1
// (each part form-encoded before Basic), byte for byte what the reference sends.
func basicHeader(id, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(formEncode(id)+":"+formEncode(secret)))
}

// formBody joins key/value pairs in the given order as a form body.
func formBody(pairs [][2]string) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(formEncode(p[0]))
		b.WriteByte('=')
		b.WriteString(formEncode(p[1]))
	}
	return b.String()
}
