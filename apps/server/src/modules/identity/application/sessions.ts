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
 * Daily housekeeping: deletes dead sessions (12 h idle or the 30-day limit) and expired login
 * links. A dead session or link is refused anyway; this only keeps the tables from growing.
 * Returns the number of deleted sessions (links are not counted).
 */
export async function pruneExpiredSessions(deps: {
  sessions: Pick<IdentityDeps['sessions'], 'deleteDead'>;
  loginLinks: Pick<IdentityDeps['loginLinks'], 'deleteExpired'>;
  clock: IdentityDeps['clock'];
}): Promise<number> {
  const now = deps.clock.now();
  await deps.loginLinks.deleteExpired(now);
  return deps.sessions.deleteDead(deadSessionCutoffs(now));
}
