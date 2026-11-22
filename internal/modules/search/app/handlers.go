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

var (
	errNoServer = errors.New("event payload has no server")
	errNoRefs   = errors.New("event payload has no refs")
)

// EventHandlers is the table of outbox handlers for the catalog events that change what is
// searchable, keyed by event type (exactly catalog.EventTypes()):
//
//   - tools_changed {server, refs}: IndexToolRefs, so a tool the admin hid, or that is gone, is
//     removed.
//   - tools_removed {refs}: removes the documents by ref without asking the catalog.
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
			server, err := serverOf(p)
			if err != nil {
				return err
			}
			refs, err := refsOf(p)
			if err != nil {
				return err
			}
			return IndexToolRefs(ctx, d, server, refs)
		},
		catalog.EventToolsRemoved: func(ctx context.Context, p map[string]any) error {
			refs, err := refsOf(p)
			if err != nil {
				return err
			}
			return RemoveTools(ctx, d, refs)
		},
		catalog.EventServerChanged: func(ctx context.Context, p map[string]any) error {
			server, err := serverOf(p)
			if err != nil {
				return err
			}
			return IndexTools(ctx, d, server)
		},
	}
}

func serverOf(p map[string]any) (string, error) {
	server, ok := p["server"].(string)
	if !ok {
		return "", errNoServer
	}
	return server, nil
}

// refsOf reads the refs of a payload as the outbox decodes it ([]any) or as a producer builds it
// ([]string); an empty list is fine.
func refsOf(p map[string]any) ([]string, error) {
	switch refs := p["refs"].(type) {
	case []string:
		return refs, nil
	case []any:
		out := make([]string, 0, len(refs))
		for _, r := range refs {
			s, ok := r.(string)
			if !ok {
				return nil, errNoRefs
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, errNoRefs
}
