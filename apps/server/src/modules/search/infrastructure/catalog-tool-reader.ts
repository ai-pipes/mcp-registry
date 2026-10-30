import { type CatalogDeps, listServerSlugs, listToolsForIndex } from '../../catalog/index.ts';
import type { ToolReader } from '../application/ports.ts';

/** ToolReader over the catalog's public queries (never catalog tables). */
export function createCatalogToolReader(
  catalog: Pick<CatalogDeps, 'servers' | 'tools'>,
): ToolReader {
  return {
    forServer: (slug) => listToolsForIndex(catalog, slug),
    serverSlugs: () => listServerSlugs(catalog),
  };
}
