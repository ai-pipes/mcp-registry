import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import {
  type Connection,
  type ExpiryReason,
  isRefreshRejected,
  type RefreshFailure,
} from '../domain/connection.ts';
import {
  type ConnectionsDeps,
  type CredentialFailure,
  type ServerConfig,
  TokenEndpointError,
} from './ports.ts';

/** What happened under the lock; the audit entry is written after the transaction. */
type Locked = { result: Result<string, CredentialFailure>; expired?: ExpiryReason };

const failureOf = (error: unknown): RefreshFailure =>
  error instanceof TokenEndpointError ? { status: error.status, error: error.error } : null;

/**
 * Renews `seen` under the connection lock. Everything else the renewal needs (the server config,
 * the client secret) is read before the lock and the audit is written after it: the lock holds a
 * database connection, and asking the pool for another one under it can starve the pool.
 *
 * The decision is taken again on the locked row: when another process renewed it after `seen`
 * was read, its new token is used rather than refreshed once more, even for a forced refresh.
 */
export async function renewUnderLock(
  deps: ConnectionsDeps,
  input: { seen: Connection; server: ServerConfig; config: string; forceRefresh: boolean },
): Promise<Result<string, CredentialFailure>> {
  const { seen, server, config, forceRefresh } = input;
  const clientSecret = server.oauth ? await deps.servers.clientSecret(server.slug) : null;
  const locked = await deps.connections.withLock(
    seen.userId,
    seen.server,
    async (current, save): Promise<Locked> => {
      if (!current) return { result: err('needs_connection') };
      const now = deps.clock.now();
      const force = forceRefresh && !current.isNewerThan(seen);
      const plan = current.credentialPlan(now, { forceRefresh: force, config });
      const expire = async (reason: ExpiryReason): Promise<Locked> => {
        current.expire(deps.clock.now());
        await save(current);
        return { result: err('expired'), expired: reason };
      };
      if (plan.action === 'use') return { result: ok(deps.secretBox.open(current.accessToken)) };
      if (plan.action === 'expired') return { result: err('expired') };
      if (plan.action === 'expire') return expire(plan.reason);
      // An OAuth connection bound to this config, yet the config has no OAuth client: an
      // inconsistent catalog; leave the connection alone.
      if (!server.oauth) return { result: err('needs_connection') };

      let tokens;
      try {
        tokens = await deps.oauth.refresh(
          server.oauth,
          clientSecret,
          deps.secretBox.open(plan.refreshToken),
        );
      } catch (error) {
        if (isRefreshRejected(failureOf(error))) return expire('refresh_rejected');
        // No answer is not a "no": the grant may be fine, so nothing is saved, and a token that
        // has not run out yet still serves.
        return current.usableWhileProviderDown(deps.clock.now(), { forceRefresh: force })
          ? { result: ok(deps.secretBox.open(current.accessToken)) }
          : { result: err('unavailable') };
      }
      current.refreshed(
        {
          access: deps.secretBox.seal(tokens.accessToken),
          refresh: tokens.refreshToken === null ? null : deps.secretBox.seal(tokens.refreshToken),
          expiresAt: tokens.expiresAt,
        },
        deps.clock.now(),
      );
      await save(current);
      return { result: ok(tokens.accessToken) };
    },
  );
  if (locked.expired) {
    await recordExpired(deps, { userId: seen.userId, server: seen.server }, locked.expired);
  }
  return locked.result;
}

export async function recordExpired(
  deps: Pick<ConnectionsDeps, 'audit'>,
  who: { userId: string; server: string },
  reason: ExpiryReason,
): Promise<void> {
  await deps.audit.record({
    actorId: who.userId,
    action: 'connection.expired',
    target: who.server,
    meta: { reason },
  });
}
