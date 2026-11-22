package domain

import (
	"regexp"
	"strings"
)

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// ParseSlug checks a server's slug: kebab-case, 2-32 characters, the prefix of its tool names.
func ParseSlug(raw string) (string, error) {
	if !slugPattern.MatchString(raw) {
		return "", newError(CodeInvalidSlug,
			"server slug must be 2-32 characters: lowercase letters, digits and dashes, starting with a letter")
	}
	return raw, nil
}

// ParseToolRef splits the agent-facing name "<server>.<tool>" at the FIRST dot (upstream tool
// names may contain dots).
func ParseToolRef(raw string) (server, tool string, err error) {
	dot := strings.IndexByte(raw, '.')
	if dot < 1 || dot == len(raw)-1 {
		return "", "", newError(CodeInvalidToolRef, "tool name must look like <server>.<tool>")
	}
	server, tool = raw[:dot], raw[dot+1:]
	if _, err := ParseSlug(server); err != nil {
		return "", "", newError(CodeInvalidToolRef, "tool name must look like <server>.<tool>")
	}
	return server, tool, nil
}

// FormatToolRef is the inverse of ParseToolRef.
func FormatToolRef(server, tool string) string { return server + "." + tool }
