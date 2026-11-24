package app

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// The limits of the REST contract (packages/contracts/src/servers.ts). They are tighter than the
// domain's, which only a test of the aggregate can reach. Lengths are code points, as zod 4 counts them.
const (
	maxNameLen          = 100
	maxDescriptionLen   = 500
	maxURLLen           = 2048
	minTimeoutMs        = 1000
	maxTimeoutMs        = 600_000
	defaultTimeoutMs    = 120_000
	maxDailyLimit       = 100_000
	maxHeaderLen        = 100
	maxTemplateLen      = 500
	maxClientIDLen      = 500
	maxScopeLen         = 200
	maxScopes           = 50
	maxParamNameLen     = 100
	maxParamValueLen    = 1000
	maxGroupLen         = 200
	maxGroups           = 100
	maxSlugLen          = 64
	maxSecretRequestLen = 4096
)

// ServerFields are the editable fields of a server as the REST contract and the CLI carry them
// (the body of PUT /api/servers/:slug). TimeoutMs nil is the contract's default (120000),
// DailyLimit nil is "no limit": a PUT that omits them resets them.
type ServerFields struct {
	Name        string
	Description string
	URL         string
	Auth        domain.AuthMode
	Access      domain.Access
	TimeoutMs   *int
	DailyLimit  *int
}

// ServerInput is the body of POST /api/servers without its secret: the fields plus the slug, the
// tool-name prefix, which cannot change later.
type ServerInput struct {
	Slug   string
	Fields ServerFields
}

// Issue is one breach of the REST contract: the dotted path of the field as zod reports it
// (empty when the whole body is wrong) and the reason.
type Issue struct {
	Path    string
	Message string
}

// Error is "<path>: <reason>", the message of the invalid_request the web form reads.
func (i Issue) Error() string {
	if i.Path == "" {
		return i.Message
	}
	return i.Path + ": " + i.Message
}

// invalidRequest is the use cases' refusal for the first issue.
func invalidRequest(i Issue) error { return kernel.NewDomainError(CodeInvalidRequest, i.Error()) }

// ValidateServerFields lists every breach of the PUT body contract, in schema order (name,
// description, url, auth, access, timeoutMs, dailyLimit). The use cases report the first one;
// the CLI prints all.
func ValidateServerFields(f ServerFields) []Issue {
	_, issues := normaliseFields(f)
	return issues
}

// ValidateServerInput lists every breach of the POST body contract without the secret, in schema
// order: the fields, then slug. This is the contract of a server file for the CLI too.
func ValidateServerInput(in ServerInput) []Issue {
	_, issues := normaliseFields(in.Fields)
	return append(issues, slugIssues(in.Slug)...)
}

// ValidateCreateRequest is ValidateServerInput plus the secret the REST body may carry (1..4096
// characters, last in schema order). A secret given through the CLI does not go through this:
// the domain judges it.
func ValidateCreateRequest(in ServerInput, secret kernel.Secret) []Issue {
	issues := ValidateServerInput(in)
	if secret.IsSet() {
		issues = append(issues, ValidateSecret(secret.Reveal())...)
	}
	return issues
}

// ValidateSecret checks a secret of a REST body: 1..4096 characters (not trimmed: the domain
// refuses a blank one with invalid_server).
func ValidateSecret(secret string) []Issue {
	if n := runeLen(secret); n < 1 || n > maxSecretRequestLen {
		return []Issue{{Path: "secret", Message: "must be 1-" + strconv.Itoa(maxSecretRequestLen) + " characters"}}
	}
	return []Issue{}
}

func slugIssues(slug string) []Issue {
	if n := runeLen(slug); n < 1 || n > maxSlugLen {
		return []Issue{{Path: "slug", Message: "must be 1-" + strconv.Itoa(maxSlugLen) + " characters"}}
	}
	return nil
}

func lengthIssue(path string, value string, lo, hi int) []Issue {
	if n := runeLen(value); n < lo || n > hi {
		return []Issue{{Path: path, Message: "must be " + strconv.Itoa(lo) + "-" + strconv.Itoa(hi) + " characters"}}
	}
	return nil
}

// cleanURL does what zod's z.url() does and then the contract's http(s) refinement: the input is
// trimmed and must parse (WHATWG), and the stored value is the trimmed text without tabs and
// line breaks; it is at most 2048 characters and starts with http:// or https:// (any case).
func cleanURL(raw string) (string, bool) {
	trimmed := kernel.JSTrim(raw)
	if !domain.ValidHTTPURL(trimmed) {
		return "", false
	}
	cleaned := strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(trimmed)
	lower := strings.ToLower(cleaned)
	if runeLen(cleaned) > maxURLLen {
		return "", false
	}
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return "", false
	}
	return cleaned, true
}

func urlIssue(path string) []Issue {
	return []Issue{{Path: path, Message: "must be an http(s) URL"}}
}

