package app

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

// personalSyncCredentials is the catalog's SyncCredentials port over the public APIs of identity
// (is the owner still an enabled admin: the real check) and connections (the person's own secret).
// It holds no rules: whose connection may serve and what a failure means are the catalog's
// (SyncOwner in catalog/domain). It is the counterpart of
// apps/server/src/platform/upstream/sync-credentials.ts.
//
// REPLACED IN MILESTONE 5: there is no connections module yet, so nobody has a personal
// connection and PersonalSecret answers needs_connection for every oauth_user and pat_user
// server. The synchronisation of such a server then fails with needs_connection (HTTP 400, "An
// administrator needs to connect this server"), exactly what TypeScript answers when no admin has
// connected.
type personalSyncCredentials struct {
	identity *identity.Service
}

var _ catalog.SyncCredentials = personalSyncCredentials{}

func (c personalSyncCredentials) IsActiveAdmin(ctx context.Context, userID string) (bool, error) {
	return c.identity.IsUserActiveAdmin(ctx, userID)
}

// PersonalSecret is the stub of milestone 3: no connections module, so no connection.
func (personalSyncCredentials) PersonalSecret(context.Context, catalog.ServerCallTarget, string) (catalog.PersonalCredential, error) {
	return catalog.PersonalCredential{Failure: domain.CredentialNeedsConnection}, nil
}
