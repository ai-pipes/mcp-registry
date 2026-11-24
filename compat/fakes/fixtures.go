//go:build compat

package fakes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Tool is an upstream tool as tools/list reports it.
type Tool struct {
	Name         string
	Title        string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Annotations  map[string]any
}

// Tools returns simple tools with the given names, each taking an optional text argument.
func Tools(names ...string) []Tool {
	out := make([]Tool, 0, len(names))
	for _, n := range names {
		out = append(out, Tool{
			Name:        n,
			Description: "Test tool " + n + ".",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
			},
		})
	}
	return out
}

// FixtureTools returns the recorded tools/list of a real upstream: "gitea" (61 tools) or
// "youtrack" (23), from compat/fixtures/tools. It panics if the fixture cannot be read,
// since that is a broken checkout and not a test outcome.
func FixtureTools(name string) []Tool {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		panic("fakes: cannot locate the fixtures")
	}
	root := filepath.Join(filepath.Dir(here), "..", "fixtures", "tools")
	raw, err := os.ReadFile(filepath.Join(root, name+".json"))
	if err != nil {
		panic(fmt.Sprintf("fakes: fixture %q: %v", name, err))
	}
	var doc struct {
		Tools []struct {
			Name         string         `json:"name"`
			Title        string         `json:"title"`
			Description  string         `json:"description"`
			InputSchema  map[string]any `json:"inputSchema"`
			OutputSchema map[string]any `json:"outputSchema"`
			Annotations  map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		panic(fmt.Sprintf("fakes: fixture %q: %v", name, err))
	}
	out := make([]Tool, 0, len(doc.Tools))
	for _, t := range doc.Tools {
		out = append(out, Tool(t))
	}
	return out
}

// DisplayName is the display name ServerInput gives a server by default: distinct from its slug, so
// that a text which must carry the name cannot be satisfied by the slug (and the other way round).
func DisplayName(slug string) string { return "Display " + slug }

// ServerOpt changes the body ServerInput builds.
type ServerOpt func(body map[string]any)

// ServerInput builds a POST /api/servers body for an upstream: slug, name = slug, the upstream's
// URL, display name DisplayName(slug), access for everyone and the auth of kind authKind ("none", "shared_secret", "pat_user" or
// "oauth_user"; the last needs WithOAuth). Header auth defaults to "Authorization: Bearer {secret}".
func ServerInput(u *Upstream, slug, authKind string, opts ...ServerOpt) map[string]any {
	auth := map[string]any{"kind": authKind}
	if authKind != "none" {
		auth["header"] = "Authorization"
		auth["template"] = "Bearer {secret}"
	}
	body := map[string]any{
		"slug":        slug,
		"name":        DisplayName(slug),
		"description": "Compat test server " + slug,
		"url":         u.URL,
		"auth":        auth,
		"access":      map[string]any{"kind": "all"},
	}
	for _, o := range opts {
		o(body)
	}
	return body
}

func authOf(body map[string]any) map[string]any {
	a, _ := body["auth"].(map[string]any)
	return a
}

// WithSecret sets the server secret (shared key or OAuth client secret).
func WithSecret(secret string) ServerOpt { return func(b map[string]any) { b["secret"] = secret } }

// WithGroups restricts access to the listed groups.
func WithGroups(groups ...string) ServerOpt {
	return func(b map[string]any) { b["access"] = map[string]any{"kind": "groups", "groups": groups} }
}

// WithHeader sets the header and the template (with {secret}) the secret is sent in.
func WithHeader(header, template string) ServerOpt {
	return func(b map[string]any) {
		authOf(b)["header"] = header
		authOf(b)["template"] = template
	}
}

// WithOAuth points an oauth_user server at the provider: its URLs, client ID and scopes, and its
// client secret as the server secret.
func WithOAuth(p *OAuthProvider, scopes ...string) ServerOpt {
	return func(b map[string]any) {
		if scopes == nil {
			scopes = []string{}
		}
		authOf(b)["oauth"] = map[string]any{
			"authorizeUrl": p.AuthorizeURL,
			"tokenUrl":     p.TokenURL,
			"revokeUrl":    p.RevokeURL,
			"clientId":     p.ClientID,
			"scopes":       scopes,
		}
		b["secret"] = p.ClientSecret
	}
}

// WithoutRevokeURL leaves the revoke URL out of the OAuth settings.
func WithoutRevokeURL() ServerOpt {
	return func(b map[string]any) {
		if oauth, ok := authOf(b)["oauth"].(map[string]any); ok {
			delete(oauth, "revokeUrl")
		}
	}
}

// WithExtraAuthorizeParams adds extraAuthorizeParams to the OAuth settings.
func WithExtraAuthorizeParams(params map[string]string) ServerOpt {
	return func(b map[string]any) {
		if oauth, ok := authOf(b)["oauth"].(map[string]any); ok {
			oauth["extraAuthorizeParams"] = params
		}
	}
}

// WithName sets the display name.
func WithName(name string) ServerOpt { return func(b map[string]any) { b["name"] = name } }

// WithTimeoutMs sets the call timeout.
func WithTimeoutMs(ms int) ServerOpt { return func(b map[string]any) { b["timeoutMs"] = ms } }

// WithDailyLimit sets the daily limit per person.
func WithDailyLimit(n int) ServerOpt { return func(b map[string]any) { b["dailyLimit"] = n } }

// WithURL replaces the upstream URL (a wrong or hostile one).
func WithURL(url string) ServerOpt { return func(b map[string]any) { b["url"] = url } }
