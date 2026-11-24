package infra

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
)

// catalogAPI is the part of *catalog.Service the gateway reads.
type catalogAPI interface {
	GetToolFor(ctx context.Context, p catalog.Principal, ref string) (catalog.ToolView, error)
	GetServerForUpstream(ctx context.Context, slug string) (*catalog.ServerCallTarget, error)
	GetServerSecretFor(ctx context.Context, target catalog.ServerCallTarget) (kernel.Secret, catalog.SecretLookup, error)
}

// toolCatalog is app.ToolCatalog over the catalog's public API (createCatalogToolCatalog of the
// TypeScript server).
type toolCatalog struct{ api catalogAPI }

var _ app.ToolCatalog = toolCatalog{}

// NewToolCatalog is the gateway's view of the catalog.
func NewToolCatalog(svc *catalog.Service) app.ToolCatalog { return toolCatalog{api: svc} }

// ToolFor maps every business refusal of the catalog (not_found: unknown, hidden or removed tool,
// unknown, disabled or closed server; invalid_tool_ref: a malformed ref) to ok = false. Anything
// else is unexpected and returned.
func (c toolCatalog) ToolFor(ctx context.Context, p app.Principal, ref string) (catalog.ToolView, bool, error) {
	view, err := c.api.GetToolFor(ctx, p, ref)
	if err == nil {
		return view, true, nil
	}
	if de, isDomain := kernel.AsDomainError(err); isDomain && (de.Code == catalog.CodeNotFound || de.Code == catalog.CodeInvalidToolRef) {
		return catalog.ToolView{}, false, nil
	}
	return catalog.ToolView{}, false, err
}

// ServerFor is the active server's call target, nil when unknown or disabled.
func (c toolCatalog) ServerFor(ctx context.Context, slug string) (*catalog.ServerCallTarget, error) {
	return c.api.GetServerForUpstream(ctx, slug)
}

// ServerSecret is the decrypted shared secret for the target read earlier: found only while it
// belongs to that target's configuration.
func (c toolCatalog) ServerSecret(ctx context.Context, target catalog.ServerCallTarget) (kernel.Secret, catalog.SecretLookup, error) {
	return c.api.GetServerSecretFor(ctx, target)
}

// NewPersonalCredentials is the gateway's view of the connections module: the service itself,
// which already has the shape of app.PersonalCredentials (ResolveForSending carries the
// configuration the secret is for, MarkExpired).
func NewPersonalCredentials(svc *connections.Service) app.PersonalCredentials { return svc }
