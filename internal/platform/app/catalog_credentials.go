package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

// credentialResolver is what the adapter needs of the connections service
// (*connections.Service).
type credentialResolver interface {
	ResolveCredential(ctx context.Context, q connections.ResolveQuery) (connections.CredentialResult, error)
}

// errCredentialsNotBound answers a personal-credential request made before the connections
// service was bound: a programming error in the composition, reported as an ordinary error (the
// synchronisation records it as `internal`) instead of a nil dereference.
var errCredentialsNotBound = errors.New("sync credentials: the connections service is not bound")

// lateSyncCredentials is the catalog's SyncCredentials port over the public APIs of identity (is
// the owner still an enabled admin) and connections (the person's own secret). It holds no rules:
// whose connection may serve and what a failure means are the catalog's (SyncOwner in
// catalog/domain). It is the counterpart of apps/server/src/platform/upstream/sync-credentials.ts.
//
// Construction cycle: the catalog service needs this port, and the connections ServerDirectory
// needs the catalog service. The catalog is therefore built with this adapter first and the
// connections service is bound afterwards (bind), by newConnectionsWiring; until then
// PersonalSecret fails with errCredentialsNotBound.
type lateSyncCredentials struct {
	identity *identity.Service

	mu       sync.RWMutex
	resolver credentialResolver
}

var _ catalog.SyncCredentials = (*lateSyncCredentials)(nil)

// bind sets the connections service. A nil resolver or a second binding is a programming error.
func (c *lateSyncCredentials) bind(r credentialResolver) {
	if r == nil {
		panic("sync credentials: binding a nil connections service")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolver != nil {
		panic("sync credentials: the connections service is bound twice")
	}
	c.resolver = r
}

func (c *lateSyncCredentials) bound() credentialResolver {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.resolver
}

func (c *lateSyncCredentials) IsActiveAdmin(ctx context.Context, userID string) (bool, error) {
	return c.identity.IsUserActiveAdmin(ctx, userID)
}

// PersonalSecret is ResolveCredential for that person and server: a secret, or one of the three
// failures the catalog maps to its own (needs_connection and expired both to "an administrator
// needs to connect this server", unavailable to "the OAuth provider is unavailable"; unavailable
// is also what connections answers when the target this sync read no longer matches the server).
func (c *lateSyncCredentials) PersonalSecret(ctx context.Context, server catalog.ServerCallTarget, userID string) (catalog.PersonalCredential, error) {
	r := c.bound()
	if r == nil {
		return catalog.PersonalCredential{}, errCredentialsNotBound
	}
	// The secret goes to the target this sync read earlier: connections hands it out only while the
	// server is still configured as that target says (else `unavailable`, retry later).
	expect := connections.ServerConfigOf(server)
	res, err := r.ResolveCredential(ctx, connections.ResolveQuery{UserID: userID, Server: server.Slug, Expect: &expect})
	if err != nil {
		return catalog.PersonalCredential{}, err
	}
	switch res.Failure {
	case "":
		return catalog.PersonalCredential{Secret: res.Secret}, nil
	case connections.FailureNeedsConnection:
		return catalog.PersonalCredential{Failure: catalog.CredentialNeedsConnection}, nil
	case connections.FailureExpired:
		return catalog.PersonalCredential{Failure: catalog.CredentialExpired}, nil
	case connections.FailureUnavailable:
		return catalog.PersonalCredential{Failure: catalog.CredentialUnavailable}, nil
	default:
		// Never guess what a new failure means for a synchronisation.
		return catalog.PersonalCredential{}, fmt.Errorf("sync credentials: unknown credential failure %q", string(res.Failure))
	}
}
