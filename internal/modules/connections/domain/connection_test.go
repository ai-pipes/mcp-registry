package domain_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

var now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func at(ms int64) time.Time         { return now.Add(time.Duration(ms) * time.Millisecond) }
func tp(t time.Time) *time.Time     { return &t }
func sp(s string) *string           { return &s }
func sealed(s string) domain.Sealed { return domain.Sealed(s) }

func endpoints(authorize, exchange, client string) domain.OAuthEndpoints {
	return domain.OAuthEndpoints{AuthorizeURL: authorize, TokenURL: exchange, ClientID: client}
}

var oauthCfg = endpoints("https://git.example.com/a", "https://git.example.com/t", "cid")

var fp = domain.ServerFingerprint(domain.ServerConfigOrigin{URL: "https://mcp.example.com/gitea", AuthKind: "oauth_user", OAuth: &oauthCfg})

type oauthOpt struct {
	expiresAt *time.Time
	refresh   domain.Sealed
}

func oauth(o oauthOpt) *domain.Connection {
	return domain.ConnectOAuth(domain.OAuthConnect{
		UserID: "u1", Server: "gitea", Tokens: domain.SealedTokens{Access: sealed("A1"), Refresh: o.refresh, ExpiresAt: o.expiresAt},
		IssuedFor: fp, Now: now,
	})
}

func oauthDefault() *domain.Connection {
	return oauth(oauthOpt{expiresAt: tp(at(3600_000)), refresh: sealed("R1")})
}

func plan(c *domain.Connection, force bool, config string) domain.CredentialPlan {
	return c.CredentialPlan(now, domain.PlanOptions{ForceRefresh: force, Config: config})
}

func pat() *domain.Connection {
	return domain.ConnectPAT(domain.PATConnect{UserID: "u1", Server: "yt", Token: sealed("P"), IssuedFor: fp, Now: now})
}

