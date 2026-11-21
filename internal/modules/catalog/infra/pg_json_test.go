package infra

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// oauthCfg builds an OAuth config field by field.
func oauthCfg(authorize, tokenEndpoint, client string, scopes []string, revoke *string, extras []domain.AuthorizeParam) *domain.OAuthClientConfig {
	c := &domain.OAuthClientConfig{}
	c.AuthorizeURL = authorize
	c.TokenURL = tokenEndpoint
	c.ClientID = client
	c.Scopes = scopes
	c.RevokeURL = revoke
	c.ExtraAuthorizeParams = extras
	return c
}

// The literals are what the TypeScript server writes (JSON.stringify of its AuthMode / Access):
// the key sets must match exactly, because the two servers read each other's rows.
func TestAuthJSONHasExactlyTheKeysTypeScriptWrites(t *testing.T) {
	revoke := "https://git.example.com/revoke"
	cases := []struct {
		name string
		in   domain.AuthMode
		want string
	}{
		{"none", domain.AuthMode{Kind: domain.AuthNone}, `{"kind":"none"}`},
		{"none ignores stray header", domain.AuthMode{Kind: domain.AuthNone, Header: "X", Template: "{secret}"}, `{"kind":"none"}`},
		{
			"shared_secret",
			domain.AuthMode{Kind: domain.AuthSharedSecret, Header: "X-Key", Template: "{secret}"},
			`{"kind":"shared_secret","header":"X-Key","template":"{secret}"}`,
		},
		{
			"pat_user",
			domain.AuthMode{Kind: domain.AuthPATUser, Header: "Authorization", Template: "Bearer {secret}"},
			`{"kind":"pat_user","header":"Authorization","template":"Bearer {secret}"}`,
		},
		{
			"oauth_user minimal",
			domain.AuthMode{
				Kind: domain.AuthOAuthUser, Header: "Authorization", Template: "Bearer {secret}",
				OAuth: oauthCfg("https://a/authorize", "https://a/token", "cid", nil, nil, nil),
			},
			`{"kind":"oauth_user","oauth":{"authorizeUrl":"https://a/authorize","tokenUrl":"https://a/token","clientId":"cid","scopes":[]},"header":"Authorization","template":"Bearer {secret}"}`,
		},
		{
			"oauth_user full",
			domain.AuthMode{
				Kind: domain.AuthOAuthUser, Header: "Authorization", Template: "Bearer {secret}",
				OAuth: oauthCfg("https://a/authorize", "https://a/token", "cid", []string{"read", "write"}, &revoke,
					[]domain.AuthorizeParam{{Key: "access_type", Value: "offline"}}),
			},
			`{"kind":"oauth_user","oauth":{"authorizeUrl":"https://a/authorize","tokenUrl":"https://a/token","clientId":"cid","scopes":["read","write"],"revokeUrl":"https://git.example.com/revoke","extraAuthorizeParams":{"access_type":"offline"}},"header":"Authorization","template":"Bearer {secret}"}`,
		},
		{
			"oauth_user empty extras are {} (not absent)",
			domain.AuthMode{
				Kind: domain.AuthOAuthUser, Header: "H", Template: "{secret}",
				OAuth: oauthCfg("https://a/a", "https://a/t", "c", []string{}, nil, []domain.AuthorizeParam{}),
			},
			`{"kind":"oauth_user","oauth":{"authorizeUrl":"https://a/a","tokenUrl":"https://a/t","clientId":"c","scopes":[],"extraAuthorizeParams":{}},"header":"H","template":"{secret}"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := encodeAuth(c.in)
			if err != nil {
				t.Fatal(err)
			}
			var g, w any
			if err := json.Unmarshal(got, &g); err != nil {
				t.Fatalf("not JSON: %s", got)
			}
			_ = json.Unmarshal([]byte(c.want), &w)
			if !reflect.DeepEqual(g, w) {
				t.Fatalf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

func TestAccessJSONShapes(t *testing.T) {
	for _, c := range []struct {
		in   domain.Access
		want string
	}{
		{domain.Access{Kind: domain.AccessAll}, `{"kind":"all"}`},
		{domain.Access{Kind: domain.AccessGroups, Groups: []string{"dev", "ops"}}, `{"kind":"groups","groups":["dev","ops"]}`},
	} {
		got, err := encodeAccess(c.in)
		if err != nil || string(got) != c.want {
			t.Fatalf("got %s (%v), want %s", got, err, c.want)
		}
	}
}

func TestExtraParamsAreWrittenInListOrder(t *testing.T) {
	got, err := orderedParams{{Key: "zz", Value: "1"}, {Key: "a", Value: `q"x`}, {Key: "bb", Value: ""}}.MarshalJSON()
	if err != nil || string(got) != `{"zz":"1","a":"q\"x","bb":""}` {
		t.Fatalf("got %s (%v)", got, err)
	}
}

func TestDecodeAuthRoundTripsAndRefusesUnknownShapes(t *testing.T) {
	revoke := "https://x/revoke"
	in := domain.AuthMode{
		Kind: domain.AuthOAuthUser, Header: "Authorization", Template: "Bearer {secret}",
		OAuth: oauthCfg("https://a/a", "https://a/t", "c", []string{"s"}, &revoke, []domain.AuthorizeParam{{Key: "k", Value: "v"}}),
	}
	raw, _ := encodeAuth(in)
	out, err := decodeAuth(raw)
	if err != nil || !reflect.DeepEqual(out, in) {
		t.Fatalf("got %+v (%v)", out, err)
	}
	for _, bad := range []string{
		`{"kind":"bogus"}`, `{"kind":"oauth_user","header":"H","template":"{secret}"}`, `[]`,
		`{"kind":"oauth_user","oauth":{"extraAuthorizeParams":{"a":1}}}`,
	} {
		if _, err := decodeAuth([]byte(bad)); err == nil {
			t.Fatalf("%s must be refused", bad)
		}
	}
	if _, err := decodeAccess([]byte(`{"kind":"nobody"}`)); err == nil {
		t.Fatal("unknown access kind must be refused")
	}
}

func TestDecodeAnnotationsKeepsOnlyBooleans(t *testing.T) {
	a := decodeAnnotations([]byte(`{"readOnlyHint":true,"idempotentHint":false,"title":"x","openWorldHint":"yes"}`))
	if a.ReadOnlyHint == nil || !*a.ReadOnlyHint || a.IdempotentHint == nil || *a.IdempotentHint ||
		a.DestructiveHint != nil || a.OpenWorldHint != nil {
		t.Fatalf("%+v", a)
	}
	if got := decodeAnnotations([]byte(`null`)); got != (domain.ToolAnnotations{}) {
		t.Fatalf("%+v", got)
	}
}
