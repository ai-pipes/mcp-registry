package app

import (
	"slices"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// toServerView is the only mapping from an aggregate to what leaves the module: no secret,
// sealed or not.
func toServerView(s *domain.Server, toolCount int) ServerView {
	return ServerView{
		Slug: s.Slug(), Name: s.Name(), Description: s.Description(), URL: s.URL(), Auth: s.Auth(),
		SecretSet: s.SecretSet(), Access: s.Access(), TimeoutMs: s.TimeoutMs(), DailyLimit: s.DailyLimit(),
		Status: s.Status(), LastSyncAt: s.LastSyncAt(), LastSyncError: s.LastSyncError(), ToolCount: toolCount,
	}
}

func toMemberServerView(s *domain.Server, toolCount int) MemberServerView {
	return MemberServerView{Slug: s.Slug(), Name: s.Name(), Description: s.Description(), ToolCount: toolCount, AuthKind: s.Auth().Kind}
}

func toToolView(t *domain.Tool) ToolView {
	return ToolView{
		Server: t.Server(), Name: t.Name(), Ref: t.Ref(), Title: t.Title(), Description: t.Description(),
		InputSchema: slices.Clone(t.InputSchema()), OutputSchema: slices.Clone(t.OutputSchema()),
		Annotations: t.Annotations(), Hidden: t.Hidden(),
	}
}

// toServerCallTarget holds the call parameters of a server, without its secret.
func toServerCallTarget(s *domain.Server) ServerCallTarget {
	return ServerCallTarget{Slug: s.Slug(), Name: s.Name(), URL: s.URL(), Auth: s.Auth(), TimeoutMs: s.TimeoutMs(), DailyLimit: s.DailyLimit()}
}
