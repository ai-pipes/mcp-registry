//go:build compat

package compat_test

import (
	"slices"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// ---- the server config changes after connecting ----

func TestConnectionsConfigChange(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)

	// Where a token must never be sent after the change.
	other := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	otherProvider := connWatch(fakes.StartOAuthProvider(t))

	expiring := []struct {
		name   string
		change func(b map[string]any)
	}{
		{"the upstream URL", func(b map[string]any) { b["url"] = other.URL }},
		{"the authorize URL", func(b map[string]any) { connOAuthOf(b)["authorizeUrl"] = otherProvider.AuthorizeURL }},
		{"the token URL", func(b map[string]any) { connOAuthOf(b)["tokenUrl"] = otherProvider.TokenURL }},
		{"the client id", func(b map[string]any) { connOAuthOf(b)["clientId"] = "another-client" }},
	}
	for _, c := range expiring {
		t.Run("a changed "+c.name+" expires the connection and the token never goes to the new place", func(t *testing.T) {
			env := connNewOAuth(t, s, admin, connOpts{})
			p, issued := connConnected(t, env)
			other.Reset()
			// Control: before the change the call works.
			connExpectOK(t, "call before the change", p.call(t, env))
			connEqual(t, "token before the change", connAuths(env.up, "Authorization"), connBearer(issued.AccessToken))
			env.up.Reset()

			// An expiring token: were the fingerprint ignored, a refresh would go to the (new) token endpoint.
			connExpireIn(t, s, p.id, env.slug, 59)
			provRequests, otherRequests := len(env.provider.Requests()), len(otherProvider.Requests())
			connChangeServer(t, admin, env.slug, c.change)
			if got := connStatus(t, p.c, env.slug); got != "expired" {
				t.Fatalf("status right after the change: %q, want expired", got)
			}
			connExpectText(t, "call after the change", p.call(t, env), connTextExpired(s, env.slug))
			if got := connStatus(t, p.c, env.slug); got != "expired" {
				t.Fatalf("status after the call: %q", got)
			}
			if row := connRowOf(t, s, p.id, env.slug); row.Status != "expired" {
				t.Fatalf("row status %q", row.Status)
			}
			connExpectExpiredAudit(t, s, p.id, env.slug, "config_changed", 1)
			if outcomes := connOutcomes(t, admin, p.id, env.slug); !slices.Equal(outcomes, []string{"ok", "denied:expired"}) {
				t.Fatalf("call log: %v", outcomes)
			}
			// No token request anywhere, no call at either upstream.
			if n := len(env.provider.Requests()) - provRequests; n != 0 {
				t.Fatalf("the old provider got %d token request(s) after the change", n)
			}
			if n := len(otherProvider.Requests()) - otherRequests; n != 0 {
				t.Fatalf("the new provider got %d token request(s) after the change", n)
			}
			if n := len(env.up.Requests()); n != 0 {
				t.Fatalf("the old upstream got %d request(s)", n)
			}
			for _, r := range other.Requests() {
				if r.Header.Get("Authorization") != "" {
					t.Fatalf("the new upstream received a credential: %q", harness.Redact(r.Header.Get("Authorization")))
				}
			}
			// Disconnect deletes without revoking, anywhere.
			if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
				t.Fatalf("DELETE: %d", r.Status)
			}
			if n := len(env.provider.RevokeAttempts()) + len(otherProvider.RevokeAttempts()); n != 0 {
				t.Fatalf("%d revoke attempt(s) after the config changed", n)
			}
			if _, ok := connGetRow(t, s, p.id, env.slug); ok {
				t.Fatal("the row survived the disconnect")
			}
		})
	}

	t.Run("after a changed upstream URL a reconnect works and the new token goes to the new place", func(t *testing.T) {
		env := connNewOAuth(t, s, admin, connOpts{})
		p, issued := connConnected(t, env)
		other.Reset()
		connChangeServer(t, admin, env.slug, func(b map[string]any) { b["url"] = other.URL })
		connExpectText(t, "call after the change", p.call(t, env), connTextExpired(s, env.slug))
		fresh := connConnect(t, env, p.c)
		connExpectOK(t, "call after reconnecting", p.call(t, env))
		connEqual(t, "credentials at the new upstream", connAuths(other, "Authorization"), connBearer(fresh.AccessToken))
		if n := len(env.up.Requests()); n != 0 {
			t.Fatalf("the old upstream got %d request(s)", n)
		}
		if fresh.AccessToken == issued.AccessToken {
			t.Fatal("reconnecting did not issue a new token")
		}
	})

	t.Run("other scopes or a changed or removed revoke URL expire nobody; a changed revoke URL is never used", func(t *testing.T) {
		changes := []struct {
			name         string
			revokeAtOnce bool // the revoke URL is unchanged, so disconnect still revokes at it
			change       func(env *connEnv, b map[string]any)
		}{
			{"scopes", true, func(_ *connEnv, b map[string]any) { connOAuthOf(b)["scopes"] = []string{"repo", "user"} }},
			{"revoke URL to another provider", false, func(_ *connEnv, b map[string]any) { connOAuthOf(b)["revokeUrl"] = otherProvider.RevokeURL }},
			{"revoke URL to the same endpoint with a query", false, func(env *connEnv, b map[string]any) { connOAuthOf(b)["revokeUrl"] = env.provider.RevokeURL + "?v=2" }},
			{"revoke URL removed", false, func(_ *connEnv, b map[string]any) { delete(connOAuthOf(b), "revokeUrl") }},
		}
		for _, c := range changes {
			env := connNewOAuth(t, s, admin, connOpts{})
			p, issued := connConnected(t, env)
			connExpireIn(t, s, p.id, env.slug, 59)
			connChangeServer(t, admin, env.slug, func(b map[string]any) { c.change(env, b) })
			if got := connStatus(t, p.c, env.slug); got != "ready" {
				t.Fatalf("%s: status %q, want ready", c.name, got)
			}
			// The expiring token refreshes at the (unchanged) token endpoint: the connection is alive.
			connExpectOK(t, c.name+": call", p.call(t, env))
			if got := connRefreshTokens(env.provider); len(got) == 0 || got[len(got)-1] != issued.RefreshToken {
				t.Fatalf("%s: the refresh did not happen at the token endpoint", c.name)
			}
			if n := len(connAudit(t, s, "connection.expired", p.id, env.slug)); n != 0 {
				t.Fatalf("%s: connection.expired recorded", c.name)
			}
			otherBefore := len(otherProvider.RevokeAttempts())
			if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
				t.Fatalf("%s: DELETE %d", c.name, r.Status)
			}
			if c.revokeAtOnce {
				// Control for the cases below: an unchanged revoke URL is used.
				connEqual(t, c.name+": revoked", env.provider.Revoked(), []string{issued.RefreshToken})
			} else if n := len(env.provider.RevokeAttempts()); n != 0 {
				t.Fatalf("%s: %d revoke attempt(s) at the old revoke URL, want none (the URL changed)", c.name, n)
			}
			if n := len(otherProvider.RevokeAttempts()) - otherBefore; n != 0 {
				t.Fatalf("%s: %d revoke attempt(s) at the new revoke URL, a token goes only where it was issued", c.name, n)
			}
			if _, ok := connGetRow(t, s, p.id, env.slug); ok {
				t.Fatalf("%s: the row survived", c.name)
			}
		}
	})

	t.Run("a revoke URL added after the token was issued is not used either", func(t *testing.T) {
		env := connNewOAuth(t, s, admin, connOpts{})
		// A server whose OAuth settings had no revoke URL when the person connected.
		noRevoke := connNewOAuth(t, s, admin, connOpts{server: []fakes.ServerOpt{fakes.WithoutRevokeURL()}})
		p, _ := connConnected(t, noRevoke)
		if row := connRowOf(t, s, p.id, noRevoke.slug); row.RevokeURL != nil {
			t.Fatalf("revoke_url %v, want null", row.RevokeURL)
		}
		connChangeServer(t, admin, noRevoke.slug, func(b map[string]any) { connOAuthOf(b)["revokeUrl"] = noRevoke.provider.RevokeURL })
		if got := connStatus(t, p.c, noRevoke.slug); got != "ready" {
			t.Fatalf("status: %q", got)
		}
		if r := p.c.Do(t, "DELETE", "/api/connections/"+noRevoke.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d", r.Status)
		}
		if n := len(noRevoke.provider.RevokeAttempts()); n != 0 {
			t.Fatalf("%d revoke attempt(s) at a revoke URL that did not exist at issue time", n)
		}
		// Control: a revoke URL that never changed is used (the connect flow test pins the token).
		q, issued := connConnected(t, env)
		if r := q.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d", r.Status)
		}
		connEqual(t, "revoked", env.provider.Revoked(), []string{issued.RefreshToken})
	})
}

