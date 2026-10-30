import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import { serverFingerprint } from '../domain/server-fingerprint.ts';
import type { ConnectionsDeps, CredentialFailure } from './ports.ts';
import { recordExpired, renewUnderLock } from './refresh.ts';

/**
 * The person's upstream secret for the server: the OAuth access token or the PAT, raw. Refreshes
 * ahead of expiry or when `forceRefresh` (upstream said 401). One refresh per (userId, server) at
 * a time: in this process by single-flight, across processes by the connection lock. A secret
 * issued for another server config (URL, auth kind, OAuth provider) is never handed out: the
 * connection expires instead.
 */
export async function resolveCredential(
  deps: ConnectionsDeps,
  q: { userId: string; server: string; forceRefresh?: boolean },
): Promise<Result<string, CredentialFailure>> {
  const forceRefresh = q.forceRefresh ?? false;
  const seen = await deps.connections.get(q.userId, q.server);
  if (!seen) return err('needs_connection');
  const server = await deps.servers.config(q.server);
  if (!server) return err('needs_connection');
  const config = serverFingerprint(server);
  const plan = seen.credentialPlan(deps.clock.now(), { forceRefresh, config });
  if (plan.action === 'use') return ok(deps.secretBox.open(seen.accessToken));
  if (plan.action === 'expired') return err('expired');
  return deps.flights.run(`${q.userId}\u0000${q.server}`, () =>
    renewUnderLock(deps, { seen, server, config, forceRefresh }),
  );
}

/** Upstream rejected the person's credential and it cannot be refreshed (a PAT): ready → expired. */
export async function markExpired(
  deps: ConnectionsDeps,
  cmd: { userId: string; server: string },
): Promise<void> {
  const changed = await deps.connections.withLock(cmd.userId, cmd.server, async (current, save) => {
    if (!current || current.status === 'expired') return false;
    current.expire(deps.clock.now());
    await save(current);
    return true;
  });
  if (changed) await recordExpired(deps, cmd, 'rejected_upstream');
}
