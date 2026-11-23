package domain

import (
	"strings"
	"unicode/utf8"
)

// OAuthEndpoints are the OAuth parts of a server's configuration that decide where a token or code
// is sent. The revoke URL and scopes are deliberately not here: they are not part of the fingerprint.
type OAuthEndpoints struct {
	AuthorizeURL string
	TokenURL     string
	ClientID     string
}

// ServerConfigOrigin is what a person's credential was issued for. OAuth is nil unless the server
// uses oauth_user (the application layer maps the catalog's call target to this).
type ServerConfigOrigin struct {
	URL      string
	AuthKind string
	OAuth    *OAuthEndpoints
}

// ServerFingerprint is a canonical string of the configuration a connection is bound to: the
// upstream URL, the auth kind and, for OAuth, the authorize and token URLs and the client id. Every
// one of them is a place the person's token or code is sent; when an admin changes any, the token
// must not follow, so the connection expires. It is stored (issued_for) and compared as text, and
// the database is shared with the TypeScript server, so the bytes are exactly those of
//
//	JSON.stringify([url, authKind, authorizeUrl ?? null, tokenUrl ?? null, clientId ?? null])
//
// (a five-element array, not an object; no spaces; `<`, `>`, `&` and `/` not escaped; U+2028 and
// U+2029 raw; control characters as \b \f \n \r \t or lowercase \u00xx).
func ServerFingerprint(cfg ServerConfigOrigin) string {
	var b strings.Builder
	b.WriteByte('[')
	writeJSString(&b, cfg.URL)
	b.WriteByte(',')
	writeJSString(&b, cfg.AuthKind)
	if cfg.OAuth == nil {
		b.WriteString(",null,null,null")
	} else {
		for _, v := range []string{cfg.OAuth.AuthorizeURL, cfg.OAuth.TokenURL, cfg.OAuth.ClientID} {
			b.WriteByte(',')
			writeJSString(&b, v)
		}
	}
	b.WriteByte(']')
	return b.String()
}

// writeJSString writes s as JSON.stringify writes a string. Go strings cannot hold lone
// surrogates; an invalid UTF-8 byte is what a decoder would turn into U+FFFD.
func writeJSString(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s { // ranging yields U+FFFD for an invalid byte
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hex[r>>4])
			b.WriteByte(hex[r&0xf])
		default:
			var buf [utf8.UTFMax]byte
			n := utf8.EncodeRune(buf[:], r)
			b.Write(buf[:n])
		}
	}
	b.WriteByte('"')
}
