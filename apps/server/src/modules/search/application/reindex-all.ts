import { indexTools } from './index-tools.ts';
import type { SearchDeps } from './ports.ts';

/**
 * Rebuilds the index from the catalog and drops documents of vanished or disabled servers.
 * Returns the number of indexed tools. An embedder failure does not stop the run: every tool
 * still gets its text row, then the first error is rethrown.
 */
export async function reindexAll(deps: SearchDeps): Promise<number> {
  const servers = [
    ...new Set([...(await deps.tools.serverSlugs()), ...(await deps.index.servers())]),
  ];
  // Sequential: one embedding call at a time.
  const failure = await servers.reduce<Promise<unknown>>(async (prev, server) => {
    const first = await prev;
    try {
      await indexTools(deps, { server });
    } catch (error) {
      return first ?? error;
    }
    return first;
  }, Promise.resolve(null));
  if (failure !== null) throw failure;
  return deps.index.count();
}
