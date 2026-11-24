package infra

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
)

// What the OAuth client holds of a token answer (the raw body and the tokens read from it) shows
// no token, however it is held.
func TestSecretCarryingTypesAreUnprintableHoweverHeld(t *testing.T) {
	const access, refresh = "ZZ-access-token-00001", "ZZ-refresh-token-0002"
	forbidden := secrettest.Strings(t, access, refresh)
	body := []byte(`{"access_token":"` + access + `","token_type":"bearer","refresh_token":"` + refresh + `"}`)
	res := newResponse(200, nil, body)
	tok, err := tokenAnswer(res)
	if err != nil {
		t.Fatal(err)
	}
	secrettest.AssertUnprintable(t, "response", res, forbidden)
	secrettest.AssertUnprintable(t, "tokens", tok, forbidden)
}
