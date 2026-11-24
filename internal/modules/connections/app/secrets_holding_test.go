package app_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
)

// Every type of the use cases that carries a token, a code, a state, a verifier or the client
// secret shows none of it, however it is held (secrettest.Holdings).
func TestSecretCarryingTypesAreUnprintableHoweverHeld(t *testing.T) {
	const access, refresh, client, state, code, verifier, pat = "ZZ-access-token-00001", "ZZ-refresh-token-0002",
		"ZZ-client-secret-0003", "ZZ-oauth-state-000004", "ZZ-oauth-code-0000005", "ZZ-pkce-verifier-0006", "ZZ-personal-token-007"
	forbidden := secrettest.Strings(t, access, refresh, client, state, code, verifier, pat)
	secrettest.AssertUnprintable(t, "TokenSet", app.TokenSet{AccessToken: kernel.NewSecret(access), RefreshToken: kernel.NewSecret(refresh)}, forbidden)
	secrettest.AssertUnprintable(t, "ClientSecret", app.NewClientSecret(client), forbidden)
	secrettest.AssertUnprintable(t, "CredentialResult", app.CredentialResult{Secret: kernel.NewSecret(access)}, forbidden)
	secrettest.AssertUnprintable(t, "AuthorizeInput", app.AuthorizeInput{RedirectURI: "https://g/cb", State: kernel.NewSecret(state), CodeChallenge: "c"}, forbidden)
	secrettest.AssertUnprintable(t, "ExchangeInput", app.ExchangeInput{Code: kernel.NewSecret(code), CodeVerifier: kernel.NewSecret(verifier), RedirectURI: "https://g/cb"}, forbidden)
	secrettest.AssertUnprintable(t, "CompleteConnectCommand", app.CompleteConnectCommand{Principal: alice, State: kernel.NewSecret(state), Code: kernel.NewSecret(code)}, forbidden)
	secrettest.AssertUnprintable(t, "SavePATCommand", app.SavePATCommand{Principal: alice, Server: "yt", Token: kernel.NewSecret(pat)}, forbidden)
}
