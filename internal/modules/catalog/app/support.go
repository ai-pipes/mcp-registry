package app

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

// CodeInvalidRequest is the code of a request that breaks the REST contract (the shape and the
// limits the web form also enforces); the message is "<dotted.path>: <reason>".
const CodeInvalidRequest = "invalid_request"

// actorOf maps the identity principal onto the person the catalog rules know.
func actorOf(p Principal) domain.Actor {
	role := domain.RoleMember
	if p.Role == identity.RoleAdmin {
		role = domain.RoleAdmin
	}
	return domain.Actor{Role: role, Groups: p.Groups}
}

// requireAdmin: catalog administration is for admins only.
func requireAdmin(p Principal) error {
	if domain.IsAdmin(actorOf(p)) {
		return nil
	}
	return kernel.NewDomainError(domain.CodeForbidden, "administrators only")
}

func serverNotFound(slug string) error {
	return kernel.NewDomainError(domain.CodeNotFound, `server "`+slug+`" not found`)
}

func toolNotFound(ref string) error {
	return kernel.NewDomainError(domain.CodeNotFound, `tool "`+ref+`" not found`)
}

// load reads a server for an edit; an unknown slug is not_found.
func load(ctx context.Context, deps Deps, slug string) (*domain.Server, error) {
	server, err := deps.Servers.FindBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, serverNotFound(slug)
	}
	return server, nil
}

// record writes one audit record about the acting person. The change it describes is already
// committed, so a cancelled request must not lose the record: it is written under a context that
// ignores the caller's cancellation.
func record(ctx context.Context, deps Deps, p Principal, action, target string, meta map[string]any) error {
	actor, tgt := p.UserID, target
	return deps.Audit.Record(context.WithoutCancel(ctx), kernel.AuditEntry{ActorID: &actor, Action: action, Target: &tgt, Meta: meta})
}

// viewWithCount is the view of the server with its current visible tool count.
func viewWithCount(ctx context.Context, deps Deps, server *domain.Server) (ServerView, error) {
	listing, err := deps.Servers.List(ctx)
	if err != nil {
		return ServerView{}, err
	}
	count := 0
	for _, l := range listing {
		if l.Server.Slug() == server.Slug() {
			count = l.ToolCount
			break
		}
	}
	return toServerView(server, count), nil
}
