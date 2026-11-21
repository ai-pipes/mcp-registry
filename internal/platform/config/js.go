package config

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// The TypeScript server reads its environment through JavaScript: String.prototype.trim and
// Number() (via z.coerce.number()). These helpers reproduce them so that every value the old
// server accepted is accepted here too.

// isJSSpace reports the ECMAScript WhiteSpace (TAB, VT, FF, SP, NBSP, ZWNBSP U+FEFF and every
// Unicode Zs character) and LineTerminator (LF, CR, U+2028, U+2029) code points. Go's
// unicode.IsSpace is that set except U+FEFF (added here) and U+0085 NEL (removed: JavaScript
// does not trim it).
func isJSSpace(r rune) bool {
	return (unicode.IsSpace(r) && r != '\u0085') || r == 0xFEFF
}

// jsTrim is String.prototype.trim.
func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

var (
	jsDecimal = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?$`)
	jsRadix   = regexp.MustCompile(`^0([xX][0-9a-fA-F]+|[oO][0-7]+|[bB][01]+)$`)
)

// maxSafeInteger is 2^53-1, JavaScript's Number.MAX_SAFE_INTEGER.
const maxSafeInteger = 1<<53 - 1

// jsNumber is Number(s) for a string: ok is false where JavaScript gives NaN. Whitespace-only is 0;
// unsigned 0x/0o/0b literals are accepted; the result may be infinite (a huge literal): callers
// refuse NaN and Infinity.
func jsNumber(s string) (float64, bool) {
	s = jsTrim(s)
	if s == "" {
		return 0, true
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
