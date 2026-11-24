package app

import (
	"context"
	"slices"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// ListServersFor returns the active servers the person may use, in the member's (reduced) view.
func ListServersFor(ctx context.Context, deps Deps, p Principal) ([]MemberServerView, error) {
	listing, err := deps.Servers.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []MemberServerView{}
	for _, l := range listing {
		if l.Server.CanBeUsedBy(actorOf(p)) {
			out = append(out, toMemberServerView(l.Server, l.ToolCount))
		}
	}
	return out, nil
}

// ListAllServers is the admin screen: every server, disabled ones included, by slug.
func ListAllServers(ctx context.Context, deps Deps, p Principal) ([]ServerView, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	listing, err := deps.Servers.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ServerView, 0, len(listing))
	for _, l := range listing {
		out = append(out, toServerView(l.Server, l.ToolCount))
	}
	return out, nil
}

// GetServer is the admin's view of one server. An unknown slug is not_found with the message
// `server not found` (without the slug), as GET /api/servers/:slug answers.
func GetServer(ctx context.Context, deps Deps, p Principal, slug string) (ServerView, error) {
	if err := requireAdmin(p); err != nil {
		return ServerView{}, err
	}
	listing, err := deps.Servers.List(ctx)
	if err != nil {
		return ServerView{}, err
	}
	for _, l := range listing {
		if l.Server.Slug() == slug {
			return toServerView(l.Server, l.ToolCount), nil
		}
	}
	return ServerView{}, kernel.NewDomainError(domain.CodeNotFound, "server not found")
}

// SecretLookup is what GetServerSecretFor found.
type SecretLookup string

const (
	// SecretFound means the secret belongs to the caller's configuration and is returned.
	SecretFound SecretLookup = "found"
	// SecretNotSet means the caller's configuration holds, but no secret is set.
	SecretNotSet SecretLookup = "not_set"
	// SecretMoved means the server no longer sends its secret where the caller's target would (another
	// auth kind; for shared_secret another URL; for oauth_user another token URL, revoke URL or
	// client ID), or it is gone. Nothing is opened or returned.
	SecretMoved SecretLookup = "moved"
)

// GetServerSecretFor is the opened server secret for a caller that holds a configuration read
// earlier (target): the secret and the configuration it is judged against come from ONE read of
// the server, and the secret is returned only when it belongs to the target's destination
// (domain Server.SecretGoesTo). An edit that moves the secret clears it in the same write, so the
// stored secret always belongs to the stored configuration and there is no window between two
// reads (nor an ABA: a configuration that changed and came back holds only a secret set for it).
// The status is not judged (the caller has its own rule). A secret no key opens is an error.
func GetServerSecretFor(ctx context.Context, deps Deps, target ServerCallTarget) (string, SecretLookup, error) {
	server, err := deps.Servers.FindBySlug(ctx, target.Slug)
	if err != nil {
		return "", "", err
	}
	if server == nil || !server.SecretGoesTo(target.URL, target.Auth) {
		return "", SecretMoved, nil
	}
	sealed := server.SealedSecret()
	if sealed == nil {
		return "", SecretNotSet, nil
	}
	plain, err := deps.Box.Open(sealed)
	if err != nil {
		return "", "", err
	}
	return plain, SecretFound, nil
}

// GetServerForUpstream returns the call parameters of an active server; nil when it is unknown or
// disabled.
func GetServerForUpstream(ctx context.Context, deps Deps, slug string) (*ServerCallTarget, error) {
	server, err := deps.Servers.FindBySlug(ctx, slug)
	if err != nil || server == nil || server.Status() != domain.StatusActive {
		return nil, err
	}
	target := toServerCallTarget(server)
	return &target, nil
}

// ListCatalogFor is what a person may browse: the active servers they can use and those servers'
// visible tools (not hidden, not removed), by slug and tool name. No URL, no auth config.
func ListCatalogFor(ctx context.Context, deps Deps, p Principal) ([]CatalogEntry, error) {
	listing, err := deps.Servers.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []CatalogEntry{}
	for _, l := range listing {
		server := l.Server
		if !server.CanBeUsedBy(actorOf(p)) {
			continue
		}
		tools, err := deps.Tools.ListByServer(ctx, server.Slug())
		if err != nil {
			return nil, err
		}
		entry := CatalogEntry{Slug: server.Slug(), Name: server.Name(), Description: server.Description(), Tools: []CatalogTool{}}
		for _, t := range tools {
			if t.Visible() {
				entry.Tools = append(entry.Tools, CatalogTool{Ref: t.Ref(), Title: t.Title(), Description: t.Description(), Annotations: t.Annotations()})
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// GetToolFor returns a tool the person may use right now. One not_found for everything else
// (unknown, hidden, removed, server disabled or closed to the person), so presence is not
// revealed; a malformed ref is invalid_tool_ref.
func GetToolFor(ctx context.Context, deps Deps, p Principal, ref string) (ToolView, error) {
	serverSlug, name, err := domain.ParseToolRef(ref)
	if err != nil {
		return ToolView{}, err
	}
	server, err := deps.Servers.FindBySlug(ctx, serverSlug)
	if err != nil {
		return ToolView{}, err
	}
	if server == nil || !server.CanBeUsedBy(actorOf(p)) {
		return ToolView{}, toolNotFound(ref)
	}
	tool, err := deps.Tools.Find(ctx, serverSlug, name)
	if err != nil {
		return ToolView{}, err
	}
	if tool == nil || !tool.Visible() {
		return ToolView{}, toolNotFound(ref)
	}
	return toToolView(tool), nil
}

// ListServerTools is the admin screen of a server's tools, by name: hidden ones included, removed
// ones not. An unknown server is not_found.
func ListServerTools(ctx context.Context, deps Deps, p Principal, slug string) ([]ToolView, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	server, err := deps.Servers.FindBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, serverNotFound(slug)
	}
	tools, err := deps.Tools.ListByServer(ctx, slug)
	if err != nil {
		return nil, err
	}
	out := []ToolView{}
	for _, t := range tools {
		if !t.IsRemoved() {
			out = append(out, toToolView(t))
		}
	}
	return out, nil
}

// ListToolsForIndex is what the search module indexes for a server: its visible tools plus the
// server's name and description. A disabled server has no searchable tools. nil when unknown.
func ListToolsForIndex(ctx context.Context, deps Deps, slug string) (*ServerIndexSource, error) {
	server, err := deps.Servers.FindBySlug(ctx, slug)
	if err != nil || server == nil {
		return nil, err
	}
	src := &ServerIndexSource{Slug: server.Slug(), Name: server.Name(), Description: server.Description(), Tools: []IndexedTool{}}
	if server.Status() != domain.StatusActive {
		return src, nil
	}
	tools, err := deps.Tools.ListByServer(ctx, slug)
	if err != nil {
		return nil, err
	}
	for _, t := range tools {
		if t.Visible() {
			src.Tools = append(src.Tools, IndexedTool{
				Ref: t.Ref(), Name: t.Name(), Title: t.Title(), Description: t.Description(), InputSchema: slices.Clone(t.InputSchema()),
			})
		}
	}
	return src, nil
}

// ListServerSlugs returns the slug of every server, disabled ones included (the index must also
// clean up after them).
func ListServerSlugs(ctx context.Context, deps Deps) ([]string, error) {
	listing, err := deps.Servers.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(listing))
	for _, l := range listing {
		out = append(out, l.Server.Slug())
	}
	return out, nil
}
