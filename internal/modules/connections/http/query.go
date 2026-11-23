package http

import (
	"strings"
	"unicode/utf8"
)

// queryParam is the value of the first parameter named key in a raw query string, read the way the
// reference's web framework reads it (Hono's getQueryParam), which differs from net/url: pairs are
// split on & only (a ; is data), a pair splits at its first =, a pair without = has the value "",
// an empty name is skipped, + is a space, percent-decoding is lenient, and the first occurrence of
// a name wins (an empty value included). A fragment ends the query. found is false when no
// parameter has that name.
func queryParam(rawQuery, key string) (value string, found bool) {
	if i := strings.IndexByte(rawQuery, '#'); i >= 0 {
		rawQuery = rawQuery[:i]
	}
	for rawQuery != "" {
		pair := rawQuery
		if i := strings.IndexByte(rawQuery, '&'); i >= 0 {
			pair, rawQuery = rawQuery[:i], rawQuery[i+1:]
		} else {
			rawQuery = ""
		}
		name, val := pair, ""
		if i := strings.IndexByte(pair, '='); i >= 0 {
			name, val = pair[:i], pair[i+1:]
		}
		if name = lenientDecode(name); name == "" || name != key {
			continue
		}
		return lenientDecode(val), true
	}
	return "", false
}

// lenientDecode is `+` to space, then decodeURIComponent applied to every maximal run of %XX
// escapes: a run that is not valid UTF-8 stays as written, and so does a % without two hex digits.
func lenientDecode(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if !strings.Contains(s, "%") {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		if !escapeAt(s, i) {
			out.WriteByte(s[i])
			i++
			continue
		}
		j := i
		var raw []byte
		for escapeAt(s, j) {
			raw = append(raw, unhex(s[j+1])<<4|unhex(s[j+2]))
			j += 3
		}
		if utf8.Valid(raw) {
			out.Write(raw)
		} else {
			out.WriteString(s[i:j])
		}
		i = j
	}
	return out.String()
}

func escapeAt(s string, i int) bool {
	return i+2 < len(s) && s[i] == '%' && isHex(s[i+1]) && isHex(s[i+2])
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}