func TestCredentialPlan(t *testing.T) {
	t.Run("uses a fresh OAuth token as is", func(t *testing.T) {
		if p := plan(oauthDefault(), false, fp); p.Action != domain.PlanUse {
			t.Fatalf("got %v", p.Action)
		}
		p := plan(oauth(oauthOpt{expiresAt: tp(at(0)), refresh: sealed("R1")}), false, fp)
		if p.Action != domain.PlanRefresh || string(p.RefreshToken) != "R1" {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("refreshes ahead when less than 60 s are left, or already expired", func(t *testing.T) {
		for _, c := range []struct {
			ms   int64
			want domain.PlanAction
		}{{30_000, domain.PlanRefresh}, {59_999, domain.PlanRefresh}, {60_000, domain.PlanUse}, {-1, domain.PlanRefresh}} {
			got := plan(oauth(oauthOpt{expiresAt: tp(at(c.ms)), refresh: sealed("R1")}), false, fp).Action
			if got != c.want {
				t.Errorf("%d ms left: got %v want %v", c.ms, got, c.want)
			}
		}
	})
	t.Run("a token without an expiry is used until forced", func(t *testing.T) {
		c := oauth(oauthOpt{refresh: sealed("R1")})
		if plan(c, false, fp).Action != domain.PlanUse || plan(c, true, fp).Action != domain.PlanRefresh {
			t.Fatal("wrong plan")
		}
	})
	t.Run("without a refresh token: used while valid, expired once it is not or was rejected", func(t *testing.T) {
		if plan(oauth(oauthOpt{expiresAt: tp(at(30_000))}), false, fp).Action != domain.PlanUse {
			t.Error("30 s left must be used")
		}
		p := plan(oauth(oauthOpt{expiresAt: tp(at(0))}), false, fp)
		if p.Action != domain.PlanExpire || p.Reason != domain.ReasonTokenExpired {
			t.Errorf("exactly now: %+v", p)
		}
		if plan(oauth(oauthOpt{}), true, fp).Action != domain.PlanExpire {
			t.Error("forced without a refresh token must expire")
		}
	})
	t.Run("an expired connection is never refreshed again", func(t *testing.T) {
		c := oauth(oauthOpt{expiresAt: tp(at(-1)), refresh: sealed("R1")})
		c.Expire(now)
		if c.Status() != domain.StatusExpired || plan(c, true, fp).Action != domain.PlanExpired {
			t.Fatal("must stay expired")
		}
	})
	t.Run("an expired connection stays expired even with a changed config and a fresh token", func(t *testing.T) {
		c := oauthDefault()
		c.Expire(now)
		if plan(c, false, "other").Action != domain.PlanExpired {
			t.Fatal("expired wins over config_changed")
		}
	})
	t.Run("a PAT has no expiry and cannot be refreshed", func(t *testing.T) {
		c := pat()
		if c.Kind() != domain.KindPAT || c.ExpiresAt() != nil || c.RefreshToken() != nil || c.RevokeURL() != nil {
			t.Fatal("PAT shape")
		}
		if plan(c, false, fp).Action != domain.PlanUse || plan(c, true, fp).Action != domain.PlanUse {
			t.Fatal("PAT must be used")
		}
		c.Expire(now)
		if plan(c, false, fp).Action != domain.PlanExpired {
			t.Fatal("expired PAT")
		}
	})
}

func TestIssuedForConfig(t *testing.T) {
	changed := domain.ServerFingerprint(domain.ServerConfigOrigin{URL: "https://evil.example.com/mcp", AuthKind: "oauth_user", OAuth: &oauthCfg})
	c := oauthDefault()
	if p := plan(c, false, changed); p.Action != domain.PlanExpire || p.Reason != domain.ReasonConfigChanged {
		t.Fatalf("got %+v", p)
	}
	if !c.IssuedFor(fp) || c.IssuedFor(changed) {
		t.Error("IssuedFor")
	}
	if c.StatusFor(fp) != domain.StatusReady || c.StatusFor(changed) != domain.StatusExpired {
		t.Error("StatusFor")
	}
	if plan(pat(), false, changed).Action != domain.PlanExpire {
		t.Error("a PAT issued for another config expires too")
	}
	if domain.ReasonRefreshRejected != "refresh_rejected" || domain.ReasonRejectedUpstream != "rejected_upstream" {
		t.Error("reason values")
	}
}

func issued(revoke *string) *domain.Connection {
	return domain.ConnectOAuth(domain.OAuthConnect{
		UserID: "u1", Server: "gitea", Tokens: domain.SealedTokens{Access: sealed("A1"), Refresh: sealed("R1")},
		IssuedFor: fp, RevokeURL: revoke, Now: now,
	})
}

func TestMayRevokeAt(t *testing.T) {
	c := issued(sp("https://git.example.com/revoke"))
	if got := c.RevokeURL(); got == nil || *got != "https://git.example.com/revoke" {
		t.Fatalf("RevokeURL %v", got)
	}
	if !c.MayRevokeAt(sp("https://git.example.com/revoke")) {
		t.Error("same URL")
	}
	if c.MayRevokeAt(sp("https://evil.example.com/revoke")) || c.MayRevokeAt(sp("https://git.example.com/revoke?v=2")) || c.MayRevokeAt(nil) {
		t.Error("other URL or none")
	}
	if issued(nil).MayRevokeAt(sp("https://git.example.com/revoke")) || pat().MayRevokeAt(sp("https://git.example.com/revoke")) {
		t.Error("no revoke URL at issue time, or a PAT")
	}
	c.Refreshed(domain.SealedTokens{Access: sealed("A2")}, now)
	if !c.MayRevokeAt(sp("https://git.example.com/revoke")) {
		t.Error("a refresh keeps the issuing revoke URL")
	}
	// the returned pointer is a copy
	*c.RevokeURL() = "x"
	if *c.RevokeURL() != "https://git.example.com/revoke" {
		t.Error("RevokeURL leaks the internal value")
	}
}

func TestIsRefreshRejected(t *testing.T) {
	f := func(status int, code string) *domain.RefreshFailure {
		return &domain.RefreshFailure{Status: status, Error: code}
	}
	for _, c := range []struct {
		name string
		in   *domain.RefreshFailure
		want bool
	}{
		{"400 invalid_grant", f(400, "invalid_grant"), true},
		{"400 no code", f(400, ""), true},
		{"500 invalid_grant", f(500, "invalid_grant"), true},
		{"503 no code", f(503, ""), false},
		{"500 server_error", f(500, "server_error"), false},
		{"429", f(429, ""), false},
		{"no answer", nil, false},
		{"hub wrong client secret", f(401, "invalid_client"), false},
		{"gitea wrong client secret", f(400, "invalid_client"), false},
		{"401 unauthorized_client", f(401, "unauthorized_client"), false},
		{"400 unauthorized_client", f(400, "unauthorized_client"), true},
		{"401 no code", f(401, ""), true},
		{"403 invalid_grant", f(403, "invalid_grant"), true},
		{"400 invalid_request", f(400, "invalid_request"), true},
		{"401 invalid_token", f(401, "invalid_token"), true},
		{"200 nothing", f(200, ""), false},
	} {
		if got := domain.IsRefreshRejected(c.in); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestUsableWhileProviderDown(t *testing.T) {
	c := oauth(oauthOpt{expiresAt: tp(at(30_000)), refresh: sealed("R1")})
	no, yes := domain.ProviderDownOptions{}, domain.ProviderDownOptions{}
	yes.ForceRefresh = true
	if !c.UsableWhileProviderDown(now, no) || !c.UsableWhileProviderDown(at(29_999), no) {
		t.Error("still valid")
	}
	if c.UsableWhileProviderDown(at(30_000), no) {
		t.Error("exactly at expiry is not usable")
	}
	if c.UsableWhileProviderDown(now, yes) {
		t.Error("rejected upstream")
	}
	if !oauth(oauthOpt{}).UsableWhileProviderDown(now, no) {
		t.Error("no expiry")
	}
	c.Expire(now)
	if c.UsableWhileProviderDown(now, no) {
		t.Error("expired")
	}
}

func TestRefreshed(t *testing.T) {
	c := oauth(oauthOpt{expiresAt: tp(at(10_000)), refresh: sealed("R1")})
	later := at(5_000)
	c.Refreshed(domain.SealedTokens{Access: sealed("A2"), ExpiresAt: tp(at(3_605_000))}, later)
	if string(c.AccessToken()) != "A2" || string(c.RefreshToken()) != "R1" || !c.ExpiresAt().Equal(at(3_605_000)) || !c.UpdatedAt().Equal(later) || c.Status() != domain.StatusReady {
		t.Fatalf("after refresh: %v", c)
	}
	c.Refreshed(domain.SealedTokens{Access: sealed("A3"), Refresh: sealed("R2")}, later)
	if string(c.RefreshToken()) != "R2" || c.ExpiresAt() != nil {
		t.Fatal("a rotated refresh token replaces the old one; no expiry becomes none")
	}
	// refresh of an expired connection makes it ready again (the caller decides when)
	c.Expire(later)
	c.Refreshed(domain.SealedTokens{Access: sealed("A4")}, at(6_000))
	if c.Status() != domain.StatusReady {
		t.Fatal("status")
	}
}

func TestIsNewerThan(t *testing.T) {
	seen := oauthDefault()
	same := domain.Restore(seen.Snapshot())
	if same.IsNewerThan(seen) {
		t.Fatal("same")
	}
	same.Refreshed(domain.SealedTokens{Access: sealed("A2")}, at(1))
	if !same.IsNewerThan(seen) {
		t.Fatal("newer")
	}
	// milliseconds, like the reference: two writes inside one millisecond are not told apart
	a := domain.Restore(domain.Snapshot{UpdatedAt: now.Add(100 * time.Microsecond)})
	b := domain.Restore(domain.Snapshot{UpdatedAt: now.Add(900 * time.Microsecond)})
	if b.IsNewerThan(a) {
		t.Fatal("sub-millisecond differences do not count")
	}
}

func TestReplaceSealed(t *testing.T) {
	c := oauthDefault()
	c.Expire(at(1))
	before := c.Snapshot()
	if err := c.ReplaceSealed(sealed("A1*"), sealed("R1*")); err != nil {
		t.Fatal(err)
	}
	after := c.Snapshot()
	if string(after.Access) != "A1*" || string(after.Refresh) != "R1*" {
		t.Fatal("not replaced")
	}
	after.Access, after.Refresh = before.Access, before.Refresh
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("status, expiry and updatedAt must stay: %v vs %v", after, before)
	}
	if err := oauthDefault().ReplaceSealed(sealed("A"), nil); err == nil {
		t.Error("dropping the refresh token must fail")
	}
	if err := oauth(oauthOpt{}).ReplaceSealed(sealed("A"), sealed("R")); err == nil {
		t.Error("adding a refresh token must fail")
	}
	if err := domain.ErrReplaceRefreshPresence; err == nil || err.Error() != "a re-seal keeps the refresh token present or absent" {
		t.Errorf("message %v", err)
	}
}

func TestRestoreRoundTripAndCopies(t *testing.T) {
	c := issued(sp("https://r"))
	c.Expire(at(5))
	r := domain.Restore(c.Snapshot())
	if !reflect.DeepEqual(r.Snapshot(), c.Snapshot()) {
		t.Fatal("round trip")
	}
	// accessors and snapshots hand out copies
	buf := []byte("A1")
	in := domain.Restore(domain.Snapshot{Access: buf, Refresh: sealed("R"), ExpiresAt: tp(now), RevokeURL: sp("u")})
	buf[0] = 'X'
	in.AccessToken()[0] = 'Y'
	s := in.Snapshot()
	s.Access[0] = 'Z'
	*s.ExpiresAt = at(99)
	*s.RevokeURL = "changed"
	if string(in.AccessToken()) != "A1" || !in.ExpiresAt().Equal(now) || *in.RevokeURL() != "u" {
		t.Fatal("aliasing")
	}
	if c.UserID() != "u1" || c.Server() != "gitea" || c.Kind() != domain.KindOAuth {
		t.Error("accessors")
	}
}

func TestSealedNeverPrints(t *testing.T) {
	s := sealed("TOPSECRET")
	c := issued(nil)
	c.Refreshed(domain.SealedTokens{Access: sealed("TOPSECRET"), Refresh: sealed("TOPSECRET")}, now)
	pending := domain.NewPendingConnect("TOPSECRETSTATE", "u1", "gitea", "TOPSECRETVERIFIER", fp, now)
	var logbuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logbuf, nil))
	logger.Info("x", "sealed", s, "conn", c, "snap", c.Snapshot(), "pending", pending)
	jb, err := json.Marshal(map[string]any{"s": s, "c": c, "snap": c.Snapshot(), "p": pending})
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q %x %d", s, s, s, s, s, s, s),
		fmt.Sprintf("%v %+v %#v %s", c, c, c, c), fmt.Sprintf("%v %+v %#v", c.Snapshot(), c.Snapshot(), c.Snapshot()),
		fmt.Sprintf("%v %+v %#v %s", pending, pending, pending, &pending),
		logbuf.String(), string(jb),
	} {
		if strings.Contains(out, "TOPSECRET") {
			t.Errorf("leak: %s", out)
		}
	}
}

func TestPatParse(t *testing.T) {
	got, err := domain.ParsePAT("  abc  ")
	if err != nil || got != "abc" {
		t.Fatalf("%q %v", got, err)
	}
	// JavaScript trim: NBSP, U+FEFF, U+2028 are whitespace
	if got, err := domain.ParsePAT("\u00a0\ufeffabc\u2028"); err != nil || got != "abc" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"", "   ", "a\nb", "a\rb", "a\x00b", strings.Repeat("x", 4097), "\u00a0 \ufeff"} {
		_, err := domain.ParsePAT(bad)
		de, ok := kernel.AsDomainError(err)
		if !ok || de.Code != "invalid_pat" || de.Message != "the token must be a non-empty single line" {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := domain.ParsePAT(strings.Repeat("x", 4096)); err != nil {
		t.Error("4096 is allowed")
	}
	// the limit counts UTF-16 units: an astral character is two
	if _, err := domain.ParsePAT(strings.Repeat("x", 4095) + "😀"); err == nil {
		t.Error("4097 UTF-16 units must be refused")
	}
	if _, err := domain.ParsePAT(strings.Repeat("😀", 2048)); err != nil {
		t.Error("exactly 4096 UTF-16 units must pass")
	}
}

func TestPendingConnect(t *testing.T) {
	p := domain.NewPendingConnect("s", "u1", "gitea", "v", fp, now)
	if !p.ExpiresAt().Equal(at(600_000)) {
		t.Fatalf("expiry %v", p.ExpiresAt())
	}
	if domain.PendingConnectExpiry(now) != at(600_000) {
		t.Fatal("PendingConnectExpiry")
	}
	if domain.IsPendingConnectExpired(p.ExpiresAt(), at(599_999)) || !domain.IsPendingConnectExpired(p.ExpiresAt(), at(600_000)) {
		t.Fatal("boundary: expired exactly at +10 min")
	}
	if !p.CanComplete("u1", now) || p.CanComplete("u2", now) || p.CanComplete("u1", at(600_000)) || !p.CanComplete("u1", at(599_999)) {
		t.Error("CanComplete")
	}
	if !p.StartedWithConfig(fp) || p.StartedWithConfig("other") {
		t.Error("StartedWithConfig")
	}
	if p.State() != "s" || p.UserID() != "u1" || p.Server() != "gitea" || p.CodeVerifier() != "v" || p.IssuedFor() != fp {
		t.Error("accessors")
	}
	r := domain.RestorePendingConnect(domain.PendingSnapshot{State: "s", UserID: "u1", Server: "gitea", CodeVerifier: "v", IssuedFor: fp, ExpiresAt: at(600_000)})
	if r != p {
		t.Error("restore")
	}
}

func TestErrors(t *testing.T) {
	for _, c := range []struct {
		err        error
		code, text string
	}{
		{domain.ErrNotFound("gitea"), "not_found", `server "gitea" not found`},
		{domain.ErrInvalidAuthKind("gitea", domain.WhatOAuth), "invalid_auth_kind", `server "gitea" does not use OAuth`},
		{domain.ErrInvalidAuthKind("yt", domain.WhatPAT), "invalid_auth_kind", `server "yt" does not use personal access tokens`},
		{domain.ErrInvalidState(), "invalid_state", "connect attempt is unknown or expired"},
		{domain.ErrExchangeFailed(), "exchange_failed", "the provider rejected the authorization"},
	} {
		de, ok := kernel.AsDomainError(c.err)
		if !ok || de.Code != c.code || c.err.Error() != c.text {
			t.Errorf("%v", c.err)
		}
	}
	if domain.CodeNotFound != "not_found" || domain.CodeInvalidAuthKind != "invalid_auth_kind" || domain.CodeInvalidState != "invalid_state" || domain.CodeExchangeFailed != "exchange_failed" || domain.CodeInvalidPAT != "invalid_pat" {
		t.Error("codes")
	}
}
