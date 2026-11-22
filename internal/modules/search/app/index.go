package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

// ErrBadDimensions is an embedder answer that is not Dimensions numbers long; it is treated like
// any other embedder failure (UpsertEmbedFailed).
var ErrBadDimensions = errors.New("embedder returned a vector of the wrong length")

// IndexTools brings the documents of the whole server in line with the catalog: every offered
// tool (visible, server active) is (re)indexed and documents of tools it no longer offers are
// removed. An unknown or disabled server offers nothing, so its documents disappear.
// If the embedder fails, every document is still written (text search works) and the first
// embedder error is returned afterwards, so the outbox retries and the sweep fills the vectors in
// later. Decided deviation vector-kept-on-failed-reembedding: a document that already has a vector
// keeps it (stale beats none) and is marked for the sweep; a document that has none stays without
// (the TypeScript server nulls the existing vector).
func IndexTools(ctx context.Context, d Deps, server string) error {
	return indexTools(ctx, d, server, nil, false)
}

// IndexToolRefs is IndexTools for the given refs only: each is (re)indexed if the catalog still
// offers it, otherwise removed. Other documents of the server are left alone; no refs, no work.
func IndexToolRefs(ctx context.Context, d Deps, server string, refs []string) error {
	return indexTools(ctx, d, server, refs, true)
}

func indexTools(ctx context.Context, d Deps, server string, refs []string, only bool) error {
	source, err := d.Tools.ListToolsForIndex(ctx, server)
	if err != nil {
		return err
	}
	var (
		serverName, serverDescription string
		offered                       []catalog.IndexedTool
	)
	if source != nil {
		serverName, serverDescription, offered = source.Name, source.Description, source.Tools
	}
	wanted := offered
	if only {
		wanted = nil
		for _, t := range offered {
			if slices.Contains(refs, t.Ref) {
				wanted = append(wanted, t)
			}
		}
	}
	keep := make(map[string]bool, len(wanted))
	for _, t := range wanted {
		keep[t.Ref] = true
	}
	candidates := refs
	if !only {
		if candidates, err = d.Index.RefsOfServer(ctx, server); err != nil {
			return err
		}
	}
	var gone []string
	for _, ref := range candidates {
		if !keep[ref] {
			gone = append(gone, ref)
		}
	}
	// Removal runs before any embedding, so an embedder failure never blocks it.
	if len(gone) > 0 {
		if err := d.Index.Remove(ctx, gone); err != nil {
			return err
		}
	}

	// One passage per embedder call, sequentially (the q8 model's activation scale is per run, so
	// a vector would shift with its batch mates). After the first failure the remaining tools are
	// written through UpsertEmbedFailed and the embedder is not called again.
	var failure error
	docs := make([]Document, 0, len(wanted))
	for _, t := range wanted {
		body, err := domain.SchemaText(t.InputSchema)
		if err != nil {
			return fmt.Errorf("search: input schema of %q is not JSON: %w", t.Ref, err)
		}
		doc := Document{Ref: t.Ref, Server: server, Title: t.Title, Description: t.Description, Body: body}
		if failure == nil {
			passage := domain.BuildPassage(domain.PassageSource{
				ServerName: serverName, ServerDescription: serverDescription,
				ToolName: t.Name, Title: deref(t.Title), Description: t.Description,
			})
			vec, err := d.Embedder.EmbedPassage(ctx, passage)
			if err == nil && len(vec) != Dimensions {
				err = ErrBadDimensions
			}
			if err != nil {
				// A cancelled run writes nothing: stop.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failure = err
			} else {
				doc.Embedding = vec
			}
		}
		docs = append(docs, doc)
	}
	// Nothing is written while embedding: every upsert follows the last embedder call.
	for _, doc := range docs {
		var err error
		if doc.Embedding == nil {
			err = d.Index.UpsertEmbedFailed(ctx, doc)
		} else {
			err = d.Index.Upsert(ctx, doc)
		}
		if err != nil {
			return err
		}
	}
	return failure
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// RemoveTools deletes the documents of the listed refs of a server that the catalog does not
// currently offer for indexing. Decided deviation stale-removal-rechecks-catalog (the TypeScript
// server deletes the listed refs without asking): a removal event delivered late or twice, after
// the tool was offered again, leaves the live document alone. An unknown or disabled server offers
// nothing, so all listed refs go. It is idempotent; a catalog error is returned (the event is
// retried) and nothing is removed.
func RemoveTools(ctx context.Context, d Deps, server string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	source, err := d.Tools.ListToolsForIndex(ctx, server)
	if err != nil {
		return err
	}
	offered := map[string]bool{}
	if source != nil {
		for _, t := range source.Tools {
			offered[t.Ref] = true
		}
	}
	var gone []string
	for _, ref := range refs {
		if !offered[ref] {
			gone = append(gone, ref)
		}
	}
	return d.Index.Remove(ctx, gone)
}
