import type { ConnectionStatus } from '../domain/connection.ts';
import { serverFingerprint } from '../domain/server-fingerprint.ts';
import type { ConnectionsDeps, PersonalAuthKind, Principal } from './ports.ts';

export type ConnectionSummary = {
  server: string;
  name: string;
  authKind: PersonalAuthKind;
  status: ConnectionStatus | 'missing';
};

/**
 * Every server the person may use that needs their own credentials, with the connection state.
 * A connection made for an older server config shows as expired.
 */
export async function listConnections(
  deps: Pick<ConnectionsDeps, 'servers' | 'connections'>,
  q: { principal: Principal },
): Promise<ConnectionSummary[]> {
  const [servers, connections] = await Promise.all([
    deps.servers.personalServersFor(q.principal),
    deps.connections.listForUser(q.principal.userId),
  ]);
  const byServer = new Map(connections.map((c) => [c.server, c]));
  return servers.map((s) => ({
    server: s.slug,
    name: s.name,
    authKind: s.authKind,
    status: byServer.get(s.slug)?.statusFor(serverFingerprint(s.config)) ?? 'missing',
  }));
}