// ---- revoke failures ----

func TestConnectionsDisconnectIgnoresRevokeFailures(t *testing.T) {
	s := harness.Shared(t)
	admin := s.Admin(t)
	connLeakCheck(t, s, admin)
	withRevokeURL := func(u string) fakes.ServerOpt {
		return func(b map[string]any) { connOAuthOf(b)["revokeUrl"] = u }
	}

	t.Run("a revoke endpoint answering 500", func(t *testing.T) {
		failing := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		failing.FailWith(500)
		env := connNewOAuth(t, s, admin, connOpts{server: []fakes.ServerOpt{withRevokeURL(failing.URL)}})
		p, _ := connConnected(t, env)
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d %s", r.Status, r.Body)
		}
		if n := len(failing.Requests()); n != 1 {
			t.Fatalf("the revoke endpoint got %d request(s), want the one attempt", n)
		}
		if _, ok := connGetRow(t, s, p.id, env.slug); ok {
			t.Fatal("the row survived a failed revoke")
		}
		if got := connStatus(t, p.c, env.slug); got != "missing" {
			t.Fatalf("status: %q", got)
		}
	})

	t.Run("a revoke endpoint nobody listens on", func(t *testing.T) {
		env := connNewOAuth(t, s, admin, connOpts{server: []fakes.ServerOpt{withRevokeURL("http://host.docker.internal:1/revoke")}})
		p, _ := connConnected(t, env)
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d %s", r.Status, r.Body)
		}
		if _, ok := connGetRow(t, s, p.id, env.slug); ok {
			t.Fatal("the row survived")
		}
	})

	t.Run("a revoke endpoint refusing the client's credentials", func(t *testing.T) {
		env := connNewOAuth(t, s, admin, connOpts{})
		p, issued := connConnected(t, env)
		if r := admin.Do(t, "PUT", "/api/servers/"+env.slug+"/secret", map[string]string{"secret": "not-the-secret"}); r.Status != 204 {
			t.Fatalf("PUT secret: %d", r.Status)
		}
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d", r.Status)
		}
		attempts := env.provider.RevokeAttempts()
		if len(attempts) != 1 || attempts[0].Accepted || attempts[0].Token != issued.RefreshToken {
			t.Fatalf("revoke attempts: %+v, want one refused attempt for the refresh token", attempts)
		}
		if len(env.provider.Revoked()) != 0 {
			t.Fatal("a refused revoke is recorded as accepted")
		}
		if _, ok := connGetRow(t, s, p.id, env.slug); ok {
			t.Fatal("the row survived")
		}
	})

	t.Run("one's own credentials can be removed after the server is disabled", func(t *testing.T) {
		env := connNewOAuth(t, s, admin, connOpts{})
		p, _ := connConnected(t, env)
		connSetStatus(t, admin, env.slug, "disabled")
		if r := p.c.Do(t, "DELETE", "/api/connections/"+env.slug, nil); r.Status != 204 {
			t.Fatalf("DELETE: %d", r.Status)
		}
		if _, ok := connGetRow(t, s, p.id, env.slug); ok {
			t.Fatal("the row survived")
		}
	})
}
