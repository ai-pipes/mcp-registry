package app

import (
	"context"
	"errors"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

// ErrPanicked is what a panic inside a port during a search becomes. The panic value is never
// kept or logged (it could quote the query).
var ErrPanicked = errors.New("search: a search half panicked")

// MsgVectorFallback is the warning logged when the vector half of a search fails; the only
// record of the module. Its fields are exactly {err: <kernel.ErrorLabel>}, never the query.
const MsgVectorFallback = "search: vector search failed, text-only fallback"

// SearchTools is hybrid search: full-text and vector candidates (similarity >= MinSimilarity)
// fused with RRF; it works on text alone when the embedder is not ready, fails, or the query is
// too long for it. Nothing matching and nothing above the floor gives no hits. A blank query,
// limit <= 0 or no servers gives none at once, with no index or embedder call. servers is the
// set the caller may use; it reaches both candidate queries, so other servers never eat the limit.
// Both halves run concurrently; a full-text error is returned, a vector error only logged.
func SearchTools(ctx context.Context, d Deps, query string, limit int, servers []string) ([]domain.ToolHit, error) {
	q, ok := domain.PrepareQuery(query, limit, servers)
	if !ok {
		return []domain.ToolHit{}, nil // never nil: callers render it as []
	}
	// The full-text query runs in its own goroutine, the vector half on this one. A panic in a
	// port must not bypass the callers' recovery (HTTP, outbox) from a goroutine of ours.
	var (
		fts    []domain.Candidate
		ftsErr error
		done   = make(chan struct{})
	)
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil { // the value is dropped: it could quote the query
				fts, ftsErr = nil, ErrPanicked
			}
		}()
		fts, ftsErr = d.Index.FTSCandidates(ctx, q, domain.Candidates, servers)
	}()
	vector := vectorCandidates(ctx, d, q, servers)
	<-done
	if ftsErr != nil {
		return nil, ftsErr
	}
	return domain.Fuse(fts, vector, limit), nil
}

// vectorCandidates is nil when the embedder is not ready, fails, or the query is too long (the
// last two without ever touching the embedder when too long); a failure after that is logged.
func vectorCandidates(ctx context.Context, d Deps, query string, servers []string) (found []domain.Candidate) {
	defer func() {
		if recover() != nil { // treated like any vector failure; the value is dropped
			found = nil
			warnFallback(d, ErrPanicked)
		}
	}()
	if !domain.QueryFitsTheEmbedder(query) || !d.Embedder.Ready(ctx) {
		return nil
	}
	embedding, err := d.Embedder.EmbedQuery(ctx, query)
	if err == nil {
		found, err = d.Index.VectorCandidates(ctx, embedding, domain.Candidates, d.MinSimilarity, servers)
		if err == nil {
			if found == nil {
				found = []domain.Candidate{}
			}
			return found
		}
	}
	warnFallback(d, err)
	return nil
}

func warnFallback(d Deps, err error) {
	if d.Logger != nil {
		d.Logger.Warn(MsgVectorFallback, "err", kernel.ErrorLabel(err))
	}
}
