package app

import (
	"context"
	"sync"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

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
		return nil, nil
	}
	var (
		wg     sync.WaitGroup
		fts    []domain.Candidate
		ftsErr error
		vector []domain.Candidate
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		fts, ftsErr = d.Index.FTSCandidates(ctx, q, domain.Candidates, servers)
	}()
	go func() {
		defer wg.Done()
		vector = vectorCandidates(ctx, d, q, servers)
	}()
	wg.Wait()
	if ftsErr != nil {
		return nil, ftsErr
	}
	return domain.Fuse(fts, vector, limit), nil
}

// vectorCandidates is nil when the embedder is not ready, fails, or the query is too long (the
// last two without ever touching the embedder when too long); a failure after that is logged.
func vectorCandidates(ctx context.Context, d Deps, query string, servers []string) []domain.Candidate {
	if !domain.QueryFitsTheEmbedder(query) || !d.Embedder.Ready(ctx) {
		return nil
	}
	embedding, err := d.Embedder.EmbedQuery(ctx, query)
	if err == nil {
		var found []domain.Candidate
		found, err = d.Index.VectorCandidates(ctx, embedding, domain.Candidates, d.MinSimilarity, servers)
		if err == nil {
			if found == nil {
				found = []domain.Candidate{}
			}
			return found
		}
	}
	if d.Logger != nil {
		d.Logger.Warn(MsgVectorFallback, "err", kernel.ErrorLabel(err))
	}
	return nil
}
