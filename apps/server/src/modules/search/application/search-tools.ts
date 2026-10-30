import { errorLabel } from '../../../shared-kernel/index.ts';
import { rrf } from '../domain/rrf.ts';
import { MAX_PASSAGE_CHARS } from '../domain/search-document.ts';
import type { Candidate, SearchDeps } from './ports.ts';

const CANDIDATES = 50;
const RRF_K = 60;
/** FTS and vector: the number of lists a perfect hit tops. */
const LISTS = 2;

/**
 * Default τ, calibrated on multilingual-e5-small q8 and the Gitea+YouTrack tools:
 * cosine similarity of unrelated queries reached 0.809 at most, relevant ones started at 0.859,
 * so 0.83 sits in the middle of the gap. Recalibrate with SEARCH_MIN_SIMILARITY for other models or tools.
 */
export const DEFAULT_MIN_SIMILARITY = 0.83;

export type ToolHit = { ref: string; server: string; score: number };

/** Vector candidates, or null when the embedder is not ready, fails, or the query is too long. */
async function vectorCandidates(
  deps: Pick<SearchDeps, 'index' | 'embedder' | 'minSimilarity' | 'logger'>,
  query: string,
  servers: string[],
): Promise<Candidate[] | null> {
  if (query.length > MAX_PASSAGE_CHARS) return null;
  try {
    if (!(await deps.embedder.ready())) return null;
    const embedding = await deps.embedder.embedQuery(query);
    return await deps.index.vectorCandidates(embedding, CANDIDATES, deps.minSimilarity, servers);
  } catch (error) {
    // Degraded to text-only search: say so, without the query.
    deps.logger.warn(
      { err: errorLabel(error) },
      'search: vector search failed, text-only fallback',
    );
    return null;
  }
}

/**
 * Hybrid search: FTS and vector candidates (similarity ≥ τ) fused with RRF; works FTS-only
 * without embeddings. Nothing matches the text and nothing clears τ → [].
 *
 * score = rrf / (2 / (k + 1)): the share of the best possible fused score over the two lists.
 * The divisor is fixed, so a hit's score does not depend on whether the embedder was ready:
 * 1 = first in both lists; a hit found by one list scores at most 0.5. Never rescaled to the best hit.
 *
 * `servers` (slugs the caller may use) restricts both candidate lists in the same query, before
 * the top-N cut, so inaccessible tools never eat the `limit`. `[]` → nothing, no query is made.
 */
export async function searchTools(
  deps: Pick<SearchDeps, 'index' | 'embedder' | 'minSimilarity' | 'logger'>,
  q: { query: string; limit: number; servers: string[] },
): Promise<ToolHit[]> {
  const query = q.query.trim();
  if (query.length === 0 || q.limit <= 0 || q.servers.length === 0) return [];
  const [fts, vector] = await Promise.all([
    deps.index.ftsCandidates(query, CANDIDATES, q.servers),
    vectorCandidates(deps, query, q.servers),
  ]);
  const serverOf = new Map([...fts, ...(vector ?? [])].map((c) => [c.ref, c.server]));
  const lists = [fts.map((c) => c.ref), ...(vector ? [vector.map((c) => c.ref)] : [])];
  const maxScore = LISTS / (RRF_K + 1);
  return rrf(lists, RRF_K)
    .slice(0, q.limit)
    .flatMap((f) => {
      const server = serverOf.get(f.id);
      return server === undefined ? [] : [{ ref: f.id, server, score: f.score / maxScore }];
    });
}
