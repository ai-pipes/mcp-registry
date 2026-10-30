import {
  type CatalogDeps,
  getServerForUpstream,
  getServerSecret,
  listServersFor,
  type ServerCallTarget,
} from '../../catalog/index.ts';
import type { PersonalServer, ServerConfig, ServerDirectory } from '../application/ports.ts';

const toConfig = (t: ServerCallTarget): ServerConfig => ({
  slug: t.slug,
  url: t.url,
  authKind: t.auth.kind,
  oauth: t.auth.kind === 'oauth_user' ? t.auth.oauth : null,
});

/** ServerDirectory over the catalog's public queries (never catalog tables). */
export function createCatalogServerDirectory(
  catalog: Pick<CatalogDeps, 'servers' | 'secretBox'>,
): ServerDirectory {
  const config = async (slug: string) => {
    const target = await getServerForUpstream(catalog, slug);
    return target ? toConfig(target) : null;
  };
  return {
    async personalServersFor(principal) {
      const servers = await listServersFor(catalog, { principal });
      const personal = await Promise.all(
        servers.map(async (s): Promise<PersonalServer | null> => {
          if (s.authKind !== 'oauth_user' && s.authKind !== 'pat_user') return null;
          const c = await config(s.slug);
          return c ? { slug: s.slug, name: s.name, authKind: s.authKind, config: c } : null;
        }),
      );
      return personal.filter((s) => s !== null);
    },
    config,
    clientSecret: (slug) => getServerSecret(catalog, slug),
  };
}
