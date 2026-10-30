const PENDING_CONNECT_TTL_MS = 10 * 60_000;

/** A started OAuth connect: one-time, short-lived, bound to the person who started it. */
export interface PendingConnect {
  state: string;
  userId: string;
  server: string;
  codeVerifier: string;
  /** `serverFingerprint` when the attempt started: the code is exchanged only with that config. */
  issuedFor: string;
  expiresAt: Date;
}

/** When a connect attempt started at `now` stops being valid. */
export function pendingConnectExpiry(now: Date): Date {
  return new Date(now.getTime() + PENDING_CONNECT_TTL_MS);
}

export function isPendingConnectExpired(expiresAt: Date, now: Date): boolean {
  return now.getTime() >= expiresAt.getTime();
}

/** Whether the attempt may be finished by `userId` at `now`. */
export function canComplete(
  pending: PendingConnect,
  input: { userId: string; now: Date },
): boolean {
  return pending.userId === input.userId && !isPendingConnectExpired(pending.expiresAt, input.now);
}

/**
 * Whether the server config (fingerprint) is still the one the attempt started with: when an
 * admin changed the provider meanwhile, the code is not sent to the new one.
 */
export function startedWithConfig(pending: PendingConnect, config: string): boolean {
  return pending.issuedFor === config;
}
