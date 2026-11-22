package domain

import (
	"slices"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// Event types written to the outbox (payload keys are part of the contract with search).
const (
	EventToolsChanged  = "catalog.tools_changed"
	EventToolsRemoved  = "catalog.tools_removed"
	EventServerChanged = "catalog.server_changed"
)

// ToolsChangedEvent is raised when tools were added, changed, revived, hidden or unhidden. Payload {server, refs}.
func ToolsChangedEvent(server string, refs []string) kernel.Event {
	return kernel.Event{Type: EventToolsChanged, Payload: map[string]any{"server": server, "refs": slices.Clone(refs)}}
}

// ToolsRemovedEvent is raised when upstream no longer offers these tools. Payload {server, refs}.
func ToolsRemovedEvent(server string, refs []string) kernel.Event {
	return kernel.Event{Type: EventToolsRemoved, Payload: map[string]any{"server": server, "refs": slices.Clone(refs)}}
}

// ServerChangedEvent is raised when the name, description, access or status of a server changed (or it was
// created): the search index must follow. Payload {server}.
func ServerChangedEvent(server string) kernel.Event {
	return kernel.Event{Type: EventServerChanged, Payload: map[string]any{"server": server}}
}
