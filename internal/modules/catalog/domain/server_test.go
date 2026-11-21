package domain_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

func baseParams() domain.NewServerParams {
	return domain.NewServerParams{
		Slug: "gitea", Name: "Gitea", Description: "Git hosting",
		URL:    "https://mcp.example.com/gitea",
		Auth:   domain.AuthMode{Kind: domain.AuthNone},
		Access: domain.Access{Kind: domain.AccessAll},
		Now:    t0,
	}
}

func make1(t *testing.T, mut ...func(*domain.NewServerParams)) *domain.Server {
	t.Helper()
	p := baseParams()
	for _, m := range mut {
		m(&p)
	}
	s, err := domain.NewServer(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func serverChangedEvent(slug string) kernel.Event {
	return kernel.Event{Type: "catalog.server_changed", Payload: map[string]any{"server": slug}}
}

func TestNewServerDefaults(t *testing.T) {
	s := make1(t)
	if s.Slug() != "gitea" || s.TimeoutMs() != 120_000 || s.DailyLimit() != nil || s.Status() != domain.StatusActive ||
		s.SecretSet() || s.SealedSecret() != nil || s.Version() != 0 || s.LastSyncAt() != nil ||
		s.LastSyncError() != nil || s.SyncUserID() != nil || !s.CreatedAt().Equal(t0) || !s.UpdatedAt().Equal(t0) {
		t.Fatalf("%+v", s)
	}
	if s.Name() != "Gitea" || s.Description() != "Git hosting" || s.URL() != "https://mcp.example.com/gitea" {
		t.Fatal("fields")
	}
}

func TestNewServerEmitsServerChanged(t *testing.T) {
	s := make1(t)
	if got := s.PullEvents(); !reflect.DeepEqual(got, []kernel.Event{serverChangedEvent("gitea")}) {
		t.Fatalf("%#v", got)
	}
	if got := s.PullEvents(); len(got) != 0 {
		t.Fatalf("events are drained: %#v", got)
	}
}

func TestNewServerValidation(t *testing.T) {
	bad := []struct {
		name string
		mut  func(*domain.NewServerParams)
		code string
		msg  string
	}{
		{"slug", func(p *domain.NewServerParams) { p.Slug = "Gitea" }, "invalid_slug", ""},
		{"blank name", func(p *domain.NewServerParams) { p.Name = " " }, "invalid_server", "server name must be 1-100 characters"},
		{"long name", func(p *domain.NewServerParams) { p.Name = strings.Repeat("x", 101) }, "invalid_server", "server name must be 1-100 characters"},
		{"long description", func(p *domain.NewServerParams) { p.Description = strings.Repeat("x", 2001) }, "invalid_server", "server description must be at most 2000 characters"},
		{"ftp", func(p *domain.NewServerParams) { p.URL = "ftp://x.example" }, "invalid_server", "upstream URL must be a valid http(s) URL"},
		{"not a url", func(p *domain.NewServerParams) { p.URL = "not a url" }, "invalid_server", "upstream URL must be a valid http(s) URL"},
		{"timeout 0", func(p *domain.NewServerParams) { p.TimeoutMs = ptr(0) }, "invalid_server", "timeout must be 1-3600000 ms"},
		{"timeout too long", func(p *domain.NewServerParams) { p.TimeoutMs = ptr(3_600_001) }, "invalid_server", "timeout must be 1-3600000 ms"},
		{"timeout negative", func(p *domain.NewServerParams) { p.TimeoutMs = ptr(-5) }, "invalid_server", "timeout must be 1-3600000 ms"},
		{"limit 0", func(p *domain.NewServerParams) { p.DailyLimit = ptr(0) }, "invalid_server", "daily limit must be a positive integer or empty"},
		{"limit negative", func(p *domain.NewServerParams) { p.DailyLimit = ptr(-1) }, "invalid_server", "daily limit must be a positive integer or empty"},
		{"auth", func(p *domain.NewServerParams) { p.Auth = sharedMode("X", "no placeholder") }, "invalid_auth", "header template must contain {secret} and be a single line"},
		{"access", func(p *domain.NewServerParams) { p.Access = groupsAccess() }, "invalid_access", "list at least one non-empty group"},
	}
	for _, c := range bad {
		p := baseParams()
		c.mut(&p)
		s, err := domain.NewServer(p)
		if s != nil {
			t.Errorf("%s: a server was returned with the error", c.name)
		}
		if c.msg == "" {
			wantCode(t, err, c.code)
		} else {
			wantMsg(t, err, c.code, c.msg)
		}
	}
}

func TestNewServerValidationOrder(t *testing.T) {
	// slug, name, description, url, timeout, limit, auth, access
	p := baseParams()
	p.Slug, p.Name, p.Description, p.URL = "X", "", strings.Repeat("x", 2001), "nope"
	p.TimeoutMs, p.DailyLimit = ptr(0), ptr(0)
	p.Auth, p.Access = sharedMode("", ""), groupsAccess()
	steps := []func(){
		func() { p.Slug = "gitea" },
		func() { p.Name = "n" },
		func() { p.Description = "" },
		func() { p.URL = "https://x.example" },
		func() { p.TimeoutMs = nil },
		func() { p.DailyLimit = nil },
		func() { p.Auth = domain.AuthMode{Kind: domain.AuthNone} },
	}
	want := []string{"invalid_slug", "invalid_server", "invalid_server", "invalid_server", "invalid_server", "invalid_server", "invalid_auth", "invalid_access"}
	for i, w := range want {
		_, err := domain.NewServer(p)
		wantCode(t, err, w)
		if i < len(steps) {
			steps[i]()
		}
	}
	if _, err := domain.NewServer(p); err == nil {
		t.Fatal("access is still invalid")
	}
}

func TestNewServerBoundaries(t *testing.T) {
	s := make1(t, func(p *domain.NewServerParams) {
		p.TimeoutMs, p.DailyLimit = ptr(3_600_000), ptr(1)
		p.Name = strings.Repeat("x", 100)
		p.Description = strings.Repeat("d", 2000)
	})
	if s.TimeoutMs() != 3_600_000 || *s.DailyLimit() != 1 {
		t.Fatal("bounds")
	}
	if make1(t, func(p *domain.NewServerParams) { p.TimeoutMs = ptr(1) }).TimeoutMs() != 1 {
		t.Fatal("min timeout")
	}
	// lengths are UTF-16 code units: 50 astral characters are 100 units, 51 are 102
	p := baseParams()
	p.Name = strings.Repeat("😀", 50)
	if _, err := domain.NewServer(p); err != nil {
		t.Fatal(err)
	}
	p.Name = strings.Repeat("😀", 51)
	_, err := domain.NewServer(p)
	wantCode(t, err, "invalid_server")
	p.Name = "é" // one unit, two bytes
	if _, err := domain.NewServer(p); err != nil {
		t.Fatal(err)
	}
}

func TestNewServerTrimsNameAndDescriptionButKeepsURLAsGiven(t *testing.T) {
	s := make1(t, func(p *domain.NewServerParams) {
		p.Name, p.Description = "  Gitea \u00a0", " d\ufeff"
		p.URL = "HTTPS://Mcp.Example.com:443/a/../b?x=1#f"
	})
	if s.Name() != "Gitea" || s.Description() != "d" {
		t.Fatalf("%q %q", s.Name(), s.Description())
	}
	if s.URL() != "HTTPS://Mcp.Example.com:443/a/../b?x=1#f" {
		t.Fatal("the URL is stored exactly as given, no re-serialisation")
	}
}

func TestNewServerExplicitValues(t *testing.T) {
	s := make1(t, func(p *domain.NewServerParams) { p.TimeoutMs, p.DailyLimit = ptr(5000), ptr(10) })
	if s.TimeoutMs() != 5000 || *s.DailyLimit() != 10 {
		t.Fatal("explicit")
	}
}

func TestNewServerAccessAndAuthAreNormalised(t *testing.T) {
	s := make1(t, func(p *domain.NewServerParams) {
		p.Access = groupsAccess(" dev ", "dev")
		p.Auth = domain.AuthMode{Kind: domain.AuthNone, Header: "x"}
	})
	if !reflect.DeepEqual(s.Access(), groupsAccess("dev")) || !reflect.DeepEqual(s.Auth(), domain.AuthMode{Kind: domain.AuthNone}) {
		t.Fatalf("%+v %+v", s.Access(), s.Auth())
	}
}

func TestUpdateChangesFieldsAndEmitsForNameDescription(t *testing.T) {
	s := make1(t)
	s.PullEvents()
	cleared, err := s.Update(domain.Changes{Name: domain.Some("Gitea 2")}, t1)
	if err != nil || cleared {
		t.Fatal(cleared, err)
	}
	if s.Name() != "Gitea 2" || !s.UpdatedAt().Equal(t1) {
		t.Fatal("not applied")
	}
	if got := s.PullEvents(); len(got) != 1 {
		t.Fatalf("%#v", got)
	}
	// description change, and the compared value is the trimmed one
	if _, err := s.Update(domain.Changes{Description: domain.Some(" new ")}, t1); err != nil || s.Description() != "new" {
		t.Fatal(err)
	}
	if len(s.PullEvents()) != 1 {
		t.Fatal("description change tells the index")
	}
	if _, err := s.Update(domain.Changes{Name: domain.Some("  Gitea 2 "), Description: domain.Some("new")}, t1); err != nil {
		t.Fatal(err)
	}
	if len(s.PullEvents()) != 0 {
		t.Fatal("a value that trims to the stored one is not a change")
	}
}

func TestUpdateAccessEvent(t *testing.T) {
	s := make1(t)
	s.PullEvents()
	_, _ = s.Update(domain.Changes{Access: domain.Some(domain.Access{Kind: domain.AccessAll})}, t1)
	if len(s.PullEvents()) != 0 {
		t.Fatal("same access, no event")
	}
	_, _ = s.Update(domain.Changes{Access: domain.Some(groupsAccess("dev", "ops"))}, t1)
	if len(s.PullEvents()) != 1 {
		t.Fatal("changed access, one event")
	}
	_, _ = s.Update(domain.Changes{Access: domain.Some(groupsAccess("dev", "ops"))}, t1)
	if len(s.PullEvents()) != 0 {
		t.Fatal("same groups")
	}
	_, _ = s.Update(domain.Changes{Access: domain.Some(groupsAccess("ops", "dev"))}, t1)
	if len(s.PullEvents()) != 1 {
		t.Fatal("group order counts as a change, as in the TypeScript comparison")
	}
}

func TestUpdateNonSearchableFields(t *testing.T) {
	s := make1(t)
	s.PullEvents()
	_, err := s.Update(domain.Changes{
		URL: domain.Some("https://other.example/mcp"), TimeoutMs: domain.Some(1000), DailyLimit: domain.Some(ptr(5)),
	}, t1)
	if err != nil || s.URL() != "https://other.example/mcp" || *s.DailyLimit() != 5 || s.TimeoutMs() != 1000 {
		t.Fatal(err)
	}
	if len(s.PullEvents()) != 0 {
		t.Fatal("url, timeout and limit do not tell the index")
	}
	if _, err := s.Update(domain.Changes{DailyLimit: domain.Some[*int](nil)}, t1); err != nil || s.DailyLimit() != nil {
		t.Fatal("limit can be cleared", err)
	}
	if _, err := s.Update(domain.Changes{}, t1); err != nil || !s.UpdatedAt().Equal(t1) {
		t.Fatal("an empty update is valid and still stamps updatedAt")
	}
}

func TestUpdateIsAtomic(t *testing.T) {
	s := make1(t)
	_, err := s.Update(domain.Changes{Name: domain.Some("New"), TimeoutMs: domain.Some(0)}, t1)
	wantCode(t, err, "invalid_server")
	if s.Name() != "Gitea" || !s.UpdatedAt().Equal(t0) {
		t.Fatal("changed on error")
	}
	_, err = s.Update(domain.Changes{Auth: domain.Some(patMode("A", "x"))}, t1)
	wantCode(t, err, "invalid_auth")
	if !reflect.DeepEqual(s.Auth(), domain.AuthMode{Kind: domain.AuthNone}) {
		t.Fatal("auth changed on error")
	}
	if _, err := s.Update(domain.Changes{Access: domain.Some(groupsAccess("dev"))}, t1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Access(), groupsAccess("dev")) {
		t.Fatal("access")
	}
	_, err = s.Update(domain.Changes{Access: domain.Some(groupsAccess())}, t1)
	wantCode(t, err, "invalid_access")
}

func TestEventsAreCoalesced(t *testing.T) {
	s := make1(t)
	s.PullEvents()
	_, _ = s.Update(domain.Changes{Name: domain.Some("N")}, t1)
	s.SetStatus(domain.StatusDisabled, t1)
	if got := s.PullEvents(); !reflect.DeepEqual(got, []kernel.Event{serverChangedEvent("gitea")}) {
		t.Fatalf("%#v", got)
	}
	// a new server not yet saved plus an edit: still one
	n := make1(t)
	_, _ = n.Update(domain.Changes{Name: domain.Some("N")}, t1)
	if len(n.PullEvents()) != 1 {
		t.Fatal("create + update coalesce")
	}
}

var shared = sharedMode("X-Key", "{secret}")

func TestSecretSetAndClear(t *testing.T) {
	s := make1(t, func(p *domain.NewServerParams) { p.Auth = shared })
	s.PullEvents()
	if err := s.SetSealedSecret([]byte{1, 2, 3}, t1); err != nil {
		t.Fatal(err)
	}
	if !s.SecretSet() || !reflect.DeepEqual(s.SealedSecret(), []byte{1, 2, 3}) || !s.UpdatedAt().Equal(t1) {
		t.Fatal("set")
	}
	s.ClearSecret(t2)
	if s.SecretSet() || s.SealedSecret() != nil || !s.UpdatedAt().Equal(t2) {
		t.Fatal("clear")
	}
	if len(s.PullEvents()) != 0 {
		t.Fatal("secret changes emit nothing")
	}
}

func TestSetSealedSecretCopiesInput(t *testing.T) {
	s := make1(t, func(p *domain.NewServerParams) { p.Auth = shared })
	in := []byte{1, 2, 3}
	_ = s.SetSealedSecret(in, t1)
	in[0] = 9
	if s.SealedSecret()[0] != 1 {
		t.Fatal("the aggregate keeps its own copy")
	}
}

func TestSecretRefusedForModesWithoutOne(t *testing.T) {
	for _, auth := range []domain.AuthMode{{Kind: domain.AuthNone}, patMode("Authorization", "Bearer {secret}")} {
		s := make1(t, func(p *domain.NewServerParams) { p.Auth = auth })
		err := s.SetSealedSecret([]byte{1}, t1)
		wantMsg(t, err, "invalid_server", `auth mode "`+string(auth.Kind)+`" does not use a server secret`)
		if s.SecretSet() {
			t.Fatal("stored")
		}
		s.ClearSecret(t1) // clearing is always allowed
	}
	s := make1(t, func(p *domain.NewServerParams) { p.Auth = shared })
	wantCode(t, s.SetSealedSecret(nil, t1), "invalid_server")
	wantCode(t, s.SetSealedSecret([]byte{}, t1), "invalid_server")
}

func TestCheckSecret(t *testing.T) {
	for _, ok := range []string{"k", " padded ", strings.Repeat("x", 4096), strings.Repeat("😀", 2048), "a\n"} {
		if err := domain.CheckSecret(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " ", "\n\t", "\u00a0\ufeff", strings.Repeat("x", 4097), strings.Repeat("😀", 2049)} {
		wantMsg(t, domain.CheckSecret(bad), "invalid_server", "secret must be non-blank and at most 4096 characters")
	}
}

func TestAuthKindChangeDropsSecret(t *testing.T) {
	s := make1(t, func(p *domain.NewServerParams) { p.Auth = shared })
	_ = s.SetSealedSecret([]byte{1}, t0)
	cleared, err := s.Update(domain.Changes{Auth: domain.Some(domain.AuthMode{Kind: domain.AuthPATUser, Header: "A", Template: "{secret}"})}, t1)
	if err != nil || !cleared || s.SecretSet() {
		t.Fatal(cleared, err)
	}
}

func TestSameKindKeepsSecret(t *testing.T) {
	s := make1(t, func(p *domain.NewServerParams) { p.Auth = shared })
	_ = s.SetSealedSecret([]byte{1}, t0)
	cleared, err := s.Update(domain.Changes{Auth: domain.Some(sharedMode("X-Other", "{secret}"))}, t1)
	if err != nil || cleared || !s.SecretSet() {
		t.Fatal(cleared, err)
	}
	bare := make1(t)
	cleared, err = bare.Update(domain.Changes{Auth: domain.Some(shared)}, t1)
	if err != nil || cleared {
		t.Fatal("there was no secret to clear", cleared, err)
	}
}

func oauthOf(mut func(*domain.OAuthClientConfig), withRevoke bool) domain.AuthMode {
	o := domain.OAuthClientConfig{
		AuthorizeURL: "https://idp.example/authorize",
		ClientID:     "client-1",
		Scopes:       []string{"read"},
	}
	o.TokenURL = "https://idp.example/token"
	if withRevoke {
		o.RevokeURL = ptr("https://idp.example/revoke")
	}
	if mut != nil {
		mut(&o)
	}
	return oauthMode(o)
}

func withSecret(t *testing.T, auth domain.AuthMode) *domain.Server {
	t.Helper()
	s := make1(t, func(p *domain.NewServerParams) { p.Auth = auth })
	if err := s.SetSealedSecret([]byte{1}, t0); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSharedSecretDestinationChange(t *testing.T) {
	s := withSecret(t, shared)
	cleared, err := s.Update(domain.Changes{URL: domain.Some("https://elsewhere.example/mcp")}, t1)
	if err != nil || !cleared || s.SecretSet() || !s.SecretChanged() {
		t.Fatal(cleared, err)
	}
}

func TestSharedSecretKeepsOnOtherFields(t *testing.T) {
	s := withSecret(t, shared)
	cleared, err := s.Update(domain.Changes{URL: domain.Some(s.URL()), Name: domain.Some("Other"), TimeoutMs: domain.Some(5), DailyLimit: domain.Some(ptr(3))}, t1)
	if err != nil || cleared || !s.SecretSet() {
		t.Fatal(cleared, err)
	}
	if _, err := s.Update(domain.Changes{Auth: domain.Some(sharedMode("X-Other", "{secret}"))}, t1); err != nil || !s.SecretSet() {
		t.Fatal(err)
	}
}

func TestSharedSecretURLComparedAsPlainString(t *testing.T) {
	// no normalisation: an equivalent spelling is another destination
	for _, spelled := range []string{"https://MCP.example.com/gitea", "https://mcp.example.com/gitea/", "https://mcp.example.com:443/gitea", "https://mcp.example.com/gitea?", " https://mcp.example.com/gitea"} {
		s := withSecret(t, shared)
		cleared, err := s.Update(domain.Changes{URL: domain.Some(spelled)}, t1)
		if err != nil || !cleared {
			t.Errorf("%q: cleared=%v err=%v", spelled, cleared, err)
		}
	}
}

func TestSharedSecretWithoutStoredSecretReportsNothingCleared(t *testing.T) {
	s := make1(t, func(p *domain.NewServerParams) { p.Auth = shared })
	cleared, err := s.Update(domain.Changes{URL: domain.Some("https://elsewhere.example/mcp")}, t1)
	if err != nil || cleared || s.SecretChanged() == false {
		// a new server's secretChanged is always true: this only checks `cleared`
		t.Fatal(cleared, err)
	}
}

func TestOAuthDestinationChange(t *testing.T) {
	variants := map[string]domain.AuthMode{
		"token url":          oauthOf(func(o *domain.OAuthClientConfig) { o.TokenURL = "https://evil.example/token" }, true),
		"revoke url":         oauthOf(func(o *domain.OAuthClientConfig) { o.RevokeURL = ptr("https://evil.example/revoke") }, true),
		"client id":          oauthOf(func(o *domain.OAuthClientConfig) { o.ClientID = "client-2" }, true),
		"revoke url removed": oauthOf(nil, false),
		"token url spelling": oauthOf(func(o *domain.OAuthClientConfig) { o.TokenURL = "https://IDP.example/token" }, true),
	}
	for name, auth := range variants {
		s := withSecret(t, oauthOf(nil, true))
		cleared, err := s.Update(domain.Changes{Auth: domain.Some(auth)}, t1)
		if err != nil || !cleared || s.SecretSet() {
			t.Errorf("%s: cleared=%v err=%v", name, cleared, err)
		}
	}
	// revoke URL added where there was none
	s := withSecret(t, oauthOf(nil, false))
	cleared, err := s.Update(domain.Changes{Auth: domain.Some(oauthOf(nil, true))}, t1)
	if err != nil || !cleared {
		t.Fatal("added revoke url", cleared, err)
	}
}

func TestOAuthKeepsSecretOnOtherFields(t *testing.T) {
	s := withSecret(t, oauthOf(nil, true))
	next := oauthOf(func(o *domain.OAuthClientConfig) {
		o.AuthorizeURL = "https://idp.example/authorize2"
		o.Scopes = []string{"read", "write"}
		o.ExtraAuthorizeParams = []domain.AuthorizeParam{{Key: "access_type", Value: "offline"}}
	}, true)
	next.Header = "X-Token"
	cleared, err := s.Update(domain.Changes{URL: domain.Some("https://elsewhere.example/mcp"), Auth: domain.Some(next)}, t1)
	if err != nil || cleared || !s.SecretSet() {
		t.Fatal(cleared, err)
	}
}

func TestOAuthToSharedAndBackClears(t *testing.T) {
	s := withSecret(t, oauthOf(nil, true))
	cleared, err := s.Update(domain.Changes{Auth: domain.Some(shared)}, t1)
	if err != nil || !cleared {
		t.Fatal(cleared, err)
	}
}

func TestRejectedEditChangesNothing(t *testing.T) {
	s := withSecret(t, shared)
	_, err := s.Update(domain.Changes{URL: domain.Some("not a url")}, t1)
	wantCode(t, err, "invalid_server")
	if !s.SecretSet() {
		t.Fatal("secret dropped by a rejected edit")
	}
}

func TestSecretSetAfterEditIsKept(t *testing.T) {
	s := withSecret(t, shared)
	_, _ = s.Update(domain.Changes{URL: domain.Some("https://elsewhere.example/mcp")}, t1)
	_ = s.SetSealedSecret([]byte{9}, t1)
	if !reflect.DeepEqual(s.SealedSecret(), []byte{9}) {
		t.Fatal("kept")
	}
}

func loaded() *domain.Server {
	return domain.RehydrateServer(domain.ServerState{
		Slug: "gitea", Version: 3, Name: "G", Description: "", URL: "https://x.example",
		Auth: shared, SealedSecret: []byte{1}, Access: domain.Access{Kind: domain.AccessAll},
		TimeoutMs: 1000, Status: domain.StatusActive, CreatedAt: t0, UpdatedAt: t0,
	})
}

func TestSecretChanged(t *testing.T) {
	s := loaded()
	if s.SecretChanged() {
		t.Fatal("loaded")
	}
	_, _ = s.Update(domain.Changes{Name: domain.Some("Other"), Auth: domain.Some(sharedMode("X-Other", "{secret}"))}, t1)
	s.SetStatus(domain.StatusDisabled, t1)
	s.ApplySync(domain.SyncSuccess(ptr("a"), t1))
	if s.SecretChanged() {
		t.Fatal("unrelated edits do not touch the secret")
	}
	set := loaded()
	_ = set.SetSealedSecret([]byte{2}, t1)
	cleared := loaded()
	cleared.ClearSecret(t1)
	resealed := loaded()
	resealed.ReplaceSealedSecret([]byte{3})
	dropped := loaded()
	_, _ = dropped.Update(domain.Changes{Auth: domain.Some(patMode("A", "{secret}"))}, t1)
	for name, s := range map[string]*domain.Server{"set": set, "clear": cleared, "reseal": resealed, "drop": dropped} {
		if !s.SecretChanged() {
			t.Errorf("%s: secretChanged must be true", name)
		}
	}
	if !make1(t).SecretChanged() || make1(t).Version() != 0 {
		t.Fatal("a new server")
	}
}

func TestReplaceSealedSecretIsNotAnEdit(t *testing.T) {
	s := loaded()
	in := []byte{7, 7}
	s.ReplaceSealedSecret(in)
	in[0] = 0
	if !reflect.DeepEqual(s.SealedSecret(), []byte{7, 7}) || !s.UpdatedAt().Equal(t0) || len(s.PullEvents()) != 0 {
		t.Fatal("replace")
	}
}

func TestSetStatus(t *testing.T) {
	s := make1(t)
	s.PullEvents()
	if s.SetStatus(domain.StatusActive, t1) || len(s.PullEvents()) != 0 || !s.UpdatedAt().Equal(t0) {
		t.Fatal("no change")
	}
	if !s.SetStatus(domain.StatusDisabled, t1) || s.Status() != domain.StatusDisabled {
		t.Fatal("change")
	}
	if got := s.PullEvents(); !reflect.DeepEqual(got, []kernel.Event{serverChangedEvent("gitea")}) {
		t.Fatalf("%#v", got)
	}
}

func TestRehydrate(t *testing.T) {
	s := domain.RehydrateServer(domain.ServerState{
		Slug: "gitea", Version: 3, Name: "G", URL: "https://x.example", Auth: domain.AuthMode{Kind: domain.AuthNone},
		SealedSecret: []byte{1}, Access: domain.Access{Kind: domain.AccessAll}, TimeoutMs: 1000,
		DailyLimit: ptr(7), Status: domain.StatusDisabled, LastSyncAt: &t0, LastSyncError: ptr("boom"),
		SyncUserID: ptr("u1"), CreatedAt: t0, UpdatedAt: t1,
	})
	if !s.SecretSet() || s.Version() != 3 || *s.LastSyncError() != "boom" || !s.LastSyncAt().Equal(t0) ||
		*s.SyncUserID() != "u1" || *s.DailyLimit() != 7 || s.Status() != domain.StatusDisabled || len(s.PullEvents()) != 0 {
		t.Fatalf("%+v", s)
	}
	if s.CanBeUsedBy(member("dev")) || s.CanBeUsedBy(admin) {
		t.Fatal("disabled")
	}
	if !loaded().CanBeUsedBy(member()) {
		t.Fatal("active, open to all")
	}
}

func TestSyncStateSuccessClearsErrorAndRemembersAdmin(t *testing.T) {
	s := make1(t)
	s.ApplySync(domain.SyncFailureRecord(domain.FailureTimeout, t0))
	if !s.ApplySync(domain.SyncSuccess(ptr("admin-1"), t1)) {
		t.Fatal("applied")
	}
	if !s.LastSyncAt().Equal(t1) || s.LastSyncError() != nil || *s.SyncUserID() != "admin-1" {
		t.Fatal("state")
	}
}

func TestSyncStateScheduledSuccessKeepsAdmin(t *testing.T) {
	s := make1(t)
	s.ApplySync(domain.SyncSuccess(ptr("admin-1"), t0))
	s.ApplySync(domain.SyncSuccess(nil, t1))
	if *s.SyncUserID() != "admin-1" || !s.LastSyncAt().Equal(t1) {
		t.Fatal("kept")
	}
}

func TestSyncStateFailureStoresTextAndKeepsAdmin(t *testing.T) {
	s := make1(t)
	s.ApplySync(domain.SyncSuccess(ptr("admin-1"), t0))
	s.ApplySync(domain.SyncFailureRecord(domain.FailureNeedsConnection, t1))
	if *s.LastSyncError() != "An administrator needs to connect this server" || !s.LastSyncAt().Equal(t1) || *s.SyncUserID() != "admin-1" {
		t.Fatal("failure")
	}
}

func TestSyncStateOlderAttemptIgnored(t *testing.T) {
	s := make1(t)
	s.ApplySync(domain.SyncSuccess(ptr("admin-2"), t2))
	if s.ApplySync(domain.SyncFailureRecord(domain.FailureTimeout, t1)) || s.ApplySync(domain.SyncSuccess(ptr("admin-1"), t0)) {
		t.Fatal("an older attempt must be ignored")
	}
	if !s.LastSyncAt().Equal(t2) || s.LastSyncError() != nil || *s.SyncUserID() != "admin-2" {
		t.Fatal("overwritten")
	}
	if !s.ApplySync(domain.SyncFailureRecord(domain.FailureTimeout, t2)) {
		t.Fatal("an equal start is accepted")
	}
}

func TestSyncCredentialsOwner(t *testing.T) {
	s := make1(t)
	if s.SyncCredentialsOwner(nil) != nil || *s.SyncCredentialsOwner(ptr("admin-2")) != "admin-2" {
		t.Fatal("no owner yet")
	}
	s.ApplySync(domain.SyncSuccess(ptr("admin-1"), t0))
	if *s.SyncCredentialsOwner(nil) != "admin-1" || *s.SyncCredentialsOwner(ptr("admin-2")) != "admin-2" {
		t.Fatal("owner")
	}
}

func TestSyncStateEmitsNoEvents(t *testing.T) {
	s := make1(t)
	s.PullEvents()
	s.ApplySync(domain.SyncSuccess(ptr("a"), t0))
	s.ApplySync(domain.SyncFailureRecord(domain.FailureTransport, t1))
	if len(s.PullEvents()) != 0 {
		t.Fatal("events")
	}
}

func TestApplySyncDoesNotAliasRecord(t *testing.T) {
	s := make1(t)
	by := "admin-1"
	rec := domain.SyncSuccess(&by, t0)
	s.ApplySync(rec)
	by = "changed"
	if *s.SyncUserID() != "admin-1" {
		t.Fatal("aliased")
	}
}

func TestServerGettersReturnCopies(t *testing.T) {
	limit, synced, lastErr, syncUser := 7, t0, "boom", "u1"
	secret := []byte{1, 2, 3}
	groups := []string{"b", "a"}
	in := domain.ServerState{
		Slug: "gitea", Name: "G", URL: "https://x.example", Auth: shared, SealedSecret: secret,
		Access: domain.Access{Kind: domain.AccessGroups, Groups: groups}, TimeoutMs: 1000, DailyLimit: &limit,
		Status: domain.StatusActive, LastSyncAt: &synced, LastSyncError: &lastErr, SyncUserID: &syncUser, CreatedAt: t0, UpdatedAt: t0,
	}
	s := domain.RehydrateServer(in)
	// RehydrateServer clones what it is given
	secret[0], groups[0], limit, synced, lastErr, syncUser = 9, "changed", 99, t2, "changed", "changed"
	in.Auth.Header = "changed"
	check := func(when string) {
		t.Helper()
		if !reflect.DeepEqual(s.SealedSecret(), []byte{1, 2, 3}) || !reflect.DeepEqual(s.Access().Groups, []string{"b", "a"}) ||
			*s.DailyLimit() != 7 || !s.LastSyncAt().Equal(t0) || *s.LastSyncError() != "boom" || *s.SyncUserID() != "u1" ||
			s.Auth().Header != "X-Key" {
			t.Fatalf("%s: the aggregate changed", when)
		}
	}
	check("after changing the input")
	s.SealedSecret()[0] = 9
	g := s.Access()
	sort.Strings(g.Groups)
	g.Groups = append(g.Groups, "more")
	*s.DailyLimit() = 99
	*s.LastSyncAt() = t2
	*s.LastSyncError() = "changed"
	*s.SyncUserID() = "changed"
	check("after changing what the getters returned")
	// the constructor
	lim := 5
	n := make1(t, func(p *domain.NewServerParams) { p.DailyLimit = &lim })
	lim = 6
	*n.DailyLimit() = 8
	if *n.DailyLimit() != 5 {
		t.Fatal("NewServer or DailyLimit shares the pointer")
	}
	by := "admin"
	n.ApplySync(domain.SyncSuccess(&by, t1))
	*n.SyncCredentialsOwner(nil) = "changed"
	if *n.SyncUserID() != "admin" {
		t.Fatal("SyncCredentialsOwner shares the pointer")
	}
}

func TestSecretKeepsOnTemplateAndHeaderChange(t *testing.T) {
	for _, auth := range []domain.AuthMode{sharedMode("X-Other", "{secret}"), sharedMode("X-Key", "Bearer {secret}"), sharedMode("X-Other", "k={secret}")} {
		s := withSecret(t, shared)
		cleared, err := s.Update(domain.Changes{Auth: domain.Some(auth)}, t1)
		if err != nil || cleared || !s.SecretSet() {
			t.Errorf("%+v: cleared=%v err=%v", auth, cleared, err)
		}
	}
	// oauth keep-list: template, header and the extra params
	base := oauthOf(nil, true)
	next := oauthOf(func(o *domain.OAuthClientConfig) {
		o.ExtraAuthorizeParams = []domain.AuthorizeParam{{Key: "prompt", Value: "consent"}}
	}, true)
	next.Template = "token {secret}"
	for _, auth := range []domain.AuthMode{next, func() domain.AuthMode { a := base; a.Template = "x {secret}"; return a }(), func() domain.AuthMode { a := base; a.Header = "X-Token"; return a }()} {
		s := withSecret(t, base)
		cleared, err := s.Update(domain.Changes{Auth: domain.Some(auth)}, t1)
		if err != nil || cleared || !s.SecretSet() {
			t.Errorf("oauth %+v: cleared=%v err=%v", auth, cleared, err)
		}
	}
	// pat_user holds no server secret and its edits never clear anything
	pat := make1(t, func(p *domain.NewServerParams) { p.Auth = patMode("A", "{secret}") })
	if cleared, err := pat.Update(domain.Changes{Auth: domain.Some(patMode("B", "t {secret}"))}, t1); err != nil || cleared {
		t.Fatal(cleared, err)
	}
}