// normaliseFields applies the REST contract to the editable fields: the issues in schema order
// and the fields as the domain should see them (URLs cleaned, lists copied, defaults applied).
func normaliseFields(f ServerFields) (ServerFields, []Issue) {
	issues := []Issue{}
	out := f
	issues = append(issues, lengthIssue("name", f.Name, 1, maxNameLen)...)
	if runeLen(f.Description) > maxDescriptionLen {
		issues = append(issues, Issue{Path: "description", Message: "must be at most " + strconv.Itoa(maxDescriptionLen) + " characters"})
	}
	if cleaned, ok := cleanURL(f.URL); ok {
		out.URL = cleaned
	} else {
		issues = append(issues, urlIssue("url")...)
	}
	auth, authIssues := normaliseAuth(f.Auth)
	out.Auth = auth
	issues = append(issues, authIssues...)
	access, accessIssues := normaliseAccess(f.Access)
	out.Access = access
	issues = append(issues, accessIssues...)

	timeout := defaultTimeoutMs
	if f.TimeoutMs != nil {
		timeout = *f.TimeoutMs
	}
	if timeout < minTimeoutMs || timeout > maxTimeoutMs {
		issues = append(issues, Issue{Path: "timeoutMs", Message: "must be an integer from " + strconv.Itoa(minTimeoutMs) + " to " + strconv.Itoa(maxTimeoutMs)})
	}
	out.TimeoutMs = &timeout
	if f.DailyLimit != nil {
		limit := *f.DailyLimit
		if limit < 1 || limit > maxDailyLimit {
			issues = append(issues, Issue{Path: "dailyLimit", Message: "must be an integer from 1 to " + strconv.Itoa(maxDailyLimit) + " or null"})
		}
		out.DailyLimit = &limit
	}
	return out, issues
}

func headerIssues(a domain.AuthMode) []Issue {
	issues := lengthIssue("auth.header", a.Header, 1, maxHeaderLen)
	return append(issues, lengthIssue("auth.template", a.Template, 1, maxTemplateLen)...)
}

func normaliseAuth(a domain.AuthMode) (domain.AuthMode, []Issue) {
	switch a.Kind {
	case domain.AuthNone:
		return domain.AuthMode{Kind: domain.AuthNone}, nil
	case domain.AuthSharedSecret, domain.AuthPATUser:
		return domain.AuthMode{Kind: a.Kind, Header: a.Header, Template: a.Template}, headerIssues(a)
	case domain.AuthOAuthUser:
		oauth, issues := normaliseOAuth(a.OAuth)
		issues = append(issues, headerIssues(a)...)
		return domain.AuthMode{Kind: a.Kind, Header: a.Header, Template: a.Template, OAuth: oauth}, issues
	default:
		// The shape layer rejects an unknown kind; the domain says "unknown auth mode".
		return a, nil
	}
}

func normaliseOAuth(o *domain.OAuthClientConfig) (*domain.OAuthClientConfig, []Issue) {
	if o == nil {
		return nil, []Issue{{Path: "auth.oauth", Message: "is required"}}
	}
	issues := []Issue{}
	out := *o
	for _, u := range []struct {
		path string
		raw  string
		dst  *string
	}{{"auth.oauth.authorizeUrl", o.AuthorizeURL, &out.AuthorizeURL}, {"auth.oauth.tokenUrl", o.TokenURL, &out.TokenURL}} {
		if cleaned, ok := cleanURL(u.raw); ok {
			*u.dst = cleaned
		} else {
			issues = append(issues, urlIssue(u.path)...)
		}
	}
	out.RevokeURL = nil
	if o.RevokeURL != nil {
		if cleaned, ok := cleanURL(*o.RevokeURL); ok {
			out.RevokeURL = &cleaned
		} else {
			issues = append(issues, urlIssue("auth.oauth.revokeUrl")...)
		}
	}
	issues = append(issues, lengthIssue("auth.oauth.clientId", o.ClientID, 1, maxClientIDLen)...)
	// A failed max skips the checks of the elements (zod).
	if len(o.Scopes) > maxScopes {
		issues = append(issues, Issue{Path: "auth.oauth.scopes", Message: "must have at most " + strconv.Itoa(maxScopes) + " items"})
	} else {
		for i, s := range o.Scopes {
			issues = append(issues, lengthIssue("auth.oauth.scopes."+strconv.Itoa(i), s, 1, maxScopeLen)...)
		}
	}
	out.Scopes = append([]string{}, o.Scopes...)
	for _, p := range o.ExtraAuthorizeParams {
		key := p.Key
		path := "auth.oauth.extraAuthorizeParams." + key
		if runeLen(key) > maxParamNameLen {
			issues = append(issues, Issue{Path: path, Message: "name must be at most " + strconv.Itoa(maxParamNameLen) + " characters"})
		} else if runeLen(p.Value) > maxParamValueLen {
			issues = append(issues, Issue{Path: path, Message: "must be at most " + strconv.Itoa(maxParamValueLen) + " characters"})
		}
	}
	out.ExtraAuthorizeParams = slices.Clone(o.ExtraAuthorizeParams)
	return &out, issues
}

func normaliseAccess(a domain.Access) (domain.Access, []Issue) {
	if a.Kind != domain.AccessGroups {
		return a, nil
	}
	issues := []Issue{}
	if n := len(a.Groups); n < 1 || n > maxGroups {
		issues = append(issues, Issue{Path: "access.groups", Message: "must have 1-" + strconv.Itoa(maxGroups) + " items"})
	} else {
		for i, g := range a.Groups {
			issues = append(issues, lengthIssue("access.groups."+strconv.Itoa(i), g, 1, maxGroupLen)...)
		}
	}
	return domain.Access{Kind: a.Kind, Groups: slices.Clone(a.Groups)}, issues
}

// runeLen is the length zod's string min and max count: code points (zod 4), not UTF-16 units. The
// domain's own limits count UTF-16 units, as the TypeScript domain's `.length` does.
func runeLen(s string) int { return utf8.RuneCountInString(s) }
