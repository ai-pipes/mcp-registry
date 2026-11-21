package kernel

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Pure reproductions of the two JavaScript string functions the TypeScript server's input
// handling goes through: String.prototype.trim and Number(string) (zod's z.coerce.number()).

// isJSSpace reports the ECMAScript WhiteSpace (TAB, VT, FF, SP, NBSP, ZWNBSP U+FEFF and every
// Unicode Zs character) and LineTerminator (LF, CR, U+2028, U+2029) code points. Go's
// unicode.IsSpace is that set except U+FEFF (added here) and U+0085 NEL (removed: JavaScript
// does not trim it).
func isJSSpace(r rune) bool {
	return (unicode.IsSpace(r) && r != '\u0085') || r == 0xFEFF
}

// JSTrim is String.prototype.trim.
func JSTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

var (
	jsDecimal = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?$`)
	jsRadix   = regexp.MustCompile(`^0([xX][0-9a-fA-F]+|[oO][0-7]+|[bB][01]+)$`)
)

// JSNumber is Number(s) for a string: ok is false where JavaScript gives NaN. Whitespace-only is
// 0; unsigned 0x/0o/0b literals are accepted; "Infinity" (optionally signed, case-sensitive) is
// +-Inf; the result may be infinite (also a huge literal):
// callers refuse NaN and Infinity.
func JSNumber(s string) (float64, bool) {
	s = JSTrim(s)
	if s == "" {
		return 0, true
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	if jsRadix.MatchString(s) {
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[s[1]|0x20]
		n, _ := new(big.Int).SetString(s[2:], base)
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, true
	}
	if !jsDecimal.MatchString(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}
