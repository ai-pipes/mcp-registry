package infra

import (
	"regexp"
	"strings"
)

// challenge is one parsed WWW-Authenticate challenge: the lower-cased scheme and its parameters
// (names lower-cased). A token68 value is recognised but not kept: nothing reads it.
type challenge struct {
	scheme string
	params map[string]string
}

// The parser is a port of oauth4webapi's parseWwwAuthenticateChallenges (the library the
// reference's token endpoint calls go through), including its regular expressions, because a
// challenge on a non-200 answer decides the reported `error` code before the body is looked at.
const (
	tchars      = "[a-zA-Z0-9!#$%&'*+\\-.^_`|~]+"
	tchars68    = "[a-zA-Z0-9\\-._~+/]+={0,2}"
	quotedMatch = `"((?:[^"\\]|\\[\s\S])*)"`
)

var (
	schemeRE        = regexp.MustCompile(`^[,\s]*(` + tchars + `)`)
	quotedParamRE   = regexp.MustCompile(`^[,\s]*(` + tchars + `)\s*=\s*` + quotedMatch + `[,\s]*(.*)`)
	unquotedParamRE = regexp.MustCompile(`^[,\s]*(` + tchars + `)\s*=\s*(` + tchars + `)[,\s]*(.*)`)
	token68ParamRE  = regexp.MustCompile(`^(` + tchars68 + `)(?:$|[,\s])(.*)`)
	spaceRestRE     = regexp.MustCompile(`(?s)^\s+(.*)$`)
	startsSepRE     = regexp.MustCompile(`^[\s,]`)
	unescapeRE      = regexp.MustCompile(`\\([\s\S])`)
)

// parseWWWAuthenticate returns nil for an unparsable (or absent) header, as the library returns
// undefined.
func parseWWWAuthenticate(header string) []challenge {
	var out []challenge
	rest := header
	for rest != "" {
		m := schemeRE.FindStringSubmatch(rest)
		if m == nil || m[1] == "" {
			return nil
		}
		scheme := strings.ToLower(m[1])
		afterScheme := rest[len(m[0]):]
		if afterScheme != "" && !startsSepRE.MatchString(afterScheme) {
			return nil
		}
		sm := spaceRestRE.FindStringSubmatch(afterScheme)
		hasParams := sm != nil
		rest = ""
		if hasParams {
			rest = sm[1]
		}
		params := map[string]string{}
		if hasParams {
			for rest != "" {
				if q := quotedParamRE.FindStringSubmatch(rest); q != nil {
					params[strings.ToLower(q[1])] = unescapeRE.ReplaceAllString(q[2], "$1")
					rest = q[3]
					continue
				}
				if u := unquotedParamRE.FindStringSubmatch(rest); u != nil {
					params[strings.ToLower(u[1])] = u[2]
					rest = u[3]
					continue
				}
				if t := token68ParamRE.FindStringSubmatch(rest); t != nil {
					if len(params) > 0 {
						break
					}
					rest = t[2]
					break
				}
				return nil
			}
		} else {
			rest = afterScheme
		}
		out = append(out, challenge{scheme: scheme, params: params})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
