import { deadSessionCutoffs, isSessionAlive } from '../domain/session.ts';
import { type Principal, principalOf } from '../domain/user.ts';
import type { IdentityDeps } from './ports.ts';

export async function logout(deps: IdentityDeps, input: { sessionId: string }): Promise<void> {
  const idHash = deps.sha256.hex(input.sessionId);
  const session = await deps.sessions.findByHash(idHash);
  if (!session) return;
  await deps.sessions.delete(idHash);
  await deps.audit.record({ actorId: session.userId, action: 'auth.logout' });
}

export async function authenticateSession(
  deps: IdentityDeps,
  input: { sessionId: string },
): Promise<Principal | null> {
  const idHash = deps.sha256.hex(input.sessionId);
  const session = await deps.sessions.findByHash(idHash);
  if (!session) return null;
  const now = deps.clock.now();
  if (!isSessionAlive(session, now)) {
    await deps.sessions.delete(idHash);
    return null;
  }
  const user = await deps.users.findById(session.userId);
  if (!user || user.disabled) return null;
  await deps.sessions.touch(idHash, now);
  return principalOf(user, 'session');
}

/**
 * Daily housekeeping: sessions that ended (12 h idle or the 30-day limit) are deleted. A dead
 * session is refused anyway; this only keeps `identity.sessions` from growing. Returns how many.
 */
export async function pruneExpiredSessions(deps: {
  sessions: Pick<IdentityDeps['sessions'], 'deleteDead'>;
  clock: IdentityDeps['clock'];
}): Promise<number> {
  return deps.sessions.deleteDead(deadSessionCutoffs(deps.clock.now()));
}
