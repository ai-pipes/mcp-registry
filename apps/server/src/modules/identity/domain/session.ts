const SESSION_IDLE_MS = 12 * 3600_000;
const SESSION_ABSOLUTE_MS = 30 * 24 * 3600_000;

export function sessionExpiry(now: Date): Date {
  return new Date(now.getTime() + SESSION_ABSOLUTE_MS);
}

/** A session ends after 12 h without use or at its absolute 30-day limit. */
export function isSessionAlive(session: { lastSeenAt: Date; expiresAt: Date }, now: Date): boolean {
  return (
    now.getTime() < session.expiresAt.getTime() &&
    now.getTime() <= session.lastSeenAt.getTime() + SESSION_IDLE_MS
  );
}

/**
 * Where `isSessionAlive` ends, as cutoffs a store can delete by: a session is dead once its
 * `expiresAt` is at or before `expiresAtOrBefore`, or it was last seen before `lastSeenBefore`.
 */
export function deadSessionCutoffs(now: Date): { expiresAtOrBefore: Date; lastSeenBefore: Date } {
  return { expiresAtOrBefore: now, lastSeenBefore: new Date(now.getTime() - SESSION_IDLE_MS) };
}
