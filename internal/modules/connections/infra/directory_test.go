package infra

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
)

type fakeCatalog struct {
	views    []catalog.MemberServerView
	targets  map[string]*catalog.ServerCallTarget
	secrets  map[string]string
	moved    map[string]bool
	secretEr error
	targetOf []catalog.ServerCallTarget
	calls    []string
	listErr  error
	asked    app.Principal
}

func (f *fakeCatalog) ListServersFor(_ context.Context, p catalog.Principal) ([]catalog.MemberServerView, error) {
	f.calls = append(f.calls, "list")
	f.asked = p
	return f.views, f.listErr
}

func (f *fakeCatalog) GetServerForUpstream(_ context.Context, slug string) (*catalog.ServerCallTarget, error) {
	f.calls = append(f.calls, "target:"+slug)
	return f.targets[slug], nil
}

func (f *fakeCatalog) GetServerSecretFor(_ context.Context, t catalog.ServerCallTarget) (string, catalog.SecretLookup, error) {
	f.targetOf = append(f.targetOf, t)
	if f.secretEr != nil {
		return "", "", f.secretEr
	}
	if f.moved[t.Slug] {
		return "", catalog.SecretMoved, nil
	}
	v, ok := f.secrets[t.Slug]
	if !ok {
		return "", catalog.SecretNotSet, nil
	}
	return v, catalog.SecretFound, nil
}

func oauthTarget(slug string) *catalog.ServerCallTarget {
	rev := "https://idp/revoke"
	return &catalog.ServerCallTarget{
		Slug: slug, Name: strings.ToUpper(slug), URL: "https://Up.example/mcp?x=1",
		Auth: catalog.AuthMode{Kind: catalog.AuthOAuthUser, OAuth: &catalog.OAuthClientConfig{ //nolint:gosec // G101: test data, no real credential (URLs and OAuth parameter names)
			AuthorizeURL: "https://idp/a", TokenURL: "https://idp/t", ClientID: "cid", Scopes: []string{"s1", "s2"}, RevokeURL: &rev,
			ExtraAuthorizeParams: []catalog.AuthorizeParam{{Key: "z", Value: "1"}, {Key: "a", Value: "2"}},
		}},
	}
}

func TestConfigOAuthOnlyForOAuthUserAndURLAsStored(t *testing.T) {
	pat := &catalog.ServerCallTarget{Slug: "yt", URL: "https://yt/mcp", Auth: catalog.AuthMode{Kind: catalog.AuthPATUser}}
	shared := &catalog.ServerCallTarget{Slug: "sh", URL: "https://sh/mcp", Auth: catalog.AuthMode{Kind: catalog.AuthSharedSecret, OAuth: oauthTarget("x").Auth.OAuth}}
	f := &fakeCatalog{targets: map[string]*catalog.ServerCallTarget{"gitea": oauthTarget("gitea"), "yt": pat, "sh": shared}}
	d := NewServerDirectory(f)

	cfg, err := d.Config(context.Background(), "gitea")
	if err != nil || cfg == nil {
		t.Fatal(cfg, err)
	}
	if cfg.URL != "https://Up.example/mcp?x=1" || cfg.AuthKind != catalog.AuthOAuthUser || cfg.Slug != "gitea" {
		t.Errorf("%+v", cfg)
	}
	if cfg.OAuth == nil || cfg.OAuth.ClientID != "cid" || fmt.Sprint(cfg.OAuth.Scopes) != "[s1 s2]" || *cfg.OAuth.RevokeURL != "https://idp/revoke" ||
		fmt.Sprint(cfg.OAuth.ExtraAuthorizeParams) != "[{z 1} {a 2}]" {
		t.Errorf("oauth %+v", cfg.OAuth)
	}
	// the copy is independent of the catalog's value
	cfg.OAuth.Scopes[0] = "changed"
	*cfg.OAuth.RevokeURL = "changed"
	if f.targets["gitea"].Auth.OAuth.Scopes[0] != "s1" || *f.targets["gitea"].Auth.OAuth.RevokeURL != "https://idp/revoke" {
		t.Error("the directory handed out the catalog's own slices")
	}
	for _, slug := range []string{"yt", "sh"} {
		c, _ := d.Config(context.Background(), slug)
		if c == nil || c.OAuth != nil {
			t.Errorf("%s: OAuth must be nil: %+v", slug, c)
		}
	}
	if c, err := d.Config(context.Background(), "nope"); c != nil || err != nil {
		t.Errorf("unknown or disabled: %v %v", c, err)
	}
}

