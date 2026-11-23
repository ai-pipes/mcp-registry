package domain

import "unicode/utf8"

// utf16Len is String.prototype.length of s: UTF-16 code units. A byte that is not valid UTF-8
// counts as one unit (U+FFFD).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += unitsOf(r)
	}
	return n
}

func unitsOf(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// cutUTF16 is s.slice(0, limit) for a Go string: at most limit UTF-16 units. A character that would
// be split (a surrogate pair cut in half) is dropped whole, since a Go string cannot hold half
// of one.
func cutUTF16(s string, limit int) string {
	n := 0
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		n += unitsOf(r)
		if n > limit {
			return s[:i]
		}
		i += w
	}
	return s
}
