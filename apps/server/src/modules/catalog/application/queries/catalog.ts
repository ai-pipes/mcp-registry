import { canUse } from '../../domain/access.ts';
import type { ToolAnnotations } from '../../domain/tool.ts';
import type { CatalogDeps } from '../deps.ts';
import type { Principal } from '../ports.ts';

export type CatalogEntry = {
  slug: string;
  name: string;
  description: string;
  tools: { ref: string; title: string | null; description: string; annotations: ToolAnnotations }[];
};

/**
 * What a person may browse: the active servers they can use and those servers' visible tools
 * (not hidden, not removed). No URL, no auth config.
 */
export async function listCatalogFor(
  deps: Pick<CatalogDeps, 'servers' | 'tools'>,
  q: { principal: Principal },
): Promise<CatalogEntry[]> {
  const usable = (await deps.servers.list()).filter((l) => canUse(l.server, q.principal));
  return Promise.all(
    usable.map(async ({ server }) => {
      const tools = await deps.tools.listByServer(server.slug);
      return {
        slug: server.slug,
        name: server.name,
        description: server.description,
        tools: tools
          .filter((t) => t.visible)
          .map((t) => ({
            ref: t.ref,
            title: t.title,
            description: t.description,
            annotations: t.annotations,
          })),
      };
    }),
  );
}
