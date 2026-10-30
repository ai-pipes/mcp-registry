import type { CatalogDeps } from '../deps.ts';

/** One visible tool as the search index needs it. */
export type IndexedToolSource = {
  ref: string;
  name: string;
  title: string | null;
  description: string;
  inputSchema: Record<string, unknown>;
};

/** A server and its searchable tools; `tools` is empty for a disabled server. */
export type ServerIndexSource = {
  slug: string;
  name: string;
  description: string;
  tools: IndexedToolSource[];
};

/**
 * What the search module indexes for a server: its non-hidden, non-removed tools plus the
 * server's name and description. A disabled server has no searchable tools. Null when unknown.
 */
export async function listToolsForIndex(
  deps: Pick<CatalogDeps, 'servers' | 'tools'>,
  serverSlug: string,
): Promise<ServerIndexSource | null> {
  const server = await deps.servers.findBySlug(serverSlug);
  if (!server) return null;
  const tools = server.status === 'active' ? await deps.tools.listByServer(serverSlug) : [];
  return {
    slug: server.slug,
    name: server.name,
    description: server.description,
    tools: tools
      .filter((t) => t.visible)
      .map((t) => ({
        ref: t.ref,
        name: t.name,
        title: t.title,
        description: t.description,
        inputSchema: t.inputSchema,
      })),
  };
}

/** Slugs of every server, disabled ones included (the index must also clean up after them). */
export async function listServerSlugs(deps: Pick<CatalogDeps, 'servers'>): Promise<string[]> {
  return (await deps.servers.list()).map((l) => l.server.slug);
}
