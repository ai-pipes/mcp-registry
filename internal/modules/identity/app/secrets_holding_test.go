package app_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
)

// Every type of the identity use cases that carries a session id, an sk_ token, a sign-in code or
// an OIDC state, code, verifier or nonce shows none of it, however it is held.
func TestSecretCarryingTypesAreUnprintableHoweverHeld(t *testing.T) {
	const state, code, verifier, nonce, link, session, skValue = "ZZ-oidc-state-0000001", "ZZ-oidc-code-00000002",
		"ZZ-pkce-verifier-0003", "ZZ-oidc-nonce-0000004", "ll_ZZ-login-code-0005", "ZZ-session-id-0000006", "sk_ZZ-api-token-00007"
	forbidden := secrettest.Strings(t, state, code, verifier, nonce, link, session, skValue)
	secrettest.AssertUnprintable(t, "OIDCAuthorizationInput", app.OIDCAuthorizationInput{State: kernel.NewSecret(state), CodeChallenge: "c", Nonce: kernel.NewSecret(nonce)}, forbidden)
	secrettest.AssertUnprintable(t, "OIDCExchangeInput", app.OIDCExchangeInput{Code: kernel.NewSecret(code), CodeVerifier: kernel.NewSecret(verifier), Nonce: kernel.NewSecret(nonce)}, forbidden)
	secrettest.AssertUnprintable(t, "PendingLogin", app.PendingLogin{State: kernel.NewSecret(state), CodeVerifier: kernel.NewSecret(verifier), Nonce: kernel.NewSecret(nonce), ReturnTo: "/"}, forbidden)
	secrettest.AssertUnprintable(t, "IssuedLink", app.IssuedLink{Code: kernel.NewSecret(link)}, forbidden)
	secrettest.AssertUnprintable(t, "CompleteLinkLoginInput", app.CompleteLinkLoginInput{Code: kernel.NewSecret(link), IP: "10.0.0.1"}, forbidden)
	secrettest.AssertUnprintable(t, "CompleteOIDCLoginInput", app.CompleteOIDCLoginInput{State: kernel.NewSecret(state), Code: kernel.NewSecret(code), IP: "10.0.0.1"}, forbidden)
	secrettest.AssertUnprintable(t, "OIDCLoginResult", app.OIDCLoginResult{SessionID: kernel.NewSecret(session), ReturnTo: "/"}, forbidden)
	secrettest.AssertUnprintable(t, "CreatedToken", app.CreatedToken{ID: "1", Token: kernel.NewSecret(skValue), Prefix: "sk_ZZ"}, forbidden)
}
