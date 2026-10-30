import { type Resealed, resealSecret, type SecretBox } from '../../../shared-kernel/index.ts';
import type { ConnectionsDeps } from './ports.ts';

/**
 * What a key rotation did to people's connections. `failed`: `<userId>/<server>` of connections
 * left as they were because neither key opens one of their tokens.
 */
export type ResealSummary = { resealed: number; current: number; failed: string[] };

type Keys = { from: SecretBox; to: SecretBox };
type Outcome = 'resealed' | 'current' | 'failed' | 'gone';

const sealedOf = (p: Resealed, was: Uint8Array): Uint8Array =>
  p.kind === 'resealed' ? p.sealed : was;

function resealOne(
  deps: Pick<ConnectionsDeps, 'connections'>,
  keys: Keys,
  userId: string,
  server: string,
): Promise<Outcome> {
  return deps.connections.withLock(userId, server, async (c, save): Promise<Outcome> => {
    if (!c) return 'gone';
    const access = resealSecret(c.accessToken, keys);
    const was = c.refreshToken;
    const refresh = was ? resealSecret(was, keys) : null;
    const parts = refresh ? [access, refresh] : [access];
    if (parts.some((p) => p.kind === 'unreadable')) return 'failed';
    if (parts.every((p) => p.kind === 'current')) return 'current';
    c.replaceSealed({
      access: sealedOf(access, c.accessToken),
      refresh: refresh && was ? sealedOf(refresh, was) : null,
    });
    await save(c);
    return 'resealed';
  });
}

/**
 * SECRETS_KEY rotation for connections: the access, refresh and PAT tokens sealed with `from` are
 * sealed again with `to`. Each connection is rewritten under its lock (`withLock`), so a refresh
 * running in the app at the same time is never overwritten with older tokens. Tokens already
 * under `to` are left alone: an interrupted run can simply be repeated.
 */
export async function resealConnections(
  deps: Pick<ConnectionsDeps, 'connections'>,
  keys: Keys,
): Promise<ResealSummary> {
  const summary: ResealSummary = { resealed: 0, current: 0, failed: [] };
  const all = await deps.connections.listKeys();
  // One at a time: each holds a row lock and a pool connection.
  await all.reduce<Promise<void>>(async (prev, { userId, server }) => {
    await prev;
    const outcome = await resealOne(deps, keys, userId, server);
    if (outcome === 'resealed') summary.resealed += 1;
    else if (outcome === 'current') summary.current += 1;
    else if (outcome === 'failed') summary.failed.push(`${userId}/${server}`);
  }, Promise.resolve());
  return summary;
}
