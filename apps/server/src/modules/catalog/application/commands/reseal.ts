import { resealSecret, type SecretBox } from '../../../../shared-kernel/index.ts';
import type { CatalogDeps } from '../deps.ts';

/**
 * What a key rotation did to the server secrets. `failed`: slugs left as they were — neither key
 * opens the secret, or it kept changing under the rotation.
 */
export type ResealSummary = { resealed: number; current: number; failed: string[] };

type Keys = { from: SecretBox; to: SecretBox };
type Outcome = 'resealed' | 'current' | 'failed' | 'none';

/** A compare-and-set lost to a concurrent write is retried this many times in all. */
const ATTEMPTS = 3;

async function resealOne(
  deps: Pick<CatalogDeps, 'servers'>,
  keys: Keys,
  slug: string,
  sealed: Uint8Array | null,
  attempt = 1,
): Promise<Outcome> {
  if (sealed === null) return 'none';
  const r = resealSecret(sealed, keys);
  if (r.kind === 'current') return 'current';
  if (r.kind === 'unreadable') return 'failed';
  if (await deps.servers.replaceSealedSecret(slug, sealed, r.sealed)) return 'resealed';
  if (attempt === ATTEMPTS) return 'failed';
  const now = (await deps.servers.findBySlug(slug))?.sealedSecret ?? null;
  return resealOne(deps, keys, slug, now, attempt + 1);
}

/**
 * SECRETS_KEY rotation for the catalog: every server secret sealed with `from` is sealed again
 * with `to`. One already under `to` is left alone, so an interrupted run can simply be repeated.
 * Each swap is a compare-and-set: a secret an admin replaced meanwhile is read again, not
 * overwritten. Plain secrets never leave this function.
 */
export async function resealServerSecrets(
  deps: Pick<CatalogDeps, 'servers'>,
  keys: Keys,
): Promise<ResealSummary> {
  const summary: ResealSummary = { resealed: 0, current: 0, failed: [] };
  const listings = await deps.servers.list();
  // One server at a time: a handful of rows, and an error stops the run where it is.
  await listings.reduce<Promise<void>>(async (prev, { server }) => {
    await prev;
    const outcome = await resealOne(deps, keys, server.slug, server.sealedSecret);
    if (outcome === 'resealed') summary.resealed += 1;
    else if (outcome === 'current') summary.current += 1;
    else if (outcome === 'failed') summary.failed.push(server.slug);
  }, Promise.resolve());
  return summary;
}
