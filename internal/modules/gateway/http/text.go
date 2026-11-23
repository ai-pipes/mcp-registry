package http

import (
	"strings"
	"unicode"
)

// Instructions is what an agent is told about the server (instructions.ts).
const Instructions = "This server is a gateway to the user's services (git hosting, issue tracker and others). " +
	"When a task needs one of them, call search_tools with a short description of the action, then describe_tool " +
	"for the chosen tool, then call_tool. If nothing fits, continue without them. If a tool needs a connection, " +
	"tell the user the link from the error."

const (
	maxDescription = 200
	ellipsis       = '…'
)

// isJSSpace is JavaScript's \s: the ECMAScript WhiteSpace and LineTerminator code points, which is
// Go's unicode.IsSpace without U+0085 and with U+FEFF.
func isJSSpace(r rune) bool { return (unicode.IsSpace(r) && r != '\u0085') || r == 0xFEFF }

// isTrailingJunk is the class [\s,.;:!?—–-] of the reference's cut.
func isTrailingJunk(r rune) bool { return isJSSpace(r) || strings.ContainsRune(",.;:!?—–-", r) }

// ShortDescription is a tool description as search_tools lists it (text.ts): every run of
// whitespace one space, trimmed; up to 200 code points kept as they are; a longer one cut to 199
// code points, back to the last word boundary unless the cut falls on one, without trailing
// whitespace and , . ; : ! ? — – -, then an ellipsis.
func ShortDescription(description string) string {
	var b strings.Builder
	space := false
	for _, r := range description {
		if isJSSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	chars := []rune(strings.Trim(b.String(), " "))
	if len(chars) <= maxDescription {
		return string(chars)
	}
	room := maxDescription - 1
	cut := chars[:room]
	if chars[room] != ' ' {
		for i := len(cut) - 1; i > 0; i-- {
			if cut[i] == ' ' {
				cut = cut[:i]
				break
			}
		}
	}
	trimmed := cut
	for len(trimmed) > 0 && isTrailingJunk(trimmed[len(trimmed)-1]) {
		trimmed = trimmed[:len(trimmed)-1]
	}
	if len(trimmed) == 0 {
		trimmed = cut
	}
	return string(trimmed) + string(ellipsis)
}
