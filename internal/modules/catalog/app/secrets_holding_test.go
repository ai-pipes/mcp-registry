package app_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
)

// The commands that carry a server secret and the personal credential a synchronisation borrows
// show none of it, however they are held.
func TestSecretCarryingTypesAreUnprintableHoweverHeld(t *testing.T) {
	const secret, personal = "ZZ-server-secret-00001", "ZZ-personal-token-0002"
	forbidden := secrettest.Strings(t, secret, personal)
	s := secret
	secrettest.AssertUnprintable(t, "CreateServerCommand", app.CreateServerCommand{Principal: admin, Secret: kernel.NewSecret(s)}, forbidden)
	secrettest.AssertUnprintable(t, "SetSecretCommand", app.SetSecretCommand{Principal: admin, Slug: "gitea", Secret: kernel.NewSecret(s)}, forbidden)
	secrettest.AssertUnprintable(t, "PersonalCredential", app.PersonalCredential{Secret: kernel.NewSecret(personal)}, forbidden)
}
