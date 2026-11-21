package app

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// SetToolHiddenCommand hides or unhides a tool by its reference.
type SetToolHiddenCommand struct {
	Principal Principal
	Ref       string
	Hidden    bool
}

// SetToolHidden is for admins only. A malformed ref is invalid_tool_ref; an unknown tool, or one the
// upstream no longer offers, is not_found `tool "<ref>" not found` (also for an unknown server).
// Only a real change is saved, with its tools_changed event in the same write, and audited as
// tool.hide / tool.unhide (target: the ref).
func SetToolHidden(ctx context.Context, deps Deps, cmd SetToolHiddenCommand) error {
	if err := requireAdmin(cmd.Principal); err != nil {
		return err
	}
	server, name, err := domain.ParseToolRef(cmd.Ref)
	if err != nil {
		return err
	}
	tool, err := deps.Tools.Find(ctx, server, name)
	if err != nil {
		return err
	}
	if tool == nil {
		return toolNotFound(cmd.Ref)
	}
	changed, err := tool.SetHidden(cmd.Hidden, deps.Clock.Now())
	if err != nil || !changed {
		return err
	}
	if err := deps.Tools.Save(ctx, tool, tool.PullEvents()); err != nil {
		return err
	}
	action := "tool.unhide"
	if cmd.Hidden {
		action = "tool.hide"
	}
	return record(ctx, deps, cmd.Principal, action, tool.Ref(), nil)
}
