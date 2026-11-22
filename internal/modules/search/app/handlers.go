package app

import (
	"context"
	"errors"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
)

// EventHandler handles one outbox event; it has the shape of the platform's outbox handler, which
// the composition adapts. Delivery is at least once and handlers read the current catalog state,
// so they are idempotent. A handler that returns an error is retried by the outbox.
type EventHandler = func(ctx context.Context, payload map[string]any) error

// ErrBadPayload is a catalog event whose payload is not the shape the catalog writes.
var ErrBadPayload = errors.New("event payload is malformed")

// EventHandlers is the table of outbox handlers for the catalog events that change what is
// searchable, keyed by event type (exactly catalog.EventTypes()):
//
//   - tools_changed {server, refs}: IndexToolRefs, so a tool the admin hid, or that is gone, is
//     removed.
//   - tools_removed {server, refs}: removes the documents by ref without asking the catalog.
//     Reproduced from TypeScript on purpose (owner decision pending): a removal delivered late,
//     after the tool came back, deletes a live document, and nothing but the next event for that
//     tool or `reindex` restores the row (the sweep only fills missing vectors of existing rows).
//   - server_changed {server}: IndexTools for the whole server (name, description and status are
//     part of every passage); a disabled server loses all its documents.
//
// A handler fails (the event is retried, up to the outbox's attempts) when its payload is
// malformed, when the catalog or the index fail, or when the embedder failed for at least one
// tool that had to be indexed; the text rows of that run are written all the same. It does not
// fail when no tool needs a vector.
func EventHandlers(d Deps) map[string]EventHandler {
	return map[string]EventHandler{
		catalog.EventToolsChanged: func(ctx context.Context, p map[string]any) error {
			ev, ok := catalog.ParseToolsEvent(p)
			if !ok {
				return ErrBadPayload
			}
			return IndexToolRefs(ctx, d, ev.Server, ev.Refs)
		},
		catalog.EventToolsRemoved: func(ctx context.Context, p map[string]any) error {
			ev, ok := catalog.ParseToolsEvent(p)
			if !ok {
				return ErrBadPayload
			}
			return RemoveTools(ctx, d, ev.Refs)
		},
		catalog.EventServerChanged: func(ctx context.Context, p map[string]any) error {
			server, ok := catalog.ParseServerEvent(p)
			if !ok {
				return ErrBadPayload
			}
			return IndexTools(ctx, d, server)
		},
	}
}
