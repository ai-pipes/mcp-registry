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
// any other embedder failure (the document is stored without a vector).
var ErrBadDimensions = errors.New("embedder returned a vector of the wrong length")

// IndexTools brings the documents of the whole server in line with the catalog: every offered
// tool (visible, server active) is (re)indexed and documents of tools it no longer offers are
// removed. An unknown or disabled server offers nothing, so its documents disappear.
// If the embedder fails, every document is still written (text search works, no vector) and the
// first embedder error is returned afterwards, so the outbox retries and the sweep fills the
// vectors in later.
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
	// written without a vector and the embedder is not called again.
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
				// A cancelled run must not null vectors on its way out: stop, nothing is written.
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
		if err := d.Index.Upsert(ctx, doc); err != nil {
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

// RemoveTools deletes the documents of the refs without asking the catalog (a late removal event
// can delete a document the catalog has offered again since; the next event or reindex restores it).
func RemoveTools(ctx context.Context, d Deps, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	return d.Index.Remove(ctx, refs)
}
