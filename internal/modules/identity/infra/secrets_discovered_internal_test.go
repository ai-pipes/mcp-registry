package infra

import (
	"context"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/infra/oidctest"
)

// The discovered provider metadata is cached for the life of the process; its OAuth configuration
// holds no client secret (the secret is put into a copy for the one request that sends it).
func TestDiscoveredHoldsNoClientSecret(t *testing.T) {
	idp := oidctest.Start(t)
	p := NewOIDCProvider(OIDCConfig{Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: kernel.NewSecret(idp.ClientSecret), RedirectURI: "http://gateway.test/cb"})
	d, err := p.discovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.oauth.ClientSecret != "" {
		t.Fatal("the cached OAuth configuration holds the client secret")
	}
	secrettest.AssertUnprintable(t, "discovered", d, secrettest.Strings(t, idp.ClientSecret))
}
