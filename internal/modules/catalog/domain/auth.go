package domain

import (
	"regexp"
	"slices"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// AuthKind is how the gateway authenticates to an upstream.
type AuthKind string

// The kinds of upstream authentication.
const (
	AuthNone         AuthKind = "none"
	AuthSharedSecret AuthKind = "shared_secret"
	AuthOAuthUser    AuthKind = "oauth_user"
	AuthPATUser      AuthKind = "pat_user"
)

// OAuthClientConfig is the OAuth client of an oauth_user server. The client secret is not here:
// it lives sealed in the server's secret.
type OAuthClientConfig struct {
	AuthorizeURL string
	TokenURL     string
	ClientID     string
	Scopes       []string
	// RevokeURL is nil when absent; absent and present always differ (for the secret rule).
	RevokeURL *string
	// ExtraAuthorizeParams are added to the authorize request as is, e.g. access_type=offline for
	// YouTrack Hub, which issues a refresh token only then. They may not override the flow's own
	// parameters. nil = absent; an empty non-nil slice is `{}`. They are ordered, like the
	// properties of a JavaScript object: validation reports the first offending key in this
	// order. Keys are unique: for a duplicated key the FIRST position and the LAST value are kept
	// (JavaScript object semantics), which ParseAuthMode applies; a decoder of a JSON object
	// should produce the same. (The values are strings by type; the TypeScript check "values must
	// be strings" is enforced by the type here.)
	ExtraAuthorizeParams []AuthorizeParam
}

// AuthorizeParam is one extra query parameter of the authorize request.
type AuthorizeParam struct {
	Key   string
	Value string
}

// uniqueParams applies JavaScript object semantics to a key list: the first position and the
// last value of a duplicated key win. nil stays nil and empty stays empty.
func uniqueParams(in []AuthorizeParam) []AuthorizeParam {
	if in == nil {
		return nil
	}
	out := make([]AuthorizeParam, 0, len(in))
	at := make(map[string]int, len(in))
	for _, p := range in {
		if i, ok := at[p.Key]; ok {
			out[i].Value = p.Value
			continue
		}
		at[p.Key] = len(out)
		out = append(out, p)
	}
	return out
}

// AuthMode is a server's authentication to its upstream. Header and Template are used by every
// kind except none; Template contains "{secret}", e.g. "Bearer {secret}". OAuth is set only for
// oauth_user. Treat values as immutable: ParseAuthMode returns a deep copy.
type AuthMode struct {
	Kind     AuthKind
	Header   string
	Template string
	OAuth    *OAuthClientConfig
}

// CredentialSource says where the upstream credential comes from.
type CredentialSource string

// The sources of an upstream credential.
const (
	SourceNone         CredentialSource = "none"
	SourceServerSecret CredentialSource = "server_secret"
	SourcePersonal     CredentialSource = "personal"
)

const secretPlaceholder = "{secret}"

var (
	// RFC 9110 token characters, narrowed to what header names look like in practice.
	headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	paramNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// Parameters the gateway sets itself on the authorize request; an extra one may not replace them.
var reservedAuthorizeParams = []string{
	"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method",
}

func invalidAuth(message string) error { return newError(CodeInvalidAuth, message) }

func checkHeader(header, template string) error {
	if !headerNamePattern.MatchString(header) {
		return invalidAuth("header name may contain letters, digits and dashes")
	}
	if !strings.Contains(template, secretPlaceholder) || hasAny(template, "\r\n\x00") {
		return invalidAuth("header template must contain " + secretPlaceholder + " and be a single line")
	}
	return nil
}

func checkOAuth(o *OAuthClientConfig) error {
	if o == nil || !ValidHTTPURL(o.AuthorizeURL) || !ValidHTTPURL(o.TokenURL) {
		return invalidAuth("authorize and token URLs must be http(s) URLs")
	}
	if o.RevokeURL != nil && !ValidHTTPURL(*o.RevokeURL) {
		return invalidAuth("revoke URL must be an http(s) URL")
	}
	if trim(o.ClientID) == "" {
		return invalidAuth("OAuth client id is required")
	}
	// With `openid` the provider adds an ID token, and its issuer cannot be checked: there is no
	// discovery, only the endpoints the admin entered. Upstream access needs no ID token anyway.
	for _, s := range o.Scopes {
		if asciiLower(s) == "openid" {
			return invalidAuth("the openid scope is not supported for upstream OAuth")
		}
	}
	for _, p := range uniqueParams(o.ExtraAuthorizeParams) { // document order, as TypeScript's Object.entries
		if !paramNamePattern.MatchString(p.Key) || slices.Contains(reservedAuthorizeParams, p.Key) {
			return invalidAuth(`extra authorize parameter "` + p.Key + `" is not allowed`)
		}
	}
	return nil
}

// ParseAuthMode validates an auth mode: the single place that knows what a valid one looks like.
// Order: header and template, then the OAuth config (URLs, revoke URL, client id, scopes, extras).
func ParseAuthMode(m AuthMode) (AuthMode, error) {
	switch m.Kind {
	case AuthNone:
		return AuthMode{Kind: AuthNone}, nil
	case AuthSharedSecret, AuthPATUser:
		if err := checkHeader(m.Header, m.Template); err != nil {
			return AuthMode{}, err
		}
		return AuthMode{Kind: m.Kind, Header: m.Header, Template: m.Template}, nil
	case AuthOAuthUser:
		if err := checkHeader(m.Header, m.Template); err != nil {
			return AuthMode{}, err
		}
		if err := checkOAuth(m.OAuth); err != nil {
			return AuthMode{}, err
		}
		return AuthMode{Kind: m.Kind, Header: m.Header, Template: m.Template, OAuth: m.OAuth.clone()}, nil
	default:
		return AuthMode{}, invalidAuth("unknown auth mode")
	}
}

func (o *OAuthClientConfig) clone() *OAuthClientConfig {
	if o == nil {
		return nil
	}
	c := *o
	c.Scopes = slices.Clone(o.Scopes)
	if o.RevokeURL != nil {
		v := *o.RevokeURL
		c.RevokeURL = &v
	}
	c.ExtraAuthorizeParams = uniqueParams(o.ExtraAuthorizeParams)
	return &c
}

// Clone returns a deep copy.
func (m AuthMode) Clone() AuthMode {
	m.OAuth = m.OAuth.clone()
	return m
}

// HoldsSecret reports whether the server itself holds a secret (the OAuth client secret or the shared key).
func (m AuthMode) HoldsSecret() bool {
	return m.Kind == AuthSharedSecret || m.Kind == AuthOAuthUser
}

// CredentialSource says where the credential comes from: nothing, the server's own secret, or the person's connection (OAuth token or
// PAT). The OAuth client secret is not an upstream credential.
func (m AuthMode) CredentialSource() CredentialSource {
	switch m.Kind {
	case AuthSharedSecret:
		return SourceServerSecret
	case AuthOAuthUser, AuthPATUser:
		return SourcePersonal
	default:
		return SourceNone
	}
}

// HeaderFor is the only place that builds the upstream auth header: the template with every
// "{secret}" replaced by the secret, inserted literally. none sends nothing (an empty map).
func HeaderFor(m AuthMode, secret kernel.Secret) kernel.UpstreamHeaders {
	if m.Kind == AuthNone {
		return kernel.UpstreamHeaders{}
	}
	return kernel.UpstreamHeaders{m.Header: kernel.NewSecret(strings.ReplaceAll(m.Template, secretPlaceholder, secret.Reveal()))}
}