func TestPersonalServersForKeepsOrderAndDropsOthers(t *testing.T) {
	f := &fakeCatalog{
		views: []catalog.MemberServerView{
			{Slug: "a-gitea", Name: "Gitea", AuthKind: catalog.AuthOAuthUser},
			{Slug: "b-shared", Name: "Shared", AuthKind: catalog.AuthSharedSecret},
			{Slug: "c-none", Name: "None", AuthKind: catalog.AuthNone},
			{Slug: "d-yt", Name: "YouTrack", AuthKind: catalog.AuthPATUser},
			{Slug: "e-gone", Name: "Gone", AuthKind: catalog.AuthPATUser},
		},
		targets: map[string]*catalog.ServerCallTarget{
			"a-gitea": oauthTarget("a-gitea"),
			"d-yt":    {Slug: "d-yt", URL: "https://yt", Auth: catalog.AuthMode{Kind: catalog.AuthPATUser}},
		},
	}
	p := app.Principal{UserID: "u1", Role: "admin"}
	got, err := NewServerDirectory(f).PersonalServersFor(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Slug != "a-gitea" || got[0].Name != "Gitea" || got[0].AuthKind != catalog.AuthOAuthUser || got[0].Config.OAuth == nil ||
		got[1].Slug != "d-yt" || got[1].Name != "YouTrack" || got[1].AuthKind != catalog.AuthPATUser || got[1].Config.URL != "https://yt" {
		t.Fatalf("%+v", got)
	}
	if !reflect.DeepEqual(f.asked, p) {
		t.Errorf("the principal was not passed on: %+v", f.asked)
	}
	// the N+1 of the reference: one target read per personal server
	if fmt.Sprint(f.calls) != "[list target:a-gitea target:d-yt target:e-gone]" {
		t.Errorf("calls %v", f.calls)
	}
	f.views = nil
	if empty, err := NewServerDirectory(f).PersonalServersFor(context.Background(), p); err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("empty list: %#v %v", empty, err)
	}
	f.listErr = errors.New("db down")
	if _, err := NewServerDirectory(f).PersonalServersFor(context.Background(), p); err == nil {
		t.Error("a catalog error must surface")
	}
}

func TestClientSecretForZeroWhenUnsetMovedAndErrorsNeverCarryIt(t *testing.T) {
	f := &fakeCatalog{secrets: map[string]string{"gitea": "SECRETMARK"}, moved: map[string]bool{"gone": true}}
	d := NewServerDirectory(f)
	cfg := func(slug string) app.ServerConfig { return app.ServerConfigOf(*oauthTarget(slug)) }
	s, moved, err := d.ClientSecretFor(context.Background(), cfg("gitea"))
	if v, ok := s.Value(); err != nil || moved || !ok || v != "SECRETMARK" {
		t.Fatalf("%v %v %v", ok, moved, err)
	}
	// the catalog is asked with the configuration the caller holds, once
	if len(f.targetOf) != 1 || f.targetOf[0].Slug != "gitea" || f.targetOf[0].URL != "https://Up.example/mcp?x=1" ||
		f.targetOf[0].Auth.Kind != catalog.AuthOAuthUser || f.targetOf[0].Auth.OAuth == nil || f.targetOf[0].Auth.OAuth.TokenURL != "https://idp/t" {
		t.Fatalf("target %+v", f.targetOf)
	}
	if s, moved, err := d.ClientSecretFor(context.Background(), cfg("yt")); err != nil || moved || s != (app.ClientSecret{}) {
		t.Fatalf("unset must be the zero value, not moved: %v %v %v", s, moved, err)
	}
	if s, moved, err := d.ClientSecretFor(context.Background(), cfg("gone")); err != nil || !moved || s != (app.ClientSecret{}) {
		t.Fatalf("moved must carry no secret: %v %v %v", s, moved, err)
	}
	f.secretEr = fmt.Errorf("cannot open SECRETMARK with this key")
	_, _, err = d.ClientSecretFor(context.Background(), cfg("gitea"))
	if err == nil {
		t.Fatal("no error")
	}
	for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(text, "SECRETMARK") {
			t.Errorf("error carries the secret: %s", text)
		}
	}
}
