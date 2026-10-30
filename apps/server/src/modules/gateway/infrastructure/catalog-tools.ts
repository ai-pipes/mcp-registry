import {
  type CatalogDeps,
  getServerForUpstream,
  getServerSecret,
  getToolFor,
} from '../../catalog/index.ts';
import type { ToolCatalog } from '../application/ports.ts';

/** ToolCatalog over the catalog's public queries (never catalog tables). */
export function createCatalogToolCatalog(
  catalog: Pick<CatalogDeps, 'servers' | 'tools' | 'secretBox'>,
): ToolCatalog {
  return {
    async toolFor(principal, ref) {
      const r = await getToolFor(catalog, { principal, ref });
      return r.ok ? r.value : null;
    },
    serverFor: (slug) => getServerForUpstream(catalog, slug),
    serverSecret: (slug) => getServerSecret(catalog, slug),
  };
}
