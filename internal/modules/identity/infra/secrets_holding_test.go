package infra_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/infra"
)

// The OIDC client secret shows nowhere: not in the adapter's configuration, nor in the adapter
// before or after discovery, however they are held.
func TestSecretCarryingTypesAreUnprintableHoweverHeld(t *testing.T) {
	r := newRig(t, nil)
	forbidden := secrettest.Strings(t, r.idp.ClientSecret)
	secrettest.AssertUnprintable(t, "OIDCConfig", oidcConfig(r.idp.Issuer, r.idp.ClientID, r.idp.ClientSecret), forbidden)
	secrettest.AssertUnprintable(t, "OIDCProvider before discovery", r.prov, forbidden)
	r.login(t)
	secrettest.AssertUnprintable(t, "OIDCProvider after discovery", r.prov, forbidden)
}

func oidcConfig(issuer, clientID, secret string) infra.OIDCConfig {
	return infra.OIDCConfig{Issuer: issuer, ClientID: clientID, ClientSecret: kernel.NewSecret(secret), RedirectURI: redirectURI}
}
