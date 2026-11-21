package domain

import (
	"strings"
	"time"
	"unicode/utf16"
)

const (
	// TokenMark starts every API token.
	TokenMark = "sk_"
	// TokenPrefixLength is how much of a token is safe to show in lists.
	TokenPrefixLength = 10
	// MaxTokenLength is the longest string that is looked up as a token.
	MaxTokenLength = 100
	// LastUsedGranularity is how often at most lastUsedAt is refreshed.
	LastUsedGranularity = time.Minute
)

// utf16Len is the length JavaScript's String.length reports.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// LooksLikeToken is a cheap shape check before a token reaches the database.
func LooksLikeToken(token string) bool {
	return utf16Len(token) <= MaxTokenLength && strings.HasPrefix(token, TokenMark)
}

// NewTokenValue marks a random part as a token.
func NewTokenValue(randomPart string) string { return TokenMark + randomPart }

// TokenPrefix is the first 10 UTF-16 code units of a token.
func TokenPrefix(token string) string {
	units := utf16.Encode([]rune(token))
	if len(units) <= TokenPrefixLength {
		return token
	}
	return string(utf16.Decode(units[:TokenPrefixLength]))
}

// ShouldTouchLastUsed reports whether lastUsedAt is refreshed: when never set or at least a minute old, so reads
// do not become writes.
func ShouldTouchLastUsed(lastUsedAt *time.Time, now time.Time) bool {
	return lastUsedAt == nil || now.Sub(*lastUsedAt) >= LastUsedGranularity
}
