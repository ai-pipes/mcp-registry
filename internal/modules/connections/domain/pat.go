package domain

import (
	"strings"
	"unicode/utf16"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

const maxPATLength = 4096

// ParsePAT validates a personal access token as pasted by a person: trimmed the way JavaScript's
// String.prototype.trim does, not empty, at most 4096 UTF-16 code units, no CR, LF or NUL. It
// returns the trimmed token or an invalid_pat error.
func ParsePAT(raw string) (string, error) {
	token := kernel.JSTrim(raw)
	if token == "" || len(utf16.Encode([]rune(token))) > maxPATLength || strings.ContainsAny(token, "\r\n\x00") {
		return "", errInvalidPAT()
	}
	return token, nil
}
