package domain_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel/secrettest"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// The sealed server secret is ciphertext, which is not printed either: neither the stored form
// nor the aggregate shows it, however they are held.
func TestSecretCarryingTypesAreUnprintableHoweverHeld(t *testing.T) {
	sealed := []byte("ZZ-sealed-server-secret-0001")
	forbidden := secrettest.Forbidden(t, sealed)
	st := domain.ServerState{Slug: "gitea", Name: "Gitea", URL: "https://g.example/mcp", SealedSecret: sealedBytes(sealed), CreatedAt: t0, UpdatedAt: t0}
	secrettest.AssertUnprintable(t, "ServerState", st, forbidden)
	secrettest.AssertUnprintable(t, "Server", *domain.RehydrateServer(st), forbidden)
}
