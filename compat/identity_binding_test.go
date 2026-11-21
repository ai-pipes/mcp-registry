//go:build compat

package compat_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// TestIdentityOIDCBindingKept: a sign-in in progress survives a foreign callback (decided deviation
// oidc-binding-kept). Any site can make a browser navigate to /api/auth/callback?state=x&code=y.
// Reference 1.0.x clears the mcp_login binding cookie before checking it, so the person's genuine
// sign-in, still at the provider, then fails with invalid_state; the decided behaviour clears the
// cookie only for a callback whose state matches it. Nobody is signed in by the foreign callback
// either way.
func TestIdentityOIDCBindingKept(t *testing.T) {
	idp := fakes.StartIdP(t)
	s := harness.Start(t, harness.Options{OIDC: idp})

	a := beginIDPLogin(t, s, idp, identityLoginPerson(harness.Unique("victim"), "Vera", "mcp-users"), "")
	if a.Binding == "" {
		t.Fatal("the sign-in did not set the mcp_login binding cookie")
	}

	// The foreign callback arrives in the same browser before the person comes back from the provider.
	foreign := a.Client.Do(t, "GET", "/api/auth/callback?state=not-the-state-of-this-attempt&code=x", nil)
	if foreign.Status != 302 || foreign.Header.Get("Location") != "/?loginError=invalid_state" || cookieNamed(foreign, "mcp_session") != nil {
		t.Fatalf("foreign callback: %d Location %q session %v", foreign.Status, foreign.Header.Get("Location"), cookieNamed(foreign, "mcp_session") != nil)
	}

	// The genuine callback, same browser, same jar (a cleared cookie is dropped by the jar as by a browser).
	cb := a.callback(t)
	if s.Fixed(harness.DevOIDCBindingKept) {
		if cb.Status != 302 || cb.Header.Get("Location") != "/" || cookieNamed(cb, "mcp_session") == nil {
			t.Fatalf("decided: the genuine callback after a foreign one must sign in, got %d Location %q session %v",
				cb.Status, cb.Header.Get("Location"), cookieNamed(cb, "mcp_session") != nil)
		}
		if me := whoami(t, a.Client); me.Name != "Vera" {
			t.Fatalf("me: %+v", me)
		}
		return
	}
	if cb.Status != 302 || cb.Header.Get("Location") != "/?loginError=invalid_state" || cookieNamed(cb, "mcp_session") != nil {
		t.Fatalf("reference: the genuine callback after a foreign one must be refused with invalid_state, got %d Location %q session %v",
			cb.Status, cb.Header.Get("Location"), cookieNamed(cb, "mcp_session") != nil)
	}
	// The stack is not broken: a fresh sign-in works.
	c := loginViaIDP(t, s, idp, identityLoginPerson(harness.Unique("again"), "Vera", "mcp-users"))
	if me := whoami(t, c); me.Name != "Vera" {
		t.Fatalf("me after a fresh sign-in: %+v", me)
	}
}
